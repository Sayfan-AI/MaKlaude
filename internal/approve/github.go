package approve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
)

// GitHubSink is the production [ApprovalSink]: the approval trail backed by GitHub
// issues, where the decision signal is a label event.
//
// # Why it shares the escalation trail's config but not its interface
//
// It is constructed from an [escalate.GitHubConfig] because both trails are
// literally the same repository, reached with the same token — duplicating the
// owner/repo/token/API-base plumbing would create two ways to point MaKlaude's comms
// at two different places, which is a configuration bug waiting to happen rather
// than an isolation property.
//
// The isolation that matters is at the QUERY, and it is unaffected: this sink lists
// only issues carrying [ManagedLabel] ("maklaude-proposal") and parses only bodies
// carrying the proposal marker, while [escalate.GitHubSink] lists only
// [escalate.ManagedLabel] ("maklaude") and parses only identity markers. Neither can
// see, let alone act on, the other's issues. See [ApprovalSink] for why the two are
// separate interfaces.
//
// # Attribution comes from the label EVENT, not the label
//
// The issue payload says WHICH labels are present; it does not say who applied one
// or when. Those are the two facts this entire gate rests on, so for any artifact
// carrying a decision label the sink additionally reads the issue's events endpoint
// and takes the most recent `labeled` event for that label. An artifact whose
// approval has no recoverable event yields an empty approver, which [Decide] refuses
// ([ReasonUnattributedApproval]) rather than honors anonymously — an unreadable
// attribution must fail closed.
type GitHubSink struct {
	cfg       escalate.GitHubConfig
	selfLogin string
	client    *http.Client
	base      string
}

// SelfLoginEnv is the environment variable naming the account MaKlaude itself acts
// as, used to recognize a self-applied decision label.
const SelfLoginEnv = "MAKLAUDE_GITHUB_SELF_LOGIN"

// SelfLoginFromEnv reads [SelfLoginEnv]. An empty result is safe rather than
// permissive: see [GitHubSink.isSelfActor] for the bot-account check that holds
// regardless of whether this is configured.
func SelfLoginFromEnv(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	return strings.TrimSpace(getenv(SelfLoginEnv))
}

// NewGitHubSink builds a GitHub-backed approval sink. ok is false when cfg is not
// [escalate.GitHubConfig.Configured]; the caller should then fall back to a
// [MemorySink], which keeps a credential-less deployment side-effect-free instead of
// crashing — the same graceful-degradation seam the escalation trail uses.
//
// selfLogin names the account MaKlaude runs as, so a decision label MaKlaude applied
// to its own artifact is recognized and refused. It may be empty; the bot-account
// check in [GitHubSink.isSelfActor] still applies.
func NewGitHubSink(cfg escalate.GitHubConfig, selfLogin string) (*GitHubSink, bool) {
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
	return &GitHubSink{
		cfg:       cfg,
		selfLogin: strings.TrimSpace(selfLogin),
		client:    client,
		base:      base,
	}, true
}

// ListOpen returns every open, MaKlaude-managed approval artifact, with its identity
// and preview recovered from the body markers and its decision recovered from the
// labels plus their events.
//
// Issues without a parseable proposal marker are skipped even when they carry the
// label: the marker is the authoritative key, and an issue a human hand-labelled is
// not this gate's to manage.
func (g *GitHubSink) ListOpen(ctx context.Context) ([]PendingAction, error) {
	var tracked []PendingAction
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("labels", ManagedLabel)
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))

		var issues []ghIssue
		if err := g.do(ctx, http.MethodGet, g.issuesPath()+"?"+q.Encode(), nil, &issues); err != nil {
			return nil, err
		}
		for i := range issues {
			// The issues endpoint also returns pull requests; skip those.
			if issues[i].PullRequest != nil {
				continue
			}
			pa, ok, err := g.pendingFrom(ctx, issues[i])
			if err != nil {
				return nil, err
			}
			if ok {
				tracked = append(tracked, pa)
			}
		}
		if len(issues) < 100 {
			break
		}
		page++
	}
	return tracked, nil
}

