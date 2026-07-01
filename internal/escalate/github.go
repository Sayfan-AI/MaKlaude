package escalate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubConfig configures a [GitHubSink]. Everything is injected — nothing is
// hardcoded — so the same binary can target any repo and degrade to a no-op when
// unconfigured. The zero value is intentionally "not configured": [NewGitHubSink]
// returns ok=false for it, letting unit tests and the e2e harness run without
// real credentials.
//
// The values typically come from environment, loaded via [GitHubConfigFromEnv]:
//
//	MAKLAUDE_GITHUB_REPO   owner/repo of the issue tracker to use as the trail
//	MAKLAUDE_GITHUB_TOKEN  a token with issues:write on that repo
//	MAKLAUDE_GITHUB_API    optional API base override (for GitHub Enterprise)
type GitHubConfig struct {
	// Owner is the repository owner (user or org).
	Owner string

	// Repo is the repository name. Issues are opened here as the comms trail.
	Repo string

	// Token authenticates the API calls. It needs permission to read, create,
	// comment on, label, and close issues in the repo. It is never logged.
	Token string

	// APIBase is the REST API base URL, defaulting to https://api.github.com.
	// Override it for GitHub Enterprise. No trailing slash.
	APIBase string

	// HTTPClient is the client used for requests; nil uses a sensible default
	// with a timeout. Injectable so tests can use an httptest server.
	HTTPClient *http.Client
}

// Configured reports whether the config carries the minimum needed to talk to
// GitHub (owner, repo, token). When false, callers should fall back to a no-op
// so the system degrades gracefully without credentials.
func (c GitHubConfig) Configured() bool {
	return strings.TrimSpace(c.Owner) != "" &&
		strings.TrimSpace(c.Repo) != "" &&
		strings.TrimSpace(c.Token) != ""
}

// GitHubConfigFromEnv reads a [GitHubConfig] from the environment using the
// MAKLAUDE_GITHUB_* variables (see [GitHubConfig]). It never errors: a missing
// variable simply leaves its field empty, and the caller decides what to do via
// [GitHubConfig.Configured]. The lookup function is injected so tests can supply
// a map without mutating the process environment.
func GitHubConfigFromEnv(getenv func(string) string) GitHubConfig {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	repo := strings.TrimSpace(getenv("MAKLAUDE_GITHUB_REPO"))
	owner, name := splitRepo(repo)
	return GitHubConfig{
		Owner:   owner,
		Repo:    name,
		Token:   strings.TrimSpace(getenv("MAKLAUDE_GITHUB_TOKEN")),
		APIBase: strings.TrimSpace(getenv("MAKLAUDE_GITHUB_API")),
	}
}

// IssueBaseURL derives the WEB base URL of this repo's issues path — the URL a
// human opens in a browser, e.g. "https://github.com/OWNER/REPO/issues" — so the
// comms layer can render a backing issue as a clickable link that opens the tracked
// issue (issue #58). It returns empty when owner or repo is unknown, so callers
// degrade gracefully to a non-linked reference.
//
// The web host is NOT the REST API host: on github.com the API lives at
// api.github.com while issues live at github.com, so the default web host is
// "https://github.com". For GitHub Enterprise, where APIBase is set (typically
// "https://HOST/api/v3"), the web host is the scheme+host of that API base
// ("https://HOST"), which is where GHE serves both the API and the web UI. This
// keeps the derivation self-hosting-aware without a second config knob.
func (c GitHubConfig) IssueBaseURL() string {
	owner := strings.TrimSpace(c.Owner)
	repo := strings.TrimSpace(c.Repo)
	if owner == "" || repo == "" {
		return ""
	}
	host := "https://github.com"
	if api := strings.TrimSpace(c.APIBase); api != "" {
		if u, err := url.Parse(api); err == nil && u.Scheme != "" && u.Host != "" {
			host = u.Scheme + "://" + u.Host
		}
	}
	return host + "/" + owner + "/" + repo + "/issues"
}

