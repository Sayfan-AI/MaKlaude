package trust

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
)

const (
	// PromotionThreshold is how many human-approved converged executions of a shape,
	// inside the evaluation window, promotion requires. Approved on the Milestone 5
	// plan; not a tunable, because a knob that lowers it is a knob that buys autonomy
	// with configuration instead of with evidence.
	PromotionThreshold = 3

	// EvaluationWindow is how many of a shape's most recent recorded executions the
	// whole rule is evaluated over. A single demoting outcome anywhere inside it
	// blocks trust, and the promoting executions must be inside it too — see the
	// package doc on why the window bounds both halves rather than only the failures.
	EvaluationWindow = 10
)

// Ledger is the recorded execution history, indexed by shape, and the
// [autonomy.TrustOracle] computed from it.
//
// It is safe for concurrent use. A Ledger with no backing file is a valid, purely
// in-memory ledger — see [NewMemory] — which is what the decision layer's tests use
// and what a process configured with no ledger path gets. The in-memory case trusts
// exactly what it has been told about in this process, which for a fresh process is
// nothing, so the absence of durable storage degrades to "everything gates" rather
// than to an error.
type Ledger struct {
	mu sync.Mutex

	// store is the durable backing, nil for an in-memory ledger.
	store *store

	// keys is every recorded entry key, so a repeated Record is a no-op and a rebuild
	// from the same artifacts lands on the same history.
	keys map[string]struct{}

	// byShape holds each shape's entries in the ledger's total order (see
	// [Entry.before]). Indexing by shape rather than filtering a flat slice on every
	// question keeps [Ledger.Trust] proportional to one shape's history, which matters
	// because it is asked once per proposal per reconciliation cycle.
	byShape map[autonomy.Shape][]Entry
}

// NewMemory returns an empty ledger with no durable backing.
func NewMemory() *Ledger {
	return &Ledger{keys: map[string]struct{}{}, byShape: map[autonomy.Shape][]Entry{}}
}

// Record adds one execution to the history, and to the ledger file when there is
// one.
//
// It is idempotent on [Entry.Key]: recording the same execution twice stores it
// once, silently. That is what makes the live path safe to retry and what makes
// "replay the approval artifacts" produce the same ledger as "append as things
// happen" — the property [Ledger.Rebuild] depends on.
//
// An invalid entry is REJECTED rather than stored in a degraded form. The
// alternative — storing it with whatever fields were populated — would put an entry
// in the window whose meaning nobody can state, and the window is the input to a
// decision about mutating a cluster with no human watching.
func (l *Ledger) Record(e Entry) error {
	if err := validate(e); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if _, seen := l.keys[e.Key]; seen {
		return nil
	}

	// The file is written before the in-memory index so a failed write leaves the
	// ledger exactly as it was. The other order would report an error to the caller
	// while this process went on granting trust from an entry no restart will ever
	// see again.
	if l.store != nil {
		if err := l.store.append(e); err != nil {
			return fmt.Errorf("trust: recording %s: %w", e.Key, err)
		}
	}
	l.insert(e)
	return nil
}

