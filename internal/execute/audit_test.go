package execute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// auditSecret is a token shape the shared redactor recognizes. It is planted where a
// real secret would plausibly arrive — inside an error string the API server handed
// back — rather than in a field a test controls directly, because the realistic leak
// is not someone typing a password into a report, it is a credential riding out of
// the cluster inside somebody else's error message.
const auditSecret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// TestExecute_AppendsTheWholeLifecycleTiedToItsApprover is the headline done
// criterion: every execution appends a complete, ordered record linking the action,
// the approver, and the outcome.
//
// All three legs are asserted on the SAME record set rather than in separate tests,
// because the criterion is about the linkage. A trail that recorded the action in
// one entry, the approver in another, and the outcome in a third, with nothing
// joining them, would pass three narrower tests and be useless to the person it
// exists for.
func TestExecute_AppendsTheWholeLifecycleTiedToItsApprover(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()

	rep, err := h.execute(p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}

	if got := h.phases(); !equalStrings(got, []string{"approved", "executed", "verified"}) {
		t.Fatalf("the trail holds phases %v, want approved → executed → verified", got)
	}

	for i, rec := range h.records() {
		if rec.Seq != i+1 {
			t.Errorf("record %d has Seq %d, want %d", i, rec.Seq, i+1)
		}
		if rec.Action.Identity != p.Identity {
			t.Errorf("record %d is not linked to the proposal: identity %q, want %q", i, rec.Action.Identity, p.Identity)
		}
		if rec.Action.Cluster != testCluster || rec.Action.Target != p.Target {
			t.Errorf("record %d names %s on cluster %q, want %s on %q", i, rec.Action.Target, rec.Action.Cluster, p.Target, testCluster)
		}
		if rec.Approver.Identity != "the-gigi" || !rec.Approver.Authority.HumanReviewed() {
			t.Errorf("record %d is attributed to %s, want the human who approved it", i, rec.Approver)
		}
		if !rec.Approver.ApprovedAt.Equal(rep.StartedAt) && rec.Approver.ApprovedAt.IsZero() {
			t.Errorf("record %d carries no approval time", i)
		}
		if rec.Approver.Ref == "" {
			t.Errorf("record %d does not point back at the approval artifact", i)
		}
		if rec.Action.ProposedAt.IsZero() {
			t.Errorf("record %d carries no proposal time, so the trail cannot show the proposed stage", i)
		}
	}

	executed := h.recordFor(audit.PhaseExecuted)
	if !executed.Change.Sent || !executed.Change.Applied || executed.Change.Attempts != 1 {
		t.Fatalf("the executed record says sent=%t applied=%t attempts=%d, want one applied request",
			executed.Change.Sent, executed.Change.Applied, executed.Change.Attempts)
	}
	if !executed.Change.RecordedOnTrail {
		t.Error("the executed record does not say the execution reached the approval trail")
	}
	if !executed.PreState.Captured || len(executed.PreState.Fields) == 0 {
		t.Fatalf("the executed record captured no pre-state, so nothing says what the object looked like: %+v", executed.PreState)
	}
	if executed.Rollback.Kind != RollbackPerformable.String() || !executed.Rollback.Available {
		t.Errorf("the executed record does not record an available rollback: %+v", executed.Rollback)
	}

	verified := h.recordFor(audit.PhaseVerified)
	if verified.Outcome.Convergence != ConvergenceConverged.String() {
		t.Fatalf("the verified record says %q, want converged (%s)", verified.Outcome.Convergence, verified.Outcome.Detail)
	}
	if verified.Outcome.Failed() {
		t.Errorf("a converged execution recorded a failure: %+v", verified.Outcome)
	}
}

