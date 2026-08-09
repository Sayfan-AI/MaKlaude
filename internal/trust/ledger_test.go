package trust

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The fixture shape every test builds a history for, plus a second one so the tests
// that care about isolation have something to be isolated from.
var (
	shape = autonomy.Shape{Cluster: "prod", Operation: remediate.OpRolloutRestart}
	other = autonomy.Shape{Cluster: "staging", Operation: remediate.OpRolloutRestart}
)

// base is the instant the synthetic histories start at. Fixed rather than derived
// from time.Now so a failing citation assertion prints the same string on every run.
var base = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)

// history builds a ledger from a compact spelling of a synthetic history: one rune
// per execution, oldest first, spaced a minute apart so the ordering is unambiguous.
//
//	h — human-approved, converged (the only rune that builds trust)
//	p — policy-waived (auto-applied), converged
//	i — inconclusive (the observation window saw nothing conclusive)
//	f — failed
//	r — rolled back
//	d — drift-aborted
//
// The compact spelling is the point: a promotion rule about counts inside a sliding
// window is read wrong far more easily from twelve struct literals than from
// "hhhf", and every scenario below is one string.
func history(t *testing.T, spelling string) *Ledger {
	t.Helper()
	l := NewMemory()
	for i, r := range spelling {
		if err := l.Record(entryAt(t, shape, r, i)); err != nil {
			t.Fatalf("recording %q at index %d: %v", string(r), i, err)
		}
	}
	return l
}

// entryAt builds the entry one rune of a spelling stands for.
func entryAt(t *testing.T, s autonomy.Shape, kind rune, i int) Entry {
	t.Helper()

	e := Entry{
		Key:   fmt.Sprintf("%s-%d", s, i),
		Shape: s,
		At:    base.Add(time.Duration(i) * time.Minute),
	}
	switch kind {
	case 'h':
		e.Authority, e.Outcome, e.Ref = audit.AuthorityHuman, OutcomeConverged, refFor(i)
	case 'p':
		e.Authority, e.Outcome = audit.AuthorityPolicy, OutcomeConverged
	case 'F':
		e.Authority, e.Outcome = audit.AuthorityPolicy, OutcomeFailed
	case 'i':
		e.Authority, e.Outcome, e.Ref = audit.AuthorityHuman, OutcomeInconclusive, refFor(i)
	case 'f':
		e.Authority, e.Outcome, e.Ref = audit.AuthorityHuman, OutcomeFailed, refFor(i)
	case 'r':
		e.Authority, e.Outcome, e.Ref = audit.AuthorityHuman, OutcomeRolledBack, refFor(i)
	case 'd':
		e.Authority, e.Outcome, e.Ref = audit.AuthorityHuman, OutcomeDriftAborted, refFor(i)
	default:
		t.Fatalf("unknown history rune %q", string(kind))
	}
	return e
}

func refFor(i int) string {
	return fmt.Sprintf("https://github.com/Sayfan-AI/MaKlaude/issues/%d", 1000+i)
}

// repeat spells a run of one kind of execution.
func repeat(kind rune, n int) string { return strings.Repeat(string(kind), n) }

func TestShapeWithNoHistoryIsUntrusted(t *testing.T) {
	l := NewMemory()

	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("a shape with no recorded history was trusted: %+v", ev)
	}
	if got := l.Explain(shape); !strings.Contains(got, "no recorded executions") {
		t.Errorf("explanation does not say the history is empty: %q", got)
	}
}

// The promotion bar itself: below the threshold nothing is trusted, at it the shape
// is, and the boundary is exercised from both sides so an off-by-one cannot pass.
func TestPromotionNeedsThresholdHumanApprovedConvergedExecutions(t *testing.T) {
	for n := 0; n <= PromotionThreshold+1; n++ {
		spelling := repeat('h', n)
		want := n >= PromotionThreshold

		l := history(t, spelling)
		if got := l.Trust(shape).Trusted; got != want {
			t.Errorf("history %q: trusted = %v, want %v (%d approvals, threshold %d)",
				spelling, got, want, n, PromotionThreshold)
		}
	}
}

