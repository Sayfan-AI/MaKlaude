package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recordingMirror is a zero-network [ReplyMirror] for inbound tests: it records
// every reply handed to it (so a test can assert what was captured) and can be
// told to fail to exercise the listener's log-and-continue behavior.
type recordingMirror struct {
	replies []InboundReply
	failErr error
}

func (m *recordingMirror) MirrorReply(_ context.Context, reply InboundReply) error {
	if m.failErr != nil {
		return m.failErr
	}
	m.replies = append(m.replies, reply)
	return nil
}

// messageEvent builds a raw Slack message-event payload for the tests.
func messageEvent(channel, user, text, ts, threadTS, botID, subtype string) []byte {
	return []byte(fmt.Sprintf(`{
		"type":"events_api",
		"event":{
			"type":"message",
			"subtype":%q,
			"channel":%q,
			"user":%q,
			"bot_id":%q,
			"text":%q,
			"ts":%q,
			"thread_ts":%q
		}
	}`, subtype, channel, user, botID, text, ts, threadTS))
}

// TestParseMessageEvent_TableDriven is the policy test for "what counts as a
// capturable reply": only fresh human replies inside a thread are captured; roots,
// bot posts, edits, and non-message events are ignored.
func TestParseMessageEvent_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		wantOK  bool
		wantTxt string
	}{
		{
			name:    "human threaded reply is captured",
			raw:     messageEvent("C1", "U99", "looking into it", "200.0002", "100.0001", "", ""),
			wantOK:  true,
			wantTxt: "looking into it",
		},
		{
			name:   "thread root (ts==thread_ts) is ignored",
			raw:    messageEvent("C1", "U99", "root post", "100.0001", "100.0001", "", ""),
			wantOK: false,
		},
		{
			name:   "top-level message (no thread_ts) is ignored",
			raw:    messageEvent("C1", "U99", "unrelated", "300.0003", "", "", ""),
			wantOK: false,
		},
		{
			name:   "bot post is ignored (no echo loop)",
			raw:    messageEvent("C1", "", "MaKlaude root", "200.0002", "100.0001", "B123", ""),
			wantOK: false,
		},
		{
			name:   "message edit (subtype) is ignored",
			raw:    messageEvent("C1", "U99", "edited", "200.0002", "100.0001", "", "message_changed"),
			wantOK: false,
		},
		{
			name:   "empty text reply is ignored",
			raw:    messageEvent("C1", "U99", "", "200.0002", "100.0001", "", ""),
			wantOK: false,
		},
		{
			name:   "non-message event is ignored",
			raw:    []byte(`{"type":"events_api","event":{"type":"reaction_added","thread_ts":"1.1","ts":"2.2"}}`),
			wantOK: false,
		},
		{
			name:   "malformed json is ignored",
			raw:    []byte(`{not json`),
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseMessageEvent(c.raw)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (reply=%+v)", ok, c.wantOK, got)
			}
			if ok && got.Text != c.wantTxt {
				t.Errorf("text = %q, want %q", got.Text, c.wantTxt)
			}
		})
	}
}

// TestInboundProcessor_MirrorsThreadedReply proves a captured reply is forwarded to
// the mirror with its routing keys intact, and that ignored events are NOT.
func TestInboundProcessor_MirrorsThreadedReply(t *testing.T) {
	mirror := &recordingMirror{}
	proc := NewInboundProcessor(mirror)

	// A real reply is mirrored.
	handled, err := proc.Process(context.Background(),
		messageEvent("C1", "U99", "I'll take it", "200.0002", "100.0001", "", ""))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !handled {
		t.Fatal("threaded human reply should be handled")
	}
	if len(mirror.replies) != 1 {
		t.Fatalf("want 1 mirrored reply, got %d", len(mirror.replies))
	}
	got := mirror.replies[0]
	if got.ThreadTS != "100.0001" || got.Channel != "C1" || got.User != "U99" || got.Text != "I'll take it" {
		t.Errorf("mirrored reply lost data: %+v", got)
	}

	// A bot echo is ignored: not handled, nothing mirrored.
	handled, err = proc.Process(context.Background(),
		messageEvent("C1", "", "echo", "201.0001", "100.0001", "B1", ""))
	if err != nil || handled {
		t.Fatalf("bot post should be ignored: handled=%v err=%v", handled, err)
	}
	if len(mirror.replies) != 1 {
		t.Errorf("bot post must not be mirrored; got %d total", len(mirror.replies))
	}
}

// TestInboundProcessor_MirrorErrorSurfaces proves a mirror failure is surfaced as
// an error (so the listener can log-and-continue) rather than swallowed.
func TestInboundProcessor_MirrorErrorSurfaces(t *testing.T) {
	mirror := &recordingMirror{failErr: errors.New("github down")}
	proc := NewInboundProcessor(mirror)

	_, err := proc.Process(context.Background(),
		messageEvent("C1", "U99", "hello", "200.0002", "100.0001", "", ""))
	if err == nil || !strings.Contains(err.Error(), "github down") {
		t.Fatalf("mirror error should surface, got %v", err)
	}
}

