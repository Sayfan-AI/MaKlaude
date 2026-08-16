package trust

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file keeps deliberate faults out of the trust history, and records that it did.
//
// # Why the ledger has to be quarantined at all
//
// Two of this package's rules are individually correct and together describe exactly
// what a chaos experiment does on purpose:
//
//   - [RecurrenceHorizon] demotes a shape when the same fault is re-diagnosed on the
//     same object within an hour of a reported convergence. An experiment re-breaks
//     something MaKlaude just fixed; that is the point of running it.
//   - Demotion is (cluster, operation)-shaped across the last [DemotionScope] entries,
//     so a handful of by-design failures on one cluster re-gates every fingerprint of
//     that shape on it.
//
// So without a quarantine, running chaos would systematically erase the trust
// Milestone 5 exists to accumulate: the better the experiments, the less autonomy
// MaKlaude would ever earn. Neither rule is wrong and neither is being weakened. What
// changes is which outcomes are admissible as evidence.
//
// # Why the window is an object and not a flag
//
// A boolean "chaos is running" would suppress the writes and leave nothing behind, and
// a trail with a silent gap is the failure mode this project keeps rediscovering — a
// stale gate nobody noticed for 21 days, a stderr fallback that produced zero lines. So
// the requirement the human set is that the window itself is recorded, and that
//
//	"was the ledger quarantined when this happened?"
//
// is answerable from the record alone, by someone reading it a year later with no
// memory of the run. A [Window] has a cluster, a reason, a start, a declared ceiling and
// an actual end, and all five are on disk.
//
// # Why a window declares a ceiling
//
// An open window with no bound is the stall this system has already paid for twice: if
// the process dies between injecting a fault and closing the window, a quarantine with
// no ceiling never lifts, and the symptom is that MaKlaude quietly stops learning from
// every outcome on that cluster, forever, with nothing red anywhere. So [Windows.Begin]
// requires an Until, and the caller supplies it because the caller is the only party
// that knows both halves of the arithmetic — the fault's own duration and how long the
// cluster needs to settle afterwards. A window past its ceiling is not active; it is
// EXPIRED, which is a distinct state the record shows and a report can name.

// Window is one recorded period during which outcomes on one cluster were not
// admissible as trust evidence.
//
// The zero value is not a window: [Windows.Begin] refuses an empty cluster or reason,
// so a window in the record always says what it covered and why.
type Window struct {
	// Cluster is the registered cluster whose outcomes were quarantined. A window is
	// per-cluster because chaos eligibility is per-cluster, and quarantining a cluster
	// nobody is breaking would discard evidence for no reason.
	Cluster string

	// Reason is why the quarantine was opened, in words. It is required: a gap in the
	// history explained only by its own existence is the thing this type is meant to
	// prevent.
	Reason string

	// Start is when the quarantine opened (UTC).
	Start time.Time

	// Until is the declared ceiling: the instant after which this window stops being
	// active whether or not anything closed it. See the file doc for why it is
	// mandatory.
	Until time.Time

	// End is when the quarantine was actually closed (UTC), zero if it never was.
	//
	// It is a separate fact from Until rather than a correction to it, and both are
	// kept: "declared until 14:12, closed at 14:07" and "declared until 14:12, never
	// closed" are different histories, and the second is the signature of a process
	// that died mid-experiment.
	End time.Time
}

// Active reports whether this window covers the given instant.
//
// A window is active from its start until whichever comes first: the instant it was
// closed, or its declared ceiling. The ceiling is what makes the answer bounded for a
// window nothing ever closed.
func (w Window) Active(at time.Time) bool {
	at = at.UTC()
	if at.Before(w.Start) {
		return false
	}
	if !w.End.IsZero() && !at.Before(w.End) {
		return false
	}
	return at.Before(w.Until)
}

// Expired reports whether this window passed its declared ceiling without being
// closed. It is the state a report should name, because it means a process did not
// finish what it started.
func (w Window) Expired(at time.Time) bool {
	return w.End.IsZero() && !at.UTC().Before(w.Until)
}

// String renders the window as one line for a report, a log, or a test failure.
func (w Window) String() string {
	end := "never closed"
	if !w.End.IsZero() {
		end = "closed " + stamp(w.End)
	}
	return fmt.Sprintf("%s: %s → until %s, %s (%s)", w.Cluster, stamp(w.Start), stamp(w.Until), end, w.Reason)
}

// key identifies one window in the append-only log. A cluster cannot have two windows
// starting at the same nanosecond, and the close event carries the same key so a
// reader can fold the two lines back into one window.
func (w Window) key() string { return w.Cluster + "@" + w.Start.UTC().Format(time.RFC3339Nano) }

