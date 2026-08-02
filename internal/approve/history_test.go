package approve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// These tests cover the two properties that make a GATED execution rebuildable: the
// approval artifact carries the machine-readable lifecycle marker, and the trail can be
// enumerated after the artifact has been closed.
//
// Both are about this trail specifically rather than about the marker format, which has
// its own tests in internal/audit. The reason they are needed here at all is the one the
// T3 carry-over gives: the ledger's promotion arithmetic counts HUMAN-APPROVED executions,
// so a format that marked up only unattended actions would make the evidence for autonomy
// the one thing a rebuild cannot reconstruct.

// executedLifecycle is the audit lifecycle of one human-approved execution that converged,
// attributed to the artifact the approval lives on.
func executedLifecycle(ref ActionRef, at time.Time) []audit.Record {
	action := audit.Action{
		Identity:  testRequest().Proposal.Identity,
		Cluster:   testRequest().Proposal.Cluster,
		Operation: testRequest().Proposal.Operation,
	}
	approver := audit.Approver{Authority: audit.AuthorityHuman, Ref: string(ref)}
	change := audit.Change{Sent: true, Applied: true, FinishedAt: at}

	recs := []audit.Record{
		{RecordedAt: at, Phase: audit.PhaseExecuted, Action: action, Approver: approver, Change: change},
		{
			RecordedAt: at, Phase: audit.PhaseVerified, Action: action, Approver: approver, Change: change,
			Outcome: audit.Outcome{Convergence: "converged", Failure: "none"},
		},
	}
	for i := range recs {
		recs[i].Seq = i + 1
	}
	return recs
}

// approveAndExecute drives the gate to a real permission slip: open the request, have a
// person apply the label, reconcile again. Nothing here manufactures an authorization —
// a slip the gate did not issue is exactly what [Gatekeeper.RecordLifecycle] refuses.
func approveAndExecute(t *testing.T, h *harness) (*Authorization, ArtifactView) {
	t.Helper()
	req := testRequest()

	h.pass(req)
	artifact := h.only()

	approvedAt := h.at.Add(time.Second)
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", approvedAt); err != nil {
		t.Fatalf("recording the human decision: %v", err)
	}
	h.at = approvedAt.Add(time.Minute)

	res := h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d after an explicit approval, want 1", len(res.Authorized))
	}
	return res.Authorized[0], h.only()
}

// TestRecordLifecycle_MakesAHumanApprovedExecutionRebuildable is the criterion this file
// exists for. The marker has to land on the approval artifact, carry the human authority,
// and coexist with every marker the gate already depends on.
func TestRecordLifecycle_MakesAHumanApprovedExecutionRebuildable(t *testing.T) {
	h := newHarness(t)
	auth, artifact := approveAndExecute(t, h)

	// Before the execution is recorded the body is an in-flight artifact: a rebuild must
	// read it as "nothing to contribute", not as corruption.
	if _, err := audit.ParseLifecycleMarker(artifact.Body); !errors.Is(err, audit.ErrNoMarker) {
		t.Fatalf("an artifact with no recorded execution returned %v, want audit.ErrNoMarker", err)
	}

	recs := executedLifecycle(artifact.Ref, h.at)
	if err := h.gk.RecordLifecycle(context.Background(), auth, recs); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}

	marked := h.only()
	got, err := audit.ParseLifecycleMarker(marked.Body)
	if err != nil {
		t.Fatalf("the approval artifact carries no readable lifecycle: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("read back %d records, wrote %d", len(got), len(recs))
	}
	if got[0].Approver.Authority != audit.AuthorityHuman {
		t.Errorf("the rebuilt lifecycle reports authority %v, want %v — this is the evidence autonomy is earned from",
			got[0].Approver.Authority, audit.AuthorityHuman)
	}
	if got[0].Action.Identity != testRequest().Proposal.Identity {
		t.Errorf("the rebuilt lifecycle names proposal %q, want %q", got[0].Action.Identity, testRequest().Proposal.Identity)
	}

	// The gate's own markers must survive: writing the lifecycle must not cost the trail
	// the ability to recognize its own artifact or to judge a stale approval.
	if id, ok := ParseProposalMarker(marked.Body); !ok || id != testRequest().Proposal.Identity {
		t.Error("the proposal marker did not survive the lifecycle write")
	}
	if rv, _, ok := ParsePreviewMarker(marked.Body); !ok || rv == "" {
		t.Error("the preview marker did not survive the lifecycle write")
	}
	if ParseGateMarker(marked.Body) == "" {
		t.Error("the gate marker did not survive the lifecycle write")
	}
	if !strings.Contains(marked.Body, "## Exactly what will run") {
		t.Error("the prose a person reads did not survive the lifecycle write")
	}
}

