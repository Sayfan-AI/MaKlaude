package budget

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// base is the instant every test's injected clock starts from. A fixed value rather
// than time.Now keeps every assertion about elapsed time exact.
var base = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

// clock is an injectable, manually advanced clock. Every test in this package uses
// one: a bound measured against the wall clock is a bound whose test is a race.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: base} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// target builds a target in a cluster, named so a test reads as the object it is about.
func target(cluster, name string) remediate.Target {
	return remediate.Target{
		Cluster:         cluster,
		Kind:            "deployment",
		Namespace:       "default",
		Name:            name,
		ResourceVersion: "1",
	}
}

// admit is the common call shape: begin-less callers are a separate test.
func admit(t *testing.T, b *Budget, cluster, name string, at time.Time) Grant {
	t.Helper()
	return b.Admit(cluster, target(cluster, name), at)
}

func TestAdmit_WithinEveryBoundIsAdmitted(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	g := admit(t, b, "prod", "api", base)
	if !g.Admitted() {
		t.Fatalf("a first auto-apply on an untouched cluster must be admitted, got %s", g)
	}
	if g.Reason != ReasonAdmitted {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonAdmitted)
	}
	if g.Detail != "" {
		t.Errorf("an admission carries no detail, got %q", g.Detail)
	}
}

func TestAdmit_PassCapBoundsOneCluster(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	// The approved default is 2 per cluster per pass. Three distinct targets, so the
	// cooldown cannot be what stops the third.
	for i, name := range []string{"api", "web"} {
		if g := admit(t, b, "prod", name, base); !g.Admitted() {
			t.Fatalf("admission %d must be within the cap, got %s", i+1, g)
		}
	}

	g := admit(t, b, "prod", "worker", base)
	if g.Admitted() {
		t.Fatalf("the third admission must exceed the cap of %d", DefaultPerClusterPerPass)
	}
	if g.Reason != ReasonPassCapReached {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonPassCapReached)
	}
	if !strings.Contains(g.Detail, "2 of 2") {
		t.Errorf("the detail must state the cap and its use, got %q", g.Detail)
	}
}

func TestAdmit_CapIsPerClusterNotGlobal(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	for _, name := range []string{"api", "web"} {
		if g := admit(t, b, "prod", name, base); !g.Admitted() {
			t.Fatalf("prod admission must be within the cap, got %s", g)
		}
	}
	// Multi-cluster isolation: prod being full says nothing about staging.
	if g := admit(t, b, "staging", "api", base); !g.Admitted() {
		t.Fatalf("a full cap on one cluster must not bound another, got %s", g)
	}
}

func TestAdmit_BeginRefillsTheCap(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)

	b.Begin()
	for _, name := range []string{"api", "web"} {
		admit(t, b, "prod", name, c.Now())
	}
	if g := admit(t, b, "prod", "worker", c.Now()); g.Reason != ReasonPassCapReached {
		t.Fatalf("the pass must be full, got %s", g)
	}

	// A new pass, past every cooldown, is a fresh allowance.
	c.advance(DefaultCooldown + time.Minute)
	b.Begin()
	if g := admit(t, b, "prod", "worker", c.Now()); !g.Admitted() {
		t.Fatalf("a new pass refills the cap, got %s", g)
	}
}

func TestAdmit_CooldownBoundsOneTargetAcrossPasses(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)

	b.Begin()
	if g := admit(t, b, "prod", "api", c.Now()); !g.Admitted() {
		t.Fatalf("the first admission must succeed, got %s", g)
	}

	// A brand new pass, so the cap is not what is being measured.
	c.advance(DefaultCooldown - time.Second)
	b.Begin()
	g := admit(t, b, "prod", "api", c.Now())
	if g.Admitted() {
		t.Fatalf("one second inside the cooldown must not admit, got %s", g)
	}
	if g.Reason != ReasonTargetCoolingDown {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonTargetCoolingDown)
	}

	// And the boundary itself: at exactly the cooldown, the target is free again.
	c.advance(time.Second)
	b.Begin()
	if g := admit(t, b, "prod", "api", c.Now()); !g.Admitted() {
		t.Fatalf("at exactly the cooldown the target must be admitted, got %s", g)
	}
}

func TestAdmit_CooldownIsPerTarget(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)

	b.Begin()
	admit(t, b, "prod", "api", c.Now())

	c.advance(time.Minute)
	b.Begin()
	if g := admit(t, b, "prod", "web", c.Now()); !g.Admitted() {
		t.Fatalf("a cooling-down target must not bound a different one, got %s", g)
	}
}