// pendingFrom reconstructs one [PendingAction] from an issue, fetching its label
// events only when a decision label is actually present. ok is false for an issue
// with no parseable proposal marker.
//
// The conditional fetch is what keeps a pass cheap: the common case is a trail of
// undecided artifacts, and an undecided artifact has no attribution to recover, so
// the extra request is spent only on the artifacts whose decision is about to be
// acted on.
func (g *GitHubSink) pendingFrom(ctx context.Context, issue ghIssue) (PendingAction, bool, error) {
	id, ok := ParseProposalMarker(issue.Body)
	if !ok {
		return PendingAction{}, false, nil
	}

	labels := make(map[string]bool, len(issue.Labels))
	for _, l := range issue.Labels {
		labels[l.Name] = true
	}

	pa := PendingAction{
		Identity: id,
		Ref:      ActionRef(fmt.Sprintf("%d", issue.Number)),
		Executed: labels[ExecutedLabel],
	}
	pa.ThreadTS, _ = ParseThreadMarker(issue.Body)
	pa.PreviewedResourceVersion, pa.PreviewedAt, _ = ParsePreviewMarker(issue.Body)
	pa.PreviewedState = ParsePreviewStateMarker(issue.Body)

	events := map[string]labelEvent{}
	if labels[ApprovedLabel] || labels[RejectedLabel] {
		var err error
		if events, err = g.labelEvents(ctx, issue.Number); err != nil {
			return PendingAction{}, false, err
		}
	}

	d := decisionFrom(labels, events)
	pa.State, pa.Approver, pa.DecidedAt, pa.ApproverIsSelf = d.state, d.approver, d.decidedAt, d.isSelf
	return pa, true, nil
}

// labelEvents returns the most recent `labeled` event for each label on an issue.
//
// Later events overwrite earlier ones because the endpoint returns them oldest
// first, so a label removed and re-applied carries the attribution of the SECOND
// application — which is the decision that currently stands. An `unlabeled` event
// drops the record, so a stale attribution cannot outlive the label it described:
// without that, withdrawing an approval and having a human re-approve would be
// judged against the first approver's timestamp.
func (g *GitHubSink) labelEvents(ctx context.Context, number int) (map[string]labelEvent, error) {
	events := map[string]labelEvent{}
	page := 1
	for {
		q := url.Values{}
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprintf("%d", page))
		path := fmt.Sprintf("%s/%d/events?%s", g.issuesPath(), number, q.Encode())

		var raw []ghIssueEvent
		if err := g.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		for i := range raw {
			ev := raw[i]
			if ev.Label == nil || ev.Label.Name == "" {
				continue
			}
			switch ev.Event {
			case "labeled":
				at, err := time.Parse(time.RFC3339, ev.CreatedAt)
				if err != nil {
					// An unreadable timestamp is left ABSENT rather than defaulted to now:
					// a zero DecidedAt skips the TTL check but still requires an approver,
					// whereas a fabricated "now" would silently refresh an expired consent.
					at = time.Time{}
				}
				login := ""
				actorType := ""
				if ev.Actor != nil {
					login, actorType = ev.Actor.Login, ev.Actor.Type
				}
				events[ev.Label.Name] = labelEvent{
					actor:  login,
					at:     at.UTC(),
					isSelf: g.isSelfActor(login, actorType),
				}
			case "unlabeled":
				delete(events, ev.Label.Name)
			}
		}
		if len(raw) < 100 {
			break
		}
		page++
	}
	return events, nil
}

// isSelfActor reports whether a label event's actor was MaKlaude rather than a
// person. It answers yes on two independent signals, because either alone leaves a
// hole this gate cannot afford:
//
//   - The login matches the configured [SelfLoginEnv], which covers MaKlaude running
//     under a personal access token belonging to a normal user account.
//   - GitHub reports the actor as a Bot, or its login carries the "[bot]" suffix
//     GitHub Apps are rendered with. This covers the deployment MaKlaude actually
//     ships as — a GitHub App — and, crucially, it holds even when nobody configured
//     the login. A gate whose central protection is opt-in is not a gate.
//
// Neither check is a substitute for the other: a bot check alone misses a PAT
// deployment, and a login check alone silently fails open when the variable is unset.
func (g *GitHubSink) isSelfActor(login, actorType string) bool {
	if login == "" {
		return false
	}
	if g.selfLogin != "" && strings.EqualFold(login, g.selfLogin) {
		return true
	}
	return strings.EqualFold(actorType, "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]")
}

