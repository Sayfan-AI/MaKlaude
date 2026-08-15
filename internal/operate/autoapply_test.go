package operate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// These tests drive the WHOLE unattended path through the real pipeline — a crashlooping
// deployment in a fake cluster produces a real proposal, the real policy layer decides it,
// the real budget admits it, the real runner executes it, and the real disclosure trail
// records it. Nothing is constructed by hand except the operator's rules and the trust
// history, which are the two inputs a person supplies.
//
// The reason for driving it end to end rather than unit-testing the sequencing is that
// the sequencing IS the subject. Every layer below already refuses correctly on its own;
// what nobody had tested before this file is that they are called in the order that makes
// those refusals mean anything.

const autoRule = "test-rollout-restart"

// earnedTrust is the trust posture in which the test cluster's rollout restarts have been
// earned. [autonomy.StaticTrust] is a real [autonomy.TrustOracle], so this pins the trust
// input exactly without standing up a ledger the test would then be testing.
func earnedTrust() shapeTrust {
	return shapeTrust{autonomy.Shape{Cluster: testCluster, Operation: remediate.OpRolloutRestart}: true}
}

// shapeTrust trusts every fingerprint of the shapes it names.
//
// [autonomy.StaticTrust] would need the exact [remediate.Fingerprint] the cycle
// computes, which these tests would have to recompute from a hand-built proposal — and
// a hand-built proposal that drifted in any fingerprinted field would silently make
// every case below assert that an UNTRUSTED shape gates, which is what they all do
// anyway. Trusting the shape keeps the fingerprint out of tests that are about the
// unattended path; the fingerprint's own effect on trust is asserted where it belongs,
// in promotion_test.go and the trust package.
type shapeTrust map[autonomy.Shape]bool

func (s shapeTrust) Trust(subject autonomy.Subject) autonomy.TrustEvidence {
	if !s[subject.Shape] {
		return autonomy.TrustEvidence{}
	}
	return autonomy.TrustEvidence{
		Trusted:  true,
		Citation: "3 human-approved rolloutrestarts of this fix converged",
	}
}

// permissiveRuleset permits exactly the action the crashlooping fixture provokes.
func permissiveRuleset() autonomy.Ruleset {
	return autonomy.Ruleset{{
		Name:       autoRule,
		Clusters:   []string{testCluster},
		Namespaces: []string{testNamespace},
		Operations: []remediate.Operation{remediate.OpRolloutRestart},
	}}
}

// recordingLedger captures what the cycle asked the trust ledger to record, and can be
// told to fail — the case that must not be silent, since a failed demotion leaves a shape
// trusted after an unattended failure.
type recordingLedger struct {
	lifecycles  [][]audit.Record
	recurrences []remediate.ProposalIdentity
	err         error
}

func (l *recordingLedger) RecordLifecycle(recs []audit.Record) error {
	l.lifecycles = append(l.lifecycles, recs)
	return l.err
}

// NoteRecurrence records what the cycle offered as a possible regression. A fake ledger
// holds no history, so nothing here can contradict a claim of convergence and every
// offer is a no-op — which is the point: these tests are about the unattended path, and
// a fake that invented demotions would change their subject.
func (l *recordingLedger) NoteRecurrence(id remediate.ProposalIdentity, _ autonomy.Shape, _ time.Time) error {
	l.recurrences = append(l.recurrences, id)
	return l.err
}

// autonomousCycle builds an execution-enabled cycle with autonomy fully wired.
func autonomousCycle(t *testing.T) (*Cycle, *mutatorFactory, *disclose.MemorySink, *recordingLedger) {
	t.Helper()

	c, factory, _ := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
	sink := disclose.NewMemorySink()
	trail, err := disclose.NewTrail(sink, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	ledger := &recordingLedger{}

	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), trail.WithClock(func() time.Time { return fixedTime }), ledger)
	return c, factory, sink, ledger
}

