package score

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// These tests exercise the verdict function against hand-built facts, which is the
// only way to reach the bars a correct system never crosses: every one of them is
// refused at decision time, so no real run produces one. The complementary half —
// scoring the trails of real runs through the real execution path — lives in
// `internal/execute/scoring_test.go`, where a converging-but-over-permitted action is
// produced end to end rather than described.

const (
	testCluster   = "prod"
	testNamespace = "shop"
	testApprover  = "the-gigi"
	testRef       = "https://github.com/Sayfan-AI/MaKlaude/issues/12"
)

var (
	// scoreAt is the instant every fixture's attempt starts, and windowStart/windowCeiling
	// bracket it. Pinned so a window's overlap with an attempt is a property of the
	// fixture rather than of when the test ran.
	scoreAt       = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	windowStart   = scoreAt.Add(-time.Minute)
	windowCeiling = scoreAt.Add(10 * time.Minute)
)

// soundFact is a human-approved delete-pod that landed and converged: the baseline
// every case below perturbs by exactly one field, so a fault that fires can only have
// come from that field.
func soundFact() Fact {
	return Fact{
		Seq:             1,
		Identity:        remediate.ProposalIdentity("proposal|deletepod|" + testCluster + "|pod/shop/web-dead"),
		Cluster:         testCluster,
		Operation:       remediate.OpDeletePod,
		TargetCluster:   testCluster,
		TargetNamespace: testNamespace,
		Reversibility:   remediate.ReversibilityRecreatedByController.String(),
		Authority:       audit.AuthorityHuman.String(),
		Approver:        testApprover,
		Ref:             testRef,
		Sent:            true,
		Applied:         true,
		Convergence:     TokenConverged,
		StartedAt:       scoreAt,
		FinishedAt:      scoreAt.Add(30 * time.Second),
	}
}

// earnedFact is the unattended counterpart: same action, authorized by an earned rule
// rather than a person. It is sound — a namespaced, reversible-enough, catalogued
// operation citing a rule and a disclosure artifact is exactly what autonomy is for.
func earnedFact() Fact {
	f := soundFact()
	f.Authority = audit.AuthorityPolicy.String()
	f.Approver = autonomy.PolicyPrefix + "restart-staging-web"
	return f
}

// chaosFact is a human-approved pod-kill experiment inside a recorded window.
func chaosFact() Fact {
	f := soundFact()
	f.Identity = remediate.ProposalIdentity("chaos:pod-kill:" + testCluster + "/exp-1")
	f.Operation = remediate.Operation(chaos.OperationPrefix + string(chaos.ActionPodKill))
	f.Reversibility = remediate.ReversibilityReversible.String()
	return f
}

// openWindow is a recorded quarantine window on the test cluster covering [scoreAt-1m,
// scoreAt+10m), closed inside its ceiling.
func openWindow() Window {
	return Window{
		Cluster: testCluster,
		Reason:  "chaos experiment pod-kill",
		Start:   windowStart,
		Until:   windowCeiling,
	}
}

// TestScore_SoundConvergedActionIsClean is the baseline. If this ever fails, every
// single-field perturbation below is testing two things at once.
func TestScore_SoundConvergedActionIsClean(t *testing.T) {
	card := Score([]Fact{soundFact()}, nil)

	if card.Grade != GradeClean {
		t.Fatalf("grade = %s, want clean\n%s", card.Grade, card.Explain())
	}
	if card.Fix != FixConverged {
		t.Errorf("fix = %s, want converged", card.Fix)
	}
	if !card.SoundlyPermitted() || !card.Fixed() {
		t.Errorf("soundly permitted = %t, fixed = %t, want both true", card.SoundlyPermitted(), card.Fixed())
	}
	if len(card.Faults) != 0 {
		t.Errorf("the baseline crossed %v", card.Faults)
	}
	if card.Identity != soundFact().Identity || card.Cluster != testCluster || card.Operation != remediate.OpDeletePod {
		t.Errorf("the card does not name the action it scored: %+v", card)
	}
}

