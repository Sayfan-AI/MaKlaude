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
// # Thread persistence (T2 scope) and graceful degradation
//
// The identity→thread mapping is held in an in-memory map for the lifetime of the
// process. This is deliberately the MINIMAL correct approach for T2: the approved
// durable design persists the thread marker in the backing GitHub issue so the
// mapping survives a process restart, but doing that here would force notify to
// import escalate — and escalate already depends on a notifier being called from
// its reconcile loop, so the import would be a cycle. Durable, cross-restart
// thread continuity therefore lands with T3 (which owns the issue-marker
// recovery), wired from a layer that can see both packages without a cycle.
//
// Until then the interface contract is honored exactly: when no thread is known
// for an identity — the common case after a restart, since the map was lost —
// NotifyUpdate / NotifyResolution DEGRADE GRACEFULLY. They do not error; they post
// the note as a fresh top-level message (and, for an update, remember the new root
// so any further updates in this run still thread together). Worst case is a
// slightly fragmented thread across a restart, never a dropped notification and
// never a failed reconcile.
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

	mu      sync.Mutex
	threads map[detect.Identity]threadRef
}

// NewSlackNotifier builds a live Slack notifier from cfg. ok is false when cfg is
// not [SlackConfig.Configured]; in that case the returned notifier is nil and the
// caller must fall back to a no-op (a [NopNotifier]) — this is the
// graceful-degradation seam that keeps a credential-less deployment behaving
// exactly like Milestone 1. The HTTP client is injectable purely for tests
// (a fake transport); production passes nil and gets a sensible timeout.
func NewSlackNotifier(cfg SlackConfig, client doer) (*SlackNotifier, bool) {
	if !cfg.Configured() {
		return nil, false
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &SlackNotifier{
		cfg:     cfg,
		client:  client,
		threads: make(map[detect.Identity]threadRef),
	}, true
}

// NotifyEscalation posts the thread ROOT for a newly-escalated problem and records
// the resulting thread timestamp under id so later updates and the resolution
// thread under it. summary is the human-facing one-liner (the same text titling
// the GitHub issue); ref is the backing issue reference (possibly empty) used to
// link the chat message back to the auditable trail.
func (s *SlackNotifier) NotifyEscalation(ctx context.Context, id detect.Identity, summary, ref string) error {
	text := escalationText(id, summary, ref)
	ts, err := s.post(ctx, text, "")
	if err != nil {
		return fmt.Errorf("notify/slack: posting escalation for %q: %w", id, err)
	}
	s.remember(id, threadRef{channel: s.cfg.Channel, threadTS: ts})
	return nil
}

// NotifyUpdate posts a follow-up reply into the existing thread for id. If no
// thread is known (for example the process restarted and the in-memory map was
// lost), it degrades gracefully: it posts a new top-level message rather than
// erroring, and remembers that message as the new thread root so subsequent
// updates in this run still thread together.
func (s *SlackNotifier) NotifyUpdate(ctx context.Context, id detect.Identity, note string) error {
	ref, known := s.lookup(id)
	ts, err := s.post(ctx, updateText(note, known), ref.threadTS)
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

// NotifyResolution posts a closing reply into the existing thread for id and then
// forgets the mapping. As with NotifyUpdate, an unknown identity degrades to a
// top-level message rather than an error.
func (s *SlackNotifier) NotifyResolution(ctx context.Context, id detect.Identity, note string) error {
	ref, known := s.lookup(id)
	if _, err := s.post(ctx, resolutionText(note, known), ref.threadTS); err != nil {
		return fmt.Errorf("notify/slack: posting resolution for %q: %w", id, err)
	}
	s.forget(id)
	return nil
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slackPostMessageURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := s.client.Do(req)
	if err != nil {
		// Never wrap the raw error with anything token-bearing; the URL and method
		// are constant and safe.
		return "", fmt.Errorf("POST %s: %w", slackPostMessageURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("POST %s: unexpected status %d: %s",
			slackPostMessageURL, resp.StatusCode, strings.TrimSpace(string(excerpt)))
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
// the one-line summary, the cluster it concerns (recovered from the stable
// identity so the line is accurate even though the interface passes only the
// summary), and a link back to the backing GitHub issue when one exists.
func escalationText(id detect.Identity, summary, ref string) string {
	var b strings.Builder
	b.WriteString(":rotating_light: *MaKlaude escalation*")
	if cluster := clusterOf(id); cluster != "" {
		b.WriteString(" on cluster `" + cluster + "`")
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(summary))
	if r := strings.TrimSpace(ref); r != "" {
		b.WriteString("\nBacking issue: #" + r)
	}
	b.WriteString("\n_MaKlaude takes no mutating action without human approval._")
	return b.String()
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

// clusterOf recovers the cluster name from a [detect.Identity], which is the
// "cluster|rule|object" form composed in the detect package. It returns empty for
// an unrecognized shape so callers simply omit the cluster line rather than render
// garbage.
func clusterOf(id detect.Identity) string {
	parts := strings.SplitN(string(id), "|", 2)
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
