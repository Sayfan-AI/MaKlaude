package execute

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/score"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// Milestone 6's scoring half (issue #195): every timing scenario gets a verdict on the
// human's two questions — did the action fix the fault, and should it have been allowed
// — and the verdict is derived from what MaKlaude RECORDED rather than from what the
// scenario seeded.
//
// # Why the scoring lives here and not beside the scorer
//
// [score] is tested against hand-built facts, which is the only way to reach the bars a
// correct system never crosses. That leaves one thing unproven and it is the thing that
// matters: whether a real run's real audit trail carries enough for the scorer to reach
// the right conclusion at all. A projection that dropped a field, or an execution layer
// that never populated one, would pass every test in that package and score every real
// run as unassessable.
//
// So the scenarios are scored here, from the trail a real [Runner.Execute] produced
// against the real cluster model, through the real permission slips. The scenarios
// themselves are the ones `faultinjection_test.go` defines; this file re-runs them and
// asks a different question about each. Their own assertions stay where they are — this
// file must not become the place window 2's behaviour is pinned, or a scoring change
// could quietly relax a behavioural guarantee.
//
// # The import direction, and why the tokens are mirrored
//
// [score] deliberately does not import this package: if it did, this file could not
// exist, because an internal test cannot import a package that imports the package under
// test. So the convergence tokens are declared on both sides and
// [TestScoreConvergenceTokensMatch] holds them together from the side that owns them.

// TestScoreConvergenceTokensMatch is the guard on that mirror.
//
// [score] matches on convergence tokens it declares itself, because it cannot import
// this package. A rename here with no rename there would not fail to compile — it would
// silently make every applied action score as [score.FixUnknown], which reads as "the
// cluster was unobservable" and is the most plausible-looking wrong answer available.
func TestScoreConvergenceTokensMatch(t *testing.T) {
	cases := map[Convergence]string{
		ConvergenceConverged:    score.TokenConverged,
		ConvergenceTimedOut:     score.TokenTimedOut,
		ConvergenceUnobservable: score.TokenUnobservable,
		ConvergenceUnobserved:   score.TokenUnobserved,
	}
	for verdict, mirrored := range cases {
		if verdict.String() != mirrored {
			t.Errorf("%T(%d) renders %q and score mirrors it as %q", verdict, verdict, verdict.String(), mirrored)
		}
	}
	if len(cases) != int(ConvergenceUnobservable)+1 {
		t.Fatalf("the mirror covers %d verdicts and the enum declares %d; a new one needs a token on both sides",
			len(cases), int(ConvergenceUnobservable)+1)
	}
}

// TestScoreEveryTimingScenario is task T6's first criterion over task T5's scenarios:
// each one produces a scored verdict on both questions, from the trail alone.
//
// Every case here is soundly permitted — these are correct runs — so what the table
// pins is that question 1's answer distinguishes the five outcomes that all "look like a
// failure" from the outside: nothing sent, sent and cleanly rejected, sent and rejected
// unhealthily, applied and not settled, applied and settled.
func TestScoreEveryTimingScenario(t *testing.T) {
	cases := []struct {
		name      string
		run       func(t *testing.T) (*harness, remediate.Proposal)
		wantFix   score.Fix
		wantGrade score.Grade
		why       string
	}{
		{
			name: "window 1: the fault lands before the precondition re-check",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
				h := newHarness(t, model, fastPolicy())
				fault := podKill("shop", "web-dead")
				h.observer.beforeRead = func(read int) {
					if read == 1 {
						fault.apply(model)
					}
				}
				return h, deletePodProposal()
			},
			wantFix:   score.FixNotAttempted,
			wantGrade: score.GradeClean,
			why:       "no request left the process, so no fix was claimed and none can have failed",
		},
		{
			name: "window 2: the target moves between the re-check and the write",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
				h := newHarness(t, model, fastPolicy())
				fault := podFailure("shop", "web-dead")
				h.mutator.beforeWrite = func() { fault.apply(model) }
				return h, deletePodProposal()
			},
			wantFix:   score.FixCleanlyAborted,
			wantGrade: score.GradeClean,
			why:       "the API server enforced the precondition MaKlaude sent; the cluster is unchanged and re-proposing is the answer",
		},
		{
			name: "window 2: the target is destroyed between the re-check and the write",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
				h := newHarness(t, model, fastPolicy())
				fault := podKill("shop", "web-dead")
				h.mutator.beforeWrite = func() { fault.apply(model) }
				return h, deletePodProposal()
			},
			wantFix:   score.FixNotApplied,
			wantGrade: score.GradeUnfixed,
			// This is issue #214 restated as a score, which is worth more than restating
			// it as prose. The pod is gone — the action's whole goal was reached — and
			// because a 404 classifies as execute-failed rather than a clean abort, the
			// record says nothing landed and the scorecard grades the fault unfixed. When
			// #214 is fixed this expectation changes to cleanly-aborted and clean.
			why: "the record says the request was rejected on a class that calls for a human, so the trail cannot claim a fix",
		},
		{
			name: "window 3: the fault lands during the observation window",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
				h := newHarness(t, model, shortWindow())
				fault := podKill("shop", "web-abc")
				h.observer.beforeRead = func(read int) {
					if read == 2 {
						model.rollOut("shop", "web", 5)
						fault.apply(model)
						model.dropReadyReplica("shop", "web")
					}
				}
				return h, restartProposal()
			},
			wantFix:   score.FixNotConverged,
			wantGrade: score.GradeUnfixed,
			// The grade scores the OUTCOME, not the implementation. MaKlaude did the
			// right thing here — it reported rather than retrying or rolling back — and
			// the fault is still there, which is what unfixed means.
			why: "the action landed and the expected state never appeared inside the window",
		},
		{
			name: "window 3: the fault lands after the verdict",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
				h := newHarness(t, model, shortWindow())
				h.observer.beforeRead = func(read int) {
					if read == 2 {
						model.rollOut("shop", "web", 5)
					}
				}
				return h, restartProposal()
			},
			wantFix:   score.FixConverged,
			wantGrade: score.GradeClean,
			why:       "the observation saw the expected state; a fault arriving afterwards is outside what this verdict is about",
		},
		{
			name: "no fault at all: the control",
			run: func(t *testing.T) (*harness, remediate.Proposal) {
				return newHarness(t, newClusterModel().withNode("node-a"), fastPolicy()), cordonProposal()
			},
			wantFix:   score.FixConverged,
			wantGrade: score.GradeClean,
			why:       "an undisturbed human-approved action converges and crosses no bar",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p := tc.run(t)

			// The error is deliberately ignored: several of these scenarios fail on
			// purpose, and what is under test is the RECORD they left, not the return.
			_, _ = h.execute(p)

			card := scoreOne(t, h, nil)

			if card.Fix != tc.wantFix {
				t.Errorf("fix = %s, want %s (%s)\n%s", card.Fix, tc.wantFix, tc.why, card.Explain())
			}
			if card.Grade != tc.wantGrade {
				t.Errorf("grade = %s, want %s\n%s", card.Grade, tc.wantGrade, card.Explain())
			}
			if !card.SoundlyPermitted() {
				t.Errorf("a correct run was scored over-permitted:\n%s", card.Explain())
			}
			if card.Identity != p.Identity {
				t.Errorf("the card names %q, want the action's identity %q", card.Identity, p.Identity)
			}
		})
	}
}

