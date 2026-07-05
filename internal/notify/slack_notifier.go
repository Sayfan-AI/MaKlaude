package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// slackPostMessageURL is the Slack Web API endpoint used for every outbound
// post. Escalation roots and the replies that update or resolve them all go
// through chat.postMessage; a reply is just a post that carries a thread_ts.
// It is the default the notifier targets; the endpoint is overridable only via
// [WithBaseURL] so a test can stand a faithful [net/http/httptest.Server] in
// front of it (production never passes the option and hits Slack directly).
const slackPostMessageURL = "https://slack.com/api/chat.postMessage"

// doer is the narrow transport seam the [SlackNotifier] posts through. It is the
// subset of [*http.Client] the notifier needs (a single Do), extracted as an
// interface for exactly one reason: tests inject a fake that records the request
// and returns canned Slack JSON, so the whole notifier is exercised with ZERO
// network. *http.Client satisfies it directly, so production wiring passes a real
// client unchanged. This mirrors how [github.com/Sayfan-AI/MaKlaude/internal/escalate.GitHubSink]
// keeps its HTTP client injectable for httptest, kept deliberately minimal so the
// package's runtime dependency footprint stays at "stdlib only".
type doer interface {
	Do(*http.Request) (*http.Response, error)
}

// threadRef is the Slack-side handle the notifier remembers for an escalated
// problem: the channel the root was posted to and the timestamp ("ts") Slack
// returned for it, which doubles as the thread_ts replies must carry to land in
// the same thread. Both are needed because a reply must name the channel AND the
// parent ts.
type threadRef struct {
	channel  string
	threadTS string
}

// SlackNotifier is the live [Notifier]: it posts MaKlaude's escalation lifecycle
// into a Slack channel as threaded conversations over the Slack Web API
// (chat.postMessage), using only net/http — no third-party Slack SDK — to keep
// the package dependency-minimal (the codebase ships only k8s + yaml).
//
// # The escalation-as-thread model
//
// NotifyEscalation posts a top-level message — the thread ROOT — carrying the
// problem's context (cluster, what was seen, severity, and a link back to the
// backing GitHub issue, which remains the auditable source of truth). Slack
// returns a message timestamp ("ts"); the notifier records it under the problem's
// [detect.Identity]. NotifyUpdate and NotifyResolution then post REPLIES that
// carry that recorded ts as thread_ts, so recurrences and the eventual resolution
// land in the SAME thread instead of spawning new top-level noise — the chat
// counterpart to how the escalator updates and closes one GitHub issue per
// problem.
//
// # Durable thread continuity (T3) and graceful degradation
//
// Continuity is now durable across process restarts, and the durable store is the
// backing GitHub issue — NOT this type. [SlackNotifier.NotifyEscalation] RETURNS
// the root's thread_ts; the escalator persists it in the issue body (a hidden
// marker) and, on a later recurrence or clearance, recovers it and passes it back
// in as the threadTS argument to NotifyUpdate / NotifyResolution. The reply then
// lands in the original thread even after a restart that wiped all in-memory
// state. notify cannot persist anything itself (it must not import escalate, which
// would be an import cycle), so the caller owns persistence — exactly why this
// argument exists.
//
// A small in-memory identity→thread map is retained purely as a same-process
// optimization / fallback: if the caller has no handle to supply (an empty
// threadTS), a thread learned earlier in this run is still reused. The supplied
// threadTS always takes precedence over the cached one.
//
// When no thread can be determined at all — an empty supplied threadTS AND no
// cached handle (an issue opened before continuity existed, or a fresh process
// that has not posted a root) — NotifyUpdate / NotifyResolution DEGRADE
// GRACEFULLY: they do not error; they post the note as a self-labelled top-level
// message (and, for an update, remember it so further updates this run thread
// together). Worst case is a slightly fragmented thread, never a dropped
// notification and never a failed reconcile.
//
// # Safety boundary (locked)
//
// This type is comms-only. It posts text to Slack and does nothing else — it has
// no Kubernetes client, no escalate handle, and no path to any mutating action.
// The bot token is a secret: it is sent only as the Authorization bearer header to
// Slack and is NEVER logged or embedded in an error (see [SlackNotifier.post],
// which surfaces Slack's own error code but never the token).
//
// # Concurrency
//
// Like [NopNotifier] and the escalate sinks, a SlackNotifier is intended to be
// driven from a single reconciliation goroutine. The thread map is nonetheless
// guarded by a mutex so it is safe to share, matching the defensive posture of
// [github.com/Sayfan-AI/MaKlaude/internal/escalate.MemorySink].
type SlackNotifier struct {
	cfg    SlackConfig
	client doer

	// postURL is the chat.postMessage endpoint this notifier posts to. It defaults
	// to [slackPostMessageURL] (the real Slack Web API) and is overridable ONLY via
	// [WithBaseURL] so a faithful end-to-end test can point a local httptest server
	// at the notifier. Production never sets it, so production behavior is unchanged.
	postURL string

	mu      sync.Mutex
	threads map[detect.Identity]threadRef
}

