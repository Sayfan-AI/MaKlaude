package aidiagnose

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultModel is the Claude model the [ClaudeProvider] targets when none is
// configured. It is a current, general-purpose Claude model chosen for a good
// capability/cost balance on a bounded refinement task; a deployment can override
// it via MAKLAUDE_LLM_MODEL (see [Config]) without code changes. It is the only
// model id in the package and lives here, in config, so it is never hardcoded at a
// call site.
const DefaultModel = "claude-sonnet-5"

// Cost-bound defaults. They are deliberately conservative: this layer's whole
// justification is that data leaves the process, so the out-of-the-box posture
// caps egress volume, response size, per-call latency, and the number of calls a
// single scan can make. Every one is overridable via the MAKLAUDE_LLM_*
// environment.
const (
	// DefaultMaxEvidenceBytes caps the size of the redacted evidence sent per call,
	// bounding egress volume (and, loosely, input token cost). Evidence longer than
	// this is truncated at a rune boundary before egress.
	DefaultMaxEvidenceBytes = 8000

	// DefaultMaxResponseTokens caps the model's response size, bounding output cost
	// per call.
	DefaultMaxResponseTokens = 1024

	// DefaultCallBudget caps how many provider calls a single scan cycle may make
	// across all incidents and clusters, so a cluster with many incidents cannot
	// fan out into an unbounded (and unbounded-cost) burst of egress.
	DefaultCallBudget = 8

	// DefaultTimeout bounds a single provider call. A slower response is abandoned
	// and the incident degrades to its deterministic hypotheses.
	DefaultTimeout = 20 * time.Second
)

// Config configures the optional LLM-assisted diagnosis layer. It is OFF BY
// DEFAULT and requires an explicit opt-in: [Config.Active] is true only when both
// Enabled is set AND an API key is present, so neither an accidentally-set key nor
// the flag alone can turn cluster data egress on. Everything is injected from the
// environment via [ConfigFromEnv]; nothing is hardcoded, and the API key is a
// runtime credential that is never logged and never committed.
//
// The values come from the MAKLAUDE_LLM_* environment:
//
//	MAKLAUDE_LLM_DIAGNOSIS       the feature flag; must be truthy to enable the layer
//	MAKLAUDE_LLM_API_KEY         the Claude API key (falls back to ANTHROPIC_API_KEY)
//	MAKLAUDE_LLM_MODEL           optional model id override (default DefaultModel)
//	MAKLAUDE_LLM_API_BASE        optional API base override (default the Claude API)
//	MAKLAUDE_LLM_MAX_EVIDENCE    optional redacted-evidence byte cap
//	MAKLAUDE_LLM_MAX_TOKENS      optional response token cap
//	MAKLAUDE_LLM_CALL_BUDGET     optional per-cycle call budget
//	MAKLAUDE_LLM_TIMEOUT         optional per-call timeout (Go duration, e.g. "20s")
type Config struct {
	// Enabled is the explicit feature flag. When false the whole layer is inert no
	// matter what else is set, so the deterministic core runs exactly as it does
	// with T5 absent.
	Enabled bool

	// APIKey authenticates the provider. It is a runtime credential: operator-
	// supplied, never logged, never committed. Its presence (with Enabled) is what
	// gates egress.
	APIKey string

	// Model is the model id to target; empty means [DefaultModel].
	Model string

	// APIBase optionally overrides the provider API base URL (for a proxy or a
	// compatible gateway). Empty uses the provider's own default.
	APIBase string

	// MaxEvidenceBytes caps redacted evidence size; non-positive means
	// [DefaultMaxEvidenceBytes].
	MaxEvidenceBytes int

	// MaxResponseTokens caps response size; non-positive means
	// [DefaultMaxResponseTokens].
	MaxResponseTokens int

	// CallBudget caps provider calls per scan cycle; non-positive means
	// [DefaultCallBudget].
	CallBudget int

	// Timeout bounds a single provider call; non-positive means [DefaultTimeout].
	Timeout time.Duration

	// HTTPClient is the client the [ClaudeProvider] uses for requests; nil uses a
	// sensible default. It is injectable so tests can point the provider at an
	// httptest server instead of the real API. It is not read from the environment.
	HTTPClient *http.Client
}

