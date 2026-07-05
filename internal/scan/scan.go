// Package scan wires MaKlaude's read-only pipeline into a single, one-shot
// operation: for each registered cluster it collects a [health.Snapshot],
// analyzes it into [detect.Finding]s, and reconciles those findings into the
// external comms trail via an [escalate.Escalator]. The result is a structured,
// serializable [Report] the CLI prints and the end-to-end harness asserts on.
//
// The package is the runnable seam between the (independently tested) pipeline
// packages and the `maklaude scan` command. It deliberately holds no opinions of
// its own beyond orchestration order: collection, detection, correlation,
// diagnosis, and escalation each keep their own contracts. The findings are
// correlated into incidents and each incident is diagnosed into ranked root-cause
// hypotheses, so escalation happens at incident granularity — one auditable issue
// per correlated problem, carrying its whole diagnosis. Crucially, every step is
// read-only with respect to
// the cluster — the only writes the pipeline can perform are to the comms trail
// (issues), never to a cluster, and even those are gated behind the
// human-in-the-loop escalator.
//
// Determinism and isolation carry through from the underlying packages: findings
// are produced by the pure [detect.Analyze], and each cluster is scanned through
// its own read-only client, so one cluster's scan can never affect another.
package scan

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// clientBuilder constructs a read-only [kube.Client] for a cluster handle. It is
// a seam so tests can inject a fake clientset (via
// [kube.NewClientWithInterface]) instead of dialing a real API server, while the
// production path uses [kube.NewClient] and its transport-level write guard.
type clientBuilder func(h *cluster.Handle) (*kube.Client, error)

// Pipeline runs MaKlaude's one-shot read-only scan across a set of clusters.
//
// It owns the wiring (build client -> collect -> analyze -> escalate) and the
// two injectable seams that keep it unit-testable without a live cluster or a
// real comms backend: a [clientBuilder] and an [escalate.Escalator]. The zero
// value is not usable; construct one with [NewPipeline] or [NewPipelineForTest].
type Pipeline struct {
	newClient clientBuilder
	escalator *escalate.Escalator
	// live reports whether the escalator is backed by a real comms system
	// (GitHub) versus the in-memory dry-run sink. It is surfaced in the report so
	// an operator (or the e2e harness) can tell whether escalation actually
	// reached an external system.
	live bool
	// now stamps the report; injectable so tests get reproducible output.
	now func() time.Time
}

// NewPipeline builds the production pipeline: real read-only clients (with the
// transport write guard) and an escalator selected from the environment via
// [escalate.EscalatorFromEnv]. When no MAKLAUDE_GITHUB_* configuration is
// present the escalator degrades to a safe in-memory dry-run, so a scan never
// performs an unexpected external write.
func NewPipeline() *Pipeline {
	esc, live := escalate.EscalatorFromEnv()
	return &Pipeline{
		newClient: kube.NewClient,
		escalator: esc,
		live:      live,
		now:       time.Now,
	}
}

// NewPipelineForTest builds a pipeline with explicit seams for unit tests: a
// caller-supplied client builder (typically wrapping a client-go fake clientset)
// and escalator. now may be nil, in which case time.Now is used.
func NewPipelineForTest(newClient clientBuilder, esc *escalate.Escalator, live bool, now func() time.Time) *Pipeline {
	if now == nil {
		now = time.Now
	}
	return &Pipeline{
		newClient: newClient,
		escalator: esc,
		live:      live,
		now:       now,
	}
}

// Run scans every cluster in the registry once and returns a combined [Report].
//
// Each cluster is scanned independently: a failure to build a client or collect
// from one cluster is recorded as that cluster's error and does not abort the
// others (multi-cluster isolation). Escalation is performed per cluster against
// that cluster's findings, so one cluster's comms trail is never disturbed by
// another's. The returned error is non-nil only if the registry itself is
// unusable; per-cluster failures are reported inside the [Report] so callers get
// a complete picture in one pass.
func (p *Pipeline) Run(ctx context.Context, reg *cluster.Registry) (*Report, error) {
	if reg == nil {
		return nil, fmt.Errorf("scan: nil registry")
	}

	report := &Report{
		GeneratedAt: p.now().UTC(),
		Live:        p.live,
	}

	for _, h := range reg.Handles() {
		report.Clusters = append(report.Clusters, p.scanCluster(ctx, h))
	}

	report.finalize()
	return report, nil
}

