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
//   - When Slack IS configured, it reports live=true so callers can see the seam
//     and config parsing work end to end.
//
// IMPORTANT (T1 scope): the live Slack backend does not exist yet — it lands in
// T2. So even when live=true today, the returned [Notifier] is still the no-op
// placeholder: the config is parsed and validated and the live signal is real,
// but nothing is posted to Slack. T2 replaces the body of the configured branch
// with the real backend (and may surface a wiring error), without changing this
// function's signature or its unconfigured behavior.
//
// Returning the no-op (rather than nil) means callers never have to nil-check
// before notifying: notifier.NotifyEscalation(...) is always valid, and when not
// configured it simply discards.
func NotifierFromEnv() (notifier Notifier, live bool) {
	cfg := SlackConfigFromEnv(os.Getenv)
	if cfg.Configured() {
		// T2: construct and return the live Slack backend here. Until then the
		// seam is real (live=true) but the notifier stays a safe no-op.
		return NewNopNotifier(), true
	}
	return NewNopNotifier(), false
}