// run executes one pass and returns the report.
func run(t *testing.T, c *Cycle) *Report {
	t.Helper()
	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// onlyAutoApplied returns the single unattended action the pass took, failing otherwise.
func onlyAutoApplied(t *testing.T, report *Report) AutoApplyReport {
	t.Helper()
	if len(report.Clusters) != 1 {
		t.Fatalf("the report covers %d clusters, want 1", len(report.Clusters))
	}
	applied := report.Clusters[0].AutoApplied
	if len(applied) != 1 {
		t.Fatalf("the pass auto-applied %d actions, want 1: %+v", len(applied), applied)
	}
	return applied[0]
}

// --- The central test. --------------------------------------------------------------

// TestRun_AutoAppliesAnEarnedActionAndDisclosesIt is the whole task in one pass.
//
// It asserts the four things that together make an unattended action acceptable: it
// happened, it was authorized by a named rule rather than by a person, it consumed
// blast-radius budget, and it left a record naming all of that.
func TestRun_AutoAppliesAnEarnedActionAndDisclosesIt(t *testing.T) {
	c, factory, sink, _ := autonomousCycle(t)
	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	// It happened, through the real write path.
	if !applied.Execution.Executed {
		t.Fatalf("the action did not execute: %+v", applied)
	}
	if calls := factory.realCalls(); len(calls) != 1 {
		t.Fatalf("the write path saw %d requests, want 1: %v", len(calls), calls)
	}

	// Nobody approved it, and the record says which rule did.
	if applied.Rule != autoRule {
		t.Errorf("Rule = %q, want %q", applied.Rule, autoRule)
	}
	if applied.Evidence == "" {
		t.Error("the report carries no trust evidence for an action nobody approved")
	}
	if applied.Reason != autonomy.ReasonEarnedTrust.String() {
		t.Errorf("Reason = %q, want %q", applied.Reason, autonomy.ReasonEarnedTrust)
	}
	if applied.Execution.Approver != "policy:"+autoRule {
		t.Errorf("Approver = %q, want policy:%s", applied.Execution.Approver, autoRule)
	}
	if applied.Execution.Authority != "policy" {
		t.Errorf("Authority = %q, want policy", applied.Execution.Authority)
	}

	// The ceiling was consumed, not merely consulted.
	if applied.Admission != budget.ReasonAdmitted.String() {
		t.Errorf("Admission = %q, want %q", applied.Admission, budget.ReasonAdmitted)
	}

	// And it is disclosed on its own artifact.
	if applied.Disclosure == "" {
		t.Fatal("the action was taken with no disclosure artifact")
	}
	view, ok := sink.Snapshot(disclose.Ref(applied.Disclosure))
	if !ok {
		t.Fatalf("the disclosure %q does not exist", applied.Disclosure)
	}
	if !strings.Contains(view.Body, "NO HUMAN APPROVED THIS ACTION") {
		t.Error("the disclosure does not say that no human approved the action")
	}
	if !strings.Contains(view.Body, "policy:"+autoRule) {
		t.Error("the disclosure does not name the rule that permitted the action")
	}
	if !view.HasLabel(disclose.AppliedLabel) {
		t.Error("the disclosure was not marked applied after a real mutation landed")
	}
	if !view.Open {
		t.Error("the disclosure was closed by MaKlaude; closing it is a person's acknowledgement")
	}
}

// TestRun_AnAutoAppliedActionIsNeverAlsoPutToAHuman. Asking about an action already taken
// would invite somebody to approve what has already happened.
func TestRun_AnAutoAppliedActionIsNeverAlsoPutToAHuman(t *testing.T) {
	c, _, _, _ := autonomousCycle(t)
	report := run(t, c)

	cr := report.Clusters[0]
	if len(cr.AutoApplied) != 1 {
		t.Fatalf("the pass auto-applied %d actions, want 1", len(cr.AutoApplied))
	}
	if cr.Gate.Opened != 0 {
		t.Errorf("the gate opened %d artifacts for actions already taken", cr.Gate.Opened)
	}
	if cr.Gate.Authorized != 0 {
		t.Errorf("the gate authorized %d actions the unattended half had already run", cr.Gate.Authorized)
	}
}

// TestRun_WithoutAutonomyWiredNothingIsAutoApplied is the shipped posture, and it is the
// posture a cycle built by [New] has. Each of the four dependencies is removed on its own,
// because the failure worth catching is a half-configured autonomy that looks configured.
func TestRun_WithoutAutonomyWiredNothingIsAutoApplied(t *testing.T) {
	cases := map[string]func(c *Cycle, trail *disclose.Trail){
		"no rules":      func(c *Cycle, tr *disclose.Trail) { c.UseAutonomy(nil, earnedTrust(), tr, nil) },
		"no oracle":     func(c *Cycle, tr *disclose.Trail) { c.UseAutonomy(permissiveRuleset(), nil, tr, nil) },
		"no disclosure": func(c *Cycle, _ *disclose.Trail) { c.UseAutonomy(permissiveRuleset(), earnedTrust(), nil, nil) },
		"nothing at all": func(_ *Cycle, _ *disclose.Trail) {
			// A cycle that never calls UseAutonomy, which is what [New] produces.
		},
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			c, factory, _ := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
			trail, err := disclose.NewTrail(disclose.NewMemorySink(), notify.NewNopNotifier())
			if err != nil {
				t.Fatalf("building the disclosure trail: %v", err)
			}
			c.UseBudget(memoryBudget())
			wire(c, trail)

			report := run(t, c)
			if got := len(report.Clusters[0].AutoApplied); got != 0 {
				t.Fatalf("%d actions were auto-applied with autonomy half-wired", got)
			}
			if calls := factory.realCalls(); len(calls) != 0 {
				t.Fatalf("the write path saw %d requests: %v", len(calls), calls)
			}
			// The proposal is not lost — it takes the human gate, exactly as before.
			if report.Clusters[0].Gate.Opened != 1 {
				t.Errorf("the gate opened %d artifacts, want 1: a proposal not auto-applied still goes to a person",
					report.Clusters[0].Gate.Opened)
			}
		})
	}
}

