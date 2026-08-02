package disclose

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

	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
)

// GitHubSink is the production [Sink], backed directly by the GitHub REST API.
//
// It reuses [escalate.GitHubConfig] — the repo, token and API base are one deployment's
// single GitHub identity, and a second set of environment variables for the same
// credentials would be a way for two trails to end up pointed at different repositories.
// What it does NOT reuse is [escalate.GitHubSink]: that type's list call filters on
// [escalate.ManagedLabel] and parses incident markers, and the whole reason this trail
// has its own label is that the three trails must be disjoint at the query. Sharing the
// transport config while keeping the queries separate is the split that gets both.
//
// It uses no third-party GitHub client for the same reason the other two sinks do not:
// the surface needed here is six typed REST calls.
type GitHubSink struct {
	cfg    escalate.GitHubConfig
	client *http.Client
	base   string
}

// NewGitHubSink builds a GitHub-backed sink from cfg. ok is false when cfg is not
// [escalate.GitHubConfig.Configured]; the caller then falls back to a [MemorySink],
// which is the rehearsal posture — the whole unattended path runs and the disclosure
// reaches nobody, which the state summary reports as not live.
func NewGitHubSink(cfg escalate.GitHubConfig) (*GitHubSink, bool) {
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

// ghIssue is the slice of GitHub's issue representation this sink reads.
type ghIssue struct {
	Number      int    `json:"number"`
	Body        string `json:"body"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListOpen lists open issues carrying [ManagedLabel] and parses each one's markers.
// Issues without a parseable proposal marker are skipped — they are not this trail's to
// manage even if they happen to carry the label.
func (g *GitHubSink) ListOpen(ctx context.Context) ([]Disclosed, error) {
	var out []Disclosed
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
			id, ok := ParseProposalMarker(issues[i].Body)
			if !ok {
				continue
			}
			shape, _ := ParseShapeMarker(issues[i].Body)
			out = append(out, Disclosed{
				Ref:      Ref(fmt.Sprintf("%d", issues[i].Number)),
				Identity: id,
				Shape:    shape,
				Revoked:  hasLabel(issues[i], RevokedLabel),
				Applied:  hasLabel(issues[i], AppliedLabel),
				Body:     issues[i].Body,
			})
		}
		if len(issues) < 100 {
			break
		}
		page++
	}
	return out, nil
}

// Create opens a new issue and returns its number as the reference.
func (g *GitHubSink) Create(ctx context.Context, title, body string, labels []string) (Ref, error) {
	payload := map[string]any{"title": title, "body": body, "labels": labels}
	var created ghIssue
	if err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues", g.cfg.Owner, g.cfg.Repo), payload, &created); err != nil {
		return "", err
	}
	return Ref(fmt.Sprintf("%d", created.Number)), nil
}

// SetBody PATCHes only the body. It sends no labels field, so GitHub leaves the label
// set untouched — which is what keeps a person's [RevokedLabel] from being erased by
// MaKlaude recording an outcome. See the [Sink] doc.
func (g *GitHubSink) SetBody(ctx context.Context, ref Ref, body string) error {
	return g.do(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%s", g.cfg.Owner, g.cfg.Repo, string(ref)),
		map[string]any{"body": body}, nil)
}

// Comment posts a comment on an existing issue.
func (g *GitHubSink) Comment(ctx context.Context, ref Ref, body string) error {
	return g.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%s/comments", g.cfg.Owner, g.cfg.Repo, string(ref)),
		map[string]any{"body": body}, nil)
}

// AddLabel adds one label through the labels sub-resource, which appends rather than
// replaces.
func (g *GitHubSink) AddLabel(ctx context.Context, ref Ref, label string) error {
	return g.do(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/issues/%s/labels", g.cfg.Owner, g.cfg.Repo, string(ref)),
		map[string]any{"labels": []string{label}}, nil)
}

// Close PATCHes an existing issue to the closed state.
func (g *GitHubSink) Close(ctx context.Context, ref Ref) error {
	return g.do(ctx, http.MethodPatch,
		fmt.Sprintf("/repos/%s/%s/issues/%s", g.cfg.Owner, g.cfg.Repo, string(ref)),
		map[string]any{"state": "closed"}, nil)
}

// do performs one REST call, encoding body as JSON (when non-nil) and decoding the
// response into out (when non-nil). Any non-2xx becomes an error carrying the status and
// a short excerpt of the response body.
func (g *GitHubSink) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("disclose/github: encoding request: %w", err)
		}
		reqBody = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reqBody)
	if err != nil {
		return fmt.Errorf("disclose/github: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("disclose/github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("disclose/github: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(excerpt)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("disclose/github: decoding response: %w", err)
	}
	return nil
}

// hasLabel reports whether an issue carries a label.
func hasLabel(issue ghIssue, want string) bool {
	for _, l := range issue.Labels {
		if l.Name == want {
			return true
		}
	}
	return false
}

var _ Sink = (*GitHubSink)(nil)
