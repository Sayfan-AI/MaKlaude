package autonomy

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The fixture cluster, namespace and rule name every test builds from. They are
// package-level so a scenario names only the dimension it is varying.
const (
	testCluster   = "prod"
	testNamespace = "payments"
	testRule      = "restart-payments"
)

// proposal returns a well-formed, in-catalog, fully reversible proposal against the
// fixture cluster and namespace — the one shape the shipped ruleset is written for.
// A scenario mutates the field it is about and leaves the rest alone, so a failure
// names one cause.
func proposal() remediate.Proposal {
	return remediate.Proposal{
		Cluster:       testCluster,
		Operation:     remediate.OpRolloutRestart,
		Reversibility: remediate.ReversibilityReversible,
		Target: remediate.Target{
			Cluster:         testCluster,
			Kind:            "deployment",
			Namespace:       testNamespace,
			Name:            "web",
			ResourceVersion: "424242",
		},
	}
}

// shippedRuleset is the configuration the milestone ships: one rule, one cluster,
// one namespace, `rolloutrestart` alone, and the strictest reversibility ceiling.
func shippedRuleset() Ruleset {
	return Ruleset{{
		Name:             testRule,
		Clusters:         []string{testCluster},
		Namespaces:       []string{testNamespace},
		Operations:       []remediate.Operation{remediate.OpRolloutRestart},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
}

// subjectOf is what [Decide] will ask the oracle about for this proposal: its shape
// and its fingerprint. Tests seed trust through it rather than spelling a fingerprint,
// so a change to what [remediate.Proposal.Fingerprint] covers cannot quietly turn these
// cases into assertions about an untrusted subject that gate for the wrong reason.
func subjectOf(p remediate.Proposal) Subject {
	return Subject{
		Shape:       Shape{Cluster: p.Cluster, Operation: p.Operation},
		Fingerprint: p.Fingerprint(),
	}
}

// trusting is a trust oracle that has promoted exactly the fixes proposed here.
func trusting(ps ...remediate.Proposal) StaticTrust {
	out := StaticTrust{}
	for _, p := range ps {
		out[subjectOf(p)] = "seeded"
	}
	return out
}

// earned is a trust oracle that has promoted the fixture proposal's fix.
func earned() StaticTrust {
	return StaticTrust{
		subjectOf(proposal()): "3 human-approved executions of this fix converged; no failures in the last 10 for the shape",
	}
}

// assertVerdict compares a verdict field by field, so a failure says which field is
// wrong rather than dumping two structs.
func assertVerdict(t *testing.T, got Verdict, wantDecision Decision, wantReason Reason, wantRule string) {
	t.Helper()
	if got.Decision != wantDecision {
		t.Errorf("decision = %s, want %s (reason %s)", got.Decision, wantDecision, got.Reason)
	}
	if got.Reason != wantReason {
		t.Errorf("reason = %s, want %s", got.Reason, wantReason)
	}
	if got.Rule != wantRule {
		t.Errorf("rule = %q, want %q", got.Rule, wantRule)
	}
}

// TestDecide_EarnedTrustAutoApplies is the one path to autonomy: a valid rule
// covering the proposal, plus a shape the ledger has promoted. Everything else in
// this file is a way for it not to happen.
func TestDecide_EarnedTrustAutoApplies(t *testing.T) {
	got := Decide(testCluster, proposal(), shippedRuleset(), earned())

	assertVerdict(t, got, DecisionAutoApply, ReasonEarnedTrust, testRule)
	if !got.AutoApplies() {
		t.Error("AutoApplies() = false on an auto-apply verdict")
	}
	if !strings.Contains(got.Evidence, "3 human-approved executions") {
		t.Errorf("evidence = %q, want the ledger's citation carried through verbatim", got.Evidence)
	}
}

// TestDecide_DefaultDeny covers the done criterion in full: an absent, empty, or
// malformed configuration must gate every proposal, i.e. behave exactly as the
// system did before this package existed. Each case here is a way an operator
// arrives at a ruleset they did not mean to have.
func TestDecide_DefaultDeny(t *testing.T) {
	invalid := Ruleset{{
		Name:       testRule,
		Clusters:   []string{testCluster},
		Operations: []remediate.Operation{remediate.OpRolloutRestart},
		// Namespaces omitted — a half-written rule, not a wildcard.
	}}
	// A valid rule sitting next to a broken one. The whole set is refused: honoring
	// the good rule would grant autonomy under a configuration the operator did not
	// finish writing, and would do it with no signal.
	mixed := append(shippedRuleset(), invalid...)

	for _, tc := range []struct {
		name  string
		rules Ruleset
		want  Reason
	}{
		{"nil ruleset", nil, ReasonAutonomyNotConfigured},
		{"empty ruleset", Ruleset{}, ReasonAutonomyNotConfigured},
		{"malformed rule", invalid, ReasonRulesetInvalid},
		{"one malformed rule among valid ones", mixed, ReasonRulesetInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Full trust, so nothing but the configuration can be what holds it back.
			got := Decide(testCluster, proposal(), tc.rules, earned())
			assertVerdict(t, got, DecisionGate, tc.want, "")
		})
	}
}

// TestDecide_RefusedRegardlessOfConfiguration asserts the unconditional refusals.
// Each case is run against a ruleset that explicitly tries to permit it and against
// a fully trusting oracle, because "can't be allowed even explicitly" is the claim.
func TestDecide_RefusedRegardlessOfConfiguration(t *testing.T) {
	// A rule as permissive as validation allows: every catalog operation, the
	// riskiest configurable reversibility class, the target's own namespace.
	permissive := Ruleset{{
		Name:       "permit-everything-allowed",
		Clusters:   []string{testCluster},
		Namespaces: []string{testNamespace},
		Operations: []remediate.Operation{
			remediate.OpRolloutRestart, remediate.OpRollbackRevision,
			remediate.OpDeletePod, remediate.OpCordonNode,
		},
		MaxReversibility: remediate.ReversibilityRecreatedByController,
	}}

	for _, tc := range []struct {
		name   string
		mutate func(*remediate.Proposal)
		want   Reason
	}{
		{
			name:   "operation outside the catalog",
			mutate: func(p *remediate.Proposal) { p.Operation = remediate.Operation("deletenamespace") },
			want:   ReasonOperationOffCatalog,
		},
		{
			name:   "empty operation",
			mutate: func(p *remediate.Proposal) { p.Operation = "" },
			want:   ReasonOperationOffCatalog,
		},
		{
			name:   "irreversible action",
			mutate: func(p *remediate.Proposal) { p.Reversibility = remediate.ReversibilityIrreversible },
			want:   ReasonIrreversible,
		},
		{
			name:   "reversibility above the defined range",
			mutate: func(p *remediate.Proposal) { p.Reversibility = remediate.ReversibilityIrreversible + 1 },
			want:   ReasonReversibilityUnknown,
		},
		{
			name:   "reversibility below the defined range",
			mutate: func(p *remediate.Proposal) { p.Reversibility = remediate.ReversibilityReversible - 1 },
			want:   ReasonReversibilityUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proposal()
			tc.mutate(&p)
			// Trust every shape, including the mutated one, so trust cannot be what
			// stops it.
			trust := trusting(p)

			got := Decide(testCluster, p, permissive, trust)

			assertVerdict(t, got, DecisionRefuse, tc.want, "")
			if got.Evidence != "" {
				t.Errorf("evidence = %q on a refusal; a refused proposal was never evaluated for trust", got.Evidence)
			}
		})
	}
}

