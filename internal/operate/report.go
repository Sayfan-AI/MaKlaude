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
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
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
	// Autonomous reports whether this pass could auto-apply anything at all: rules, a
	// trust oracle, a blast-radius ceiling and a disclosure trail, all four present.
	//
	// It is the first thing rendered and the first thing an operator should read, because
	// it is the question every other line in this section is meaningless without. A
	// deployment with a state file and no rules prints breakers and cooldowns exactly like
	// a deployment that can act, and the two differ in whether MaKlaude may touch a
	// cluster unattended.
	Autonomous bool `json:"autonomous"`

	// Off is why autonomy is not in force, in words, when [AutonomyReport.Autonomous] is
	// false and something is known about the cause. See [Cycle.autonomyOff].
	Off string `json:"off,omitempty"`

	// Rules is how many grants were loaded, and RulesPath is the file they came from, so
	// an operator can confirm the file MaKlaude read is the file they edited.
	Rules     int    `json:"rules"`
	RulesPath string `json:"rulesPath,omitempty"`

	// LedgerPath names the trust ledger the evidence for every auto-apply is derived
	// from. It is where an operator looks to answer "why did it think it could do that?",
	// and deleting it is the broadest revocation available.
	LedgerPath string `json:"ledgerPath,omitempty"`

	// QuarantinePath names the log of every period during which a deliberate fault kept
	// outcomes out of the ledger. It is reported beside LedgerPath because the two only
	// make sense together: this file is the explanation for every gap in that one, and an
	// operator asking "why did this shape never promote?" needs to be able to find it
	// without knowing that chaos exists.
	QuarantinePath string `json:"quarantinePath,omitempty"`

	// Quarantined is the windows in force right now, rendered. Empty is the ordinary
	// state and prints nothing: an unconditional "no quarantine" line would appear in
	// every report of every deployment that never runs chaos.
	Quarantined []string `json:"quarantined,omitempty"`

	// QuarantineDrops is the outcomes a window held back during this process's lifetime.
	//
	// It is reported because the alternative is the silent version of a correct
	// behaviour: the shapes on a cluster under chaos simply stop moving, which reads
	// exactly like shapes that never earned anything. Saying "three outcomes were not
	// admitted, and here is the window that explains them" is the difference between a
	// gap and a hidden one.
	QuarantineDrops []string `json:"quarantineDrops,omitempty"`

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

	// RevocationError, when non-empty, reports that the disclosure trail could not be
	// read, so what a person has revoked is unknown and NOTHING was auto-applied this
	// pass.
	//
	// It is a first-class field rather than a per-cluster error because of what it
	// costs to miss. An unreadable revocation list produces an empty one, an empty one
	// is indistinguishable from "nothing is revoked", and a pass that acted on that
	// reading would be acting unattended precisely because it could not find out what
	// it was forbidden to do. So the read failure disqualifies the unattended half of
	// the whole pass, and this is where a reader is told it happened.
	RevocationError string `json:"revocationError,omitempty"`
}

// autonomyReport projects a budget's status, or the not-configured posture when there
// is no budget. The slices are always non-nil so the JSON form has the same shape
// whether or not anything happened.
//
// The posture argument carries the four-way "can this pass act unattended?" answer that
// no budget can report on its own — a budget knows about ceilings, not about rules — so
// it is passed in by the cycle rather than derived here.
func autonomyReport(b *budget.Budget, p posture) AutonomyReport {
	r := AutonomyReport{Breakers: []budget.Breaker{}, Suppressed: []budget.Suppression{}}
	r.Autonomous, r.Off = p.autonomous, p.off
	r.Rules, r.RulesPath, r.LedgerPath = p.rules, p.rulesPath, p.ledgerPath
	r.QuarantinePath, r.Quarantined, r.QuarantineDrops = p.quarantinePath, p.quarantined, p.quarantineDrops
	if b == nil {
		return r
	}
	s := b.Status()
	r.Configured = true
	r.Sealed, r.SealDetail, r.StatePath = s.Sealed, s.SealDetail, s.Path
	r.Breakers, r.Suppressed = s.Breakers, s.Suppressions
	return r
}