// The asymmetry the package doc argues for: autonomy must not manufacture its own
// evidence. A full window's worth of flawless auto-applied executions earns nothing —
// they are not stored at all (see [Entry.Counts]), so the shape's history reads as
// empty rather than as full of non-approvals.
func TestAutoAppliedSuccessesDoNotBuildTrust(t *testing.T) {
	l := history(t, repeat('p', EvaluationWindow))

	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("%d auto-applied converged executions promoted the shape: %+v", EvaluationWindow, ev)
	}
	if st := l.Standing(shape); st.Approved != 0 || st.Recorded != 0 {
		t.Errorf("Approved = %d, Recorded = %d, want 0 and 0: an auto-applied success is not evidence", st.Approved, st.Recorded)
	}
}

// Unattributed executions are the third authority and are worth as little as policy
// ones. Called out separately because the enum's zero value is the one a
// partially-built record carries, and it must not be the one that promotes.
func TestUnattributedExecutionsDoNotBuildTrust(t *testing.T) {
	l := NewMemory()
	for i := 0; i < EvaluationWindow; i++ {
		e := Entry{
			Key:     fmt.Sprintf("unattributed-%d", i),
			Shape:   shape,
			Outcome: OutcomeConverged,
			At:      base.Add(time.Duration(i) * time.Minute),
			Ref:     refFor(i),
			// Authority left at its zero value, audit.AuthorityUnattributed.
		}
		if err := l.Record(e); err != nil {
			t.Fatalf("recording: %v", err)
		}
	}

	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("unattributed executions promoted the shape: %+v", ev)
	}
}

// Every demotion case, each against a history that would otherwise be trusted, so
// the only thing under test is whether that one outcome blocks.
func TestEveryDemotionCaseDemotes(t *testing.T) {
	cases := []struct {
		name    string
		kind    rune
		outcome Outcome
	}{
		{"failure", 'f', OutcomeFailed},
		{"rollback", 'r', OutcomeRolledBack},
		{"drift abort", 'd', OutcomeDriftAborted},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Three approvals, then the demoting execution. Without the last rune this
			// history is trusted; the sibling assertion below proves that.
			trusted := history(t, repeat('h', PromotionThreshold))
			if !trusted.Trust(shape).Trusted {
				t.Fatalf("precondition failed: %d approvals alone should be trusted", PromotionThreshold)
			}

			l := history(t, repeat('h', PromotionThreshold)+string(tc.kind))
			if ev := l.Trust(shape); ev.Trusted {
				t.Fatalf("a %s did not demote: %+v", tc.name, ev)
			}

			st := l.Standing(shape)
			if !st.Blocked {
				t.Fatalf("Standing.Blocked = false after a %s", tc.name)
			}
			if st.Blocker.Outcome != tc.outcome {
				t.Errorf("Blocker.Outcome = %s, want %s", st.Blocker.Outcome, tc.outcome)
			}
			if got := l.Explain(shape); !strings.Contains(got, tc.outcome.String()) {
				t.Errorf("explanation does not name the blocking outcome %s: %q", tc.outcome, got)
			}
		})
	}
}

// Demotion is immediate: the very next Trust call after a failure says no, with no
// decay period and no averaging.
func TestDemotionIsImmediate(t *testing.T) {
	l := history(t, repeat('h', EvaluationWindow-1))
	if !l.Trust(shape).Trusted {
		t.Fatalf("precondition failed: a window of approvals should be trusted")
	}

	failure := entryAt(t, shape, 'f', EvaluationWindow)
	if err := l.Record(failure); err != nil {
		t.Fatalf("recording the failure: %v", err)
	}

	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("trust survived a single failure: %+v", ev)
	}
}

// The recovery path, which is what makes demotion a window property rather than a
// permanent brand: the failure has to age out AND fresh approvals have to accumulate.
func TestDemotionLastsUntilTheFailureLeavesTheWindow(t *testing.T) {
	// One failure followed by a full window of approvals. The failure is entry 0 and
	// the window holds the last EvaluationWindow entries, so it has just fallen out.
	l := history(t, "f"+repeat('h', EvaluationWindow))
	if !l.Trust(shape).Trusted {
		t.Fatalf("trust did not return after the failure aged out: %s", l.Explain(shape))
	}

	// One approval short, and the failure is still inside the window.
	still := history(t, "f"+repeat('h', EvaluationWindow-1))
	if ev := still.Trust(shape); ev.Trusted {
		t.Fatalf("trust returned while the failure was still in the window: %+v", ev)
	}
}

