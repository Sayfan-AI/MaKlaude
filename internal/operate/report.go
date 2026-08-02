package operate

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Report is the structured result of one [Cycle.Run]: per cluster, what MaKlaude
// would do, what it asked about, and what it actually did to anything.
//
// It is a plain value designed to be both human-printed and machine-asserted. Its
// central job is to make the posture legible at a glance — an operator reading the
// first line should be able to tell whether this run could have changed a cluster at
// all, without inferring it from the absence of an execution section.
type Report struct {
	// GeneratedAt is when the cycle ran (UTC), from the cycle's clock.
	GeneratedAt time.Time `json:"generatedAt"`

	// Mode is the kill-switch posture the whole run was under: "disabled", "dry-run",
	// or "enabled". Under "disabled" nothing here was ever capable of writing.
	Mode string `json:"mode"`

	// Live reports whether the approval gate was backed by a real comms system rather
	// than the in-memory dry-run sink. When false, no artifact reached anyone, so no
	// human could have approved anything however long the run waited.
	Live bool `json:"live"`

	// Clusters holds one entry per registered cluster, in registry (declaration)
	// order, so multi-cluster output is stable.
	Clusters []ClusterReport `json:"clusters"`

	// Autonomy is the blast-radius posture: the circuit breakers and the auto-applies
	// this pass held back. It is ALWAYS populated and always rendered, empty or not.
	Autonomy AutonomyReport `json:"autonomy"`

	// Totals aggregates across clusters. Derived by finalize.
	Totals Totals `json:"totals"`
}

// AutonomyReport is the blast-radius layer's slice of a [Report]: what
// [budget.Budget] is bounding, what it has closed off, and what it held back.
//
// Both lists are rendered unconditionally, and empty is stated in words rather than
// left as an absent section. That is a requirement rather than a rendering taste. A
// tripped breaker means MaKlaude has stopped acting on a cluster and is waiting for a
// person who does not know they are needed; a suppressed auto-apply means an action a
// rule permitted and the shape had earned did not happen. Both look, from the outside,
// exactly like a system with nothing to do — the same invisible-nothing-happened shape
// the dev system has had to drain out of its own loop repeatedly (stale gates, dropped
// triage, ready-to-merge PRs). Printing them always, and saying "none", is what makes
// the difference legible.
type AutonomyReport struct {
	// Configured reports whether a blast-radius budget is wired at all. When false
	// nothing can be auto-applied, because the ceiling that would bound it does not
	// exist — so this is a posture statement, not the absence of one.
	Configured bool `json:"configured"`

	// Sealed reports that the budget's persisted state could not be read or written,
	// so every auto-apply is denied. See [budget.Status.Sealed]: the seal is
	// deliberately a visible state rather than a returned error, and this is where it
	// becomes visible.
	Sealed     bool   `json:"sealed"`
	SealDetail string `json:"sealDetail,omitempty"`

	// StatePath names the file the breakers and cooldowns live in, so an escalation
	// can point an operator at it instead of making them infer it from configuration.
	StatePath string `json:"statePath,omitempty"`

	// Breakers is one entry per cluster with recorded state, sorted by cluster. Closed
	// breakers are included so a reader sees a failure run building before it trips.
	Breakers []budget.Breaker `json:"breakers"`

	// Suppressed is every auto-apply a bound held back during this pass.
	Suppressed []budget.Suppression `json:"suppressed"`
}

// autonomyReport projects a budget's status, or the not-configured posture when there
// is no budget. The slices are always non-nil so the JSON form has the same shape
// whether or not anything happened.
func autonomyReport(b *budget.Budget) AutonomyReport {
	if b == nil {
		return AutonomyReport{Breakers: []budget.Breaker{}, Suppressed: []budget.Suppression{}}
	}
	s := b.Status()
	return AutonomyReport{
		Configured: true,
		Sealed:     s.Sealed,
		SealDetail: s.SealDetail,
		StatePath:  s.Path,
		Breakers:   s.Breakers,
		Suppressed: s.Suppressions,
	}
}

