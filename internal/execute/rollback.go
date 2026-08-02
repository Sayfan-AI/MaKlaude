package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// Rollback undoes an action this runner performed, restoring the target to the
// pre-state recorded in its [Report].
//
// # Why it takes the same authorization
//
// A rollback is a mutating action, so it needs authority, and inventing a second
// approval mechanism for it would be a second thing to audit. The rule instead is
// narrow and easy to state: THE AUTHORITY TO TAKE AN ACTION INCLUDES THE AUTHORITY
// TO UNDO IT, AND NOTHING ELSE. So this takes the very permission slip that
// authorized the action being undone, checks it against the report the same way
// [Runner.Execute] checked it against the proposal, and refuses anything else. A
// caller cannot roll back an action nobody approved, cannot use one action's slip
// to undo another, and cannot reach a different cluster with either.
//
// # Why it is never automatic
//
// Nothing in [Runner.Execute] calls this. A run that watched its observation window
// expire without converging has learned something a human needs to hear; what it
// has NOT learned is that undoing the action is the right response. The remediation
// may simply be slow. The problem may be elsewhere. Rolling back automatically
// would be MaKlaude taking an unapproved mutating action on the strength of its own
// impatience, which is the precise autonomy this milestone exists to withhold.
//
// # What it does when there is nothing to do
//
// A target already back at its pre-action state — because a human uncordoned the
// node first, or because the action never took — is a SUCCESS with nothing sent,
// reported as [RollbackReport.AlreadyAtPreState]. A rollback that re-asserts a state
// someone else has already restored is at best a redundant write in an audit log,
// and at worst a machine and a person taking turns undoing each other.
//
// # It is audited exactly as an execution is
//
// A rollback is a mutating action, so the same rule applies to it: every path out
// appends an audit record and renders the lifecycle onto the same approval artifact
// the original action was recorded on. That is what lets the artifact read as one
// story — approved, executed, verified, undone — rather than as two disconnected
// events a reader has to correlate by hand.
func (r *Runner) Rollback(ctx context.Context, auth *approve.Authorization, rep Report) (RollbackReport, error) {
	rb, err := r.rollback(ctx, auth, rep)
	r.recordRollback(ctx, auth, rep, rb)
	return rb, err
}

// rollback is the attempt itself, separated from [Runner.Rollback] for the same
// reason [Runner.execute] is separated from [Runner.Execute]: one emission point
// that no return path can bypass.
func (r *Runner) rollback(ctx context.Context, auth *approve.Authorization, rep Report) (RollbackReport, error) {
	mode := r.mutator.Mode()
	rb := RollbackReport{
		Identity:  rep.Identity,
		Cluster:   rep.Cluster,
		Operation: rep.Operation,
		Target:    rep.Target,
		StartedAt: time.Now().UTC(),
	}

	if class, err := r.authorizedForRollback(auth, rep); err != nil {
		return rb.fail(class, err)
	}
	if !modePermitsAction(mode) {
		return rb.fail(FailureKillSwitch, fmt.Errorf("%w: the write path is in mode %q", ErrKillSwitch, mode))
	}

	undo, err := rollbackPlanFor(rep)
	if err != nil {
		return rb.fail(FailureNotRollbackable, err)
	}
	rb.Description = undo.description

	// One read serves the already-satisfied check, the current resourceVersion the
	// inverse must be conditioned on, and the convergence baseline.
	idx, class, err := r.readCluster(ctx, rep.Cluster)
	if err != nil {
		return rb.fail(class, err)
	}

	if satisfied, detail := undo.satisfied(idx, rep.PreState, rep.Target); satisfied {
		rb.AlreadyAtPreState = true
		rb.Convergence, rb.ConvergenceDetail = ConvergenceConverged, detail
		return rb.done()
	}

	// The inverse is conditioned on the target's CURRENT version, which is necessarily
	// not the one the original action used — that action is what changed it. Reading
	// it here rather than deriving it is also what makes the inverse refuse if anyone
	// else has touched the object in between.
	resourceVersion, ok := idx.resourceVersion(rep.Target)
	if !ok {
		return rb.fail(FailureNotRollbackable, fmt.Errorf(
			"%w: %s is no longer present in cluster %q, so there is nothing to restore",
			ErrNotRollbackable, rep.Target.String(), rep.Cluster))
	}

	out, attempts, err := r.send(ctx, func(ctx context.Context) (*kube.Outcome, error) {
		return undo.mutate(ctx, r.mutator, rep.Target, resourceVersion)
	})
	rb.Attempts = attempts
	switch {
	case errors.Is(err, kube.ErrPreconditionConflict):
		// Someone else changed the object between the read and the inverse. Aborting is
		// right: whatever they were doing, this rollback was computed against a state
		// that no longer exists.
		return rb.fail(FailureConflict, err)
	case err != nil:
		return rb.fail(FailureExecute, err)
	case out == nil:
		return rb.fail(FailureExecute, errors.New("execute: the write path reported neither an outcome nor an error"))
	}
	rb.Outcome = out
	rb.Performed = !(mode == kube.ExecuteDryRun || out.DryRun)

	if !rb.Performed {
		rb.ConvergenceDetail = "the rollback was a server-side preview: the cluster is unchanged"
		return rb.done()
	}

	var recordErr error
	if err := r.recorder.RecordExecution(ctx, auth, rollbackDetail(rb, resourceVersion)); err != nil {
		recordErr = fmt.Errorf("%w: %w", ErrRecord, err)
	}

	obs := r.observe(ctx, func(idx *clusterIndex) (bool, string) {
		return undo.satisfied(idx, rep.PreState, rep.Target)
	})
	rb.Convergence, rb.ConvergenceDetail, rb.ObservedFor = obs.verdict, obs.detail, obs.elapsed

	if recordErr != nil {
		return rb.fail(FailureRecord, recordErr)
	}
	return rb.done()
}

