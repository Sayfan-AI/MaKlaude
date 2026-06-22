package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// recordedRequest captures everything a test needs to assert about one outbound
// chat.postMessage call without any network.
type recordedRequest struct {
	url       string
	authHdr   string
	body      map[string]any
	bodyBytes []byte
}

// fakeSlack is a fake [doer] transport: it records every request and returns a
// canned response, so the [SlackNotifier] is exercised end to end with ZERO
// network. respFor maps a returned ts (or an error) per call.
type fakeSlack struct {
	requests []recordedRequest

	// nextTS is the ts handed back for each successful post; tests pre-seed it so
	// the root and reply timestamps are distinguishable.
	tsQueue []string
	// failOK, when set, makes the next response a Slack logical failure
	// ({"ok":false,"error":...}) using errCode, modelling Slack's HTTP-200 errors.
	failOK  bool
	errCode string
	// httpStatus, when non-zero, overrides the HTTP status code (to model a
	// transport-level non-2xx).
	httpStatus int
}

func (f *fakeSlack) Do(req *http.Request) (*http.Response, error) {
	bodyBytes, _ := io.ReadAll(req.Body)
	var parsed map[string]any
	_ = json.Unmarshal(bodyBytes, &parsed)
	f.requests = append(f.requests, recordedRequest{
		url:       req.URL.String(),
		authHdr:   req.Header.Get("Authorization"),
		body:      parsed,
		bodyBytes: bodyBytes,
	})

	status := http.StatusOK
	if f.httpStatus != 0 {
		status = f.httpStatus
	}

	var respObj map[string]any
	if f.failOK {
		respObj = map[string]any{"ok": false, "error": f.errCode}
	} else {
		ts := "1700000000.000000"
		if len(f.tsQueue) > 0 {
			ts = f.tsQueue[0]
			f.tsQueue = f.tsQueue[1:]
		}
		respObj = map[string]any{"ok": true, "ts": ts}
	}
	rb, _ := json.Marshal(respObj)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(rb))),
		Header:     make(http.Header),
	}, nil
}

func testConfig() SlackConfig {
	return SlackConfig{
		BotToken: "xoxb-test-token",
		AppToken: "xapp-test-token",
		Channel:  "C0123456789",
	}
}

// TestSlackNotifier_ThreadLifecycle is the headline test: an escalation posts a
// root, captures its ts, and the following update + resolution reply into the SAME
// thread by reusing that ts as thread_ts. It asserts the URL, the bearer auth
// header, and the JSON payload (channel, text, thread_ts) at each step.
func TestSlackNotifier_ThreadLifecycle(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{tsQueue: []string{"111.000001", "222.000002", "333.000003"}}
	cfg := testConfig()

	sn, ok := NewSlackNotifier(cfg, fake)
	if !ok {
		t.Fatal("configured SlackConfig must yield a notifier")
	}

	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	rootTS, err := sn.NotifyEscalation(ctx, id, "Pod crashlooping in team/api", "42", false)
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	if rootTS != "111.000001" {
		t.Fatalf("NotifyEscalation returned ts = %q, want the root ts 111.000001", rootTS)
	}
	// Pass an empty threadTS so this exercises the in-memory same-run fallback;
	// the durable cross-restart path is covered by TestSlackNotifier_DurableThreadAcrossRestart.
	if err := sn.NotifyUpdate(ctx, id, "", "still crashlooping, 5 restarts"); err != nil {
		t.Fatalf("NotifyUpdate: %v", err)
	}
	if err := sn.NotifyResolution(ctx, id, "", "pod recovered"); err != nil {
		t.Fatalf("NotifyResolution: %v", err)
	}

	if len(fake.requests) != 3 {
		t.Fatalf("expected 3 posts, got %d", len(fake.requests))
	}

	for i, r := range fake.requests {
		if r.url != slackPostMessageURL {
			t.Errorf("post %d URL = %q, want %q", i, r.url, slackPostMessageURL)
		}
		if r.authHdr != "Bearer xoxb-test-token" {
			t.Errorf("post %d auth = %q, want bearer bot token", i, r.authHdr)
		}
		if r.body["channel"] != "C0123456789" {
			t.Errorf("post %d channel = %v, want C0123456789", i, r.body["channel"])
		}
		if _, ok := r.body["text"].(string); !ok || r.body["text"] == "" {
			t.Errorf("post %d missing text: %v", i, r.body)
		}
	}

	// Root must NOT carry thread_ts.
	if _, present := fake.requests[0].body["thread_ts"]; present {
		t.Error("escalation root should not carry thread_ts")
	}
	// The escalation root should carry the cluster (from identity) and the issue link.
	rootText, _ := fake.requests[0].body["text"].(string)
	if !strings.Contains(rootText, "prod") {
		t.Errorf("root text should mention the cluster: %q", rootText)
	}
	if !strings.Contains(rootText, "#42") {
		t.Errorf("root text should link the backing issue: %q", rootText)
	}

	// Update + resolution must reply into the root's thread_ts = "111.000001".
	if got := fake.requests[1].body["thread_ts"]; got != "111.000001" {
		t.Errorf("update thread_ts = %v, want 111.000001 (root reuse)", got)
	}
	if got := fake.requests[2].body["thread_ts"]; got != "111.000001" {
		t.Errorf("resolution thread_ts = %v, want 111.000001 (root reuse)", got)
	}

	// After resolution the mapping is forgotten.
	if _, known := sn.lookup(id); known {
		t.Error("identity should be forgotten after resolution")
	}
}