// posture is the cycle's answer to "may this pass act without a person, and if not
// why not" — the fields a [budget.Budget] cannot know about.
//
// It is a small struct rather than four arguments so that adding a fifth thing autonomy
// depends on cannot silently be passed in the wrong position.
type posture struct {
	autonomous bool
	off        string
	rules      int
	rulesPath  string
	ledgerPath string

	quarantinePath  string
	quarantined     []string
	quarantineDrops []string
}

// posture reports whether this cycle can act unattended, and why not when it cannot.
//
// The fallback matters as much as the reported reason. [NewForTest] and any caller that
// wires autonomy directly through [Cycle.UseAutonomy] set no explanation, so an
// unexplained not-autonomous cycle gets one derived from what is actually missing rather
// than printing nothing — the absent section being the one rendering this must not have.
func (c *Cycle) posture() posture {
	p := posture{
		autonomous: c.autonomyWired(),
		off:        c.autonomyOff,
		rules:      len(c.rules),
		rulesPath:  c.rulesPath,
		ledgerPath: c.ledgerPath,
	}
	if reporter, ok := c.ledger.(interface{ Path() string }); ok && p.ledgerPath == "" {
		p.ledgerPath = reporter.Path()
	}
	c.readQuarantine(&p)
	if p.autonomous {
		p.off = ""
		return p
	}
	if p.off == "" {
		p.off = c.missingForAutonomy()
	}
	return p
}

// readQuarantine fills in what the trust ledger's quarantine has been doing, when the
// recorder is one.
//
// A cycle whose recorder is a bare ledger fills in nothing, which is correct: with no
// quarantine there is no window log to point at and no outcome could have been held back.
// The active windows are read against the report's own instant so a window that expired
// during the pass is not reported as still in force — a quarantine nobody closed is a
// distinct and less comfortable fact than one that is running, and reporting the second
// when the first is true would hide it.
func (c *Cycle) readQuarantine(p *posture) {
	q, ok := c.ledger.(*trust.Quarantine)
	if !ok {
		return
	}
	p.quarantinePath = c.windowsPath
	if p.quarantinePath == "" {
		p.quarantinePath = q.Windows().Path()
	}
	now := c.clock()
	for _, w := range q.Windows().All() {
		if w.Active(now) {
			p.quarantined = append(p.quarantined, w.String())
		}
	}
	for _, d := range q.Dropped() {
		p.quarantineDrops = append(p.quarantineDrops, d.String())
	}
}

