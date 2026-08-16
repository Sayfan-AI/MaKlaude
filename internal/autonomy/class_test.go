package autonomy

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// allClasses is every declared [Class], hand-written for the same reason allReasons is:
// the invariants below exist to notice a class that was added without anybody deciding
// whether it may run unattended, and a loop bounded by the last constant would include a
// new one silently.
var allClasses = []Class{classUnset, ClassRemediation, ClassChaos}

// chaosOperation is what internal/chaos records an action as. It is written literally
// here rather than imported, because that package imports this one — and because a test
// that derived the token from the code it is checking would pass if both changed
// together, which is exactly the change worth catching: the prefix is load-bearing (it is
// what keeps a chaos operation out of every operator's allowlist) and it is part of this
// package's contract with that one.
const chaosOperation = remediate.Operation("chaos:pod-kill")

// chaosRequest is a well-formed chaos decision request against the fixture cluster: the
// projection internal/chaos builds, restated here so this package's tests do not depend
// on it.
func chaosRequest() Request {
	return Request{
		Class:           ClassChaos,
		Cluster:         testCluster,
		ProposalCluster: testCluster,
		Operation:       chaosOperation,
		Target: remediate.Target{
			Cluster:   testCluster,
			Kind:      "chaosexperiment",
			Namespace: testNamespace,
			Name:      "pod-kill;one",
		},
	}
}

// TestClasses_AreExhaustivelyListed fails when a class is declared without being listed,
// which is what keeps the invariants below honest.
func TestClasses_AreExhaustivelyListed(t *testing.T) {
	if want := int(ClassChaos) + 1; len(allClasses) != want {
		t.Fatalf("allClasses has %d entries, want %d — a Class was added without being listed here, "+
			"so the eligibility invariant below silently skipped it", len(allClasses), want)
	}
	for i, c := range allClasses {
		if int(c) != i {
			t.Errorf("allClasses[%d] = %s (value %d); the list must be in declaration order", i, c, int(c))
		}
	}
}

// TestClass_OnlyRemediationMayAutoApply is the structural half of "chaos never promotes",
// stated once: exactly one class is even eligible to run unattended.
//
// Every path to [DecisionAutoApply] runs through [Class.MayAutoApply] in [unconditional],
// so this is also the statement that no ruleset and no ledger state can produce an
// auto-applied experiment.
func TestClass_OnlyRemediationMayAutoApply(t *testing.T) {
	for _, c := range allClasses {
		want := c == ClassRemediation
		if got := c.MayAutoApply(); got != want {
			t.Errorf("Class(%s).MayAutoApply() = %v, want %v", c, got, want)
		}
	}
}

// TestClass_UnknownValuesNeverAutoApply covers the values no constant names — a class
// deserialized from a future build, or an integer somebody cast.
func TestClass_UnknownValuesNeverAutoApply(t *testing.T) {
	for _, c := range []Class{-1, Class(len(allClasses)), Class(9999)} {
		if c.MayAutoApply() {
			t.Errorf("Class(%d) is not a class this build knows and must not be eligible for autonomy", int(c))
		}
		if got := c.String(); !strings.HasPrefix(got, "class(") {
			t.Errorf("Class(%d).String() = %q, want the numeric fallback rendering", int(c), got)
		}
	}
}

// TestClass_TokensAreStableAndDistinct pins the rendered tokens. They reach audit records
// and human-facing renderings, so two classes sharing one token would make a deliberate
// fault indistinguishable from a fix in the record.
func TestClass_TokensAreStableAndDistinct(t *testing.T) {
	want := map[Class]string{
		classUnset:       "unclassified",
		ClassRemediation: "remediation",
		ClassChaos:       "chaos",
	}
	seen := map[string]Class{}
	for _, c := range allClasses {
		got := c.String()
		if got != want[c] {
			t.Errorf("Class(%d).String() = %q, want %q", int(c), got, want[c])
		}
		if prior, dup := seen[got]; dup {
			t.Errorf("Class(%d) and Class(%d) both render as %q", int(c), int(prior), got)
		}
		seen[got] = c
	}
}

