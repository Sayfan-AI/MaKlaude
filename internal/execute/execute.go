// Package execute performs an action a human has approved. It is the policy layer
// over MaKlaude's write path, and the last thing that runs before a real cluster
// changes.
//
// # Why this is not more methods on kube.Executor
//
// [kube.Executor] answers a transport question: how do I send ONE mutating request
// safely? Its answers are structural — a client whose transport admits exactly one
// (method, path) pair, an optimistic-concurrency precondition on every call, a kill
// switch that refuses to build a write-capable object at all. It knows nothing
// about proposals, approvals, or what the cluster looked like a moment ago, and it
// should not: everything it enforces, it enforces for any caller.
//
// This package answers a policy question: SHOULD this run right now, what did the
// world look like before it ran, and did it actually work? Those need a proposal, a
// permission slip, a live view of the cluster, and a clock. Keeping them apart
// means the transport's guarantees stay simple enough to audit by reading one file,
// and the policy's judgment stays testable without a network.
//
// # What an execution actually is
//
// [Runner.Execute] is a fixed sequence, and the order is the safety property:
//
//  1. Refuse anything without a valid [approve.Authorization] that matches this
//     exact proposal, on this exact cluster.
//  2. Re-read the kill switch. Not the one that was set when the process started —
//     the one that is set now.
//  3. Refuse an operation with no plan, an irreversible action, and an operation the
//     write path cannot express faithfully. Each is a refusal a human reads, never a
//     silent skip.
//  4. Read the cluster ONCE, and re-check every precondition the approver was shown
//     against that read. Drift aborts here, having sent nothing.
//  5. Capture the pre-state from THAT SAME read, so what is recorded is provably the
//     state that was just checked.
//  6. Send exactly one mutating request — retried only for a response that certainly
//     did not apply, and never more than a small fixed number of times.
//  7. Record the execution on the approval trail immediately, because that record is
//     what stops a second one.
//  8. Watch for convergence for a BOUNDED window, then report whatever was seen.
//
// # Three things it deliberately does not do
//
// It does not block. Convergence is watched for a bounded window and then reported
// — [ConvergenceTimedOut] is a verdict, not a failure, and never triggers another
// request. A monitoring loop with other clusters to look at must not be held by one
// slow rollout.
//
// It does not thrash. A failure stops the action; it does not re-drive it. The only
// retryable response is one the API server rejected outright (see [isRetryable]),
// because a mutation whose outcome is unknown must never be repeated.
//
// It does not roll back on its own. A rollback is itself a mutating action, and
// MaKlaude taking one unbidden — because it did not like what it saw in the
// observation window — would be exactly the unapproved autonomy this milestone
// exists to prevent. So [Runner.Execute] captures the pre-state and reports that a
// rollback is available; [Runner.Rollback] performs one only when a caller asks,
// holding the same permission slip that authorized the action being undone.
//
// # Nothing constructs a Runner yet
//
// As of this change no configuration surface, command, or scheduled loop builds one
// — the same posture [kube.NewExecutor] shipped in: the capability exists, is
// tested, and is not reachable from a running MaKlaude. Wiring it up is a separate,
// deliberate step, so "the write path is off" remains a fact about what the binary
// can do rather than a setting someone has to get right.
package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Sentinel errors. Every refusal and every failure this package produces wraps one
// of these — or a [kube] sentinel from the write path underneath it — so a caller
// can branch on the class with errors.Is rather than on prose. Each also has a
// [FailureClass] counterpart in the [Report]; the error is for control flow, the
// class is for the audit trail and the humans reading it.
var (
	// ErrNotAuthorized means there was no valid permission slip for this exact
	// action. Nothing was read and nothing was sent.
	ErrNotAuthorized = errors.New("execute: no valid authorization for this action")

	// ErrClusterMismatch means the authorization, the proposal, and the write client
	// did not all name the same cluster. It is its own sentinel because it is the one
	// mistake whose consequence is acting on the wrong cluster.
	ErrClusterMismatch = errors.New("execute: authorization, proposal, and write client name different clusters")

	// ErrIrreversible means MaKlaude refused an action whose effect cannot be undone
	// and which nothing repairs on its own.
	ErrIrreversible = errors.New("execute: refusing an irreversible action")

	// ErrUnsupportedOperation means the operation has no plan here, or the write path
	// has no primitive that can express it faithfully. Refused rather than
	// approximated.
	ErrUnsupportedOperation = errors.New("execute: the write path cannot perform this operation")

	// ErrRefused means MaKlaude declined a validly-authorized action for a reason
	// specific to this attempt rather than to the operation: an approval carrying
	// nothing to re-check, or a target whose prior state cannot be recorded. Both
	// share a class ([FailureRefused]) with the two sentinels above, which callers
	// branch on; this one exists so the refusals that have no more specific name are
	// still named.
	ErrRefused = errors.New("execute: refusing to perform this action")

	// ErrKillSwitch means the global write kill switch did not permit the action at
	// the moment it would have run.
	ErrKillSwitch = errors.New("execute: execution is not enabled")

	// ErrUnobservable means the cluster could not be read, so the preconditions could
	// not be evaluated. An unreadable cluster is never treated as one where they hold.
	ErrUnobservable = errors.New("execute: the cluster cannot be read")

	// ErrPreconditionDrift means at least one condition the approver was shown no
	// longer holds. Nothing was sent. This is the expected outcome of a stale
	// approval, not a malfunction — see [FailureClass.CleanAbort].
	ErrPreconditionDrift = errors.New("execute: a precondition no longer holds")

	// ErrRecord means the action ran but could not be recorded on the approval trail,
	// so the artifact is missing the label that prevents a second execution.
	ErrRecord = errors.New("execute: the execution could not be recorded on the approval trail")

	// ErrNotRollbackable means a rollback was asked for something that has none: an
	// action that never ran, a preview, or an operation whose effect has no inverse.
	ErrNotRollbackable = errors.New("execute: this action cannot be rolled back")
)

