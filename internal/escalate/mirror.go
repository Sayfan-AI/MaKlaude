package escalate

import (
	"context"
	"fmt"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// ReplyMirror is the escalate-side implementation of [notify.ReplyMirror]: it
// takes a human reply captured inbound from a Slack escalation thread, resolves
// which tracked issue that thread belongs to (using the SAME durable thread marker
// T3 persists for outbound continuity), and mirrors the reply onto that issue as a
// comment — so the GitHub trail records the full two-way conversation.
//
// # Why this lives in escalate, not notify
//
// Resolving a Slack thread_ts back to an issue requires reading the issue store
// (to match the persisted thread marker) and writing a comment to it — both are
// the [IssueSink]'s domain. notify must not import escalate (an import cycle), so
// notify defines the [notify.ReplyMirror] seam and escalate implements it here.
// This is the inbound mirror of the same split that keeps outbound continuity in
// the escalate layer.
//
// # Safety boundary (LOCKED)
//
// This type is comms-only. Its ONLY effect of an inbound reply is a GitHub
// comment; it holds no Kubernetes client and has no path to any cluster mutation.
// An inbound reply can never trigger an actionable or destructive operation — it
// is recorded for humans and nothing more.
type ReplyMirror struct {
	sink IssueSink
}

// NewReplyMirror builds a mirror over the given sink. A nil sink panics, matching
// [NewEscalator]: a caller wanting a no-op should pass a [MemorySink].
func NewReplyMirror(sink IssueSink) *ReplyMirror {
	if sink == nil {
		panic("escalate: NewReplyMirror requires a non-nil sink (use NewMemorySink for a no-op)")
	}
	return &ReplyMirror{sink: sink}
}

// MirrorReply resolves the reply's thread_ts back to the tracked issue carrying
// that thread marker and posts the reply as a comment on it. When no open issue
// matches the thread (a stale thread, a reply in some unrelated thread, or an
// issue closed since the root was posted), it is a best-effort no-op returning nil
// — an out-of-band reply must never crash the listener. A transport/write failure
// is returned so the listener can log-and-continue.
//
// The thread→issue resolution reuses [ParseThreadMarker] on the issues listed via
// the sink, the exact same durable mapping the outbound side persists, so inbound
// and outbound agree on which conversation is which even across process restarts.
func (m *ReplyMirror) MirrorReply(ctx context.Context, reply notify.InboundReply) error {
	threadTS := strings.TrimSpace(reply.ThreadTS)
	if threadTS == "" {
		// Not a threaded reply — nothing to map to an incident.
		return nil
	}

	tracked, err := m.sink.ListOpen(ctx)
	if err != nil {
		return fmt.Errorf("escalate: listing open issues to mirror inbound reply: %w", err)
	}

	ref, ok := matchThread(tracked, threadTS)
	if !ok {
		// No open issue owns this thread; record nothing rather than guessing.
		return nil
	}

	if err := m.sink.Comment(ctx, ref, InboundReplyComment(reply)); err != nil {
		return fmt.Errorf("escalate: mirroring inbound reply onto issue %q: %w", ref, err)
	}
	return nil
}

// matchThread finds the tracked issue whose recovered thread_ts equals threadTS.
// It returns the first match; the trail self-heals toward one issue per thread, so
// a duplicate is vanishingly unlikely, and matching the first keeps behavior
// deterministic.
func matchThread(tracked []TrackedIssue, threadTS string) (IssueRef, bool) {
	for i := range tracked {
		if strings.TrimSpace(tracked[i].ThreadTS) == threadTS {
			return tracked[i].Ref, true
		}
	}
	return "", false
}

// InboundReplyComment renders the GitHub comment body for a mirrored inbound Slack
// reply. It attributes the reply to its Slack author and clearly marks it as
// arriving via Slack, and it reiterates the safety boundary so a reader knows the
// reply itself triggered no action — MaKlaude only recorded it. The reply text is
// included verbatim so the conversation reads faithfully.
func InboundReplyComment(reply notify.InboundReply) string {
	author := strings.TrimSpace(reply.User)
	if author == "" {
		author = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Reply from Slack** (user `%s`):\n\n", author)
	b.WriteString(strings.TrimSpace(reply.Text))
	b.WriteString("\n\n---\n*Mirrored from the Slack escalation thread by MaKlaude. This reply was recorded for the audit trail only; it triggers no cluster action.*")
	return b.String()
}

// Ensure the escalate mirror satisfies the notify seam at compile time.
var _ notify.ReplyMirror = (*ReplyMirror)(nil)