// Create opens a new approval artifact and returns its issue number as the
// reference.
func (g *GitHubSink) Create(ctx context.Context, title, body string, labels []string) (ActionRef, error) {
	payload := map[string]any{"title": title, "body": body, "labels": labels}
	var created ghIssue
	if err := g.do(ctx, http.MethodPost, g.issuesPath(), payload, &created); err != nil {
		return "", err
	}
	return ActionRef(fmt.Sprintf("%d", created.Number)), nil
}

// Update PATCHes an artifact's title, body, and labels.
func (g *GitHubSink) Update(ctx context.Context, ref ActionRef, title, body string, labels []string) error {
	payload := map[string]any{"title": title, "body": body, "labels": labels}
	return g.do(ctx, http.MethodPatch, g.issuePath(ref), payload, nil)
}

// Comment posts a comment on an artifact.
func (g *GitHubSink) Comment(ctx context.Context, ref ActionRef, body string) error {
	payload := map[string]any{"body": body}
	return g.do(ctx, http.MethodPost, g.issuePath(ref)+"/comments", payload, nil)
}

// AddLabel adds one label without rewriting the rest, so applying
// [ExecutedLabel] cannot race a concurrent human decision off the artifact.
func (g *GitHubSink) AddLabel(ctx context.Context, ref ActionRef, label string) error {
	payload := map[string]any{"labels": []string{label}}
	return g.do(ctx, http.MethodPost, g.issuePath(ref)+"/labels", payload, nil)
}

// RemoveLabel removes one label without disturbing the others.
//
// A 404 is treated as success. The label already being absent is the state the
// caller asked for, and the alternative — failing the pass — would strand the
// refusal path: [Gatekeeper.refuse] removes [ApprovedLabel] on every refusal,
// including a retry of one that already removed it.
func (g *GitHubSink) RemoveLabel(ctx context.Context, ref ActionRef, label string) error {
	path := g.issuePath(ref) + "/labels/" + url.PathEscape(label)
	err := g.do(ctx, http.MethodDelete, path, nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.status == http.StatusNotFound {
		return nil
	}
	return err
}

// Close closes an artifact. The gatekeeper always leaves a closing comment first.
func (g *GitHubSink) Close(ctx context.Context, ref ActionRef) error {
	payload := map[string]any{"state": "closed"}
	return g.do(ctx, http.MethodPatch, g.issuePath(ref), payload, nil)
}

// issuesPath is the repo's issues collection path.
func (g *GitHubSink) issuesPath() string {
	return fmt.Sprintf("/repos/%s/%s/issues", g.cfg.Owner, g.cfg.Repo)
}

// issuePath is one artifact's path.
func (g *GitHubSink) issuePath(ref ActionRef) string {
	return g.issuesPath() + "/" + string(ref)
}

// do performs one REST call, encoding body as JSON (when non-nil) and decoding the
// response into out (when non-nil).
//
// It duplicates the small REST helper in [escalate.GitHubSink] rather than sharing
// one. Extracting a common client would mean editing the escalation trail inside a
// change to the approval gate, and the two are deliberately independent paths: the
// trail that reports problems must not be able to break because the trail that
// authorizes mutations changed. The duplication is ~40 lines of well-covered
// plumbing; the coupling would be permanent.
func (g *GitHubSink) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("approve/github: marshalling request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.base+path, reqBody)
	if err != nil {
		return fmt.Errorf("approve/github: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("approve/github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &statusError{
			method:  method,
			path:    path,
			status:  resp.StatusCode,
			excerpt: strings.TrimSpace(string(excerpt)),
		}
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("approve/github: decoding response: %w", err)
		}
	}
	return nil
}

// statusError is a non-2xx response. It is a type rather than a formatted string so
// [GitHubSink.RemoveLabel] can distinguish "already absent" from a transport or
// permission failure without matching on message text.
type statusError struct {
	method  string
	path    string
	status  int
	excerpt string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("approve/github: %s %s: unexpected status %d: %s",
		e.method, e.path, e.status, e.excerpt)
}

// ghIssue is the slice of the GitHub issue JSON this sink consumes.
type ghIssue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"`
}

// ghIssueEvent is the slice of the GitHub issue-event JSON this sink consumes. Actor
// is a pointer because GitHub omits it for events attributed to no account.
type ghIssueEvent struct {
	Event string `json:"event"`
	Actor *struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"actor"`
	Label *struct {
		Name string `json:"name"`
	} `json:"label"`
	CreatedAt string `json:"created_at"`
}

// Ensure the production sink satisfies the interface at compile time.
var _ ApprovalSink = (*GitHubSink)(nil)
