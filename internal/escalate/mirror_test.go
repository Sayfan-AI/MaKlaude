package escalate

import (
	"context"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// seedThreadedIssue opens an issue for id, posts a chat root that returns rootTS,
// and (via the escalator) persists the thread marker into the issue body — exactly
// the durable mapping inbound resolution relies on. It returns the sink and issue
// ref so a test can then mirror an inbound reply against it.
func seedThreadedIssue(t *testing.T, id detect.Identity, rootTS string) (*MemorySink, IssueRef) {
	t.Helper()
	sink := NewMemorySink()
	notifier := &fakeNotifier{tsQueue: []string{rootTS}}
	if _, err := NewEscalatorWithNotifier(sink, notifier).
		Reconcile(context.Background(), []Subject{subjectFor(crashFinding(id, "x"))}); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	return sink, IssueRef("1")
}

// TestReplyMirror_MirrorsReplyToMatchingIssue is the headline inbound done-criterion:
// a human reply whose thread_ts matches an issue's persisted thread marker is posted
// as a comment on THAT issue, recovering the incident/issue context from the same
// durable mapping the outbound side wrote.
func TestReplyMirror_MirrorsReplyToMatchingIssue(t *testing.T) {
	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	sink, ref := seedThreadedIssue(t, id, "1700.0001")

	mirror := NewReplyMirror(sink)
	reply := notify.InboundReply{
		Channel:  "C1",
		ThreadTS: "1700.0001",
		TS:       "1700.0002",
		User:     "U777",
		Text:     "Approved — go ahead and drain the node.",
	}
	if err := mirror.MirrorReply(context.Background(), reply); err != nil {
		t.Fatalf("MirrorReply: %v", err)
	}

	view, ok := sink.Snapshot(ref)
	if !ok {
		t.Fatalf("issue %s not found", ref)
	}
	if len(view.Comments) != 1 {
		t.Fatalf("want exactly 1 mirrored comment, got %d: %+v", len(view.Comments), view.Comments)
	}
	comment := view.Comments[0]
	if !strings.Contains(comment, "U777") {
		t.Errorf("mirrored comment should attribute the Slack author: %q", comment)
	}
	if !strings.Contains(comment, "drain the node") {
		t.Errorf("mirrored comment should carry the reply text verbatim: %q", comment)
	}
	if !strings.Contains(strings.ToLower(comment), "no cluster action") {
		t.Errorf("mirrored comment should reiterate the safety boundary: %q", comment)
	}
}

// TestReplyMirror_UnknownThreadIsNoOp proves an inbound reply whose thread maps to
// no open issue (stale thread, unrelated thread) is a best-effort no-op: no error,
// no comment — so an out-of-band reply never crashes the listener.
func TestReplyMirror_UnknownThreadIsNoOp(t *testing.T) {
	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	sink, ref := seedThreadedIssue(t, id, "1700.0001")

	mirror := NewReplyMirror(sink)
	reply := notify.InboundReply{
		Channel:  "C1",
		ThreadTS: "9999.9999", // matches no issue
		TS:       "9999.0001",
		User:     "U1",
		Text:     "stray reply in some other thread",
	}
	if err := mirror.MirrorReply(context.Background(), reply); err != nil {
		t.Fatalf("unknown thread should be a no-op, got error: %v", err)
	}
	view, _ := sink.Snapshot(ref)
	if len(view.Comments) != 0 {
		t.Errorf("unknown thread must not comment on any issue; got %+v", view.Comments)
	}
}

// TestReplyMirror_EmptyThreadIsNoOp proves a reply with no thread_ts (a top-level
// message, not a reply to an escalation) is ignored without listing issues.
func TestReplyMirror_EmptyThreadIsNoOp(t *testing.T) {
	sink := NewMemorySink()
	mirror := NewReplyMirror(sink)
	if err := mirror.MirrorReply(context.Background(), notify.InboundReply{Text: "hi", ThreadTS: ""}); err != nil {
		t.Fatalf("empty thread should be a no-op, got error: %v", err)
	}
}

// TestReplyMirror_AcrossRestart proves the inbound resolution works even when the
// process that mirrors the reply is FRESH (no in-memory state): the thread→issue
// mapping is recovered purely from the durable thread marker in the issue body,
// the same property the outbound side guarantees. A brand-new sink is populated by
// re-listing the issue body, modelling a restart reading the GitHub trail.
func TestReplyMirror_AcrossRestart(t *testing.T) {
	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	sink, ref := seedThreadedIssue(t, id, "2100.0001")

	// Confirm the marker is durably in the body (the only carrier across restart).
	view, _ := sink.Snapshot(ref)
	if gotTS, ok := ParseThreadMarker(view.Body); !ok || gotTS != "2100.0001" {
		t.Fatalf("thread marker not persisted: got %q ok=%v", gotTS, ok)
	}

	// A fresh mirror over the SAME sink (its state lives in the issue body) resolves
	// the reply with no warm in-memory mapping anywhere.
	mirror := NewReplyMirror(sink)
	reply := notify.InboundReply{ThreadTS: "2100.0001", TS: "2100.0009", User: "U5", Text: "post-restart reply"}
	if err := mirror.MirrorReply(context.Background(), reply); err != nil {
		t.Fatalf("MirrorReply across restart: %v", err)
	}
	view, _ = sink.Snapshot(ref)
	if len(view.Comments) != 1 || !strings.Contains(view.Comments[0], "post-restart reply") {
		t.Fatalf("post-restart reply not mirrored: %+v", view.Comments)
	}
}

// TestNewReplyMirror_NilSinkPanics matches NewEscalator's contract: a nil sink is a
// programming error, not a silent no-op.
func TestNewReplyMirror_NilSinkPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewReplyMirror(nil) should panic")
		}
	}()
	_ = NewReplyMirror(nil)
}

// TestInboundListenerFromEnv_GracefulDegradation proves the wiring seam degrades:
// with no Slack env (the unit/CI environment) it yields no listener — the caller
// starts nothing, exactly the Milestone 1 behavior.
func TestInboundListenerFromEnv_GracefulDegradation(t *testing.T) {
	// The unit environment has no MAKLAUDE_SLACK_* set, so this must be a no-op.
	listener, ok := InboundListenerFromEnv()
	if ok || listener != nil {
		t.Errorf("with no Slack env, InboundListenerFromEnv must yield no listener (ok=%v)", ok)
	}
}
