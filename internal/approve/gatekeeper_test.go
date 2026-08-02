package approve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// These tests drive the gate the way it actually runs: repeated reconciliation
// passes against a sink, with the human's decision arriving between passes. The
// pure-value tests in reconcile_test.go prove what [Decide] concludes; these prove
// that the conclusion reaches the trail — that the artifact a person reads, the
// labels the next pass reads back, and the permission slip handed to an executor
// all say the same thing.
//
// Everything the gate cannot generate for itself — the approval label, its actor,
// its timestamp — arrives through [MemorySink.Decide], which is the whole test
// seam. A pass that could manufacture its own approval would prove nothing.

// harness bundles a gatekeeper with the sink behind it and a clock a test can move,
// so a multi-pass scenario reads as a sequence of events rather than as plumbing.
type harness struct {
	t    *testing.T
	sink *MemorySink
	gk   *Gatekeeper
	at   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, sink: NewMemorySink(), at: passAt}
	h.sink.SelfLogin = "maklaude-bot"
	h.gk = NewGatekeeper(h.sink, notify.NewNopNotifier(), DefaultPolicy()).
		WithClock(func() time.Time { return h.at })
	return h
}

// pass runs one reconciliation over the given proposals and fails the test on any
// sink error, since none of these scenarios involve a failing sink.
func (h *harness) pass(reqs ...Request) Result {
	h.t.Helper()
	res, err := h.gk.Reconcile(context.Background(), reqs)
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

// only returns the single open artifact, failing if the trail holds any other
// number — "exactly one artifact per decision" is itself the property under test in
// most of these scenarios.
func (h *harness) only() ArtifactView {
	h.t.Helper()
	if got := h.sink.OpenCount(); got != 1 {
		h.t.Fatalf("trail holds %d open artifacts, want 1", got)
	}
	list, err := h.sink.ListOpen(context.Background())
	if err != nil {
		h.t.Fatalf("listing: %v", err)
	}
	view, ok := h.sink.Snapshot(list[0].Ref)
	if !ok {
		h.t.Fatalf("no snapshot for %q", list[0].Ref)
	}
	return view
}

// TestNothingExecutesWithoutAnExplicitHumanApproval is T3's headline promise, run
// as a sequence rather than asserted on a single value: the gate asks, waits
// through repeated passes, and issues nothing until a named person says yes.
func TestNothingExecutesWithoutAnExplicitHumanApproval(t *testing.T) {
	h := newHarness(t)
	req := testRequest()

	// Pass 1 opens the question.
	res := h.pass(req)
	if res.Opened != 1 {
		t.Fatalf("opened = %d, want 1", res.Opened)
	}
	if len(res.Authorized) != 0 {
		t.Fatal("the opening pass issued an authorization")
	}

	artifact := h.only()
	if !artifact.HasLabel(NeedsHumanLabel) {
		t.Error("a pending artifact does not carry the label that puts it in front of an operator")
	}
	if !artifact.HasLabel(ManagedLabel) {
		t.Error("the artifact is not marked as a MaKlaude approval artifact")
	}

	// Passes 2 and 3 change nothing: the question stands, unanswered.
	for i := 2; i <= 3; i++ {
		h.at = h.at.Add(time.Minute)
		res = h.pass(req)
		if len(res.Authorized) != 0 {
			t.Fatalf("pass %d authorized an undecided proposal", i)
		}
		if res.Held != 1 {
			t.Errorf("pass %d: held = %d, want 1 (an unchanged pending artifact must be left strictly alone)", i, res.Held)
		}
		if res.Refreshed != 0 {
			t.Errorf("pass %d: refreshed = %d, want 0 — re-stamping the preview instant every pass makes PendingTTL unreachable", i, res.Refreshed)
		}
	}

	// A human approves. Only now does the gate issue anything.
	approvedAt := h.at.Add(time.Second)
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", approvedAt); err != nil {
		t.Fatalf("recording the human decision: %v", err)
	}
	h.at = approvedAt.Add(time.Minute)

	res = h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d after an explicit approval, want 1", len(res.Authorized))
	}
	auth := res.Authorized[0]
	if !auth.Valid() {
		t.Fatal("the issued authorization is not valid")
	}
	if auth.Approver() != "the-gigi" || !auth.ApprovedAt().Equal(approvedAt.UTC()) {
		t.Errorf("authorization records %q at %s, want the-gigi at %s", auth.Approver(), auth.ApprovedAt(), approvedAt.UTC())
	}
	if auth.Ref() != artifact.Ref {
		t.Errorf("authorization ref = %q, want the artifact that carried the approval %q", auth.Ref(), artifact.Ref)
	}

	// The trail records the authorization before the slip leaves the package, so an
	// audit never has to infer it from the executor's behavior.
	if !containsSubstring(h.only().Comments, "Approval honored") {
		t.Error("the artifact does not record that the approval was honored")
	}
}