// An inconclusive execution is neither evidence for nor against, and the approved
// parameters do not list it as a demotion case.
func TestInconclusiveNeitherPromotesNorDemotes(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold)+"ii")

	if !l.Trust(shape).Trusted {
		t.Fatalf("inconclusive executions demoted a trusted shape: %s", l.Explain(shape))
	}
	if st := l.Standing(shape); st.Approved != PromotionThreshold {
		t.Errorf("Approved = %d, want %d: an inconclusive execution is not an approval",
			st.Approved, PromotionThreshold)
	}
}

// The reason the window bounds BOTH halves of the rule rather than only the
// failures — see the package doc. Under the looser reading this history stays
// trusted forever on three year-old approvals.
func TestInconclusiveExecutionsEventuallyPushApprovalsOutOfTheWindow(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold)+repeat('i', EvaluationWindow))

	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("approvals that aged out of the window still granted trust: %+v", ev)
	}
	st := l.Standing(shape)
	if st.Blocked {
		t.Errorf("Blocked = true: an inconclusive execution must not read as a demotion")
	}
	if st.Approved != 0 {
		t.Errorf("Approved = %d, want 0: every approval should have left the window", st.Approved)
	}
	if st.Recorded != PromotionThreshold+EvaluationWindow {
		t.Errorf("Recorded = %d, want %d: total history must not be truncated",
			st.Recorded, PromotionThreshold+EvaluationWindow)
	}
}

// Trust is per shape. A cluster earning autonomy must not lend it to another.
func TestTrustDoesNotLeakBetweenShapes(t *testing.T) {
	l := history(t, repeat('h', EvaluationWindow))

	if !l.Trust(shape).Trusted {
		t.Fatalf("precondition failed: %s should be trusted", shape)
	}
	if ev := l.Trust(other); ev.Trusted {
		t.Fatalf("%s inherited trust from %s: %+v", other, shape, ev)
	}
}

// A failure on one shape must not demote another, which is the same isolation seen
// from the demoting side.
func TestDemotionDoesNotLeakBetweenShapes(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold))
	if err := l.Record(entryAt(t, other, 'f', 99)); err != nil {
		t.Fatalf("recording the other shape's failure: %v", err)
	}

	if !l.Trust(shape).Trusted {
		t.Fatalf("a failure on %s demoted %s: %s", other, shape, l.Explain(shape))
	}
}

// The citation is the entire oversight artifact for an action nobody approved, so it
// has to carry the counts, the shape, and a pointer into the record.
func TestCitationStatesTheEvidence(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold))

	ev := l.Trust(shape)
	if !ev.Trusted {
		t.Fatalf("precondition failed: %s", l.Explain(shape))
	}

	for _, want := range []string{
		shape.String(),
		fmt.Sprintf("%d of the last %d", PromotionThreshold, PromotionThreshold),
		"human-approved and converged",
		"no failure, rollback or drift-abort",
		refFor(PromotionThreshold - 1),
	} {
		if !strings.Contains(ev.Citation, want) {
			t.Errorf("citation is missing %q\ngot: %s", want, ev.Citation)
		}
	}
}

// [autonomy.TrustOracle] requires the citation to be stable for a stable history:
// [autonomy.Decide]'s determinism is only as good as this.
func TestCitationIsStableAcrossCalls(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold+1))

	first := l.Trust(shape).Citation
	for i := 0; i < 5; i++ {
		if got := l.Trust(shape).Citation; got != first {
			t.Fatalf("citation varied between calls on identical history:\n%s\n%s", first, got)
		}
	}
}

// An untrusted verdict carries no citation at all. A citation is a reason to act,
// and there is no such thing as a citation for "no" — a caller that read one would
// be reading a reason to gate as evidence for autonomy.
func TestUntrustedVerdictCarriesNoCitation(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold)+"f")

	ev := l.Trust(shape)
	if ev.Trusted || ev.Citation != "" {
		t.Fatalf("an untrusted verdict carried evidence: %+v", ev)
	}
	if l.Explain(shape) == "" {
		t.Error("Explain returned nothing for an untrusted shape")
	}
}

