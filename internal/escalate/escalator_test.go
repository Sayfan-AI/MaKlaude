package escalate

import (
	"context"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// TestEscalator_OpenThenRecurThenClear walks the full lifecycle through the
// public Escalator API against a MemorySink, proving the three done-criteria:
// a detected problem opens a well-formed issue; recurrence updates rather than
// duplicates; clearance closes the trail with a comment.
func TestEscalator_OpenThenRecurThenClear(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	esc := NewEscalator(sink)

	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	f := func(sev detect.Severity, msg string) detect.Finding {
		return detect.Finding{
			Identity: id, Severity: sev, Cluster: "prod",
			Object: detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
			Title:  "Pod crashlooping", Message: msg, DetectedAt: ts,
		}
	}

	// Cycle 1: problem first detected -> one issue opened.
	out, err := esc.Reconcile(ctx, []detect.Finding{f(detect.SeverityWarning, "1 restart")})
	if err != nil {
		t.Fatalf("cycle1: %v", err)
	}
	if out.Opened != 1 || out.Updated != 0 || out.Closed != 0 {
		t.Fatalf("cycle1 outcome = %v, want opened=1", out)
	}
	if sink.OpenCount() != 1 {
		t.Fatalf("cycle1: want 1 open issue, got %d", sink.OpenCount())
	}

	// Find the created issue and assert it is well-formed.
	ref := IssueRef("1")
	view, ok := sink.Snapshot(ref)
	if !ok {
		t.Fatal("expected issue #1 to exist")
	}
	if !strings.Contains(view.Title, "prod") || !strings.Contains(view.Title, "Pod crashlooping") {
		t.Errorf("title not well-formed: %q", view.Title)
	}
	gotID, ok := ParseIdentityMarker(view.Body)
	if !ok || gotID != id {
		t.Errorf("body marker = %q ok=%v, want %q", gotID, ok, id)
	}
	if !hasLabel(view.Labels, ManagedLabel) || !hasLabel(view.Labels, NeedsHumanLabel) {
		t.Errorf("labels = %v, want both %q and %q", view.Labels, ManagedLabel, NeedsHumanLabel)
	}

	// Cycle 2: SAME problem, worsened -> update + recurrence comment, NO new issue.
	out, err = esc.Reconcile(ctx, []detect.Finding{f(detect.SeverityCritical, "12 restarts")})
	if err != nil {
		t.Fatalf("cycle2: %v", err)
	}
	if out.Updated != 1 || out.Opened != 0 {
		t.Fatalf("cycle2 outcome = %v, want updated=1 opened=0", out)
	}
	if sink.OpenCount() != 1 {
		t.Fatalf("cycle2: recurrence must not open a duplicate; got %d open", sink.OpenCount())
	}
	view, _ = sink.Snapshot(ref)
	if len(view.Comments) != 1 || !strings.Contains(view.Comments[0], "12 restarts") {
		t.Errorf("cycle2: expected one recurrence comment with latest message, got %v", view.Comments)
	}
	if !strings.Contains(view.Body, "12 restarts") {
		t.Errorf("cycle2: body should refresh to latest message, got %q", view.Body)
	}

	// Cycle 3: problem cleared (no findings) -> close with a closing comment.
	out, err = esc.Reconcile(ctx, nil)
	if err != nil {
		t.Fatalf("cycle3: %v", err)
	}
	if out.Closed != 1 {
		t.Fatalf("cycle3 outcome = %v, want closed=1", out)
	}
	if sink.OpenCount() != 0 {
		t.Fatalf("cycle3: cleared problem must close; got %d open", sink.OpenCount())
	}
	view, _ = sink.Snapshot(ref)
	if view.Open {
		t.Error("cycle3: issue should be closed")
	}
	if len(view.Comments) != 2 || !strings.Contains(view.Comments[1], "cleared") {
		t.Errorf("cycle3: expected a closing comment, got %v", view.Comments)
	}

	// Cycle 4: nothing detected, nothing tracked -> no-op.
	out, err = esc.Reconcile(ctx, nil)
	if err != nil {
		t.Fatalf("cycle4: %v", err)
	}
	if out != (Outcome{}) {
		t.Fatalf("cycle4 should be a no-op, got %v", out)
	}
}

// TestEscalator_RediscoversAcrossRestart proves the escalator does not rely on
// in-memory state: a fresh Escalator over the same sink correctly sees the
// existing issue (via the body marker) and updates rather than duplicates.
func TestEscalator_RediscoversAcrossRestart(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()

	id := detect.Identity("prod|node.notready|node/n1")
	f := detect.Finding{
		Identity: id, Severity: detect.SeverityCritical, Cluster: "prod",
		Object: detect.Object{Kind: "node", Name: "n1"}, Title: "Node NotReady",
		Message: "node n1 not ready", DetectedAt: ts,
	}

	// First "process" opens the issue.
	if _, err := NewEscalator(sink).Reconcile(ctx, []detect.Finding{f}); err != nil {
		t.Fatal(err)
	}
	// A brand-new escalator (simulating a restart) sees the same finding.
	out, err := NewEscalator(sink).Reconcile(ctx, []detect.Finding{f})
	if err != nil {
		t.Fatal(err)
	}
	if out.Updated != 1 || out.Opened != 0 {
		t.Fatalf("post-restart outcome = %v, want updated=1 opened=0 (no duplicate)", out)
	}
	if sink.OpenCount() != 1 {
		t.Fatalf("post-restart: want 1 open issue, got %d", sink.OpenCount())
	}
}

// TestEscalator_InfoFindingNotFlaggedForHuman proves info findings are tracked
// but not labelled needs:human.
func TestEscalator_InfoFindingNotFlaggedForHuman(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	f := detect.Finding{
		Identity: "prod|event.warning|pod/ns/x", Severity: detect.SeverityInfo, Cluster: "prod",
		Object: detect.Object{Kind: "pod", Namespace: "ns", Name: "x"}, Title: "Warning event",
		Message: "something", DetectedAt: ts,
	}
	if _, err := NewEscalator(sink).Reconcile(ctx, []detect.Finding{f}); err != nil {
		t.Fatal(err)
	}
	view, _ := sink.Snapshot("1")
	if !hasLabel(view.Labels, ManagedLabel) {
		t.Errorf("info issue should still be managed: %v", view.Labels)
	}
	if hasLabel(view.Labels, NeedsHumanLabel) {
		t.Errorf("info issue should NOT be needs:human: %v", view.Labels)
	}
}

// TestEscalator_MultiClusterIsolation proves two clusters' problems live in
// separate issues and one cluster's reconcile (when all findings are passed
// together) never disturbs the other's.
func TestEscalator_MultiClusterIsolation(t *testing.T) {
	ctx := context.Background()
	sink := NewMemorySink()
	esc := NewEscalator(sink)

	mk := func(cluster string) detect.Finding {
		id := detect.Identity(cluster + "|pod.crashloop|pod/ns/a")
		return detect.Finding{
			Identity: id, Severity: detect.SeverityCritical, Cluster: cluster,
			Object: detect.Object{Kind: "pod", Namespace: "ns", Name: "a"},
			Title:  "Pod crashlooping", Message: "x", DetectedAt: ts,
		}
	}

	// Both clusters have the problem.
	if _, err := esc.Reconcile(ctx, []detect.Finding{mk("prod"), mk("staging")}); err != nil {
		t.Fatal(err)
	}
	if sink.OpenCount() != 2 {
		t.Fatalf("want 2 distinct issues, got %d", sink.OpenCount())
	}

	// prod clears, staging persists (both passed together so isolation holds).
	out, err := esc.Reconcile(ctx, []detect.Finding{mk("staging")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Closed != 1 || out.Updated != 1 {
		t.Fatalf("outcome = %v, want closed=1 (prod) updated=1 (staging)", out)
	}
	if sink.OpenCount() != 1 {
		t.Fatalf("want 1 open issue after prod cleared, got %d", sink.OpenCount())
	}
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