// Windows is the recorded set of quarantine periods, optionally durable.
//
// It is safe for concurrent use. Like [Ledger] it holds its state in memory and
// appends each change to its file, so a process that dies has already written the
// window it opened — which matters more here than for most records: the window is the
// explanation for a gap in the ledger, and a gap whose explanation was lost in a buffer
// is worse than no gap at all.
type Windows struct {
	mu      sync.Mutex
	windows []Window
	byKey   map[string]int
	store   *windowStore
}

// NewMemoryWindows returns a window log with no file behind it. Used by tests and by a
// cycle that has no ledger to quarantine.
func NewMemoryWindows() *Windows {
	return &Windows{byKey: map[string]int{}}
}

// OpenWindows loads the window log at path, creating it if it does not exist.
//
// A file that cannot be read or parsed is an error rather than an empty log, for the
// same reason [Open] refuses to silently start from nothing: an empty log is
// indistinguishable from a fresh install, and here it would additionally mean "a
// quarantine that may still be in force is invisible", which would let chaos outcomes
// into the ledger while a human believes they are being held out.
func OpenWindows(path string) (*Windows, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("trust: no quarantine-window path was given")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("trust: preparing quarantine-window directory: %w", err)
	}
	events, err := loadWindowEvents(path)
	if err != nil {
		return nil, err
	}

	w := NewMemoryWindows()
	w.store = &windowStore{path: path}
	for _, ev := range events {
		w.apply(ev)
	}
	return w, nil
}

// Path reports the file backing this log, empty for an in-memory one.
func (w *Windows) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.store == nil {
		return ""
	}
	return w.store.path
}

// Begin opens a quarantine window on one cluster and records it.
//
// It refuses an empty cluster, an empty reason, and a ceiling that is not after the
// start — the last of those because a window that is over before it begins would read
// as a quarantine in the record while admitting every outcome, which is the one
// combination a reader could not detect.
//
// A second window on a cluster that already has an active one is not an error: it is
// recorded, and the cluster stays quarantined until every window covering the instant
// has ended. Two overlapping experiments on one cluster is a thing an operator can do,
// and refusing the second window would leave its outcomes admissible.
func (w *Windows) Begin(cluster, reason string, at, until time.Time) (Window, error) {
	switch {
	case strings.TrimSpace(cluster) == "":
		return Window{}, fmt.Errorf("trust: a quarantine window must name the cluster it covers")
	case strings.TrimSpace(reason) == "":
		return Window{}, fmt.Errorf("trust: a quarantine window must state why it was opened, "+
			"because it is the explanation for a gap in %s's history", cluster)
	case !until.UTC().After(at.UTC()):
		return Window{}, fmt.Errorf("trust: quarantine window on %s ends at %s, which is not after its start %s; "+
			"a window that is over before it opens would suppress nothing while reading as a quarantine",
			cluster, stamp(until), stamp(at))
	}

	win := Window{Cluster: cluster, Reason: strings.TrimSpace(reason), Start: at.UTC(), Until: until.UTC()}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, dup := w.byKey[win.key()]; dup {
		// Same cluster, same nanosecond: this is the same window recorded twice, not two
		// of them. It collapses rather than being stored twice, so a replayed call and a
		// re-read file both produce one window.
		return w.windows[w.byKey[win.key()]], nil
	}
	if err := w.record(windowEvent{Event: eventOpened, Window: win}); err != nil {
		return Window{}, err
	}
	return win, nil
}

// End closes the given window and records the instant.
//
// Closing a window that is already closed is a no-op that returns the recorded window,
// so a caller with a deferred close and an explicit one cannot corrupt the record by
// running both. Closing a window this log has never seen is an error, because it means
// the caller and the record disagree about what happened.
func (w *Windows) End(win Window, at time.Time) (Window, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	i, ok := w.byKey[win.key()]
	if !ok {
		return Window{}, fmt.Errorf("trust: no quarantine window on %s started at %s, so there is nothing to close",
			win.Cluster, stamp(win.Start))
	}
	if !w.windows[i].End.IsZero() {
		return w.windows[i], nil
	}

	closed := w.windows[i]
	closed.End = at.UTC()
	if err := w.record(windowEvent{Event: eventClosed, Window: closed}); err != nil {
		return Window{}, err
	}
	return closed, nil
}

// Active reports the windows covering the given instant on one cluster.
//
// It returns every one rather than the first, because a report that says "quarantined"
// should be able to say by what: two overlapping experiments are two explanations for
// the same gap, and dropping one loses the reason a reader would need.
func (w *Windows) Active(cluster string, at time.Time) []Window {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []Window
	for _, win := range w.windows {
		if win.Cluster == cluster && win.Active(at) {
			out = append(out, win)
		}
	}
	return out
}

// Quarantined reports whether any window covers the given instant on one cluster.
func (w *Windows) Quarantined(cluster string, at time.Time) bool {
	return len(w.Active(cluster, at)) > 0
}