// Rebuild replaces the entire history with entries derived afresh from the approval
// artifacts, atomically on disk.
//
// This is the operation that makes the ledger a cache rather than an authority. If
// the file is lost, corrupted, hand-edited, or simply distrusted, the answer is
// never "reconstruct what it must have said" — it is to re-read the artifacts and
// call this. Anything the artifacts do not support disappears, which is the whole
// point: an entry that survives a rebuild is one GitHub can vouch for.
//
// Duplicate keys among the supplied entries collapse to the first occurrence, so a
// caller that reads overlapping pages of artifacts does not have to deduplicate
// first.
func (l *Ledger) Rebuild(entries []Entry) error {
	kept := make([]Entry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if err := validate(e); err != nil {
			return err
		}
		if _, dup := seen[e.Key]; dup {
			continue
		}
		seen[e.Key] = struct{}{}
		kept = append(kept, e)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.store != nil {
		if err := l.store.replace(kept); err != nil {
			return fmt.Errorf("trust: rebuilding ledger: %w", err)
		}
	}

	l.keys = make(map[string]struct{}, len(kept))
	l.byShape = map[autonomy.Shape][]Entry{}
	for _, e := range kept {
		l.insert(e)
	}
	return nil
}

// insert files one entry into the in-memory index, in the ledger's total order. The
// caller holds the lock.
//
// Entries usually arrive in order, so this is an append in the common case, but it
// must not ASSUME order: a rebuild reads artifacts in whatever order the API returns
// them, and the window must not depend on that.
func (l *Ledger) insert(e Entry) {
	l.keys[e.Key] = struct{}{}
	shaped := l.byShape[e.Shape]
	at := sort.Search(len(shaped), func(i int) bool { return e.before(shaped[i]) })
	shaped = append(shaped, Entry{})
	copy(shaped[at+1:], shaped[at:])
	shaped[at] = e
	l.byShape[e.Shape] = shaped
}

// Trust implements [autonomy.TrustOracle]: it reports whether this shape's recorded
// history meets the promotion bar, with the citation that says why.
//
// It reads no clock. The window is the last [EvaluationWindow] entries by the
// ledger's own order, so the answer depends on the recorded history and on nothing
// else — two calls against the same history return the same evidence and the same
// citation, in any process, at any time, which is the promise
// [autonomy.TrustOracle] makes on this implementation's behalf.
func (l *Ledger) Trust(shape autonomy.Shape) autonomy.TrustEvidence {
	standing := l.Standing(shape)
	if !standing.Trusted {
		return autonomy.TrustEvidence{}
	}
	return autonomy.TrustEvidence{Trusted: true, Citation: standing.Citation()}
}

// Standing is the full arithmetic behind one [Ledger.Trust] answer: the window that
// was examined and what was found in it.
//
// [Ledger.Trust] returns only a bool and a citation because that is all the decision
// layer may act on. This type exists for the other two readers — an operator asking
// "why is my shape still gating?", and a test asserting that a specific history
// produces a specific count rather than merely the right verdict.
type Standing struct {
	// Shape is the shape this standing concerns.
	Shape autonomy.Shape

	// Recorded is how many executions of the shape the ledger holds in total, across
	// all time. It is not what the decision is made on; it is here so an operator can
	// see the difference between "no history" and "history that aged out".
	Recorded int

	// Window is how many entries were examined: the smaller of Recorded and
	// [EvaluationWindow].
	Window int

	// Approved is how many entries in the window were human-approved AND converged —
	// the count measured against [PromotionThreshold].
	Approved int

	// Blocker is the most recent demoting entry in the window, and Blocked reports
	// that there is one. The most recent rather than the first because that is the one
	// an operator is asking about.
	Blocker Entry
	Blocked bool

	// Latest is the newest entry in the window, zero when there is no history.
	Latest Entry

	// Trusted is the verdict.
	Trusted bool
}

// Standing computes the shape's full standing.
func (l *Ledger) Standing(shape autonomy.Shape) Standing {
	l.mu.Lock()
	defer l.mu.Unlock()

	all := l.byShape[shape]
	window := all
	if len(window) > EvaluationWindow {
		window = window[len(window)-EvaluationWindow:]
	}

	st := Standing{Shape: shape, Recorded: len(all), Window: len(window)}
	if len(window) == 0 {
		return st
	}
	st.Latest = window[len(window)-1]

	for _, e := range window {
		if e.Promotes() {
			st.Approved++
		}
		if e.Demotes() {
			st.Blocker, st.Blocked = e, true
		}
	}
	st.Trusted = !st.Blocked && st.Approved >= PromotionThreshold
	return st
}

// Citation renders the one-line evidence a trusted standing carries into the audit
// trail.
//
// It is the entire oversight artifact for an action nobody approved, so it states
// the counts rather than asserting a conclusion, and it names the artifact behind
// the most recent approval so a reader can start reading somewhere. It is a pure
// function of the standing, which is what keeps [autonomy.TrustEvidence.Citation]'s
// stability promise: the same history renders the same string.
//
// An untrusted standing renders the reason it is untrusted instead, which is what
// [Ledger.Explain] hands an operator. It is never returned as a citation — see
// [Ledger.Trust], which drops it — because a citation is by definition a reason to
// act, and there is no such thing as a citation for "no".
func (s Standing) Citation() string {
	if !s.Trusted {
		return s.reason()
	}
	return fmt.Sprintf(
		"%d of the last %d recorded executions of %s were human-approved and converged (%d required); "+
			"no failure, rollback or drift-abort among them; most recent %s%s",
		s.Approved, s.Window, s.Shape, PromotionThreshold,
		stamp(s.Latest.At), refSuffix(s.Latest.Ref))
}

// reason states why an untrusted standing is untrusted, in the order an operator
// would fix them: no history at all, then a blocking outcome, then simply not enough
// approvals yet.
func (s Standing) reason() string {
	switch {
	case s.Recorded == 0:
		return fmt.Sprintf("%s has no recorded executions, so it has earned nothing", s.Shape)
	case s.Blocked:
		return fmt.Sprintf(
			"%s had a %s execution at %s within the last %d recorded, which blocks trust until it ages out of the window",
			s.Shape, s.Blocker.Outcome, stamp(s.Blocker.At), s.Window)
	default:
		return fmt.Sprintf(
			"%s has %d human-approved converged executions in the last %d recorded, and %d are required",
			s.Shape, s.Approved, s.Window, PromotionThreshold)
	}
}

// Explain renders the shape's standing for a human, trusted or not. It is the
// answer to "why is this still gating?", which is the question a fresh install
// generates on every proposal and which a bare "untrusted" does not answer.
func (l *Ledger) Explain(shape autonomy.Shape) string { return l.Standing(shape).Citation() }

// Entries returns every recorded entry, ordered by shape and then by the ledger's
// own order. The slice is freshly built, so a caller cannot reach in and change what
// the ledger holds.
//
// Shapes are emitted in sorted order rather than in map order: this feeds a rebuild
// and an operator's eyes, and a listing that shuffled between two calls on identical
// data would make both harder to trust.
func (l *Ledger) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	shapes := make([]autonomy.Shape, 0, len(l.byShape))
	for shape := range l.byShape {
		shapes = append(shapes, shape)
	}
	sort.Slice(shapes, func(i, j int) bool { return shapes[i].String() < shapes[j].String() })

	out := make([]Entry, 0, len(l.keys))
	for _, shape := range shapes {
		out = append(out, l.byShape[shape]...)
	}
	return out
}

