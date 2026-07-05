package escalate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// fakeSlackAPI is a zero-network stand-in for the Slack Web API. It satisfies the
// transport seam [notify.NewSlackNotifier] posts through (a single Do), recording
// every chat.postMessage payload and returning a queued message timestamp so a
// test can assert the REAL [notify.SlackNotifier] — same request building, same
// thread_ts logic, same text rendering — drives the whole incident lifecycle with
// no network. It is the T7 counterpart of the fakeSlack used inside package notify;
// it lives here because it wires the LIVE notifier into the escalator to prove the
// vertical slice (incident -> escalator -> Slack thread) end to end.
type fakeSlackAPI struct {
	posts   []map[string]any
	tsQueue []string
}

func (f *fakeSlackAPI) Do(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	f.posts = append(f.posts, parsed)

	ts := "ts-default"
	if len(f.tsQueue) > 0 {
		ts = f.tsQueue[0]
		f.tsQueue = f.tsQueue[1:]
	}
	body, _ := json.Marshal(map[string]any{"ok": true, "ts": ts})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// postText returns the "text" field of the i-th recorded post.
func (f *fakeSlackAPI) postText(i int) string {
	if i < 0 || i >= len(f.posts) {
		return ""
	}
	s, _ := f.posts[i]["text"].(string)
	return s
}

// badImageSubject builds a subject whose incident carries a HIGH-confidence
// bad-image root cause, so a test can assert the top-ranked cause + confidence
// reach the Slack thread. msg lets successive cycles differ (open vs recurrence).
func badImageSubject(cluster string, id detect.Identity, msg string) Subject {
	primary := detect.Finding{
		Identity:   id,
		Severity:   detect.SeverityCritical,
		Cluster:    cluster,
		Object:     detect.Object{Kind: "deployment", Namespace: "team", Name: "api"},
		Title:      "Deployment unavailable",
		Message:    msg,
		DetectedAt: ts,
	}
	inc := correlate.Incident{
		Identity:   incidentID(id),
		Cluster:    cluster,
		Primary:    primary,
		DetectedAt: ts,
	}
	h := diagnose.Hypothesis{
		Identity:   diagnose.HypothesisIdentity("hypothesis|badimage|" + string(inc.Identity)),
		Incident:   inc.Identity,
		Cluster:    cluster,
		Cause:      diagnose.CauseBadImage,
		Confidence: diagnose.ConfidenceHigh,
		Title:      "Bad or unpullable image",
		Message:    "Container api is stuck in ImagePullBackOff.",
		Evidence:   inc.Findings(),
		Source:     diagnose.SourceDeterministic,
		DetectedAt: ts,
	}
	return Subject{Incident: inc, Hypotheses: []diagnose.Hypothesis{h}}
}

// slackTestConfig is a configured SlackConfig (so NewSlackNotifier yields a live
// notifier) with an IssueBaseURL so the backing issue renders as a clickable link.
func slackTestConfig() notify.SlackConfig {
	return notify.SlackConfig{
		BotToken:     "xoxb-test-token",
		AppToken:     "xapp-test-token",
		Channel:      "C0123456789",
		IssueBaseURL: "https://github.com/acme/clusters/issues",
	}
}

// TestEscalator_IncidentLifecycleToSlackThread is the T7 headline done-criteria
// test, exercised through the LIVE [notify.SlackNotifier] over a fake Slack API: a
// seeded incident opens a Slack thread carrying the required context (cluster,
// incident summary, top-ranked root cause + confidence, severity, backing issue
// link); a recurrence and the resolution (auto-close) both land in the SAME thread.
func TestEscalator_IncidentLifecycleToSlackThread(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	api := &fakeSlackAPI{tsQueue: []string{"1700.0001"}}

	notifier, ok := notify.NewSlackNotifier(slackTestConfig(), api)
	if !ok {
		t.Fatal("configured SlackConfig must yield a live notifier")
	}
	esc := NewEscalatorWithNotifier(sink, notifier)

	id := detect.Identity("prod|deploy.unavailable|deployment/team/api")

	// Open: a new incident posts the thread ROOT.
	if _, err := esc.Reconcile(ctx, []Subject{badImageSubject("prod", id, "0/3 replicas available")}); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Recurrence: still observed on a later cycle -> update.
	if _, err := esc.Reconcile(ctx, []Subject{badImageSubject("prod", id, "still 0/3 replicas")}); err != nil {
		t.Fatalf("recur: %v", err)
	}
	// Clearance: absent from the current diagnosis -> resolution (auto-close).
	if _, err := esc.Reconcile(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if len(api.posts) != 3 {
		t.Fatalf("want 3 Slack posts (root+update+resolution), got %d: %+v", len(api.posts), api.posts)
	}

	// --- The root carries the full incident context. ---
	root := api.postText(0)
	for _, want := range []string{
		"on cluster `prod`",       // cluster (from the incident identity, unwrapped)
		"CRITICAL",                // severity
		"Deployment unavailable",  // incident summary (primary title)
		"Bad or unpullable image", // top-ranked root cause
		"confidence high",         // its confidence
		"MaKlaude escalation",     // the escalation banner
	} {
		if !strings.Contains(root, want) {
			t.Errorf("root post missing %q:\n%s", want, root)
		}
	}
	// The backing GitHub issue must be linked (clickable, issue #58 form).
	if want := "<https://github.com/acme/clusters/issues/1|#1>"; !strings.Contains(root, want) {
		t.Errorf("root post should link the backing issue %q:\n%s", want, root)
	}
	// The root is a top-level message: it must NOT carry a thread_ts.
	if _, present := api.posts[0]["thread_ts"]; present {
		t.Errorf("root post should not carry thread_ts: %+v", api.posts[0])
	}

	// --- Recurrence and resolution land in the SAME thread. ---
	if got := api.posts[1]["thread_ts"]; got != "1700.0001" {
		t.Errorf("recurrence thread_ts = %v, want root ts 1700.0001 (same thread)", got)
	}
	if got := api.posts[2]["thread_ts"]; got != "1700.0001" {
		t.Errorf("resolution thread_ts = %v, want root ts 1700.0001 (same thread)", got)
	}
	// The update keeps the diagnosis in-thread; the resolution self-labels as resolved.
	if txt := api.postText(1); !strings.Contains(txt, "Update") || !strings.Contains(txt, "Bad or unpullable image") {
		t.Errorf("recurrence update should carry the leading hypothesis:\n%s", txt)
	}
	if txt := api.postText(2); !strings.Contains(txt, "Resolved") {
		t.Errorf("resolution should be a Resolved note:\n%s", txt)
	}

	// The GitHub trail remains the source of truth and is self-consistent: the
	// incident's one issue was auto-closed on clearance (additive, not replaced).
	if sink.OpenCount() != 0 {
		t.Errorf("after clearance the backing issue should be closed; got %d open", sink.OpenCount())
	}
}

// TestEscalator_IncidentThreadContinuityAcrossRestart is the same-thread regression
// test under the harsher condition of a process restart between every cycle: the
// LIVE notifier's in-memory map is wiped each cycle, so continuity can only come
// from the durable thread marker persisted in the backing issue. The recurrence and
// resolution must still reply into the ORIGINAL thread and post NO new root.
func TestEscalator_IncidentThreadContinuityAcrossRestart(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	id := detect.Identity("prod|deploy.unavailable|deployment/team/api")

	// Process 1: open + post root.
	api1 := &fakeSlackAPI{tsQueue: []string{"2100.0001"}}
	n1, _ := notify.NewSlackNotifier(slackTestConfig(), api1)
	if _, err := NewEscalatorWithNotifier(sink, n1).
		Reconcile(ctx, []Subject{badImageSubject("prod", id, "open")}); err != nil {
		t.Fatalf("process1 open: %v", err)
	}

	// Process 2 (restart): fresh notifier, empty in-memory map. The recurrence must
	// recover the durable ts from the issue and reply into the original thread.
	api2 := &fakeSlackAPI{tsQueue: []string{"SHOULD-NOT-BE-POSTED"}}
	n2, _ := notify.NewSlackNotifier(slackTestConfig(), api2)
	if _, err := NewEscalatorWithNotifier(sink, n2).
		Reconcile(ctx, []Subject{badImageSubject("prod", id, "recur")}); err != nil {
		t.Fatalf("process2 recur: %v", err)
	}
	if len(api2.posts) != 1 {
		t.Fatalf("restart recurrence should post exactly one reply, got %d", len(api2.posts))
	}
	// Threading into the recovered root (with no "new message" self-label) is the
	// proof no fresh thread was spawned: a degraded post would carry no thread_ts
	// and would self-label. The real Slack API returns a ts for replies too, so the
	// thread_ts — not the queue — is the authoritative signal here.
	if got := api2.posts[0]["thread_ts"]; got != "2100.0001" {
		t.Errorf("post-restart recurrence thread_ts = %v, want recovered root 2100.0001 (no new thread)", got)
	}
	if txt := api2.postText(0); strings.Contains(txt, "new message") {
		t.Errorf("recovered-thread reply must not be self-labelled as degraded:\n%s", txt)
	}

	// Process 3 (restart again): clearance resolves into the same thread.
	api3 := &fakeSlackAPI{}
	n3, _ := notify.NewSlackNotifier(slackTestConfig(), api3)
	if _, err := NewEscalatorWithNotifier(sink, n3).Reconcile(ctx, nil); err != nil {
		t.Fatalf("process3 clear: %v", err)
	}
	if len(api3.posts) != 1 || api3.posts[0]["thread_ts"] != "2100.0001" {
		t.Errorf("post-restart resolution should reply into root 2100.0001, got %+v", api3.posts)
	}
	if sink.OpenCount() != 0 {
		t.Errorf("after clearance the backing issue should be closed; got %d open", sink.OpenCount())
	}
}

// TestEscalator_UnconfiguredSlackFallsBackToGitHub proves the graceful-degradation
// done-criterion at the incident level: when Slack is unconfigured, construction
// falls back to the no-op notifier and the whole incident lifecycle runs against
// the GitHub trail alone — the fake Slack API is NEVER hit, and the backing issue is
// opened, updated on recurrence, and auto-closed on clearance exactly as in the
// GitHub + email world. Incident notifications are strictly additive.
func TestEscalator_UnconfiguredSlackFallsBackToGitHub(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()

	// An unconfigured SlackConfig yields no notifier; the caller leaves the Notifier
	// interface nil and the escalator swaps in the no-op — exactly the fallback
	// NotifierFromEnv performs when no credentials are supplied. (Assigning the
	// returned nil *SlackNotifier directly would box a typed-nil into the interface;
	// production avoids that by only using the value when ok, which we mirror here.)
	api := &fakeSlackAPI{tsQueue: []string{"unused"}}
	var notifier notify.Notifier
	if sn, ok := notify.NewSlackNotifier(notify.SlackConfig{}, api); ok {
		t.Fatal("unconfigured SlackConfig must NOT yield a notifier")
	} else if sn != nil {
		t.Fatal("unconfigured SlackConfig must return a nil notifier for the caller to replace")
	}
	// A nil Notifier interface is replaced with the no-op by NewEscalatorWithNotifier.
	esc := NewEscalatorWithNotifier(sink, notifier)

	id := detect.Identity("prod|deploy.unavailable|deployment/team/api")

	// Full lifecycle: open, recurrence, clearance.
	if _, err := esc.Reconcile(ctx, []Subject{badImageSubject("prod", id, "open")}); err != nil {
		t.Fatalf("open: %v", err)
	}
	view, okIssue := sink.Snapshot(IssueRef("1"))
	if !okIssue {
		t.Fatal("expected the incident's GitHub issue to be opened")
	}
	// No thread marker is written when Slack is unconfigured (zero Slack behavior).
	if _, marked := ParseThreadMarker(view.Body); marked {
		t.Errorf("unconfigured Slack must not write a thread marker:\n%s", view.Body)
	}
	if _, err := esc.Reconcile(ctx, []Subject{badImageSubject("prod", id, "recur")}); err != nil {
		t.Fatalf("recur: %v", err)
	}
	if _, err := esc.Reconcile(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// The GitHub trail is complete: one issue opened, updated, and auto-closed.
	if sink.OpenCount() != 0 {
		t.Errorf("issue should be auto-closed on clearance; got %d open", sink.OpenCount())
	}
	// And crucially: NOT A SINGLE Slack call was made.
	if len(api.posts) != 0 {
		t.Fatalf("unconfigured Slack must make zero API calls; got %d: %+v", len(api.posts), api.posts)
	}
}
