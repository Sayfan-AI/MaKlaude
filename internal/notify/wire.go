package notify

import "os"

// NotifierFromEnv selects the chat notifier for the running process based on the
// MAKLAUDE_SLACK_* environment (see [SlackConfig]). It is the single seam the
// monitor uses to obtain a [Notifier] without caring how chat is backed — the
// direct analogue of
// [github.com/Sayfan-AI/MaKlaude/internal/escalate.SinkFromEnv]:
//
//   - When Slack is NOT configured, it returns a [NopNotifier] and live=false,
//     so the whole system degrades to "GitHub + email only" with zero behavior
//     change versus Milestone 1.
//   - When Slack IS configured, it returns the live [SlackNotifier] and
//     live=true, so escalations and milestone events post to Slack as threads.
//
// As of T2 the live Slack backend exists: a configured environment yields a real
// [SlackNotifier] posting over the Slack Web API. The unconfigured path is
// unchanged from Milestone 1 — a [NopNotifier] and live=false — so a
// credential-less deployment still degrades cleanly to "GitHub + email only".
// Construction goes through [NewSlackNotifier]'s own graceful-degradation seam:
// in the unlikely event a config passes [SlackConfig.Configured] but the notifier
// cannot be built, the no-op + live=false fallback keeps the caller safe rather
// than handing back a nil notifier.
//
// Returning the no-op (rather than nil) means callers never have to nil-check
// before notifying: notifier.NotifyEscalation(...) is always valid, and when not
// configured it simply discards.
func NotifierFromEnv() (notifier Notifier, live bool) {
	cfg := SlackConfigFromEnv(os.Getenv)
	if sn, ok := NewSlackNotifier(cfg, nil); ok {
		return sn, true
	}
	return NewNopNotifier(), false
}