// TestRecordLifecycle_TouchesNothingButTheBody. The marker is attached through the sink's
// single-purpose operation rather than through Update precisely so recording an action's
// history cannot erase the human decision that authorized it.
func TestRecordLifecycle_TouchesNothingButTheBody(t *testing.T) {
	h := newHarness(t)
	auth, artifact := approveAndExecute(t, h)

	before := h.only()
	if err := h.gk.RecordLifecycle(context.Background(), auth, executedLifecycle(artifact.Ref, h.at)); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	after := h.only()

	if strings.Join(before.Labels, ",") != strings.Join(after.Labels, ",") {
		t.Errorf("the label set changed: before %v, after %v", before.Labels, after.Labels)
	}
	if !after.HasLabel(ApprovedLabel) {
		t.Error("the human's approval label was lost")
	}
	if before.Title != after.Title {
		t.Errorf("the title changed: %q -> %q", before.Title, after.Title)
	}
	if len(before.Comments) != len(after.Comments) {
		t.Errorf("the comment trail changed: %d -> %d comments", len(before.Comments), len(after.Comments))
	}
}

// TestRecordLifecycle_ReplacesAnEarlierMarker. A rollback lands after the execution and
// reaches the same artifact, so the second write must replace the first — two markers in
// one body would make the rebuilt history depend on which the parser reached first.
func TestRecordLifecycle_ReplacesAnEarlierMarker(t *testing.T) {
	h := newHarness(t)
	auth, artifact := approveAndExecute(t, h)
	ctx := context.Background()

	if err := h.gk.RecordLifecycle(ctx, auth, executedLifecycle(artifact.Ref, h.at)); err != nil {
		t.Fatalf("first RecordLifecycle: %v", err)
	}

	withRollback := executedLifecycle(artifact.Ref, h.at)
	withRollback = append(withRollback, audit.Record{
		Seq: 3, RecordedAt: h.at, Phase: audit.PhaseRolledBack,
		Action:   withRollback[0].Action,
		Approver: withRollback[0].Approver,
		Change:   audit.Change{Sent: true, Applied: true, FinishedAt: h.at},
		Rollback: audit.Rollback{Attempted: true, Performed: true},
	})
	if err := h.gk.RecordLifecycle(ctx, auth, withRollback); err != nil {
		t.Fatalf("second RecordLifecycle: %v", err)
	}

	got, err := audit.ParseLifecycleMarker(h.only().Body)
	if err != nil {
		t.Fatalf("the artifact carries no readable lifecycle after the second write: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the artifact reports %d records, want the 3 of the most recent write", len(got))
	}
	if !got[2].Rollback.Attempted {
		t.Error("the rollback the second write recorded is absent")
	}
}

