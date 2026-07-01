package notify

import (
	"os"
	"strings"
)

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
	return NotifierFromEnvWithIssueBaseURL("")
}

// NotifierFromEnvWithIssueBaseURL is [NotifierFromEnv] plus the WEB base URL of the
// backing issue tracker's issues path (e.g. "https://github.com/OWNER/REPO/issues"),
// so a configured Slack deployment renders the backing issue as a CLICKABLE Slack
// hyperlink an operator can click straight through (issue #58).
//
// It exists because `notify` must NOT import `escalate` (import cycle), so the
// owner/repo that determines this URL lives in the GitHub config, not the Slack
// environment. The wiring layer that sees BOTH configs
// ([github.com/Sayfan-AI/MaKlaude/internal/escalate.EscalatorFromEnv]) derives the
// URL from the GitHub config and threads it in here, keeping `notify` free of
// `escalate`.
//
// issueBaseURL is a non-secret; it is only stored on the config and never logged as
// a credential. When empty (the URL is unknown, or Slack is unconfigured) the
// escalation text degrades to the previous plain "#NNN" form and every other path
// is unchanged versus [NotifierFromEnv], so an unconfigured deployment still
// behaves exactly as it did before.
func NotifierFromEnvWithIssueBaseURL(issueBaseURL string) (notifier Notifier, live bool) {
	cfg := SlackConfigFromEnv(os.Getenv)
	cfg.IssueBaseURL = strings.TrimSpace(issueBaseURL)
	if sn, ok := NewSlackNotifier(cfg, nil); ok {
		return sn, true
	}
	return NewNopNotifier(), false
}