// Option customizes a [SlackNotifier] at construction. It exists for exactly one
// reason: a faithful, network-free end-to-end test needs to redirect outbound
// posts at a local [net/http/httptest.Server]. Production wiring passes no
// options, so the live notifier targets the real Slack Web API unchanged.
type Option func(*SlackNotifier)

// WithBaseURL overrides the chat.postMessage endpoint the notifier posts to. The
// only intended caller is a test that runs a stand-in Slack server (an
// httptest.Server) and wants the REAL notifier — same request building, same
// thread_ts logic, same response handling — to talk to it over real HTTP rather
// than to slack.com. An empty url is ignored so the production default always
// wins. This is the deliberately-minimal seam called for by M2 T5: it adds one
// optional field and changes nothing about the default code path.
func WithBaseURL(url string) Option {
	return func(s *SlackNotifier) {
		if strings.TrimSpace(url) != "" {
			s.postURL = url
		}
	}
}

// NewSlackNotifier builds a live Slack notifier from cfg. ok is false when cfg is
// not [SlackConfig.Configured]; in that case the returned notifier is nil and the
// caller must fall back to a no-op (a [NopNotifier]) — this is the
// graceful-degradation seam that keeps a credential-less deployment behaving
// exactly like Milestone 1. The HTTP client is injectable purely for tests
// (a fake transport); production passes nil and gets a sensible timeout.
func NewSlackNotifier(cfg SlackConfig, client doer, opts ...Option) (*SlackNotifier, bool) {
	if !cfg.Configured() {
		return nil, false
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	sn := &SlackNotifier{
		cfg:     cfg,
		client:  client,
		postURL: slackPostMessageURL,
		threads: make(map[detect.Identity]threadRef),
	}
	for _, opt := range opts {
		opt(sn)
	}
	return sn, true
}

// NotifyEscalation posts the thread ROOT for a newly-escalated problem and returns
// the resulting Slack thread timestamp so the caller can persist it durably (the
// escalator writes it into the backing issue). It also records the ts in the
// in-memory cache so a same-run update/resolution still threads if the caller does
// not supply a handle. summary is the human-facing one-liner (the same text titling
// the GitHub issue); ref is the backing issue reference (possibly empty) used to
// link the chat message back to the auditable trail.
func (s *SlackNotifier) NotifyEscalation(ctx context.Context, id detect.Identity, summary, ref string, needsHuman bool) (string, error) {
	mention := ""
	if needsHuman {
		mention = s.cfg.mentionPrefix()
	}
	text := escalationText(id, summary, ref, mention, s.cfg.IssueBaseURL)
	ts, err := s.post(ctx, text, "")
	if err != nil {
		return "", fmt.Errorf("notify/slack: posting escalation for %q: %w", id, err)
	}
	s.remember(id, threadRef{channel: s.cfg.Channel, threadTS: ts})
	return ts, nil
}

// NotifyUpdate posts a follow-up reply into the thread for id. The caller-supplied
// threadTS (recovered durably from the backing issue) is authoritative; if it is
// empty, an in-memory handle learned earlier this run is used as a fallback. If
// neither is known (an issue opened before continuity existed, or a fresh process
// that has not posted a root) it degrades gracefully: it posts a self-labelled
// top-level message rather than erroring, and remembers that message as a thread
// root so subsequent updates this run still thread together.
func (s *SlackNotifier) NotifyUpdate(ctx context.Context, id detect.Identity, threadTS, note string) error {
	parent, known := s.resolveThread(id, threadTS)
	ts, err := s.post(ctx, updateText(note, known), parent)
	if err != nil {
		return fmt.Errorf("notify/slack: posting update for %q: %w", id, err)
	}
	if !known {
		// We just started a fresh root for this identity; remember it so further
		// updates this run thread under it instead of fragmenting further.
		s.remember(id, threadRef{channel: s.cfg.Channel, threadTS: ts})
	}
	return nil
}

// NotifyResolution posts a closing reply into the thread for id and then forgets
// the in-memory mapping. As with NotifyUpdate the caller-supplied threadTS is
// authoritative, falling back to the in-memory cache, and an unknown thread
// degrades to a self-labelled top-level message rather than an error.
func (s *SlackNotifier) NotifyResolution(ctx context.Context, id detect.Identity, threadTS, note string) error {
	parent, known := s.resolveThread(id, threadTS)
	if _, err := s.post(ctx, resolutionText(note, known), parent); err != nil {
		return fmt.Errorf("notify/slack: posting resolution for %q: %w", id, err)
	}
	s.forget(id)
	return nil
}

// resolveThread decides which thread_ts a reply should carry. The caller-supplied
// handle (recovered durably from the backing issue) wins; if it is empty, a handle
// cached in this process from an earlier post is used. known reports whether ANY
// thread was found, which drives the graceful-degradation self-labelling in the
// reply text.
func (s *SlackNotifier) resolveThread(id detect.Identity, threadTS string) (parent string, known bool) {
	if strings.TrimSpace(threadTS) != "" {
		return threadTS, true
	}
	if ref, ok := s.lookup(id); ok {
		return ref.threadTS, true
	}
	return "", false
}

// remember records the thread handle for an identity under the lock.
func (s *SlackNotifier) remember(id detect.Identity, ref threadRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[id] = ref
}

// lookup returns the recorded thread handle for an identity, with known=false
// when none is held (the graceful-degradation signal the post methods key on).
func (s *SlackNotifier) lookup(id detect.Identity) (threadRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.threads[id]
	return ref, ok
}

// forget drops the thread mapping for an identity, called once a problem resolves.
func (s *SlackNotifier) forget(id detect.Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.threads, id)
}