// TestRun_WithNoBudgetAutoAppliesNothingEvenWithRulesAndTrust. Eligibility with no ceiling
// is the failure the budget package's doc opens with, so its absence must block the path
// rather than leave it unbounded.
func TestRun_WithNoBudgetAutoAppliesNothingEvenWithRulesAndTrust(t *testing.T) {
	c, factory, _ := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
	trail, err := disclose.NewTrail(disclose.NewMemorySink(), notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), trail, nil)

	report := run(t, c)
	if got := len(report.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions were auto-applied with no blast-radius ceiling", got)
	}
	if calls := factory.realCalls(); len(calls) != 0 {
		t.Fatalf("the write path saw %d requests with no ceiling: %v", len(calls), calls)
	}
}

// TestRun_AnUntrustedShapeTakesTheHumanGate is the steady state on a fresh install: the
// machinery is real and everything still gates until a history exists.
func TestRun_AnUntrustedShapeTakesTheHumanGate(t *testing.T) {
	c, factory, sink, _ := autonomousCycle(t)
	// A ruleset that permits it, and a ledger that has never seen it.
	c.UseAutonomy(permissiveRuleset(), autonomy.StaticTrust{}, mustTrail(t, sink), nil)

	report := run(t, c)
	if got := len(report.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions were auto-applied for a shape with no history", got)
	}
	if calls := factory.realCalls(); len(calls) != 0 {
		t.Fatalf("the write path saw %d requests: %v", len(calls), calls)
	}
	if report.Clusters[0].Gate.Opened != 1 {
		t.Error("the untrusted proposal did not reach the human gate")
	}
}

// --- Revocation. ---------------------------------------------------------------------