// TestDecide_RefusalPrecedesConfiguration proves the refusals really are decided
// before any rule is read: they hold identically under a nil ruleset, a malformed
// one, and a permissive one. If a refusal were evaluated after validation, a
// malformed ruleset would downgrade it to a gate — which would offer a human an
// approval request for an action nothing downstream can describe.
func TestDecide_RefusalPrecedesConfiguration(t *testing.T) {
	p := proposal()
	p.Operation = remediate.Operation("dropdatabase")

	malformed := Ruleset{{Name: "", Clusters: nil, Namespaces: nil, Operations: nil}}

	for _, tc := range []struct {
		name  string
		rules Ruleset
	}{
		{"nil ruleset", nil},
		{"malformed ruleset", malformed},
		{"shipped ruleset", shippedRuleset()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(testCluster, p, tc.rules, earned())
			assertVerdict(t, got, DecisionRefuse, ReasonOperationOffCatalog, "")
		})
	}
}

// TestDecide_ClusterMismatchRefuses covers the corrupt-proposal case. The audit
// layer carries the cluster on both the record and the target precisely so a
// disagreement is visible; this is the layer that acts on that visibility, and it
// refuses rather than gating because a proposal whose own fields contradict each
// other is not one to put in front of a human either.
func TestDecide_ClusterMismatchRefuses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caller  string
		mutate  func(*remediate.Proposal)
		wantWhy string
	}{
		{"proposal names another cluster", testCluster, func(p *remediate.Proposal) { p.Cluster = "staging" }, "proposal"},
		{"target names another cluster", testCluster, func(p *remediate.Proposal) { p.Target.Cluster = "staging" }, "target"},
		{"caller names another cluster", "staging", func(*remediate.Proposal) {}, "caller"},
		{"caller names no cluster", "", func(*remediate.Proposal) {}, "empty caller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proposal()
			tc.mutate(&p)

			got := Decide(tc.caller, p, shippedRuleset(), earned())

			assertVerdict(t, got, DecisionRefuse, ReasonClusterMismatch, "")
		})
	}
}