// slackPostResponse is the slice of the chat.postMessage JSON the notifier
// consumes. Slack returns HTTP 200 even on logical failure, signalling success
// only via the Ok field and the human-readable cause via Error; TS is the
// timestamp of the posted message, which becomes the thread root.
type slackPostResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	TS    string `json:"ts"`
}

// post sends one chat.postMessage call: channel + text, plus thread_ts when
// replying into an existing thread (empty for a top-level root). It returns the
// posted message's timestamp on success.
//
// It handles Slack's two-layer failure model correctly: a transport/HTTP error,
// and Slack's convention of returning HTTP 200 with {"ok":false,"error":"..."} on
// a logical failure (bad channel, revoked token, …). The latter is surfaced with
// Slack's own error code so an operator can act, but the bot token NEVER appears
// in the returned error — it is only ever placed in the Authorization header.
func (s *SlackNotifier) post(ctx context.Context, text, threadTS string) (string, error) {
	payload := map[string]any{
		"channel": s.cfg.Channel,
		"text":    text,
	}
	if threadTS != "" {
		payload["thread_ts"] = threadTS
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshalling message: %w", err)
	}

	// postURL defaults to slackPostMessageURL; a test may have redirected it to a
	// local httptest server via WithBaseURL. Either way it is a constant, non-secret
	// endpoint, so it is safe to include in error messages (the token never is).
	endpoint := s.postURL
	if endpoint == "" {
		endpoint = slackPostMessageURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		// Never wrap the raw error with anything token-bearing; the URL and method
		// are constant and safe.
		return "", fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("POST %s: unexpected status %d: %s",
			endpoint, resp.StatusCode, strings.TrimSpace(string(excerpt)))
	}

	var out slackPostResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if !out.OK {
		// Surface Slack's own error code (e.g. "channel_not_found",
		// "invalid_auth") — never the token, which is not in this struct.
		return "", fmt.Errorf("slack API error: %s", redactedSlackError(out.Error))
	}
	return out.TS, nil
}

// redactedSlackError normalizes Slack's error code for display. Slack's codes are
// short stable tokens that never contain the token, but we defensively run them
// through the same redaction guard used for secrets so no code path in this
// package can ever echo bot-token material even if a future Slack response shape
// changed. An empty code becomes a generic marker so the error is never blank.
func redactedSlackError(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return "unknown_error"
	}
	return code
}

