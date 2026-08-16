package trust

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The instants these tests are written around. attemptEnd is when the fixture
// lifecycle's execution finished, and it is what a window has to cover for that
// outcome to be held back — see [Quarantine.RecordLifecycle] on why the outcome's own
// instant is the one compared, not a clock read later.
var (
	windowStart = attemptEnd.Add(-time.Minute)
	windowUntil = attemptEnd.Add(5 * time.Minute)
)

// lastConvergence is when the newest execution in a [convergedHistory] reported success.
// A recurrence only demotes when it is diagnosed within [RecurrenceHorizon] of one, so
// every recurrence scenario below is written relative to this rather than to a literal.
func lastConvergence() time.Time {
	return base.Add(time.Duration(PromotionThreshold-1) * 24 * time.Hour)
}

// openWindow opens one window covering the fixture lifecycle, on the fixture shape's
// cluster.
func openWindow(t *testing.T, w *Windows) Window {
	t.Helper()
	win, err := w.Begin(shape.Cluster, "pod-kill experiment on payments", windowStart, windowUntil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return win
}

// quarantined wires a quarantine in front of a fresh in-memory ledger and returns all
// three, which is the shape every scenario below needs.
func quarantined(t *testing.T) (*Quarantine, *Ledger, *Windows) {
	t.Helper()
	l, w := NewMemory(), NewMemoryWindows()
	q, err := NewQuarantine(l, w)
	if err != nil {
		t.Fatalf("NewQuarantine: %v", err)
	}
	return q, l, w
}

// TestWindowActiveIsBoundedByBothEnds states the window's whole semantics in one table:
// it starts, and it stops at whichever comes first of the close and the ceiling.
//
// The ceiling row is the one that matters. A window nobody closed is the signature of a
// process that died mid-experiment, and without the ceiling it would quarantine the
// cluster's trust history forever — the silent indefinite stall this project has already
// paid for twice.
func TestWindowActiveIsBoundedByBothEnds(t *testing.T) {
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	until := start.Add(10 * time.Minute)
	open := Window{Cluster: "prod", Reason: "experiment", Start: start, Until: until}
	closed := open
	closed.End = start.Add(2 * time.Minute)

	for _, tc := range []struct {
		name   string
		win    Window
		at     time.Time
		active bool
	}{
		{"before it starts", open, start.Add(-time.Second), false},
		{"at the start", open, start, true},
		{"inside", open, start.Add(5 * time.Minute), true},
		{"at the ceiling", open, until, false},
		{"past the ceiling, never closed", open, until.Add(time.Hour), false},
		{"inside, but already closed", closed, start.Add(5 * time.Minute), false},
		{"before the close", closed, start.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.win.Active(tc.at); got != tc.active {
				t.Errorf("Active(%s) = %v, want %v", tc.at.Format(time.RFC3339), got, tc.active)
			}
		})
	}

	if !open.Expired(until.Add(time.Hour)) {
		t.Error("a window past its ceiling that nobody closed must report as expired; that is a process that died mid-experiment")
	}
	if closed.Expired(until.Add(time.Hour)) {
		t.Error("a window that was closed is not expired, however long ago its ceiling passed")
	}
}

// TestBeginRefusesAWindowThatExplainsNothing covers the three malformed windows, each of
// which would leave a gap in the ledger that the record cannot account for.
func TestBeginRefusesAWindowThatExplainsNothing(t *testing.T) {
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name            string
		cluster, reason string
		start, until    time.Time
		wantErrContains string
	}{
		{"no cluster", "", "experiment", at, at.Add(time.Minute), "must name the cluster"},
		{"no reason", "prod", "  ", at, at.Add(time.Minute), "must state why"},
		{"ceiling before start", "prod", "experiment", at, at.Add(-time.Minute), "not after its start"},
		{"ceiling equals start", "prod", "experiment", at, at, "not after its start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewMemoryWindows()
			if _, err := w.Begin(tc.cluster, tc.reason, tc.start, tc.until); err == nil {
				t.Fatal("want an error")
			} else if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error %q must contain %q", err, tc.wantErrContains)
			}
			if got := w.All(); len(got) != 0 {
				t.Errorf("a refused window must not be recorded, got %v", got)
			}
		})
	}
}

