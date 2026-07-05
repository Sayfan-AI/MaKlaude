//go:build e2e

// This file is the Milestone 2 (Slack / ChatOps) end-to-end proof. It is part of
// the same e2e-tagged package as e2e_test.go and runs in the same CI `e2e` job
// (and under `task e2e`), but it deliberately does NOT need the live kind
// cluster: the M2 definition-of-done is a property of the escalate -> notify
// layer (does an escalation reach Slack as a thread? do recurrence/resolution
// land in the SAME thread? does an unconfigured Slack degrade to GitHub + email
// with zero Slack traffic?), and that layer is cluster-agnostic. Pairing it with
// the cluster-backed no-writes proof in e2e_test.go gives one e2e job that
// asserts the whole M2 story end to end.
//
// # How Slack is mocked (no tokens, no network)
//
// A single httptest.Server stands in for the Slack Web API's chat.postMessage
// endpoint. The REAL SlackNotifier is pointed at it via notify.WithBaseURL, so
// every assertion exercises production code: the same request building, the same
// thread_ts threading logic, the same {"ok":true,"ts":...} response handling. The
// fake server records each request (path, Authorization header, decoded JSON
// body) and hands back a fresh, monotonic ts per post — exactly what Slack does
// for a real message — so the test can tell a thread ROOT (no thread_ts) from a
// REPLY (carries thread_ts) and confirm continuity. No real bot token, app token,
// or signing secret is ever used, and nothing leaves the process.
//
// # What is driven
//
// The full escalate -> notify pipeline is driven, not a fork of it:
// escalate.NewEscalatorWithNotifier(MemorySink, SlackNotifier).Reconcile(...).
// The MemorySink stands in for the GitHub trail (GitHub creds are intentionally
// absent in e2e, exactly as the no-writes test requires), and — crucially — the
// durable Slack thread_ts is persisted into the issue body marker and recovered
// on the next reconcile, so continuity is proven the way it actually works in
// production: across a simulated process restart (a brand-new Escalator + a
// brand-new SlackNotifier with an empty in-memory map).
//
// # Safety boundary (locked)
//
// This is strictly a test. The SlackNotifier is comms-only and has no cluster
// client or mutating path; adding it to the e2e harness therefore changes nothing
// about the M1 no-writes guarantee, which e2e_test.go continues to prove against
// the live cluster.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// slackPost is one recorded chat.postMessage call captured by the fake Slack
// server: enough to assert WHAT was posted (channel, text, whether it is a root
// or a reply) and HOW (the bearer auth header), all without any network.
type slackPost struct {
	authHdr  string
	channel  string
	text     string
	threadTS string // empty for a thread ROOT, set for a REPLY
	ts       string // the ts the server assigned and returned for this message
	rawBody  []byte
}

// isRoot reports whether this post opened a new thread (no thread_ts) rather than
// replying into an existing one.
func (p slackPost) isRoot() bool { return strings.TrimSpace(p.threadTS) == "" }

// fakeSlackServer is an httptest-backed stand-in for Slack's chat.postMessage
// endpoint. It is the e2e counterpart to the in-package fake doer used by the
// notify unit tests, but it speaks real HTTP so the REAL SlackNotifier (pointed
// at it via notify.WithBaseURL) is exercised end to end. It records every post
// and returns a fresh, monotonically increasing ts per call so roots and replies
// have distinguishable handles, mirroring Slack's behavior.
type fakeSlackServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	posts  []slackPost
	nextTS int
}

// newFakeSlackServer starts a fake Slack endpoint and returns it; the caller must
// Close it (typically via t.Cleanup).
func newFakeSlackServer(t *testing.T) *fakeSlackServer {
	t.Helper()
	f := &fakeSlackServer{nextTS: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// handle decodes one chat.postMessage call, records it, and responds with the
// Slack-shaped {"ok":true,"ts":...} success body.
func (f *fakeSlackServer) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)

	f.mu.Lock()
	// A monotonic, Slack-looking timestamp ("1700000001.000000", ...). Distinct per
	// post so a reply's thread_ts can be matched back to the exact root it threads
	// under.
	ts := tsString(1700000000 + f.nextTS)
	f.nextTS++
	post := slackPost{
		authHdr: r.Header.Get("Authorization"),
		ts:      ts,
		rawBody: body,
	}
	if v, ok := parsed["channel"].(string); ok {
		post.channel = v
	}
	if v, ok := parsed["text"].(string); ok {
		post.text = v
	}
	if v, ok := parsed["thread_ts"].(string); ok {
		post.threadTS = v
	}
	f.posts = append(f.posts, post)
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
}

