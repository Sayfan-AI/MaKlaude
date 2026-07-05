package scan

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Report is the structured result of one [Pipeline.Run]: a per-cluster account
// of what MaKlaude observed and what it did about it, plus rolled-up totals. It
// is a plain value designed to be both human-printed and machine-asserted — the
// CLI renders it as text or JSON, and the end-to-end harness unmarshals the JSON
// to verify findings, escalation, and the read-only guarantee.
type Report struct {
	// GeneratedAt is when the scan ran (UTC), from the pipeline's clock.
	GeneratedAt time.Time `json:"generatedAt"`

	// Live reports whether escalation was backed by a real comms system (GitHub)
	// rather than the in-memory dry-run sink. When false, no external issues were
	// created — escalation counts reflect the in-memory trail only.
	Live bool `json:"live"`

	// Clusters holds one entry per registered cluster, in registry (declaration)
	// order, so multi-cluster output is stable.
	Clusters []ClusterReport `json:"clusters"`

	// Totals aggregates findings and escalation actions across all clusters. It is
	// derived by finalize and is purely a convenience for callers.
	Totals Totals `json:"totals"`
}

// ClusterReport is one cluster's slice of a [Report].
type ClusterReport struct {
	// Cluster is the registered cluster name this report concerns.
	Cluster string `json:"cluster"`

	// Reachable reports whether the cluster's API server answered the health
	// probe. When false, Findings will typically contain a single critical
	// "cluster unreachable" finding and ServerVersion is empty.
	Reachable bool `json:"reachable"`

	// ServerVersion is the API server version reported on a successful probe.
	ServerVersion string `json:"serverVersion,omitempty"`

	// Findings are the detector's raw findings for this cluster, most-urgent-first.
	// They are the unfolded symptom-level view; Incidents is the correlated,
	// diagnosed view the comms trail escalates on.
	Findings []FindingReport `json:"findings"`

	// Incidents are the correlated incidents for this cluster, each with its ranked
	// root-cause hypotheses, most-urgent-first. Escalation happens at this
	// granularity: one issue per incident. It is the diagnostic view an operator
	// (and the e2e harness) reads to see WHY, not just what.
	Incidents []IncidentReport `json:"incidents"`

	// Escalation summarizes the reconcile actions taken for this cluster (counted
	// at incident granularity: opened/updated/closed issues, one per incident).
	Escalation EscalationReport `json:"escalation"`

	// Error, when non-empty, explains a per-cluster failure (client build,
	// collection, or escalation). It never aborts the scan of other clusters.
	Error string `json:"error,omitempty"`
}