// TestDecide_ScopeNearMisses walks each selector dimension. Every case gates rather
// than refusing — an operation MaKlaude declines to automate is still one a named
// human may approve — and each names the rule that came closest, because "which of
// my rules did I get wrong?" is the operator's next question.
func TestDecide_ScopeNearMisses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*remediate.Proposal)
		want     Reason
		wantRule string
	}{
		{
			name:     "cluster not named by any rule",
			mutate:   func(p *remediate.Proposal) { p.Cluster, p.Target.Cluster = "staging", "staging" },
			want:     ReasonClusterOutOfScope,
			wantRule: testRule,
		},
		{
			name:     "operation in the catalog but off the allowlist",
			mutate:   func(p *remediate.Proposal) { p.Operation = remediate.OpDeletePod },
			want:     ReasonOperationNotAllowed,
			wantRule: testRule,
		},
		{
			name: "reversibility riskier than the rule's ceiling",
			mutate: func(p *remediate.Proposal) {
				p.Reversibility = remediate.ReversibilityRecreatedByController
			},
			want:     ReasonAboveReversibilityFloor,
			wantRule: testRule,
		},
		{
			name:     "namespace not named by the rule",
			mutate:   func(p *remediate.Proposal) { p.Target.Namespace = "kube-system" },
			want:     ReasonNamespaceOutOfScope,
			wantRule: testRule,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := proposal()
			tc.mutate(&p)
			trust := trusting(p)

			got := Decide(p.Cluster, p, shippedRuleset(), trust)

			assertVerdict(t, got, DecisionGate, tc.want, tc.wantRule)
		})
	}
}

// TestDecide_ClusterScopedTargetNeverAutoApplies pins a consequence of requiring
// every rule to name namespaces: a node has none, so no rule can bound the blast
// radius of an action against it, so it is never automated. The rule here permits
// `cordonnode` explicitly and the shape is fully trusted; it still gates.
func TestDecide_ClusterScopedTargetNeverAutoApplies(t *testing.T) {
	p := proposal()
	p.Operation = remediate.OpCordonNode
	p.Target = remediate.Target{Cluster: testCluster, Kind: "node", Name: "node-a", ResourceVersion: "7"}

	rules := Ruleset{{
		Name:       "permit-cordon",
		Clusters:   []string{testCluster},
		Namespaces: []string{testNamespace, ""}, // the empty entry is itself invalid...
		Operations: []remediate.Operation{remediate.OpCordonNode},
	}}
	if err := rules.Validate(); err == nil {
		t.Fatal("a rule listing an empty namespace validated; an operator must not be able to spell a wildcard")
	}

	// ...so try it the only way validation permits: a real namespace listed, and a
	// target that simply has none.
	rules[0].Namespaces = []string{testNamespace}
	trust := trusting(p)

	got := Decide(testCluster, p, rules, trust)

	assertVerdict(t, got, DecisionGate, ReasonClusterScopedTarget, "permit-cordon")
}