// TestRun_AHumanRevocationStopsTheNextCycle is the revocation criterion end to end: one
// label on the artifact the previous pass produced, and the next pass gates instead.
func TestRun_AHumanRevocationStopsTheNextCycle(t *testing.T) {
	c, factory, sink, _ := autonomousCycle(t)

	first := run(t, c)
	applied := onlyAutoApplied(t, first)
	if len(factory.realCalls()) != 1 {
		t.Fatalf("the first pass did not act: %v", factory.realCalls())
	}

	// A person applies one label to the disclosure and does nothing else.
	if err := sink.Revoke(disclose.Ref(applied.Disclosure)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// The next pass. A fresh budget so the cooldown, not the revocation, is not what
	// stops it — this test is about the revocation and nothing else.
	c.UseBudget(memoryBudget())
	second := run(t, c)

	if got := len(second.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions were auto-applied after a human revoked the shape", got)
	}
	if calls := factory.realCalls(); len(calls) != 1 {
		t.Fatalf("the write path saw %d requests in total, want the first pass's 1: %v", len(calls), calls)
	}
	if len(second.Clusters[0].RevokedByHuman) != 1 {
		t.Fatalf("the report does not say a revocation is why nothing happened: %+v", second.Clusters[0])
	}
	if !strings.Contains(second.Clusters[0].RevokedByHuman[0], "human gate") {
		t.Errorf("the report does not say the proposal went to a person instead: %q", second.Clusters[0].RevokedByHuman[0])
	}
	// And it did go to a person rather than being dropped.
	if second.Clusters[0].Gate.Opened != 1 {
		t.Errorf("the revoked proposal did not reach the human gate (opened=%d)", second.Clusters[0].Gate.Opened)
	}
}

// TestRun_ARevocationIsCheckedBeforeBudgetIsConsumed.
//
// Admission CONSUMES — it counts against the pass cap and starts the target's cooldown —
// so asking for it before the revocation check would let a revoked shape burn the pass's
// allowance on actions it is not permitted to take.
func TestRun_ARevocationIsCheckedBeforeBudgetIsConsumed(t *testing.T) {
	c, _, sink, _ := autonomousCycle(t)

	first := run(t, c)
	applied := onlyAutoApplied(t, first)
	if err := sink.Revoke(disclose.Ref(applied.Disclosure)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	b := memoryBudget()
	c.UseBudget(b)
	second := run(t, c)

	if got := len(second.Autonomy.Suppressed); got != 0 {
		t.Errorf("a revoked shape produced %d budget suppressions, so admission ran before the revocation check", got)
	}
	// The allowance is untouched, which is what "not consumed" means in observable terms.
	b.Begin()
	grant := b.Admit(testCluster, budgetTarget(testDeploy), fixedTime)
	if !grant.Admitted() {
		t.Errorf("the pass's allowance was spent on a revoked shape: %s", grant)
	}
}

// TestRun_AnUnreadableDisclosureTrailStopsAllUnattendedAction.
//
// A failed read produces an empty revocation list, an empty list is indistinguishable
// from "nothing is revoked", and acting on that basis would turn a network blip into a
// grant of authority.
func TestRun_AnUnreadableDisclosureTrailStopsAllUnattendedAction(t *testing.T) {
	c, factory, _ := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
	trail, err := disclose.NewTrail(&unlistableSink{MemorySink: disclose.NewMemorySink()}, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), trail, nil)

	report := run(t, c)
	if got := len(report.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions ran while the revocation list was unreadable", got)
	}
	if calls := factory.realCalls(); len(calls) != 0 {
		t.Fatalf("the write path saw %d requests: %v", len(calls), calls)
	}
	if report.Autonomy.RevocationError == "" {
		t.Error("the report does not say the disclosure trail could not be read")
	}
	// And the reader is told, in the text summary, that this is why nothing happened.
	text := reportText(t, report)
	if !strings.Contains(text, "THE DISCLOSURE TRAIL COULD NOT BE READ") {
		t.Errorf("the summary does not report the unreadable trail:\n%s", text)
	}
}

// unlistableSink fails only ListOpen.
type unlistableSink struct{ *disclose.MemorySink }

func (s *unlistableSink) ListOpen(context.Context) ([]disclose.Disclosed, error) {
	return nil, errors.New("the trail is unreachable")
}

// --- Bounds and consequences. ---------------------------------------------------------

// TestRun_ASuppressedAutoApplyStillGoesToAHuman. A bound says "not unattended", not "not
// at all" — so a tripped breaker produces the fully-gated posture the breaker is for
// rather than a cluster where MaKlaude has stopped proposing.
func TestRun_ASuppressedAutoApplyStillGoesToAHuman(t *testing.T) {
	c, factory, _, _ := autonomousCycle(t)
	c.Budget().Trip(testCluster, "a person tripped it", fixedTime)

	report := run(t, c)
	if got := len(report.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions were auto-applied on a cluster whose breaker is open", got)
	}
	if calls := factory.realCalls(); len(calls) != 0 {
		t.Fatalf("the write path saw %d requests behind an open breaker: %v", len(calls), calls)
	}
	if report.Clusters[0].Gate.Opened != 1 {
		t.Error("the suppressed proposal did not reach the human gate")
	}
	if len(report.Autonomy.Suppressed) != 1 {
		t.Errorf("the budget recorded %d suppressions, want 1", len(report.Autonomy.Suppressed))
	}
}

// TestRun_AFailedUnattendedActionRecordsTheOutcomeAndCarriesTheConsequence is the T4
// wiring obligation: [budget.Budget.RecordOutcome] has a caller, and its [budget.Consequence]
// is acted on rather than returned and dropped.
//
// The fake clientset never reconciles, so the action lands and the observation window
// times out — which is precisely the "it ran and did not do what it was supposed to"
// case the breaker exists for.
func TestRun_AFailedUnattendedActionRecordsTheOutcomeAndCarriesTheConsequence(t *testing.T) {
	c, _, sink, ledger := autonomousCycle(t)

	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	if !applied.Execution.Executed {
		t.Fatalf("the action did not run, so this test proves nothing about its outcome: %+v", applied)
	}
	if applied.Execution.Convergence == "converged" {
		t.Fatal("the fake cluster converged, so there is no failure to record")
	}
	if !applied.Escalated {
		t.Error("a failed unattended action was not escalated; nobody was watching when it ran")
	}
	if !applied.Demoted {
		t.Error("a failed unattended action did not demote the shape")
	}
	if len(ledger.lifecycles) != 1 {
		t.Fatalf("the trust ledger was asked to record %d lifecycles, want 1", len(ledger.lifecycles))
	}

	// The breaker has counted it, and the summary says so before it trips.
	breakers := report.Autonomy.Breakers
	if len(breakers) != 1 || breakers[0].ConsecutiveFailures != 1 {
		t.Fatalf("the breaker did not count the failure: %+v", breakers)
	}

	view, _ := sink.Snapshot(disclose.Ref(applied.Disclosure))
	if !view.HasLabel(disclose.NeedsHumanLabel) {
		t.Error("the disclosure was not marked needs:human")
	}
	if !strings.Contains(view.Body, "## What followed") {
		t.Error("the disclosure does not say what followed the failure")
	}
}

// TestRun_EveryUnattendedOutcomeReachesTheTrustRecorder.
//
// The unattended path hands the recorder EVERY finished lifecycle, success or failure —
// it does not filter, because [internal/rebuild] derives entries from every completed
// disclosure, and a live path that held some outcomes back would build a different
// history than a rebuild of the same artifacts. What belongs in the evaluation window
// is the ledger's own rule ([trust.Entry.Counts], which drops the auto-applied
// success), decided behind the recorder rather than in front of it. Issue #166 settled
// this: an earlier version of this test asserted the opposite — that a success must
// never reach the ledger — which was one half of the two-package contradiction that
// issue records.
func TestRun_EveryUnattendedOutcomeReachesTheTrustRecorder(t *testing.T) {
	c, _, _, ledger := autonomousCycle(t)

	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	if !applied.Execution.Executed {
		t.Fatalf("the action did not run, so there is no lifecycle to record: %+v", applied)
	}
	if len(ledger.lifecycles) != 1 {
		t.Fatalf("the trust recorder was handed %d lifecycles for one finished action, want 1", len(ledger.lifecycles))
	}
	if len(ledger.lifecycles[0]) == 0 {
		t.Fatal("the recorder was handed an empty lifecycle, which a rebuild of the artifacts would never produce")
	}
}

// TestRun_AFailedDemotionIsReportedRatherThanSwallowed. A demotion that failed silently
// leaves a shape trusted after an unattended failure, which is the one direction this
// system must not fail in.
func TestRun_AFailedDemotionIsReportedRatherThanSwallowed(t *testing.T) {
	c, _, sink, _ := autonomousCycle(t)
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), mustTrail(t, sink),
		&recordingLedger{err: errors.New("the ledger file is read-only")})

	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	if applied.Demoted {
		t.Error("the report claims a demotion the ledger refused")
	}
	view, _ := sink.Snapshot(disclose.Ref(applied.Disclosure))
	if !strings.Contains(view.Body, "The shape could not be demoted") {
		t.Errorf("the disclosure does not report the failed demotion:\n%s", view.Body)
	}
}