// TestMaKlaudeCannotApproveItsOwnProposal covers the narrowest and most important
// forgery the gate has to survive: an automation holding an issues:write token can
// add a label to its own issue, and every other check in the refusal set assumes
// the approval came from someone other than the thing being approved.
func TestMaKlaudeCannotApproveItsOwnProposal(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	artifact := h.only()
	// AddLabel is MaKlaude acting, and the sink attributes it as such.
	if err := h.gk.sink.AddLabel(context.Background(), artifact.Ref, ApprovedLabel); err != nil {
		t.Fatalf("self-labelling: %v", err)
	}
	h.at = h.at.Add(time.Minute)

	res := h.pass(req)
	if len(res.Authorized) != 0 {
		t.Fatal("MaKlaude approved its own proposal and the gate honored it")
	}
	if res.Refused != 1 {
		t.Fatalf("refused = %d, want 1", res.Refused)
	}

	after := h.only()
	if after.HasLabel(ApprovedLabel) {
		t.Error("the self-applied approval label survived the refusal")
	}
	if !after.HasLabel(NeedsHumanLabel) {
		t.Error("the refused artifact is not back in front of a human")
	}
	if !containsSubstring(after.Comments, ReasonSelfApproval.String()) {
		t.Errorf("no comment names the refusal reason; comments = %v", after.Comments)
	}
}

// TestAnApprovalIsRefusedWhenTheObjectMovedUnderIt is the drift guarantee end to
// end. Approval authorizes THIS action at THIS cluster state; when the state moves,
// the approved action and the possible action are no longer the same action, and
// the human is re-asked rather than second-guessed.
func TestAnApprovalIsRefusedWhenTheObjectMovedUnderIt(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	artifact := h.only()
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("approving: %v", err)
	}

	// The deployment changes before the next pass picks the approval up.
	moved := testRequest()
	moved.Proposal.Target.ResourceVersion = "1001"
	h.at = h.at.Add(time.Minute)

	res := h.pass(moved)
	if len(res.Authorized) != 0 {
		t.Fatal("a drifted approval was honored")
	}
	if res.Refused != 1 {
		t.Fatalf("refused = %d, want 1", res.Refused)
	}

	after := h.only()
	if after.HasLabel(ApprovedLabel) {
		t.Error("the approval label survived the drift refusal — the authority was not actually revoked")
	}
	if !containsSubstring(after.Comments, ReasonDrift.String()) {
		t.Errorf("no comment names drift as the reason; comments = %v", after.Comments)
	}

	// The re-rendered body must show the state that actually exists now, or the
	// human is asked to re-approve against the same stale evidence.
	if rv, _, ok := ParsePreviewMarker(after.Body); !ok || rv != "1001" {
		t.Errorf("refreshed body displays rv %q (parsed ok=%t), want 1001", rv, ok)
	}

	// Re-approving against the current state authorizes normally: a refusal re-asks,
	// it does not blacklist.
	if err := h.sink.Decide(after.Ref, ApprovedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("re-approving: %v", err)
	}
	h.at = h.at.Add(time.Minute)

	if res := h.pass(moved); len(res.Authorized) != 1 {
		t.Fatalf("re-approval against current state produced %d authorizations, want 1", len(res.Authorized))
	}
}