// TestDecide_TrustGates covers the three ways a matched rule still does not fire.
// They are separate reasons because they need three different responses: wire up
// the ledger, wait for history, or fix a ledger claiming what it cannot evidence.
func TestDecide_TrustGates(t *testing.T) {
	// Trust for three neighbours of the fixture: same operation on another cluster, and
	// two other operations on this one. None of them is the proposal under test, so all
	// three must leave it gating.
	otherCluster := proposal()
	otherCluster.Cluster = "staging"
	otherCluster.Target.Cluster = "staging"
	cordon := proposal()
	cordon.Operation = remediate.OpCordonNode
	rollback := proposal()
	rollback.Operation = remediate.OpRollbackRevision
	otherShape := trusting(otherCluster, cordon, rollback)

	for _, tc := range []struct {
		name  string
		trust TrustOracle
		want  Reason
	}{
		{"no ledger wired up", nil, ReasonNoTrustLedger},
		{"nil ledger map", StaticTrust(nil), ReasonUntrustedShape},
		{"no history for this shape", StaticTrust{}, ReasonUntrustedShape},
		{"history for a neighbouring shape only", otherShape, ReasonUntrustedShape},
		{
			name:  "trusted with no citation",
			trust: StaticTrust{subjectOf(proposal()): ""},
			want:  ReasonTrustEvidenceMissing,
		},
		{
			name:  "trusted with a blank citation",
			trust: StaticTrust{subjectOf(proposal()): "   \n\t"},
			want:  ReasonTrustEvidenceMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(testCluster, proposal(), shippedRuleset(), tc.trust)
			assertVerdict(t, got, DecisionGate, tc.want, testRule)
			if got.Evidence != "" {
				t.Errorf("evidence = %q on a gated verdict; only an auto-apply carries one", got.Evidence)
			}
		})
	}
}

// TestDecide_TrustIsScopedToTheCluster is the multi-cluster isolation property at
// this layer: a shape promoted on one cluster grants nothing on another, even when
// one rule covers both.
func TestDecide_TrustIsScopedToTheCluster(t *testing.T) {
	rules := shippedRuleset()
	rules[0].Clusters = []string{"prod", "staging"}
	trust := StaticTrust{subjectOf(proposal()): "3 converged"}

	staging := proposal()
	staging.Cluster, staging.Target.Cluster = "staging", "staging"

	if got := Decide("prod", proposal(), rules, trust); !got.AutoApplies() {
		t.Errorf("prod: %s, want auto-apply — this is the cluster that earned it", got)
	}
	if got := Decide("staging", staging, rules, trust); got.AutoApplies() {
		t.Errorf("staging: %s, want a gate — trust earned on prod says nothing here", got)
	}
}

// TestDecide_FirstMatchingRuleIsNamed pins which rule lands in the record when
// several would permit the action. Rules only ever grant, so the decision is the
// same either way; naming the first keeps the verdict stable when a rule is
// appended later, which matters because the name is what a revocation targets.
func TestDecide_FirstMatchingRuleIsNamed(t *testing.T) {
	rules := append(shippedRuleset(), Rule{
		Name:       "also-restart-payments",
		Clusters:   []string{testCluster},
		Namespaces: []string{testNamespace},
		Operations: []remediate.Operation{remediate.OpRolloutRestart},
	})

	got := Decide(testCluster, proposal(), rules, earned())

	assertVerdict(t, got, DecisionAutoApply, ReasonEarnedTrust, testRule)
}

// TestDecide_NearestMissWins checks the near-miss ranking rather than the decision:
// with three rules that each fail at a different dimension, the verdict names the
// one that got furthest, which is the one the operator most likely meant to write.
func TestDecide_NearestMissWins(t *testing.T) {
	rules := Ruleset{
		{ // fails first: does not cover the cluster at all
			Name:       "wrong-cluster",
			Clusters:   []string{"staging"},
			Namespaces: []string{testNamespace},
			Operations: []remediate.Operation{remediate.OpRolloutRestart},
		},
		{ // fails later: right cluster and operation, wrong namespace
			Name:       "wrong-namespace",
			Clusters:   []string{testCluster},
			Namespaces: []string{"kube-system"},
			Operations: []remediate.Operation{remediate.OpRolloutRestart},
		},
		{ // fails in between: right cluster, wrong operation
			Name:       "wrong-operation",
			Clusters:   []string{testCluster},
			Namespaces: []string{testNamespace},
			Operations: []remediate.Operation{remediate.OpDeletePod},
		},
	}

	got := Decide(testCluster, proposal(), rules, earned())

	assertVerdict(t, got, DecisionGate, ReasonNamespaceOutOfScope, "wrong-namespace")
}

