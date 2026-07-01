package notify

import "strings"

// SlackConfig configures the (T2) Slack [Notifier] backend. Everything is
// injected — nothing is hardcoded — so the same binary can target any workspace
// and degrade to a no-op when unconfigured. The zero value is intentionally "not
// configured": [SlackConfig.Configured] returns false for it, letting unit tests
// and credential-less deployments run without real tokens.
//
// The values typically come from environment, loaded via [SlackConfigFromEnv]:
//
//	MAKLAUDE_SLACK_BOT_TOKEN        bot token (xoxb-…) for outbound Web API calls
//	MAKLAUDE_SLACK_APP_TOKEN        app-level token (xapp-…) for Socket Mode inbound
//	MAKLAUDE_SLACK_SIGNING_SECRET   signing secret used to verify inbound requests
//	MAKLAUDE_SLACK_CHANNEL          target channel (ID or #name) for escalations
//	MAKLAUDE_SLACK_OPERATOR         operator to @-mention on needs:human escalations
//
// Socket Mode is the approved default inbound transport: the app-level token
// opens an outbound WebSocket so MaKlaude needs no public HTTP endpoint, while
// outbound posts use the bot token against the Web API. The signing secret is
// only needed when an operator chooses to run an HTTP Events API endpoint instead
// of Socket Mode, so it is NOT part of the minimum [SlackConfig.Configured] set.
//
// All three token/secret fields are credentials: they are operator-supplied at
// runtime, never committed, and never logged — see [SlackConfig.String] and
// [SlackConfig.Redacted], which exist precisely so a config can be printed for
// diagnostics without leaking secret material.
type SlackConfig struct {
	// BotToken authenticates outbound Web API calls (posting messages, replying in
	// threads). It is the "xoxb-" token from the app's OAuth install. Secret: never
	// logged.
	BotToken string

	// AppToken is the app-level "xapp-" token that opens the Socket Mode
	// connection for inbound events. Secret: never logged.
	AppToken string

	// SigningSecret verifies the authenticity of inbound HTTP requests when (and
	// only when) an operator runs the Events API over HTTP instead of Socket Mode.
	// Secret: never logged.
	SigningSecret string

	// Channel is the target channel for escalation threads, given either as a
	// channel ID (e.g. "C0123456789") or a "#name". It is not a secret.
	Channel string

	// Operator, when set, is the Slack handle MaKlaude @-mentions on an escalation
	// that warrants human attention (the needs:human gate), so the operator gets a
	// real notification — and, crucially, a mobile push — rather than a silent
	// channel post. It is given as a Slack user ID ("U0123456789", rendered as
	// "<@U0123456789>"), a user group ID ("S0123456789", rendered as
	// "<!subteam^S0123456789>"), or a literal mention token already in Slack's
	// "<…>" form, which is passed through verbatim. It is NOT a secret and is
	// optional: when empty, escalations post without an @-mention (no behavior
	// change). See [SlackConfig.mentionPrefix].
	Operator string

	// IssueBaseURL is the WEB base URL of the backing issue tracker's issues path
	// (e.g. "https://github.com/OWNER/REPO/issues"), used to render the backing
	// GitHub issue as a CLICKABLE Slack hyperlink in escalation posts so an operator
	// can click straight through to the tracked issue (issue #58). It is NOT a
	// secret.
	//
	// It is deliberately NOT read from the Slack environment: `notify` must not
	// import `escalate`, so the owner/repo lives in the GitHub config. The wiring
	// layer that sees both configs ([github.com/Sayfan-AI/MaKlaude/internal/escalate.EscalatorFromEnv])
	// derives this from the GitHub config and threads it in via
	// [NotifierFromEnvWithIssueBaseURL], keeping `notify` free of `escalate`.
	//
	// When empty (unknown / unconfigured, as in every unit test that does not set
	// it), escalation text degrades gracefully to the previous plain "#NNN" form,
	// so behavior is unchanged when it is not supplied.
	IssueBaseURL string
}

// Configured reports whether the config carries the minimum needed to talk to
// Slack in the approved default mode: a bot token (outbound) and a target
// channel, plus the app-level token (Socket Mode inbound). When false, callers
// should fall back to the [NopNotifier] so the system degrades gracefully to
// GitHub + email without credentials. The signing secret is intentionally NOT
// required, since it is only used for the optional HTTP Events API mode.
func (c SlackConfig) Configured() bool {
	return strings.TrimSpace(c.BotToken) != "" &&
		strings.TrimSpace(c.AppToken) != "" &&
		strings.TrimSpace(c.Channel) != ""
}

