package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// # Inbound Slack (T4): reply-in-thread understood in context
//
// This file adds the INBOUND half of the Slack integration: a listener that
// captures a human's reply in an escalation thread, resolves which incident /
// issue / cluster that thread belongs to (reusing the durable thread mapping T3
// built for OUTBOUND continuity), and mirrors the reply onto the GitHub trail so
// the audit record stays complete.
//
// # Safety boundary (LOCKED)
//
// Inbound is STRICTLY read / notify / converse. A captured reply is only ever
// turned into a GitHub comment via the injected [ReplyMirror]; there is NO code
// path from an inbound event to a cluster mutation or to any actionable behavior.
// Anything actionable still routes through MaKlaude's existing human gates. This
// type holds no Kubernetes client and no escalate handle — exactly like the
// outbound [SlackNotifier], the only thing it can do is read events and hand text
// to a mirror.
//
// # Transports
//
// Two inbound transports are supported, both behind the same [InboundProcessor]
// core so the interesting logic (parse → resolve → mirror) is identical and fully
// unit-tested with zero network:
//
//   - Socket Mode (the approved default): an app-level token opens an outbound
//     WebSocket, so MaKlaude needs no public HTTP endpoint. The WebSocket dial is
//     abstracted behind [SocketDialer] so tests inject a fake stream; the live
//     dialer is a thin seam (a real WebSocket client is a deployment concern, not
//     a unit-test concern).
//   - HTTP Events API (optional): an operator may instead expose an HTTP endpoint.
//     That path REQUIRES Slack request-signature verification, implemented here in
//     [VerifySlackSignature] and exercised by [InboundHTTPHandler]. The signing
//     secret is used ONLY to verify; it is never logged.
//
// # Graceful degradation
//
// When Slack is unconfigured the listener is a no-op: [NewInboundListener]
// returns ok=false and the caller starts nothing — no connections, no errors —
// mirroring the outbound notifier's construction seam.

// ReplyMirror is the seam through which a captured inbound Slack reply is written
// to the durable audit trail (the backing GitHub issue). It is defined HERE, in
// notify, and implemented in escalate, so notify never imports escalate (which
// would be an import cycle): notify owns the inbound transport and event parsing,
// escalate owns the thread→issue resolution and the comment write.
//
// MirrorReply is called once per captured human reply. The implementation
// resolves threadTS back to the tracked issue (via the same T3 thread marker used
// for outbound continuity) and posts the reply text as a comment, so a reader of
// the GitHub issue sees the full two-way conversation. It MUST be side-effect-free
// toward any cluster — its only effect is on the comms trail.
//
// reply carries everything the mirror needs without coupling it to Slack's wire
// format. An implementation that cannot resolve the thread (an unknown or stale
// thread_ts) should return nil after a best-effort no-op rather than an error, so
// an out-of-band reply never crashes the listener; a transport/write failure may
// be returned and the listener will log-and-continue.
type ReplyMirror interface {
	MirrorReply(ctx context.Context, reply InboundReply) error
}

// InboundReply is the transport-agnostic representation of one human reply
// captured in an escalation thread. It carries only non-secret, human-authored
// content plus the routing keys (channel + threadTS) needed to map it back to an
// incident — never any token or secret.
type InboundReply struct {
	// Channel is the Slack channel the message was posted in.
	Channel string

	// ThreadTS is the parent thread timestamp the reply belongs to — the SAME
	// handle T3 persists in the backing issue, so it is the join key back to the
	// incident / issue / cluster. A message that is not in a thread has an empty
	// ThreadTS and is ignored (it is not a reply to an escalation).
	ThreadTS string

	// TS is the reply message's own timestamp (for de-duplication / ordering).
	TS string

	// User is the Slack user ID of the human who replied (rendered into the
	// mirrored comment so the trail shows who said what). Not a secret.
	User string

	// Text is the human-authored reply text. It is mirrored verbatim into the
	// GitHub comment.
	Text string
}