// TestFullyTrustedClusterStillGatesChaos is task T4's second done criterion, executed: a
// chaos proposal can never be auto-applied, whatever the trust state.
//
// The scenario is deliberately the most permissive one this package can express — a valid
// ruleset covering the cluster and namespace, and an oracle that trusts everything it is
// asked about, including the chaos subject — because the claim is not "chaos gates under
// the shipped posture" but "chaos gates under every posture".
func TestFullyTrustedClusterStillGatesChaos(t *testing.T) {
	req := chaosRequest()
	// An oracle that trusts literally anything, so the trust question cannot be the reason
	// this gates. If the decision ever reached it, this request would auto-apply.
	oracle := &allTrusting{}

	got := DecideRequest(req, shippedRuleset(), oracle)

	assertVerdict(t, got, DecisionGate, ReasonChaosNeverPromotes, "")
	if oracle.asked {
		t.Error("the oracle was asked about a chaos proposal; the class must gate before the trust question is reachable, " +
			"or the never-promotes property depends on the oracle's answer")
	}
}

// TestChaosGatesUnderEveryRulesetPosture covers the four ruleset states, because the
// never-promotes property must not be an artifact of one of them. A wide-open ruleset
// naming the chaos operation is not among them for a reason the next test states.
func TestChaosGatesUnderEveryRulesetPosture(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules Ruleset
		trust TrustOracle
	}{
		{"no rules, no oracle", nil, nil},
		{"no rules, trusting oracle", nil, &allTrusting{}},
		{"the shipped ruleset", shippedRuleset(), earned()},
		{"an invalid ruleset", Ruleset{{Name: "broken"}}, &allTrusting{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRequest(chaosRequest(), tc.rules, tc.trust)
			assertVerdict(t, got, DecisionGate, ReasonChaosNeverPromotes, "")
		})
	}
}

// TestNoRuleCanNameAChaosOperation is the second structural bar, independent of the
// class: an operator cannot grant chaos autonomy by writing a rule, because rule
// validation refuses any operation outside the remediation catalog.
//
// It matters because the class check is one line of one function. If a future edit
// weakened it, this is the property that still stands between a configuration file and an
// unattended fault — and a ruleset that fails validation gates EVERYTHING, so the attempt
// is loud rather than partial.
func TestNoRuleCanNameAChaosOperation(t *testing.T) {
	rs := Ruleset{{
		Name:             "break-payments",
		Clusters:         []string{testCluster},
		Namespaces:       []string{testNamespace},
		Operations:       []remediate.Operation{chaosOperation},
		MaxReversibility: remediate.ReversibilityReversible,
	}}

	err := rs.Validate()
	if err == nil {
		t.Fatal("a rule naming a chaos operation must not validate; it would be an operator granting autonomy for a deliberate fault")
	}
	if !strings.Contains(err.Error(), string(chaosOperation)) {
		t.Errorf("the error must name the operation it refused, got %q", err)
	}

	// And the ruleset being invalid gates the remediation it would otherwise have
	// permitted, rather than being silently ignored.
	assertVerdict(t, Decide(testCluster, proposal(), rs, earned()), DecisionGate, ReasonRulesetInvalid, "")
}

// TestUnclassifiedRequestGates covers the wiring bug: a [Request] built without its
// class. It must not inherit remediation's authority, and it must be distinguishable in
// the record from the deliberate never-promotes case, because one is normal and the other
// needs somebody to fix a call site.
func TestUnclassifiedRequestGates(t *testing.T) {
	req := chaosRequest()
	req.Class = classUnset
	req.Operation = remediate.OpRolloutRestart
	req.Target.Kind = "deployment"

	got := DecideRequest(req, shippedRuleset(), &allTrusting{})
	assertVerdict(t, got, DecisionGate, ReasonProposalClassUnknown, "")

	if ReasonProposalClassUnknown.String() == ReasonChaosNeverPromotes.String() {
		t.Error("a wiring bug and a design property must not render as the same reason")
	}
}

