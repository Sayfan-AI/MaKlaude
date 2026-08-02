package disclose

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// recordingNotifier captures what reached chat, so a test can assert the message a person
// is paged with rather than only that a call happened.
type recordingNotifier struct {
	summaries  []string
	needsHuman []bool
	err        error
}

func (n *recordingNotifier) NotifyEscalation(_ context.Context, _ detect.Identity, summary, _ string, needsHuman bool) (string, error) {
	n.summaries = append(n.summaries, summary)
	n.needsHuman = append(n.needsHuman, needsHuman)
	return "thread-1", n.err
}

func (n *recordingNotifier) NotifyUpdate(context.Context, detect.Identity, string, string) error {
	return nil
}

func (n *recordingNotifier) NotifyResolution(context.Context, detect.Identity, string, string) error {
	return nil
}

var _ notify.Notifier = (*recordingNotifier)(nil)

// brokenSink fails one named operation and delegates the rest, so a test can assert that
// a partial failure is reported rather than swallowed — and that the steps after it still
// run.
type brokenSink struct {
	*MemorySink
	failOn string
}

var errSinkDown = errors.New("the trail is unreachable")

func (s *brokenSink) Create(ctx context.Context, title, body string, labels []string) (Ref, error) {
	if s.failOn == "Create" {
		return "", errSinkDown
	}
	return s.MemorySink.Create(ctx, title, body, labels)
}

func (s *brokenSink) SetBody(ctx context.Context, ref Ref, body string) error {
	if s.failOn == "SetBody" {
		return errSinkDown
	}
	return s.MemorySink.SetBody(ctx, ref, body)
}

func (s *brokenSink) AddLabel(ctx context.Context, ref Ref, label string) error {
	if s.failOn == "AddLabel" {
		return errSinkDown
	}
	return s.MemorySink.AddLabel(ctx, ref, label)
}

// newTrail builds a trail over a fresh memory sink on a fixed clock.
func newTrail(t *testing.T) (*Trail, *MemorySink, *recordingNotifier) {
	t.Helper()
	sink := NewMemorySink()
	notifier := &recordingNotifier{}
	trail, err := NewTrail(sink, notifier)
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	return trail.WithClock(func() time.Time { return finishedAt }), sink, notifier
}

// policySlip mints the permission slip an auto-applied action runs under.
func policySlip(t *testing.T, ref Ref) *approve.Authorization {
	t.Helper()
	a := earnedAction()
	auth, err := approve.GrantAutonomous(approve.Request{Proposal: a.Proposal}, a.Verdict, a.Grant, approve.ActionRef(ref), appliedAt)
	if err != nil {
		t.Fatalf("GrantAutonomous: %v", err)
	}
	return auth
}

// TestNewTrail_RefusesWithNowhereToWrite. A trail with no sink would let an unattended
// action run with no record anywhere, which is the state this package exists to prevent.
func TestNewTrail_RefusesWithNowhereToWrite(t *testing.T) {
	if _, err := NewTrail(nil, notify.NewNopNotifier()); err == nil {
		t.Fatal("a trail was built with no sink")
	}
	// A missing notifier is a different matter: chat is a second copy of a record whose
	// durable home is the artifact.
	if _, err := NewTrail(NewMemorySink(), nil); err != nil {
		t.Fatalf("a trail with no notifier was refused: %v", err)
	}
}

// TestOpen_CreatesTheArtifactBeforeTheActionRuns is the ordering property the whole
// disclosure design rests on.
//
// An action that starts and never reports back has to leave something behind, and the
// only way it can is if the something already exists. The test asserts the artifact is
// open, carries the markers a later pass reads, and does NOT yet carry the applied label —
// whose absence is the evidence that the action has not landed.
func TestOpen_CreatesTheArtifactBeforeTheActionRuns(t *testing.T) {
	trail, sink, _ := newTrail(t)

	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	view, ok := sink.Snapshot(ref)
	if !ok {
		t.Fatal("Open returned a reference the sink does not hold")
	}
	if !view.Open {
		t.Error("the disclosure was opened closed")
	}
	if !view.HasLabel(ManagedLabel) {
		t.Errorf("the disclosure does not carry %s, so no later pass would find it", ManagedLabel)
	}
	if view.HasLabel(AppliedLabel) {
		t.Errorf("the disclosure carries %s before anything ran", AppliedLabel)
	}
	if _, ok := ParseProposalMarker(view.Body); !ok {
		t.Error("the disclosure carries no proposal marker")
	}
	if _, ok := ParseShapeMarker(view.Body); !ok {
		t.Error("the disclosure carries no shape marker, so a revocation could not be attributed")
	}
}