// The window must not depend on the order entries were handed to the ledger — a
// rebuild reads approval artifacts in whatever order the API returns them.
func TestWindowIsIndependentOfInsertionOrder(t *testing.T) {
	// A history whose oldest entry is a failure: in event order it has aged out, and
	// in reverse insertion order a naive implementation would put it last and demote.
	spelling := "f" + repeat('h', EvaluationWindow)

	forward := history(t, spelling)

	backward := NewMemory()
	for i := len(spelling) - 1; i >= 0; i-- {
		if err := backward.Record(entryAt(t, shape, rune(spelling[i]), i)); err != nil {
			t.Fatalf("recording in reverse at %d: %v", i, err)
		}
	}

	if got, want := backward.Trust(shape), forward.Trust(shape); got != want {
		t.Fatalf("insertion order changed the verdict:\nforward:  %+v\nbackward: %+v", want, got)
	}
	if !forward.Trust(shape).Trusted {
		t.Fatalf("precondition failed: the failure should have aged out: %s", forward.Explain(shape))
	}
}

// Recording the same execution twice must not double-count toward promotion. This is
// what lets the live append path and a full rebuild agree.
func TestRecordIsIdempotentOnKey(t *testing.T) {
	l := NewMemory()
	e := entryAt(t, shape, 'h', 0)

	for i := 0; i < PromotionThreshold+2; i++ {
		if err := l.Record(e); err != nil {
			t.Fatalf("re-recording: %v", err)
		}
	}

	if got := l.Len(); got != 1 {
		t.Errorf("Len = %d, want 1: the same key was stored more than once", got)
	}
	if ev := l.Trust(shape); ev.Trusted {
		t.Fatalf("one execution recorded %d times bought trust: %+v", PromotionThreshold+2, ev)
	}
}

// The cache-not-authority guard: the approval artifact is the authority and this
// ledger is a projection of it, so an entry claiming a human approval it cannot
// point at is refused. It is the one check standing between an operator with a text
// editor and self-granted autonomy.
func TestHumanApprovedEntryWithoutAnArtifactIsRejected(t *testing.T) {
	l := NewMemory()
	e := entryAt(t, shape, 'h', 0)
	e.Ref = ""

	err := l.Record(e)
	if err == nil {
		t.Fatal("an entry claiming human approval with no approval artifact was accepted")
	}
	if !strings.Contains(err.Error(), "approval artifact") {
		t.Errorf("error does not name the missing artifact: %v", err)
	}
	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0: a rejected entry must not be stored", l.Len())
	}
}

// A policy-waived entry legitimately has no artifact — nobody approved it — and the
// ones that count must still be recordable: the unattended failure is exactly the
// evidence that re-gates a shape.
func TestPolicyWaivedFailureNeedsNoArtifactAndDemotes(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold))
	if !l.Trust(shape).Trusted {
		t.Fatalf("fixture error: %d approvals did not promote the shape", PromotionThreshold)
	}

	e := entryAt(t, shape, 'F', PromotionThreshold)
	if e.Ref != "" {
		t.Fatalf("fixture error: a policy entry should carry no ref, got %q", e.Ref)
	}
	if err := l.Record(e); err != nil {
		t.Fatalf("a policy-waived failure was rejected: %v", err)
	}
	if st := l.Standing(shape); !st.Blocked || st.Trusted {
		t.Fatalf("an unattended failure did not re-gate the shape: %+v", st)
	}
}

// Counts is the window-membership rule, and its whole truth table is pinned here
// because both [Ledger.Record] and [Ledger.Rebuild] act on it. Exactly one case is
// excluded — the auto-applied success — and every unknown falls on the counting
// (fail-closed) side.
func TestCountsExcludesExactlyTheAutoAppliedSuccess(t *testing.T) {
	cases := []struct {
		name      string
		authority audit.Authority
		outcome   Outcome
		counts    bool
	}{
		{"policy converged is the one exclusion", audit.AuthorityPolicy, OutcomeConverged, false},
		{"policy inconclusive occupies a slot", audit.AuthorityPolicy, OutcomeInconclusive, true},
		{"policy failure demotes", audit.AuthorityPolicy, OutcomeFailed, true},
		{"policy drift-abort demotes", audit.AuthorityPolicy, OutcomeDriftAborted, true},
		{"policy rollback demotes", audit.AuthorityPolicy, OutcomeRolledBack, true},
		{"human converged promotes", audit.AuthorityHuman, OutcomeConverged, true},
		{"human inconclusive occupies a slot", audit.AuthorityHuman, OutcomeInconclusive, true},
		{"unattributed converged counts", audit.AuthorityUnattributed, OutcomeConverged, true},
		{"an unknown outcome counts and demotes", audit.AuthorityPolicy, Outcome(97), true},
		{"an unknown authority counts", audit.Authority(97), OutcomeConverged, true},
	}
	for _, tc := range cases {
		e := Entry{Authority: tc.authority, Outcome: tc.outcome}
		if got := e.Counts(); got != tc.counts {
			t.Errorf("%s: Counts() = %v, want %v", tc.name, got, tc.counts)
		}
	}
}