// isThreadedReply reports whether the event is an actual reply inside an
// escalation thread (has a thread_ts) and not the thread root itself. A root has
// ts == thread_ts; a reply has a different ts. Only genuine replies are mirrored,
// so MaKlaude's own root posts and bot echoes do not loop back into the trail.
func (r InboundReply) isThreadedReply() bool {
	return strings.TrimSpace(r.ThreadTS) != "" &&
		strings.TrimSpace(r.Text) != "" &&
		r.TS != r.ThreadTS
}

// slackEnvelope is the slice of a Slack inbound event we consume, covering both
// the Socket Mode events_api envelope and the HTTP Events API callback. We only
// read message events; everything else is ignored. The bot_id field lets us drop
// MaKlaude's own posts so a mirrored reply can never echo back into the thread.
type slackEnvelope struct {
	Type  string `json:"type"`
	Event struct {
		Type     string `json:"type"`
		Subtype  string `json:"subtype"`
		Channel  string `json:"channel"`
		User     string `json:"user"`
		BotID    string `json:"bot_id"`
		Text     string `json:"text"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"event"`
}

// parseMessageEvent extracts an [InboundReply] from a raw Slack event payload,
// returning ok=false for anything that is not a human message reply we should
// mirror: non-message events, MaKlaude's own bot posts (bot_id set), message
// edits/deletes (subtype set), and messages that are not threaded replies. This
// is the single place the "what counts as a capturable reply" policy lives, so it
// is exhaustively unit-tested.
func parseMessageEvent(raw []byte) (InboundReply, bool) {
	var env slackEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return InboundReply{}, false
	}
	e := env.Event
	if e.Type != "message" {
		return InboundReply{}, false
	}
	// Drop bot posts (including MaKlaude's own) and any message subtype
	// (edits, joins, deletes); we only mirror fresh human replies.
	if strings.TrimSpace(e.BotID) != "" || strings.TrimSpace(e.Subtype) != "" {
		return InboundReply{}, false
	}
	reply := InboundReply{
		Channel:  e.Channel,
		ThreadTS: e.ThreadTS,
		TS:       e.TS,
		User:     e.User,
		Text:     e.Text,
	}
	if !reply.isThreadedReply() {
		return InboundReply{}, false
	}
	return reply, true
}

// InboundProcessor is the transport-agnostic core: it parses one raw Slack event,
// and if it is a capturable human reply, hands it to the mirror. Both the Socket
// Mode loop and the HTTP handler funnel through Process, so the
// parse→resolve→mirror behavior is identical and tested once with no network.
type InboundProcessor struct {
	mirror ReplyMirror
}

// NewInboundProcessor builds a processor over the given mirror. A nil mirror
// panics: a caller with nothing to mirror to should not construct a processor at
// all (the [NewInboundListener] seam returns ok=false when Slack is unconfigured,
// so this is only reached on a real, configured path).
func NewInboundProcessor(mirror ReplyMirror) *InboundProcessor {
	if mirror == nil {
		panic("notify: NewInboundProcessor requires a non-nil ReplyMirror")
	}
	return &InboundProcessor{mirror: mirror}
}

// Process parses one raw Slack event and mirrors it when it is a capturable human
// reply. It returns (handled, err): handled reports whether the event was a reply
// we acted on (false for ignored events — roots, bot posts, non-messages — which
// is not an error); err is non-nil only when the mirror itself failed, so a caller
// can log-and-continue without conflating "ignored" with "broken".
func (p *InboundProcessor) Process(ctx context.Context, raw []byte) (handled bool, err error) {
	reply, ok := parseMessageEvent(raw)
	if !ok {
		return false, nil
	}
	if err := p.mirror.MirrorReply(ctx, reply); err != nil {
		return false, fmt.Errorf("notify/slack: mirroring inbound reply: %w", err)
	}
	return true, nil
}

// --- HTTP Events API path (signature verification) ---

// slackSignatureVersion is the version prefix Slack uses in the signature base
// string and the X-Slack-Signature header ("v0").
const slackSignatureVersion = "v0"