// TestExecute_TheLifecycleReachesTheCommsArtifact drives the REAL gatekeeper so the
// assertion is about the artifact a human opens, not about a fake.
//
// The two comments are both required and are deliberately different. The first is
// posted the moment the mutation lands and carries the executed label — it cannot
// wait for the observation window, because the label is what stops a second
// execution. The second is the audit lifecycle, which only exists once the window
// closes. Before this change the second one did not exist at all: the artifact
// showed that an action ran and never said whether it worked.
func TestExecute_TheLifecycleReachesTheCommsArtifact(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	p := cordonProposal()
	g := newGate(t, p)
	auth := g.authorize()

	mutator := newFakeMutator(model)
	runner, err := New(mutator, &fakeObserver{model: model}, g.gk, audit.NewTrail(), fastPolicy())
	if err != nil {
		t.Fatalf("building runner: %v", err)
	}
	if _, err := runner.Execute(context.Background(), auth, p); err != nil {
		t.Fatalf("executing: %v", err)
	}

	comments := g.artifact().Comments
	if !containsSubstring(comments, "Executed.") {
		t.Fatalf("the artifact does not record the execution: %v", comments)
	}
	for _, want := range []string{
		"## Audit trail",
		"proposed → approved → executed → verified (converged)",
		"@the-gigi (human approval)",
		"**State before the action**",
		"**Rollback:** performable",
	} {
		if !containsSubstring(comments, want) {
			t.Fatalf("the artifact's audit trail is missing %q: %v", want, comments)
		}
	}
	if !g.artifact().HasLabel(approve.ExecutedLabel) {
		t.Fatalf("posting the audit lifecycle disturbed the executed label: %v", g.artifact().Labels)
	}
}

// TestExecute_ACleanAbortIsStillAudited covers the gap this task was really about.
//
// A drifted precondition sends nothing, and before this change it left nothing
// behind either: the artifact showed an approval followed by silence, which is
// indistinguishable from a run that never happened. "MaKlaude was allowed to cordon
// this node and chose not to, because the node had recovered" is an audit record,
// and the trail has to carry it.
func TestExecute_ACleanAbortIsStillAudited(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	// The node recovers between the proposal and the execution.
	model.mutateNode("node-a", func(n *health.NodeSignal) { n.Ready = true })

	if _, err := h.execute(cordonProposal()); !errors.Is(err, ErrPreconditionDrift) {
		t.Fatalf("executing a drifted proposal returned %v, want a precondition drift", err)
	}

	if got := h.phases(); !equalStrings(got, []string{"approved", "failed"}) {
		t.Fatalf("the trail holds phases %v, want approved → failed", got)
	}
	failed := h.recordFor(audit.PhaseFailed)
	if !failed.Outcome.CleanAbort {
		t.Errorf("the abort is not recorded as clean, so a reader would treat a healthy refusal as a malfunction: %+v", failed.Outcome)
	}
	if failed.Change.Sent {
		t.Error("the record claims a request was sent by an attempt that sent nothing")
	}
	if !strings.Contains(failed.Outcome.Error, "precondition") {
		t.Errorf("the record does not say what stopped the action: %q", failed.Outcome.Error)
	}
	if note := h.recorder.lastOutcome(); !strings.Contains(note, "abandoned cleanly, nothing was applied") {
		t.Fatalf("the comms artifact does not say the action was abandoned without acting: %q", note)
	}
}

// TestExecute_AnUnauthorizedAttemptIsAuditedAndAttributedToNobody is the record that
// matters most and is the easiest to omit, because there is no approval artifact to
// post it to.
//
// It goes in the trail regardless. An attempt to mutate a cluster without a valid
// permission slip is precisely the event an audit trail exists to surface, and
// "there was nowhere to file it" is not a reason for it to vanish. What must NOT
// happen is inventing an approver for it.
func TestExecute_AnUnauthorizedAttemptIsAuditedAndAttributedToNobody(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())

	// A zero Authorization cannot be made valid outside the approve package; this is
	// the forged slip an executor must refuse.
	_, err := h.runner.Execute(context.Background(), &approve.Authorization{}, cordonProposal())
	if !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("executing without a permission slip returned %v, want ErrNotAuthorized", err)
	}

	if got := h.phases(); !equalStrings(got, []string{"failed"}) {
		t.Fatalf("the trail holds phases %v, want a single failed record", got)
	}
	rec := h.recordFor(audit.PhaseFailed)
	if rec.Approver.Attributed() {
		t.Fatalf("the trail invented an approver for an unauthorized attempt: %s", rec.Approver)
	}
	if rec.Approver.Authority != audit.AuthorityUnattributed {
		t.Errorf("authority = %s, want unattributed", rec.Approver.Authority)
	}
	if h.mutator.callCount() != 0 {
		t.Fatalf("an unauthorized attempt sent %d requests", h.mutator.callCount())
	}
	if notes := h.recorder.outcomeNotes(); len(notes) != 0 {
		t.Fatalf("a note was posted for an attempt with no approval artifact to post to: %v", notes)
	}
}