// The decision recorded on issue #166: an auto-applied success is not evidence in
// either direction. It is dropped at the ledger's door — no error, no entry, no
// window slot — rather than stored and filtered later.
func TestAnAutoAppliedSuccessIsDroppedNotStored(t *testing.T) {
	l := NewMemory()
	if err := l.Record(entryAt(t, shape, 'p', 0)); err != nil {
		t.Fatalf("recording an auto-applied success errored: %v", err)
	}
	if l.Len() != 0 {
		t.Fatalf("Len = %d, want 0: an auto-applied success must not hold a window slot", l.Len())
	}
	if st := l.Standing(shape); st.Recorded != 0 {
		t.Errorf("Recorded = %d, want 0", st.Recorded)
	}
}

// The failure mode the decision on issue #166 exists to prevent, asserted against the
// ledger itself: a trusted shape that keeps converging unattended keeps its trust, no
// matter how many successes are reported. Under the old occupies-a-slot reading the
// tenth success here would have flushed the third approval out of the window and the
// shape would have revoked its own autonomy for working.
func TestAPerfectlyWorkingShapeKeepsItsTrust(t *testing.T) {
	l := history(t, repeat('h', PromotionThreshold))
	if !l.Trust(shape).Trusted {
		t.Fatalf("fixture error: %d approvals did not promote the shape", PromotionThreshold)
	}

	for i := 0; i < 2*EvaluationWindow; i++ {
		if err := l.Record(entryAt(t, shape, 'p', PromotionThreshold+i)); err != nil {
			t.Fatalf("recording auto-applied success %d: %v", i, err)
		}
	}
	if !l.Trust(shape).Trusted {
		t.Fatalf("%d flawless unattended executions revoked the shape's own trust: %s",
			2*EvaluationWindow, l.Explain(shape))
	}
}

// Rebuild applies the same membership rule as the live path — one predicate, two
// callers — so a rebuild from artifacts that include auto-applied successes produces
// the same ledger the live appends did.
func TestRebuildDropsAutoAppliedSuccesses(t *testing.T) {
	l := NewMemory()
	entries := []Entry{
		entryAt(t, shape, 'h', 0),
		entryAt(t, shape, 'p', 1),
		entryAt(t, shape, 'h', 2),
	}
	if err := l.Rebuild(entries); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want 2: the auto-applied success must not survive a rebuild", l.Len())
	}
	for _, e := range l.Entries() {
		if !e.Counts() {
			t.Errorf("a rebuilt ledger holds a non-counting entry: %+v", e)
		}
	}
}

// Every other way an entry can be unusable is rejected rather than stored in a
// degraded form: an unorderable or unclassifiable entry inside the window would make
// the verdict mean something nobody can state.
func TestMalformedEntriesAreRejected(t *testing.T) {
	valid := entryAt(t, shape, 'h', 0)

	cases := []struct {
		name   string
		mutate func(*Entry)
		want   string
	}{
		{"no key", func(e *Entry) { e.Key = "" }, "no key"},
		{"no cluster", func(e *Entry) { e.Shape.Cluster = "" }, "no cluster"},
		{"no operation", func(e *Entry) { e.Shape.Operation = "" }, "no operation"},
		{"no outcome", func(e *Entry) { e.Outcome = OutcomeUnrecorded }, "no recorded outcome"},
		{"no instant", func(e *Entry) { e.At = time.Time{} }, "no execution time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := NewMemory()
			e := valid
			tc.mutate(&e)

			err := l.Record(e)
			if err == nil {
				t.Fatalf("a %s entry was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if l.Len() != 0 {
				t.Errorf("Len = %d, want 0: a rejected entry must not be stored", l.Len())
			}
		})
	}
}