// signSlack computes a valid Slack signature for body at ts using secret — the
// exact scheme VerifySlackSignature checks, so the test signs the way Slack does.
func signSlack(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte("v0:" + ts + ":" + string(body)))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// TestVerifySlackSignature is the locked done-criterion: signature verification is
// unit-tested for BOTH the valid and the invalid cases (and the replay window).
func TestVerifySlackSignature(t *testing.T) {
	const secret = "8f742231b10e8888abcd99yyyzzz85a5"
	now := time.Unix(1_700_000_000, 0)
	nowTS := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"event_callback","event":{"type":"message"}}`)
	valid := signSlack(secret, nowTS, body)

	cases := []struct {
		name      string
		secret    string
		ts        string
		signature string
		body      []byte
		wantErr   bool
	}{
		{"valid signature accepted", secret, nowTS, valid, body, false},
		{"wrong secret rejected", "different-secret", nowTS, valid, body, true},
		{"tampered body rejected", secret, nowTS, valid, []byte(`{"tampered":true}`), true},
		{"tampered signature rejected", secret, nowTS, "v0=deadbeef", body, true},
		{"empty signing secret rejected", "", nowTS, valid, body, true},
		{"non-numeric timestamp rejected", secret, "not-a-number", valid, body, true},
		{
			name:      "stale timestamp rejected (replay)",
			secret:    secret,
			ts:        strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10),
			signature: signSlack(secret, strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10), body),
			body:      body,
			wantErr:   true,
		},
		{
			name:      "future timestamp rejected (replay)",
			secret:    secret,
			ts:        strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10),
			signature: signSlack(secret, strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10), body),
			body:      body,
			wantErr:   true,
		},
		{
			name:      "within-window timestamp accepted",
			secret:    secret,
			ts:        strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10),
			signature: signSlack(secret, strconv.FormatInt(now.Add(-2*time.Minute).Unix(), 10), body),
			body:      body,
			wantErr:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := VerifySlackSignature(c.secret, c.ts, c.signature, c.body, now)
			if c.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if c.wantErr && err != nil && !errors.Is(err, ErrInvalidSignature) {
				t.Errorf("error should be ErrInvalidSignature, got %v", err)
			}
		})
	}
}

// TestVerifySlackSignature_NeverLeaksSecret proves the verification error never
// echoes the signing secret.
func TestVerifySlackSignature_NeverLeaksSecret(t *testing.T) {
	const secret = "super-secret-signing-key"
	err := VerifySlackSignature(secret, "not-a-number", "v0=x", []byte("{}"), time.Now())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("verification error LEAKED the signing secret: %v", err)
	}
}

// inboundConfig is a configured SlackConfig (with a signing secret, since the HTTP
// path needs it) for the listener tests.
func inboundConfig(signingSecret string) SlackConfig {
	return SlackConfig{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		SigningSecret: signingSecret,
		Channel:       "C1",
	}
}

// TestInboundHTTPHandler_SignedRequestMirrored proves the end-to-end HTTP path: a
// correctly-signed threaded reply is verified, parsed, and mirrored, and the
// handler acks 200.
func TestInboundHTTPHandler_SignedRequestMirrored(t *testing.T) {
	const secret = "signing-secret-http"
	mirror := &recordingMirror{}
	listener, ok := NewInboundListener(inboundConfig(secret), mirror)
	if !ok {
		t.Fatal("configured listener expected")
	}

	body := messageEvent("C1", "U7", "ack from ops", "200.0002", "100.0001", "", "")
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", signSlack(secret, ts, body))
	rec := httptest.NewRecorder()

	listener.InboundHTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(mirror.replies) != 1 || mirror.replies[0].Text != "ack from ops" {
		t.Fatalf("expected the reply mirrored, got %+v", mirror.replies)
	}
}

// TestInboundHTTPHandler_BadSignatureRejected proves an unsigned/mis-signed request
// is rejected with 401 and NEVER reaches the mirror — the security gate holds.
func TestInboundHTTPHandler_BadSignatureRejected(t *testing.T) {
	const secret = "signing-secret-http"
	mirror := &recordingMirror{}
	listener, _ := NewInboundListener(inboundConfig(secret), mirror)

	body := messageEvent("C1", "U7", "spoofed", "200.0002", "100.0001", "", "")
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0=forged-signature")
	rec := httptest.NewRecorder()

	listener.InboundHTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(mirror.replies) != 0 {
		t.Fatalf("a mis-signed request must NEVER reach the mirror; got %+v", mirror.replies)
	}
}

// TestInboundHTTPHandler_URLVerification proves Slack's one-time URL verification
// handshake is answered with the challenge (after signature verification).
func TestInboundHTTPHandler_URLVerification(t *testing.T) {
	const secret = "signing-secret-http"
	mirror := &recordingMirror{}
	listener, _ := NewInboundListener(inboundConfig(secret), mirror)

	body := []byte(`{"type":"url_verification","challenge":"abc123challenge"}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", signSlack(secret, ts, body))
	rec := httptest.NewRecorder()

	listener.InboundHTTPHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "abc123challenge" {
		t.Errorf("challenge echo = %q, want abc123challenge", got)
	}
	if len(mirror.replies) != 0 {
		t.Errorf("URL verification must not reach the mirror; got %+v", mirror.replies)
	}
}