func TestAdmit_CooldownIgnoresResourceVersion(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)

	b.Begin()
	first := target("prod", "api")
	if g := b.Admit("prod", first, c.Now()); !g.Admitted() {
		t.Fatalf("the first admission must succeed, got %s", g)
	}

	// A restart bumps the object's resourceVersion. A cooldown keyed on it would
	// expire the instant the action it throttles took effect.
	c.advance(time.Minute)
	b.Begin()
	bumped := first
	bumped.ResourceVersion = "9999"
	if g := b.Admit("prod", bumped, c.Now()); g.Reason != ReasonTargetCoolingDown {
		t.Fatalf("a bumped resourceVersion must not clear the cooldown, got %s", g)
	}
}

func TestAdmit_DenialConsumesNothing(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	// Fill the pass, then ask twice more for a target that has never been admitted.
	admit(t, b, "prod", "api", c.Now())
	admit(t, b, "prod", "web", c.Now())
	admit(t, b, "prod", "worker", c.Now())

	// A denied admission must not have started the target's cooldown: the next pass
	// admits it immediately.
	b.Begin()
	if g := admit(t, b, "prod", "worker", c.Now()); !g.Admitted() {
		t.Fatalf("a denied admission must not start a cooldown, got %s", g)
	}
}

func TestAdmit_WithoutABeginDenies(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)

	g := admit(t, b, "prod", "api", base)
	if g.Admitted() {
		t.Fatalf("an admission before any pass must be denied, got %s", g)
	}
	if g.Reason != ReasonNoPass {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonNoPass)
	}
	// The wiring bug must still be visible to an operator, so it is a suppression too.
	if got := b.Status().Suppressions; len(got) != 1 || got[0].Reason != ReasonNoPass.String() {
		t.Errorf("a no-pass denial must be reported as a suppression, got %+v", got)
	}
}

func TestAdmit_ClusterMismatchDenies(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	g := b.Admit("prod", target("staging", "api"), base)
	if g.Admitted() {
		t.Fatalf("a target in another cluster must never be admitted, got %s", g)
	}
	if g.Reason != ReasonClusterMismatch {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonClusterMismatch)
	}
}

func TestAdmit_EmptyClusterDenies(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	if g := b.Admit("", target("", "api"), base); g.Admitted() {
		t.Fatalf("an unnamed cluster must never be admitted, got %s", g)
	}
}

func TestAdmit_InvalidLimitsDenyRatherThanGoUnbounded(t *testing.T) {
	// The dangerous reading of a zero cap is "no limit". Assert the safe one.
	for name, limits := range map[string]Limits{
		"zero value":       {},
		"no cap":           {Cooldown: time.Minute, FailureThreshold: 1},
		"no cooldown":      {PerClusterPerPass: 1, FailureThreshold: 1},
		"no failure floor": {PerClusterPerPass: 1, Cooldown: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			b := NewMemory(limits, newClock().Now)
			b.Begin()
			g := admit(t, b, "prod", "api", base)
			if g.Admitted() {
				t.Fatalf("invalid limits must deny, got %s", g)
			}
			if g.Reason != ReasonLimitsInvalid {
				t.Errorf("reason = %s, want %s", g.Reason, ReasonLimitsInvalid)
			}
		})
	}
}

func TestRecordOutcome_SuccessAsksForNothing(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()
	admit(t, b, "prod", "api", base)

	c := b.RecordOutcome("prod", target("prod", "api"), OutcomeSucceeded, base)
	if c.Acted() {
		t.Fatalf("a successful auto-apply asks for nothing, got %+v", c)
	}
	if c.ConsecutiveFailures != 0 {
		t.Errorf("consecutive failures = %d, want 0", c.ConsecutiveFailures)
	}
}

func TestRecordOutcome_FailureAlwaysRollsBackDemotesAndEscalates(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	c := b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, base)
	switch {
	case !c.RollBack:
		t.Error("a failed auto-apply must ask for a rollback")
	case !c.Demote:
		t.Error("a failed auto-apply must demote the shape")
	case !c.Escalate:
		t.Error("a failed auto-apply must escalate to a human")
	}
	// One failure of two does not yet trip the breaker.
	if c.Tripped {
		t.Errorf("the breaker must not trip on failure 1 of %d", DefaultFailureThreshold)
	}
	if c.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", c.ConsecutiveFailures)
	}
}