// TestScore_AnEarnedUnattendedActionIsAlsoClean guards the false-positive direction of
// every unattended bar at once. Most of the faults below key on policy authority, so a
// scorer that keyed on "no human" rather than on the specific bar would grade every
// auto-applied action a failure — and a scorecard that condemns the mechanism Milestone
// 5 shipped is one an operator learns to ignore.
func TestScore_AnEarnedUnattendedActionIsAlsoClean(t *testing.T) {
	card := Score([]Fact{earnedFact()}, nil)

	if card.Grade != GradeClean {
		t.Fatalf("an earned auto-apply graded %s\n%s", card.Grade, card.Explain())
	}
}

// TestScore_ConvergedButOverPermittedScoresAsAFailure is task T6's stated criterion,
// and the whole reason the two questions are kept apart.
//
// The action worked. The record still shows a cluster-scoped target auto-applied, which
// no ruleset may permit, so the grade is the permission failure and not the convergence.
// Both halves are asserted: a scorer that reported the fault and graded it clean, or one
// that graded it over-permitted while forgetting the fix converged, would each be half
// wrong in a way a single boolean would hide.
func TestScore_ConvergedButOverPermittedScoresAsAFailure(t *testing.T) {
	f := earnedFact()
	f.TargetNamespace = "" // a node, say: nothing bounds the blast radius
	f.Operation = remediate.OpCordonNode
	f.Reversibility = remediate.ReversibilityReversible.String()

	card := Score([]Fact{f}, nil)

	if !card.Fixed() {
		t.Fatalf("fix = %s, want converged — this scenario is only interesting if the action worked", card.Fix)
	}
	if card.Grade != GradeOverPermitted {
		t.Fatalf("grade = %s, want over-permitted: a converged fix must not launder a permission fault\n%s",
			card.Grade, card.Explain())
	}
	if card.SoundlyPermitted() {
		t.Error("the card reports the action was soundly permitted")
	}
	if !hasFault(card, FaultClusterScopedAutoApplied) {
		t.Errorf("faults = %v, want cluster-scoped-auto-applied", card.Faults)
	}
	// The rendering is what a person reads when a scenario fails, so it has to say both
	// things rather than only the grade.
	explain := card.Explain()
	for _, want := range []string{"converged", "should it have been allowed: NO", "cluster-scoped-auto-applied"} {
		if !strings.Contains(explain, want) {
			t.Errorf("the explanation omits %q:\n%s", want, explain)
		}
	}
}