// TestOpen_RefusesAnActionThatWasNotAutoApplicable. Minting a bad slip means the action
// does not run; opening a bad artifact means a public record asserts a permission that was
// never granted, which is why the check is repeated here rather than left to the mint.
func TestOpen_RefusesAnActionThatWasNotAutoApplicable(t *testing.T) {
	trail, sink, _ := newTrail(t)

	a := earnedAction()
	a.Verdict = autonomy.Verdict{Decision: autonomy.DecisionGate, Reason: autonomy.ReasonUntrustedShape}
	if _, err := trail.Open(context.Background(), a); err == nil {
		t.Fatal("a gated action was disclosed as an unattended one")
	}
	if sink.OpenCount() != 0 {
		t.Error("a refused Open left an artifact behind")
	}
}

// TestRecordExecution_MarksTheArtifactAppliedAndSaysNoHumanApprovedIt.
//
// The label is the machine-readable half — its absence on a finished artifact is the only
// evidence that a process died mid-action — and the comment is the half that notifies
// somebody at the moment their cluster changed, rather than a minute and a half later when
// the observation window closes.
func TestRecordExecution_MarksTheArtifactAppliedAndSaysNoHumanApprovedIt(t *testing.T) {
	trail, sink, _ := newTrail(t)
	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := trail.RecordExecution(context.Background(), policySlip(t, ref), "patched the deployment"); err != nil {
		t.Fatalf("RecordExecution: %v", err)
	}
	view, _ := sink.Snapshot(ref)
	if !view.HasLabel(AppliedLabel) {
		t.Errorf("the artifact was not marked %s after a mutation landed", AppliedLabel)
	}
	if len(view.Comments) != 1 {
		t.Fatalf("RecordExecution posted %d comments, want 1", len(view.Comments))
	}
	if !strings.HasPrefix(view.Comments[0], bannerNoHuman) {
		t.Errorf("the execution note does not lead with the banner:\n%s", view.Comments[0])
	}
}