// All returns every recorded window, oldest first. This is the read a human does when
// they want to know what was quarantined and why.
func (w *Windows) All() []Window {
	w.mu.Lock()
	defer w.mu.Unlock()

	out := append([]Window(nil), w.windows...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// record applies one event to memory and appends it to the file. The order is
// deliberate: the durable write happens first, so a failed append never leaves memory
// claiming a window the record does not have.
func (w *Windows) record(ev windowEvent) error {
	if w.store != nil {
		if err := w.store.append(ev); err != nil {
			return fmt.Errorf("trust: recording quarantine window on %s: %w", ev.Window.Cluster, err)
		}
	}
	w.apply(ev)
	return nil
}

// apply folds one event into the in-memory set.
func (w *Windows) apply(ev windowEvent) {
	key := ev.Window.key()
	if i, ok := w.byKey[key]; ok {
		w.windows[i] = ev.Window
		return
	}
	w.byKey[key] = len(w.windows)
	w.windows = append(w.windows, ev.Window)
}

// Event tokens for the window log. They are strings on the wire so a person reading the
// file sees what happened rather than an integer.
const (
	eventOpened = "opened"
	eventClosed = "closed"
)

// windowEvent is one line of the window log: what happened, and the window as it stood
// after it. Storing the whole window on both lines rather than a delta means any single
// line is readable on its own, and the close line is a complete record of the window.
type windowEvent struct {
	Event  string `json:"event"`
	Window Window `json:"window"`
}

// windowStore is the durable backing for [Windows]: a file it appends event lines to.
// It holds no handle between calls, for the reasons [store] does not.
type windowStore struct{ path string }

// append writes one event and flushes it to stable storage.
func (s *windowStore) append(ev windowEvent) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// loadWindowEvents reads every event from the file, in order. A missing file is an
// empty history, which is the correct reading of "nothing has ever been quarantined".
func loadWindowEvents(path string) ([]windowEvent, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: opening quarantine-window log %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var events []windowEvent
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var ev windowEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, fmt.Errorf("trust: quarantine-window log %s line %d: %w", path, line, err)
		}
		switch {
		case ev.Event != eventOpened && ev.Event != eventClosed:
			return nil, fmt.Errorf("trust: quarantine-window log %s line %d: unknown event %q", path, line, ev.Event)
		case ev.Window.Cluster == "":
			return nil, fmt.Errorf("trust: quarantine-window log %s line %d: window names no cluster", path, line)
		case ev.Window.Start.IsZero() || ev.Window.Until.IsZero():
			return nil, fmt.Errorf("trust: quarantine-window log %s line %d: window on %s has no start or no ceiling, "+
				"so what it covered cannot be established", path, line, ev.Window.Cluster)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("trust: reading quarantine-window log %s: %w", path, err)
	}
	return events, nil
}

// Recorder is the trust ledger's write side: the two methods a cycle uses to feed
// history in.
//
// It exists so [Quarantine] can wrap a *[Ledger] — or anything else with the same two
// methods, including a test double that fails on demand — without either side knowing
// about the cycle's own narrowed interface. *[Ledger] and *[Quarantine] both satisfy
// it, which is what makes the quarantine a drop-in.
type Recorder interface {
	// RecordLifecycle projects one action's audit lifecycle onto an entry and records it.
	RecordLifecycle(recs []audit.Record) error

	// NoteRecurrence records that a converged execution did not hold.
	NoteRecurrence(identity remediate.ProposalIdentity, shape autonomy.Shape, now time.Time) error
}

// Drop is one outcome that was not admitted into the ledger, and the window that
// explains it.
//
// The drops are kept rather than merely counted because of who asks. "Why did this
// shape not promote?" is answered by the window; "which outcomes did the window
// swallow?" is answered by this, and an operator debugging a cluster whose autonomy
// never advances needs both. It is in memory only: the durable record of the gap is the
// window, and re-deriving which entries fell inside it is a matter of comparing the
// disclosures against the window log.
type Drop struct {
	// Cluster is the cluster whose outcome was dropped.
	Cluster string

	// Detail names what was dropped, in the vocabulary of the thing that was dropped:
	// an entry key for a lifecycle, a proposal identity for a recurrence.
	Detail string

	// Window is the window that was active when it happened.
	Window Window
}

// String renders one drop as a line for a report.
func (d Drop) String() string {
	return fmt.Sprintf("%s: %s was not recorded — the trust ledger was quarantined (%s)", d.Cluster, d.Detail, d.Window)
}

// Quarantine wraps a [Recorder] and holds back the outcomes that fall inside a chaos
// window, so a deliberate fault cannot demote the shapes MaKlaude has earned autonomy
// on.
//
// It suppresses writes and nothing else. It is not an oracle, it does not hide history
// that is already recorded, and it never makes a shape look MORE trusted than the
// ledger says: [Ledger.Trust] is untouched, so a quarantine can only ever withhold
// evidence, never manufacture it. That asymmetry is the reason this is safe to wire
// unconditionally — the worst a bug in here can do is fail to demote something, which
// is a bounded harm compared with the alternative it replaces (chaos silently erasing
// every shape's standing).
//
// It is safe for concurrent use.
type Quarantine struct {
	inner   Recorder
	windows *Windows

	mu      sync.Mutex
	dropped []Drop
}

// NewQuarantine wraps a recorder with the given window log.
//
// A nil inner recorder is an error rather than a silent no-op: a cycle wired with a
// quarantine and no ledger behind it would drop every outcome and report that the
// quarantine did it, which would look like chaos was running when nothing was.
func NewQuarantine(inner Recorder, windows *Windows) (*Quarantine, error) {
	if inner == nil {
		return nil, fmt.Errorf("trust: a quarantine needs a recorder to wrap; with nothing behind it every " +
			"outcome would be dropped and the window log would be blamed for it")
	}
	if windows == nil {
		return nil, fmt.Errorf("trust: a quarantine needs a window log; with none, no window can ever be " +
			"recorded and the gap it explains would be unexplained")
	}
	return &Quarantine{inner: inner, windows: windows}, nil
}

// Windows returns the window log behind this quarantine, so a caller can open and close
// windows without holding a second reference.
func (q *Quarantine) Windows() *Windows { return q.windows }

// Dropped returns the outcomes this quarantine held back, oldest first.
func (q *Quarantine) Dropped() []Drop {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Drop(nil), q.dropped...)
}