// Active reports whether the layer should actually run: it requires the explicit
// feature flag AND a credential. This double gate is intentional — it means a
// stray API key in the environment does nothing on its own, and the flag alone
// (with no key) cannot cause a failing, credential-less egress attempt. When
// Active is false, callers use the deterministic core with no refinement.
func (c Config) Active() bool {
	return c.Enabled && strings.TrimSpace(c.APIKey) != ""
}

// model returns the effective model id, applying [DefaultModel] when unset.
func (c Config) model() string {
	if m := strings.TrimSpace(c.Model); m != "" {
		return m
	}
	return DefaultModel
}

// maxEvidenceBytes returns the effective evidence cap, applying the default when
// unset.
func (c Config) maxEvidenceBytes() int {
	if c.MaxEvidenceBytes > 0 {
		return c.MaxEvidenceBytes
	}
	return DefaultMaxEvidenceBytes
}

// maxResponseTokens returns the effective response-token cap, applying the
// default when unset.
func (c Config) maxResponseTokens() int {
	if c.MaxResponseTokens > 0 {
		return c.MaxResponseTokens
	}
	return DefaultMaxResponseTokens
}

// callBudget returns the effective per-cycle call budget, applying the default
// when unset.
func (c Config) callBudget() int {
	if c.CallBudget > 0 {
		return c.CallBudget
	}
	return DefaultCallBudget
}

// timeout returns the effective per-call timeout, applying the default when unset.
func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// ConfigFromEnv reads a [Config] from the MAKLAUDE_LLM_* environment (see
// [Config]). It never errors: a missing or malformed numeric/duration variable
// simply leaves the corresponding field at its zero value, and the caller decides
// via [Config.Active]. The lookup function is injected so tests can supply a map
// without mutating the process environment, mirroring
// [github.com/Sayfan-AI/MaKlaude/internal/escalate.GitHubConfigFromEnv].
func ConfigFromEnv(getenv func(string) string) Config {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	apiKey := strings.TrimSpace(getenv("MAKLAUDE_LLM_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(getenv("ANTHROPIC_API_KEY"))
	}
	cfg := Config{
		Enabled:           truthy(getenv("MAKLAUDE_LLM_DIAGNOSIS")),
		APIKey:            apiKey,
		Model:             strings.TrimSpace(getenv("MAKLAUDE_LLM_MODEL")),
		APIBase:           strings.TrimSpace(getenv("MAKLAUDE_LLM_API_BASE")),
		MaxEvidenceBytes:  atoiOrZero(getenv("MAKLAUDE_LLM_MAX_EVIDENCE")),
		MaxResponseTokens: atoiOrZero(getenv("MAKLAUDE_LLM_MAX_TOKENS")),
		CallBudget:        atoiOrZero(getenv("MAKLAUDE_LLM_CALL_BUDGET")),
	}
	if d, err := time.ParseDuration(strings.TrimSpace(getenv("MAKLAUDE_LLM_TIMEOUT"))); err == nil {
		cfg.Timeout = d
	}
	return cfg
}

// truthy interprets a feature-flag string liberally: the common affirmatives are
// enabled, everything else (including empty) is off. Keeping the flag forgiving
// avoids an operator being surprised that "true" worked but "1" did not.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "y", "yes", "on", "enable", "enabled":
		return true
	default:
		return false
	}
}

// atoiOrZero parses a non-negative integer, returning 0 (which every consumer
// reads as "use the default") on any malformed or negative input, so a typo in a
// cost knob degrades safely to the conservative default rather than erroring.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