// TestScoreAConvergingActionThatShouldNotHaveBeenAllowed is task T6's second criterion,
// produced end to end rather than described: the action runs through the real runner,
// really lands, really converges, and scores as a failure because the record shows a
// cluster-scoped target applied with nobody watching.
//
// # Why this is reachable, and why it therefore needs scoring
//
// [autonomy.Decide] never returns auto-apply for a cluster-scoped target
// ([autonomy.ReasonClusterScopedTarget]), so a correct decision path cannot produce this
// run. What produces it is the gap [approve.GrantAutonomous] documents about itself: the
// verdict and the grant it takes are plain structs, and "neither argument is
// unforgeable". A wiring bug, a hand-built verdict, or a regression in the selector
// ladder mints a slip the decision function would never have issued — and every layer
// below then behaves correctly, because a valid permission slip is exactly what they are
// built to honour.
//
// That is the case an independent scorer exists for. It reads the record, which is what
// an incident review reads, and it does not trust the layer that produced it.
func TestScoreAConvergingActionThatShouldNotHaveBeenAllowed(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()

	rep, err := h.runner.Execute(context.Background(), overPermittedAuthorization(t, p), p)
	if err != nil {
		t.Fatalf("executing the action: %v", err)
	}

	// The premise, checked rather than assumed: this scenario is only interesting if the
	// action genuinely worked. A scorecard condemning an action that failed anyway would
	// prove nothing about the dominance rule.
	if !rep.Executed || rep.Convergence != ConvergenceConverged {
		t.Fatalf("executed=%t convergence=%s, want a real execution that converged", rep.Executed, rep.Convergence)
	}
	if !model.node("node-a").Unschedulable {
		t.Fatal("the node is still schedulable, so the fix did not actually land")
	}

	card := scoreOne(t, h, nil)

	if !card.Fixed() {
		t.Fatalf("fix = %s, want converged\n%s", card.Fix, card.Explain())
	}
	if card.Grade != score.GradeOverPermitted {
		t.Fatalf("grade = %s, want over-permitted: a converged fix must not launder a permission fault\n%s",
			card.Grade, card.Explain())
	}
	if !hasFault(card, score.FaultClusterScopedAutoApplied) {
		t.Fatalf("faults = %v, want cluster-scoped-auto-applied\n%s", card.Faults, card.Explain())
	}

	// The same action with a person behind it is sound, and asserting that here is what
	// makes the case above about the bar rather than about cordoning nodes.
	control := newHarness(t, newClusterModel().withNode("node-a"), fastPolicy())
	if _, err := control.execute(cordonProposal()); err != nil {
		t.Fatalf("executing the human-approved control: %v", err)
	}
	if controlCard := scoreOne(t, control, nil); controlCard.Grade != score.GradeClean {
		t.Fatalf("the human-approved control graded %s\n%s", controlCard.Grade, controlCard.Explain())
	}
}