// authorizedForRollback applies the same three-way permission and cluster checks
// [Runner.Execute] applies, against the report instead of the proposal.
//
// It compares the slip's target verbatim, resourceVersion included. That version is
// the one the ACTION was conditioned on, which both the report and the slip still
// carry, so the comparison stays exact even though the object has moved since.
func (r *Runner) authorizedForRollback(auth *approve.Authorization, rep Report) (FailureClass, error) {
	if !auth.Valid() {
		return FailureNotAuthorized, fmt.Errorf("%w: the approval gate issued no permission slip", ErrNotAuthorized)
	}
	if auth.Identity() != rep.Identity || auth.Operation() != rep.Operation || auth.Target() != rep.Target {
		return FailureNotAuthorized, fmt.Errorf(
			"%w: the permission slip covers %s on %s, not the %s on %s being rolled back",
			ErrNotAuthorized, auth.Operation(), auth.Target().String(), rep.Operation, rep.Target.String())
	}
	if auth.Cluster() != rep.Cluster || rep.Cluster != r.mutator.Name() {
		return FailureClusterMismatch, fmt.Errorf(
			"%w: approved for %q, executed against %q, write client reaches %q",
			ErrClusterMismatch, auth.Cluster(), rep.Cluster, r.mutator.Name())
	}
	return FailureNone, nil
}

// rollbackPlanFor resolves the inverse action for a report, refusing every case
// where there is nothing to undo. The refusals are enumerated rather than collapsed
// into one because they are genuinely different situations and a human reading the
// error needs to know which one they are in: a preview changed nothing, a delete
// cannot be reversed by anyone, and a restart has no inverse worth performing.
func rollbackPlanFor(rep Report) (*undoPlan, error) {
	pl, ok := planFor(rep.Operation)
	if !ok {
		return nil, fmt.Errorf("%w: operation %q has no plan in the execution layer", ErrNotRollbackable, rep.Operation)
	}
	if !rep.Executed {
		return nil, fmt.Errorf(
			"%w: %s on %s never ran (executed=%t, dry-run=%t), so there is nothing to undo",
			ErrNotRollbackable, rep.Operation, rep.Target.String(), rep.Executed, rep.DryRun)
	}
	if pl.rollback != RollbackPerformable || pl.undo == nil {
		return nil, fmt.Errorf("%w: %s is classified %s — %s",
			ErrNotRollbackable, rep.Operation, pl.rollback, pl.rollbackNote)
	}
	if !rep.PreState.Captured {
		return nil, fmt.Errorf(
			"%w: no pre-state was captured for %s, so there is no recorded state to restore",
			ErrNotRollbackable, rep.Target.String())
	}
	return pl.undo, nil
}

// rollbackDetail renders the note recording a rollback on the approval trail. It is
// written to the same artifact as the execution it undoes, so the trail reads as one
// story: what was approved, what ran, and what was put back.
func rollbackDetail(rb RollbackReport, resourceVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Rolled back.** MaKlaude performed the inverse action (%s) on `%s`, conditioned on resourceVersion `%s`, after %d attempt(s).",
		rb.Description, rb.Target.String(), resourceVersion, rb.Attempts)
	if rb.Outcome != nil {
		fmt.Fprintf(&b, "\n\nSent via `%s`.", rb.Outcome.Scope)
	}
	b.WriteString("\n\nThe original action remains recorded above; this note records that its effect was undone, not that it never happened.")
	return b.String()
}
