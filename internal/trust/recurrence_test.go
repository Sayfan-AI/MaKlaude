package trust

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// These tests are the answer to the last and heaviest done criterion of issue #167:
// "the convergence check is strong enough to carry the safety weight the window used
// to carry. A test seeds a fix that appears to converge and does not, and asserts
// trust is withdrawn."
//
// The window's real job was never counting. It was forcing a shape back in front of a
// person on a schedule REGARDLESS of what MaKlaude's own health signal claimed, which
// mattered because that signal can be wrong in the optimistic direction — the
// milestone-1 crashloop detector read a pod one instant after a restart, saw it was
// not currently in CrashLoopBackOff, and called it healthy. Removing the window
// without replacing that property would leave a fix that appears to work and does not
// trusted forever, because nothing would ever demote it.
//
// [Ledger.NoteRecurrence] is the replacement, and it is strictly better evidence than
// a counter: it demotes on the fault actually coming back rather than on a number
// rolling over.

const recurringIdentity = remediate.ProposalIdentity("proposal|rolloutrestart|prod|deployment/payments/web")

// convergedHistory builds the history of a fix that ran, was approved, and reported success.
// The entries carry the identity, which is what a recurrence is matched on.
func convergedHistory(t *testing.T, n int) *Ledger {
	t.Helper()

	l := NewMemory()
	for i := 0; i < n; i++ {
		if err := l.Record(Entry{
			Key:         string(recurringIdentity) + "@" + base.Add(time.Duration(i)*time.Hour*24).Format(time.RFC3339Nano),
			Identity:    recurringIdentity,
			Shape:       shape,
			Fingerprint: fixtureFP,
			Authority:   audit.AuthorityHuman,
			Outcome:     OutcomeConverged,
			At:          base.Add(time.Duration(i) * time.Hour * 24),
			Ref:         refFor(i),
		}); err != nil {
			t.Fatalf("seeding converged execution %d: %v", i, err)
		}
	}
	return l
}