// Tripped returns the open breakers.
func (a AutonomyReport) Tripped() []budget.Breaker {
	out := make([]budget.Breaker, 0, len(a.Breakers))
	for _, b := range a.Breakers {
		if b.Tripped {
			out = append(out, b)
		}
	}
	return out
}

// ClusterReport is one cluster's slice of a [Report].
type ClusterReport struct {
	// Cluster is the registered cluster name this report concerns.
	Cluster string `json:"cluster"`

	// Proposals are the remediations the read-only pipeline suggested for this
	// cluster. They are what MaKlaude WOULD do; nothing here implies anything was
	// asked or done.
	Proposals []ProposalReport `json:"proposals"`

	// PreviewErrors records proposals that could not be put to a human because the
	// dry run could not be completed — almost always drift, where the next pass
	// re-proposes. It is a list of sentences rather than structured values because
	// each is a distinct situation an operator reads once.
	PreviewErrors []string `json:"previewErrors,omitempty"`

	// Gate summarizes what the approval gate did with the proposals this pass.
	Gate GateReport `json:"gate"`

	// Executions is one entry per permission slip acted on. Empty is the ordinary
	// outcome: it means nobody had approved anything yet.
	Executions []ExecutionReport `json:"executions,omitempty"`

	// Error, when non-empty, explains a per-cluster failure. It never aborts the
	// cycle over the other clusters.
	Error string `json:"error,omitempty"`
}

// ProposalReport is the serializable projection of a [remediate.Proposal] — enough
// for an operator to see what was suggested and why, without the full precondition
// set the artifact itself carries.
type ProposalReport struct {
	Identity      string `json:"identity"`
	Cluster       string `json:"cluster"`
	Operation     string `json:"operation"`
	Target        string `json:"target"`
	Reversibility string `json:"reversibility"`
	Confidence    string `json:"confidence"`
	Title         string `json:"title"`
	Intent        string `json:"intent"`
}

// GateReport mirrors [approve.Result] in a JSON-tagged form. Authorized is a count
// here rather than the slips themselves: a permission slip is not a reportable value,
// and what it authorized appears under Executions.
type GateReport struct {
	Opened     int `json:"opened"`
	Refreshed  int `json:"refreshed"`
	Held       int `json:"held"`
	Refused    int `json:"refused"`
	Withdrawn  int `json:"withdrawn"`
	Authorized int `json:"authorized"`
}