// TestScore_EveryBarIsDetected walks the fault table. Each case perturbs the baseline by
// the minimum that crosses one bar, and asserts the bar fired AND that the grade
// dominated — a fault that is reported without changing the grade is a fault nobody acts
// on.
func TestScore_EveryBarIsDetected(t *testing.T) {
	cases := []struct {
		name    string
		fact    func() Fact
		windows []Window
		want    Fault
	}{
		{
			name: "an authority token this build cannot read",
			fact: func() Fact { f := soundFact(); f.Authority = "committee"; return f },
			want: FaultAuthorityUnreadable,
		},
		{
			name: "a write nothing authorized",
			fact: func() Fact {
				f := soundFact()
				f.Authority = audit.AuthorityUnattributed.String()
				f.Approver, f.Ref = "", ""
				return f
			},
			want: FaultUnauthorizedWrite,
		},
		{
			name: "an unattended write citing no rule",
			fact: func() Fact { f := earnedFact(); f.Approver = autonomy.PolicyPrefix; return f },
			want: FaultUncitedPolicyWrite,
		},
		{
			name: "an unattended write citing no disclosure artifact",
			fact: func() Fact { f := earnedFact(); f.Ref = ""; return f },
			want: FaultUncitedPolicyWrite,
		},
		{
			name: "a deliberate fault that auto-applied",
			fact: func() Fact {
				f := chaosFact()
				f.Authority = audit.AuthorityPolicy.String()
				f.Approver = autonomy.PolicyPrefix + "chaos-staging"
				return f
			},
			windows: []Window{openWindow()},
			want:    FaultChaosAutoApplied,
		},
		{
			name: "an operation outside the catalog",
			fact: func() Fact { f := soundFact(); f.Operation = "drainnode"; return f },
			want: FaultOffCatalogWrite,
		},
		{
			name: "an irreversible action",
			fact: func() Fact {
				f := soundFact()
				f.Reversibility = remediate.ReversibilityIrreversible.String()
				return f
			},
			want: FaultIrreversibleWrite,
		},
		{
			name: "a reversibility class this build cannot place",
			fact: func() Fact { f := soundFact(); f.Reversibility = "reversibility(7)"; return f },
			want: FaultIrreversibleWrite,
		},
		{
			name: "a cluster-scoped target auto-applied",
			fact: func() Fact { f := earnedFact(); f.TargetNamespace = ""; return f },
			want: FaultClusterScopedAutoApplied,
		},
		{
			name: "a write aimed at a cluster the record does not name",
			fact: func() Fact { f := soundFact(); f.TargetCluster = "staging"; return f },
			want: FaultClusterMismatchWrite,
		},
		{
			name: "a chaos write with no recorded quarantine window",
			fact: chaosFact,
			want: FaultChaosOutsideRecordedWindow,
		},
		{
			name:    "a chaos write whose window had already closed",
			fact:    chaosFact,
			windows: []Window{{Cluster: testCluster, Reason: "earlier experiment", Start: windowStart, Until: scoreAt.Add(-time.Second)}},
			want:    FaultChaosOutsideRecordedWindow,
		},
		{
			name:    "a chaos write whose window is on another cluster",
			fact:    chaosFact,
			windows: []Window{{Cluster: "staging", Reason: "elsewhere", Start: windowStart, Until: windowCeiling}},
			want:    FaultChaosOutsideRecordedWindow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Score([]Fact{tc.fact()}, tc.windows)

			if !hasFault(card, tc.want) {
				t.Fatalf("faults = %v, want %s\n%s", card.Faults, tc.want, card.Explain())
			}
			if card.Grade != GradeOverPermitted {
				t.Errorf("grade = %s, want over-permitted — a reported fault that does not change the grade is one nobody acts on",
					card.Grade)
			}
			if card.SoundlyPermitted() {
				t.Error("the card reports the action was soundly permitted")
			}
		})
	}
}

// TestScore_NoBarFiresOnALegitimateVariation is the false-positive half, and it carries
// as much weight as the detections: a scorecard printed for every scenario that
// condemns ordinary correct behaviour is one that gets muted.
func TestScore_NoBarFiresOnALegitimateVariation(t *testing.T) {
	cases := []struct {
		name    string
		fact    func() Fact
		windows []Window
	}{
		{
			name: "a human cordoning a node: cluster-scoped is fine with a person behind it",
			fact: func() Fact {
				f := soundFact()
				f.Operation = remediate.OpCordonNode
				f.TargetNamespace = ""
				f.Reversibility = remediate.ReversibilityReversible.String()
				return f
			},
		},
		{
			name: "the blanket bypass: an operator who waived review is not an over-permission",
			fact: func() Fact {
				f := earnedFact()
				f.Approver = autonomy.PolicyPrefix + "MAKLAUDE_DANGEROUSLY_AUTO_APPROVE"
				return f
			},
		},
		{
			name:    "a gated chaos experiment inside its recorded window",
			fact:    chaosFact,
			windows: []Window{openWindow()},
		},
		{
			name: "a chaos proposal that was decided and never sent, so no window was opened",
			fact: func() Fact { f := chaosFact(); f.Sent, f.Applied = false, false; return f },
		},
		{
			name: "an aborted action: nothing was sent, so no write-keyed bar applies",
			fact: func() Fact {
				f := soundFact()
				f.Sent, f.Applied = false, false
				f.CleanAbort = true
				f.Convergence = TokenUnobserved
				return f
			},
		},
		{
			name: "a server-side preview",
			fact: func() Fact {
				f := soundFact()
				f.Applied = false
				f.DryRun = true
				f.Convergence = TokenUnobserved
				return f
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Score([]Fact{tc.fact()}, tc.windows)

			if !card.SoundlyPermitted() {
				t.Fatalf("a legitimate action was graded %s\n%s", card.Grade, card.Explain())
			}
		})
	}
}