// Runner performs authorized actions against exactly one cluster and reports what
// happened.
//
// It is bound to one cluster by construction, through its [Mutator], and it checks
// that binding against every authorization it is given rather than trusting it —
// multi-cluster isolation is a first-class concern and this is the layer where
// getting it wrong would actually change something.
//
// A Runner holds no mutable state and is safe for concurrent use. It does not
// serialize actions against each other: two concurrent executions against the same
// object are prevented by the optimistic-concurrency precondition each carries
// (the second gets a conflict and aborts cleanly), not by a lock here.
type Runner struct {
	mutator  Mutator
	observer Observer
	recorder Recorder
	policy   Policy
}

// New builds a runner over the write path, the live cluster view, and the approval
// trail.
//
// All three dependencies are required and a missing one is an error rather than a
// tolerated nil. The recorder in particular has no safe default: without it an
// execution is never marked on the trail, so the next reconciliation pass would
// authorize the same action again, and "exactly once" would quietly become "once
// per cycle". A no-op recorder is a thing a caller must construct on purpose, not
// something they get by leaving an argument out.
//
// A zero [Policy] takes the shipped defaults; see [Policy] for why a forgotten
// field must not be read literally.
func New(mutator Mutator, observer Observer, recorder Recorder, policy Policy) (*Runner, error) {
	switch {
	case mutator == nil:
		return nil, errors.New("execute: a runner requires a write client")
	case observer == nil:
		return nil, errors.New("execute: a runner requires a cluster observer (preconditions and convergence cannot be checked without one)")
	case recorder == nil:
		return nil, errors.New("execute: a runner requires a recorder (single-execution is enforced on the approval trail, not in memory)")
	}
	return &Runner{
		mutator:  mutator,
		observer: observer,
		recorder: recorder,
		policy:   policy.normalized(),
	}, nil
}

// Cluster returns the registered name of the cluster this runner can act on.
func (r *Runner) Cluster() string { return r.mutator.Name() }

// Policy returns the runner's effective policy, with defaults already applied.
func (r *Runner) Policy() Policy { return r.policy }