// missingForAutonomy names the first thing [Cycle.autonomyWired] is missing.
//
// It reports one piece rather than all of them because an operator fixes them one at a
// time, and because the order is the order they are configured in: rules are the opt-in,
// and the rest are what the opt-in requires.
func (c *Cycle) missingForAutonomy() string {
	switch {
	case len(c.rules) == 0:
		return "no autonomy rules are loaded, so every proposal takes the human gate"
	case c.oracle == nil:
		return "autonomy rules are loaded and no trust ledger is wired, so no shape can have earned anything"
	case c.budget == nil:
		return "autonomy rules are loaded and no blast-radius ceiling is wired, so nothing bounds an unattended action"
	case c.disclosure == nil:
		return "autonomy rules are loaded and there is nowhere to disclose an unattended action, so none is taken"
	default:
		return ""
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

	// AutoApplied is one entry per action MaKlaude took on this cluster WITHOUT asking
	// anybody, in the order it took them. Empty is the shipped posture.
	AutoApplied []AutoApplyReport `json:"autoApplied,omitempty"`

	// RefusedByPolicy lists proposals the autonomy layer refused outright — an operation
	// off the catalog, an irreversible or unclassifiable action.
	//
	// They are reported here because a refusal is the one verdict that removes a
	// proposal from BOTH paths: it is not auto-applied and it is not offered to a human
	// either. Without this list, the only trace of a refused proposal would be its
	// absence from the approval gate, which reads exactly like a proposal that was never
	// made.
	RefusedByPolicy []string `json:"refusedByPolicy,omitempty"`

	// RevokedByHuman lists proposals a rule would have auto-applied and a person's
	// revocation held back. They still went to the approval gate, so nothing was lost —
	// but "your revocation is why this is waiting for you" is the one thing the gate
	// itself cannot say.
	RevokedByHuman []string `json:"revokedByHuman,omitempty"`

	// Regressions lists fixes this pass found had not held: MaKlaude reported the fault
	// converged, and the same fault is back within [trust.RecurrenceHorizon]. Each one
	// has demoted its shape back to the human gate.
	//
	// It is reported rather than left to the ledger for the same reason
	// [ClusterReport.RefusedByPolicy] is. A demotion's only other trace is a shape that
	// silently stopped auto-applying, which reads exactly like a shape that had never
	// earned trust — and "the fix you approved does not work" is a strictly more
	// important thing to tell an operator than any of the actions that did succeed.
	Regressions []string `json:"regressions,omitempty"`

	// Error, when non-empty, explains a per-cluster failure. It never aborts the
	// cycle over the other clusters.
	Error string `json:"error,omitempty"`
}

// AutoApplyReport is one action MaKlaude took with nobody watching: what permitted it,
// what earned it, where it is disclosed, and what happened.
//
// The decision half (Rule, Evidence, Reason, Admission) is populated before the action
// runs and survives every failure path, so an entry always answers "why was this
// allowed?" even when it answers nothing else. That ordering is deliberate: the failures
// most worth reading are the ones where the action did not happen, and an entry that
// lost its authorization story on the way to reporting a failure would be the least
// useful record of the most interesting case.
type AutoApplyReport struct {
	Identity  string `json:"identity"`
	Cluster   string `json:"cluster"`
	Operation string `json:"operation"`
	Target    string `json:"target"`

	// Rule is the autonomy rule that permitted the action, and Evidence is the trust
	// citation that earned it. Together they are what stands in for an approver's name;
	// see [approve.GrantAutonomous] for why neither may be empty.
	Rule     string `json:"rule"`
	Evidence string `json:"evidence"`

	// Reason is the policy layer's verdict token, always "earned-trust" for an action
	// that got this far. It is recorded rather than assumed so a report cannot imply a
	// decision path the code did not take.
	Reason string `json:"reason"`

	// Admission is the blast-radius layer's verdict token.
	Admission string `json:"admission"`

	// Disclosure is the artifact this action is recorded on — the reference a person
	// opens, and the one they apply the revocation label to. Empty means the disclosure
	// could not be opened, in which case the action was NOT taken.
	Disclosure string `json:"disclosure,omitempty"`

	// Execution is what the execution layer reported. Its Authority is always "policy".
	Execution ExecutionReport `json:"execution"`

	// Tripped, Escalated, Demoted and RolledBack are the consequences that followed a
	// failure, as carried out rather than as merely decided — Demoted is false when the
	// ledger refused the write, and the disclosure says why.
	Tripped    bool `json:"tripped,omitempty"`
	Escalated  bool `json:"escalated,omitempty"`
	Demoted    bool `json:"demoted,omitempty"`
	RolledBack bool `json:"rolledBack,omitempty"`

	// Error, when non-empty, explains a failure in the unattended machinery itself —
	// the disclosure could not be opened, the permission slip was refused, the outcome
	// could not be recorded. It is separate from [ExecutionReport.Error], which is about
	// the action.
	Error string `json:"error,omitempty"`
}

// withError stamps a machinery failure onto a report and returns it, so every abort
// path in the unattended half is one line and cannot forget to record why.
func (r AutoApplyReport) withError(msg string) AutoApplyReport {
	if r.Error == "" {
		r.Error = msg
		return r
	}
	r.Error += "; " + msg
	return r
}

// ProposalReport is the serializable projection of a [remediate.Proposal] — enough
// for an operator to see what was suggested and why, without the full precondition
// set the artifact itself carries.
type ProposalReport struct {
	Identity string `json:"identity"`

	// Fingerprint is the fix's validity token — what trust is keyed on since issue
	// #167. It is reported because "this proposal gated" and "this proposal was
	// auto-applied" are both answers about a fingerprint, and without it an operator
	// comparing two passes cannot tell a fix that changed from one that did not.
	Fingerprint string `json:"fingerprint"`

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

	// AutoApplied counts actions taken with nobody watching. It is a SEPARATE total from
	// Mutated rather than a subset spelled out in prose, because the two answer different
	// questions and only one of them is about oversight: Mutated asks how much changed,
	// this asks how much changed that no person agreed to. It is reported even when zero,
	// which is the shipped posture and the one an operator should be able to confirm at a
	// glance.
	AutoApplied int `json:"autoApplied"`

	// ByOperation counts proposals by operation token. Absent operations are omitted.
	ByOperation map[string]int `json:"byOperation,omitempty"`
}

// count folds one execution into the totals. It is shared by the gated and unattended
// paths so the two cannot come to disagree about what counts as a mutation, a preview,
// or a failure — which they would, since only one of the two is under a human's eye.
func (t *Totals) count(e *ExecutionReport) {
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
			t.count(&c.Executions[j])
		}
		// An unattended action counts toward the same Mutated/Previewed/Failed totals as
		// an approved one — it is the same execution through the same runner, and a total
		// that excluded it would understate what happened to the cluster. AutoApplied is
		// the extra column, not a replacement for the ordinary ones.
		for j := range c.AutoApplied {
			t.AutoApplied++
			t.count(&c.AutoApplied[j].Execution)
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

		writeAutoApplied(&b, c)

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
	fmt.Fprintf(&b, "; asked=%d authorized=%d previewed=%d mutated=%d failed=%d auto-applied=%d\n",
		r.Totals.Asked, r.Totals.Authorized, r.Totals.Previewed, r.Totals.Mutated, r.Totals.Failed, r.Totals.AutoApplied)

	_, err := io.WriteString(w, b.String())
	return err
}

// writeAutoApplied renders one cluster's unattended half: what MaKlaude did without
// asking, what policy refused outright, and what a person's revocation held back.
//
// The header line is printed UNCONDITIONALLY, "none" included, and it is printed above
// the approval line rather than below it. Both are the same argument: an operator
// scanning a pass has to be able to tell "nothing was auto-applied" from "the section
// that would have said so is missing", and the count that matters most is the one for
// actions nobody agreed to. The two absence lists follow only when non-empty — they are
// exceptions rather than a posture, and printing "refused by policy: none" every pass on
// every cluster is how a reader learns to skip the block that eventually says something.
func writeAutoApplied(b *strings.Builder, c *ClusterReport) {
	if len(c.AutoApplied) == 0 {
		b.WriteString("  auto-applied (no human): none\n")
	} else {
		fmt.Fprintf(b, "  AUTO-APPLIED WITH NO HUMAN REVIEW (%d):\n", len(c.AutoApplied))
		for i := range c.AutoApplied {
			a := &c.AutoApplied[i]
			fmt.Fprintf(b, "    - %s on %s: %s (rule %s; disclosed at %s)\n",
				a.Operation, a.Target, describeEffect(&a.Execution), orUnknown(a.Rule), orUnknown(a.Disclosure))
			if a.Error != "" {
				fmt.Fprintf(b, "      ! %s\n", a.Error)
			}
			if consequences := describeConsequences(a); consequences != "" {
				fmt.Fprintf(b, "      %s\n", consequences)
			}
		}
	}
	for _, r := range c.RefusedByPolicy {
		fmt.Fprintf(b, "    x refused by policy: %s\n", r)
	}
	for _, r := range c.RevokedByHuman {
		fmt.Fprintf(b, "    o revoked by a human: %s\n", r)
	}
	// Rendered last and marked distinctly, because it is the only line in this section
	// that is about a PAST action being wrong rather than about a present one being
	// held back. An operator skimming for what needs their attention should not have to
	// distinguish it from the ordinary gating traffic above.
	for _, r := range c.Regressions {
		fmt.Fprintf(b, "    ! regression: %s\n", r)
	}
}

// describeConsequences renders what followed a failed unattended action, or empty when
// nothing did.
func describeConsequences(a *AutoApplyReport) string {
	var parts []string
	if a.Tripped {
		parts = append(parts, "BREAKER TRIPPED — autonomy is now off for this cluster until a human clears it")
	}
	if a.RolledBack {
		parts = append(parts, "rolled back")
	}
	if a.Demoted {
		parts = append(parts, "shape demoted in the trust ledger")
	}
	if a.Escalated {
		parts = append(parts, "escalated (needs:human)")
	}
	if len(parts) == 0 {
		return ""
	}
	return "consequences: " + strings.Join(parts, "; ")
}

// writeRevocationError reports an unreadable disclosure trail.
//
// It is its own method, and it is called on BOTH branches of [AutonomyReport.writeText],
// because the two conditions are independent: the disclosure trail can be wired and
// unreadable while no blast-radius budget exists at all. Printing it only under the
// configured branch would hide the failure in exactly the configuration where the
// operator is midway through enabling autonomy and most needs to be told.
func (a AutonomyReport) writeRevocationError(b *strings.Builder) {
	if a.RevocationError == "" {
		return
	}
	fmt.Fprintf(b, "  ! THE DISCLOSURE TRAIL COULD NOT BE READ — nothing was auto-applied this pass, because what a human has revoked is unknown: %s\n",
		a.RevocationError)
}

// writeUnattended renders the one line that says whether MaKlaude may change a cluster
// without asking, above everything that only makes sense once that is answered.
//
// The ON line names the rules file and the ledger rather than just asserting the state,
// because "autonomy is on" is a claim an operator should be able to check: the file they
// edited is the file MaKlaude read, and the ledger named here is the evidence behind any
// auto-apply in the report — and the file whose deletion revokes every earned shape.
func (a AutonomyReport) writeUnattended(b *strings.Builder) {
	b.WriteString("\nUnattended actions: ")
	if !a.Autonomous {
		b.WriteString("OFF")
		if a.Off != "" {
			fmt.Fprintf(b, " — %s", a.Off)
		}
		b.WriteString("\n")
		return
	}
	fmt.Fprintf(b, "ON — %d autonomy rule(s)", a.Rules)
	if a.RulesPath != "" {
		fmt.Fprintf(b, " from %s", a.RulesPath)
	}
	if a.LedgerPath != "" {
		fmt.Fprintf(b, "; trust ledger %s", a.LedgerPath)
	}
	b.WriteString("\n  a shape still gates until it has EARNED autonomy, and every auto-apply is disclosed\n")
}

// writeText renders the blast-radius posture as two always-present sections.
//
// The sections are written even when both are empty, and the empty case says "none"
// rather than being skipped — see [AutonomyReport] for why the absence of a section
// is the one rendering this must not have.
func (a AutonomyReport) writeText(b *strings.Builder) {
	a.writeUnattended(b)
	b.WriteString("Autonomy (blast radius): ")
	if !a.Configured {
		b.WriteString("not configured — no action can be auto-applied, so nothing is bounded and nothing is suppressed.\n")
		a.writeRevocationError(b)
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
	a.writeRevocationError(b)

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
	} else {
		fmt.Fprintf(b, "  suppressed auto-applies (%d) — eligible actions a bound held back:\n", len(a.Suppressed))
		for _, s := range a.Suppressed {
			fmt.Fprintf(b, "    - %s %s: %s (%s)\n", s.Cluster, s.Target, s.Reason, orUnknown(s.Detail))
		}
	}
	a.writeQuarantine(b)
}

// writeQuarantine renders the trust ledger's chaos quarantine, and prints nothing at all
// when no window is in force and nothing was held back.
//
// Silence in the ordinary case is deliberate. Every deployment that never runs chaos
// would otherwise carry two lines saying nothing happened, and a report that pads itself
// with all-clear notices is one an operator stops reading — which is the same argument
// the deterministic dev-system nets make for empty sections meaning all-clear.
func (a AutonomyReport) writeQuarantine(b *strings.Builder) {
	if len(a.Quarantined) == 0 && len(a.QuarantineDrops) == 0 {
		return
	}
	fmt.Fprintf(b, "  trust ledger QUARANTINED — outcomes on these clusters are not admissible as trust evidence")
	if a.QuarantinePath != "" {
		fmt.Fprintf(b, " (window log %s)", a.QuarantinePath)
	}
	b.WriteString(":\n")
	for _, w := range a.Quarantined {
		fmt.Fprintf(b, "    - %s\n", w)
	}
	for _, d := range a.QuarantineDrops {
		fmt.Fprintf(b, "    ~ %s\n", d)
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
			Fingerprint:   string(p.Fingerprint()),
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