func TestRecordOutcome_UnrecordedCountsAsAFailure(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)

	// An outcome nobody set, and one from a build this one does not know, must both
	// count against the breaker rather than being ignored.
	for _, o := range []Outcome{OutcomeUnrecorded, Outcome(99)} {
		c := b.RecordOutcome("prod", target("prod", "api"), o, base)
		if !c.Escalate {
			t.Errorf("outcome %s must be treated as a failure, got %+v", o, c)
		}
	}
}

func TestRecordOutcome_BreakerTripsOnceAndBlocksTheCluster(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	second := b.RecordOutcome("prod", target("prod", "web"), OutcomeFailed, c.Now())
	if !second.Tripped {
		t.Fatalf("failure %d of %d must trip the breaker, got %+v", 2, DefaultFailureThreshold, second)
	}

	// The transition is announced once, not on every subsequent failure.
	third := b.RecordOutcome("prod", target("prod", "worker"), OutcomeFailed, c.Now())
	if third.Tripped {
		t.Error("a failure against an already-open breaker must not report a fresh trip")
	}

	// And the cluster is now fully gated, whatever the cap and cooldown would say.
	c.advance(DefaultCooldown * 10)
	b.Begin()
	g := admit(t, b, "prod", "never-touched", c.Now())
	if g.Admitted() {
		t.Fatalf("an open breaker must block every auto-apply, got %s", g)
	}
	if g.Reason != ReasonBreakerTripped {
		t.Errorf("reason = %s, want %s", g.Reason, ReasonBreakerTripped)
	}
}

func TestRecordOutcome_BreakerIsPerCluster(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	for range DefaultFailureThreshold {
		b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	}
	if g := admit(t, b, "staging", "api", c.Now()); !g.Admitted() {
		t.Fatalf("a tripped breaker on one cluster must not gate another, got %s", g)
	}
}

func TestRecordOutcome_SuccessResetsTheFailureRun(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)

	b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	b.RecordOutcome("prod", target("prod", "api"), OutcomeSucceeded, c.Now())
	// The run restarted, so this is failure 1 of 2 and must not trip.
	again := b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	if again.Tripped {
		t.Fatalf("a success must reset the consecutive count, got %+v", again)
	}
	if again.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", again.ConsecutiveFailures)
	}
}

func TestRecordOutcome_InvalidThresholdTripsOnTheFirstFailure(t *testing.T) {
	// A zero threshold must not mean "never trips". The fail-closed reading is 1.
	b := NewMemory(Limits{PerClusterPerPass: 1, Cooldown: time.Minute}, newClock().Now)
	c := b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, base)
	if !c.Tripped {
		t.Fatalf("an invalid threshold must trip on the first failure, got %+v", c)
	}
}

func TestClear_IsTheOnlyWayBackAndNeedsAName(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	for range DefaultFailureThreshold {
		b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	}

	if err := b.Clear("prod", "   ", c.Now()); err == nil {
		t.Fatal("clearing the breaker without naming who cleared it must be refused")
	}
	if g := admit(t, b, "prod", "api", c.Now()); g.Reason != ReasonBreakerTripped {
		t.Fatalf("a refused clear must leave the breaker open, got %s", g)
	}

	if err := b.Clear("prod", "gigi", c.Now()); err != nil {
		t.Fatalf("clearing the breaker: %v", err)
	}
	b.Begin()
	if g := admit(t, b, "prod", "web", c.Now()); !g.Admitted() {
		t.Fatalf("a cleared breaker must admit again, got %s", g)
	}
}

func TestClear_LeavesCooldownsInPlace(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	admit(t, b, "prod", "api", c.Now())
	for range DefaultFailureThreshold {
		b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	}
	if err := b.Clear("prod", "gigi", c.Now()); err != nil {
		t.Fatalf("clearing the breaker: %v", err)
	}

	// The object whose failure tripped the breaker must not be immediately re-acted on.
	b.Begin()
	if g := admit(t, b, "prod", "api", c.Now()); g.Reason != ReasonTargetCoolingDown {
		t.Fatalf("a clear must not wipe cooldowns, got %s", g)
	}
}

func TestClear_OnAClosedBreakerIsANoOp(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	if err := b.Clear("prod", "gigi", base); err != nil {
		t.Fatalf("clearing a closed breaker must be safe to repeat: %v", err)
	}
	b.Begin()
	if g := admit(t, b, "prod", "api", base); !g.Admitted() {
		t.Fatalf("a no-op clear must leave autonomy working, got %s", g)
	}
}