// Execute runs one approved action end to end and returns a full account of it.
//
// The [Report] is returned on every path, including the ones where nothing was
// sent, so a caller never has to distinguish "no report" from "nothing happened".
// The error is non-nil whenever [Report.Failure] is not [FailureNone], and it is
// the same information in the shape Go callers branch on; a drifted precondition
// returns [ErrPreconditionDrift] and is an expected outcome rather than a
// malfunction (see [FailureClass.CleanAbort]).
//
// It is safe to call for an action that has already run only in the sense that the
// preconditions will almost certainly refuse it. The real guard against a second
// execution is durable and lives on the approval trail: the gate does not issue a
// second authorization for an artifact already marked executed. This function
// enforces the "exactly" in "exactly once" by recording that mark; it cannot
// enforce it by itself, and does not pretend to.
func (r *Runner) Execute(ctx context.Context, auth *approve.Authorization, p remediate.Proposal) (Report, error) {
	mode := r.mutator.Mode()
	rep := Report{
		Identity:      p.Identity,
		Cluster:       p.Cluster,
		Operation:     p.Operation,
		Target:        p.Target,
		Reversibility: p.Reversibility,
		Mode:          mode,
		StartedAt:     time.Now().UTC(),
	}
	if auth.Valid() {
		rep.Approver = auth.Approver()
		rep.ApprovalRef = string(auth.Ref())
	}

	// Permission first: everything below this point reads or writes a real cluster.
	if class, err := r.checkAuthorization(auth, p); err != nil {
		return rep.fail(class, err)
	}

	// The kill switch is read here, at execution time, and not remembered from
	// construction. A runner may outlive the posture it was built under.
	if !modePermitsAction(mode) {
		return rep.fail(FailureKillSwitch, fmt.Errorf("%w: the write path is in mode %q", ErrKillSwitch, mode))
	}

	// What this operation is, whether it can be undone, and whether MaKlaude is
	// willing to perform it at all.
	pl, ok := planFor(p.Operation)
	if !ok {
		return rep.fail(FailureRefused, fmt.Errorf("%w: operation %q has no plan in the execution layer", ErrUnsupportedOperation, p.Operation))
	}
	rep.Rollback = RollbackPlan{Kind: pl.rollback, Note: pl.rollbackNote}
	if class, err := checkActionable(pl, p); err != nil {
		return rep.fail(class, err)
	}

	// The conditions the APPROVER was shown, taken from the permission slip rather
	// than from the proposal the caller passed in. [approve.Authorization.Matches]
	// compares identity, operation, and target — not preconditions — so reading them
	// from the proposal would let a caller present a re-derived proposal with weaker
	// conditions and the same identity. The slip is the record of what a human
	// actually consented to.
	conditions := auth.Preconditions()
	if len(conditions) == 0 {
		return rep.fail(FailureRefused, fmt.Errorf(
			"%w: the authorization carries no preconditions, so nothing could be re-checked before acting", ErrRefused))
	}

	// One read of the cluster serves both the precondition re-check and the pre-state
	// capture, which is what makes them provably consistent with each other.
	idx, class, err := r.readCluster(ctx, p.Cluster)
	if err != nil {
		return rep.fail(class, err)
	}

	rep.Preconditions = recheckPreconditions(idx, conditions, p.Target)
	if !allHeld(rep.Preconditions) {
		return rep.fail(FailureDrifted, fmt.Errorf("%w: %s", ErrPreconditionDrift, renderDrift(rep.DriftedPreconditions())))
	}

	pre, err := capturePreState(idx, p.Target)
	if err != nil {
		return rep.fail(FailureRefused, fmt.Errorf(
			"%w: MaKlaude does not change an object whose prior state it cannot record: %w", ErrRefused, err))
	}
	rep.PreState = pre

	// The action's timestamp is fixed here, before the first attempt, so a retried
	// attempt re-sends a byte-identical request rather than a second, different one.
	at := time.Now().UTC()
	out, attempts, err := r.send(ctx, func(ctx context.Context) (*kube.Outcome, error) {
		return pl.mutate(ctx, r.mutator, p.Target, at)
	})
	rep.Attempts = attempts
	switch {
	case errors.Is(err, kube.ErrPreconditionConflict):
		// The drift the pre-check is designed to catch, caught instead by the API
		// server. Nothing was applied, and the response is the same: re-propose.
		return rep.fail(FailureConflict, err)
	case err != nil:
		return rep.fail(FailureExecute, err)
	case out == nil:
		return rep.fail(FailureExecute, errors.New("execute: the write path reported neither an outcome nor an error"))
	}
	rep.Outcome = out

	// A preview is not an execution. The two flags are derived from the mode AND the
	// outcome, and any hint of a dry run wins, because the direction that must never
	// be wrong is claiming a preview changed nothing when it did not — a report that
	// under-claims costs a re-ask, one that over-claims records an execution that
	// never happened and permanently blocks the real one.
	rep.DryRun = mode == kube.ExecuteDryRun || out.DryRun
	rep.Executed = !rep.DryRun
	if !rep.Executed {
		rep.ConvergenceDetail = "the action was a server-side preview: the cluster is unchanged, so there is nothing to converge to"
		return rep.done()
	}
	rep.Rollback.Available = pl.rollback == RollbackPerformable && pl.undo != nil && pre.Captured

	// Record before observing, not after. The observation window is up to a minute
	// and a half; the recording is one call. Whatever happens in between — a crash, a
	// cancelled context, a process eviction — the artifact already carries the label
	// that stops a second execution. The convergence verdict reaches a human through
	// the returned report and the comms layer; the label cannot wait for it.
	var recordErr error
	if err := r.recorder.RecordExecution(ctx, auth, executionDetail(rep, r.policy.ObserveWindow)); err != nil {
		recordErr = fmt.Errorf("%w: %w", ErrRecord, err)
	} else {
		rep.Recorded = true
	}

	obs := r.observe(ctx, func(idx *clusterIndex) (bool, string) {
		return pl.converged(idx, pre, p.Target)
	})
	rep.Convergence, rep.ConvergenceDetail, rep.ObservedFor = obs.verdict, obs.detail, obs.elapsed

	if recordErr != nil {
		return rep.fail(FailureRecord, recordErr)
	}
	return rep.done()
}

