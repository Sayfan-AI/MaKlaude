package budget

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Budget is the live blast-radius ceiling: the configured [Limits], the durable state
// behind them, and the accounting for the pass currently in progress.
//
// The zero value is not usable; construct one with [NewMemory] or [Open]. It is safe
// for concurrent use — one cycle runs over several clusters and a caller may well
// remediate them in parallel, and a cap that two goroutines can both pass is not a cap.
type Budget struct {
	mu sync.Mutex

	limits Limits
	now    func() time.Time

	// store is the durable backing, nil for an in-memory budget.
	store *store

	// state is the persisted posture: per-cluster breaker and per-target cooldown.
	state state

	// sealDetail is non-empty when the persisted state could not be read. A sealed
	// budget admits nothing; see the package doc for why this is a state of the object
	// rather than an error the caller might discard.
	sealDetail string

	// pass is the current pass's accounting, nil before [Budget.Begin] is first called.
	pass *pass
}

// pass is one cycle's worth of admissions: the per-cluster count the cap is measured
// against, and the suppressions to report. It is deliberately NOT persisted — a cap is
// per pass, so it must reset when the pass does, and reloading yesterday's count would
// be a cap that never refills.
type pass struct {
	admitted   map[string]int
	suppressed []Suppression
}

// newPass starts an empty pass.
func newPass() *pass { return &pass{admitted: map[string]int{}} }

// NewMemory builds a budget with no durable backing.
//
// It is the right choice for a test and the wrong choice for production: a breaker
// that forgets it tripped when the process restarts is not a breaker, because the
// condition that tripped it — a cluster MaKlaude is wrong about — outlives any one
// process. [Open] is the production constructor.
//
// Invalid limits are accepted rather than refused, and every admission then denies
// with [ReasonLimitsInvalid]. Refusing to construct would push the caller into the
// nil-budget case, where there is no ceiling at all.
func NewMemory(limits Limits, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	return &Budget{limits: limits, now: now, state: newState()}
}

// Open builds a budget backed by the state file at path, creating it lazily on the
// first write.
//
// It returns an error ONLY for an argument it cannot work with at all — an empty path,
// a directory it cannot create. A file that exists and cannot be read or parsed is not
// an error here: it produces a SEALED budget that denies every admission, because a
// caller that treated the failure as fatal and dropped the returned budget would
// thereby remove the ceiling. See the package doc; this is the one place this package
// deliberately diverges from [trust.Open]'s fail-loudly behavior, and the loudness is
// recovered by [Status.Sealed], which the state summary always prints.
func Open(path string, limits Limits, now func() time.Time) (*Budget, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("budget: no state path was given")
	}
	if now == nil {
		now = time.Now
	}

	s, err := newStore(path)
	if err != nil {
		return nil, err
	}

	b := &Budget{limits: limits, now: now, store: s, state: newState()}
	loaded, err := s.load()
	if err != nil {
		b.sealDetail = err.Error()
		return b, nil
	}
	b.state = loaded
	return b, nil
}

// Begin starts a new pass, resetting the per-cluster caps and clearing the
// suppressions from the previous one.
//
// It must be called before the first [Budget.Admit] of every cycle. Forgetting is not
// silently tolerated: admissions before the first Begin deny with [ReasonNoPass],
// because the alternative — treating "no pass" as "an empty pass" — would let a caller
// that never begins one admit up to the cap on every single call.
func (b *Budget) Begin() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pass = newPass()
}

// Admit asks whether one eligible auto-apply may run, and CONSUMES the budget when the
// answer is yes: the pass count rises and the target's cooldown starts. See the package
// doc on why admission rather than completion is the charging point.
//
// Callers pass only proposals [autonomy.Decide] has already ruled auto-appliable.
// Nothing here re-checks eligibility — this layer bounds how much of it may happen, and
// duplicating the policy questions would create a second place for them to be answered
// differently.
//
// The checks run hardest first and each ends the decision:
//
//  1. Is the state readable at all? A sealed budget knows nothing and permits nothing.
//  2. Do the limits bound anything? An invalid configuration is not a lenient one.
//  3. Do the caller and the target agree on the cluster? A disagreement is a confused
//     caller, and this system's worst failure is an action aimed at the wrong cluster.
//  4. Is the breaker open? Nothing runs unattended on a cluster a human has not cleared.
//  5. Is the pass cap already spent?
//  6. Is the target still cooling down?
//
// Every denial is recorded as a [Suppression] on the current pass, so the state summary
// can report it whether or not the caller does anything with the returned [Grant].
func (b *Budget) Admit(cluster string, target remediate.Target, at time.Time) Grant {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := targetKey(target)
	at = at.UTC()

	g, ok := b.check(cluster, target, key, at)
	if !ok {
		b.suppress(g, at)
		return g
	}

	// Consume. Both effects happen together and only on the admitting path, so a
	// denied admission never starts a cooldown and never spends a slot.
	cs := b.state.cluster(cluster)
	cs.LastAdmitted[key] = at
	if b.pass != nil {
		b.pass.admitted[cluster]++
	}
	b.persist()
	return g
}