// TestWindowIsRecoverableFromTheRecordAlone is the human's requirement from choice A,
// executed: "was the ledger quarantined when this happened?" has to be answerable from
// the record alone, by someone reading it later with no memory of the run.
//
// So the assertion is deliberately made against a SECOND process's view — a fresh
// [OpenWindows] over the same file — rather than against the in-memory object that wrote
// it. All four facts the human named must survive: a start, an end, a cluster and a
// reason.
func TestWindowIsRecoverableFromTheRecordAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.jsonl"+".chaos-windows")
	w, err := OpenWindows(path)
	if err != nil {
		t.Fatalf("OpenWindows: %v", err)
	}
	win, err := w.Begin("prod", "pod-kill experiment on payments", windowStart, windowUntil)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	closedAt := windowStart.Add(90 * time.Second)
	if _, err := w.End(win, closedAt); err != nil {
		t.Fatalf("End: %v", err)
	}

	reread, err := OpenWindows(path)
	if err != nil {
		t.Fatalf("reopening the window log: %v", err)
	}
	all := reread.All()
	if len(all) != 1 {
		t.Fatalf("the reread log holds %d windows, want the one that was recorded: %v", len(all), all)
	}
	got := all[0]
	switch {
	case got.Cluster != "prod":
		t.Errorf("cluster = %q, want prod", got.Cluster)
	case !strings.Contains(got.Reason, "pod-kill experiment"):
		t.Errorf("reason = %q, want the experiment that opened it", got.Reason)
	case !got.Start.Equal(windowStart):
		t.Errorf("start = %s, want %s", got.Start, windowStart)
	case !got.End.Equal(closedAt):
		t.Errorf("end = %s, want %s", got.End, closedAt)
	case !got.Until.Equal(windowUntil):
		t.Errorf("ceiling = %s, want %s", got.Until, windowUntil)
	}

	// The file is a person's read as much as a program's, so the reason has to be in it
	// as words rather than as an identifier resolved elsewhere.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the window log: %v", err)
	}
	if !strings.Contains(string(body), "pod-kill experiment on payments") {
		t.Errorf("the window log must state the reason in words:\n%s", body)
	}
}

