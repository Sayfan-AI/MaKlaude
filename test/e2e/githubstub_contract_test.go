//go:build e2e

// This file checks the stub trail against the real client that will drive it.
//
// It needs no cluster and no credentials, and it exists because of where the stub sits:
// TestE2E_BinaryTwoPassGatedRemediation's entire result is "what did approve.GitHubSink
// conclude", so a stub that answers a shape the sink cannot read turns a green gate into
// a red e2e with the failure a hundred lines away from its cause. Pinning the wire
// contract here localizes that.
//
// It is deliberately NOT a test of the gate's policy — reconcile_test.go and
// gatekeeper_test.go own that. The question here is narrower and purely mechanical: does
// the live sink recover, from this stub, the four facts the gate decides on — which
// artifacts are open, what identity each carries, whether a decision label is present,
// and WHO applied it.
package e2e

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
)

// TestStubTrailSatisfiesTheLiveSink drives approve.GitHubSink — the production client,
// unmodified — through every request shape it can issue, against the stub.
func TestStubTrailSatisfiesTheLiveSink(t *testing.T) {
	const (
		self     = "stub-contract-bot"
		human    = "stub-contract-operator"
		identity = "contract-identity-1"
	)
	ctx := context.Background()
	stub := newGitHubStub(t, "owner", "repo", "contract-token", self)

	cfg := escalate.GitHubConfig{
		Owner: "owner", Repo: "repo", Token: "contract-token", APIBase: stub.apiBase(),
	}
	if !cfg.Configured() {
		t.Fatal("a config with owner, repo and token must be Configured; the stub is unreachable otherwise")
	}
	sink, ok := approve.NewGitHubSink(cfg, self)
	if !ok {
		t.Fatal("NewGitHubSink refused a configured config")
	}

	// The proposal marker is written literally rather than through a helper, because
	// pinning the wire format is the point: approve.ParseProposalMarker reads exactly
	// this, and a body the sink cannot parse is an artifact it will not manage.
	body := "Rollback of deployment/ns/thing.\n\n<!-- maklaude:proposal=" + identity + " -->\n"

	ref, err := sink.Create(ctx, "[APPROVAL] contract", body, []string{approve.ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// --- Undecided: listed, identified, and carrying no decision. ---
	pending := soleOpen(ctx, t, sink)
	if pending.Identity != identity {
		t.Errorf("recovered identity %q, want %q", pending.Identity, identity)
	}
	if pending.Ref != ref {
		t.Errorf("recovered ref %q, want %q", pending.Ref, ref)
	}
	if pending.State != approve.StatePending {
		t.Errorf("an artifact with no decision label reads as %v, want pending", pending.State)
	}

	// --- Decided by MaKlaude: recognized as self, which is what the gate refuses on. ---
	if err := sink.AddLabel(ctx, ref, approve.ApprovedLabel); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	selfDecided := soleOpen(ctx, t, sink)
	if selfDecided.State != approve.StateApproved {
		t.Errorf("after the approval label the artifact reads as %v, want approved", selfDecided.State)
	}
	if selfDecided.Approver != self {
		t.Errorf("recovered approver %q, want %q — attribution comes from the label EVENT, and the stub must serve it",
			selfDecided.Approver, self)
	}
	if !selfDecided.ApproverIsSelf {
		t.Error("SELF-APPROVAL HOLE IN THE HARNESS: a label MaKlaude applied through the API is not recognized as its own, " +
			"so the binary test's negative control would pass whether or not the gate works")
	}

	// --- Withdrawn, then re-applied by a person: the SECOND attribution is the one that
	// stands. An `unlabeled` event must retire the first, or a refused self-approval
	// would keep answering for the human one that replaced it. ---
	if err := sink.RemoveLabel(ctx, ref, approve.ApprovedLabel); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if withdrawn := soleOpen(ctx, t, sink); withdrawn.State != approve.StatePending {
		t.Errorf("after withdrawing the approval the artifact reads as %v, want pending", withdrawn.State)
	}
	// A second removal must not fail: RemoveLabel treats 404 as success precisely so the
	// refusal path can be retried.
	if err := sink.RemoveLabel(ctx, ref, approve.ApprovedLabel); err != nil {
		t.Errorf("removing an absent label must succeed (the stub answers 404, the sink swallows it): %v", err)
	}

	stub.decideAs(t, issueNumber(t, ref), approve.ApprovedLabel, human)
	humanDecided := soleOpen(ctx, t, sink)
	if humanDecided.State != approve.StateApproved {
		t.Errorf("after a person's approval the artifact reads as %v, want approved", humanDecided.State)
	}
	if humanDecided.Approver != human {
		t.Errorf("recovered approver %q, want %q", humanDecided.Approver, human)
	}
	if humanDecided.ApproverIsSelf {
		t.Error("a person's approval is being read as MaKlaude's own; the binary test could never authorize anything")
	}
	if humanDecided.DecidedAt.IsZero() {
		t.Error("the decision carries no timestamp; the approval-lifetime check has nothing to measure and would silently pass")
	}

	// --- The remaining shapes: comment, executed marker, close. ---
	if err := sink.Comment(ctx, ref, "executed"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := sink.AddLabel(ctx, ref, approve.ExecutedLabel); err != nil {
		t.Fatalf("AddLabel(executed): %v", err)
	}
	if done := soleOpen(ctx, t, sink); !done.Executed {
		t.Error("the executed marker is not recovered, so a later pass would re-ask about work already done")
	}
	if err := sink.Close(ctx, ref); err != nil {
		t.Fatalf("Close: %v", err)
	}
	open, err := sink.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen after close: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("a closed artifact is still listed as open: %+v", open)
	}

	if n := stub.unauthorizedCount(); n != 0 {
		t.Errorf("%d request(s) arrived without the bearer token", n)
	}
}

// TestStubTrailDecisionsCannotPredateTheArtifactTheyDecide pins the one property of the
// stub's clock that the gate reads as a safety signal.
//
// approve.Body stamps a preview instant from the REAL clock the binary runs on, and
// disqualify refuses an approval recorded before it (approve.ReasonApprovalPredatesPreview)
// because such a decision is consent to a body that has since been replaced. The stub
// invents its own timestamps for label events, so if that invented clock sits behind the
// real one, every simulated human approval predates the artifact it is deciding and the
// gate correctly refuses all of them — forever, on a wedge that no re-approval can clear.
//
// That is not a hypothetical: the first version of this stub anchored its clock one minute
// in the past, and TestE2E_BinaryTwoPassGatedRemediation could not authorize anything.
// The property is cheap to state and needs no cluster, so it is stated here rather than
// discovered again as a six-minute red e2e whose cause is in a different file.
func TestStubTrailDecisionsCannotPredateTheArtifactTheyDecide(t *testing.T) {
	const (
		self     = "stub-clock-bot"
		human    = "stub-clock-operator"
		identity = "clock-identity-1"
	)
	ctx := context.Background()
	stub := newGitHubStub(t, "owner", "repo", "clock-token", self)

	sink, ok := approve.NewGitHubSink(escalate.GitHubConfig{
		Owner: "owner", Repo: "repo", Token: "clock-token", APIBase: stub.apiBase(),
	}, self)
	if !ok {
		t.Fatal("NewGitHubSink refused a configured config")
	}

	// The instant approve.Body would stamp, at the precision the marker round-trips at:
	// RFC3339 is second-resolution, and the gate compares the parsed values, so the
	// comparison this test makes is exactly the comparison disqualify makes.
	previewedAt := time.Now().UTC().Truncate(time.Second)
	body := "Rollback of deployment/ns/thing.\n\n" +
		"<!-- maklaude:proposal=" + identity + " -->\n" +
		"<!-- maklaude:preview=4242@" + previewedAt.Format(time.RFC3339) + " -->\n"

	ref, err := sink.Create(ctx, "[APPROVAL] clock", body, []string{approve.ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A person approves AFTER the artifact was rendered — the only ordering a real
	// approval can have, since the human reads the body before deciding.
	stub.decideAs(t, issueNumber(t, ref), approve.ApprovedLabel, human)

	pending := soleOpen(ctx, t, sink)
	if !pending.PreviewedAt.Equal(previewedAt) {
		t.Fatalf("the sink recovered preview instant %s, want %s — the marker written here is not the one it parses",
			pending.PreviewedAt, previewedAt)
	}
	if pending.DecidedAt.Before(pending.PreviewedAt) {
		t.Fatalf("the stub stamped a decision at %s against an artifact previewed at %s (%s earlier). "+
			"approve.disqualify refuses that as ReasonApprovalPredatesPreview, so no simulated approval in this "+
			"package can ever authorize an action",
			pending.DecidedAt, pending.PreviewedAt, pending.PreviewedAt.Sub(pending.DecidedAt))
	}

	// The other half of the clock's contract, which the ordering fix must not break:
	// distinct events get distinct, increasing timestamps at the resolution the wire
	// format carries. Equal stamps would make "which approval currently stands" depend on
	// something other than the order the events happened in.
	if err := sink.RemoveLabel(ctx, ref, approve.ApprovedLabel); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	stub.decideAs(t, issueNumber(t, ref), approve.ApprovedLabel, human)
	reDecided := soleOpen(ctx, t, sink)
	if !reDecided.DecidedAt.After(pending.DecidedAt) {
		t.Errorf("a re-application stamped %s, which is not after the first decision at %s; "+
			"the stub's timestamps must strictly increase",
			reDecided.DecidedAt, pending.DecidedAt)
	}
}

// TestStubTrailServesAnActorlessDecisionAsUnattributed pins the precondition that
// pass 4 of TestE2E_BinaryTwoPassGatedRemediation depends on and cannot itself
// distinguish.
//
// That pass asserts the gate REFUSES an approval nobody can be named for. But a refusal
// is also what the gate produces when it never saw the approval at all, so if the stub
// served an actorless event in a shape the sink drops on the floor, pass 4 would go
// green against a gate that was never asked the question. The fact under test is
// therefore not the refusal but the input to it: the artifact must read as APPROVED with
// an EMPTY approver — approved, so disqualify() is reached, and unnamed, so it reaches
// ReasonUnattributedApproval rather than authorizing.
//
// It also pins the branch: an empty login must not be mistaken for MaKlaude's own
// (approve.isSelfActor returns false on ""), or the refusal would arrive under the wrong
// reason and pass 4's comment assertion would fail a hundred lines from the cause.
func TestStubTrailServesAnActorlessDecisionAsUnattributed(t *testing.T) {
	const (
		self     = "stub-actorless-bot"
		identity = "actorless-identity-1"
	)
	ctx := context.Background()
	stub := newGitHubStub(t, "owner", "repo", "actorless-token", self)

	sink, ok := approve.NewGitHubSink(escalate.GitHubConfig{
		Owner: "owner", Repo: "repo", Token: "actorless-token", APIBase: stub.apiBase(),
	}, self)
	if !ok {
		t.Fatal("NewGitHubSink refused a configured config")
	}

	body := "Rollback of deployment/ns/thing.\n\n<!-- maklaude:proposal=" + identity + " -->\n"
	ref, err := sink.Create(ctx, "[APPROVAL] actorless", body, []string{approve.ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stub.decideAsNobody(t, issueNumber(t, ref), approve.ApprovedLabel)

	got := soleOpen(ctx, t, sink)
	if got.State != approve.StateApproved {
		t.Fatalf("an actorless approval label reads as %v, want approved — the gate must REACH the attribution "+
			"check to refuse there, and an artifact that reads as pending never does", got.State)
	}
	if got.Approver != "" {
		t.Errorf("recovered approver %q from an event serving `\"actor\": null`, want empty; "+
			"approve.ReasonUnattributedApproval fires on an empty approver and nothing else", got.Approver)
	}
	if got.ApproverIsSelf {
		t.Error("an actorless approval is being read as MaKlaude's own; it would be refused as a self-approval " +
			"and the attribution check would stay unproven")
	}
	if got.DecidedAt.IsZero() {
		t.Error("the actorless event carries no timestamp, so it is malformed in a second way and would not " +
			"isolate the attribution check")
	}

	if n := stub.unauthorizedCount(); n != 0 {
		t.Errorf("%d request(s) arrived without the bearer token", n)
	}
}

// soleOpen requires exactly one open artifact and returns it.
func soleOpen(ctx context.Context, t *testing.T, sink *approve.GitHubSink) approve.PendingAction {
	t.Helper()
	open, err := sink.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("the trail holds %d open artifacts, want exactly 1: %+v", len(open), open)
	}
	return open[0]
}

// issueNumber converts the sink's opaque ref back to the stub's issue number.
func issueNumber(t *testing.T, ref approve.ActionRef) int {
	t.Helper()
	n, err := strconv.Atoi(string(ref))
	if err != nil {
		t.Fatalf("the sink's ref %q is not an issue number: %v", ref, err)
	}
	return n
}