// snapshot returns a copy of the recorded posts so far.
func (f *fakeSlackServer) snapshot() []slackPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]slackPost(nil), f.posts...)
}

// count returns how many posts the server has received — the assertion the
// unconfigured-fallback leg keys on (it must be ZERO).
func (f *fakeSlackServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

// tsString renders an integer second value as a Slack-shaped message timestamp.
func tsString(sec int) string {
	return strings.TrimSpace(itoa(sec)) + ".000000"
}

// itoa avoids pulling strconv into this tiny helper; the values are small,
// non-negative seconds.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// e2eSlackConfig is a fully-"configured" SlackConfig built from obviously-fake
// tokens. Configured() is true (so the live notifier is constructed), but the
// tokens never authenticate against anything real — the fake server ignores them
// beyond recording that the bearer header carried the bot token.
func e2eSlackConfig() notify.SlackConfig {
	return notify.SlackConfig{
		BotToken: "xoxb-e2e-fake-bot-token",
		AppToken: "xapp-e2e-fake-app-token",
		Channel:  "C0E2ESLACK",
	}
}

// e2eFinding builds a worth-escalating crashloop finding for the given identity,
// mirroring what the live scan would emit for the seeded crashloop pod (so this
// leg models the same escalation the cluster-backed leg detects, just driven
// directly so it needs no cluster).
func e2eFinding(id detect.Identity, msg string) detect.Finding {
	return detect.Finding{
		Identity: id,
		Severity: detect.SeverityCritical,
		Cluster:  "maklaude-e2e",
		Object:   detect.Object{Kind: "pod", Namespace: e2eNamespace, Name: crashloopPod},
		Title:    "Pod crashlooping in " + e2eNamespace + "/" + crashloopPod,
		Message:  msg,
	}
}

// e2eSubject wraps the e2e crashloop finding into the incident-granularity subject
// the escalator reconciles since T4, driving the SAME correlate+diagnose path the
// real scan uses (a lone finding becomes a singleton incident with the fallback
// hypothesis) so this leg exercises production wiring, just without a cluster.
func e2eSubject(id detect.Identity, msg string) escalate.Subject {
	f := e2eFinding(id, msg)
	snap := health.Snapshot{Cluster: f.Cluster}
	incidents := correlate.Correlate(snap, []detect.Finding{f})
	return escalate.Subject{Incident: incidents[0], Hypotheses: diagnose.Diagnose(snap, incidents[0])}
}

// TestE2E_SlackThreadDeliveryAndContinuity proves M2 DoD assertions (1) and (2)
// end to end against the REAL SlackNotifier and a mocked Slack API:
//
//	(1) an escalation reaches Slack as a thread ROOT carrying the problem context
//	    (cluster + summary) and the backing issue link;
//	(2) a recurrence and the eventual resolution land in the SAME thread (the
//	    root's thread_ts is reused) with NO duplicate root thread —
//
// and it proves them the way production actually works: continuity survives a
// process restart because the thread_ts is persisted in the backing issue and
// recovered on the next reconcile. Each lifecycle stage is driven by a FRESH
// escalator + FRESH notifier (empty in-memory state), so the ONLY carrier of
// continuity is the durable issue marker.
func TestE2E_SlackThreadDeliveryAndContinuity(t *testing.T) {
	ctx := context.Background()
	fake := newFakeSlackServer(t)
	cfg := e2eSlackConfig()

	// One MemorySink stands in for the durable GitHub trail across all three
	// "processes": it is where the thread_ts marker is persisted and recovered,
	// exactly as a real repo would be.
	sink := escalate.NewMemorySink()
	id := detect.Identity("maklaude-e2e|pod.crashloop|pod/" + e2eNamespace + "/" + crashloopPod)

	// newProcess builds a brand-new escalator wired to a brand-new live Slack
	// notifier pointed at the fake server — modelling a monitor process (re)start
	// with no warm in-memory thread map. The shared sink is the only durable state.
	newProcess := func(t *testing.T) *escalate.Escalator {
		t.Helper()
		sn, ok := notify.NewSlackNotifier(cfg, fake.srv.Client(), notify.WithBaseURL(fake.srv.URL))
		if !ok {
			t.Fatal("configured SlackConfig must yield a live notifier")
		}
		return escalate.NewEscalatorWithNotifier(sink, sn)
	}

	// --- Process 1: the problem is detected and escalated (thread ROOT posted). ---
	if _, err := newProcess(t).Reconcile(ctx, []escalate.Subject{e2eSubject(id, "1 restart")}); err != nil {
		t.Fatalf("process1 open: %v", err)
	}

	// --- Process 2 (restart): the problem recurs (REPLY into the original thread). ---
	out2, err := newProcess(t).Reconcile(ctx, []escalate.Subject{e2eSubject(id, "12 restarts")})
	if err != nil {
		t.Fatalf("process2 recur: %v", err)
	}
	if out2.Opened != 0 || out2.Updated != 1 {
		t.Fatalf("recurrence outcome = %+v, want opened=0 updated=1 (no duplicate issue/thread)", out2)
	}

	// --- Process 3 (restart): the problem clears (REPLY resolving the thread). ---
	if _, err := newProcess(t).Reconcile(ctx, nil); err != nil {
		t.Fatalf("process3 clear: %v", err)
	}
	if sink.OpenCount() != 0 {
		t.Errorf("after clearance the backing issue should be closed; got %d open", sink.OpenCount())
	}

	posts := fake.snapshot()
	if len(posts) != 3 {
		t.Fatalf("expected exactly 3 Slack posts (root + recurrence + resolution), got %d: %+v", len(posts), posts)
	}

	// Every post must target the configured channel and carry the bot token ONLY in
	// the Authorization header — never in the body (the locked egress boundary).
	for i, p := range posts {
		if p.channel != cfg.Channel {
			t.Errorf("post %d channel = %q, want %q", i, p.channel, cfg.Channel)
		}
		if p.authHdr != "Bearer "+cfg.BotToken {
			t.Errorf("post %d auth header = %q, want the bearer bot token", i, p.authHdr)
		}
		if strings.Contains(string(p.rawBody), cfg.BotToken) {
			t.Errorf("post %d body LEAKED the bot token: %s", i, p.rawBody)
		}
	}

	// --- (1) The first post is a thread ROOT with context + the issue link. ---
	root := posts[0]
	if !root.isRoot() {
		t.Errorf("first post should be a thread ROOT (no thread_ts), got thread_ts=%q", root.threadTS)
	}
	if !strings.Contains(root.text, "maklaude-e2e") {
		t.Errorf("root text should name the cluster: %q", root.text)
	}
	if !strings.Contains(root.text, crashloopPod) {
		t.Errorf("root text should carry the problem summary: %q", root.text)
	}
	// The backing issue is #1 in the fresh sink; the root must link it so the chat
	// message points back at the auditable trail.
	if !strings.Contains(root.text, "#1") {
		t.Errorf("root text should link the backing issue (#1): %q", root.text)
	}

	// --- (2) Recurrence + resolution are REPLIES into the SAME thread. ---
	recurrence, resolution := posts[1], posts[2]
	if recurrence.isRoot() {
		t.Errorf("recurrence should reply into the thread, not open a new root: %+v", recurrence)
	}
	if resolution.isRoot() {
		t.Errorf("resolution should reply into the thread, not open a new root: %+v", resolution)
	}
	if recurrence.threadTS != root.ts {
		t.Errorf("recurrence thread_ts = %q, want the root's ts %q (same thread)", recurrence.threadTS, root.ts)
	}
	if resolution.threadTS != root.ts {
		t.Errorf("resolution thread_ts = %q, want the root's ts %q (same thread)", resolution.threadTS, root.ts)
	}
	// Exactly one root thread ever opened — the no-duplicate guarantee.
	roots := 0
	for _, p := range posts {
		if p.isRoot() {
			roots++
		}
	}
	if roots != 1 {
		t.Errorf("expected exactly 1 thread root across the lifecycle, got %d", roots)
	}

	// Neither reply should be self-labelled as a degraded top-level message: the
	// thread was always known (recovered from the durable marker on each restart).
	for _, p := range []slackPost{recurrence, resolution} {
		if strings.Contains(p.text, "new message") {
			t.Errorf("reply should NOT be self-labelled as degraded (thread was recovered): %q", p.text)
		}
	}
}

// TestE2E_SlackUnconfiguredDegradesToGitHubOnly proves M2 DoD assertion (3): with
// Slack UNCONFIGURED the escalation pipeline degrades cleanly to the existing
// GitHub + email path — the full open/recurrence/resolution lifecycle completes
// against the issue trail, and ZERO Slack API calls are made (no errors, no
// thread marker written). This is the graceful-degradation boundary from T1/T2,
// proven end to end with a fake Slack server standing by that must never be hit.
//
// The unconfigured selection is exercised through the real env seam
// (notify.NotifierFromEnv) with the MAKLAUDE_SLACK_* variables cleared, so the
// test proves the actual production decision point yields the NopNotifier — not
// merely that a hand-constructed no-op is silent.
func TestE2E_SlackUnconfiguredDegradesToGitHubOnly(t *testing.T) {
	ctx := context.Background()

	// A fake Slack server is started but must receive NOTHING: any post to it is a
	// regression in the graceful-degradation boundary.
	fake := newFakeSlackServer(t)

	// Clear the Slack environment for this test so NotifierFromEnv takes the
	// unconfigured branch. t.Setenv restores the prior values on cleanup.
	for _, k := range []string{
		"MAKLAUDE_SLACK_BOT_TOKEN",
		"MAKLAUDE_SLACK_APP_TOKEN",
		"MAKLAUDE_SLACK_SIGNING_SECRET",
		"MAKLAUDE_SLACK_CHANNEL",
	} {
		t.Setenv(k, "")
	}

	notifier, live := notify.NotifierFromEnv()
	if live {
		t.Fatal("with the Slack env cleared, NotifierFromEnv must report live=false (unconfigured)")
	}
	if _, isNop := notifier.(notify.NopNotifier); !isNop {
		t.Fatalf("unconfigured NotifierFromEnv must yield a NopNotifier, got %T", notifier)
	}

	sink := escalate.NewMemorySink()
	esc := escalate.NewEscalatorWithNotifier(sink, notifier)
	id := detect.Identity("maklaude-e2e|pod.crashloop|pod/" + e2eNamespace + "/" + crashloopPod)

	// Full lifecycle: open -> recurrence -> clear, all against the GitHub trail.
	if _, err := esc.Reconcile(ctx, []escalate.Subject{e2eSubject(id, "1 restart")}); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := esc.Reconcile(ctx, []escalate.Subject{e2eSubject(id, "9 restarts")}); err != nil {
		t.Fatalf("recur: %v", err)
	}
	if _, err := esc.Reconcile(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// The GitHub + email trail is intact: issue #1 was opened and is now closed.
	if sink.OpenCount() != 0 {
		t.Errorf("issue should be closed after clearance; got %d open", sink.OpenCount())
	}
	view, ok := sink.Snapshot(escalate.IssueRef("1"))
	if !ok {
		t.Fatal("expected the backing issue #1 to have been opened")
	}
	if view.Open {
		t.Error("issue #1 should be closed after the clearance reconcile")
	}
	// No thread marker is ever written when Slack is unconfigured — the body stays
	// byte-for-byte the Milestone 1 body.
	if _, hasMarker := escalate.ParseThreadMarker(view.Body); hasMarker {
		t.Errorf("unconfigured Slack must not cause a thread marker to be written:\n%s", view.Body)
	}

	// THE assertion: not a single Slack call was made.
	if n := fake.count(); n != 0 {
		t.Fatalf("ZERO-SLACK VIOLATION: unconfigured pipeline made %d Slack call(s); want 0", n)
	}
}