// TestExecute_APreviewIsAuditedAsAPreview guards the one thing a record must never
// overstate. A dry run is a sent request that changed nothing, and a record calling
// it an execution would permanently block the real one — the same asymmetry the
// [Report] flags are derived under.
func TestExecute_APreviewIsAuditedAsAPreview(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.mutator.mode = kube.ExecuteDryRun

	if _, err := h.execute(cordonProposal()); err != nil {
		t.Fatalf("previewing: %v", err)
	}

	if got := h.phases(); !equalStrings(got, []string{"approved", "executed"}) {
		t.Fatalf("the trail holds phases %v, want approved → executed with no verification", got)
	}
	rec := h.recordFor(audit.PhaseExecuted)
	if !rec.Change.Sent {
		t.Error("the record says nothing was sent; a preview is a sent request")
	}
	if rec.Change.Applied {
		t.Fatal("the record claims a preview changed the cluster")
	}
	if !rec.Change.DryRun || rec.Change.Mode != kube.ExecuteDryRun.String() {
		t.Errorf("the record does not identify the preview: dryRun=%t mode=%q", rec.Change.DryRun, rec.Change.Mode)
	}
	if note := h.recorder.lastOutcome(); !strings.Contains(note, "previewed") {
		t.Errorf("the comms artifact does not call the attempt a preview: %q", note)
	}
}

// TestRollback_IsAuditedOntoTheSameStory proves a rollback continues the action's
// trail rather than starting a new one, and that it is distinguishable from the
// execution it undoes.
func TestRollback_IsAuditedOntoTheSameStory(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}
	if _, err := h.runner.Rollback(context.Background(), auth, rep); err != nil {
		t.Fatalf("rolling back: %v", err)
	}

	if got := h.phases(); !equalStrings(got, []string{"approved", "executed", "verified", "rolled-back"}) {
		t.Fatalf("the trail holds phases %v, want the execution followed by the rollback", got)
	}

	rec := h.recordFor(audit.PhaseRolledBack)
	if rec.Action.Identity != p.Identity {
		t.Fatalf("the rollback record is filed under %q, not the action it undoes (%q)", rec.Action.Identity, p.Identity)
	}
	if !rec.Rollback.Attempted || !rec.Rollback.Performed {
		t.Fatalf("the rollback record says attempted=%t performed=%t", rec.Rollback.Attempted, rec.Rollback.Performed)
	}
	if rec.Rollback.Description == "" {
		t.Error("the rollback record does not say what inverse was performed")
	}
	if rec.Approver.Identity != "the-gigi" {
		t.Errorf("the rollback is attributed to %s, want the human whose approval it ran under", rec.Approver)
	}
	if note := h.recorder.lastOutcome(); !strings.Contains(note, "rolled back") {
		t.Errorf("the comms artifact does not close the story with the rollback: %q", note)
	}
}

// TestRollback_AFailedRollbackIsRecordedAsARollback guards the ambiguity in the
// phase enum from the execution side: a failed rollback and a failed execution are
// both "failed", and the cluster is in opposite states in the two cases.
func TestRollback_AFailedRollbackIsRecordedAsARollback(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}

	// The inverse request fails at the API server.
	h.mutator.err = errors.New("etcdserver: request timed out")
	if _, err := h.runner.Rollback(context.Background(), auth, rep); err == nil {
		t.Fatal("a failing rollback returned no error")
	}

	records := h.records()
	last := records[len(records)-1]
	if last.Phase != audit.PhaseFailed {
		t.Fatalf("the last record is %s, want failed", last.Phase)
	}
	if !last.Rollback.Attempted {
		t.Fatal("a failed rollback is recorded as a failed execution; nothing says which of the two failed")
	}
	if note := h.recorder.lastOutcome(); !strings.Contains(note, "the rollback failed") {
		t.Errorf("the comms artifact does not say the rollback is what failed: %q", note)
	}
}

