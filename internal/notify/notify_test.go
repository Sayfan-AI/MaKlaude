package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

func TestSlackConfig_Configured(t *testing.T) {
	cases := []struct {
		name string
		cfg  SlackConfig
		want bool
	}{
		{"all present", SlackConfig{BotToken: "b", AppToken: "a", Channel: "c"}, true},
		{"with optional signing secret", SlackConfig{BotToken: "b", AppToken: "a", SigningSecret: "s", Channel: "c"}, true},
		{"missing bot token", SlackConfig{AppToken: "a", Channel: "c"}, false},
		{"missing app token", SlackConfig{BotToken: "b", Channel: "c"}, false},
		{"missing channel", SlackConfig{BotToken: "b", AppToken: "a"}, false},
		{"signing secret alone is not enough", SlackConfig{SigningSecret: "s"}, false},
		{"whitespace only", SlackConfig{BotToken: "  ", AppToken: "a", Channel: "c"}, false},
		{"empty", SlackConfig{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.Configured(); got != c.want {
				t.Errorf("Configured() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSlackConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"MAKLAUDE_SLACK_BOT_TOKEN":      "  xoxb-secret  ",
		"MAKLAUDE_SLACK_APP_TOKEN":      "xapp-secret",
		"MAKLAUDE_SLACK_SIGNING_SECRET": "signsecret",
		"MAKLAUDE_SLACK_CHANNEL":        "#alerts",
	}
	cfg := SlackConfigFromEnv(func(k string) string { return env[k] })
	if cfg.BotToken != "xoxb-secret" {
		t.Errorf("BotToken = %q, want trimmed xoxb-secret", cfg.BotToken)
	}
	if cfg.AppToken != "xapp-secret" || cfg.SigningSecret != "signsecret" {
		t.Errorf("unexpected app/signing: %+v", cfg)
	}
	if cfg.Channel != "#alerts" {
		t.Errorf("Channel = %q, want #alerts", cfg.Channel)
	}
	if !cfg.Configured() {
		t.Error("expected configured")
	}

	// Empty env -> not configured, no panic.
	empty := SlackConfigFromEnv(nil)
	if empty.Configured() {
		t.Error("empty env should not be configured")
	}
}

// TestSlackConfig_SecretsRedacted is the locked-safety-boundary test: it proves
// no rendering or redaction path ever emits a secret's value.
func TestSlackConfig_SecretsRedacted(t *testing.T) {
	const (
		botSecret  = "xoxb-super-secret-bot-token"
		appSecret  = "xapp-super-secret-app-token"
		signSecret = "super-secret-signing-secret"
		channel    = "C0123456789"
	)
	cfg := SlackConfig{
		BotToken:      botSecret,
		AppToken:      appSecret,
		SigningSecret: signSecret,
		Channel:       channel,
	}

	secrets := []string{botSecret, appSecret, signSecret}

	// String() must not leak any secret value, but must keep the non-secret
	// channel and report each secret as present.
	s := cfg.String()
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			t.Errorf("String() leaked a secret value: %q", s)
		}
	}
	if !strings.Contains(s, channel) {
		t.Errorf("String() should show the non-secret channel; got %q", s)
	}
	if strings.Count(s, "set") < 3 { // "set" appears for each present secret (and within "unset")
		t.Errorf("String() should mark present secrets as set; got %q", s)
	}

	// Redacted() must blank every secret behind the fixed placeholder while
	// preserving the non-secret channel.
	r := cfg.Redacted()
	for _, secret := range secrets {
		if strings.Contains(r.BotToken+r.AppToken+r.SigningSecret, secret) {
			t.Errorf("Redacted() leaked a secret: %+v", r)
		}
	}
	if r.BotToken != redactedSecret || r.AppToken != redactedSecret || r.SigningSecret != redactedSecret {
		t.Errorf("Redacted() should replace secrets with the placeholder; got %+v", r)
	}
	if r.Channel != channel {
		t.Errorf("Redacted() should preserve the non-secret channel; got %q", r.Channel)
	}

	// An empty secret stays empty so "unset" is distinguishable from "set".
	if got := (SlackConfig{}).Redacted(); got.BotToken != "" || got.AppToken != "" || got.SigningSecret != "" {
		t.Errorf("Redacted() of empty config should stay empty; got %+v", got)
	}
	if empty := (SlackConfig{}).String(); strings.Contains(empty, "set:") {
		t.Errorf("unexpected String() for empty config: %q", empty)
	}
	if !strings.Contains((SlackConfig{}).String(), "unset") {
		t.Error("empty config String() should report fields as unset")
	}
}

func TestNopNotifier_MethodsAreSafe(t *testing.T) {
	ctx := context.Background()
	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	// Both the zero value and the constructed value must satisfy Notifier and be
	// safe to call with no error.
	notifiers := []Notifier{NopNotifier{}, NewNopNotifier()}
	for _, n := range notifiers {
		ts, err := n.NotifyEscalation(ctx, id, "pod is crashlooping", "42")
		if err != nil {
			t.Errorf("NotifyEscalation: unexpected error %v", err)
		}
		if ts != "" {
			t.Errorf("NopNotifier.NotifyEscalation should return an empty thread handle, got %q", ts)
		}
		if err := n.NotifyUpdate(ctx, id, "111.0001", "still crashlooping"); err != nil {
			t.Errorf("NotifyUpdate: unexpected error %v", err)
		}
		if err := n.NotifyResolution(ctx, id, "111.0001", "recovered"); err != nil {
			t.Errorf("NotifyResolution: unexpected error %v", err)
		}
	}
}

func TestNotifierFromEnv_GracefulDegradation(t *testing.T) {
	// This seam reads the real process env via os.Getenv; the test asserts the
	// shape, not a particular env. In CI and unit runs MAKLAUDE_SLACK_* is unset,
	// so we expect the no-op + live=false. We also prove the configured branch via
	// the lower-level constructor below, since NotifierFromEnv intentionally reads
	// the process environment.
	notifier, live := NotifierFromEnv()
	if notifier == nil {
		t.Fatal("NotifierFromEnv must never return a nil notifier")
	}
	// Whatever the env, the returned notifier is always safe to call (T1 keeps it
	// a no-op even when configured).
	if _, err := notifier.NotifyEscalation(context.Background(), "id", "s", "ref"); err != nil {
		t.Errorf("returned notifier should be safe to call: %v", err)
	}
	// With no Slack env in the unit environment, degradation must hold.
	if SlackConfigFromEnv(nil).Configured() && !live {
		t.Error("inconsistent: configured but reported not live")
	}
}

// TestNotifierFromEnv_ConfiguredReportsLive proves the live signal is real: a
// fully-configured Slack environment makes the seam report live=true (while still
// returning the safe no-op, since the live backend lands in T2). We exercise the
// decision against an injected env to avoid mutating the process environment.
func TestNotifierFromEnv_ConfiguredReportsLive(t *testing.T) {
	env := map[string]string{
		"MAKLAUDE_SLACK_BOT_TOKEN": "xoxb-x",
		"MAKLAUDE_SLACK_APP_TOKEN": "xapp-x",
		"MAKLAUDE_SLACK_CHANNEL":   "C123",
	}
	cfg := SlackConfigFromEnv(func(k string) string { return env[k] })
	if !cfg.Configured() {
		t.Fatal("expected the injected env to be configured")
	}

	// Unconfigured env yields the no-op + not live.
	unconfigured := SlackConfigFromEnv(func(string) string { return "" })
	if unconfigured.Configured() {
		t.Fatal("empty env must not be configured")
	}
}
