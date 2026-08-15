package trust

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	// PromotionThreshold is how many human-approved converged executions of one FIX
	// promotion requires. Approved on the Milestone 5 plan; not a tunable, because a
	// knob that lowers it is a knob that buys autonomy with configuration instead of
	// with evidence.
	//
	// The three no longer have to be recent. Issue #167 replaced the counting window
	// with invalidation: an approval stays good until the fix it was given for changes
	// or the fix stops working, so an approval from a year ago is exactly as valid as
	// one from this morning provided nothing has moved. See the package doc.
	PromotionThreshold = 3

	// DemotionScope is how many of a SHAPE's most recent recorded executions a
	// demoting outcome keeps blocking trust for. It is the counting window's one
	// surviving job, kept deliberately and named for what it now does.
	//
	// The window used to bound both halves of the rule and issue #167 took the
	// promotion half away, because ageing out a still-valid approval on a counter is
	// the thing that measured nothing. This half measures something real: it is the
	// RECOVERY path. Without a bound, one failure would block a shape forever and the
	// only escape would be editing the ledger — which [validate] exists to make
	// impossible — so a shape that had one bad day could never be trusted again.
	// With it, a demoted shape climbs back by accumulating this many further recorded
	// executions without another demoting one, and since an auto-applied success is
	// not recorded at all ([Entry.Counts]), every one of those is an execution a human
	// approved. So recovery is real, bounded, and driven entirely by people.
	DemotionScope = 10
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
//
// An entry that does not [Entry.Counts] — an auto-applied success — is dropped
// silently rather than rejected: it is a legitimate execution that simply is not
// evidence, and a caller reporting it did nothing wrong. Dropping it HERE rather
// than asking callers to filter is what makes the window-membership rule a property
// of the ledger: the live path and [Ledger.Rebuild] funnel through the same
// predicate, so no caller can put an entry in one history that the other would not
// hold.
func (l *Ledger) Record(e Entry) error {
	if err := validate(e); err != nil {
		return err
	}
	if !e.Counts() {
		return nil
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
//
// An entry that does not [Entry.Counts] is dropped, exactly as [Ledger.Record] drops
// it on the live path — one predicate, two callers, no way for the two histories to
// disagree about an auto-applied success.
func (l *Ledger) Rebuild(entries []Entry) error {
	kept := make([]Entry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if err := validate(e); err != nil {
			return err
		}
		if !e.Counts() {
			continue
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

// Trust implements [autonomy.TrustOracle]: it reports whether this subject's recorded
// history meets the promotion bar, with the citation that says why.
//
// It reads no clock and no counter of cycles. The answer is a function of the
// recorded history and the subject's fingerprint and of nothing else — two calls
// against the same history return the same evidence and the same citation, in any
// process, at any time, which is the promise [autonomy.TrustOracle] makes on this
// implementation's behalf. Under the counting-window model that promise held only
// per-call: the same history genuinely did answer differently as unrelated executions
// pushed approvals out of the window.
func (l *Ledger) Trust(subject autonomy.Subject) autonomy.TrustEvidence {
	standing := l.Standing(subject)
	if !standing.Trusted {
		return autonomy.TrustEvidence{}
	}
	return autonomy.TrustEvidence{Trusted: true, Citation: standing.Citation()}
}

// Standing is the full arithmetic behind one [Ledger.Trust] answer: which entries
// were examined and what was found among them.
//
// [Ledger.Trust] returns only a bool and a citation because that is all the decision
// layer may act on. This type exists for the other two readers — an operator asking
// "why is my fix still gating?", and a test asserting that a specific history
// produces a specific count rather than merely the right verdict.
type Standing struct {
	// Subject is the shape and fingerprint this standing concerns.
	Subject autonomy.Subject

	// Recorded is how many executions of the SHAPE the ledger holds in total, across
	// every fingerprint and all time. It is here so an operator can see the difference
	// between "no history" and "plenty of history, none of it for this fix".
	Recorded int

	// Matching is how many of those executions were of this exact fingerprint. The gap
	// between Recorded and Matching is the answer to the most common confused
	// question — "this shape has run fifty times, why is it not trusted?" — and the
	// answer is that the fix changed.
	Matching int

	// Approved is how many entries of this fingerprint were human-approved AND
	// converged — the count measured against [PromotionThreshold]. It is NOT bounded
	// by a window: see [PromotionThreshold].
	Approved int

	// Scope is how many of the shape's most recent entries were examined for a
	// demoting outcome: the smaller of Recorded and [DemotionScope].
	Scope int

	// Blocker is the most recent demoting entry within [DemotionScope], and Blocked
	// reports that there is one. The most recent rather than the first because that is
	// the one an operator is asking about.
	//
	// It is drawn from the SHAPE's history rather than the fingerprint's, which is the
	// asymmetry the whole model rests on — see the package doc. A rollout-restart that
	// failed on one deployment is evidence about restarting things on this cluster, and
	// letting a re-fingerprinted proposal walk past it would turn "the fix changed"
	// from a reason to re-earn trust into a way to launder a failure.
	Blocker Entry
	Blocked bool

	// Blind reports that the block came from a streak of unobserved outcomes rather
	// than from a bad one — see [unobservedStreak]. It is separate from Blocked because
	// the two need different words to an operator: one says a fix went wrong, the other
	// says MaKlaude has stopped being able to tell whether it did.
	Blind bool

	// Latest is the newest entry of this fingerprint, zero when there is none. It is
	// what the citation names, so an incident review starts at an artifact that
	// actually concerns the fix that ran.
	Latest Entry

	// Trusted is the verdict.
	Trusted bool
}

// Standing computes the subject's full standing.
//
// The two halves are scoped differently on purpose and the difference is the model:
// promotion counts only entries carrying this exact fingerprint and looks across all
// of history, while demotion looks at every entry for the shape and only within
// [DemotionScope]. See the package doc for why each direction is the fail-closed one.
func (l *Ledger) Standing(subject autonomy.Subject) Standing {
	l.mu.Lock()
	defer l.mu.Unlock()

	all := l.byShape[subject.Shape]
	scope := all
	if len(scope) > DemotionScope {
		scope = scope[len(scope)-DemotionScope:]
	}

	st := Standing{Subject: subject, Recorded: len(all), Scope: len(scope)}

	for _, e := range scope {
		if e.Demotes() {
			st.Blocker, st.Blocked = e, true
		}
	}
	if !st.Blocked {
		if blocker, blind := unobservedStreak(scope); blind {
			st.Blocker, st.Blocked, st.Blind = blocker, true, true
		}
	}

	// An empty fingerprint matches nothing, ever. It is what a subject built from a
	// proposal this build could not fingerprint looks like, and what a ledger entry
	// recorded before fingerprints existed carries. Both must gate, and the guard is
	// here rather than at the call sites so no caller can skip it.
	if subject.Fingerprint == "" {
		return st
	}
	for _, e := range all {
		if e.Fingerprint != subject.Fingerprint {
			continue
		}
		st.Matching++
		st.Latest = e
		if e.Promotes() {
			st.Approved++
		}
	}

	st.Trusted = !st.Blocked && st.Approved >= PromotionThreshold
	return st
}

// unobservedStreak reports whether the shape's most recent executions are an unbroken
// run of [OutcomeInconclusive] at least [PromotionThreshold] long, and the newest one
// if so. Entries are in the ledger's order, oldest first.
//
// This exists because of what the counting window used to do by accident. An
// inconclusive execution does not demote — the action did not fail, it just could not
// be confirmed — so under the window a shape that timed out on every attempt still
// lost trust eventually, as the timeouts crowded its approvals out. Issue #167 removed
// the window on the grounds that a counter measures nothing, which is right, and that
// reasoning does not extend to this: a run of unobserved outcomes measures something
// real and specific, which is that MaKlaude can no longer tell whether this fix works.
//
// That is the same defect [OutcomeRegressed] guards against, arriving from the other
// side. A regression is the convergence check saying yes and being wrong; a streak is
// the convergence check unable to say anything at all. Both mean the evidence behind
// the cached approval has stopped being produced, and acting unattended on evidence
// that stopped being produced is exactly what the window was blundering into
// preventing.
//
// The length is [PromotionThreshold] rather than a new tunable, and the symmetry is
// the argument: it takes that many CONFIRMED successes to earn trust, so that many
// consecutive unconfirmable executions is the point at which the same quantity of
// evidence has gone missing. A knob here would be a knob that buys autonomy by
// widening the definition of "still working".
func unobservedStreak(scope []Entry) (Entry, bool) {
	if len(scope) < PromotionThreshold {
		return Entry{}, false
	}
	for i := len(scope) - PromotionThreshold; i < len(scope); i++ {
		if scope[i].Outcome != OutcomeInconclusive {
			return Entry{}, false
		}
	}
	return scope[len(scope)-1], true
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
		"%d human-approved executions of this exact fix on %s converged (%d required), and no failure, "+
			"rollback or drift-abort in the last %d recorded executions of that shape; "+
			"fingerprint %s; most recent %s%s",
		s.Approved, s.Subject.Shape, PromotionThreshold, s.Scope,
		s.Subject.Fingerprint, stamp(s.Latest.At), refSuffix(s.Latest.Ref))
}

// reason states why an untrusted standing is untrusted, in the order an operator
// would fix them: no fingerprint to judge, then no history at all, then a blocking
// outcome, then a history that is all for some other fix, then simply not enough
// approvals yet.
//
// The "some other fix" case is the one this model adds and the one most likely to
// confuse, because the shape looks busy and trusted-adjacent while nothing about the
// proposal in hand has ever been approved. It says so in those words rather than
// reporting a count of zero.
func (s Standing) reason() string {
	switch {
	case s.Subject.Fingerprint == "":
		return fmt.Sprintf(
			"the proposal for %s carries no fingerprint, so no past approval can be matched to it", s.Subject.Shape)
	case s.Recorded == 0:
		return fmt.Sprintf("%s has no recorded executions, so it has earned nothing", s.Subject.Shape)
	case s.Blind:
		return fmt.Sprintf(
			"the last %d recorded executions of %s were all inconclusive, most recently at %s: MaKlaude can "+
				"no longer confirm that fixes of this shape work, so it must not apply them unwatched",
			PromotionThreshold, s.Subject.Shape, stamp(s.Blocker.At))
	case s.Blocked:
		return fmt.Sprintf(
			"%s had a %s execution at %s within the last %d recorded, which blocks trust for every fix of "+
				"that shape until it ages out",
			s.Subject.Shape, s.Blocker.Outcome, stamp(s.Blocker.At), s.Scope)
	case s.Matching == 0:
		return fmt.Sprintf(
			"%s has %d recorded executions but none of this fix (fingerprint %s): the approvals on record "+
				"were given for a different action, so they do not carry over",
			s.Subject.Shape, s.Recorded, s.Subject.Fingerprint)
	default:
		return fmt.Sprintf(
			"this fix on %s has %d human-approved converged executions and %d are required (%d recorded "+
				"in total for the fingerprint)",
			s.Subject.Shape, s.Approved, PromotionThreshold, s.Matching)
	}
}

// Explain renders the subject's standing for a human, trusted or not. It is the
// answer to "why is this still gating?", which is the question a fresh install
// generates on every proposal and which a bare "untrusted" does not answer.
func (l *Ledger) Explain(subject autonomy.Subject) string { return l.Standing(subject).Citation() }

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

// RecurrenceHorizon is how long after a converged execution the same fault
// reappearing on the same object counts as that execution having failed.
//
// It is the one number in this package that is a judgment about the world rather than
// an approved parameter, so the reasoning is worth stating. Below it, a fault that
// returns is a fix that did not hold: the convergence check watched, saw the intended
// state, and was wrong — either it read the cluster during a transient improvement, or
// the change it made was undone, or the fix addressed a symptom. Above it, a fault that
// returns is plausibly a new incident, and demoting the shape for it would punish a fix
// that worked for a day.
//
// An hour is deliberately generous on the "this is a regression" side, because the two
// errors are not symmetric. Calling a genuine new incident a regression costs a shape
// its autonomy until it re-earns it, which a human notices and fixes by approving. Not
// calling a regression a regression leaves a shape acting unattended on the strength of
// a convergence check that has already been demonstrated to lie about it, which is the
// exposure removing the counting window created and the thing this constant exists to
// close.
const RecurrenceHorizon = time.Hour

// NoteRecurrence records that a fix which reported convergence did not hold: the same
// fault, on the same object, was diagnosed again within [RecurrenceHorizon].
//
// This is the mechanism that lets the counting window go. The window's real job was
// never counting — it was forcing a shape back in front of a person regardless of what
// MaKlaude's own health signal claimed, which mattered precisely because that signal
// can be wrong in the optimistic direction. Invalidation on its own would have left
// that exposure uncovered: a fix that appears to work and does not would keep its trust
// forever, since nothing would ever demote it. This demotes it, on evidence rather than
// on a schedule.
//
// It takes the instant from the caller rather than reading a clock, because everything
// else in this package is reproducible from recorded history and a promotion decision
// that depended on when it was asked would not be. The caller is the reconciliation
// cycle, which already has an injected clock.
//
// The recurrence is recorded against the shape and the fingerprint of the ORIGINAL
// converged execution, not of the proposal that recurred. They are usually the same
// token, and when they differ the original is the right one: this entry is evidence
// about the fix that ran and failed to hold, and filing it under the fix that has not
// run yet would blame a proposal for its predecessor's failure while leaving the
// predecessor's approvals intact.
//
// Nothing is recorded, and no error returned, when the shape has no converged execution
// of that identity inside the horizon. A fault being diagnosed is the ordinary case; it
// is only a regression relative to a specific recent claim that it was fixed.
func (l *Ledger) NoteRecurrence(identity remediate.ProposalIdentity, shape autonomy.Shape, now time.Time) error {
	l.mu.Lock()
	original, found := l.recentConvergence(identity, shape, now)
	l.mu.Unlock()
	if !found {
		return nil
	}

	// A distinct key, in the same spirit as the rollback suffix: the converged entry and
	// the regression are two things that happened to one proposal and the history needs
	// both, so they cannot collide under the idempotent [Ledger.Record]. It is derived
	// from the original entry's key rather than from `now`, which makes the whole
	// operation idempotent — a cycle that observes the same recurrence twice records it
	// once, instead of stacking demotions that all describe one event.
	return l.Record(Entry{
		Key:         original.Key + recurrenceKeySuffix,
		Identity:    identity,
		Shape:       shape,
		Fingerprint: original.Fingerprint,
		Authority:   original.Authority,
		Outcome:     OutcomeRegressed,
		At:          now.UTC(),
		Ref:         original.Ref,
	})
}

// recentConvergence finds the newest converged execution of one proposal identity for
// this shape that finished within [RecurrenceHorizon] of now. Callers hold the lock.
//
// It matches on [Entry.Identity], because the ledger is indexed by shape and one
// shape's history spans every object the operation has touched. A recurrence is about
// one object: a deployment crashlooping again says nothing about the restart of a
// different deployment that is still holding.
func (l *Ledger) recentConvergence(identity remediate.ProposalIdentity, shape autonomy.Shape, now time.Time) (Entry, bool) {
	var best Entry
	var found bool
	for _, e := range l.byShape[shape] {
		if e.Identity != identity || e.Outcome != OutcomeConverged {
			continue
		}
		if now.Sub(e.At) > RecurrenceHorizon || e.At.After(now) {
			continue
		}
		if !found || best.before(e) {
			best, found = e, true
		}
	}
	return best, found
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

// Ensure the two tuned constants stay sane relative to each other. They no longer
// bound the same quantity — promotion counts matching fingerprints across all
// history, demotion scans the shape's last [DemotionScope] entries — but a recovery
// scope shorter than the approvals a shape must accumulate to recover would make a
// demoted shape's climb back shorter than its original climb up, which is backwards.
// A negative array length does not compile, so the build catches it rather than a
// test.
var _ [DemotionScope - PromotionThreshold]struct{}
