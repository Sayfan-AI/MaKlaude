package operate

import (
	"context"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// This file is the unattended half of the cycle, and it is the call site three earlier
// tasks were built for and did not have.
//
// [autonomy] decides one proposal at a time and cannot bound anything across proposals.
// [budget] bounds them and does not decide. [trust] answers whether a shape earned it
// and cannot see an execution. [disclose] records what happened and does not make it
// happen. Each one refused, deliberately, to reach past its own concern — which left
// exactly one thing unwritten: the order they go in. That is this file, and it is
// nothing else. There is no decision here that is not delegated, and the value of that
// is that the whole policy is unit-testable without a cluster while the sequencing is
// the only thing this layer can get wrong.
//
// # The order, and why each step is where it is
//
//  1. REVOCATIONS ARE READ FIRST, once per pass, before any cluster is touched. A
//     person's decision to stop trusting a shape must not race the pass that acts on it.
//  2. DECIDE, then check the revocation, then ADMIT. Deciding first means the report can
//     say a rule WOULD have fired and a person stopped it, which is the fact an operator
//     needs to know they are the reason nothing happened. Admitting last matters more:
//     admission CONSUMES — it counts against the cap and starts the cooldown — so asking
//     for it before the revocation check would let a revoked shape burn the pass's
//     allowance on actions it is not permitted to take.
//  3. DISCLOSE, then MINT, then EXECUTE. The artifact exists before the permission slip
//     does, because the slip carries the artifact's reference and because an action that
//     dies mid-flight must leave something behind. [approve.GrantAutonomous] refuses a
//     slip with no disclosure to point at, so the ordering is enforced rather than
//     remembered.
//  4. RECORD THE OUTCOME, then carry out the consequence, then complete the disclosure.
//     The consequence can produce a rollback, and a rollback produces audit records, so
//     the lifecycle is read once at the end — after everything that could add to it.
//
// # What is NOT auto-applied still goes to a human
//
// A bound that holds an action back says "not unattended", not "not at all". So a
// suppressed auto-apply falls through to the approval gate exactly as an untrusted shape
// does, and a tripped breaker therefore produces the fully-gated posture the breaker is
// for rather than a cluster where MaKlaude has stopped proposing. The one class that
// does NOT fall through is [autonomy.DecisionRefuse], which means the proposal must not
// be acted on by any authority — including a human's — and is reported instead.

// TrustRecorder is the trust ledger's write side, narrowed to the one method this
// package is allowed to call.
//
// It is an interface rather than a *[trust.Ledger] so that a cycle can be wired with a
// ledger, without one, or with a recorder that fails on demand — and so that this
// package cannot reach [trust.Ledger.Rebuild], which replaces the whole file and is not
// a thing a remediation pass should be able to do by accident.
//
// Both halves of the cycle write through it: the gated path hands it the
// human-approved executions that promote a fix, and the unattended path hands it
// the failures that demote a shape. Neither caller filters what it reports — what
// belongs in the history is the ledger's own rule ([trust.Entry.Counts]), decided
// behind this interface rather than in front of it.
type TrustRecorder interface {
	// RecordLifecycle projects one action's audit lifecycle onto a ledger entry and
	// records it, treating a lifecycle with no execution behind it as a no-op.
	RecordLifecycle(recs []audit.Record) error

	// NoteRecurrence records that a converged execution of this proposal did not hold,
	// because the same fault has been diagnosed again within the ledger's recurrence
	// horizon. It is a no-op when there is no such execution to contradict.
	//
	// This is on the write side rather than being inferred by the ledger because only
	// the cycle knows a fault was diagnosed AGAIN: the ledger sees executions, and a
	// recurrence is a proposal that should not have needed making. The cycle also owns
	// the injected clock the horizon is measured against.
	NoteRecurrence(identity remediate.ProposalIdentity, shape autonomy.Shape, now time.Time) error
}

// lifecycleReader is the audit sink's read side: the records for one action.
//
// [audit.Sink] is write-only by design — the execution layer must not be able to read
// back what it wrote — so recovering a finished lifecycle is a capability of the
// concrete trail rather than of the interface. *[audit.Trail] has it. A sink that does
// not is not an error: the disclosure states plainly that no records were produced,
// which is the honest rendering and is better than a cycle that refuses to run because
// its audit sink is an unusual one.
type lifecycleReader interface {
	For(id remediate.ProposalIdentity) []audit.Record
}

// autoApplyPass is what one cluster's unattended half produced: the actions taken, the
// proposals that must still go to a human, and the two classes that go to neither.
//
// Refused and Revoked are carried as rendered sentences rather than as structured values
// because each is a distinct situation an operator reads once and acts on, and because
// both are absences — a proposal that vanished from the gate without a human declining
// it. The whole point of listing them is that they would otherwise be invisible.
type autoApplyPass struct {
	Applied  []AutoApplyReport
	Deferred []remediate.Proposal
	Refused  []string
	Revoked  []string
}

// autonomyWired reports whether this cycle can auto-apply anything at all: it needs a
// ruleset, a trust oracle, a blast-radius ceiling, and somewhere to disclose.
//
// All four are required and none of them defaults. A missing ruleset or oracle gates
// everything through [autonomy.Decide] anyway; a missing budget means no ceiling exists,
// and eligibility with no ceiling is the failure [budget]'s doc opens with; a missing
// disclosure trail means an unattended mutation with no record, which is the one thing
// this milestone forbids outright. Checking here rather than relying on each layer's own
// refusal keeps the shipped posture — nothing wired — from building an executor and a
// runner in order to decline every proposal.
func (c *Cycle) autonomyWired() bool {
	return len(c.rules) > 0 && c.oracle != nil && c.budget != nil && c.disclosure != nil
}

// revocationView is one pass's reading of what a person has forbidden, and whether that
// reading succeeded.
//
// The error is a FIELD rather than a returned value that a caller might log and move
// past, because of what proceeding would mean. A failed read produces an empty map, an
// empty map is indistinguishable from "nothing is revoked", and acting unattended on
// that basis turns a network blip into a grant of authority. So the failure travels with
// the data and [revocationView.disqualifies] is what the unattended half asks.
type revocationView struct {
	shapes map[autonomy.Shape]disclose.Ref
	err    string
}

// disqualifies reports that this pass must not auto-apply anything, because what a
// person forbade is unknown.
func (r revocationView) disqualifies() bool { return r.err != "" }

// revoked reports whether a shape's autonomy has been revoked, and on which artifact.
func (r revocationView) revoked(s autonomy.Shape) (disclose.Ref, bool) {
	ref, ok := r.shapes[s]
	return ref, ok
}

// revocations reads the shapes a person has revoked, once per pass. See [revocationView]
// for why a failure is carried rather than returned.
func (c *Cycle) revocations(ctx context.Context) revocationView {
	if c.disclosure == nil {
		return revocationView{}
	}
	shapes, err := c.disclosure.Revocations(ctx)
	if err != nil {
		return revocationView{err: err.Error()}
	}
	return revocationView{shapes: shapes}
}

// noteRecurrences tells the ledger which of this pass's proposals contradict a recent
// claim of convergence, and reports the ones that did in a form a human can read.
//
// A proposal existing at all is the signal. Every proposal here was produced from a
// fresh diagnosis of a fault that is happening NOW; if the ledger holds a converged
// execution of that same proposal identity from within [trust.RecurrenceHorizon], then
// MaKlaude reported that exact fault fixed and the cluster disagrees. The ledger owns
// the horizon and the "was there such an execution" question — see
// [TrustRecorder.NoteRecurrence] — so this loop is deliberately dumb: it offers every
// proposal and lets the ledger decide which ones are contradictions.
//
// It runs whether or not autonomy is wired. A regression is a fact about the cluster
// and about a fix that already ran, not a step in the unattended path, and a cycle that
// only learned from its mistakes while autonomy happened to be configured would forget
// exactly the history a later operator turning autonomy on most needs.
//
// A recording failure is reported and not returned. The pass has already produced its
// proposals and a human still needs them; refusing to remediate because a demotion could
// not be written would convert a bookkeeping fault into an outage. The failure is loud
// in the report because it means the next pass may trust a fix this one knows is broken.
func (c *Cycle) noteRecurrences(proposals []remediate.Proposal) []string {
	if c.ledger == nil {
		return nil
	}
	now := c.now().UTC()

	var out []string
	for _, p := range proposals {
		shape := autonomy.Shape{Cluster: p.Cluster, Operation: p.Operation}
		before := c.ledgerHolds(shape)
		if err := c.ledger.NoteRecurrence(p.Identity, shape, now); err != nil {
			out = append(out, fmt.Sprintf(
				"%s on %s: a recurrence of this fault could not be recorded (%s), so a fix that may not "+
					"have held keeps whatever trust it has",
				p.Operation, p.Target.String(), err))
			continue
		}
		// Asking the ledger whether it grew is how this reports a recurrence without a
		// second copy of the horizon arithmetic living out here. Two implementations of
		// "is this a regression" would be two chances to disagree, and the disagreement
		// would be invisible: the ledger's answer is what actually demotes, and this one
		// is only what a human reads.
		if after := c.ledgerHolds(shape); after > before {
			out = append(out, fmt.Sprintf(
				"%s on %s: MaKlaude reported this fixed within the last %s and the fault is back, so the "+
					"fix did not hold and %s returns to the human gate",
				p.Operation, p.Target.String(), trust.RecurrenceHorizon, shape))
		}
	}
	return out
}

// ledgerHolds reports how many entries the ledger has for one shape, or -1 when the
// wired recorder cannot say.
//
// The count is read through an optional interface rather than added to [TrustRecorder],
// because a write-side interface that also reads is not a write-side interface, and the
// two fakes in this package's tests would then have to implement a query none of their
// assertions use. A recorder that cannot answer simply produces no recurrence lines —
// the demotion still happened, only the prose is missing, which is the right thing to
// lose.
func (c *Cycle) ledgerHolds(shape autonomy.Shape) int {
	counter, ok := c.ledger.(interface {
		Standing(autonomy.Subject) trust.Standing
	})
	if !ok {
		return -1
	}
	return counter.Standing(autonomy.Subject{Shape: shape}).Recorded
}

// autoApply runs the unattended half for one cluster.
//
// It never returns an error. A per-proposal failure is recorded against that proposal
// and the pass continues, for the same reason a per-cluster failure does not abort the
// cycle: one action MaKlaude could not take is not a reason to stop taking the others,
// and an aborted pass reports less than a completed one.
func (c *Cycle) autoApply(ctx context.Context, h *cluster.Handle, proposals []remediate.Proposal,
	revoked revocationView) autoApplyPass {

	pass := autoApplyPass{Deferred: make([]remediate.Proposal, 0, len(proposals))}
	if !c.autonomyWired() || revoked.disqualifies() {
		pass.Deferred = append(pass.Deferred, proposals...)
		return pass
	}

	// Built on first use, not up front. An executor that is constructed and used for
	// nothing still holds write authority open for the life of the process, and the
	// overwhelmingly common outcome of this loop is that nothing is admitted.
	var runner *execute.Runner

	for _, p := range proposals {
		v := autonomy.Decide(h.Name(), p, c.rules, c.oracle)
		if v.Decision == autonomy.DecisionRefuse {
			pass.Refused = append(pass.Refused, fmt.Sprintf(
				"%s on %s: %s — refused by policy, so it was not offered to a human either",
				p.Operation, p.Target.String(), v.Reason))
			continue
		}
		if !v.AutoApplies() {
			pass.Deferred = append(pass.Deferred, p)
			continue
		}

		shape := autonomy.Shape{Cluster: p.Cluster, Operation: p.Operation}
		if ref, ok := revoked.revoked(shape); ok {
			pass.Revoked = append(pass.Revoked, fmt.Sprintf(
				"%s on %s: rule %q would have auto-applied this, and autonomy for shape %s is revoked on disclosure %s; it goes to the human gate instead",
				p.Operation, p.Target.String(), v.Rule, shape, ref))
			pass.Deferred = append(pass.Deferred, p)
			continue
		}

		grant := c.budget.Admit(h.Name(), p.Target, c.now().UTC())
		if !grant.Admitted() {
			// The budget has already recorded the suppression, and the state summary
			// renders it unconditionally. Here the proposal simply takes the human gate.
			pass.Deferred = append(pass.Deferred, p)
			continue
		}

		if runner == nil {
			r, err := c.autonomousRunner(h)
			if err != nil {
				// The admission is already spent. Report it against this proposal, hand it
				// and everything after it to the gate, and stop trying to build the runner.
				pass.Applied = append(pass.Applied, autoApplyReportFor(p, v, grant).withError(err.Error()))
				pass.Deferred = append(pass.Deferred, p)
				continue
			}
			runner = r
		}
		pass.Applied = append(pass.Applied, c.applyOne(ctx, runner, p, v, grant))
	}
	return pass
}

// autonomousRunner builds the execution runner for the unattended path.
//
// It differs from the gated one in exactly one argument: the recorder is the disclosure
// trail rather than the approval gate. Everything else — the same mutator, the same
// observer, the same policy, the same [execute.Runner] — is shared on purpose. An
// unattended action is not a different KIND of action, and giving it its own execution
// path would give the half nobody reviews its own opportunities to diverge from the half
// that is reviewed.
func (c *Cycle) autonomousRunner(h *cluster.Handle) (*execute.Runner, error) {
	mutator, err := c.newMutator(h, c.mode)
	if err != nil {
		return nil, fmt.Errorf("building the write client for unattended action: %w", err)
	}
	client, err := c.newClient(h)
	if err != nil {
		return nil, fmt.Errorf("building the read-only client for convergence checks: %w", err)
	}
	runner, err := execute.New(mutator, health.NewCollector(client), c.disclosure, c.trail, c.policy)
	if err != nil {
		return nil, fmt.Errorf("building the unattended execution runner: %w", err)
	}
	return runner, nil
}

// applyOne discloses, authorizes, executes, and records one unattended action.
func (c *Cycle) applyOne(ctx context.Context, runner *execute.Runner,
	p remediate.Proposal, v autonomy.Verdict, grant budget.Grant) AutoApplyReport {

	rep := autoApplyReportFor(p, v, grant)
	action := disclose.Action{Proposal: p, Verdict: v, Grant: grant, Mode: c.mode.String(), At: c.now().UTC()}

	ref, err := c.disclosure.Open(ctx, action)
	if err != nil {
		// No record, no action. This is a policy statement rather than a transport
		// failure being tolerated: the disclosure IS the oversight for an unattended
		// mutation, so an action MaKlaude cannot disclose is one it does not take.
		return rep.withError(fmt.Sprintf("not taken: %v", err))
	}
	rep.Disclosure = string(ref)

	auth, err := approve.GrantAutonomous(approve.Request{Proposal: p}, v, grant, approve.ActionRef(ref), c.now().UTC())
	if err != nil {
		rep = rep.withError(err.Error())
		if aerr := c.disclosure.Abandon(ctx, ref, err.Error()); aerr != nil {
			rep.Error += fmt.Sprintf("; the disclosure could not be closed: %v", aerr)
		}
		return rep
	}

	execRep, execErr := runner.Execute(ctx, auth, p)
	rep.Execution = toExecutionReport(execRep, execErr)
	rep.Execution.Authority = auth.Authority().String()

	out := disclose.Outcome{Report: execRep, At: c.now().UTC()}
	if consequential(execRep) {
		out.Consequence = c.budget.RecordOutcome(p.Cluster, p.Target, budgetOutcome(execRep), c.now().UTC())
		c.rollBackIfAsked(ctx, runner, auth, execRep, &out)
	}

	// Read once, here, after everything that could add to the lifecycle: the execution,
	// and the rollback the consequence may have triggered.
	out.Records = c.lifecycleFor(p.Identity)
	c.recordTrust(&out)

	rep.Tripped = out.Consequence.Tripped
	rep.Escalated = out.Consequence.Escalate
	rep.Demoted = out.Consequence.Demote && out.DemotionErr == ""
	rep.RolledBack = out.RolledBack

	if err := c.disclosure.Complete(ctx, ref, action, out); err != nil {
		rep = rep.withError(fmt.Sprintf("the action ran and its disclosure is incomplete: %v", err))
	}
	return rep
}

// consequential reports whether this attempt is evidence about whether autonomy is
// working on this cluster — which is the only question the circuit breaker asks.
//
// Two outcomes are deliberately not evidence. A REHEARSAL changed nothing, so counting
// it would trip a breaker over a dry run and take a cluster fully gated for successfully
// doing what dry-run mode is for. A CLEAN ABORT means the target moved between the
// snapshot and the action and nothing was sent: the correct response is to re-propose
// next pass, and [execute.FailureClass.CleanAbort] exists precisely so a consumer does
// not have to decide which classes qualify.
//
// Note the asymmetry with the trust ledger, which DOES treat a drift abort as demoting.
// The two are answering different questions on purpose — see [budget.Outcome]'s doc —
// and this cycle honors both rather than picking one.
func consequential(rep execute.Report) bool { return !rep.DryRun && !rep.CleanAbort() }

// budgetOutcome reduces an execution to the single distinction the breaker makes.
//
// Anything short of "a real mutation landed AND the window saw the expected state" is a
// failure. A timed-out convergence is included in that: the action ran, the cluster did
// not reach the state MaKlaude predicted, and with nobody watching, "my model of this
// cluster may be wrong" is the reading that fails safe.
func budgetOutcome(rep execute.Report) budget.Outcome {
	if rep.Executed && rep.Convergence == execute.ConvergenceConverged {
		return budget.OutcomeSucceeded
	}
	return budget.OutcomeFailed
}

// rollBackIfAsked carries out the reversal the blast-radius layer asked for, when one is
// possible, and records why not when it is not.
//
// [budget.Consequence.RollBack] is unconditional because that package cannot know
// whether an operation is reversible — [remediate.Reversibility] travels with the
// proposal — so resolving it is the caller's job, and "asked for and not possible" is a
// distinct outcome from "not asked for" that the artifact states in words.
func (c *Cycle) rollBackIfAsked(ctx context.Context, runner *execute.Runner,
	auth *approve.Authorization, execRep execute.Report, out *disclose.Outcome) {

	if !out.Consequence.RollBack {
		return
	}
	if !execRep.Rollback.Available {
		out.RollbackSkipped = fmt.Sprintf(
			"the rollback for this operation is classified %q and nothing landed with a captured pre-state to undo",
			execRep.Rollback.Kind)
		return
	}
	rb, _ := runner.Rollback(ctx, auth, execRep)
	out.RolledBack, out.Rollback = true, rb
}

// recordTrust hands the finished action's lifecycle to the trust ledger — the write
// that re-gates a shape when the outcome demotes it.
//
// It hands over EVERY lifecycle rather than only the demoting ones, and the reason is
// agreement rather than generosity: [internal/rebuild] derives entries from every
// completed disclosure, so a live path that filtered here would build a different
// history than a rebuild of the same artifacts. What belongs in the evaluation window
// is decided in exactly one place — [trust.Entry.Counts], behind the recorder — and an
// auto-applied success is dropped there, not here. The conclusion the old
// record-only-on-demotion rule protected still holds (a success never flushes the
// approvals that earned the trust out of the window), but its premise does not:
// expiry is handled by condition-based invalidation (issue #167), not by counting
// successes into a window, so nothing here needs to hold outcomes back to preserve it.
func (c *Cycle) recordTrust(out *disclose.Outcome) {
	if c.ledger == nil {
		if out.Consequence.Demote {
			out.DemotionErr = "no trust ledger is wired, so this failure did not re-gate the shape"
		}
		return
	}
	if err := c.ledger.RecordLifecycle(out.Records); err != nil {
		out.DemotionErr = err.Error()
	}
}

// lifecycleFor recovers one action's audit records, or nothing when the sink cannot be
// read back. See [lifecycleReader].
func (c *Cycle) lifecycleFor(id remediate.ProposalIdentity) []audit.Record {
	reader, ok := c.trail.(lifecycleReader)
	if !ok {
		return nil
	}
	return reader.For(id)
}

// autoApplyReportFor projects the decision half of one unattended action, before
// anything is known about how it went.
func autoApplyReportFor(p remediate.Proposal, v autonomy.Verdict, g budget.Grant) AutoApplyReport {
	return AutoApplyReport{
		Identity:  string(p.Identity),
		Cluster:   p.Cluster,
		Operation: string(p.Operation),
		Target:    p.Target.String(),
		Rule:      v.Rule,
		Evidence:  v.Evidence,
		Reason:    v.Reason.String(),
		Admission: g.Reason.String(),
	}
}
