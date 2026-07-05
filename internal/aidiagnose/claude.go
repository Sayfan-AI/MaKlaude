package aidiagnose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicVersion is the required anthropic-version header value for the Claude
// Messages API. It pins the request/response schema this provider is written
// against.
const anthropicVersion = "2023-06-01"

// defaultAPIBase is the Claude Messages API base URL. It is overridable via
// [Config.APIBase] for a proxy or compatible gateway, but is never a secret and
// is the only network endpoint this package contacts.
const defaultAPIBase = "https://api.anthropic.com"

// ClaudeProvider is the default, production [Provider]: a thin, read-only HTTP
// client over the Claude Messages API. It has NO Kubernetes capability — it only
// turns a redacted [Request] into text [Suggestion]s — so it cannot widen the T5
// boundary. It carries the API key (a runtime credential, never logged) and sends
// exactly the redacted evidence the refiner hands it, honouring the request's
// deadline via the caller's context.
type ClaudeProvider struct {
	apiKey     string
	model      string
	apiBase    string
	httpClient *http.Client
}

// NewClaudeProvider builds a [ClaudeProvider] from cfg. It applies the default
// model, API base, and HTTP client when those are unset. The returned provider is
// only ever reached when [Config.Active] is true, so a missing key here would be a
// wiring bug, not a normal state.
func NewClaudeProvider(cfg Config) *ClaudeProvider {
	client := cfg.HTTPClient
	if client == nil {
		// A generous ceiling; the refiner's per-call context deadline is the real
		// bound, this just prevents a truly stuck socket from leaking forever.
		client = &http.Client{Timeout: 60 * time.Second}
	}
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.APIBase), "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	return &ClaudeProvider{
		apiKey:     strings.TrimSpace(cfg.APIKey),
		model:      cfg.model(),
		apiBase:    apiBase,
		httpClient: client,
	}
}

// claudeRequest is the Messages API request body. Sampling parameters
// (temperature/top_p/top_k) are intentionally omitted: they are unsupported on
// current Claude models and behaviour is steered by the system prompt instead.
type claudeRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []claudeMessage `json:"messages"`
}

// claudeMessage is one turn in the Messages API request.
type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudeResponse is the subset of the Messages API response this provider reads:
// the content blocks (from which the text is extracted).
type claudeResponse struct {
	Content []claudeContentBlock `json:"content"`
}

// claudeContentBlock is one block of the model's reply; only text blocks carry
// output this provider uses.
type claudeContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// suggestionsEnvelope is the strict-JSON output contract stated in the system
// prompt: the model returns an object with a "suggestions" array.
type suggestionsEnvelope struct {
	Suggestions []Suggestion `json:"suggestions"`
}

// Suggest implements [Provider]. It POSTs the redacted request to the Claude
// Messages API, extracts the assistant's text, and parses it into [Suggestion]s.
// It returns an error on any transport, status, or decode failure — the refiner
// treats every error as a cue to degrade to the deterministic hypotheses, so
// failing loudly here is safe. It sends ONLY the request's already-redacted
// evidence; it neither adds cluster data nor logs the payload.
func (p *ClaudeProvider) Suggest(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(claudeRequest{
		Model:     p.model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  []claudeMessage{{Role: "user", Content: req.Evidence}},
	})
	if err != nil {
		return Response{}, fmt.Errorf("aidiagnose: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("aidiagnose: building request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("x-api-key", p.apiKey)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("aidiagnose: calling provider: %w", err)
	}
	defer resp.Body.Close()

	// Bound the response read so a pathological body cannot exhaust memory even if
	// the token cap is somehow not honoured upstream.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("aidiagnose: reading response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The status line is safe to surface; the body may echo request content, so
		// it is deliberately NOT included in the error.
		return Response{}, fmt.Errorf("aidiagnose: provider returned %s", resp.Status)
	}

	var decoded claudeResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, fmt.Errorf("aidiagnose: decoding response: %w", err)
	}

	text := extractText(decoded)
	if strings.TrimSpace(text) == "" {
		// No text output means no refinement; an empty response is valid.
		return Response{}, nil
	}

	env, err := parseSuggestions(text)
	if err != nil {
		return Response{}, err
	}
	return Response(env), nil
}

// extractText concatenates the text of the reply's text blocks, ignoring any
// non-text block, so the provider reads the model's prose regardless of how it is
// chunked.
func extractText(r claudeResponse) string {
	var b strings.Builder
	for _, block := range r.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// parseSuggestions extracts the strict-JSON envelope from the model's text. Models
// occasionally wrap JSON in prose or a code fence, so it isolates the outermost
// object (first '{' to last '}') before decoding, and returns an error on
// genuinely unparseable output — which the refiner degrades on.
func parseSuggestions(text string) (suggestionsEnvelope, error) {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end < start {
		return suggestionsEnvelope{}, fmt.Errorf("aidiagnose: no JSON object in provider reply")
	}
	var env suggestionsEnvelope
	if err := json.Unmarshal([]byte(text[start:end+1]), &env); err != nil {
		return suggestionsEnvelope{}, fmt.Errorf("aidiagnose: parsing suggestions: %w", err)
	}
	return env, nil
}