// FindingReport is the serializable projection of a detect.Finding. It drops the
// wall-clock DetectedAt (which equals the snapshot time and is not needed for
// assertions) in favour of a flat, stable shape.
type FindingReport struct {
	Identity string `json:"identity"`
	Severity string `json:"severity"`
	Cluster  string `json:"cluster"`
	Object   string `json:"object"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

// IncidentReport is the serializable projection of one correlated, diagnosed
// incident: the incident's stable identity and severity, its primary object, the
// full set of affected objects, and its ranked root-cause hypotheses. It is the
// report's diagnostic unit — the same granularity the comms trail escalates on.
type IncidentReport struct {
	Identity      string             `json:"identity"`
	Cluster       string             `json:"cluster"`
	Severity      string             `json:"severity"`
	PrimaryObject string             `json:"primaryObject"`
	PrimaryTitle  string             `json:"primaryTitle"`
	Affected      []string           `json:"affected"`
	Hypotheses    []HypothesisReport `json:"hypotheses"`
}

// HypothesisReport is the serializable projection of one ranked root-cause
// hypothesis: its cause class, coarse confidence, human-readable title/message,
// provenance (deterministic vs. refined), and the object strings of the evidence
// findings that support it. It drops the wall-clock (which equals the snapshot
// time) in favour of a flat, stable shape.
type HypothesisReport struct {
	Cause      string   `json:"cause"`
	Confidence string   `json:"confidence"`
	Title      string   `json:"title"`
	Message    string   `json:"message"`
	Source     string   `json:"source"`
	Evidence   []string `json:"evidence"`
}

// EscalationReport mirrors escalate.Outcome in a JSON-tagged form.
type EscalationReport struct {
	Opened  int `json:"opened"`
	Updated int `json:"updated"`
	Closed  int `json:"closed"`
}

// Totals are the cross-cluster aggregates a caller most often wants at a glance.
type Totals struct {
	// Clusters is the number of clusters scanned.
	Clusters int `json:"clusters"`
	// Findings is the total number of raw findings across all clusters.
	Findings int `json:"findings"`
	// Incidents is the total number of correlated incidents across all clusters —
	// the granularity escalation operates at.
	Incidents int `json:"incidents"`
	// BySeverity counts findings by their severity token ("critical", "warning",
	// "info"). Absent severities are omitted.
	BySeverity map[string]int `json:"bySeverity,omitempty"`
	// Opened/Updated/Closed sum the per-cluster escalation actions.
	Opened  int `json:"opened"`
	Updated int `json:"updated"`
	Closed  int `json:"closed"`
}

// finalize computes the report's rolled-up totals from its per-cluster entries.
func (r *Report) finalize() {
	t := Totals{Clusters: len(r.Clusters), BySeverity: map[string]int{}}
	for i := range r.Clusters {
		c := &r.Clusters[i]
		t.Findings += len(c.Findings)
		t.Incidents += len(c.Incidents)
		for j := range c.Findings {
			t.BySeverity[c.Findings[j].Severity]++
		}
		t.Opened += c.Escalation.Opened
		t.Updated += c.Escalation.Updated
		t.Closed += c.Escalation.Closed
	}
	if len(t.BySeverity) == 0 {
		t.BySeverity = nil
	}
	r.Totals = t
}

// WriteJSON renders the report as indented JSON to w. It is the machine-readable
// form the e2e harness consumes.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("scan: encoding report as JSON: %w", err)
	}
	return nil
}

// WriteText renders the report as a compact, human-readable summary to w. It is
// the default CLI output: a per-cluster block listing findings most-urgent-first
// and the escalation outcome, followed by cross-cluster totals.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder

	mode := "dry-run (in-memory escalation; no external writes)"
	if r.Live {
		mode = "live (escalation reaches the configured comms system)"
	}
	fmt.Fprintf(&b, "MaKlaude scan @ %s — %s\n", r.GeneratedAt.Format("2006-01-02 15:04:05 MST"), mode)

	if len(r.Clusters) == 0 {
		b.WriteString("\nNo clusters registered.\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	for i := range r.Clusters {
		c := &r.Clusters[i]
		b.WriteString("\n")
		fmt.Fprintf(&b, "Cluster %q:\n", c.Cluster)
		if c.Error != "" {
			fmt.Fprintf(&b, "  error: %s\n", c.Error)
		}
		if c.Reachable {
			fmt.Fprintf(&b, "  reachable: yes (server %s)\n", c.ServerVersion)
		} else {
			b.WriteString("  reachable: no\n")
		}
		if len(c.Findings) == 0 {
			b.WriteString("  findings: none\n")
		} else {
			fmt.Fprintf(&b, "  findings (%d):\n", len(c.Findings))
			for j := range c.Findings {
				f := &c.Findings[j]
				fmt.Fprintf(&b, "    - [%s] %s — %s\n", strings.ToUpper(f.Severity), f.Object, f.Title)
			}
		}
		if len(c.Incidents) == 0 {
			b.WriteString("  incidents: none\n")
		} else {
			fmt.Fprintf(&b, "  incidents (%d):\n", len(c.Incidents))
			for j := range c.Incidents {
				in := &c.Incidents[j]
				fmt.Fprintf(&b, "    - [%s] %s — %s (%d affected)\n",
					strings.ToUpper(in.Severity), in.PrimaryObject, in.PrimaryTitle, len(in.Affected))
				for k := range in.Hypotheses {
					h := &in.Hypotheses[k]
					fmt.Fprintf(&b, "        * hypothesis: %s [confidence: %s]\n", h.Title, h.Confidence)
				}
			}
		}
		fmt.Fprintf(&b, "  escalation: opened=%d updated=%d closed=%d\n",
			c.Escalation.Opened, c.Escalation.Updated, c.Escalation.Closed)
	}

	b.WriteString("\nTotals: ")
	fmt.Fprintf(&b, "%d cluster(s), %d finding(s), %d incident(s)", r.Totals.Clusters, r.Totals.Findings, r.Totals.Incidents)
	if len(r.Totals.BySeverity) > 0 {
		keys := make([]string, 0, len(r.Totals.BySeverity))
		for k := range r.Totals.BySeverity {
			keys = append(keys, k)
		}
		sortStrings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Totals.BySeverity[k]))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "; escalation opened=%d updated=%d closed=%d\n",
		r.Totals.Opened, r.Totals.Updated, r.Totals.Closed)

	_, err := io.WriteString(w, b.String())
	return err
}

// HasSeverity reports whether any cluster produced at least one finding of the
// given severity token. It is a small convenience for callers (and tests) that
// want to branch on, say, the presence of a critical finding.
func (r *Report) HasSeverity(severity string) bool {
	return r.Totals.BySeverity[severity] > 0
}