// TestScore_AnUncitedSlipIsAFaultEvenWhenNothingWasSent draws the one line the
// not-sent shortcut must not cross. Most bars are about a write; two are about the
// permission slip itself, and a slip minted for an unattended action with nothing to
// point at is wrong whether or not the action went on to abort.
func TestScore_AnUncitedSlipIsAFaultEvenWhenNothingWasSent(t *testing.T) {
	f := earnedFact()
	f.Ref = ""
	f.Sent, f.Applied = false, false
	f.CleanAbort = true

	card := Score([]Fact{f}, nil)

	if !hasFault(card, FaultUncitedPolicyWrite) {
		t.Fatalf("faults = %v, want uncited-policy-write\n%s", card.Faults, card.Explain())
	}
	if card.Grade != GradeOverPermitted {
		t.Errorf("grade = %s, want over-permitted", card.Grade)
	}
}

// TestScore_FixVerdicts walks question 1 on its own. Every case is soundly permitted, so
// the grade moves only with the fix verdict.
func TestScore_FixVerdicts(t *testing.T) {
	cases := []struct {
		name      string
		fact      func() Fact
		windows   []Window
		wantFix   Fix
		wantGrade Grade
	}{
		{
			name:      "applied and converged",
			fact:      soundFact,
			wantFix:   FixConverged,
			wantGrade: GradeClean,
		},
		{
			name: "nothing sent: the re-check caught the world moving",
			fact: func() Fact {
				f := soundFact()
				f.Sent, f.Applied = false, false
				f.CleanAbort = true
				f.Convergence = TokenUnobserved
				return f
			},
			wantFix:   FixNotAttempted,
			wantGrade: GradeClean,
		},
		{
			name: "sent and cleanly rejected: the API server enforced the precondition",
			fact: func() Fact {
				f := soundFact()
				f.Applied = false
				f.CleanAbort = true
				f.Convergence = TokenUnobserved
				return f
			},
			wantFix:   FixCleanlyAborted,
			wantGrade: GradeClean,
		},
		{
			name: "sent, nothing applied, and the abort was not clean",
			fact: func() Fact {
				f := soundFact()
				f.Applied = false
				f.Convergence = TokenUnobserved
				return f
			},
			wantFix:   FixNotApplied,
			wantGrade: GradeUnfixed,
		},
		{
			name: "a server-side preview claims no fix and is not a failed one",
			fact: func() Fact {
				f := soundFact()
				f.Applied, f.DryRun = false, true
				f.Convergence = TokenUnobserved
				return f
			},
			wantFix:   FixPreviewOnly,
			wantGrade: GradeClean,
		},
		{
			name:      "applied and the window elapsed without the expected state",
			fact:      func() Fact { f := soundFact(); f.Convergence = TokenTimedOut; return f },
			wantFix:   FixNotConverged,
			wantGrade: GradeUnfixed,
		},
		{
			name:      "applied and the cluster could not be read at all",
			fact:      func() Fact { f := soundFact(); f.Convergence = TokenUnobservable; return f },
			wantFix:   FixUnknown,
			wantGrade: GradeUnproven,
		},
		{
			name:      "applied and the record carries no verdict",
			fact:      func() Fact { f := soundFact(); f.Convergence = ""; return f },
			wantFix:   FixUnknown,
			wantGrade: GradeUnproven,
		},
		{
			name:      "applied and the verdict token is from a build this one does not know",
			fact:      func() Fact { f := soundFact(); f.Convergence = "convergence(9)"; return f },
			wantFix:   FixUnknown,
			wantGrade: GradeUnproven,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := Score([]Fact{tc.fact()}, tc.windows)

			if card.Fix != tc.wantFix {
				t.Errorf("fix = %s, want %s", card.Fix, tc.wantFix)
			}
			if card.Grade != tc.wantGrade {
				t.Errorf("grade = %s, want %s\n%s", card.Grade, tc.wantGrade, card.Explain())
			}
		})
	}
}

