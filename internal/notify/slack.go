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