// maxSlackTimestampSkew bounds how old a signed request may be before it is
// rejected as a replay, matching Slack's own guidance (5 minutes).
const maxSlackTimestampSkew = 5 * time.Minute

// ErrInvalidSignature is returned by [VerifySlackSignature] when a request's
// signature does not validate (bad secret, tampered body, malformed or stale
// timestamp, missing headers). It is a sentinel so an HTTP handler can map it to
// 401 without string matching.
var ErrInvalidSignature = errors.New("notify/slack: invalid request signature")

// VerifySlackSignature verifies a Slack HTTP Events API request signature using
// the signing secret, per Slack's scheme: the base string is
// "v0:{timestamp}:{body}", HMAC-SHA256'd with the signing secret, hex-encoded and
// compared (in constant time) to the "v0=" X-Slack-Signature header. It also
// rejects a timestamp skewed more than [maxSlackTimestampSkew] from now to defeat
// replay.
//
// It is the security gate for the optional HTTP transport and is deliberately a
// pure function of its inputs (now is injected) so both the valid and invalid
// cases are unit-tested with no clock and no network. The signing secret is used
// only here and is never logged or returned in the error.
func VerifySlackSignature(signingSecret, timestamp, signature string, body []byte, now time.Time) error {
	if strings.TrimSpace(signingSecret) == "" {
		// No secret configured to verify against: refuse rather than accept blindly.
		return ErrInvalidSignature
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	// Reject stale/future timestamps (replay protection).
	delta := now.Sub(time.Unix(ts, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > maxSlackTimestampSkew {
		return ErrInvalidSignature
	}

	base := slackSignatureVersion + ":" + strings.TrimSpace(timestamp) + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write([]byte(base))
	expected := slackSignatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))

	// Constant-time compare to avoid leaking timing information about the secret.
	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature))) {
		return ErrInvalidSignature
	}
	return nil
}

// InboundHTTPHandler returns an [http.Handler] for the optional HTTP Events API
// transport. Every request is signature-verified (with the configured signing
// secret) BEFORE its body is parsed or mirrored; an unsigned, mis-signed, or stale
// request is rejected with 401 and never reaches the mirror. Slack's URL
// verification handshake (a one-time {"type":"url_verification","challenge":…}
// post) is answered by echoing the challenge.
//
// readBody bounds the request body so a hostile caller cannot exhaust memory; the
// raw bytes are needed verbatim for signature verification, so they are read once
// and reused for both the check and the parse.
func (l *InboundListener) InboundHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if verr := VerifySlackSignature(
			l.cfg.SigningSecret,
			r.Header.Get("X-Slack-Request-Timestamp"),
			r.Header.Get("X-Slack-Signature"),
			body,
			time.Now(),
		); verr != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// Answer Slack's URL verification handshake without involving the mirror.
		if challenge, ok := urlVerificationChallenge(body); ok {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(challenge))
			return
		}

		// Verified human reply (or an ignored event): process and ack. We ack 200
		// regardless of mirror outcome so Slack does not retry-storm; a mirror error
		// is recorded via the listener's error sink.
		if _, perr := l.proc.Process(r.Context(), body); perr != nil {
			l.onError(perr)
		}
		w.WriteHeader(http.StatusOK)
	})
}

// urlVerificationChallenge returns the challenge string when body is Slack's
// one-time URL verification handshake, so the HTTP handler can echo it back.
func urlVerificationChallenge(body []byte) (string, bool) {
	var v struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", false
	}
	if v.Type == "url_verification" && v.Challenge != "" {
		return v.Challenge, true
	}
	return "", false
}

// maxInboundBody caps an inbound HTTP request body. Slack events are tiny; this is
// purely a defensive bound against a hostile caller.
const maxInboundBody = 1 << 20 // 1 MiB

// readBody reads up to maxInboundBody bytes of r's body so the same bytes can be
// used for both signature verification and parsing.
func readBody(r *http.Request) ([]byte, error) {
	return readAllLimited(r, maxInboundBody)
}