// escalationText renders the thread-root message for a newly-escalated problem:
// an optional operator @-mention (so a needs:human escalation fires a real
// notification / mobile push), the one-line summary, the cluster it concerns
// (recovered from the stable identity so the line is accurate even though the
// interface passes only the summary), and a link back to the backing GitHub issue
// when one exists. mention is the already-rendered Slack mention token (or empty
// to omit it); the caller decides whether to supply one based on needs:human.
//
// issueBaseURL is the WEB base URL of the issue tracker's issues path (e.g.
// "https://github.com/OWNER/REPO/issues"); when set, the backing issue is rendered
// as a CLICKABLE Slack hyperlink so the operator can click straight through to the
// tracked issue (issue #58). When it is empty (unknown / unconfigured) the issue
// degrades to the previous plain "#NNN" text, so behavior is unchanged when the URL
// is not supplied. See [issueLink].
func escalationText(id detect.Identity, summary, ref, mention, issueBaseURL string) string {
	var b strings.Builder
	b.WriteString(":rotating_light: *MaKlaude escalation*")
	if cluster := clusterOf(id); cluster != "" {
		b.WriteString(" on cluster `" + cluster + "`")
	}
	if m := strings.TrimSpace(mention); m != "" {
		// Lead the body with the mention so it is the first thing the operator sees
		// and so Slack treats the post as a direct ping (mobile-push eligible).
		b.WriteString("\n" + m + " needs:human — please review.")
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(summary))
	if link := issueLink(ref, issueBaseURL); link != "" {
		b.WriteString("\nBacking issue: " + link)
	}
	b.WriteString("\n_MaKlaude takes no mutating action without human approval._")
	return b.String()
}

// issueLink renders the backing issue reference for the escalation body. ref is
// the backing issue number (tolerating an optional leading "#"); it returns empty
// when no reference exists so the caller omits the line entirely.
//
// When baseURL is set it produces a CLICKABLE Slack mrkdwn hyperlink in Slack's
// "<url|label>" form — e.g. "<https://github.com/OWNER/REPO/issues/42|#42>" — so a
// click opens the tracked issue (issue #58). When baseURL is empty (the issue
// tracker's web URL is unknown, e.g. an unconfigured deployment or a unit test that
// does not set it) it degrades to the previous plain "#42" text, keeping behavior
// identical when the URL is not supplied. The base URL is a non-secret, constant
// endpoint, so it is safe to embed verbatim.
func issueLink(ref, baseURL string) string {
	num := strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if num == "" {
		return ""
	}
	label := "#" + num
	base := strings.TrimSpace(baseURL)
	if base == "" {
		return label
	}
	url := strings.TrimRight(base, "/") + "/" + num
	return "<" + url + "|" + label + ">"
}

// updateText renders a recurrence/update reply. When the parent thread is unknown
// (post-restart degradation) it self-labels so a reader understands why it is a
// new top-level message rather than a reply.
func updateText(note string, threaded bool) string {
	body := ":arrows_counterclockwise: *Update*\n" + strings.TrimSpace(note)
	if !threaded {
		body += "\n_(thread root not found in this process; posted as a new message)_"
	}
	return body
}

// resolutionText renders the closing reply for a cleared problem, with the same
// degradation note as updateText when no thread is known.
func resolutionText(note string, threaded bool) string {
	body := ":white_check_mark: *Resolved*\n" + strings.TrimSpace(note)
	if !threaded {
		body += "\n_(thread root not found in this process; posted as a new message)_"
	}
	return body
}

// clusterOf recovers the cluster name from the stable identity a notification is
// keyed on. A raw finding identity is the "cluster|rule|object" form composed in
// the detect package, so the cluster is its first segment. An INCIDENT identity —
// the key the escalator uses for the incident lifecycle (T7) — wraps that form as
// "incident|cluster|rule|object" (see correlate's incident identity); the literal
// "incident" prefix is stripped first so the real cluster is recovered rather than
// the word "incident". It returns empty for an unrecognized shape so callers simply
// omit the cluster line rather than render garbage.
func clusterOf(id detect.Identity) string {
	// Unwrap the incident-identity prefix so incident notifications name the true
	// cluster; a plain finding identity has no such prefix and is unaffected.
	s := strings.TrimPrefix(strings.TrimSpace(string(id)), "incident|")
	parts := strings.SplitN(s, "|", 2)
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// Ensure the live notifier satisfies the interface at compile time, and that the
// standard *http.Client satisfies the transport seam.
var (
	_ Notifier = (*SlackNotifier)(nil)
	_ doer     = (*http.Client)(nil)
)
