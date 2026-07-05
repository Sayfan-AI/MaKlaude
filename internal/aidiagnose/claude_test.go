package aidiagnose

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClaudeProvider_SendsRedactedRequestAndParses drives the provider against an
// httptest server, asserting the request shape/headers and that a well-formed
// reply parses into suggestions. No real network or key is involved.
func TestClaudeProvider_SendsRedactedRequestAndParses(t *testing.T) {
	var gotBody claudeRequest
	var gotHeaders http.Header
	var gotPath, gotMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		gotMethod = r.Method
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"{\"suggestions\":[{\"cause\":\"probe\",\"title\":\"Bad probe\",\"message\":\"m\",\"confidence\":\"medium\"}]}"}]}`)
	}))
	defer srv.Close()

	cfg := Config{Enabled: true, APIKey: "sk-ant-test", Model: "claude-test", APIBase: srv.URL, HTTPClient: srv.Client()}
	p := NewClaudeProvider(cfg)

	resp, err := p.Suggest(context.Background(), Request{System: "sys", Evidence: "redacted evidence", MaxTokens: 321})
	if err != nil {
		t.Fatalf("Suggest error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/v1/messages" {
		t.Errorf("request = %s %s, want POST /v1/messages", gotMethod, gotPath)
	}
	if gotHeaders.Get("x-api-key") != "sk-ant-test" {
		t.Errorf("x-api-key = %q", gotHeaders.Get("x-api-key"))
	}
	if gotHeaders.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", gotHeaders.Get("anthropic-version"), anthropicVersion)
	}
	if gotBody.Model != "claude-test" || gotBody.MaxTokens != 321 || gotBody.System != "sys" {
		t.Errorf("body = %+v", gotBody)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "redacted evidence" {
		t.Errorf("messages = %+v", gotBody.Messages)
	}
	if len(resp.Suggestions) != 1 || resp.Suggestions[0].Cause != "probe" || resp.Suggestions[0].Confidence != "medium" {
		t.Errorf("suggestions = %+v", resp.Suggestions)
	}
}

func TestClaudeProvider_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "rate limited")
	}))
	defer srv.Close()

	p := NewClaudeProvider(Config{APIKey: "k", APIBase: srv.URL, HTTPClient: srv.Client()})
	if _, err := p.Suggest(context.Background(), Request{Evidence: "e", MaxTokens: 10}); err == nil {
		t.Fatal("expected an error on non-200 status")
	}
}

func TestClaudeProvider_ParsesJSONWrappedInProse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"Here is my analysis:\n{\"suggestions\":[{\"cause\":\"x\",\"title\":\"t\"}]}\nHope that helps."}]}`)
	}))
	defer srv.Close()

	p := NewClaudeProvider(Config{APIKey: "k", APIBase: srv.URL, HTTPClient: srv.Client()})
	resp, err := p.Suggest(context.Background(), Request{Evidence: "e", MaxTokens: 10})
	if err != nil {
		t.Fatalf("Suggest error: %v", err)
	}
	if len(resp.Suggestions) != 1 || resp.Suggestions[0].Cause != "x" {
		t.Errorf("suggestions = %+v", resp.Suggestions)
	}
}

func TestClaudeProvider_EmptyTextIsEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"content":[]}`)
	}))
	defer srv.Close()

	p := NewClaudeProvider(Config{APIKey: "k", APIBase: srv.URL, HTTPClient: srv.Client()})
	resp, err := p.Suggest(context.Background(), Request{Evidence: "e", MaxTokens: 10})
	if err != nil {
		t.Fatalf("Suggest error: %v", err)
	}
	if len(resp.Suggestions) != 0 {
		t.Errorf("suggestions = %+v, want none", resp.Suggestions)
	}
}

func TestClaudeProvider_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, `{"content":[]}`)
	}))
	defer srv.Close()

	p := NewClaudeProvider(Config{APIKey: "k", APIBase: srv.URL, HTTPClient: srv.Client()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := p.Suggest(ctx, Request{Evidence: "e", MaxTokens: 10}); err == nil {
		t.Fatal("expected an error when the context deadline is exceeded")
	}
}

func TestNewClaudeProvider_Defaults(t *testing.T) {
	p := NewClaudeProvider(Config{APIKey: "k"})
	if p.apiBase != defaultAPIBase {
		t.Errorf("apiBase = %q, want default", p.apiBase)
	}
	if p.model != DefaultModel {
		t.Errorf("model = %q, want default", p.model)
	}
	if p.httpClient == nil {
		t.Error("expected a default http client")
	}
	// A configured base with a trailing slash is normalized.
	p2 := NewClaudeProvider(Config{APIKey: "k", APIBase: "https://x.example/"})
	if strings.HasSuffix(p2.apiBase, "/") {
		t.Errorf("apiBase not trimmed: %q", p2.apiBase)
	}
}