// splitRepo parses an "owner/repo" string into its parts, tolerating leading or
// trailing whitespace and returning empties for a malformed value.
func splitRepo(s string) (owner, repo string) {
	parts := strings.SplitN(strings.TrimSpace(s), "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

// GitHubSink is the production [IssueSink], backed directly by the GitHub REST
// API over net/http. It deliberately uses no third-party GitHub client: the
// surface it needs (list/create/comment/close issues, filtered to the
// [ManagedLabel]) is small, so a few typed REST calls keep MaKlaude's runtime
// dependency footprint minimal and avoid assuming the `gh` CLI is installed
// wherever the monitor runs.
//
// Email notifications are NOT this type's concern: for M1 MaKlaude relies on
// GitHub's own per-issue notification emails (watchers, assignees, and label
// subscribers are emailed by GitHub when an issue is opened, commented on, or
// closed). There is intentionally no SMTP layer here.
type GitHubSink struct {
	cfg    GitHubConfig
	client *http.Client
	base   string
}

// NewGitHubSink builds a GitHub-backed sink from cfg. ok is false when cfg is
// not [GitHubConfig.Configured]; in that case the returned sink is nil and the
// caller should use a no-op (for example a [MemorySink]) instead. This is the
// graceful-degradation seam: no credentials means no GitHub calls, never a
// crash.
func NewGitHubSink(cfg GitHubConfig) (*GitHubSink, bool) {
	if !cfg.Configured() {
		return nil, false
	}
	base := cfg.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	base = strings.TrimRight(base, "/")

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &GitHubSink{cfg: cfg, client: client, base: base}, true
}

// ListOpen lists open issues carrying the [ManagedLabel] and parses each one's
// embedded identity marker. Issues without a parseable marker are skipped — they
// are not MaKlaude's to manage even if they happen to carry the label.
func (g *GitHubSink) ListOpen(ctx context.Context) ([]TrackedIssue, error) {
	var tracked []TrackedIssue
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("labels", ManagedLabel)
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))
		path := fmt.Sprintf("/repos/%s/%s/issues?%s", g.cfg.Owner, g.cfg.Repo, q.Encode())

		var issues []ghIssue
		if err := g.do(ctx, http.MethodGet, path, nil, &issues); err != nil {
			return nil, err
		}
		for i := range issues {
			// The issues endpoint also returns pull requests; skip those.
			if issues[i].PullRequest != nil {
				continue
			}
			id, ok := ParseIdentityMarker(issues[i].Body)
			if !ok {
				continue
			}
			// Recover the durable Slack thread handle when present so updates and
			// the resolution reply into the original thread across restarts.
			threadTS, _ := ParseThreadMarker(issues[i].Body)
			tracked = append(tracked, TrackedIssue{
				Identity: id,
				Ref:      IssueRef(fmt.Sprintf("%d", issues[i].Number)),
				ThreadTS: threadTS,
			})
		}
		if len(issues) < 100 {
			break
		}
		page++
	}
	return tracked, nil
}

// Create opens a new issue and returns its number as the reference.
func (g *GitHubSink) Create(ctx context.Context, title, body string, labels []string) (IssueRef, error) {
	payload := map[string]any{"title": title, "body": body, "labels": labels}
	var created ghIssue
	if err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues", g.cfg.Owner, g.cfg.Repo),
		payload, &created); err != nil {
		return "", err
	}
	return IssueRef(fmt.Sprintf("%d", created.Number)), nil
}

// Update PATCHes an existing issue's title, body, and labels.
func (g *GitHubSink) Update(ctx context.Context, ref IssueRef, title, body string, labels []string) error {
	payload := map[string]any{"title": title, "body": body, "labels": labels}
	return g.do(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%s", g.cfg.Owner, g.cfg.Repo, string(ref)),
		payload, nil)
}

// Comment posts a comment on an existing issue.
func (g *GitHubSink) Comment(ctx context.Context, ref IssueRef, body string) error {
	payload := map[string]any{"body": body}
	return g.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%s/comments", g.cfg.Owner, g.cfg.Repo, string(ref)),
		payload, nil)
}

// Close PATCHes an existing issue to the closed state.
func (g *GitHubSink) Close(ctx context.Context, ref IssueRef) error {
	payload := map[string]any{"state": "closed"}
	return g.do(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%s", g.cfg.Owner, g.cfg.Repo, string(ref)),
		payload, nil)
}

// do performs one REST call, encoding body as JSON (when non-nil) and decoding
// the response into out (when non-nil). It sets the auth and content headers and
// turns any non-2xx response into an error carrying the status and a short
// excerpt of the body for debugging.
func (g *GitHubSink) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("escalate/github: marshalling request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reqBody)
	if err != nil {
		return fmt.Errorf("escalate/github: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("escalate/github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("escalate/github: %s %s: unexpected status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("escalate/github: decoding response: %w", err)
		}
	}
	return nil
}

// ghIssue is the slice of the GitHub issue JSON the sink consumes. PullRequest
// is present only on pull requests, letting us filter them out of the issues
// endpoint's results.
type ghIssue struct {
	Number      int    `json:"number"`
	Body        string `json:"body"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

// Ensure the production sink satisfies the interface at compile time.
var _ IssueSink = (*GitHubSink)(nil)