// Len reports how many executions the ledger holds.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

// validate rejects an entry the promotion arithmetic could not honestly use.
//
// The human-approval rule is the load-bearing one and it is the reason this function
// exists at all. Every other check catches a programming mistake; that one catches
// an operator who edited the ledger file to grant themselves trust, by requiring the
// one thing a text editor cannot manufacture — an approval artifact a rebuild would
// find again.
func validate(e Entry) error {
	switch {
	case e.Key == "":
		return fmt.Errorf("trust: entry has no key")
	case e.Shape.Cluster == "":
		return fmt.Errorf("trust: entry %s names no cluster", e.Key)
	case e.Shape.Operation == "":
		return fmt.Errorf("trust: entry %s names no operation", e.Key)
	case e.Outcome == OutcomeUnrecorded:
		return fmt.Errorf("trust: entry %s has no recorded outcome", e.Key)
	case e.At.IsZero():
		return fmt.Errorf("trust: entry %s has no execution time, so it cannot be ordered", e.Key)
	case e.Authority == audit.AuthorityHuman && e.Ref == "":
		return fmt.Errorf(
			"trust: entry %s claims human approval but names no approval artifact; "+
				"the artifact is the authority and this ledger is only a cache of it", e.Key)
	}
	return nil
}

// stamp renders an instant in the fixed UTC form the audit trail uses, so a citation
// and the records behind it spell the same moment the same way.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "an unrecorded time"
	}
	return t.UTC().Format(time.RFC3339)
}

// refSuffix renders the trailing "(ref …)" clause, or nothing when the entry names
// no artifact. A policy-waived execution legitimately has none, and "(ref )" in the
// one field an incident review reads is worse than silence.
func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " (ref " + ref + ")"
}

// Ensure the ledger really is the oracle the decision layer declared. This is the
// single line that would break if [autonomy.TrustOracle] changed shape, which is
// where it should break rather than at a call site in the binary.
var _ autonomy.TrustOracle = (*Ledger)(nil)

// Ensure the two tuned constants stay sane relative to each other: a promotion
// threshold larger than the window would make trust silently unreachable, and an
// unreachable safety mechanism looks exactly like a working one. A negative array
// length does not compile, so the build catches it rather than a test.
var _ [EvaluationWindow - PromotionThreshold]struct{}
