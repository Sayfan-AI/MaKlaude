package escalate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

func TestGitHubConfig_Configured(t *testing.T) {
	cases := []struct {
		name string
		cfg  GitHubConfig
		want bool
	}{
		{"all present", GitHubConfig{Owner: "o", Repo: "r", Token: "t"}, true},
		{"missing token", GitHubConfig{Owner: "o", Repo: "r"}, false},
		{"missing repo", GitHubConfig{Owner: "o", Token: "t"}, false},
		{"missing owner", GitHubConfig{Repo: "r", Token: "t"}, false},
		{"whitespace only", GitHubConfig{Owner: "  ", Repo: "r", Token: "t"}, false},
		{"empty", GitHubConfig{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.Configured(); got != c.want {
				t.Errorf("Configured() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGitHubConfigFromEnv(t *testing.T) {
	env := map[string]string{
		"MAKLAUDE_GITHUB_REPO":  "acme/clusters",
		"MAKLAUDE_GITHUB_TOKEN": "secret",
		"MAKLAUDE_GITHUB_API":   "https://ghe.example.com/api/v3",
	}
	cfg := GitHubConfigFromEnv(func(k string) string { return env[k] })
	if cfg.Owner != "acme" || cfg.Repo != "clusters" {
		t.Errorf("repo parse = %q/%q, want acme/clusters", cfg.Owner, cfg.Repo)
	}
	if cfg.Token != "secret" || cfg.APIBase != "https://ghe.example.com/api/v3" {
		t.Errorf("unexpected token/api: %+v", cfg)
	}
	if !cfg.Configured() {
		t.Error("expected configured")
	}

	// Empty env -> not configured, no panic.
	empty := GitHubConfigFromEnv(nil)
	if empty.Configured() {
		t.Error("empty env should not be configured")
	}
}

func TestNewGitHubSink_GracefulDegradation(t *testing.T) {
	if _, ok := NewGitHubSink(GitHubConfig{}); ok {
		t.Error("unconfigured GitHubConfig must not yield a sink")
	}
	if sink, ok := NewGitHubSink(GitHubConfig{Owner: "o", Repo: "r", Token: "t"}); !ok || sink == nil {
		t.Error("configured GitHubConfig must yield a sink")
	}
}

// TestGitHubSink_AgainstFakeAPI drives the real GitHubSink against an httptest
// server that mimics the GitHub REST issues API, exercising the full
// list/create/update/comment/close surface without real network or credentials.
func TestGitHubSink_AgainstFakeAPI(t *testing.T) {
	ctx := context.Background()
	var (
		createdBodies []string
		patched       []map[string]any
		comments      []string
		closed        bool
	)

	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/clusters/issues":
			if r.URL.Query().Get("labels") != ManagedLabel {
				t.Errorf("list did not filter by managed label: %s", r.URL.RawQuery)
			}
			// Return one managed issue (with marker) and one unmanaged (no marker)
			// plus a PR, to prove filtering.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]ghIssue{
				{Number: 42, Body: "tracked\n" + identityMarker(id)},
				{Number: 7, Body: "human-authored, no marker"},
				{Number: 9, Body: "x" + identityMarker("ignored"), PullRequest: &struct {
					URL string `json:"url"`
				}{URL: "http://pr"}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/clusters/issues":
			b, _ := io.ReadAll(r.Body)
			createdBodies = append(createdBodies, string(b))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(ghIssue{Number: 100})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/clusters/issues/42":
			var m map[string]any
			_ = json.NewDecoder(r.Body).Decode(&m)
			patched = append(patched, m)
			if _, ok := m["state"]; ok {
				closed = true
			}
			_ = json.NewEncoder(w).Encode(ghIssue{Number: 42})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/clusters/issues/42/comments":
			var m map[string]string
			_ = json.NewDecoder(r.Body).Decode(&m)
			comments = append(comments, m["body"])
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sink, ok := NewGitHubSink(GitHubConfig{
		Owner: "acme", Repo: "clusters", Token: "tok",
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	if !ok {
		t.Fatal("expected a configured sink")
	}

	// ListOpen: only the marked, non-PR issue should come back.
	tracked, err := sink.ListOpen(ctx)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(tracked) != 1 || tracked[0].Identity != id || tracked[0].Ref != "42" {
		t.Fatalf("ListOpen = %+v, want exactly one (#42, %q)", tracked, id)
	}

	// Create.
	ref, err := sink.Create(ctx, "title", "body\n"+identityMarker(id), []string{ManagedLabel})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref != "100" {
		t.Errorf("Create ref = %q, want 100", ref)
	}
	if len(createdBodies) != 1 || !strings.Contains(createdBodies[0], "title") {
		t.Errorf("create payload not sent: %v", createdBodies)
	}

	// Update + Comment + Close on #42.
	if err := sink.Update(ctx, "42", "t", "b", []string{ManagedLabel}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := sink.Comment(ctx, "42", "recurred"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := sink.Close(ctx, "42"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(comments) != 1 || comments[0] != "recurred" {
		t.Errorf("comments = %v", comments)
	}
	if !closed {
		t.Error("expected issue #42 to be closed")
	}
}

// TestGitHubSink_ErrorOnNon2xx proves a failing API response surfaces as an
// error rather than being silently swallowed.
func TestGitHubSink_ErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()

	sink, _ := NewGitHubSink(GitHubConfig{
		Owner: "acme", Repo: "clusters", Token: "tok",
		APIBase: srv.URL, HTTPClient: srv.Client(),
	})
	_, err := sink.ListOpen(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected a 403 error, got %v", err)
	}
}