// TestARejectionIsStickyAndIsNeverReasked covers the other human decision. A "no"
// that gets re-asked every cycle is not a gate, it is nagging — and an operator who
// is nagged learns to approve to make it stop.
func TestARejectionIsStickyAndIsNeverReasked(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	artifact := h.only()
	if err := h.sink.Decide(artifact.Ref, RejectedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("rejecting: %v", err)
	}

	frozen := h.only().Body
	for i := 1; i <= 3; i++ {
		h.at = h.at.Add(time.Minute)
		res := h.pass(req)

		if len(res.Authorized) != 0 {
			t.Fatalf("pass %d authorized a rejected proposal", i)
		}
		if res.Held != 1 {
			t.Errorf("pass %d: held = %d, want 1", i, res.Held)
		}
		if res.Opened != 0 {
			t.Fatalf("pass %d re-opened a rejected question", i)
		}
		if h.sink.OpenCount() != 1 {
			t.Fatalf("pass %d: trail holds %d artifacts, want the single rejected one", i, h.sink.OpenCount())
		}
	}

	// The record must say what was actually declined, not what MaKlaude would
	// propose today.
	if h.only().Body != frozen {
		t.Error("the rejected artifact was re-rendered; the trail no longer shows what the human turned down")
	}
}

// TestAnActionRunsAtMostOnceAcrossRestarts is the durable-idempotency property.
// The executed flag lives on the artifact rather than in memory precisely so a
// process that dies between acting and the next pass cannot act twice, so the test
// throws away the gatekeeper and rebuilds it over the same trail.
func TestAnActionRunsAtMostOnceAcrossRestarts(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	artifact := h.only()
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("approving: %v", err)
	}
	h.at = h.at.Add(time.Minute)

	res := h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d, want 1", len(res.Authorized))
	}
	auth := res.Authorized[0]

	// The caller runs the action and records the outcome.
	if err := h.gk.RecordExecution(context.Background(), auth, "rollout restarted; 3 pods replaced"); err != nil {
		t.Fatalf("recording execution: %v", err)
	}

	executed := h.only()
	if !executed.HasLabel(ExecutedLabel) {
		t.Fatal("the artifact is not marked executed — the idempotency flag is not durable")
	}
	if executed.HasLabel(NeedsHumanLabel) {
		t.Error("an executed artifact still asks for a human decision")
	}

	// The process restarts: a brand-new gatekeeper over the same trail. The proposal
	// is still being made because the cluster has not caught up yet.
	restarted := NewGatekeeper(h.sink, notify.NewNopNotifier(), DefaultPolicy()).
		WithClock(func() time.Time { return h.at })
	for i := 1; i <= 2; i++ {
		h.at = h.at.Add(time.Minute)
		res, err := restarted.Reconcile(context.Background(), []Request{req})
		if err != nil {
			t.Fatalf("pass %d after restart: %v", i, err)
		}
		if len(res.Authorized) != 0 {
			t.Fatalf("pass %d after restart re-authorized an executed action", i)
		}
		if res.Held != 1 {
			t.Errorf("pass %d after restart: held = %d, want 1", i, res.Held)
		}
	}

	// Once the problem clears, the artifact closes as completed rather than as a
	// self-heal, because the action is what fixed it.
	h.at = h.at.Add(time.Minute)
	if res := h.pass(); res.Withdrawn != 1 {
		t.Fatalf("withdrawn = %d once the proposal stopped, want 1", res.Withdrawn)
	}
	if h.sink.OpenCount() != 0 {
		t.Errorf("trail holds %d open artifacts after completion, want 0", h.sink.OpenCount())
	}
}

// TestRecordExecutionRefusesAnAuthorizationTheGateNeverIssued guards the audit
// trail against a false entry, which is worse than a missing one: an execution
// recorded against a forged slip would read as though the gate had allowed it.
func TestRecordExecutionRefusesAnAuthorizationTheGateNeverIssued(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name string
		auth *Authorization
	}{
		{"a nil authorization", nil},
		{"a struct literal from outside the gate", &Authorization{ref: ActionRef("1"), approver: "the-gigi"}},
		{"a granted slip with no artifact to record against", &Authorization{granted: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.gk.RecordExecution(context.Background(), tc.auth, "ran it"); err == nil {
				t.Fatal("RecordExecution accepted an authorization the gate did not issue")
			}
		})
	}
}

