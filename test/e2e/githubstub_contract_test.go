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