// check runs the admission ladder without consuming anything. It reports the grant and
// whether it admits, and is separated from [Budget.Admit] only so the ladder reads as a
// ladder.
func (b *Budget) check(cluster string, target remediate.Target, key string, at time.Time) (Grant, bool) {
	g := Grant{Cluster: cluster, Target: key}

	if b.sealDetail != "" {
		g.Reason = ReasonStateUnreadable
		g.Detail = b.sealDetail
		return g, false
	}
	if err := b.limits.Validate(); err != nil {
		g.Reason = ReasonLimitsInvalid
		g.Detail = err.Error()
		return g, false
	}
	if cluster == "" || target.Cluster != cluster {
		g.Reason = ReasonClusterMismatch
		g.Detail = fmt.Sprintf("the admission names cluster %q and the target names %q", cluster, target.Cluster)
		return g, false
	}
	if b.pass == nil {
		g.Reason = ReasonNoPass
		g.Detail = "no pass has been started, so nothing can be counted against the per-pass cap"
		return g, false
	}

	cs := b.state.cluster(cluster)
	if cs.Tripped {
		g.Reason = ReasonBreakerTripped
		g.Detail = fmt.Sprintf("the breaker opened at %s after %d consecutive failures and stays open until a human clears it",
			cs.TrippedAt.UTC().Format(time.RFC3339), cs.ConsecutiveFailures)
		return g, false
	}
	if used := b.pass.admitted[cluster]; used >= b.limits.PerClusterPerPass {
		g.Reason = ReasonPassCapReached
		g.Detail = fmt.Sprintf("%d of %d auto-applies already admitted for this cluster this pass", used, b.limits.PerClusterPerPass)
		return g, false
	}
	if last, seen := cs.LastAdmitted[key]; seen {
		if elapsed := at.Sub(last.UTC()); elapsed < b.limits.Cooldown {
			g.Reason = ReasonTargetCoolingDown
			g.Detail = fmt.Sprintf("last auto-applied %s ago; the cooldown is %s and expires at %s",
				elapsed.Round(time.Second), b.limits.Cooldown, last.UTC().Add(b.limits.Cooldown).Format(time.RFC3339))
			return g, false
		}
	}

	g.Reason = ReasonAdmitted
	return g, true
}

// suppress records a denied admission on the current pass.
//
// A denial that arrives with no pass in progress is still recorded, on a pass created
// for the purpose, because [ReasonNoPass] is itself a suppression an operator needs to
// see — it means a caller is wired wrong and autonomy is silently doing nothing.
func (b *Budget) suppress(g Grant, at time.Time) {
	if b.pass == nil {
		b.pass = newPass()
	}
	b.pass.suppressed = append(b.pass.suppressed, Suppression{
		Cluster: g.Cluster,
		Target:  g.Target,
		Reason:  g.Reason.String(),
		Detail:  g.Detail,
		At:      at,
	})
}

// RecordOutcome records how one admitted auto-apply ended and returns what must follow.
//
// A success resets the cluster's consecutive-failure count and asks for nothing. A
// failure — including [OutcomeUnrecorded] and any value this build does not recognize —
// always asks for the full approved response: stop, roll back if reversible, demote the
// shape, escalate to a human, and never retry autonomously. The last of those is not in
// the returned [Consequence] because it is not an action: it is enforced by the target's
// cooldown, which [Budget.Admit] started before the action ran.
//
// The breaker trips when the consecutive count reaches [Limits.FailureThreshold], and
// [Consequence.Tripped] is true on that transition only, so a caller announces the trip
// once rather than on every subsequent failure.
//
// A sealed budget still returns the failure consequences. Being unable to read the
// state is a reason to trust the ceiling less, never a reason to skip a rollback or an
// escalation.
func (b *Budget) RecordOutcome(cluster string, target remediate.Target, outcome Outcome, at time.Time) Consequence {
	b.mu.Lock()
	defer b.mu.Unlock()

	at = at.UTC()
	cs := b.state.cluster(cluster)

	if !outcome.failed() {
		cs.ConsecutiveFailures = 0
		b.persist()
		return Consequence{}
	}

	cs.ConsecutiveFailures++
	c := Consequence{
		RollBack:            true,
		Demote:              true,
		Escalate:            true,
		ConsecutiveFailures: cs.ConsecutiveFailures,
	}

	// BREAK-VERIFICATION (issue #146, assertion (d)) — DO NOT MERGE: the trip
	// transition is removed, so no run of consecutive failures ever opens the
	// breaker. assertFailedAutoApplyTripsBreaker must fail the e2e on this branch
	// ("the budget reports tripped breakers [], want exactly one"); a green run
	// means the assertion lacks teeth.
	b.persist()
	return c
}