// TestRecordLifecycle_RefusesWhatItCannotHonestlyRecord. Each refusal writes nothing: an
// artifact with no marker reads as an action still in flight, which is recoverable, while
// an artifact with a wrong or empty marker is history that exists and lies.
func TestRecordLifecycle_RefusesWhatItCannotHonestlyRecord(t *testing.T) {
	h := newHarness(t)
	auth, artifact := approveAndExecute(t, h)
	ctx := context.Background()

	if err := h.gk.RecordLifecycle(ctx, auth, nil); err == nil {
		t.Error("an empty lifecycle was written to the trail")
	}
	if err := h.gk.RecordLifecycle(ctx, &Authorization{}, executedLifecycle(artifact.Ref, h.at)); err == nil {
		t.Error("a lifecycle was recorded against an authorization the gate did not issue")
	}

	disagreeing := executedLifecycle(artifact.Ref, h.at)
	disagreeing[1].Action.Cluster = "some-other-cluster"
	if err := h.gk.RecordLifecycle(ctx, auth, disagreeing); err == nil {
		t.Error("a lifecycle whose records name two clusters was written to the trail")
	}

	if _, err := audit.ParseLifecycleMarker(h.only().Body); !errors.Is(err, audit.ErrNoMarker) {
		t.Errorf("a refused write still left a marker on the artifact (parse returned %v)", err)
	}
}

// TestListAll_ReturnsTheCLOSEDArtifactsToo is the enumeration half, and the case it
// covers is the whole reason it is needed: a finished execution's artifact is withdrawn
// and closed, so [ApprovalSink.ListOpen] — the only read the gate has — returns exactly
// none of the history a rebuild is looking for.
func TestListAll_ReturnsTheCLOSEDArtifactsToo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	auth, artifact := approveAndExecute(t, h)

	if err := h.gk.RecordLifecycle(ctx, auth, executedLifecycle(artifact.Ref, h.at)); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	// The proposal stops recurring, so the gate withdraws and closes the artifact — the
	// ordinary end of a gated action's life.
	h.at = h.at.Add(time.Hour)
	h.pass()

	if open, err := h.sink.ListOpen(ctx); err != nil {
		t.Fatalf("ListOpen: %v", err)
	} else if len(open) != 0 {
		t.Fatalf("the artifact is still open (%d), so this test is not exercising the closed case", len(open))
	}

	all, err := h.sink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListAll returned %d artifacts, want the 1 closed one", len(all))
	}
	if all[0].Ref != artifact.Ref {
		t.Errorf("ListAll returned %q, want %q", all[0].Ref, artifact.Ref)
	}
	if _, err := audit.ParseLifecycleMarker(all[0].Body); err != nil {
		t.Errorf("the closed artifact's lifecycle is unreadable: %v", err)
	}
}

// TestListAll_SkipsWhatWasNeverThisTrails. An issue a person opened by hand is not
// history this trail lost, so it is filtered rather than surfaced as an unreadable
// artifact — which a rebuild would treat as a reason to refuse.
func TestListAll_SkipsWhatWasNeverThisTrails(t *testing.T) {
	sink := NewMemorySink()
	ctx := context.Background()

	if _, err := sink.Create(ctx, "hand-written", "an issue a person opened by hand", []string{ManagedLabel}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ours, err := sink.Create(ctx, "ours", Body(testRequest(), time.Now().UTC(), DefaultPolicy()), []string{ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	all, err := sink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 1 || all[0].Ref != ours {
		t.Fatalf("ListAll returned %+v, want only the marker-carrying artifact %q", all, ours)
	}
}

// TestListAll_OrdersNumericallyRatherThanLexically. Purely so a failure message reads as
// though artifacts came back in creation order; "10" sorting before "9" looks like the
// enumeration is broken when it is not.
func TestListAll_OrdersNumericallyRatherThanLexically(t *testing.T) {
	sink := NewMemorySink()
	ctx := context.Background()

	body := func(n int) string {
		return "prose\n" + proposalMarker(remediate.ProposalIdentity("p-"+string(rune('a'+n)))) + "\n"
	}
	for i := range 11 {
		if _, err := sink.Create(ctx, "artifact", body(i), []string{ManagedLabel}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	all, err := sink.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 11 {
		t.Fatalf("ListAll returned %d artifacts, want 11", len(all))
	}
	if all[8].Ref != "9" || all[9].Ref != "10" {
		t.Errorf("artifacts 9 and 10 came back as %q and %q, want \"9\" then \"10\"", all[8].Ref, all[9].Ref)
	}
}