// checkAuthorization verifies that a real permission slip exists, that it covers
// this exact action, and that everything involved names the same cluster.
//
// The cluster check is three-way — the slip, the proposal, and the write client —
// rather than two-way, because each pair alone leaves a hole: a slip matching a
// proposal says nothing about which cluster the runner can reach, and a proposal
// matching the runner says nothing about which cluster a human approved.
func (r *Runner) checkAuthorization(auth *approve.Authorization, p remediate.Proposal) (FailureClass, error) {
	if !auth.Valid() {
		return FailureNotAuthorized, fmt.Errorf("%w: the approval gate issued no permission slip", ErrNotAuthorized)
	}
	if !auth.Matches(p) {
		return FailureNotAuthorized, fmt.Errorf(
			"%w: the permission slip covers %s on %s, not %s on %s",
			ErrNotAuthorized, auth.Operation(), auth.Target().String(), p.Operation, p.Target.String())
	}
	if auth.Cluster() != p.Cluster || p.Cluster != r.mutator.Name() {
		return FailureClusterMismatch, fmt.Errorf(
			"%w: approved for %q, proposed for %q, write client reaches %q",
			ErrClusterMismatch, auth.Cluster(), p.Cluster, r.mutator.Name())
	}
	return FailureNone, nil
}

// checkActionable applies the refusals that are about the ACTION rather than about
// permission: an unclassified rollback, an irreversible effect, and an operation the
// write path cannot express faithfully.
//
// Irreversible actions are refused outright rather than routed to a second
// confirmation, and the reason is structural rather than a policy preference. This
// package has no channel to a human — it holds a write client, a cluster reader, and
// a recorder, none of which can ask a question. Asking is [approve]'s entire job and
// it already has the surface for it, so the honest thing for this layer to do with
// an action it will not take is to refuse it loudly and let the gate decide whether
// to ask for something more. A refusal here is visible in the report, on the trail,
// and in the returned error; a second confirmation invented here would be a second
// approval mechanism nobody audited.
//
// No operation in the current catalog is irreversible, which makes this a guard
// against the catalog GROWING one rather than a branch that fires today. That is
// exactly when such a guard has to be written.
func checkActionable(pl plan, p remediate.Proposal) (FailureClass, error) {
	if pl.rollback == RollbackUnclassified {
		return FailureRefused, fmt.Errorf(
			"%w: operation %q has no rollback classification, so what undoing it would take was never established",
			ErrUnsupportedOperation, p.Operation)
	}
	switch p.Reversibility {
	case remediate.ReversibilityReversible, remediate.ReversibilityRecreatedByController:
		// Both are within what this layer will perform unattended once approved.
	case remediate.ReversibilityIrreversible:
		return FailureRefused, fmt.Errorf(
			"%w: %s on %s is classified %s; MaKlaude does not perform irreversible actions, and an approval cannot make one reversible",
			ErrIrreversible, p.Operation, p.Target.String(), p.Reversibility)
	default:
		// An unrecognized class is treated as the worst one. A reversibility this layer
		// cannot name is not a reversibility it may assume is safe.
		return FailureRefused, fmt.Errorf(
			"%w: %s on %s carries unclassified reversibility %s, which is treated as irreversible",
			ErrIrreversible, p.Operation, p.Target.String(), p.Reversibility)
	}
	if pl.unsupported != "" {
		return FailureRefused, fmt.Errorf("%w: %s: %s", ErrUnsupportedOperation, p.Operation, pl.unsupported)
	}
	if pl.mutate == nil {
		return FailureRefused, fmt.Errorf("%w: operation %q has no send path", ErrUnsupportedOperation, p.Operation)
	}
	return FailureNone, nil
}