// Trip opens a cluster's breaker directly, without waiting for a run of failures.
//
// It exists for the second condition the milestone plan names alongside consecutive
// failures — "an anomalous burst" — which is a judgement this package cannot make,
// because it sees admissions one at a time and has no notion of what is normal for a
// cluster. So the mechanism lives here and the detection lives with whoever has the
// context to detect it. detail is the caller's short explanation and is recorded as-is.
func (b *Budget) Trip(cluster, detail string, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cs := b.state.cluster(cluster)
	if cs.Tripped {
		return
	}
	cs.Tripped = true
	cs.TrippedAt = at.UTC()
	cs.TrippedDetail = strings.TrimSpace(detail)
	if cs.TrippedDetail == "" {
		cs.TrippedDetail = "tripped without a stated reason"
	}
	b.persist()
}

// Clear closes a cluster's breaker. It is the human's move, and the only one: nothing
// in this package reopens autonomy on its own, and no timeout does it either.
//
// A named clearer is REQUIRED. The breaker exists because MaKlaude was wrong about a
// cluster in a way it could not detect, and the thing that resolves that is a person
// having looked — so an unattributed clear would be the system quietly re-authorizing
// itself, which is the failure the whole gate exists to prevent.
//
// Cooldowns are deliberately NOT cleared. They are short, they are per target, and they
// bound repetition rather than express distrust; wiping them would let the first pass
// after a clear immediately re-act on the exact object whose failure tripped the
// breaker. The consecutive-failure count IS reset, because a human has now looked.
//
// Clearing a closed breaker is a no-op and not an error, so a runbook step is safe to
// repeat. A sealed budget refuses: the state it would clear is state it cannot read.
func (b *Budget) Clear(cluster, by string, at time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if strings.TrimSpace(by) == "" {
		return fmt.Errorf("budget: clearing the breaker on %q requires naming who cleared it", cluster)
	}
	if b.sealDetail != "" {
		return fmt.Errorf("budget: refusing to clear the breaker on %q: %s", cluster, b.sealDetail)
	}

	cs := b.state.cluster(cluster)
	cs.Tripped = false
	cs.TrippedAt = time.Time{}
	cs.TrippedDetail = ""
	cs.ConsecutiveFailures = 0
	cs.ClearedAt = at.UTC()
	cs.ClearedBy = strings.TrimSpace(by)
	b.persist()
	return nil
}

// Status snapshots the whole posture for the operator-facing state summary.
//
// Everything it returns is copied, so a caller holding a status cannot mutate the
// budget, and a status rendered later still describes the instant it was taken.
func (b *Budget) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()

	s := Status{
		Sealed:       b.sealDetail != "",
		SealDetail:   b.sealDetail,
		Limits:       b.limits,
		Breakers:     make([]Breaker, 0, len(b.state.Clusters)),
		Suppressions: []Suppression{},
	}
	if b.store != nil {
		s.Path = b.store.path
	}

	names := make([]string, 0, len(b.state.Clusters))
	for name := range b.state.Clusters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cs := b.state.Clusters[name]
		s.Breakers = append(s.Breakers, Breaker{
			Cluster:             name,
			Tripped:             cs.Tripped,
			TrippedAt:           cs.TrippedAt,
			ConsecutiveFailures: cs.ConsecutiveFailures,
			Detail:              cs.TrippedDetail,
		})
	}
	if b.pass != nil && len(b.pass.suppressed) > 0 {
		s.Suppressions = append(s.Suppressions, b.pass.suppressed...)
	}
	return s
}

// Limits reports the bounds in force.
func (b *Budget) Limits() Limits {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limits
}

// Sealed reports whether the persisted state was unreadable, so every admission is
// denied. It is exposed separately from [Budget.Status] so a caller can escalate the
// seal without rendering a whole status.
func (b *Budget) Sealed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sealDetail != ""
}

// persist writes the state through to disk, seals the budget if it cannot.
//
// A write failure seals rather than being returned, and that direction is the same
// argument the package doc makes about [Open]: state that cannot be written is state
// the next process will not see, so the breaker and the cooldowns are no longer
// durable, and continuing to admit actions on the strength of an in-memory ceiling
// nobody can recover is the unbounded failure. Sealing stops autonomy and says why.
//
// The caller must hold b.mu.
func (b *Budget) persist() {
	if b.store == nil || b.sealDetail != "" {
		return
	}
	b.state.prune(b.now().UTC(), b.limits.Cooldown)
	if err := b.store.save(b.state); err != nil {
		b.sealDetail = "the state could not be written, so the breaker and cooldowns are no longer durable: " + err.Error()
	}
}