// The headline property. A fix reports convergence, earns trust, and then the fault it
// claimed to fix comes back — so the convergence was a lie and the trust goes with it.
func TestARecurringFaultWithdrawsTrust(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	if !l.Trust(subject).Trusted {
		t.Fatalf("precondition failed: %d converged approvals should be trusted", PromotionThreshold)
	}

	// The fault is diagnosed again, minutes after the last execution said it was fixed.
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)
	if err := l.NoteRecurrence(recurringIdentity, shape, last.Add(5*time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	if ev := l.Trust(subject); ev.Trusted {
		t.Fatalf("a fix that did not hold kept its trust: %+v", ev)
	}
	st := l.Standing(subject)
	if st.Blocker.Outcome != OutcomeRegressed {
		t.Errorf("Blocker.Outcome = %s, want %s", st.Blocker.Outcome, OutcomeRegressed)
	}
	if got := l.Explain(subject); !strings.Contains(got, OutcomeRegressed.String()) {
		t.Errorf("Explain() = %q, want it to name the regression", got)
	}

	// The regression is recorded, not substituted for the convergence. Both happened and
	// an operator reconstructing the incident needs to see the claim and its refutation.
	if got, want := l.Len(), PromotionThreshold+1; got != want {
		t.Errorf("the ledger holds %d entries, want %d: the regression must be added, not replace anything", got, want)
	}
}

// A fault returning long after the fix is a new incident, not a fix that failed to
// hold. Demoting for it would punish a fix that worked for a day, and the two errors
// are not symmetric — see [RecurrenceHorizon].
func TestAFaultReturningPastTheHorizonIsNotARegression(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)

	if err := l.NoteRecurrence(recurringIdentity, shape, last.Add(RecurrenceHorizon+time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	if !l.Trust(subject).Trusted {
		t.Errorf("a fault returning past the horizon demoted the shape: %s", l.Explain(subject))
	}
	if got, want := l.Len(), PromotionThreshold; got != want {
		t.Errorf("the ledger holds %d entries, want %d: nothing should have been recorded", got, want)
	}
}

// A recurrence is about ONE object. A shape's history spans every object the operation
// has touched, and a deployment crashlooping again says nothing about the restart of a
// different deployment that is still holding.
func TestARecurrenceIsScopedToTheProposal(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)

	other := remediate.ProposalIdentity("proposal|rolloutrestart|prod|deployment/payments/api")
	if err := l.NoteRecurrence(other, shape, last.Add(5*time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	if !l.Trust(subject).Trusted {
		t.Errorf("a different object's fault demoted this fix: %s", l.Explain(subject))
	}
}

// Nothing is recorded when there is no claim of convergence to contradict. A fault
// being diagnosed is the ordinary case — it is only a regression relative to a
// specific recent assertion that it was fixed.
func TestNoRecurrenceWithoutAConvergenceToContradict(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func() *Ledger
	}{
		{"no history at all", NewMemory},
		{
			name: "the previous attempt failed rather than converged",
			seed: func() *Ledger { return history(t, "f") },
		},
		{
			name: "the previous attempt was inconclusive",
			seed: func() *Ledger { return history(t, "i") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.seed()
			before := l.Len()
			if err := l.NoteRecurrence(recurringIdentity, shape, base.Add(time.Minute)); err != nil {
				t.Fatalf("noting the recurrence: %v", err)
			}
			if got := l.Len(); got != before {
				t.Errorf("the ledger grew from %d to %d entries; a fault with no convergence behind it is "+
					"an ordinary fault, not a regression", before, got)
			}
		})
	}
}

// One recurrence is one demotion however many times the cycle observes it. A pass that
// re-reads the same unfixed fault must not stack entries, or a single event would read
// as a shape failing repeatedly.
func TestRecurrenceIsIdempotent(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)

	for i := 0; i < 5; i++ {
		// Deliberately at DIFFERENT instants, because that is what repeated passes look
		// like. The key is derived from the contradicted execution rather than from now,
		// which is what makes this idempotent — see [Ledger.NoteRecurrence].
		at := last.Add(time.Duration(i+1) * time.Minute)
		if err := l.NoteRecurrence(recurringIdentity, shape, at); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	if got, want := l.Len(), PromotionThreshold+1; got != want {
		t.Errorf("five observations of one recurrence recorded %d entries, want %d", got, want)
	}
}

// A regression demotes every fix of the shape, not only the one that regressed. Same
// asymmetry as every other demoting outcome: a fix that had to be reapplied is
// evidence about this operation on this cluster, and letting a re-fingerprinted
// proposal walk past it would turn "the fix changed" into a way to launder a failure.
func TestARegressionBlocksTheWholeShape(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)
	if err := l.NoteRecurrence(recurringIdentity, shape, last.Add(time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	if ev := l.Trust(changed); ev.Trusted {
		t.Fatalf("a differently-fingerprinted fix walked past the regression: %+v", ev)
	}
	if st := l.Standing(changed); !st.Blocked || st.Blocker.Outcome != OutcomeRegressed {
		t.Errorf("Standing(%s) = blocked %v by %s, want blocked by %s",
			changed, st.Blocked, st.Blocker.Outcome, OutcomeRegressed)
	}
}

// The recurrence is filed against the fingerprint of the execution it contradicts, not
// against whatever the proposal in hand carries. Blaming a fix that has not run yet for
// its predecessor's failure would leave the predecessor's approvals intact, which is
// exactly backwards.
func TestARecurrenceIsFiledAgainstTheFixThatFailedToHold(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)
	if err := l.NoteRecurrence(recurringIdentity, shape, last.Add(time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	var regression Entry
	for _, e := range l.Entries() {
		if e.Outcome == OutcomeRegressed {
			regression = e
		}
	}
	if regression.Fingerprint != fixtureFP {
		t.Errorf("the regression carries fingerprint %q, want %q — the fix that failed to hold",
			regression.Fingerprint, fixtureFP)
	}
	if regression.Identity != recurringIdentity {
		t.Errorf("the regression carries identity %q, want %q", regression.Identity, recurringIdentity)
	}
}

// An auto-applied execution that regresses demotes exactly like a human-approved one.
// [Entry.Counts] excludes only the auto-applied SUCCESS: autonomy cannot earn itself
// more autonomy, and it is fully able to lose the autonomy it has.
func TestAnAutoAppliedFixThatRegressesStillDemotes(t *testing.T) {
	l := convergedHistory(t, PromotionThreshold)
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)

	// The unattended run: converged, so not recorded at all.
	autoAt := last.Add(time.Hour * 24)
	if err := l.Record(Entry{
		Key:         string(recurringIdentity) + "@" + autoAt.Format(time.RFC3339Nano),
		Identity:    recurringIdentity,
		Shape:       shape,
		Fingerprint: fixtureFP,
		Authority:   audit.AuthorityPolicy,
		Outcome:     OutcomeConverged,
		At:          autoAt,
	}); err != nil {
		t.Fatalf("recording the auto-applied success: %v", err)
	}
	if got, want := l.Len(), PromotionThreshold; got != want {
		t.Fatalf("the auto-applied success was stored (%d entries, want %d)", got, want)
	}

	// So the fault returning contradicts the last HUMAN-approved convergence, which is
	// the newest one the ledger holds. It is well past the horizon from that one, so the
	// unattended success being unrecorded means the recurrence is not attributable — and
	// that is the honest answer rather than a demotion invented from nothing.
	if err := l.NoteRecurrence(recurringIdentity, shape, autoAt.Add(5*time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}
	if !l.Trust(subject).Trusted {
		t.Errorf("trust was withdrawn on a recurrence no recorded convergence claims to have fixed: %s",
			l.Explain(subject))
	}

	// A policy-authorized execution that FAILED is recorded, and demotes.
	failAt := autoAt.Add(time.Hour)
	if err := l.Record(Entry{
		Key:         string(recurringIdentity) + "@" + failAt.Format(time.RFC3339Nano),
		Identity:    recurringIdentity,
		Shape:       shape,
		Fingerprint: fixtureFP,
		Authority:   audit.AuthorityPolicy,
		Outcome:     OutcomeFailed,
		At:          failAt,
	}); err != nil {
		t.Fatalf("recording the auto-applied failure: %v", err)
	}
	if ev := l.Trust(subject); ev.Trusted {
		t.Fatalf("an unattended failure left the shape trusted: %+v", ev)
	}
}

// A recurrence survives a restart like every other entry, because it is an ordinary
// ledger entry rather than in-process state. A demotion that evaporated on restart
// would be the worst possible kind: the shape would silently regain autonomy the next
// time the process came up.
func TestARecurrenceSurvivesARestart(t *testing.T) {
	path := t.TempDir() + "/trust.jsonl"

	l, err := Open(path)
	if err != nil {
		t.Fatalf("opening the ledger: %v", err)
	}
	for _, e := range convergedHistory(t, PromotionThreshold).Entries() {
		if err := l.Record(e); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)
	if err := l.NoteRecurrence(recurringIdentity, shape, last.Add(time.Minute)); err != nil {
		t.Fatalf("noting the recurrence: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopening the ledger: %v", err)
	}
	if ev := reopened.Trust(subject); ev.Trusted {
		t.Fatalf("the regression did not survive the restart: %+v", ev)
	}
}

// The outcome token is on-disk format, so it has to round-trip. An unreadable token
// fails the whole ledger open rather than degrading, which is what stops a newer
// build's history reading here as a mystery.
func TestRegressedRoundTripsThroughTheWireFormat(t *testing.T) {
	if got, ok := parseOutcome(OutcomeRegressed.String()); !ok || got != OutcomeRegressed {
		t.Fatalf("parseOutcome(%q) = %s, %v; want %s, true", OutcomeRegressed.String(), got, ok, OutcomeRegressed)
	}
	if !OutcomeRegressed.Demotes() {
		t.Error("OutcomeRegressed does not demote; a fix that had to be reapplied is not a fix")
	}
	if OutcomeRegressed.Promotes() {
		t.Error("OutcomeRegressed promotes")
	}
	if !(Entry{Authority: audit.AuthorityPolicy, Outcome: OutcomeRegressed}).Counts() {
		t.Error("an auto-applied regression is excluded from the history; only the auto-applied SUCCESS is")
	}
}

// The clock is the caller's. Everything else in this package is reproducible from
// recorded history, and a promotion decision that depended on when it was asked would
// not be — so the horizon is measured against an instant that is passed in.
func TestRecurrenceReadsNoClock(t *testing.T) {
	last := base.Add(time.Duration(PromotionThreshold-1) * time.Hour * 24)

	inside := convergedHistory(t, PromotionThreshold)
	if err := inside.NoteRecurrence(recurringIdentity, shape, last.Add(time.Minute)); err != nil {
		t.Fatalf("inside the horizon: %v", err)
	}
	outside := convergedHistory(t, PromotionThreshold)
	if err := outside.NoteRecurrence(recurringIdentity, shape, last.Add(2*RecurrenceHorizon)); err != nil {
		t.Fatalf("outside the horizon: %v", err)
	}

	if inside.Trust(subject).Trusted {
		t.Error("the supplied instant inside the horizon did not demote")
	}
	if !outside.Trust(subject).Trusted {
		t.Error("the supplied instant outside the horizon demoted anyway, so a real clock is being read somewhere")
	}

	// An instant BEFORE the execution is nonsense — a clock that went backwards, a
	// restored backup — and must not be read as a recurrence zero seconds after the fix.
	backwards := convergedHistory(t, PromotionThreshold)
	if err := backwards.NoteRecurrence(recurringIdentity, shape, base.Add(-time.Hour)); err != nil {
		t.Fatalf("with an instant before the history: %v", err)
	}
	if !backwards.Trust(subject).Trusted {
		t.Error("an instant before the execution was treated as a recurrence of it")
	}
}

var _ autonomy.TrustOracle = (*Ledger)(nil)