// TestRecordOutcomeAppendsWithoutTouchingAnyLabel is why the audit trail has its
// own recording method rather than reusing [Gatekeeper.RecordExecution].
//
// The executed label is applied once, the instant a mutation lands, and must mean
// exactly "a real mutation landed". Audit notes arrive afterwards, repeatedly, and
// for attempts that never executed at all — a drifted precondition, a refusal, a
// rollback. Routing those through RecordExecution would label aborted actions
// executed, which is the one label whose meaning has to stay exact.
func TestRecordOutcomeAppendsWithoutTouchingAnyLabel(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	opened := h.only()
	approvedAt := h.at.Add(time.Second)
	if err := h.sink.Decide(opened.Ref, ApprovedLabel, "the-gigi", approvedAt); err != nil {
		t.Fatalf("approving: %v", err)
	}
	h.at = approvedAt.Add(time.Minute)
	auth := h.pass(req).Authorized[0]

	before := h.only()
	if before.HasLabel(ExecutedLabel) {
		t.Fatal("the artifact was already labelled executed before anything ran")
	}

	if err := h.gk.RecordOutcome(context.Background(), auth, "## Audit trail\n\nthe action was abandoned cleanly"); err != nil {
		t.Fatalf("recording an outcome: %v", err)
	}
	if err := h.gk.RecordOutcome(context.Background(), auth, "## Audit trail\n\nand then it was rolled back"); err != nil {
		t.Fatalf("recording a second outcome: %v", err)
	}

	after := h.only()
	if after.HasLabel(ExecutedLabel) {
		t.Fatalf("recording an audit note labelled an unexecuted action executed: %v", after.Labels)
	}
	if len(after.Labels) != len(before.Labels) {
		t.Fatalf("recording an audit note changed the labels: %v → %v", before.Labels, after.Labels)
	}
	if !containsSubstring(after.Comments, "abandoned cleanly") || !containsSubstring(after.Comments, "rolled back") {
		t.Fatalf("the trail did not append both notes: %v", after.Comments)
	}
}

// TestRecordOutcomeRefusesWhatItCannotAttribute mirrors RecordExecution's guard: a
// note on a trail the gate never authorized is a false entry, and an empty note is
// a comment that tells a reader nothing while looking like it should.
func TestRecordOutcomeRefusesWhatItCannotAttribute(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name string
		auth *Authorization
		note string
	}{
		{"a nil authorization", nil, "something happened"},
		{"a struct literal from outside the gate", &Authorization{ref: ActionRef("1")}, "something happened"},
		{"a granted slip with no artifact", &Authorization{granted: true}, "something happened"},
		{"an empty note", &Authorization{granted: true, ref: ActionRef("1")}, "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.gk.RecordOutcome(context.Background(), tc.auth, tc.note); err == nil {
				t.Fatal("RecordOutcome accepted something it should have refused")
			}
		})
	}
}

// TestAPendingProposalThatSelfHealsIsWithdrawnNotExecuted is the guarantee that a
// pending approval is not a queued job. The dangerous version of this bug is silent:
// the problem clears, nobody notices the artifact, a human approves it later, and
// MaKlaude acts on a reason that no longer exists.
func TestAPendingProposalThatSelfHealsIsWithdrawnNotExecuted(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	artifact := h.only()
	// Approved, but the problem clears before the next pass picks it up.
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("approving: %v", err)
	}
	h.at = h.at.Add(time.Minute)

	res := h.pass() // no proposals: the cluster is healthy again
	if len(res.Authorized) != 0 {
		t.Fatal("an approved proposal that self-healed was still authorized")
	}
	if res.Withdrawn != 1 {
		t.Fatalf("withdrawn = %d, want 1", res.Withdrawn)
	}
	if h.sink.OpenCount() != 0 {
		t.Errorf("trail holds %d open artifacts, want 0", h.sink.OpenCount())
	}

	view, ok := h.sink.Snapshot(artifact.Ref)
	if !ok {
		t.Fatal("the withdrawn artifact is gone from the trail entirely")
	}
	if view.Open {
		t.Error("the withdrawn artifact is still open")
	}
	// The note must say plainly that nothing ran, since "approved then closed" is
	// exactly the shape a reader would otherwise assume means "it executed".
	if !containsSubstring(view.Comments, "not") {
		t.Errorf("the withdrawal note does not state whether anything ran; comments = %v", view.Comments)
	}
}