// TestRun_WithNoLedgerSaysTheShapeWasNotReGated. A cycle may legitimately run against an
// oracle it does not write back to; what it may not do is report a demotion that did not
// happen.
func TestRun_WithNoLedgerSaysTheShapeWasNotReGated(t *testing.T) {
	c, _, sink, _ := autonomousCycle(t)
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), mustTrail(t, sink), nil)

	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	if applied.Demoted {
		t.Error("the report claims a demotion with no ledger to record it in")
	}
	view, _ := sink.Snapshot(disclose.Ref(applied.Disclosure))
	if !strings.Contains(view.Body, "no trust ledger is wired") {
		t.Errorf("the disclosure does not say the failure did not re-gate the shape:\n%s", view.Body)
	}
}

// TestRun_ARehearsalIsDisclosedAndCountsAgainstNothing.
//
// A dry run changed nothing, so counting it would trip a breaker over a rehearsal and take
// a cluster fully gated for doing exactly what dry-run mode is for.
func TestRun_ARehearsalIsDisclosedAndCountsAgainstNothing(t *testing.T) {
	c, factory, _ := newCycle(t, kube.ExecuteDryRun, crashloopingWorkload()...)
	sink := disclose.NewMemorySink()
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), mustTrail(t, sink), &recordingLedger{})

	report := run(t, c)
	applied := onlyAutoApplied(t, report)

	if applied.Execution.Executed {
		t.Fatal("a dry-run pass reported a real mutation")
	}
	if len(factory.realCalls()) != 0 {
		t.Fatalf("a dry-run pass reached the real write path: %v", factory.realCalls())
	}
	for _, br := range report.Autonomy.Breakers {
		if br.ConsecutiveFailures != 0 {
			t.Errorf("a rehearsal counted against cluster %s's breaker (%d failures)", br.Cluster, br.ConsecutiveFailures)
		}
	}
	view, _ := sink.Snapshot(disclose.Ref(applied.Disclosure))
	if !strings.Contains(view.Body, "This is a **rehearsal**") {
		t.Error("the rehearsal is not disclosed as one")
	}
}

