// Package notify is the narrow, side-effecting boundary between MaKlaude's
// escalation core and a real-time chat backend (Slack for Milestone 2). It plays
// the same role for ChatOps that [github.com/Sayfan-AI/MaKlaude/internal/escalate]
// (the [escalate.IssueSink] / [escalate.MemorySink] pair) plays for the GitHub
// comms trail: a small interface with a safe, side-effect-free no-op default, so
// the whole system degrades cleanly to "GitHub + email only" whenever the chat
// backend is unconfigured.
//
// Safety boundary (locked): this package is comms-only. Nothing here touches a
// cluster, and no implementation may ever gain a cluster-mutating capability —
// it notifies, it does not remediate. Operator-supplied secrets (Slack tokens,
// signing secret) are read from the environment at runtime, are NEVER committed,
// and are NEVER logged: see [SlackConfig] and its redaction helpers.
//
// Milestone roadmap. T1 (this change) introduces only the seam: the [Notifier]
// interface, the [NopNotifier] no-op default, the [SlackConfig] +
// [SlackConfigFromEnv] surface, and the [NotifierFromEnv] selection point. It
// deliberately does NOT post to Slack — even when Slack is configured,
// [NotifierFromEnv] currently returns the no-op (it reports live=true so callers
// can see the seam works), and the live Slack backend lands in T2.
package notify

import (
	"context"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// Notifier is the side-effecting boundary to a real-time chat backend. It models
// the escalation lifecycle as a conversation: a problem is first announced (the
// thread root), then updated as it recurs or changes, then marked resolved when
// it clears — mirroring how the [escalate.Escalator] opens, updates, and closes a
// GitHub issue for the same problem.
//
// Every method is keyed by a stable [detect.Identity] (the same identity the
// escalator uses to dedup an active issue across cycles). That key is what lets a
// real backend map a problem to a single chat thread.
//
// # Durable thread continuity (T3)
//
// Continuity is owned by the caller, not this package. [NotifyEscalation] RETURNS
// the thread handle (Slack's "thread_ts") of the root it posts; the escalator
// persists that handle in the backing GitHub issue (a hidden marker in the body,
// the approved default — no new datastore) and, on a later recurrence or
// clearance, recovers it and passes it back in via the threadTS argument of
// [Notifier.NotifyUpdate] / [Notifier.NotifyResolution]. The reply therefore lands
// in the ORIGINAL thread even after the monitor process has restarted and any
// in-memory mapping is gone. This is why continuity lives in the escalate/scan
// layer (which can see both the issue store and the notifier) rather than here:
// notify must not import escalate, so it cannot persist anything itself.
//
// When the caller has no thread handle to supply (an empty threadTS — for example
// an issue opened before the marker existed, or a backend that lost its own
// in-memory state), an implementation MUST degrade gracefully: post a self-labelled
// top-level message rather than erroring. A notification is never dropped and a
// reconcile is never failed by the chat layer.
//
// Keeping this interface small and behind a seam — exactly as [escalate.IssueSink]
// is — is what lets the escalation core be exercised with no network: the
// [NopNotifier] substitutes for a real backend in tests and whenever chat is
// unconfigured.
//
// Implementations should be safe to call from a single reconciliation goroutine;
// like [escalate.IssueSink] they are not required to be concurrency-safe across
// simultaneous reconciles of the same problem, since a monitor reconciles one
// cluster's findings at a time. Every method takes a context so a real backend's
// network calls can be cancelled or time-bounded.
type Notifier interface {
	// NotifyEscalation announces a newly-escalated problem as the root of a new
	// chat thread. It is called when the escalator first opens the comms trail for
	// an identity. A real backend posts a top-level message and returns the
	// resulting thread handle (Slack's "thread_ts") so the caller can persist it
	// durably and supply it back on later updates/resolutions. summary is the
	// human-facing one-line description of the problem (the same text that titles
	// the GitHub issue); ref is the backing issue reference (or empty if none) so
	// the chat message can link back to the auditable trail.
	//
	// The returned threadTS may be empty (for example a no-op backend, or a backend
	// whose post did not surface a handle); callers must treat empty as "no thread
	// to persist" and not as an error signal — the error return is the only failure
	// signal.
	NotifyEscalation(ctx context.Context, id detect.Identity, summary, ref string) (threadTS string, err error)

	// NotifyUpdate posts a follow-up into the existing thread for an identity, used
	// when an active problem recurs or its details change. threadTS is the thread
	// handle the caller recovered for this problem (from the backing issue); a real
	// backend replies into that thread. When threadTS is empty — no thread is known
	// (an issue opened before continuity existed, or a backend that lost state) —
	// the implementation MUST degrade gracefully: post a self-labelled top-level
	// message rather than error. note is the human-facing update text.
	NotifyUpdate(ctx context.Context, id detect.Identity, threadTS, note string) error

	// NotifyResolution posts a closing message into the existing thread for an
	// identity, used when the problem clears and the escalator closes the trail. It
	// is the conversational counterpart to closing the GitHub issue. threadTS is the
	// recovered thread handle; as with NotifyUpdate an empty threadTS degrades
	// gracefully to a top-level message rather than an error. note is the
	// human-facing resolution text.
	NotifyResolution(ctx context.Context, id detect.Identity, threadTS, note string) error
}

// NopNotifier is a [Notifier] that does nothing and always succeeds. It is the
// safe default whenever a chat backend is unconfigured, and the stand-in used by
// tests — exactly the role [escalate.MemorySink] plays for [escalate.IssueSink].
//
// Returning a NopNotifier (rather than nil) from [NotifierFromEnv] means callers
// never have to nil-check before notifying: every method is always valid to call
// and simply discards its input, so an unconfigured deployment behaves EXACTLY as
// Milestone 1 (GitHub + email only) with zero extra behavior.
//
// It carries no state, so the zero value is ready to use and it is trivially safe
// for concurrent use.
type NopNotifier struct{}

// NewNopNotifier returns a ready-to-use no-op notifier. It exists for symmetry
// with the other constructors in this package; the zero [NopNotifier] is equally
// valid.
func NewNopNotifier() NopNotifier { return NopNotifier{} }

// NotifyEscalation does nothing and returns an empty thread handle and a nil
// error. The empty handle means the caller persists no thread marker, exactly as
// if Slack were unconfigured — which is the whole point of the no-op.
func (NopNotifier) NotifyEscalation(context.Context, detect.Identity, string, string) (string, error) {
	return "", nil
}

// NotifyUpdate does nothing and returns nil.
func (NopNotifier) NotifyUpdate(context.Context, detect.Identity, string, string) error { return nil }

// NotifyResolution does nothing and returns nil.
func (NopNotifier) NotifyResolution(context.Context, detect.Identity, string, string) error {
	return nil
}

// Ensure the no-op satisfies the interface at compile time.
var _ Notifier = NopNotifier{}