// TestUnclosedWindowSurvivesARestartAndStillExpires is the crash case. The window is
// opened, nothing closes it, and a later process reads it back: it must still be there
// (so the gap is explained) and must still stop being active at its ceiling (so the
// quarantine lifts without anybody).
func TestUnclosedWindowSurvivesARestartAndStillExpires(t *testing.T) {
	path := filepath.Join(t.TempDir(), "windows.jsonl")
	w, err := OpenWindows(path)
	if err != nil {
		t.Fatalf("OpenWindows: %v", err)
	}
	if _, err := w.Begin("prod", "experiment nobody closed", windowStart, windowUntil); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	reread, err := OpenWindows(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if !reread.Quarantined("prod", windowStart.Add(time.Second)) {
		t.Error("an unclosed window must still be in force after a restart, or the gap it explains is unexplained")
	}
	if reread.Quarantined("prod", windowUntil.Add(time.Second)) {
		t.Error("an unclosed window must stop at its ceiling; a quarantine that never lifts silently stops the ledger learning anything")
	}
	if !reread.All()[0].Expired(windowUntil.Add(time.Second)) {
		t.Error("the reread window must report as expired, which is what tells a human a process died mid-experiment")
	}
}

// TestQuarantineHoldsBackOutcomesInsideTheWindow is choice A's core requirement: an
// outcome recorded during an active chaos window does not enter the ledger.
func TestQuarantineHoldsBackOutcomesInsideTheWindow(t *testing.T) {
	q, ledger, windows := quarantined(t)
	openWindow(t, windows)

	if err := q.RecordLifecycle(converged().records()); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	if n := ledger.Len(); n != 0 {
		t.Fatalf("the ledger holds %d entries; an outcome inside a chaos window must not enter it", n)
	}

	drops := q.Dropped()
	if len(drops) != 1 {
		t.Fatalf("the quarantine reported %d drops, want the one it held back: %v", len(drops), drops)
	}
	if !strings.Contains(drops[0].String(), "pod-kill experiment") {
		t.Errorf("a drop must carry the window that explains it, got %q", drops[0])
	}
}

// TestQuarantineAdmitsOutcomesOutsideTheWindow is the other half, and the one that keeps
// the quarantine from being a permanent hole in the history.
func TestQuarantineAdmitsOutcomesOutsideTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name         string
		start, until time.Time
	}{
		{"window ends before the outcome", attemptEnd.Add(-time.Hour), attemptEnd.Add(-time.Minute)},
		{"window starts after the outcome", attemptEnd.Add(time.Minute), attemptEnd.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, ledger, windows := quarantined(t)
			if _, err := windows.Begin(shape.Cluster, "an experiment at another time", tc.start, tc.until); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if err := q.RecordLifecycle(converged().records()); err != nil {
				t.Fatalf("RecordLifecycle: %v", err)
			}
			if n := ledger.Len(); n != 1 {
				t.Fatalf("the ledger holds %d entries, want the outcome that fell outside the window", n)
			}
			if drops := q.Dropped(); len(drops) != 0 {
				t.Errorf("nothing should have been dropped, got %v", drops)
			}
		})
	}
}

// TestQuarantineJudgesTheOutcomesOwnInstant pins the choice that makes the answer stable:
// the window is compared against when the execution FINISHED, not against when somebody
// got round to recording it.
//
// The scenario is the ordinary one, not an exotic one — a window that closes while a
// lifecycle is still being assembled — and the wrong reading would admit an outcome that
// happened while the cluster was under a deliberate fault, which is exactly the evidence
// the quarantine exists to exclude.
func TestQuarantineJudgesTheOutcomesOwnInstant(t *testing.T) {
	q, ledger, windows := quarantined(t)
	win := openWindow(t, windows)
	// The window closes one second after the execution finished. Read against "now" — any
	// instant after this close — the outcome would look admissible.
	if _, err := windows.End(win, attemptEnd.Add(time.Second)); err != nil {
		t.Fatalf("End: %v", err)
	}

	if err := q.RecordLifecycle(converged().records()); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	if n := ledger.Len(); n != 0 {
		t.Fatalf("the ledger holds %d entries; the outcome finished while the window was open, so it is not evidence", n)
	}
}