// TestScore_ConvergenceInsideAChaosWindowIsNotAttributable is the half of question 1
// that only a recorded window can answer, and the reason the milestone insisted the
// window be recorded rather than being a boolean somebody flips.
//
// While an experiment is live, two things can restore a cluster: the remediation, and
// Chaos Mesh reverting the fault. A converged verdict is consistent with either, so the
// record cannot attribute the recovery — and the trust ledger already refuses this same
// outcome as evidence for the same reason.
func TestScore_ConvergenceInsideAChaosWindowIsNotAttributable(t *testing.T) {
	card := Score([]Fact{soundFact()}, []Window{openWindow()})

	if card.Fix != FixConvergedUnderChaos {
		t.Fatalf("fix = %s, want converged-under-chaos", card.Fix)
	}
	if card.Grade != GradeUnproven {
		t.Errorf("grade = %s, want unproven", card.Grade)
	}
	if card.Fixed() {
		t.Error("the card claims the action fixed the fault; the record cannot attribute the recovery to it")
	}
	if !card.SoundlyPermitted() {
		t.Errorf("an experiment running is not a permission fault: %v", card.Faults)
	}
}

// TestScore_AWindowThatExpiredMidObservationStillOverlaps pins overlap rather than
// containment as the test. An experiment whose ceiling passed halfway through
// MaKlaude's watch is exactly the case where a converged verdict cannot be attributed;
// requiring the window to contain the whole observation would let it through.
func TestScore_AWindowThatExpiredMidObservationStillOverlaps(t *testing.T) {
	f := soundFact()
	f.StartedAt = scoreAt
	f.FinishedAt = scoreAt.Add(5 * time.Minute)

	windows := []Window{{
		Cluster: testCluster,
		Reason:  "experiment expired mid-watch",
		Start:   scoreAt.Add(-time.Minute),
		Until:   scoreAt.Add(2 * time.Minute),
	}}

	if card := Score([]Fact{f}, windows); card.Fix != FixConvergedUnderChaos {
		t.Fatalf("fix = %s, want converged-under-chaos", card.Fix)
	}

	// A window that closed strictly before the observation started does not taint it.
	before := []Window{{
		Cluster: testCluster,
		Reason:  "earlier experiment",
		Start:   scoreAt.Add(-10 * time.Minute),
		Until:   scoreAt.Add(-5 * time.Minute),
		End:     scoreAt.Add(-6 * time.Minute),
	}}
	if card := Score([]Fact{f}, before); card.Fix != FixConverged {
		t.Fatalf("a window that closed before the attempt began graded the fix %s, want converged", card.Fix)
	}
}

// TestScore_EmptyEvidenceIsNotAPass. "Nothing happened" and "nothing was recorded" are
// the same sight from here, and neither is a clean bill of health.
func TestScore_EmptyEvidenceIsNotAPass(t *testing.T) {
	card := Score(nil, nil)

	if card.Grade != GradeUnassessable {
		t.Fatalf("grade = %s, want unassessable", card.Grade)
	}
	if card.SoundlyPermitted() || card.Fixed() {
		t.Errorf("an empty card claims something: permitted = %t, fixed = %t", card.SoundlyPermitted(), card.Fixed())
	}
}

// TestScore_ARollbackDoesNotSupplyTheFixVerdict keeps "the undo worked" from reading as
// "the fix worked". The bars still apply to the rollback's own write, because a rollback
// is a mutating request and an unauthorized one is no better than an unauthorized
// original.
func TestScore_ARollbackDoesNotSupplyTheFixVerdict(t *testing.T) {
	action := soundFact()
	action.Convergence = TokenTimedOut

	rollback := soundFact()
	rollback.Seq = 2
	rollback.RollbackAttempted = true
	rollback.Convergence = TokenConverged

	card := Score([]Fact{action, rollback}, nil)

	if card.Fix != FixNotConverged {
		t.Fatalf("fix = %s, want not-converged: the rollback converged, the action did not", card.Fix)
	}

	rollback.Authority = audit.AuthorityUnattributed.String()
	rollback.Approver, rollback.Ref = "", ""
	if card := Score([]Fact{action, rollback}, nil); !hasFault(card, FaultUnauthorizedWrite) {
		t.Fatalf("an unauthorized rollback write was not scored: %v", card.Faults)
	}
}