// TestDecideAndDecideRequestAreTheSameFunction is T4's first done criterion at this
// layer: a chaos proposal reaches the same decision function a remediation does.
//
// It is asserted through a check that BOTH classes are subject to — the cluster
// agreement, which is the one question that is universal — and by pinning that
// [Decide] is a projection rather than a second implementation: the same remediation,
// asked both ways, must produce identical verdicts.
func TestDecideAndDecideRequestAreTheSameFunction(t *testing.T) {
	p := proposal()
	viaHelper := Decide(testCluster, p, shippedRuleset(), earned())
	viaRequest := DecideRequest(Remediation(testCluster, p), shippedRuleset(), earned())
	if viaHelper != viaRequest {
		t.Fatalf("Decide produced %+v and DecideRequest produced %+v for the same proposal; they must be one function", viaHelper, viaRequest)
	}

	// The universal check fires for both classes, which is what "the same decision
	// function" buys: an experiment aimed at the wrong cluster is refused by the same line
	// that refuses a remediation aimed at the wrong cluster.
	chaosElsewhere := chaosRequest()
	chaosElsewhere.Target.Cluster = "staging"
	assertVerdict(t, DecideRequest(chaosElsewhere, shippedRuleset(), earned()), DecisionRefuse, ReasonClusterMismatch, "")

	remediationElsewhere := proposal()
	remediationElsewhere.Target.Cluster = "staging"
	assertVerdict(t, Decide(testCluster, remediationElsewhere, shippedRuleset(), earned()), DecisionRefuse, ReasonClusterMismatch, "")
}

// TestRemediationProjectionKeepsAllThreeClusters is the regression guard for the check
// the [Request] refactor could most easily have deleted.
//
// Three separate facts have to agree: the caller's cluster, the proposal's own record of
// it, and the target's. Collapsing any two into one field would make that comparison
// tautological and the check would pass by construction — silently, since nothing else in
// the ladder looks at the cluster again.
func TestRemediationProjectionKeepsAllThreeClusters(t *testing.T) {
	for _, tc := range []struct {
		name             string
		caller           string
		mutate           func(*remediate.Proposal)
		wantDecision     Decision
		wantReasonIsMiss bool
	}{
		{
			name:         "the proposal disagrees with the caller",
			caller:       testCluster,
			mutate:       func(p *remediate.Proposal) { p.Cluster = "staging" },
			wantDecision: DecisionRefuse,
		},
		{
			name:         "the target disagrees with the caller",
			caller:       testCluster,
			mutate:       func(p *remediate.Proposal) { p.Target.Cluster = "staging" },
			wantDecision: DecisionRefuse,
		},
		{
			name:         "the caller names no cluster at all",
			caller:       "",
			mutate:       func(*remediate.Proposal) {},
			wantDecision: DecisionRefuse,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proposal()
			tc.mutate(&p)
			got := Decide(tc.caller, p, shippedRuleset(), earned())
			assertVerdict(t, got, tc.wantDecision, ReasonClusterMismatch, "")
		})
	}

	// The projection carries the proposal's own cluster rather than duplicating the
	// caller's, which is what makes the case above reachable at all.
	p := proposal()
	p.Cluster = "staging"
	if req := Remediation(testCluster, p); req.ProposalCluster == req.Cluster {
		t.Error("Remediation copied the caller's cluster over the proposal's; the disagreement it exists to expose is now invisible")
	}
}

// TestChaosRequestCarriesNoTrustInputs pins that the chaos projection leaves the fields
// only remediation reads at their zero values. A fingerprint on an experiment would
// invite a reader — or a future consumer — to believe an experiment can be trusted
// per-fix.
func TestChaosRequestCarriesNoTrustInputs(t *testing.T) {
	req := chaosRequest()
	if req.Fingerprint != "" {
		t.Errorf("a chaos request carries fingerprint %q; nothing reads it and its presence implies promotable chaos", req.Fingerprint)
	}
	if req.Reversibility != remediate.ReversibilityReversible {
		t.Errorf("a chaos request carries reversibility %v; a fault ends by its self-limit, not by being undone", req.Reversibility)
	}
}

// allTrusting is an oracle that trusts every subject and records that it was consulted.
// The recording is the point: several tests above assert the trust question was never
// reached, which is a stronger claim than "the verdict was not auto-apply".
type allTrusting struct{ asked bool }

func (a *allTrusting) Trust(Subject) TrustEvidence {
	a.asked = true
	return TrustEvidence{Trusted: true, Citation: "this oracle trusts everything"}
}
