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
func EscalatorFromEnv() (esc *Escalator, live bool) {
	sink, live := SinkFromEnv()
	notifier, _ := notify.NotifierFromEnv()
	return NewEscalatorWithNotifier(sink, notifier), live
}