func TestTrip_OpensTheBreakerWithoutAFailureRun(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	b.Trip("prod", "an anomalous burst of 40 proposals in one pass", c.Now())
	g := admit(t, b, "prod", "api", c.Now())
	if g.Reason != ReasonBreakerTripped {
		t.Fatalf("a directly tripped breaker must block admissions, got %s", g)
	}

	tripped := b.Status().Tripped()
	if len(tripped) != 1 || !strings.Contains(tripped[0].Detail, "anomalous burst") {
		t.Fatalf("the caller's reason must be recorded, got %+v", tripped)
	}
}

func TestTrip_WithoutADetailStillSaysSomething(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Trip("prod", "  ", base)

	tripped := b.Status().Tripped()
	if len(tripped) != 1 || tripped[0].Detail == "" {
		t.Fatalf("a tripped breaker must always carry a detail, got %+v", tripped)
	}
}

func TestStatus_EmptyMeansAllClearAndIsNeverNil(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.Begin()

	s := b.Status()
	switch {
	case s.Sealed:
		t.Error("a fresh in-memory budget is not sealed")
	case s.Breakers == nil:
		t.Error("Breakers must be an empty slice rather than nil — the summary prints it unconditionally")
	case s.Suppressions == nil:
		t.Error("Suppressions must be an empty slice rather than nil")
	case len(s.Tripped()) != 0:
		t.Errorf("no breaker can be tripped on a fresh budget, got %+v", s.Tripped())
	}
}

func TestStatus_ReportsSuppressionsForThePassAndResetsWithIt(t *testing.T) {
	c := newClock()
	b := NewMemory(DefaultLimits(), c.Now)
	b.Begin()

	admit(t, b, "prod", "api", c.Now())
	admit(t, b, "prod", "web", c.Now())
	admit(t, b, "prod", "worker", c.Now()) // over the cap

	s := b.Status()
	if len(s.Suppressions) != 1 {
		t.Fatalf("one suppression expected, got %+v", s.Suppressions)
	}
	sup := s.Suppressions[0]
	switch {
	case sup.Cluster != "prod":
		t.Errorf("cluster = %q, want prod", sup.Cluster)
	case sup.Target != "deployment/default/worker":
		t.Errorf("target = %q, want deployment/default/worker", sup.Target)
	case sup.Reason != ReasonPassCapReached.String():
		t.Errorf("reason = %q, want %s", sup.Reason, ReasonPassCapReached)
	case sup.At.IsZero():
		t.Error("a suppression must record when it happened")
	}

	// A suppression describes one pass. The next pass starts clean, so a stale
	// suppression cannot be read as a current one.
	b.Begin()
	if got := b.Status().Suppressions; len(got) != 0 {
		t.Errorf("a new pass clears the previous pass's suppressions, got %+v", got)
	}
}

func TestStatus_ReportsAClosedBreakersFailureCount(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, base)

	s := b.Status()
	if len(s.Breakers) != 1 {
		t.Fatalf("one cluster expected, got %+v", s.Breakers)
	}
	// One failure away from tripping is worth seeing before the trip, not after.
	if s.Breakers[0].Tripped || s.Breakers[0].ConsecutiveFailures != 1 {
		t.Errorf("a closed breaker must still report its failure run, got %+v", s.Breakers[0])
	}
	if len(s.Tripped()) != 0 {
		t.Errorf("a closed breaker is not a tripped one, got %+v", s.Tripped())
	}
}

func TestStatus_BreakersAreSortedByCluster(t *testing.T) {
	b := NewMemory(DefaultLimits(), newClock().Now)
	for _, name := range []string{"prod", "dev", "staging"} {
		b.Trip(name, "test", base)
	}

	var got []string
	for _, br := range b.Status().Breakers {
		got = append(got, br.Cluster)
	}
	want := []string{"dev", "prod", "staging"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("breakers = %v, want %v — a report over a map must be ordered", got, want)
		}
	}
}

func TestAdmit_IsSafeUnderConcurrentCallers(t *testing.T) {
	// A cap two goroutines can both pass is not a cap. Run the race detector over
	// this with `go test -race`.
	b := NewMemory(Limits{PerClusterPerPass: 5, Cooldown: time.Hour, FailureThreshold: 2}, newClock().Now)
	b.Begin()

	const callers = 32
	var wg sync.WaitGroup
	admitted := make(chan struct{}, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct targets, so only the cap can bound them.
			if g := b.Admit("prod", target("prod", string(rune('a'+i))), base); g.Admitted() {
				admitted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(admitted)

	if n := len(admitted); n != 5 {
		t.Fatalf("%d admissions got through a cap of 5", n)
	}
}
