package aidiagnose

import (
	"testing"
	"time"
)

// envMap turns a map into the getenv signature ConfigFromEnv expects, so tests
// never touch the real process environment.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfig_ActiveRequiresFlagAndKey(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		key     string
		want    bool
	}{
		{"off by default", false, "", false},
		{"flag only, no key", true, "", false},
		{"key only, no flag", false, "sk-ant-x", false},
		{"flag and key", true, "sk-ant-x", true},
		{"flag and whitespace key", true, "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Enabled: tc.enabled, APIKey: tc.key}
			if got := c.Active(); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigFromEnv_ParsesAndGates(t *testing.T) {
	c := ConfigFromEnv(envMap(map[string]string{
		"MAKLAUDE_LLM_DIAGNOSIS":    "true",
		"MAKLAUDE_LLM_API_KEY":      "sk-ant-secret",
		"MAKLAUDE_LLM_MODEL":        "claude-custom",
		"MAKLAUDE_LLM_API_BASE":     "https://proxy.example/api",
		"MAKLAUDE_LLM_MAX_EVIDENCE": "1234",
		"MAKLAUDE_LLM_MAX_TOKENS":   "222",
		"MAKLAUDE_LLM_CALL_BUDGET":  "3",
		"MAKLAUDE_LLM_TIMEOUT":      "7s",
	}))
	if !c.Active() {
		t.Fatal("expected active config")
	}
	if c.model() != "claude-custom" {
		t.Errorf("model = %q", c.model())
	}
	if c.APIBase != "https://proxy.example/api" {
		t.Errorf("APIBase = %q", c.APIBase)
	}
	if c.maxEvidenceBytes() != 1234 || c.maxResponseTokens() != 222 || c.callBudget() != 3 {
		t.Errorf("caps = %d/%d/%d", c.maxEvidenceBytes(), c.maxResponseTokens(), c.callBudget())
	}
	if c.timeout() != 7*time.Second {
		t.Errorf("timeout = %v", c.timeout())
	}
}

func TestConfigFromEnv_APIKeyFallbackAndDefaults(t *testing.T) {
	c := ConfigFromEnv(envMap(map[string]string{
		"MAKLAUDE_LLM_DIAGNOSIS": "1",
		"ANTHROPIC_API_KEY":      "sk-ant-fallback",
	}))
	if c.APIKey != "sk-ant-fallback" {
		t.Errorf("APIKey = %q, want fallback from ANTHROPIC_API_KEY", c.APIKey)
	}
	if c.model() != DefaultModel {
		t.Errorf("model = %q, want default %q", c.model(), DefaultModel)
	}
	if c.maxEvidenceBytes() != DefaultMaxEvidenceBytes || c.callBudget() != DefaultCallBudget {
		t.Errorf("expected defaults, got evidence=%d budget=%d", c.maxEvidenceBytes(), c.callBudget())
	}
	if c.timeout() != DefaultTimeout {
		t.Errorf("timeout = %v, want default", c.timeout())
	}
}

func TestConfigFromEnv_FlagFalseIsInert(t *testing.T) {
	c := ConfigFromEnv(envMap(map[string]string{
		"MAKLAUDE_LLM_API_KEY": "sk-ant-secret",
		// no MAKLAUDE_LLM_DIAGNOSIS
	}))
	if c.Active() {
		t.Error("config with a key but no flag must be inert")
	}
}

func TestConfigFromEnv_MalformedNumbersFallBackToDefaults(t *testing.T) {
	c := ConfigFromEnv(envMap(map[string]string{
		"MAKLAUDE_LLM_DIAGNOSIS":   "yes",
		"MAKLAUDE_LLM_API_KEY":     "k",
		"MAKLAUDE_LLM_MAX_TOKENS":  "not-a-number",
		"MAKLAUDE_LLM_CALL_BUDGET": "-5",
		"MAKLAUDE_LLM_TIMEOUT":     "garbage",
	}))
	if c.maxResponseTokens() != DefaultMaxResponseTokens {
		t.Errorf("malformed tokens should fall back, got %d", c.maxResponseTokens())
	}
	if c.callBudget() != DefaultCallBudget {
		t.Errorf("negative budget should fall back, got %d", c.callBudget())
	}
	if c.timeout() != DefaultTimeout {
		t.Errorf("malformed timeout should fall back, got %v", c.timeout())
	}
}

func TestConfigFromEnv_NilGetenv(t *testing.T) {
	c := ConfigFromEnv(nil)
	if c.Active() {
		t.Error("nil getenv must yield an inert config")
	}
}

func TestRefinerFromEnv_OffByDefault(t *testing.T) {
	// RefinerFromEnv reads the real environment; in the test env the flag is unset,
	// so it must report the layer as off with a nil builder.
	build, live := RefinerFromEnv()
	if live || build != nil {
		t.Errorf("RefinerFromEnv without config: live=%v build!=nil=%v, want off", live, build != nil)
	}
}