// TestScoreAConvergenceInsideARecordedChaosWindow is the verdict only the recorded window
// can produce, over a real trail.
//
// The run is the undisturbed control from the table above: it converges and crosses no
// bar. Scored against a recorded quarantine window covering the attempt, the fix verdict
// stops being attributable — while an experiment is live, Chaos Mesh reverting the fault
// explains the recovery just as well as the remediation does. The trust ledger already
// refuses this outcome as evidence; a scorer that admitted it would be a second opinion
// on the same evidence that contradicted the first.
func TestScoreAConvergenceInsideARecordedChaosWindow(t *testing.T) {
	h := newHarness(t, newClusterModel().withNode("node-a"), fastPolicy())
	if _, err := h.execute(cordonProposal()); err != nil {
		t.Fatalf("executing an approved action: %v", err)
	}

	// A real window log rather than a hand-built value, so the overlap is decided by the
	// same End-or-Until rule the ledger uses.
	log := trust.NewMemoryWindows()
	if _, err := log.Begin(testCluster, "chaos experiment pod-kill", time.Now().Add(-time.Minute), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("opening a quarantine window: %v", err)
	}

	card := scoreOne(t, h, log.All())

	if card.Fix != score.FixConvergedUnderChaos {
		t.Fatalf("fix = %s, want converged-under-chaos\n%s", card.Fix, card.Explain())
	}
	if card.Grade != score.GradeUnproven {
		t.Errorf("grade = %s, want unproven", card.Grade)
	}
	if !card.SoundlyPermitted() {
		t.Errorf("an experiment running is not a permission fault: %v", card.Faults)
	}
	if card.Fixed() {
		t.Error("the card claims the action fixed the fault; the record cannot attribute the recovery to it")
	}
}

// TestScoreIsReproducibleFromTheStoredScorecard is task T6's third criterion over a real
// run: the verdict is recomputed from a file, after the fact, with no cluster, no
// harness, and nothing re-executed.
func TestScoreIsReproducibleFromTheStoredScorecard(t *testing.T) {
	h := newHarness(t, newClusterModel().withNode("node-a"), fastPolicy())
	p := cordonProposal()
	if _, err := h.execute(p); err != nil {
		t.Fatalf("executing an approved action: %v", err)
	}

	log := trust.NewMemoryWindows()
	if _, err := log.Begin(testCluster, "chaos experiment pod-kill", time.Now().Add(-time.Minute), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("opening a quarantine window: %v", err)
	}

	path := filepath.Join(t.TempDir(), "scorecard.json")
	bundle := score.NewBundle(score.EvidenceFrom(h.records(), log.All()))
	if err := score.WriteFile(path, bundle); err != nil {
		t.Fatalf("storing the scorecard: %v", err)
	}

	restored, err := score.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the scorecard back: %v", err)
	}
	replayed, err := score.Replay(restored)
	if err != nil {
		t.Fatalf("replaying the stored scorecard: %v", err)
	}
	if len(replayed) != 1 || !replayed[0].Equal(bundle.Cards[0]) {
		t.Fatalf("replay produced %+v, want %+v", replayed, bundle.Cards)
	}
	if replayed[0].Fix != score.FixConvergedUnderChaos {
		t.Fatalf("fix = %s after the round trip, want converged-under-chaos — the recorded window did not survive storage",
			replayed[0].Fix)
	}
}

// overPermittedAuthorization mints a real policy permission slip for an action no
// ruleset may auto-apply, standing in for the wiring bug or regression that could
// produce one. The slip itself is genuine: [approve.GrantAutonomous] built it, so every
// layer below honours it exactly as it would honour any other.
func overPermittedAuthorization(t *testing.T, p remediate.Proposal) *approve.Authorization {
	t.Helper()

	verdict := autonomy.Verdict{
		Decision: autonomy.DecisionAutoApply,
		Reason:   autonomy.ReasonEarnedTrust,
		Rule:     "cordon-prod",
		Evidence: "3 of the last 3 human-approved cordons on this cluster converged",
	}
	grant := budget.Grant{Reason: budget.ReasonAdmitted, Cluster: p.Cluster, Target: p.Target.String()}

	auth, err := approve.GrantAutonomous(approve.Request{Proposal: p}, verdict, grant, "disclosure-7", time.Now().UTC())
	if err != nil {
		t.Fatalf("minting a policy authorization: %v", err)
	}
	return auth
}

// scoreOne scores the single action a harness's trail holds, failing the test if the
// trail holds anything other than exactly one.
func scoreOne(t *testing.T, h *harness, windows []trust.Window) score.Card {
	t.Helper()

	cards := score.Cards(score.EvidenceFrom(h.records(), windows))
	if len(cards) != 1 {
		t.Fatalf("the trail holds %d action(s), want exactly 1 (phases: %v)", len(cards), h.phases())
	}
	return cards[0]
}

// hasFault reports whether the card lists the given fault.
func hasFault(c score.Card, want score.Fault) bool {
	for _, f := range c.Faults {
		if f == want {
			return true
		}
	}
	return false
}