// RecordLifecycle records the outcome unless a window was active when it FINISHED.
//
// The instant compared against the window is the execution's own, taken from the
// derived entry, not a wall clock read now. That is what makes the answer stable: the
// question is whether the cluster was under a deliberate fault when this outcome
// happened, and re-deriving it from "now" would give a different answer depending on
// how long the caller took to hand the lifecycle over — and would admit an outcome that
// occurred mid-experiment simply because the window closed before it was recorded.
//
// A lifecycle with no execution behind it is passed straight through. The inner
// recorder treats it as a no-op, and inventing a drop for it would put an outcome in
// the report that never existed.
func (q *Quarantine) RecordLifecycle(recs []audit.Record) error {
	e, err := EntryFrom(recs)
	if err != nil {
		return q.inner.RecordLifecycle(recs)
	}
	active := q.windows.Active(e.Shape.Cluster, e.At)
	if len(active) == 0 {
		return q.inner.RecordLifecycle(recs)
	}
	q.drop(Drop{Cluster: e.Shape.Cluster, Detail: "outcome " + e.Outcome.String() + " of " + string(e.Identity), Window: active[0]})
	return nil
}

// NoteRecurrence records the regression unless a window is active now.
//
// This is the case the quarantine exists for. A recurrence means "MaKlaude said it
// fixed this and the fault is back", which is precisely what an experiment produces on
// purpose — re-breaking something that was just repaired — so admitting it would demote
// the shape for doing exactly what the operator asked.
//
// The instant is the caller's, passed in, because the recurrence horizon is measured
// against the same clock the cycle uses everywhere else.
func (q *Quarantine) NoteRecurrence(identity remediate.ProposalIdentity, shape autonomy.Shape, now time.Time) error {
	active := q.windows.Active(shape.Cluster, now)
	if len(active) == 0 {
		return q.inner.NoteRecurrence(identity, shape, now)
	}
	q.drop(Drop{Cluster: shape.Cluster, Detail: "recurrence of " + string(identity), Window: active[0]})
	return nil
}

// Standing forwards the ledger's own standing for a subject, when the wrapped recorder
// can answer.
//
// It exists because the cycle asks for a standing count through an optional interface
// in order to report a recurrence in words, and a recorder that stopped answering once
// it was wrapped would silently lose that prose. Forwarding rather than reimplementing
// is the point: the quarantine has no opinion about standing, only about what gets
// written.
func (q *Quarantine) Standing(subject autonomy.Subject) Standing {
	counter, ok := q.inner.(interface {
		Standing(autonomy.Subject) Standing
	})
	if !ok {
		return Standing{}
	}
	return counter.Standing(subject)
}

// drop records one held-back outcome.
func (q *Quarantine) drop(d Drop) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropped = append(q.dropped, d)
}

// Both the plain ledger and the quarantined one are recorders, which is the property
// that lets a cycle be wired with either and behave identically apart from what the
// quarantine withholds.
var (
	_ Recorder = (*Ledger)(nil)
	_ Recorder = (*Quarantine)(nil)
)