// readCluster takes the single live view an execution is judged against, refusing
// anything it cannot trust: a failed read, an unreachable cluster, or a snapshot of
// a DIFFERENT cluster than the one being acted on.
//
// The last of those looks paranoid and is not. The observer and the write client are
// separate dependencies wired by a caller, so nothing but this check stops a runner
// from being handed one cluster's reader and another's writer — a mistake that would
// evaluate preconditions against a cluster it is not about to change.
func (r *Runner) readCluster(ctx context.Context, cluster string) (*clusterIndex, FailureClass, error) {
	snap, err := r.observer.Collect(ctx)
	if err != nil {
		return nil, FailureUnobservable, fmt.Errorf("%w: %w", ErrUnobservable, err)
	}
	if !snap.Reachability.Reachable {
		return nil, FailureUnobservable, fmt.Errorf("%w: cluster %q is unreachable: %s", ErrUnobservable, cluster, snap.Reachability.Error)
	}
	if snap.Cluster != cluster {
		return nil, FailureClusterMismatch, fmt.Errorf(
			"%w: the observer reports cluster %q while the action targets %q", ErrClusterMismatch, snap.Cluster, cluster)
	}
	return newClusterIndex(snap), FailureNone, nil
}

// modePermitsAction reports whether the kill switch allows anything to be sent.
// Only the two explicitly opted-in modes qualify; an unrecognized value is refused
// rather than guessed at.
func modePermitsAction(mode kube.ExecuteMode) bool {
	return mode == kube.ExecuteDryRun || mode == kube.ExecuteEnabled
}

// renderDrift turns the failed preconditions into one sentence naming what was
// actually observed, so the returned error is readable on its own — an escalation
// that says only "a precondition failed" makes a human open the report to learn
// anything at all.
func renderDrift(drifted []PreconditionResult) string {
	parts := make([]string, 0, len(drifted))
	for _, pc := range drifted {
		parts = append(parts, fmt.Sprintf("%s: %s", pc.Kind, pc.Observed))
	}
	return strings.Join(parts, "; ")
}

// executionDetail renders what goes onto the approval trail alongside the executed
// label: what was sent, what the target looked like before, whether it can be undone,
// and the fact that convergence is still being watched.
//
// It says the observation is pending rather than reporting it, because this is
// written BEFORE the window (see [Runner.Execute] for why the label cannot wait).
// Saying so explicitly is what keeps a reader from taking the absence of a
// convergence line as evidence that nobody checked.
func executionDetail(rep Report, window time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sent via `%s`, conditioned on resourceVersion `%s`, after %d attempt(s).",
		rep.Outcome.Scope, rep.Outcome.ResourceVersion, rep.Attempts)

	if rep.PreState.Captured {
		fields := make([]string, 0, len(rep.PreState.Fields))
		for _, f := range rep.PreState.Fields {
			fields = append(fields, fmt.Sprintf("%s=%s", f.Name, f.Value))
		}
		fmt.Fprintf(&b, "\n\nState before the action (%s at resourceVersion `%s`): %s.",
			rep.PreState.Kind, rep.PreState.ResourceVersion, strings.Join(fields, ", "))
	}

	fmt.Fprintf(&b, "\n\n**Rollback:** %s %s", rep.Rollback.Kind, rep.Rollback.Note)
	if rep.Rollback.Available {
		b.WriteString(" MaKlaude captured the pre-state and can perform this rollback on request.")
	}

	fmt.Fprintf(&b, "\n\nConvergence is being watched for up to %s and is reported separately; this note is written first so the record of the execution cannot be lost while waiting.", window)
	return b.String()
}