// scanCluster runs the full pipeline for a single cluster, never panicking and
// always returning a populated [ClusterReport] — including on failure, where the
// Error field explains what went wrong and the rest stays zero.
func (p *Pipeline) scanCluster(ctx context.Context, h *cluster.Handle) ClusterReport {
	cr := ClusterReport{Cluster: h.Name()}

	client, err := p.newClient(h)
	if err != nil {
		cr.Error = fmt.Sprintf("building client: %v", err)
		return cr
	}

	collector := health.NewCollector(client)
	snap, err := collector.Collect(ctx)
	if err != nil {
		cr.Error = fmt.Sprintf("collecting health: %v", err)
		return cr
	}
	cr.Reachable = snap.Reachability.Reachable
	cr.ServerVersion = snap.Reachability.ServerVersion

	// The read-only interpretation pipeline: raw findings, correlated into
	// incidents, then diagnosed into ranked root-cause hypotheses. Escalation
	// happens at INCIDENT granularity (one issue per correlated problem carrying
	// its whole diagnosis), not per raw finding — see the escalate package.
	findings := detect.Analyze(snap)
	cr.Findings = toFindingReports(findings)

	incidents := correlate.Correlate(snap, findings)
	subjects := make([]escalate.Subject, 0, len(incidents))
	for i := range incidents {
		subjects = append(subjects, escalate.Subject{
			Incident:   incidents[i],
			Hypotheses: diagnose.Diagnose(snap, incidents[i]),
		})
	}
	cr.Incidents = toIncidentReports(subjects)

	outcome, err := p.escalator.Reconcile(ctx, subjects)
	if err != nil {
		// Escalation is best-effort; record the error but keep the findings and
		// whatever the outcome counted as succeeded, so a partial comms failure
		// does not hide what MaKlaude observed.
		cr.Error = fmt.Sprintf("escalating incidents: %v", err)
	}
	cr.Escalation = EscalationReport{
		Opened:  outcome.Opened,
		Updated: outcome.Updated,
		Closed:  outcome.Closed,
	}

	return cr
}

// toFindingReports converts detector findings into the report's serializable
// finding shape, preserving the detector's most-urgent-first ordering.
func toFindingReports(findings []detect.Finding) []FindingReport {
	out := make([]FindingReport, 0, len(findings))
	for i := range findings {
		f := findings[i]
		out = append(out, FindingReport{
			Identity: string(f.Identity),
			Severity: f.Severity.String(),
			Cluster:  f.Cluster,
			Object:   f.Object.String(),
			Title:    f.Title,
			Message:  f.Message,
		})
	}
	return out
}

// toIncidentReports converts the escalation subjects (correlated incidents plus
// their ranked diagnosis) into the report's serializable incident shape,
// preserving correlation's most-urgent-first incident order and diagnosis's
// most-confident-first hypothesis order.
func toIncidentReports(subjects []escalate.Subject) []IncidentReport {
	out := make([]IncidentReport, 0, len(subjects))
	for i := range subjects {
		s := subjects[i]
		affected := make([]string, 0, len(s.Incident.Findings()))
		for _, f := range s.Incident.Findings() {
			affected = append(affected, f.Object.String())
		}
		hyps := make([]HypothesisReport, 0, len(s.Hypotheses))
		for _, h := range s.Hypotheses {
			evidence := make([]string, 0, len(h.Evidence))
			for _, e := range h.Evidence {
				evidence = append(evidence, e.Object.String())
			}
			hyps = append(hyps, HypothesisReport{
				Cause:      string(h.Cause),
				Confidence: h.Confidence.String(),
				Title:      h.Title,
				Message:    h.Message,
				Source:     string(h.Source),
				Evidence:   evidence,
			})
		}
		out = append(out, IncidentReport{
			Identity:      string(s.Identity()),
			Cluster:       s.Cluster(),
			Severity:      s.Severity().String(),
			PrimaryObject: s.Incident.Primary.Object.String(),
			PrimaryTitle:  s.Incident.Primary.Title,
			Affected:      affected,
			Hypotheses:    hyps,
		})
	}
	return out
}

// sortStrings is a tiny helper kept local so report finalization has a stable,
// dependency-light sort for the severity-count keys.
func sortStrings(s []string) { sort.Strings(s) }