// SlackConfigFromEnv reads a [SlackConfig] from the environment using the
// MAKLAUDE_SLACK_* variables (see [SlackConfig]). It never errors: a missing
// variable simply leaves its field empty, and the caller decides what to do via
// [SlackConfig.Configured]. The lookup function is injected so tests can supply a
// map without mutating the process environment — mirroring
// [github.com/Sayfan-AI/MaKlaude/internal/escalate.GitHubConfigFromEnv].
func SlackConfigFromEnv(getenv func(string) string) SlackConfig {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return SlackConfig{
		BotToken:      strings.TrimSpace(getenv("MAKLAUDE_SLACK_BOT_TOKEN")),
		AppToken:      strings.TrimSpace(getenv("MAKLAUDE_SLACK_APP_TOKEN")),
		SigningSecret: strings.TrimSpace(getenv("MAKLAUDE_SLACK_SIGNING_SECRET")),
		Channel:       strings.TrimSpace(getenv("MAKLAUDE_SLACK_CHANNEL")),
		Operator:      strings.TrimSpace(getenv("MAKLAUDE_SLACK_OPERATOR")),
	}
}

// mentionPrefix renders the configured operator as a Slack mention token suitable
// for prefixing an escalation message so the operator is notified (and a mobile
// push fires). It returns an empty string when no operator is configured, so the
// caller simply omits the mention. The token is recognized in three forms:
//
//   - an explicit "<…>" mention is passed through verbatim (operator knows best);
//   - a "U…" value becomes a user mention "<@U…>";
//   - an "S…" value becomes a user-group mention "<!subteam^S…>";
//   - anything else (e.g. a "@name" or bare name) is wrapped as "<@value>" on a
//     best-effort basis — Slack resolves a valid user ID and otherwise renders it
//     harmlessly as text, so this never errors or leaks.
//
// The operator handle is not a secret, so it is rendered verbatim.
func (c SlackConfig) mentionPrefix() string {
	op := strings.TrimSpace(c.Operator)
	if op == "" {
		return ""
	}
	if strings.HasPrefix(op, "<") && strings.HasSuffix(op, ">") {
		return op
	}
	switch op[0] {
	case 'S':
		return "<!subteam^" + op + ">"
	case 'U', 'W':
		return "<@" + op + ">"
	default:
		return "<@" + strings.TrimPrefix(op, "@") + ">"
	}
}

// redactedSecret is the fixed placeholder substituted for any present secret
// value in diagnostic output. It reveals only that a secret is set, never its
// content or length.
const redactedSecret = "[REDACTED]"

// redact maps a secret value to a leak-proof token for display: empty stays
// empty (so "unset" is distinguishable from "set"), and any non-empty value
// collapses to the fixed [redactedSecret] placeholder regardless of its content
// or length.
func redact(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	return redactedSecret
}

// Redacted returns a copy of the config with every secret field replaced by a
// fixed placeholder (the non-secret Channel is preserved). Use it whenever a
// config must cross a logging or reporting boundary, so secret material can never
// leak. An empty secret stays empty so callers can still tell "set" from "unset".
func (c SlackConfig) Redacted() SlackConfig {
	return SlackConfig{
		BotToken:      redact(c.BotToken),
		AppToken:      redact(c.AppToken),
		SigningSecret: redact(c.SigningSecret),
		Channel:       c.Channel,
		Operator:      c.Operator,
		IssueBaseURL:  c.IssueBaseURL,
	}
}

// String renders the config for humans WITHOUT exposing any secret. It is the
// fmt.Stringer implementation, so a SlackConfig is safe to pass to %v/%s in logs:
// the bot token, app token, and signing secret are shown only as "set"/"unset",
// never as their values. Only the non-secret channel and the overall
// configured-ness are shown verbatim.
func (c SlackConfig) String() string {
	set := func(s string) string {
		if strings.TrimSpace(s) == "" {
			return "unset"
		}
		return "set"
	}
	var b strings.Builder
	b.WriteString("SlackConfig{")
	b.WriteString("BotToken:" + set(c.BotToken))
	b.WriteString(" AppToken:" + set(c.AppToken))
	b.WriteString(" SigningSecret:" + set(c.SigningSecret))
	b.WriteString(" Channel:" + quoteChannel(c.Channel))
	b.WriteString(" Operator:" + quoteChannel(c.Operator))
	b.WriteString(" IssueBaseURL:" + quoteChannel(c.IssueBaseURL))
	b.WriteString(" Configured:")
	if c.Configured() {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	b.WriteString("}")
	return b.String()
}

// quoteChannel renders the (non-secret) channel for display, showing "unset" for
// an empty value and the bare value otherwise.
func quoteChannel(ch string) string {
	if strings.TrimSpace(ch) == "" {
		return "unset"
	}
	return ch
}
