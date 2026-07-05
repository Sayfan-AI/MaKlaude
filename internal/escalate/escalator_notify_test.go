package escalate

import (
	"context"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// notifyCall records one method invocation on the fakeNotifier so tests can assert
// the exact chat lifecycle the escalator drove: which method, for which identity,
// and with which thread handle.
type notifyCall struct {
	method     string // "escalation", "update", or "resolution"
	id         detect.Identity
	threadTS   string // the thread_ts the escalator SUPPLIED (empty on escalation)
	note       string
	needsHuman bool // the needs:human hint passed on escalation
}

// fakeNotifier is a zero-network [notify.Notifier] for escalate tests. It records
// every call and hands back a deterministic thread_ts per escalation (modelling
// Slack returning the root's ts), so a test can assert root + update + resolution
// land in one thread — including across a simulated restart, by checking the
// thread_ts the escalator recovered from the issue marker and supplied back in.
type fakeNotifier struct {
	calls []notifyCall

	// rootTS is the ts returned for the next NotifyEscalation; tests pre-seed a
	// queue so successive roots are distinguishable.
	tsQueue []string
}

func (f *fakeNotifier) NotifyEscalation(_ context.Context, id detect.Identity, summary, _ string, needsHuman bool) (string, error) {
	ts := "thread-default"
	if len(f.tsQueue) > 0 {
		ts = f.tsQueue[0]
		f.tsQueue = f.tsQueue[1:]
	}
	f.calls = append(f.calls, notifyCall{method: "escalation", id: id, note: summary, needsHuman: needsHuman})
	return ts, nil
}

func (f *fakeNotifier) NotifyUpdate(_ context.Context, id detect.Identity, threadTS, note string) error {
	f.calls = append(f.calls, notifyCall{method: "update", id: id, threadTS: threadTS, note: note})
	return nil
}

func (f *fakeNotifier) NotifyResolution(_ context.Context, id detect.Identity, threadTS, note string) error {
	f.calls = append(f.calls, notifyCall{method: "resolution", id: id, threadTS: threadTS, note: note})
	return nil
}

// crashFinding builds a worth-escalating finding for the given identity.
func crashFinding(id detect.Identity, msg string) detect.Finding {
	return detect.Finding{
		Identity: id, Severity: detect.SeverityCritical, Cluster: "prod",
		Object: detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
		Title:  "Pod crashlooping", Message: msg, DetectedAt: ts,
	}
}

// TestEscalator_ChatLifecycleOneThread is the T3 done-criteria test: a
// recurring-then-cleared incident yields root + update + resolution, and the
// update and resolution both carry the SAME thread_ts the root produced — one
// thread, no duplicates — all within a single process (in-memory state warm).
func TestEscalator_ChatLifecycleOneThread(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	notifier := &fakeNotifier{tsQueue: []string{"1700.0001"}}
	esc := NewEscalatorWithNotifier(sink, notifier)

	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	// Open.
	if _, err := esc.Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "1 restart"))}); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Recurrence (update).
	if _, err := esc.Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "12 restarts"))}); err != nil {
		t.Fatalf("recur: %v", err)
	}
	// Clearance (resolution).
	if _, err := esc.Reconcile(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	if len(notifier.calls) != 3 {
		t.Fatalf("want exactly 3 chat calls (root+update+resolution), got %d: %+v", len(notifier.calls), notifier.calls)
	}
	if notifier.calls[0].method != "escalation" || notifier.calls[1].method != "update" || notifier.calls[2].method != "resolution" {
		t.Fatalf("chat lifecycle order wrong: %+v", notifier.calls)
	}
	// The update and resolution must thread under the root's ts — one thread.
	if got := notifier.calls[1].threadTS; got != "1700.0001" {
		t.Errorf("update thread_ts = %q, want root ts 1700.0001 (same thread)", got)
	}
	if got := notifier.calls[2].threadTS; got != "1700.0001" {
		t.Errorf("resolution thread_ts = %q, want root ts 1700.0001 (same thread)", got)
	}
	// No duplicate issue/thread was opened on recurrence.
	if sink.OpenCount() != 0 {
		t.Errorf("after clearance the issue should be closed; got %d open", sink.OpenCount())
	}
}

// TestEscalator_ThreadMarkerPersistedOnOpen proves the thread_ts the root produced
// is written DURABLY into the backing issue body (the marker), so it survives a
// process restart.
func TestEscalator_ThreadMarkerPersistedOnOpen(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	notifier := &fakeNotifier{tsQueue: []string{"1800.0009"}}
	esc := NewEscalatorWithNotifier(sink, notifier)

	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	if _, err := esc.Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "x"))}); err != nil {
		t.Fatalf("open: %v", err)
	}

	view, ok := sink.Snapshot(IssueRef("1"))
	if !ok {
		t.Fatal("expected issue #1")
	}
	// Both markers must coexist in the body.
	gotID, ok := ParseIdentityMarker(view.Body)
	if !ok || gotID != incidentID(id) {
		t.Errorf("identity marker lost: got %q ok=%v", gotID, ok)
	}
	gotTS, ok := ParseThreadMarker(view.Body)
	if !ok || gotTS != "1800.0009" {
		t.Errorf("thread marker = %q ok=%v, want 1800.0009", gotTS, ok)
	}
}