// ExecutionReport is the serializable projection of one [execute.Report].
//
// Executed and Previewed are separate, mutually exclusive fields because the question
// "did a cluster change?" has three answers, not two: nothing was sent, a preview was
// sent, and a real mutation landed. Collapsing the middle case into either neighbour is
// how a rehearsal gets read as a change or a change as a rehearsal.
//
// Executed carries [execute.Report.Executed]'s meaning EXACTLY — a real mutation
// landed, false for a preview — rather than a wider local one. Two fields of the same
// name meaning different things in adjacent packages is a bug waiting for whoever
// copies a condition from one to the other.
type ExecutionReport struct {
	Identity  string `json:"identity,omitempty"`
	Cluster   string `json:"cluster,omitempty"`
	Operation string `json:"operation,omitempty"`
	Target    string `json:"target,omitempty"`

	// Approver and Authority record who allowed this, and on what basis. Authority
	// distinguishes a human decision from a policy-waived one; see [audit.Authority].
	Approver  string `json:"approver,omitempty"`
	Authority string `json:"authority,omitempty"`

	// Executed reports that a REAL mutation landed. It is false for a preview.
	Executed bool `json:"executed"`

	// Previewed reports that the request was sent as a server-side dry run and the API
	// server accepted it — so the action is known to be valid and the cluster is
	// unchanged. It is never true alongside Executed.
	Previewed bool `json:"previewed"`

	// Attempts is how many mutating requests the action produced. The promise is that
	// a failure does not thrash, and a promise only checkable in logs is unchecked.
	Attempts int `json:"attempts"`

	// Convergence is the bounded-window verdict on whether the change took effect.
	Convergence string `json:"convergence,omitempty"`

	// CleanAbort marks the expected outcomes — drift, a stale approval — that mean
	// "nothing was sent and nothing is wrong", as distinct from a genuine failure.
	CleanAbort bool `json:"cleanAbort,omitempty"`

	// Failure and Error carry the classification and the message when something did
	// go wrong.
	Failure string `json:"failure,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Totals are the cross-cluster aggregates a caller most often wants at a glance.
type Totals struct {
	Clusters  int `json:"clusters"`
	Proposals int `json:"proposals"`

	// Asked is how many approval artifacts were opened this pass.
	Asked int `json:"asked"`

	// Authorized is how many permission slips the gate issued this pass.
	Authorized int `json:"authorized"`

	// Previewed counts actions sent as an accepted server-side dry run; Mutated counts
	// the ones that really changed a cluster. The pair is deliberate: an operator
	// auditing a dry-run rollout wants a non-zero Previewed and a zero Mutated, and one
	// number cannot say both.
	Previewed int `json:"previewed"`
	Mutated   int `json:"mutated"`

	// Failed counts executions that failed for a reason that is not a clean abort.
	Failed int `json:"failed"`

	// ByOperation counts proposals by operation token. Absent operations are omitted.
	ByOperation map[string]int `json:"byOperation,omitempty"`
}

// finalize computes the report's rolled-up totals from its per-cluster entries.
func (r *Report) finalize() {
	t := Totals{Clusters: len(r.Clusters), ByOperation: map[string]int{}}
	for i := range r.Clusters {
		c := &r.Clusters[i]
		t.Proposals += len(c.Proposals)
		for j := range c.Proposals {
			t.ByOperation[c.Proposals[j].Operation]++
		}
		t.Asked += c.Gate.Opened
		t.Authorized += c.Gate.Authorized
		for j := range c.Executions {
			e := &c.Executions[j]
			if e.Executed {
				t.Mutated++
			}
			if e.Previewed {
				t.Previewed++
			}
			if e.Failure != "" && !e.CleanAbort {
				t.Failed++
			}
		}
	}
	if len(t.ByOperation) == 0 {
		t.ByOperation = nil
	}
	r.Totals = t
}

// WriteJSON renders the report as indented JSON to w.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("operate: encoding report as JSON: %w", err)
	}
	return nil
}

// WriteText renders the report as a compact, human-readable summary to w.
//
// The header states the posture in words rather than leaving it to be inferred, and
// the disabled case says outright that nothing could have been written. An operator
// who has just enabled execution and an operator who thinks they have should be able
// to tell each other apart from the first line of output.
func (r *Report) WriteText(w io.Writer) error {
	var b strings.Builder

	fmt.Fprintf(&b, "MaKlaude remediate @ %s — execution %s; approvals %s\n",
		r.GeneratedAt.Format("2006-01-02 15:04:05 MST"), describeMode(r.Mode), describeGate(r.Live))

	if len(r.Clusters) == 0 {
		b.WriteString("\nNo clusters registered.\n")
		r.Autonomy.writeText(&b)
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

		if len(c.Proposals) == 0 {
			b.WriteString("  proposals: none\n")
		} else {
			fmt.Fprintf(&b, "  proposals (%d):\n", len(c.Proposals))
			for j := range c.Proposals {
				p := &c.Proposals[j]
				fmt.Fprintf(&b, "    - %s on %s [%s, confidence: %s] — %s\n",
					p.Operation, p.Target, p.Reversibility, p.Confidence, p.Title)
			}
		}
		for _, pe := range c.PreviewErrors {
			fmt.Fprintf(&b, "    ! %s\n", pe)
		}

		fmt.Fprintf(&b, "  approval: opened=%d refreshed=%d held=%d refused=%d withdrawn=%d authorized=%d\n",
			c.Gate.Opened, c.Gate.Refreshed, c.Gate.Held, c.Gate.Refused, c.Gate.Withdrawn, c.Gate.Authorized)

		if len(c.Executions) == 0 {
			b.WriteString("  executions: none\n")
			continue
		}
		fmt.Fprintf(&b, "  executions (%d):\n", len(c.Executions))
		for j := range c.Executions {
			e := &c.Executions[j]
			if e.Error != "" && !e.Executed {
				fmt.Fprintf(&b, "    - %s\n", describeNonExecution(e))
				continue
			}
			fmt.Fprintf(&b, "    - %s on %s: %s (approved by %s, %s); %d attempt(s), convergence %s\n",
				e.Operation, e.Target, describeEffect(e), orUnknown(e.Approver), orUnknown(e.Authority),
				e.Attempts, orUnknown(e.Convergence))
		}
	}

	r.Autonomy.writeText(&b)

	b.WriteString("\nTotals: ")
	fmt.Fprintf(&b, "%d cluster(s), %d proposal(s)", r.Totals.Clusters, r.Totals.Proposals)
	if len(r.Totals.ByOperation) > 0 {
		keys := make([]string, 0, len(r.Totals.ByOperation))
		for k := range r.Totals.ByOperation {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Totals.ByOperation[k]))
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "; asked=%d authorized=%d previewed=%d mutated=%d failed=%d\n",
		r.Totals.Asked, r.Totals.Authorized, r.Totals.Previewed, r.Totals.Mutated, r.Totals.Failed)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeText renders the blast-radius posture as two always-present sections.
//
// The sections are written even when both are empty, and the empty case says "none"
// rather than being skipped — see [AutonomyReport] for why the absence of a section
// is the one rendering this must not have.
func (a AutonomyReport) writeText(b *strings.Builder) {
	b.WriteString("\nAutonomy (blast radius): ")
	if !a.Configured {
		b.WriteString("not configured — no action can be auto-applied, so nothing is bounded and nothing is suppressed.\n")
		return
	}
	if a.StatePath != "" {
		fmt.Fprintf(b, "state %s\n", a.StatePath)
	} else {
		b.WriteString("in-memory state (nothing survives a restart)\n")
	}
	if a.Sealed {
		fmt.Fprintf(b, "  ! STATE UNREADABLE — every auto-apply is denied: %s\n", a.SealDetail)
	}

	tripped := a.Tripped()
	if len(tripped) == 0 {
		b.WriteString("  circuit breakers: none tripped\n")
	} else {
		fmt.Fprintf(b, "  circuit breakers TRIPPED (%d) — autonomy is off on these clusters until a human clears them:\n", len(tripped))
		for _, br := range tripped {
			fmt.Fprintf(b, "    - %s: %s (since %s)\n", br.Cluster, orUnknown(br.Detail),
				br.TrippedAt.UTC().Format("2006-01-02 15:04:05 MST"))
		}
	}
	// A failure run that has not yet tripped is the warning before the outage, so it
	// is reported separately rather than folded into "none tripped".
	for _, br := range a.Breakers {
		if !br.Tripped && br.ConsecutiveFailures > 0 {
			fmt.Fprintf(b, "    ~ %s: %d consecutive auto-apply failure(s), breaker still closed\n",
				br.Cluster, br.ConsecutiveFailures)
		}
	}

	if len(a.Suppressed) == 0 {
		b.WriteString("  suppressed auto-applies: none\n")
		return
	}
	fmt.Fprintf(b, "  suppressed auto-applies (%d) — eligible actions a bound held back:\n", len(a.Suppressed))
	for _, s := range a.Suppressed {
		fmt.Fprintf(b, "    - %s %s: %s (%s)\n", s.Cluster, s.Target, s.Reason, orUnknown(s.Detail))
	}
}

// describeMode renders the kill switch as a sentence rather than a token, because the
// token "disabled" and the token "enabled" differ by less on a screen than the two
// postures differ in consequence.
func describeMode(mode string) string {
	switch mode {
	case "disabled":
		return "DISABLED (propose only — no executor was built and no cluster could be written to)"
	case "dry-run":
		return "DRY-RUN (every request sent with dryRun=All — no cluster changes)"
	case "enabled":
		return "ENABLED (an approved action WILL change a cluster)"
	default:
		return mode
	}
}

// describeGate says whether an approval could have arrived at all.
func describeGate(live bool) string {
	if live {
		return "live (artifacts reach the configured comms system)"
	}
	return "dry-run (in-memory only — nobody can approve anything)"
}

// describeEffect states what one execution did to the cluster, keeping the
// preview/real distinction in words.
func describeEffect(e *ExecutionReport) string {
	switch {
	case e.Previewed:
		return "PREVIEWED (dryRun=All; cluster unchanged)"
	case e.Executed:
		return "MUTATED"
	case e.CleanAbort:
		return fmt.Sprintf("abandoned cleanly (%s)", orUnknown(e.Failure))
	default:
		return fmt.Sprintf("FAILED (%s)", orUnknown(e.Failure))
	}
}

// describeNonExecution renders an entry that never reached the runner at all — a
// wiring failure recorded against the cluster rather than against an action.
func describeNonExecution(e *ExecutionReport) string {
	if e.Target != "" {
		return fmt.Sprintf("%s on %s: %s", e.Operation, e.Target, e.Error)
	}
	return e.Error
}

// orUnknown keeps an empty field from rendering as a gap a reader has to interpret.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// toProposalReports projects proposals into the report shape, preserving the
// remediate package's ordering.
func toProposalReports(proposals []remediate.Proposal) []ProposalReport {
	out := make([]ProposalReport, 0, len(proposals))
	for i := range proposals {
		p := &proposals[i]
		out = append(out, ProposalReport{
			Identity:      string(p.Identity),
			Cluster:       p.Cluster,
			Operation:     string(p.Operation),
			Target:        p.Target.String(),
			Reversibility: p.Reversibility.String(),
			Confidence:    p.Confidence.String(),
			Title:         p.Title,
			Intent:        p.Intent,
		})
	}
	return out
}

// toExecutionReport projects one [execute.Report] plus the error that accompanied it.
//
// The error is folded in rather than reported separately because [Runner.Execute]
// returns both and they are the same information in two shapes; a report that showed
// one without the other would let a reader see a failure class with no message, or a
// message with no class.
func toExecutionReport(rep execute.Report, err error) ExecutionReport {
	out := ExecutionReport{
		Identity:   string(rep.Identity),
		Cluster:    rep.Cluster,
		Operation:  string(rep.Operation),
		Target:     rep.Target.String(),
		Approver:   rep.Approver,
		Executed:   rep.Executed,
		Attempts:   rep.Attempts,
		CleanAbort: rep.CleanAbort(),

		// A preview counts as previewed only when the API server actually accepted
		// it. rep.DryRun alone is true for every attempt made under a dry-run client,
		// including the ones that never got a response — reading it as "the action is
		// valid and the cluster is unchanged" would report a refused action as a
		// successful rehearsal.
		Previewed: rep.DryRun && rep.Outcome != nil && rep.Failure == execute.FailureNone,
	}
	if rep.Convergence != 0 || rep.Executed {
		out.Convergence = rep.Convergence.String()
	}
	if rep.Failure != 0 {
		out.Failure = rep.Failure.String()
	}
	switch {
	case rep.Error != "":
		out.Error = rep.Error
	case err != nil:
		out.Error = err.Error()
	}
	return out
}