// TestSlackNotifier_GracefulDegradationOnUnknownThread proves that an update or
// resolution for an identity with no known thread (e.g. process restart) does NOT
// error and posts a top-level message instead of a reply.
func TestSlackNotifier_GracefulDegradationOnUnknownThread(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{tsQueue: []string{"900.000001", "900.000002"}}

	sn, _ := NewSlackNotifier(testConfig(), fake)
	id := detect.Identity("prod|node.notready|node/node-a")

	// No prior escalation AND no supplied handle: update must succeed and post
	// top-level (no thread_ts).
	if err := sn.NotifyUpdate(ctx, id, "", "recurred after restart"); err != nil {
		t.Fatalf("NotifyUpdate on unknown thread must not error: %v", err)
	}
	if _, present := fake.requests[0].body["thread_ts"]; present {
		t.Error("degraded update should be top-level (no thread_ts)")
	}
	if txt, _ := fake.requests[0].body["text"].(string); !strings.Contains(txt, "new message") {
		t.Errorf("degraded update should self-label: %q", txt)
	}

	// The degraded update remembered a new root; a second update (still no supplied
	// handle) threads under it via the in-memory fallback.
	if err := sn.NotifyUpdate(ctx, id, "", "still recurring"); err != nil {
		t.Fatalf("second NotifyUpdate: %v", err)
	}
	if got := fake.requests[1].body["thread_ts"]; got != "900.000001" {
		t.Errorf("second update thread_ts = %v, want 900.000001 (degraded root reuse)", got)
	}

	// A resolution with no known thread also degrades without error.
	other := detect.Identity("prod|pod.crashloop|pod/team/db")
	if err := sn.NotifyResolution(ctx, other, "", "cleared"); err != nil {
		t.Fatalf("NotifyResolution on unknown thread must not error: %v", err)
	}
	if _, present := fake.requests[2].body["thread_ts"]; present {
		t.Error("degraded resolution should be top-level (no thread_ts)")
	}
}