// TestRecurrenceRefreshesTheSameArtifactRatherThanOpeningAnother preserves the
// dedup semantics the escalation trail established. Two artifacts for one action are
// two chances to approve it, and therefore two executions of something a human meant
// to allow once.
func TestRecurrenceRefreshesTheSameArtifactRatherThanOpeningAnother(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)
	first := h.only().Ref

	// The same proposal recurs with the object at a new resourceVersion.
	for i, rv := range []string{"1001", "1002", "1003"} {
		moved := testRequest()
		moved.Proposal.Target.ResourceVersion = rv
		h.at = h.at.Add(time.Minute)

		res := h.pass(moved)
		if res.Opened != 0 {
			t.Fatalf("recurrence %d opened a duplicate artifact", i+1)
		}
		if res.Refreshed != 1 {
			t.Errorf("recurrence %d: refreshed = %d, want 1", i+1, res.Refreshed)
		}
		if h.sink.OpenCount() != 1 {
			t.Fatalf("recurrence %d: trail holds %d artifacts, want 1", i+1, h.sink.OpenCount())
		}

		view := h.only()
		if view.Ref != first {
			t.Fatalf("recurrence %d landed on %q, want the original artifact %q", i+1, view.Ref, first)
		}
		if got, _, ok := ParsePreviewMarker(view.Body); !ok || got != rv {
			t.Errorf("recurrence %d: body displays rv %q, want %q", i+1, got, rv)
		}
	}
}

// TestTheTrailNamesWhoApprovedWhatAndWhen is the auditability criterion. Everything
// a reviewer needs must be recoverable from the artifact alone, months later,
// without the process that wrote it.
func TestTheTrailNamesWhoApprovedWhatAndWhen(t *testing.T) {
	h := newHarness(t)
	req := testRequest()
	h.pass(req)

	opened := h.only()
	// The question itself must be answerable without opening anything else.
	for _, want := range []string{"prod", "shop/web", "Rollback"} {
		if !strings.Contains(opened.Body+opened.Title, want) {
			t.Errorf("the artifact never mentions %q; a human cannot decide from it alone", want)
		}
	}
	if _, ok := ParseProposalMarker(opened.Body); !ok {
		t.Error("the artifact carries no proposal marker, so no later pass can rediscover what it tracks")
	}

	approvedAt := h.at.Add(time.Second)
	if err := h.sink.Decide(opened.Ref, ApprovedLabel, "the-gigi", approvedAt); err != nil {
		t.Fatalf("approving: %v", err)
	}
	h.at = approvedAt.Add(time.Minute)

	auth := h.pass(req).Authorized[0]
	if err := h.gk.RecordExecution(context.Background(), auth, "rollout restarted"); err != nil {
		t.Fatalf("recording execution: %v", err)
	}

	final := h.only()
	trail := strings.Join(final.Comments, "\n")
	for _, want := range []string{
		"the-gigi",                            // who
		approvedAt.UTC().Format(time.RFC3339), // when they consented
		"1000",                                // the cluster state it was bound to
		"rollout restarted",                   // what actually happened
	} {
		if !strings.Contains(trail, want) {
			t.Errorf("the trail never records %q; comments = %v", want, final.Comments)
		}
	}
}

// containsSubstring reports whether any string in the slice contains want. The
// assertions above check that a comment SAYS something, not that it is worded
// exactly one way — wording is covered in approve_test.go and pinning it twice
// would make every copy edit fail two tests.
func containsSubstring(haystack []string, want string) bool {
	for _, s := range haystack {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
