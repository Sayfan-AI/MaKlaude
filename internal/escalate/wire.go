package escalate

import (
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// SinkFromEnv selects the comms sink for the running process based on the
// MAKLAUDE_GITHUB_* environment (see [GitHubConfig]). It is the single seam the
// monitor / e2e harness uses to obtain an [Escalator] without caring how comms
// are backed:
//
//   - When GitHub is configured, it returns a live [GitHubSink] and live=true.
//   - When it is NOT configured, it returns an in-memory [MemorySink] and
//     live=false, so the whole system degrades to a safe, side-effect-free
//     dry-run. Tests and credential-less e2e runs rely on exactly this.
//
// Returning the no-op sink (rather than nil) means callers never have to
// nil-check before escalating: NewEscalator(sink).Reconcile(...) is always
// valid, and when not configured it simply records to memory and discards.
func SinkFromEnv() (sink IssueSink, live bool) {
	cfg := GitHubConfigFromEnv(os.Getenv)
	if gh, ok := NewGitHubSink(cfg); ok {
		return gh, true
	}
	return NewMemorySink(), false
}

// EscalatorFromEnv is a convenience over [SinkFromEnv] that returns a ready
// [Escalator] plus whether it is backed by a live comms system. The monitor can
// call this once at startup and reuse the escalator across reconcile cycles.
//
// It also selects the chat [notify.Notifier] from the environment (via
// [notify.NotifierFromEnv]) and wires it into the escalator, so a configured Slack
// deployment gets durable, threaded chat mirroring of the escalation lifecycle.
// This is the layer that owns chat thread continuity: it can see both the issue
// store (to persist/recover the thread handle) and the notifier, which is why the
// wiring lives here rather than in notify (notify must not import escalate).
//
// The returned live reflects the GITHUB trail (the auditable source of truth), not
// chat: an unconfigured Slack backend simply yields a [notify.NopNotifier] and the
// system degrades to exactly the Milestone 1 GitHub + email behavior.
//
// This is also the layer that gives the chat notifier the WEB URL of the issue
// tracker so a Slack escalation can render its backing issue as a CLICKABLE link
// (issue #58): it derives the URL from the GitHub config (via
// [GitHubConfig.IssueBaseURL]) — the only config that knows owner/repo — and threads
// it in through [notify.NotifierFromEnvWithIssueBaseURL]. `notify` therefore never
// needs to import `escalate` to learn where the issues live. When GitHub is
// unconfigured the derived URL is empty and the notifier degrades to the previous
// plain "#NNN" text, so nothing changes for a credential-less deployment.
func EscalatorFromEnv() (esc *Escalator, live bool) {
	sink, live := SinkFromEnv()
	issueBaseURL := GitHubConfigFromEnv(os.Getenv).IssueBaseURL()
	notifier, _ := notify.NotifierFromEnvWithIssueBaseURL(issueBaseURL)
	return NewEscalatorWithNotifier(sink, notifier), live
}

// InboundListenerFromEnv selects the INBOUND Slack listener for the running
// process from the MAKLAUDE_SLACK_* environment (see [notify.SlackConfig]). It is
// the inbound counterpart of [EscalatorFromEnv], and the layer that wires the two
// halves of the Slack integration together: it resolves the same sink the
// escalator writes to, builds a [ReplyMirror] over it so a captured reply lands on
// the right issue, and hands both to [notify.NewInboundListener].
//
//   - When Slack is configured it returns the listener and ok=true (the caller
//     starts it — Socket Mode by default, or wires the HTTP handler).
//   - When Slack is NOT configured it returns nil and ok=false, so the caller
//     starts nothing: no connection, no errors, exactly the Milestone 1 behavior.
//
// Construction reuses [notify.NewInboundListener]'s own graceful-degradation seam,
// so an unconfigured environment can never produce a live inbound listener. The
// sink is shared with the escalator's view of the trail (both come from
// [SinkFromEnv]), so inbound replies are mirrored onto the very issues the outbound
// side opened. Optional Socket Mode dialer / error handler options are passed
// through to the listener.
func InboundListenerFromEnv(opts ...notify.InboundOption) (listener *notify.InboundListener, ok bool) {
	sink, _ := SinkFromEnv()
	cfg := notify.SlackConfigFromEnv(os.Getenv)
	return notify.NewInboundListener(cfg, NewReplyMirror(sink), opts...)
}