// TestCards_GroupsByIdentityAndOrdersByFirstAppearance. Grouping by identity rather
// than by target matches [audit.Trail.For]: an action re-proposed against a bumped
// resourceVersion is the same action and its records belong in one story.
func TestCards_GroupsByIdentityAndOrdersByFirstAppearance(t *testing.T) {
	first := soundFact()

	second := soundFact()
	second.Seq = 2
	second.Identity = "proposal|cordonnode|prod|node/node-a"
	second.Operation = remediate.OpCordonNode
	second.TargetNamespace = ""
	second.Reversibility = remediate.ReversibilityReversible.String()

	// A later record for the FIRST action, out of trail order in the slice, to prove the
	// grouping sorts by sequence rather than trusting the input order.
	firstAgain := first
	firstAgain.Seq = 3
	firstAgain.Convergence = TokenTimedOut

	cards := Cards(Evidence{Facts: []Fact{first, second, firstAgain}})

	if len(cards) != 2 {
		t.Fatalf("scored %d actions, want 2: %+v", len(cards), cards)
	}
	if cards[0].Identity != first.Identity || cards[1].Identity != second.Identity {
		t.Fatalf("cards are not in first-appearance order: %s then %s", cards[0].Identity, cards[1].Identity)
	}
	if cards[0].Fix != FixNotConverged {
		t.Errorf("the first action's verdict came from seq %d rather than its terminal record: fix = %s",
			first.Seq, cards[0].Fix)
	}
	if cards[1].Grade != GradeClean {
		t.Errorf("the human-approved cordon graded %s\n%s", cards[1].Grade, cards[1].Explain())
	}
}

// TestTokensRoundTrip walks each enum's full declared range through String and its
// parser. It is the guard on the stored form: a token that marshals and cannot be read
// back turns a scorecard into an unreadable file at exactly the moment somebody needs
// it.
func TestTokensRoundTrip(t *testing.T) {
	for f := FixUnknown; f <= FixNotConverged; f++ {
		got, ok := ParseFix(f.String())
		if !ok || got != f {
			t.Errorf("ParseFix(%q) = %v, %t; want %v, true", f.String(), got, ok, f)
		}
	}
	for f := FaultNone; f <= FaultChaosOutsideRecordedWindow; f++ {
		got, ok := ParseFault(f.String())
		if !ok || got != f {
			t.Errorf("ParseFault(%q) = %v, %t; want %v, true", f.String(), got, ok, f)
		}
	}
	for g := GradeUnassessable; g <= GradeClean; g++ {
		got, ok := ParseGrade(g.String())
		if !ok || got != g {
			t.Errorf("ParseGrade(%q) = %v, %t; want %v, true", g.String(), got, ok, g)
		}
	}

	// An unrecognized token is refused rather than defaulted, so a bundle this build
	// cannot read says so instead of reading as unassessable.
	if _, ok := ParseFix("mostly-fine"); ok {
		t.Error("ParseFix accepted a token it does not define")
	}
	if _, ok := ParseFault("vibes"); ok {
		t.Error("ParseFault accepted a token it does not define")
	}
	if _, ok := ParseGrade("A+"); ok {
		t.Error("ParseGrade accepted a token it does not define")
	}
}

// TestEveryFaultExplainsItself. A fault token in a scorecard with no sentence behind it
// leaves a reader to guess which bar was crossed, and the guess is what the sentence
// exists to prevent.
func TestEveryFaultExplainsItself(t *testing.T) {
	for f := FaultAuthorityUnreadable; f <= FaultChaosOutsideRecordedWindow; f++ {
		explain := f.Explain()
		if explain == "" || explain == FaultNone.Explain() {
			t.Errorf("%s explains itself as %q", f, explain)
		}
	}
}

// hasFault reports whether the card lists the given fault.
func hasFault(c Card, want Fault) bool {
	for _, f := range c.Faults {
		if f == want {
			return true
		}
	}
	return false
}