// TestSlackNotifier_DurableThreadAcrossRestart is the T3 headline test: a FRESH
// notifier (modelling a process restart that wiped the in-memory map) still threads
// an update and resolution into the original root when the caller supplies the
// durably-recovered thread_ts. No new root is posted; the replies carry the
// supplied thread_ts; and they are NOT self-labelled as degraded.
func TestSlackNotifier_DurableThreadAcrossRestart(t *testing.T) {
	ctx := context.Background()

	// "Process 1" posts the root and learns its ts (which the caller would persist
	// in the backing issue).
	fake1 := &fakeSlack{tsQueue: []string{"111.000001"}}
	sn1, _ := NewSlackNotifier(testConfig(), fake1)
	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	rootTS, err := sn1.NotifyEscalation(ctx, id, "Pod crashlooping", "42", false)
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}

	// "Process 2": a brand-new notifier with an empty in-memory map. The caller
	// recovered rootTS from the issue and supplies it.
	fake2 := &fakeSlack{tsQueue: []string{"222.000002", "333.000003"}}
	sn2, _ := NewSlackNotifier(testConfig(), fake2)
	if _, known := sn2.lookup(id); known {
		t.Fatal("fresh notifier must have no in-memory thread for the identity")
	}

	if err := sn2.NotifyUpdate(ctx, id, rootTS, "still crashlooping"); err != nil {
		t.Fatalf("NotifyUpdate: %v", err)
	}
	if err := sn2.NotifyResolution(ctx, id, rootTS, "recovered"); err != nil {
		t.Fatalf("NotifyResolution: %v", err)
	}

	if len(fake2.requests) != 2 {
		t.Fatalf("restart process should post exactly 2 replies (no new root), got %d", len(fake2.requests))
	}
	for i, r := range fake2.requests {
		if got := r.body["thread_ts"]; got != rootTS {
			t.Errorf("reply %d thread_ts = %v, want recovered root %q", i, got, rootTS)
		}
		if txt, _ := r.body["text"].(string); strings.Contains(txt, "new message") {
			t.Errorf("reply %d should NOT be self-labelled as degraded: %q", i, txt)
		}
	}
}

// TestSlackNotifier_SlackLogicalError proves Slack's HTTP-200 {"ok":false} error
// surfaces as a Go error carrying Slack's error code — and never the bot token.
func TestSlackNotifier_SlackLogicalError(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{failOK: true, errCode: "channel_not_found"}

	sn, _ := NewSlackNotifier(testConfig(), fake)
	_, err := sn.NotifyEscalation(ctx, "prod|x|pod/a/b", "summary", "1", false)
	if err == nil {
		t.Fatal("expected an error for ok:false response")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error should surface Slack's code: %v", err)
	}
	if strings.Contains(err.Error(), "xoxb-test-token") {
		t.Fatalf("error LEAKED the bot token: %v", err)
	}
}

// TestSlackNotifier_HTTPError proves a transport-level non-2xx surfaces as an
// error without leaking the token.
func TestSlackNotifier_HTTPError(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{httpStatus: http.StatusTooManyRequests}

	sn, _ := NewSlackNotifier(testConfig(), fake)
	_, err := sn.NotifyEscalation(ctx, "prod|x|pod/a/b", "summary", "1", false)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected a 429 error, got %v", err)
	}
	if strings.Contains(err.Error(), "xoxb-test-token") {
		t.Fatalf("error LEAKED the bot token: %v", err)
	}
}

// TestSlackNotifier_NoTokenInPayloadBody is the locked-safety-boundary egress
// test: the bot token must travel ONLY in the Authorization header, never in any
// request body sent over the wire.
func TestSlackNotifier_NoTokenInPayloadBody(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{tsQueue: []string{"1.1", "1.2", "1.3"}}

	sn, _ := NewSlackNotifier(testConfig(), fake)
	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	_, _ = sn.NotifyEscalation(ctx, id, "summary", "42", false)
	_ = sn.NotifyUpdate(ctx, id, "1.1", "update note")
	_ = sn.NotifyResolution(ctx, id, "1.1", "resolved note")

	for i, r := range fake.requests {
		if strings.Contains(string(r.bodyBytes), "xoxb-test-token") {
			t.Errorf("post %d body LEAKED the bot token: %s", i, r.bodyBytes)
		}
	}
}

// TestNewSlackNotifier_GracefulDegradation proves the construction seam: an
// unconfigured config yields no notifier (caller falls back to no-op), a
// configured one yields a live notifier.
func TestNewSlackNotifier_GracefulDegradation(t *testing.T) {
	if _, ok := NewSlackNotifier(SlackConfig{}, nil); ok {
		t.Error("unconfigured SlackConfig must not yield a notifier")
	}
	if sn, ok := NewSlackNotifier(testConfig(), nil); !ok || sn == nil {
		t.Error("configured SlackConfig must yield a notifier")
	}
}
