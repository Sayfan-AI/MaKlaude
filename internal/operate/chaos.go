package operate

import (
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// This file is where a deliberate fault meets the machinery built for remediation, and
// it exists so there is exactly ONE place that says how the two relate.
//
// The whole of Milestone 5's safety argument is that one function decides, one layer
// bounds, one trail records. Milestone 6 adds MaKlaude's first deliberate write path,
// and the tempting shape for it — a chaos runner with its own eligibility check, its own
// ceiling and its own record — would give the most dangerous class of action in the
// system its own unreviewed policy. So instead:
//
//   - The DECISION is [autonomy.DecideRequest], the same function a remediation goes
//     through, reached through [chaos.Proposal.Request]. It can only ever gate — see
//     [autonomy.ClassChaos] — and this file asserts that rather than assuming it.
//   - The CEILING is the same [budget.Budget]: the same per-pass cap, the same
//     per-target cooldown, the same per-cluster circuit breaker. An experiment on a
//     cluster whose breaker is open does not run, and an experiment spends the same
//     allowance an unattended remediation would.
//   - The LEDGER is quarantined for the duration, because an experiment's outcomes are
//     not evidence about whether MaKlaude's fixes work. See [trust.Quarantine].
//
// # Why admission is not asked for at proposal time
//
// [budget.Budget.Admit] CONSUMES: it counts against the pass cap and starts the target's
// cooldown. A chaos proposal always gates, so between the decision and the injection
// there is a human, and asking for admission before they answer would spend the
// cluster's allowance on experiments that were never approved — the same mistake the
// unattended half avoids by admitting after the revocation check rather than before it.
// So the two halves are separate calls on purpose: [Cycle.DecideChaos] costs nothing and
// can be run on a whole basket of candidate experiments, and [Cycle.AdmitChaos] is called
// once, immediately before an approved experiment is injected.

// ChaosDecision is what the decision half concluded about one experiment.
//
// It is a distinct type from [AutoApplyReport] and [ProposalReport] rather than a shared
// one with a class field, which is the fourth of task T4's done criteria: a chaos
// experiment must render as an unmistakably distinct class in the trail and never as a
// remediation. A consumer holding this value cannot mistake it for a fix, and a renderer
// cannot accidentally list it among them.
type ChaosDecision struct {
	// Class is always [autonomy.ClassChaos]'s token. It is recorded explicitly, even
	// though the type already implies it, because the token is what survives into JSON
	// and into whatever reads the artifact later.
	Class string `json:"class"`

	// Identity is the experiment's stable key, in the same shape a remediation's is so
	// one audit trail can hold both.
	Identity string `json:"identity"`

	Cluster string `json:"cluster"`

	// Action is the chaos action, and Operation is how it is recorded as an operation —
	// namespaced under [chaos.OperationPrefix] so it can never be read as a catalog
	// operation.
	Action    string `json:"action"`
	Operation string `json:"operation"`

	// Target is the rendered blast scope the ceiling bounds. See
	// [chaos.Proposal.BlastTarget] for why it describes the fault's reach rather than
	// the custom resource that requests it.
	Target string `json:"target"`

	// SelfLimit is how this fault ends without MaKlaude doing anything. It is on the
	// decision because it is the fact that makes an injection reviewable, and a reviewer
	// should not have to know the action catalog to find it.
	SelfLimit string `json:"selfLimit"`

	// Description is the sentence a human approves or declines.
	Description string `json:"description"`

	// Decision and Reason are the policy verdict. Decision is always "gate" for this
	// class; see [Verdict] below for what happens if it ever is not.
	Decision string `json:"decision"`
	Reason   string `json:"reason"`

	// Verdict is the whole verdict, carried so a caller can assert on it without
	// re-deriving it from the tokens.
	Verdict autonomy.Verdict `json:"-"`

	// Error, when non-empty, means this experiment was not put to anybody: the proposal
	// was malformed, or the policy answered something this path refuses to act on.
	Error string `json:"error,omitempty"`
}

// Gated reports whether this experiment may be offered to a human.
func (d ChaosDecision) Gated() bool {
	return d.Error == "" && d.Verdict.Decision == autonomy.DecisionGate
}

// DecideChaos runs one experiment through the same decision function a remediation goes
// through, and reports what it concluded.
//
// It never returns an error and never touches a cluster: it validates the proposal,
// asks the policy, and renders the answer. Nothing is admitted, nothing is injected, and
// no allowance is spent — see the file doc.
//
// The one thing it does beyond delegating is REFUSE TO PROCEED on an answer that should
// be impossible. If the policy ever returns anything other than a gate for a chaos
// proposal, this path stops and says so rather than acting on it. That check is
// unreachable today — [autonomy.Class.MayAutoApply] is false for chaos, so the decision
// gates before the ruleset is read, and no rule can even name a chaos operation — and it
// is here because "unreachable" is a property of the current code rather than of the
// requirement. A future edit that made an experiment auto-appliable would be a serious
// bug, and the cheapest place to catch it is the call site that would otherwise carry it
// out.
func (c *Cycle) DecideChaos(p chaos.Proposal) ChaosDecision {
	d := ChaosDecision{
		Class:       autonomy.ClassChaos.String(),
		Identity:    string(p.Identity()),
		Cluster:     p.Cluster,
		Action:      string(p.Experiment.Action),
		Operation:   string(p.Operation()),
		Target:      p.BlastTarget().String(),
		SelfLimit:   p.Experiment.SelfLimit().String(),
		Description: p.Describe(),
	}

	if err := p.Validate(); err != nil {
		d.Error = err.Error()
		return d
	}

	v := autonomy.DecideRequest(p.Request(), c.rules, c.oracle)
	d.Verdict, d.Decision, d.Reason = v, v.Decision.String(), v.Reason.String()

	if v.Decision != autonomy.DecisionGate {
		d.Error = fmt.Sprintf(
			"the policy answered %q for a chaos experiment, and this path only carries out %q; "+
				"an experiment is never eligible to run unattended, so this is a defect in the decision "+
				"path rather than a permission to act",
			v.Decision, autonomy.DecisionGate)
	}
	return d
}

// ChaosAdmission is the blast-radius half's answer for one approved experiment: whether
// the ceiling lets it run now, and the window that must be opened if it does.
type ChaosAdmission struct {
	// Grant is the budget's verdict. An experiment whose grant does not admit is NOT
	// injected — unlike a remediation, which falls through to the human gate when the
	// ceiling holds it back, because here the human has already said yes and the ceiling
	// is the only remaining bound.
	Grant budget.Grant `json:"grant"`

	// Window is the quarantine window opened for this experiment, zero when nothing was
	// admitted or no ledger is being quarantined.
	Window trust.Window `json:"window"`

	// Error, when non-empty, explains why the admission could not be completed. An
	// experiment with an error here must not be injected: it means the window that keeps
	// its outcomes out of the trust ledger does not exist, and injecting anyway would
	// corrupt the history the quarantine exists to protect.
	Error string `json:"error,omitempty"`
}

// Admitted reports whether this experiment may now be injected.
func (a ChaosAdmission) Admitted() bool { return a.Error == "" && a.Grant.Admitted() }

// AdmitChaos asks the blast-radius ceiling whether one approved experiment may run now,
// and opens its quarantine window if so.
//
// Call it immediately before injecting, never before the human has approved: it consumes
// the cluster's pass allowance and starts the blast target's cooldown.
//
// The two effects are ordered admission-then-window, and the order is the safety
// argument. Opening the window first would leave a quarantine in force for an experiment
// the ceiling then refused — a gap in the trust history explained by a fault that never
// happened. Admitting first means the only failure left is a window that could not be
// recorded, and that case refuses the injection outright rather than proceeding: an
// experiment whose outcomes would land in the ledger as genuine failures is worse than an
// experiment that does not run.
//
// settle is how long after the fault's own end the cluster is still considered to be
// recovering from it, and it is a parameter rather than a constant because it is a
// property of the cluster and the caller's patience rather than of chaos. The window's
// declared ceiling is the fault's duration plus settle, which is what bounds the
// quarantine if this process dies before closing it — see [trust.Windows.Begin].
func (c *Cycle) AdmitChaos(p chaos.Proposal, settle time.Duration) ChaosAdmission {
	var a ChaosAdmission

	if err := p.Validate(); err != nil {
		a.Error = err.Error()
		return a
	}
	if c.budget == nil {
		a.Error = "no blast-radius budget is wired, so nothing bounds how many faults this cycle could inject; " +
			"an experiment with no ceiling is not admitted"
		return a
	}

	at := c.now().UTC()
	a.Grant = c.budget.Admit(p.Cluster, p.BlastTarget(), at)
	if !a.Grant.Admitted() {
		return a
	}

	win, err := c.openChaosWindow(p, at, settle)
	if err != nil {
		a.Error = fmt.Sprintf("the admission was granted and the trust ledger could not be quarantined (%v), "+
			"so the experiment was not injected: its outcomes would have been recorded as genuine failures", err)
		return a
	}
	a.Window = win
	return a
}

// openChaosWindow records the quarantine window covering one experiment.
//
// A cycle whose recorder is not a quarantine gets a zero window and no error, which is
// the honest answer for a cycle with no trust ledger to protect: there is no history for
// a deliberate fault to corrupt. A cycle that HAS a ledger and no quarantine in front of
// it is the case that must not pass silently — see [Cycle.chaosQuarantine].
func (c *Cycle) openChaosWindow(p chaos.Proposal, at time.Time, settle time.Duration) (trust.Window, error) {
	q, err := c.chaosQuarantine()
	if err != nil {
		return trust.Window{}, err
	}
	if q == nil {
		return trust.Window{}, nil
	}
	return q.Windows().Begin(p.Cluster, chaosWindowReason(p), at, at.Add(p.Experiment.Duration).Add(settle))
}

// CloseChaosWindow ends a quarantine window once the experiment is over and the cluster
// has settled.
//
// Closing is best-effort in the sense that failing to close does not corrupt anything —
// the window's declared ceiling expires it either way, which is exactly why the ceiling
// is mandatory — but the failure is returned rather than swallowed, because a window that
// stays open until its ceiling means the ledger ignores real outcomes for longer than it
// had to, and an operator debugging a shape that will not promote needs to know that
// happened.
func (c *Cycle) CloseChaosWindow(win trust.Window) (trust.Window, error) {
	if win.Cluster == "" {
		return trust.Window{}, nil
	}
	q, err := c.chaosQuarantine()
	if err != nil {
		return trust.Window{}, err
	}
	if q == nil {
		return trust.Window{}, fmt.Errorf("operate: no quarantine is wired, so window %s cannot be closed", win)
	}
	return q.Windows().End(win, c.now().UTC())
}

// RecordChaosOutcome tells the blast-radius layer how an injection ended.
//
// An experiment's outcome is evidence about the same thing an unattended remediation's
// is — whether MaKlaude's write path is working on this cluster — so it feeds the same
// breaker. Note what it is NOT evidence about: whether a FIX works, which is why nothing
// here touches the trust ledger and why the quarantine holds back the outcomes the
// cluster produces while the fault is live.
//
// A failed injection therefore counts toward tripping the breaker, and a tripped breaker
// stops chaos and unattended remediation alike. That is deliberate: an injection that
// fails is MaKlaude asking a cluster to do something and being unable to tell whether it
// happened, which is the condition the breaker exists for regardless of which class of
// action produced it.
func (c *Cycle) RecordChaosOutcome(p chaos.Proposal, outcome budget.Outcome) budget.Consequence {
	if c.budget == nil {
		return budget.Consequence{}
	}
	return c.budget.RecordOutcome(p.Cluster, p.BlastTarget(), outcome, c.now().UTC())
}

// chaosQuarantine returns the quarantine in front of this cycle's trust ledger, nil when
// there is no ledger at all, and an error when there is a ledger that is NOT quarantined.
//
// The error case is the one worth having a function for. A cycle that records trust
// through a bare ledger and injects faults would demote every shape on the cluster for
// failures it caused on purpose, and it would do it silently — the shapes would simply
// stop auto-applying, which is indistinguishable from shapes that never earned anything.
// So it is refused at the point of injection rather than discovered later from a history
// that no longer says what happened.
func (c *Cycle) chaosQuarantine() (*trust.Quarantine, error) {
	if c.ledger == nil {
		return nil, nil
	}
	q, ok := c.ledger.(*trust.Quarantine)
	if !ok {
		return nil, fmt.Errorf("operate: this cycle records trust through %T rather than a *trust.Quarantine, so a "+
			"deliberate fault would demote every shape on the cluster for failing on purpose; wire the ledger "+
			"through trust.NewQuarantine before injecting one", c.ledger)
	}
	return q, nil
}

// chaosWindowReason renders why a window was opened, in the words a person reading the
// window log a year later needs: which experiment, on what scope.
func chaosWindowReason(p chaos.Proposal) string {
	return fmt.Sprintf("chaos experiment %s (%s) on %s", p.Experiment.Action, p.Identity(), p.BlastTarget().String())
}
