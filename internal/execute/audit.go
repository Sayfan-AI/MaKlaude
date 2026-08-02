package execute

import (
	"context"
	"fmt"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file is the seam between "what this package did" and "what an operator can
// reconstruct six months later". Everything here is derivation: it reads a finished
// [Report] or [RollbackReport] and turns it into ordered [audit.Record]s. It sends
// nothing, decides nothing, and cannot change what happened.
//
// # Why the records are derived at the end rather than emitted as things happen
//
// The obvious alternative is to append a record at each step of [Runner.Execute],
// which would put every record's write closer to the event it describes. It was not
// done, for two reasons.
//
// The first is that it would not buy what it looks like it buys. The durable trail
// is the approval artifact, and the execution is already written there BEFORE the
// observation window precisely so a crash cannot lose it (see the recording note in
// [Runner.Execute]). The in-process [audit.Sink] is not the thing that survives a
// crash, so scattering appends through the sequence protects nothing that is not
// already protected.
//
// The second is that it would put an append — and its error handling — on every one
// of the fifteen or so abort paths in this package, each of which currently ends in
// a single line that cannot forget to stamp the failure class. Fifteen chances to
// omit a record, in the code whose job is to not omit records, is a poor trade for
// timestamps that [audit.Record.RecordedAt] already documents as recording time
// rather than event time. Every instant that matters — when the human approved,
// when the gate honored it, when the attempt started and finished — is carried in
// the record's own fields and is unaffected by when the record was written.
//
// So there is exactly one emission point per public method, it runs on every path
// including the aborts, and the ordering of the records it produces is fixed by the
// lifecycle rather than by control flow.

// recordExecution derives and appends the audit records for one finished execution,
// then renders the lifecycle onto the comms trail.
//
// It runs on EVERY path out of [Runner.Execute], including the ones that sent
// nothing. An abort is a thing that happened: "MaKlaude was authorized to cordon
// this node and did not, because the node had recovered" is an audit record an
// operator wants, and before this it existed nowhere — the artifact showed an
// approval and then silence.
func (r *Runner) recordExecution(ctx context.Context, auth *approve.Authorization, rep Report) {
	r.emit(ctx, auth, executionRecords(auth, rep))
}

// recordRollback is the [Runner.Rollback] counterpart. It takes the original
// execution's report as well as the rollback's, because a rollback's audit record
// has to name the pre-state it was restoring TO, and that lives on the report of
// the action being undone.
func (r *Runner) recordRollback(ctx context.Context, auth *approve.Authorization, rep Report, rb RollbackReport) {
	r.emit(ctx, auth, rollbackRecords(auth, rep, rb))
}

// emit appends each record and posts the resulting lifecycle to the approval trail.
//
// Both halves are best-effort and neither can change the outcome that was already
// determined. That is a deliberate asymmetry with the executed label, which DOES
// fail an execution when it cannot be written ([FailureRecord]), and the reason is
// what each one protects: the label prevents a second execution, so losing it has a
// consequence for the cluster; the audit record is a description of something that
// has already finished, so failing the action because its description could not be
// filed would turn a successful remediation into a reported failure. The record is
// important; it is not more important than the truth about what happened.
//
// The lifecycle is rendered from the records the SINK returned, never from the ones
// that went in. See [audit.Sink] — the stored copies are the redacted ones, and the
// comms artifact is world-readable.
func (r *Runner) emit(ctx context.Context, auth *approve.Authorization, recs []audit.Record) {
	stored := make([]audit.Record, 0, len(recs))
	for _, rec := range recs {
		s, err := r.trail.Append(ctx, rec)
		if err != nil {
			continue
		}
		stored = append(stored, s)
	}
	if len(stored) == 0 || !auth.Valid() {
		// With no valid authorization there is no artifact to post to. The records are
		// still in the trail; there is simply no conversation they belong to.
		return
	}
	_ = r.recorder.RecordOutcome(ctx, auth, audit.Lifecycle(stored))
}

// executionRecords derives the ordered lifecycle records for one execution.
//
// The order is the lifecycle's own and does not depend on how the attempt ended:
// approved (if there was a valid slip), executed (if a request was sent), verified
// (if the window was watched), failed (if something terminated it). An attempt that
// was refused before it reached the write path produces exactly one record; a
// successful execution produces three.
func executionRecords(auth *approve.Authorization, rep Report) []audit.Record {
	base := audit.Record{
		Action:   actionOf(rep),
		Approver: approverOf(auth),
		Change:   changeOf(rep),
		PreState: preStateOf(rep.PreState),
		Rollback: rollbackOf(rep),
	}

	var recs []audit.Record

	if base.Approver.Attributed() {
		approved := base
		approved.Phase = audit.PhaseApproved
		// The approval record deliberately carries no outcome: at the instant it
		// describes, nothing had run.
		approved.Change = audit.Change{}
		approved.PreState = audit.PreState{}
		recs = append(recs, approved)
	}

	if rep.Outcome != nil {
		executed := base
		executed.Phase = audit.PhaseExecuted
		if rep.DryRun {
			executed.Detail = "this was a server-side preview: the request was accepted by the API server and the cluster is unchanged"
		}
		recs = append(recs, executed)
	}

	// A verification record is written exactly when the window was actually watched.
	// A preview returns before observing and an abort never gets there, and in both
	// cases a "verified" record would assert that someone checked.
	if rep.Convergence != ConvergenceUnobserved {
		verified := base
		verified.Phase = audit.PhaseVerified
		verified.Outcome = outcomeOf(rep)
		recs = append(recs, verified)
	}

	if rep.Failure != FailureNone {
		failed := base
		failed.Phase = audit.PhaseFailed
		failed.Outcome = outcomeOf(rep)
		recs = append(recs, failed)
	}

	// Every path must leave a trace. A dry run that neither failed nor converged would
	// otherwise produce only the approval record, and a trail that records permission
	// but not what was done with it is the failure mode this whole package exists to
	// close.
	if len(recs) == 0 {
		only := base
		only.Phase = audit.PhaseExecuted
		only.Detail = "no mutating request was sent and the attempt reported no failure"
		recs = append(recs, only)
	}
	return recs
}

// rollbackRecords derives the records for one rollback attempt.
//
// Every record it produces is marked [audit.Rollback.Attempted], which is what
// distinguishes a failed rollback from a failed execution — both are
// [audit.PhaseFailed], and an operator reading "failed" needs to know which of the
// two things failed before anything else about it matters.
func rollbackRecords(auth *approve.Authorization, rep Report, rb RollbackReport) []audit.Record {
	base := audit.Record{
		Action:   actionOf(rep),
		Approver: approverOf(auth),
		Change:   rollbackChangeOf(rb),
		PreState: preStateOf(rep.PreState),
		Rollback: audit.Rollback{
			Kind:              rep.Rollback.Kind.String(),
			Note:              rep.Rollback.Note,
			Available:         rep.Rollback.Available,
			Attempted:         true,
			Performed:         rb.Performed,
			AlreadyAtPreState: rb.AlreadyAtPreState,
			Description:       rb.Description,
		},
	}

	if rb.Failure != FailureNone {
		failed := base
		failed.Phase = audit.PhaseFailed
		failed.Outcome = rollbackOutcomeOf(rb)
		return []audit.Record{failed}
	}

	done := base
	done.Phase = audit.PhaseRolledBack
	done.Outcome = rollbackOutcomeOf(rb)
	if !rb.Performed && !rb.AlreadyAtPreState {
		done.Detail = "the rollback was a server-side preview: the cluster is unchanged"
	}
	return []audit.Record{done}
}

// actionOf restates the proposal from the report. The report is used rather than the
// proposal itself because a rollback has no proposal in hand — only the report of
// the action it is undoing — and one derivation means the two cannot describe the
// same action differently.
func actionOf(rep Report) audit.Action {
	return audit.Action{
		Identity:      rep.Identity,
		Cluster:       rep.Cluster,
		Operation:     rep.Operation,
		Target:        rep.Target,
		Reversibility: rep.Reversibility,
		Title:         titleFor(rep.Operation),
		ProposedAt:    rep.ProposedAt,
	}
}

// approverOf reads the permission slip.
//
// Authority is [audit.AuthorityHuman] because that is the only authority the gate
// can currently issue: an approval with no identifiable approver is refused before a
// slip exists (approve's ReasonUnattributedApproval), and one applied by MaKlaude's
// own account is refused as self-approval. When the planned bypass (issue #124)
// adds a second kind, this is the single line that learns to ask the authorization
// which one it is — the record's shape, the renderer, and every record already
// written are unaffected. That is what [audit.Authority] is for.
func approverOf(auth *approve.Authorization) audit.Approver {
	if !auth.Valid() {
		return audit.Approver{}
	}
	return audit.Approver{
		Authority:    audit.AuthorityHuman,
		Identity:     auth.Approver(),
		ApprovedAt:   auth.ApprovedAt(),
		AuthorizedAt: auth.AuthorizedAt(),
		Ref:          string(auth.Ref()),
	}
}

// changeOf reads what was actually sent off the report.
func changeOf(rep Report) audit.Change {
	c := audit.Change{
		Sent:            rep.Outcome != nil,
		Applied:         rep.Executed,
		DryRun:          rep.DryRun,
		Mode:            rep.Mode.String(),
		Attempts:        rep.Attempts,
		RecordedOnTrail: rep.Recorded,
		StartedAt:       rep.StartedAt,
		FinishedAt:      rep.FinishedAt,
	}
	if rep.Outcome != nil {
		c.Scope = rep.Outcome.Scope
		c.ResourceVersion = rep.Outcome.ResourceVersion
	}
	return c
}

// rollbackChangeOf is the [RollbackReport] counterpart of [changeOf].
func rollbackChangeOf(rb RollbackReport) audit.Change {
	c := audit.Change{
		Sent:       rb.Outcome != nil,
		Applied:    rb.Performed,
		Attempts:   rb.Attempts,
		StartedAt:  rb.StartedAt,
		FinishedAt: rb.FinishedAt,
	}
	if rb.Outcome != nil {
		c.Scope = rb.Outcome.Scope
		c.ResourceVersion = rb.Outcome.ResourceVersion
		c.DryRun = rb.Outcome.DryRun
	}
	return c
}

// preStateOf copies the captured pre-state into the record's own shape. The audit
// package deliberately does not import this one, so the two types are separate; the
// copy is what keeps the audit record a plain serializable value rather than a
// window onto another package's internals.
func preStateOf(pre PreState) audit.PreState {
	out := audit.PreState{
		Captured:        pre.Captured,
		Kind:            pre.Kind,
		ResourceVersion: pre.ResourceVersion,
		ObservedAt:      pre.ObservedAt,
	}
	for _, f := range pre.Fields {
		out.Fields = append(out.Fields, audit.PreStateField{Name: f.Name, Value: f.Value})
	}
	return out
}

// outcomeOf renders the verdict and the terminating failure as the stable tokens the
// audit trail records. The tokens come from the enums' own String methods, which are
// documented as the audit-trail form, so there is one spelling of each.
func outcomeOf(rep Report) audit.Outcome {
	return audit.Outcome{
		Convergence: rep.Convergence.String(),
		Detail:      rep.ConvergenceDetail,
		ObservedFor: rep.ObservedFor,
		Failure:     rep.Failure.String(),
		CleanAbort:  rep.CleanAbort(),
		Error:       rep.Error,
	}
}

// rollbackOutcomeOf is the [RollbackReport] counterpart of [outcomeOf].
func rollbackOutcomeOf(rb RollbackReport) audit.Outcome {
	return audit.Outcome{
		Convergence: rb.Convergence.String(),
		Detail:      rb.ConvergenceDetail,
		ObservedFor: rb.ObservedFor,
		Failure:     rb.Failure.String(),
		CleanAbort:  rb.Failure.CleanAbort(),
		Error:       rb.Error,
	}
}

// rollbackOf copies the reversal plan for an execution record. Attempted is false
// here by construction: this describes what undoing the action WOULD take, not an
// attempt to do it.
func rollbackOf(rep Report) audit.Rollback {
	return audit.Rollback{
		Kind:      rep.Rollback.Kind.String(),
		Note:      rep.Rollback.Note,
		Available: rep.Rollback.Available,
	}
}

// titleFor renders the short human label for an operation.
//
// It is derived here rather than carried on the [Report] because a rollback has no
// proposal to read one off, and a record whose title is empty for rollbacks and
// populated for executions would be a difference a reader has to explain. The
// wording matches the approval artifact's, so the audit trail and the thing the
// human approved call the action the same name.
func titleFor(op remediate.Operation) string {
	switch op {
	case remediate.OpRolloutRestart:
		return "Restart deployment rollout"
	case remediate.OpRollbackRevision:
		return "Roll deployment back one revision"
	case remediate.OpDeletePod:
		return "Delete failed pod so its controller recreates it"
	case remediate.OpCordonNode:
		return "Cordon NotReady node"
	default:
		return fmt.Sprintf("Operation %s", op)
	}
}