// TestRun_DisabledModeNeverReachesTheUnattendedHalf. The opt-in check comes first, so the
// default posture builds no executor whether or not autonomy is configured.
func TestRun_DisabledModeNeverReachesTheUnattendedHalf(t *testing.T) {
	c, factory, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	sink := disclose.NewMemorySink()
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), earnedTrust(), mustTrail(t, sink), &recordingLedger{})

	report := run(t, c)
	if got := len(report.Clusters[0].AutoApplied); got != 0 {
		t.Fatalf("%d actions were auto-applied with the kill switch off", got)
	}
	if calls := factory.calls(); len(calls) != 0 {
		t.Fatalf("a disabled cycle built %d mutators: %v", len(calls), calls)
	}
	if sink.OpenCount() != 0 {
		t.Errorf("a disabled cycle opened %d disclosures", sink.OpenCount())
	}
}

// --- Reporting. -----------------------------------------------------------------------

// TestReport_AutoAppliedSectionIsPrintedUnconditionally is the empty-means-all-clear rule
// applied to the one number an operator most needs to be able to confirm.
func TestReport_AutoAppliedSectionIsPrintedUnconditionally(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
	c.UseBudget(memoryBudget())

	text := reportText(t, run(t, c))
	if !strings.Contains(text, "auto-applied (no human): none") {
		t.Errorf("a pass that auto-applied nothing does not say so:\n%s", text)
	}
	if !strings.Contains(text, "auto-applied=0") {
		t.Errorf("the totals do not report the auto-applied count:\n%s", text)
	}
}

// TestReport_AutoAppliedActionsAreShoutedRatherThanListed. The line an operator scans has
// to distinguish an action a person agreed to from one nobody did.
func TestReport_AutoAppliedActionsAreShoutedRatherThanListed(t *testing.T) {
	c, _, _, _ := autonomousCycle(t)
	text := reportText(t, run(t, c))

	if !strings.Contains(text, "AUTO-APPLIED WITH NO HUMAN REVIEW (1)") {
		t.Errorf("the summary does not flag the unattended action:\n%s", text)
	}
	if !strings.Contains(text, "rule "+autoRule) {
		t.Errorf("the summary does not name the rule that permitted it:\n%s", text)
	}
	if !strings.Contains(text, "auto-applied=1") {
		t.Errorf("the totals do not count the unattended action:\n%s", text)
	}
}

// TestReport_AutoAppliedRoundTripsThroughJSON keeps the machine-readable form usable by
// whatever reads it next.
func TestReport_AutoAppliedRoundTripsThroughJSON(t *testing.T) {
	c, _, _, _ := autonomousCycle(t)
	report := run(t, c)

	var buf strings.Builder
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	for _, want := range []string{`"autoApplied"`, `"rule"`, `"evidence"`, `"disclosure"`, `"authority": "policy"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the JSON report does not carry %s", want)
		}
	}
}

// mustTrail builds a disclosure trail over an existing sink, on the test clock.
func mustTrail(t *testing.T, sink disclose.Sink) *disclose.Trail {
	t.Helper()
	trail, err := disclose.NewTrail(sink, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	return trail.WithClock(func() time.Time { return fixedTime })
}

// reportText renders a report's text summary.
func reportText(t *testing.T, report *Report) string {
	t.Helper()
	var buf strings.Builder
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return buf.String()
}