// TestExecute_ClusterDerivedSecretsNeverReachTheTrailOrTheArtifact is the
// no-secrets criterion asserted end to end rather than against the redactor.
//
// The token arrives the way one actually would: inside the error string the write
// path handed back. Both destinations are checked, because they fail
// independently — the trail is in-process and the artifact is a GitHub issue that on
// a public repository anyone can read, and it is the second one that matters.
func TestExecute_ClusterDerivedSecretsNeverReachTheTrailOrTheArtifact(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.mutator.err = errors.New("admission webhook denied the request: could not authenticate with token " + auditSecret)

	if _, err := h.execute(cordonProposal()); err == nil {
		t.Fatal("the execution reported no error")
	}

	for _, rec := range h.records() {
		if strings.Contains(rec.Outcome.Error, auditSecret) || strings.Contains(rec.String(), auditSecret) {
			t.Fatalf("the audit trail stored a credential from an API server error: %+v", rec)
		}
	}
	note := h.recorder.lastOutcome()
	if strings.Contains(note, auditSecret) {
		t.Fatalf("the comms artifact leaked a credential:\n%s", note)
	}
	if !strings.Contains(note, "[REDACTED]") {
		t.Fatalf("the artifact removed the credential without saying material was removed:\n%s", note)
	}
	if !strings.Contains(note, "admission webhook denied") {
		t.Fatalf("redaction shredded the diagnostic context a human needs:\n%s", note)
	}
}

// TestExecute_AuditFailuresDoNotChangeWhatHappened pins the deliberate asymmetry
// with the executed label.
//
// The label fails an execution when it cannot be written, because losing it has a
// consequence for the cluster: the action could run twice. The audit record has no
// such consequence — it describes something already finished — so failing the
// action because its description could not be filed would turn a successful
// remediation into a reported failure. Both halves are exercised: a sink that
// refuses the record, and a trail that refuses the note.
func TestExecute_AuditFailuresDoNotChangeWhatHappened(t *testing.T) {
	t.Run("the audit sink refuses the record", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		mutator := newFakeMutator(model)
		recorder := &fakeRecorder{}
		runner, err := New(mutator, &fakeObserver{model: model}, recorder, refusingSink{}, fastPolicy())
		if err != nil {
			t.Fatalf("building runner: %v", err)
		}

		p := cordonProposal()
		rep, err := runner.Execute(context.Background(), authorizationFor(t, p), p)
		if err != nil {
			t.Fatalf("executing: %v", err)
		}
		if rep.Convergence != ConvergenceConverged || rep.Failure != FailureNone {
			t.Fatalf("an unrecordable audit changed the outcome: %s", rep)
		}
		if !model.node("node-a").Unschedulable {
			t.Fatal("the action did not reach the cluster")
		}
		if notes := recorder.outcomeNotes(); len(notes) != 0 {
			t.Fatalf("a lifecycle was rendered from records the sink never stored: %v", notes)
		}
	})

	t.Run("the comms trail refuses the note", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		h.recorder.outcomeErr = errors.New("502 Bad Gateway")

		rep, err := h.execute(cordonProposal())
		if err != nil {
			t.Fatalf("executing: %v", err)
		}
		if rep.Convergence != ConvergenceConverged || rep.Failure != FailureNone {
			t.Fatalf("an unpostable audit note changed the outcome: %s", rep)
		}
		if h.trail.Len() != 3 {
			t.Fatalf("the trail holds %d records; a failed comms post must not lose them", h.trail.Len())
		}
	})
}

// TestExecutionRecords_AlwaysProduceAtLeastOneRecord covers the fallback branch
// directly, because the paths that reach it are the ones a future change would
// introduce rather than any that exist today. The invariant it protects is worth
// stating on its own: a trail that recorded permission but not what was done with it
// is the failure mode this whole package exists to close.
func TestExecutionRecords_AlwaysProduceAtLeastOneRecord(t *testing.T) {
	recs := executionRecords(nil, Report{})
	if len(recs) != 1 {
		t.Fatalf("an empty report produced %d records, want exactly 1", len(recs))
	}
	if recs[0].Phase != audit.PhaseExecuted || recs[0].Detail == "" {
		t.Fatalf("the fallback record is %s with detail %q, want an executed record that says nothing was sent",
			recs[0].Phase, recs[0].Detail)
	}
}

// refusingSink is an [audit.Sink] that stores nothing, standing in for a durable
// sink whose backing store is unreachable.
type refusingSink struct{}

func (refusingSink) Append(context.Context, audit.Record) (audit.Record, error) {
	return audit.Record{}, errors.New("the audit store is unreachable")
}

// equalStrings compares two string slices element by element.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