// TestQuarantineHoldsBackRecurrences is the case the quarantine was actually built for.
//
// A recurrence means "MaKlaude said it fixed this and the fault is back within the
// horizon", which is precisely what an experiment produces on purpose. Admitting it would
// demote the shape — and demotion is (cluster, operation)-shaped, so one by-design
// failure re-gates every fingerprint of that shape on the cluster.
func TestQuarantineHoldsBackRecurrences(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	w := NewMemoryWindows()
	q, err := NewQuarantine(l, w)
	if err != nil {
		t.Fatalf("NewQuarantine: %v", err)
	}

	trustedBefore := l.Trust(autonomy.Subject{Shape: shape, Fingerprint: fixtureFP}).Trusted
	if !trustedBefore {
		t.Fatal("the fixture history must be trusted before the experiment, or this test proves nothing")
	}

	// The fault is diagnosed again minutes after the last execution reported it fixed,
	// which is inside [RecurrenceHorizon] and is what would ordinarily demote the shape.
	now := lastConvergence().Add(5 * time.Minute)
	if _, err := w.Begin(shape.Cluster, "pod-kill experiment on payments", now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := q.NoteRecurrence(recurringIdentity, shape, now); err != nil {
		t.Fatalf("NoteRecurrence: %v", err)
	}

	if !l.Trust(autonomy.Subject{Shape: shape, Fingerprint: fixtureFP}).Trusted {
		t.Error("a fault MaKlaude caused on purpose must not demote the shape; that is chaos erasing the trust M5 accumulates")
	}
	if drops := q.Dropped(); len(drops) != 1 {
		t.Fatalf("the held-back recurrence must be reported, got %v", drops)
	}
}

// TestQuarantineAdmitsRecurrencesOutsideTheWindow proves the demotion machinery is
// untouched: the same call, with no window, still demotes.
func TestQuarantineAdmitsRecurrencesOutsideTheWindow(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	q, err := NewQuarantine(l, NewMemoryWindows())
	if err != nil {
		t.Fatalf("NewQuarantine: %v", err)
	}

	before := l.Len()
	if err := q.NoteRecurrence(recurringIdentity, shape, lastConvergence().Add(5*time.Minute)); err != nil {
		t.Fatalf("NoteRecurrence: %v", err)
	}
	if l.Len() != before+1 {
		t.Errorf("with no window in force the recurrence must be recorded: ledger went from %d to %d entries", before, l.Len())
	}
}

// TestQuarantineNeverManufacturesTrust states the asymmetry the type's doc claims, which
// is what makes it safe to wire unconditionally: it can withhold evidence and it can
// never add any.
func TestQuarantineNeverManufacturesTrust(t *testing.T) {
	q, ledger, windows := quarantined(t)
	openWindow(t, windows)

	if err := q.RecordLifecycle(converged().records()); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	if ev := ledger.Trust(autonomy.Subject{Shape: shape, Fingerprint: fixtureFP}); ev.Trusted {
		t.Error("a quarantined outcome must not make a shape trusted; the quarantine only ever withholds")
	}
}

// TestQuarantinePassesThroughLifecyclesWithNoExecution keeps the drop list honest: a
// lifecycle the ledger would ignore anyway must not be reported as something a window
// held back.
func TestQuarantinePassesThroughLifecyclesWithNoExecution(t *testing.T) {
	q, _, windows := quarantined(t)
	openWindow(t, windows)

	if err := q.RecordLifecycle(nil); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	if drops := q.Dropped(); len(drops) != 0 {
		t.Errorf("a lifecycle with no execution behind it is not an outcome the window swallowed, got %v", drops)
	}
}

// TestQuarantineForwardsStanding covers the optional interface the cycle uses to report
// recurrences in words. A recorder that stopped answering once it was wrapped would lose
// that prose silently.
func TestQuarantineForwardsStanding(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	q, err := NewQuarantine(l, NewMemoryWindows())
	if err != nil {
		t.Fatalf("NewQuarantine: %v", err)
	}
	if got, want := q.Standing(autonomy.Subject{Shape: shape}).Recorded, l.Standing(autonomy.Subject{Shape: shape}).Recorded; got != want {
		t.Errorf("Standing through the quarantine = %d, want the ledger's own %d", got, want)
	}
}

// TestNewQuarantineRefusesAHalfWiring covers the two constructions that would look like a
// working quarantine and behave like a hole.
func TestNewQuarantineRefusesAHalfWiring(t *testing.T) {
	if _, err := NewQuarantine(nil, NewMemoryWindows()); err == nil {
		t.Error("a quarantine with no recorder behind it would drop every outcome and blame the window log")
	}
	if _, err := NewQuarantine(NewMemory(), nil); err == nil {
		t.Error("a quarantine with no window log could never record the gap it creates")
	}
}

// TestOverlappingWindowsBothHoldAndBothAreReported covers two experiments running at once
// on one cluster: the quarantine lifts only when every window covering the instant has.
func TestOverlappingWindowsBothHoldAndBothAreReported(t *testing.T) {
	_, _, windows := quarantined(t)
	first, err := windows.Begin("prod", "first experiment", windowStart, windowStart.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := windows.Begin("prod", "second experiment", windowStart.Add(time.Minute), windowStart.Add(10*time.Minute)); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	at := windowStart.Add(90 * time.Second)
	if got := windows.Active("prod", at); len(got) != 2 {
		t.Fatalf("both windows cover %s, got %d: %v", at, len(got), got)
	}
	if _, err := windows.End(first, windowStart.Add(95*time.Second)); err != nil {
		t.Fatalf("End: %v", err)
	}
	if !windows.Quarantined("prod", windowStart.Add(2*time.Minute)) {
		t.Error("closing one of two overlapping windows must not lift the quarantine the other one holds")
	}
}

// TestWindowsAreScopedToTheirCluster is multi-cluster isolation, which is a first-class
// property everywhere else in this system and would be a quiet way to lose it here: an
// experiment on staging must not stop production learning from its own outcomes.
func TestWindowsAreScopedToTheirCluster(t *testing.T) {
	q, ledger, windows := quarantined(t)
	if _, err := windows.Begin("staging", "experiment on another cluster", windowStart, windowUntil); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := q.RecordLifecycle(converged().records()); err != nil {
		t.Fatalf("RecordLifecycle: %v", err)
	}
	if ledger.Len() != 1 {
		t.Errorf("the fixture outcome is on %s and the window is on staging, so it must be recorded", shape.Cluster)
	}
}

// TestEndRefusesAWindowItNeverSaw and the double-close case: the first is a caller and a
// record that disagree, the second is a deferred close racing an explicit one and must be
// harmless.
func TestEndIsIdempotentAndRefusesUnknownWindows(t *testing.T) {
	w := NewMemoryWindows()
	win := openWindow(t, w)

	closed, err := w.End(win, windowStart.Add(time.Minute))
	if err != nil {
		t.Fatalf("End: %v", err)
	}
	again, err := w.End(win, windowStart.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("closing an already-closed window must be a no-op, got %v", err)
	}
	if !again.End.Equal(closed.End) {
		t.Errorf("the second close moved the end from %s to %s; a deferred close must not rewrite the record", closed.End, again.End)
	}

	unknown := Window{Cluster: "prod", Start: windowStart.Add(time.Hour), Until: windowUntil.Add(time.Hour)}
	if _, err := w.End(unknown, windowStart); err == nil {
		t.Error("closing a window the log never saw means the caller and the record disagree, which must not pass silently")
	}
}

// TestOpenWindowsRefusesACorruptLog: an unreadable log must fail loudly rather than
// present as "nothing was ever quarantined", which would let chaos outcomes into the
// ledger while a human believes they are being held out.
func TestOpenWindowsRefusesACorruptLog(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"not json", "{oh no\n"},
		{"unknown event", `{"event":"maybe","window":{"Cluster":"prod","Start":"2026-08-01T12:00:00Z","Until":"2026-08-01T12:10:00Z"}}` + "\n"},
		{"no cluster", `{"event":"opened","window":{"Start":"2026-08-01T12:00:00Z","Until":"2026-08-01T12:10:00Z"}}` + "\n"},
		{"no ceiling", `{"event":"opened","window":{"Cluster":"prod","Start":"2026-08-01T12:00:00Z"}}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "windows.jsonl")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("writing the fixture: %v", err)
			}
			if _, err := OpenWindows(path); err == nil {
				t.Error("a corrupt window log must be an error, not an empty history")
			}
		})
	}
}

// TestQuarantineIsARecorder is the drop-in property, stated as a compile-time-ish check a
// reader can find: a cycle wired with either behaves identically apart from what the
// quarantine withholds.
func TestQuarantineIsARecorder(t *testing.T) {
	var r Recorder
	q, _, _ := quarantined(t)
	r = q
	if err := r.NoteRecurrence(testIdentity, shape, attemptEnd); err != nil {
		t.Fatalf("NoteRecurrence through the interface: %v", err)
	}
	r = NewMemory()
	if err := r.RecordLifecycle([]audit.Record{}); err != nil {
		t.Fatalf("RecordLifecycle through the interface: %v", err)
	}
	_ = remediate.ProposalIdentity("")
}