// TestDecide_IsDeterministic runs the whole decision surface repeatedly and asserts
// byte-stable output. Determinism is not a nice property here — it is what lets an
// operator reason about an unattended mutation from the record of it, and what lets
// the e2e assert an exact verdict rather than a shape.
func TestDecide_IsDeterministic(t *testing.T) {
	// Several rules and several trusted shapes, so any map iteration or slice
	// ordering that leaked into the decision would show up as a flake.
	rules := Ruleset{
		{Name: "a-restart", Clusters: []string{"prod", "staging"}, Namespaces: []string{"payments", "web"}, Operations: []remediate.Operation{remediate.OpRolloutRestart}},
		{Name: "b-restart", Clusters: []string{"prod"}, Namespaces: []string{"payments"}, Operations: []remediate.Operation{remediate.OpRolloutRestart, remediate.OpDeletePod}, MaxReversibility: remediate.ReversibilityRecreatedByController},
	}
	prodDelete := proposal()
	prodDelete.Operation = remediate.OpDeletePod
	stagingRestart := proposal()
	stagingRestart.Cluster, stagingRestart.Target.Cluster = "staging", "staging"
	trust := trusting(proposal(), prodDelete, stagingRestart)

	cases := []remediate.Proposal{proposal()}
	for _, op := range []remediate.Operation{remediate.OpDeletePod, remediate.OpCordonNode, remediate.OpRollbackRevision, "unknown"} {
		p := proposal()
		p.Operation = op
		cases = append(cases, p)
	}
	for _, ns := range []string{"web", "kube-system", ""} {
		p := proposal()
		p.Target.Namespace = ns
		cases = append(cases, p)
	}

	for i, p := range cases {
		first := Decide(testCluster, p, rules, trust)
		for n := 0; n < 50; n++ {
			if got := Decide(testCluster, p, rules, trust); got != first {
				t.Fatalf("case %d call %d: %#v, want %#v — the decision is not deterministic", i, n, got, first)
			}
		}
		if first.String() == "" {
			t.Errorf("case %d: verdict renders as the empty string", i)
		}
	}
}

// TestDecide_IsTotal feeds deliberately broken input — zero values, out-of-range
// enums, nil everything — and asserts only that a verdict comes back and never
// authorizes. Totality is the property that lets a caller wire this in without a
// recover(): a policy that panics on a malformed proposal is a policy that stops
// the remediation cycle.
func TestDecide_IsTotal(t *testing.T) {
	broken := []remediate.Proposal{
		{},
		{Cluster: testCluster},
		{Cluster: testCluster, Target: remediate.Target{Cluster: testCluster}},
		{Cluster: testCluster, Operation: remediate.OpRolloutRestart, Target: remediate.Target{Cluster: testCluster}, Reversibility: -5},
		{Cluster: testCluster, Operation: "  ", Target: remediate.Target{Cluster: testCluster}},
	}
	rulesets := []Ruleset{nil, {}, shippedRuleset(), {{}}, {{Name: "x"}}}
	oracles := []TrustOracle{nil, StaticTrust(nil), earned()}
	clusters := []string{"", testCluster, "staging"}

	for _, p := range broken {
		for _, rs := range rulesets {
			for _, tr := range oracles {
				for _, c := range clusters {
					got := Decide(c, p, rs, tr)
					if got.AutoApplies() {
						t.Fatalf("Decide(%q, %#v, %#v, %#v) authorized a broken proposal", c, p, rs, tr)
					}
					if got.Decision != got.Reason.Decision() {
						t.Fatalf("verdict %s pairs decision %s with reason %s", got, got.Decision, got.Reason)
					}
				}
			}
		}
	}
}