// TestEscalator_DurableThreadAcrossRestart is the cross-restart done-criterion: a
// FRESH escalator + FRESH notifier (in-memory map gone) recovers the thread_ts
// from the issue marker and threads the recurrence/resolution into the ORIGINAL
// thread — no duplicate thread, no duplicate issue.
func TestEscalator_DurableThreadAcrossRestart(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()

	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	// Process 1 opens the issue and posts the root.
	n1 := &fakeNotifier{tsQueue: []string{"2100.0001"}}
	if _, err := NewEscalatorWithNotifier(sink, n1).Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "1 restart"))}); err != nil {
		t.Fatalf("process1 open: %v", err)
	}

	// Process 2 (restart): brand-new escalator AND notifier — no in-memory thread
	// state anywhere. Recurrence must recover the persisted ts from the issue.
	n2 := &fakeNotifier{tsQueue: []string{"SHOULD-NOT-BE-USED"}}
	out, err := NewEscalatorWithNotifier(sink, n2).Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "12 restarts"))})
	if err != nil {
		t.Fatalf("process2 recur: %v", err)
	}
	if out.Updated != 1 || out.Opened != 0 {
		t.Fatalf("post-restart recurrence outcome = %v, want updated=1 opened=0 (no duplicate)", out)
	}
	if sink.OpenCount() != 1 {
		t.Fatalf("post-restart: want exactly 1 open issue (no duplicate), got %d", sink.OpenCount())
	}
	if len(n2.calls) != 1 || n2.calls[0].method != "update" {
		t.Fatalf("process2 should post exactly one update, got %+v", n2.calls)
	}
	if got := n2.calls[0].threadTS; got != "2100.0001" {
		t.Errorf("post-restart update thread_ts = %q, want recovered root 2100.0001 (no new thread)", got)
	}
	// The tsQueue must be untouched: no new root was posted on restart.
	if len(n2.tsQueue) != 1 {
		t.Errorf("a new root was posted on restart (tsQueue consumed): %v", n2.tsQueue)
	}

	// Process 3 (another restart): clearance must resolve into the same thread.
	n3 := &fakeNotifier{}
	if _, err := NewEscalatorWithNotifier(sink, n3).Reconcile(ctx, nil); err != nil {
		t.Fatalf("process3 clear: %v", err)
	}
	if len(n3.calls) != 1 || n3.calls[0].method != "resolution" {
		t.Fatalf("process3 should post exactly one resolution, got %+v", n3.calls)
	}
	if got := n3.calls[0].threadTS; got != "2100.0001" {
		t.Errorf("post-restart resolution thread_ts = %q, want recovered root 2100.0001", got)
	}
	if sink.OpenCount() != 0 {
		t.Errorf("after clearance the issue should be closed; got %d open", sink.OpenCount())
	}
}

// TestEscalator_ThreadMarkerSurvivesRecurrence proves the durable thread marker is
// NOT clobbered when the body is regenerated on recurrence — continuity holds for
// any number of update cycles across restarts.
func TestEscalator_ThreadMarkerSurvivesRecurrence(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	if _, err := NewEscalatorWithNotifier(sink, &fakeNotifier{tsQueue: []string{"2200.0002"}}).
		Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "a"))}); err != nil {
		t.Fatalf("open: %v", err)
	}
	// Two more recurrences, each from a fresh escalator (restart) so the marker is
	// the only carrier of continuity.
	for i, msg := range []string{"b", "c"} {
		if _, err := NewEscalatorWithNotifier(sink, &fakeNotifier{}).
			Reconcile(ctx, []Subject{subjectFor(crashFinding(id, msg))}); err != nil {
			t.Fatalf("recur %d: %v", i, err)
		}
		view, _ := sink.Snapshot(IssueRef("1"))
		gotTS, ok := ParseThreadMarker(view.Body)
		if !ok || gotTS != "2200.0002" {
			t.Fatalf("recur %d: thread marker lost/clobbered: got %q ok=%v", i, gotTS, ok)
		}
		// The body must carry exactly one thread marker (no accumulation).
		if n := strings.Count(view.Body, threadMarkerPrefix); n != 1 {
			t.Fatalf("recur %d: want exactly 1 thread marker, got %d:\n%s", i, n, view.Body)
		}
	}
}

// TestEscalator_NopNotifierZeroChatCalls proves the unconfigured path: with the
// default no-op notifier the full lifecycle runs exactly as Milestone 1 — the
// GitHub trail is complete and NO thread marker is ever written (zero Slack
// behavior). NewEscalator (no notifier) must behave identically.
func TestEscalator_NopNotifierZeroChatCalls(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	esc := NewEscalator(sink) // no notifier => NopNotifier

	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	if _, err := esc.Reconcile(ctx, []Subject{subjectFor(crashFinding(id, "x"))}); err != nil {
		t.Fatalf("open: %v", err)
	}
	view, _ := sink.Snapshot(IssueRef("1"))
	// The no-op returns an empty ts, so the escalator persists no thread marker —
	// the GitHub body is byte-for-byte the M1 body.
	if _, ok := ParseThreadMarker(view.Body); ok {
		t.Errorf("no-op notifier must not cause a thread marker to be written:\n%s", view.Body)
	}
	if view.Body != Body(subjectFor(crashFinding(id, "x"))) {
		t.Errorf("no-op body must equal the plain diagnostic body, got:\n%s", view.Body)
	}
	// Lifecycle still completes against GitHub.
	if _, err := esc.Reconcile(ctx, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if sink.OpenCount() != 0 {
		t.Errorf("issue should be closed; got %d open", sink.OpenCount())
	}
}