// TestRecordExecution_AppliesTheLabelEvenWhenTheCommentFails. The comment is for a person;
// the label is the fact. Losing the second because the first failed would erase the only
// evidence that the action landed.
func TestRecordExecution_AppliesTheLabelEvenWhenTheCommentFails(t *testing.T) {
	mem := NewMemorySink()
	sink := &failingComment{MemorySink: mem}
	trail, err := NewTrail(sink, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := trail.RecordExecution(context.Background(), policySlip(t, ref), "patched"); err == nil {
		t.Fatal("a failed comment was not reported")
	}
	view, _ := mem.Snapshot(ref)
	if !view.HasLabel(AppliedLabel) {
		t.Error("the applied label was skipped because the comment failed")
	}
}

// failingComment fails only Comment.
type failingComment struct{ *MemorySink }

func (s *failingComment) Comment(context.Context, Ref, string) error { return errSinkDown }

// TestRecordExecution_RefusesSlipsThatDoNotBelongOnThisTrail is the mirror of the
// "never reads as human-approved" criterion, enforced at the write rather than at the
// renderer.
//
// A human-approved action recorded here would produce a public artifact headed "no human
// approved this" describing an action a person did approve. The blanket bypass is refused
// for the same reason with the sign flipped: it belongs to the approval artifact it was
// granted against, and disclosing it here would attribute it to an earned rule.
func TestRecordExecution_RefusesSlipsThatDoNotBelongOnThisTrail(t *testing.T) {
	trail, sink, _ := newTrail(t)
	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A slip the gate never issued.
	if err := trail.RecordExecution(context.Background(), &approve.Authorization{}, "x"); err == nil {
		t.Error("an ungranted authorization was recorded")
	}
	if err := trail.RecordOutcome(context.Background(), nil, "x"); err == nil {
		t.Error("a nil authorization was recorded")
	}

	// A human-approved slip. Built through the gate so it is a real one.
	human := humanSlip(t)
	err = trail.RecordExecution(context.Background(), human, "x")
	if err == nil {
		t.Fatal("a human-approved action was disclosed as unattended")
	}
	if !strings.Contains(err.Error(), "unattended actions only") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	view, _ := sink.Snapshot(ref)
	if len(view.Comments) != 0 {
		t.Errorf("a refused record still wrote to the trail: %v", view.Comments)
	}
}

// TestComplete_RecordsTheOutcomeAndAnnouncesIt walks the success path end to end.
func TestComplete_RecordsTheOutcomeAndAnnouncesIt(t *testing.T) {
	trail, sink, notifier := newTrail(t)
	action := earnedAction()
	ref, err := trail.Open(context.Background(), action)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := trail.Complete(context.Background(), ref, action, convergedOutcome()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	view, _ := sink.Snapshot(ref)
	if !view.Open {
		t.Error("MaKlaude closed a disclosed action's artifact; closing it is a person's acknowledgement")
	}
	if view.HasLabel(NeedsHumanLabel) {
		t.Error("a successful unattended action was marked needs:human")
	}
	if _, err := audit.ParseLifecycleMarker(view.Body); err != nil {
		t.Errorf("the completed body carries no readable lifecycle marker: %v", err)
	}
	if len(view.Comments) != 1 {
		t.Fatalf("Complete posted %d comments, want 1", len(view.Comments))
	}
	if len(notifier.summaries) != 1 {
		t.Fatalf("Complete announced %d times, want 1", len(notifier.summaries))
	}
	if !strings.HasPrefix(notifier.summaries[0], bannerNoHuman) {
		t.Errorf("the chat announcement does not lead with the banner: %q", notifier.summaries[0])
	}
	if notifier.needsHuman[0] {
		t.Error("a successful unattended action paged a human")
	}
}

// TestComplete_EscalatesAFailureBeforeItRewritesTheBody.
//
// The ordering is what lets the body state its own escalation failure. A demotion or an
// escalation that failed silently would leave a person uninformed after an unattended
// failure, and the artifact is the only place that could have said so.
func TestComplete_EscalatesAFailureBeforeItRewritesTheBody(t *testing.T) {
	trail, sink, notifier := newTrail(t)
	action := earnedAction()
	ref, err := trail.Open(context.Background(), action)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	out := Outcome{
		Report:      failedReport(),
		Records:     lifecycle("timed-out", "", false, false),
		Consequence: budget.Consequence{Demote: true, Escalate: true, Tripped: true, ConsecutiveFailures: 2},
	}
	if err := trail.Complete(context.Background(), ref, action, out); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	view, _ := sink.Snapshot(ref)
	if !view.HasLabel(NeedsHumanLabel) {
		t.Error("a failed unattended action was not marked needs:human")
	}
	if !strings.Contains(strings.Join(view.Comments, "\n"), "Nobody was watching when it ran") {
		t.Error("the escalation comment did not reach the trail")
	}
	if !notifier.needsHuman[0] {
		t.Error("the chat announcement did not flag that a person is needed")
	}
}

// TestComplete_ReportsAFailedEscalationInTheBodyItWrites is the property the ordering
// buys, asserted directly.
func TestComplete_ReportsAFailedEscalationInTheBodyItWrites(t *testing.T) {
	mem := NewMemorySink()
	sink := &brokenSink{MemorySink: mem, failOn: "AddLabel"}
	trail, err := NewTrail(sink, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	action := earnedAction()
	ref, err := trail.Open(context.Background(), action)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	out := Outcome{
		Report:      failedReport(),
		Records:     lifecycle("timed-out", "", false, false),
		Consequence: budget.Consequence{Escalate: true, ConsecutiveFailures: 1},
	}
	if err := trail.Complete(context.Background(), ref, action, out); err == nil {
		t.Fatal("a failed escalation was not reported to the caller")
	}
	view, _ := mem.Snapshot(ref)
	if !strings.Contains(view.Body, "This could not be escalated") {
		t.Errorf("the body does not report that the escalation failed:\n%s", view.Body)
	}
}

// TestComplete_ContinuesAfterAFailedStep. A chat outage must not cost the durable record,
// and a body rewrite that failed must not cost the comment that notifies somebody.
func TestComplete_ContinuesAfterAFailedStep(t *testing.T) {
	mem := NewMemorySink()
	sink := &brokenSink{MemorySink: mem, failOn: "SetBody"}
	notifier := &recordingNotifier{}
	trail, err := NewTrail(sink, notifier)
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	action := earnedAction()
	ref, err := trail.Open(context.Background(), action)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := trail.Complete(context.Background(), ref, action, convergedOutcome()); err == nil {
		t.Fatal("the failed body rewrite was not reported")
	}
	view, _ := mem.Snapshot(ref)
	if len(view.Comments) == 0 {
		t.Error("the outcome comment was skipped because the body rewrite failed")
	}
	if len(notifier.summaries) == 0 {
		t.Error("the announcement was skipped because the body rewrite failed")
	}
}

// TestAbandon_ClosesAnArtifactForAnActionThatNeverRan. It is the one path on which
// MaKlaude closes a disclosure itself: noise on the trail that exists to be noticed is the
// one cost this package cannot afford.
func TestAbandon_ClosesAnArtifactForAnActionThatNeverRan(t *testing.T) {
	trail, sink, _ := newTrail(t)
	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := trail.Abandon(context.Background(), ref, "the permission slip was refused"); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	view, _ := sink.Snapshot(ref)
	if view.Open {
		t.Error("an abandoned disclosure is still open")
	}
	if !strings.Contains(strings.Join(view.Comments, "\n"), "Nothing was sent to the cluster") {
		t.Error("the abandonment does not say that nothing was sent")
	}
}

// TestRevocations_ReadsTheSingleSignalFromTheOpenArtifacts is the revocation criterion at
// the layer that enforces it.
func TestRevocations_ReadsTheSingleSignalFromTheOpenArtifacts(t *testing.T) {
	trail, sink, _ := newTrail(t)
	action := earnedAction()
	ref, err := trail.Open(context.Background(), action)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Before anybody says anything, nothing is revoked.
	revoked, err := trail.Revocations(context.Background())
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(revoked) != 0 {
		t.Fatalf("an untouched trail reports %d revocations", len(revoked))
	}

	// One label, applied by a person, is the whole of the signal.
	if err := sink.Revoke(ref); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	revoked, err = trail.Revocations(context.Background())
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if got, ok := revoked[action.Shape()]; !ok || got != ref {
		t.Fatalf("Revocations = %v, want shape %s revoked on %s", revoked, action.Shape(), ref)
	}
}

// TestRevocations_AreLiftedByClosingTheArtifact. Removing the label and closing the issue
// are each one action for a person, and neither should require them to remember a second
// mechanism for undoing what the first one did.
func TestRevocations_AreLiftedByClosingTheArtifact(t *testing.T) {
	trail, sink, _ := newTrail(t)
	ref, err := trail.Open(context.Background(), earnedAction())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sink.Revoke(ref); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := sink.Close(context.Background(), ref); err != nil {
		t.Fatalf("Close: %v", err)
	}

	revoked, err := trail.Revocations(context.Background())
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(revoked) != 0 {
		t.Fatalf("closing the artifact did not lift the revocation: %v", revoked)
	}
}

// TestRevocations_SkipAnArtifactWithNoReadableShape. A revocation that cannot be
// attributed to a shape cannot be applied to one, and the alternative — revoking
// everything on an unreadable marker — would let one malformed body silently disable
// autonomy an operator had earned.
func TestRevocations_SkipAnArtifactWithNoReadableShape(t *testing.T) {
	sink := NewMemorySink()
	trail, err := NewTrail(sink, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	ref, err := sink.Create(context.Background(),
		"hand-written", proposalMarker(remediate.ProposalIdentity("x")), []string{ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sink.Revoke(ref); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	revoked, err := trail.Revocations(context.Background())
	if err != nil {
		t.Fatalf("Revocations: %v", err)
	}
	if len(revoked) != 0 {
		t.Fatalf("a shapeless artifact revoked %v", revoked)
	}
}

// TestRevocations_ReportTheirOwnReadFailure. The caller treats the error as disqualifying;
// this asserts the error is surfaced at all, since an empty map on failure is
// indistinguishable from "nothing is revoked".
func TestRevocations_ReportTheirOwnReadFailure(t *testing.T) {
	trail, err := NewTrail(&unreadableSink{MemorySink: NewMemorySink()}, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("NewTrail: %v", err)
	}
	if _, err := trail.Revocations(context.Background()); err == nil {
		t.Fatal("an unreadable trail reported no revocations and no error")
	}
}

// unreadableSink fails only ListOpen.
type unreadableSink struct{ *MemorySink }

func (s *unreadableSink) ListOpen(context.Context) ([]Disclosed, error) { return nil, errSinkDown }

// TestTrail_DisclosesEachActionIndividually is the never-batched criterion. Three actions
// produce three artifacts, each naming its own target — a digest is exactly the shape in
// which the third one goes unnoticed.
func TestTrail_DisclosesEachActionIndividually(t *testing.T) {
	trail, sink, notifier := newTrail(t)

	names := []string{"web", "api", "worker"}
	refs := make([]Ref, 0, len(names))
	for _, name := range names {
		a := earnedAction()
		a.Proposal.Target.Name = name
		a.Proposal.Identity = remediate.ProposalIdentity("rolloutrestart|prod|deployment|shop|" + name)
		ref, err := trail.Open(context.Background(), a)
		if err != nil {
			t.Fatalf("Open %s: %v", name, err)
		}
		refs = append(refs, ref)
		if err := trail.Complete(context.Background(), ref, a, convergedOutcome()); err != nil {
			t.Fatalf("Complete %s: %v", name, err)
		}
	}

	if sink.OpenCount() != len(names) {
		t.Fatalf("%d actions produced %d artifacts", len(names), sink.OpenCount())
	}
	for i, ref := range refs {
		view, _ := sink.Snapshot(ref)
		if !strings.Contains(view.Title, names[i]) {
			t.Errorf("artifact %s does not name its own target %q: %q", ref, names[i], view.Title)
		}
	}
	if len(notifier.summaries) != len(names) {
		t.Errorf("%d actions produced %d chat messages; the noise is the oversight", len(names), len(notifier.summaries))
	}
}

// humanSlip builds a genuine human-approved authorization through the gate, so the refusal
// test above is exercised against a real slip rather than a hand-made one.
func humanSlip(t *testing.T) *approve.Authorization {
	t.Helper()

	p := testProposal()
	sink := approve.NewMemorySink()
	sink.SelfLogin = "maklaude-bot"
	gate := approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy()).
		WithClock(func() time.Time { return finishedAt })

	req := approve.Request{
		Proposal: p,
		Preview:  approve.Preview{Performed: true, Summary: "the API server accepted a dryRun=All patch"},
	}
	// First pass opens the artifact; a person then approves it; the second pass honors it.
	if _, err := gate.Reconcile(context.Background(), []approve.Request{req}); err != nil {
		t.Fatalf("opening the approval artifact: %v", err)
	}
	open, err := sink.ListOpen(context.Background())
	if err != nil || len(open) != 1 {
		t.Fatalf("listing the approval trail: %v (%d artifacts)", err, len(open))
	}
	// After the preview was rendered, not before: the gate refuses an approval that
	// predates the state the artifact displayed.
	if err := sink.Decide(open[0].Ref, approve.ApprovedLabel, "a-person", finishedAt.Add(time.Second)); err != nil {
		t.Fatalf("recording the approval: %v", err)
	}
	result, err := gate.Reconcile(context.Background(), []approve.Request{req})
	if err != nil {
		t.Fatalf("honoring the approval: %v", err)
	}
	if len(result.Authorized) != 1 {
		t.Fatalf("the gate issued %d slips, want 1", len(result.Authorized))
	}
	return result.Authorized[0]
}