// An outcome value this build has never seen — a newer ledger read by an older
// binary, or a hand-edited number — must demote rather than be tolerated.
func TestUnknownOutcomeValueDemotes(t *testing.T) {
	unknown := Outcome(99)

	if unknown.Promotes() {
		t.Error("an unrecognized outcome promoted")
	}
	if !unknown.Demotes() {
		t.Error("an unrecognized outcome did not demote")
	}
}

// The zero value of every type in the decision path has to be the safe one.
func TestZeroValuesAreSafe(t *testing.T) {
	if OutcomeUnrecorded.Promotes() {
		t.Error("the zero Outcome promotes")
	}
	if !OutcomeUnrecorded.Demotes() {
		t.Error("the zero Outcome does not demote")
	}
	if (Entry{}).Promotes() {
		t.Error("the zero Entry promotes")
	}
	if (Standing{}).Trusted {
		t.Error("the zero Standing is trusted")
	}
}

// The tokens are an on-disk format, so a rename is a compatibility break and has to
// be a deliberate edit to this list rather than a refactor nobody noticed.
func TestOutcomeTokensAreStableAndDistinct(t *testing.T) {
	want := map[Outcome]string{
		OutcomeUnrecorded:   "unrecorded",
		OutcomeConverged:    "converged",
		OutcomeInconclusive: "inconclusive",
		OutcomeFailed:       "failed",
		OutcomeDriftAborted: "drift-aborted",
		OutcomeRolledBack:   "rolled-back",
	}

	seen := map[string]Outcome{}
	for outcome, token := range want {
		if got := outcome.String(); got != token {
			t.Errorf("Outcome(%d).String() = %q, want %q", outcome, got, token)
		}
		if dup, clash := seen[token]; clash {
			t.Errorf("token %q is shared by Outcome(%d) and Outcome(%d)", token, dup, outcome)
		}
		seen[token] = outcome
	}
	if _, ok := parseOutcome("outcome(99)"); ok {
		t.Error("the fallback rendering of an unknown outcome parses as a real one")
	}
}

// The ledger is the oracle the decision layer consumes, so the end-to-end property
// worth asserting is that a history actually flips [autonomy.Decide]. Everything
// above tests the arithmetic; this tests that the arithmetic is wired to the thing
// that decides whether a cluster gets mutated unattended.
func TestLedgerDrivesTheAutonomyDecision(t *testing.T) {
	ruleset := autonomy.Ruleset{{
		Name:             "restart-payments",
		Clusters:         []string{shape.Cluster},
		Namespaces:       []string{"payments"},
		Operations:       []remediate.Operation{remediate.OpRolloutRestart},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
	proposal := remediate.Proposal{
		Cluster:       shape.Cluster,
		Operation:     shape.Operation,
		Reversibility: remediate.ReversibilityReversible,
		Target: remediate.Target{
			Cluster:         shape.Cluster,
			Kind:            "deployment",
			Namespace:       "payments",
			Name:            "web",
			ResourceVersion: "424242",
		},
	}

	untrusted := history(t, repeat('h', PromotionThreshold-1))
	v := autonomy.Decide(shape.Cluster, proposal, ruleset, untrusted)
	if v.Decision != autonomy.DecisionGate || v.Reason != autonomy.ReasonUntrustedShape {
		t.Fatalf("an under-threshold history did not gate: %s", v)
	}

	trusted := history(t, repeat('h', PromotionThreshold))
	v = autonomy.Decide(shape.Cluster, proposal, ruleset, trusted)
	if v.Decision != autonomy.DecisionAutoApply || v.Reason != autonomy.ReasonEarnedTrust {
		t.Fatalf("an earned history did not auto-apply: %s", v)
	}
	if v.Evidence != trusted.Trust(shape).Citation {
		t.Errorf("the verdict's evidence is not the ledger's citation:\n%s\n%s",
			v.Evidence, trusted.Trust(shape).Citation)
	}

	demoted := history(t, repeat('h', PromotionThreshold)+"r")
	v = autonomy.Decide(shape.Cluster, proposal, ruleset, demoted)
	if v.Decision != autonomy.DecisionGate || v.Reason != autonomy.ReasonUntrustedShape {
		t.Fatalf("a rollback did not return the shape to the human gate: %s", v)
	}
}