// fakeStream is a zero-network [SocketStream]: it replays queued payloads then
// returns io.EOF, so the Socket Mode loop is exercised without a WebSocket.
type fakeStream struct {
	payloads [][]byte
	closed   bool
}

func (s *fakeStream) Next(_ context.Context) ([]byte, error) {
	if len(s.payloads) == 0 {
		return nil, io.EOF
	}
	p := s.payloads[0]
	s.payloads = s.payloads[1:]
	return p, nil
}

func (s *fakeStream) Close() error { s.closed = true; return nil }

// fakeDialer hands back a pre-seeded fakeStream (or an error) so RunSocketMode is
// tested with no network.
type fakeDialer struct {
	stream  *fakeStream
	dialErr error
}

func (d *fakeDialer) Dial(_ context.Context, _ SlackConfig) (SocketStream, error) {
	if d.dialErr != nil {
		return nil, d.dialErr
	}
	return d.stream, nil
}

// TestRunSocketMode_ProcessesUntilEOF proves the approved-default loop: it dials,
// processes each replayed event (mirroring the human replies, ignoring the rest),
// closes the stream, and returns nil on a clean EOF.
func TestRunSocketMode_ProcessesUntilEOF(t *testing.T) {
	mirror := &recordingMirror{}
	stream := &fakeStream{payloads: [][]byte{
		messageEvent("C1", "U1", "on it", "200.0002", "100.0001", "", ""),      // reply -> mirrored
		messageEvent("C1", "", "bot echo", "200.0003", "100.0001", "B1", ""),   // bot -> ignored
		messageEvent("C1", "U1", "root", "100.0001", "100.0001", "", ""),       // root -> ignored
		messageEvent("C1", "U2", "also on it", "200.0004", "100.0001", "", ""), // reply -> mirrored
	}}
	listener, _ := NewInboundListener(inboundConfig(""), mirror, WithSocketDialer(&fakeDialer{stream: stream}))

	if err := listener.RunSocketMode(context.Background()); err != nil {
		t.Fatalf("RunSocketMode: %v", err)
	}
	if !stream.closed {
		t.Error("stream should be closed when the loop ends")
	}
	if len(mirror.replies) != 2 {
		t.Fatalf("want 2 human replies mirrored, got %d: %+v", len(mirror.replies), mirror.replies)
	}
}

// TestRunSocketMode_DialErrorReturned proves a dial failure is returned (not
// swallowed), so an operator sees a broken inbound connection.
func TestRunSocketMode_DialErrorReturned(t *testing.T) {
	mirror := &recordingMirror{}
	listener, _ := NewInboundListener(inboundConfig(""), mirror,
		WithSocketDialer(&fakeDialer{dialErr: errors.New("dial refused")}))

	err := listener.RunSocketMode(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dial refused") {
		t.Fatalf("dial error should be returned, got %v", err)
	}
}

// TestRunSocketMode_RequiresDialer proves that without an injected dialer the loop
// refuses to run rather than guessing a transport.
func TestRunSocketMode_RequiresDialer(t *testing.T) {
	mirror := &recordingMirror{}
	listener, _ := NewInboundListener(inboundConfig(""), mirror)
	if err := listener.RunSocketMode(context.Background()); err == nil {
		t.Fatal("RunSocketMode without a dialer should error")
	}
}

// TestRunSocketMode_MirrorErrorContinues proves a per-event mirror failure is
// routed to the error handler and the loop CONTINUES (one bad reply never kills
// the listener).
func TestRunSocketMode_MirrorErrorContinues(t *testing.T) {
	mirror := &recordingMirror{failErr: errors.New("transient github error")}
	var caught int
	stream := &fakeStream{payloads: [][]byte{
		messageEvent("C1", "U1", "reply one", "200.0002", "100.0001", "", ""),
		messageEvent("C1", "U1", "reply two", "200.0003", "100.0001", "", ""),
	}}
	listener, _ := NewInboundListener(inboundConfig(""), mirror,
		WithSocketDialer(&fakeDialer{stream: stream}),
		WithErrorHandler(func(error) { caught++ }))

	if err := listener.RunSocketMode(context.Background()); err != nil {
		t.Fatalf("RunSocketMode should not return on per-event errors: %v", err)
	}
	if caught != 2 {
		t.Errorf("both mirror failures should reach the error handler; caught %d", caught)
	}
}

// TestNewInboundListener_GracefulDegradation proves the construction seam: an
// unconfigured Slack config yields no listener (the caller starts nothing — a pure
// no-op), a configured one yields a live listener.
func TestNewInboundListener_GracefulDegradation(t *testing.T) {
	if l, ok := NewInboundListener(SlackConfig{}, &recordingMirror{}); ok || l != nil {
		t.Error("unconfigured SlackConfig must not yield an inbound listener")
	}
	if l, ok := NewInboundListener(inboundConfig(""), &recordingMirror{}); !ok || l == nil {
		t.Error("configured SlackConfig must yield an inbound listener")
	}
}
