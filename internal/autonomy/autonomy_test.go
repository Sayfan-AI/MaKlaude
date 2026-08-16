package autonomy

import (
	"strings"
	"testing"
)

// allReasons is every declared [Reason]. It is a hand-written list rather than a
// range over the iota, because the point of the invariants below is to notice a
// reason that was added without being thought about — and a loop bounded by the
// last constant would silently include it.
var allReasons = []Reason{
	ReasonNoRuleMatched,
	ReasonAutonomyNotConfigured,
	ReasonRulesetInvalid,
	ReasonClusterMismatch,
	ReasonOperationOffCatalog,
	ReasonIrreversible,
	ReasonReversibilityUnknown,
	ReasonClusterOutOfScope,
	ReasonOperationNotAllowed,
	ReasonAboveReversibilityFloor,
	ReasonClusterScopedTarget,
	ReasonNamespaceOutOfScope,
	ReasonNoTrustLedger,
	ReasonUntrustedShape,
	ReasonTrustEvidenceMissing,
	ReasonEarnedTrust,
	ReasonChaosNeverPromotes,
	ReasonProposalClassUnknown,
}

// lastReason is the final constant in the iota block, and the only thing this file
// has to update when a reason is added. It is named rather than written inline so the
// exhaustiveness check below reads as "the list covers the block" instead of pinning
// one particular reason as the last one forever — [ReasonEarnedTrust] held that spot
// until chaos needed two more, and the check silently measured against it.
const lastReason = ReasonProposalClassUnknown

// TestReasons_AreExhaustivelyListed fails when a reason is declared without being
// added to allReasons, which is what keeps the invariants below honest. It works
// because the constants are a contiguous iota block: the count is the last value
// plus one.
func TestReasons_AreExhaustivelyListed(t *testing.T) {
	if want := int(lastReason) + 1; len(allReasons) != want {
		t.Fatalf("allReasons has %d entries, want %d — a Reason was added without being listed here, "+
			"so the decision-mapping and token invariants below silently skipped it", len(allReasons), want)
	}
	for i, r := range allReasons {
		if int(r) != i {
			t.Errorf("allReasons[%d] = %s (value %d); the list must be in declaration order", i, r, int(r))
		}
	}
}

// TestReason_OnlyEarnedTrustAuthorizes is the safety invariant of the whole
// package, stated once: exactly one reason produces [DecisionAutoApply]. Every
// verdict is built from a reason via [Reason.Decision], so this is also the
// statement that no code path can authorize an unattended mutation for any other
// stated cause.
func TestReason_OnlyEarnedTrustAuthorizes(t *testing.T) {
	for _, r := range allReasons {
		got := r.Decision()
		if r == ReasonEarnedTrust {
			if got != DecisionAutoApply {
				t.Errorf("%s authorizes %s, want auto-apply", r, got)
			}
			continue
		}
		if got == DecisionAutoApply {
			t.Errorf("%s authorizes an unattended mutation; only earned-trust may", r)
		}
	}
}

// TestReason_UnknownValuesGate keeps the fail-closed direction on a value that
// escaped the enum — a corrupted deserialization, or a reason from a newer build.
func TestReason_UnknownValuesGate(t *testing.T) {
	for _, r := range []Reason{-1, Reason(len(allReasons)), Reason(9999)} {
		if got := r.Decision(); got != DecisionGate {
			t.Errorf("Reason(%d).Decision() = %s, want gate", int(r), got)
		}
		if !strings.HasPrefix(r.String(), "reason(") {
			t.Errorf("Reason(%d).String() = %q, want the numeric fallback rendering", int(r), r.String())
		}
	}
}

// TestReason_TokensAreStableAndDistinct guards the strings themselves. They land in
// audit records and human-facing renderings, so a duplicate would make two
// different decision paths indistinguishable in the artifact an incident review
// reads, and a missing case would render as a number.
func TestReason_TokensAreStableAndDistinct(t *testing.T) {
	seen := make(map[string]Reason, len(allReasons))
	for _, r := range allReasons {
		token := r.String()
		switch {
		case token == "":
			t.Errorf("%d renders as the empty string", int(r))
		case strings.HasPrefix(token, "reason("):
			t.Errorf("%d has no case in String(), rendering as %q", int(r), token)
		case token != strings.ToLower(token) || strings.ContainsAny(token, " _"):
			t.Errorf("%d renders as %q; tokens are lowercase and hyphen-delimited", int(r), token)
		}
		if prev, dup := seen[token]; dup {
			t.Errorf("%d and %d both render as %q", int(prev), int(r), token)
		}
		seen[token] = r
	}
}

// TestDecision_Tokens pins the three decision renderings and the fallback.
func TestDecision_Tokens(t *testing.T) {
	for d, want := range map[Decision]string{
		DecisionGate:      "gate",
		DecisionAutoApply: "auto-apply",
		DecisionRefuse:    "refuse",
	} {
		if got := d.String(); got != want {
			t.Errorf("Decision(%d).String() = %q, want %q", int(d), got, want)
		}
	}
	if got := Decision(42).String(); got != "decision(42)" {
		t.Errorf("unknown decision rendered as %q", got)
	}
	if Decision(42).AutoApplies() {
		t.Error("an unrecognized decision authorizes an unattended mutation")
	}
}

// TestVerdict_ZeroValueIsSafe states the property that makes every partially
// constructed or deserialized verdict harmless: the zero value gates.
func TestVerdict_ZeroValueIsSafe(t *testing.T) {
	var v Verdict
	if v.AutoApplies() {
		t.Fatal("the zero Verdict authorizes an unattended mutation")
	}
	if v.Decision != DecisionGate || v.Reason != ReasonNoRuleMatched {
		t.Errorf("zero Verdict = %#v, want gate / no-rule-matched", v)
	}
	if v.String() != "gate: no-rule-matched" {
		t.Errorf("zero Verdict renders as %q", v.String())
	}
}

// TestVerdict_String distinguishes the three renderings. The middle case is the one
// worth a test of its own: a near-miss verdict names a rule, and must not read as
// though that rule permitted anything.
func TestVerdict_String(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    Verdict
		want string
	}{
		{
			name: "auto-apply names the rule and the evidence",
			v:    Verdict{Decision: DecisionAutoApply, Reason: ReasonEarnedTrust, Rule: "restart-payments", Evidence: "3 converged"},
			want: "auto-apply: earned-trust (rule restart-payments; evidence: 3 converged)",
		},
		{
			name: "a matched rule held back by trust says so",
			v:    verdict(ReasonUntrustedShape, "restart-payments"),
			want: "gate: untrusted-shape (rule restart-payments matched)",
		},
		{
			name: "a near miss never claims the rule matched",
			v:    verdict(ReasonNamespaceOutOfScope, "restart-payments"),
			want: "gate: namespace-out-of-scope (closest rule: restart-payments)",
		},
		{
			name: "no rule was considered",
			v:    verdict(ReasonAutonomyNotConfigured, ""),
			want: "gate: autonomy-not-configured",
		},
		{
			name: "a refusal is decided before any rule is read",
			v:    verdict(ReasonOperationOffCatalog, ""),
			want: "refuse: operation-off-catalog",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerdict_PolicyIdentity covers the value an audit record will carry as the
// authorizing policy, including that it is empty for every verdict that authorized
// nothing — a gated action's record must not name a policy that did not fire.
func TestVerdict_PolicyIdentity(t *testing.T) {
	auto := Verdict{Decision: DecisionAutoApply, Reason: ReasonEarnedTrust, Rule: "restart-payments", Evidence: "3 converged"}
	if got := auto.PolicyIdentity(); got != "policy:restart-payments" {
		t.Errorf("PolicyIdentity() = %q, want %q", got, "policy:restart-payments")
	}
	for _, r := range allReasons {
		if r == ReasonEarnedTrust {
			continue
		}
		if got := verdict(r, "restart-payments").PolicyIdentity(); got != "" {
			t.Errorf("%s produced policy identity %q; nothing authorized this action", r, got)
		}
	}
}

// TestPolicyIdentity_CannotCollideWithTheBlanketBypass pins the reason rule names
// are validated lowercase. Both an earned rule and the autonomous-mode bypass are
// recorded in the same audit field under the same prefix, and they mean opposite
// things: the bypass says a human waived review, an earned rule says a human
// approved this shape repeatedly and it worked. A rule name that could spell the
// bypass's marker would let the weaker claim be recorded as the stronger one.
func TestPolicyIdentity_CannotCollideWithTheBlanketBypass(t *testing.T) {
	// Spelled out rather than imported from approve. The gate is a plausible future
	// consumer of this package, and an import here would make that a test-only cycle
	// discovered at the worst moment. The value is a constant in a deployment
	// manifest, so it does not drift.
	const bypassMarker = "MAKLAUDE_DANGEROUSLY_AUTO_APPROVE"

	if err := validateName(strings.ToLower(bypassMarker)); err == nil {
		t.Error("a rule may be named after the bypass marker in lowercase; underscores must stay rejected")
	}
	if err := validateName(bypassMarker); err == nil {
		t.Error("a rule may be named exactly after the bypass marker")
	}
	if !strings.HasPrefix(PolicyPrefix+bypassMarker, PolicyPrefix) {
		t.Fatal("the bypass records under a different prefix than this package assumes")
	}
}

// TestVerdict_NeverClaimsHumanApproval is a wording guard on the one sentence this
// package must never be able to write. Nobody approved an auto-applied action, so
// no rendering of any verdict may suggest somebody did — the trail is the entire
// oversight surface for these, and a record that laundered an unreviewed action
// into a reviewed one would be worse than having no policy at all.
func TestVerdict_NeverClaimsHumanApproval(t *testing.T) {
	forbidden := []string{"approved", "approval", "human", "reviewed", "consent"}
	for _, r := range allReasons {
		v := verdict(r, "restart-payments")
		v.Evidence = "3 converged"
		rendered := strings.ToLower(v.String())
		for _, word := range forbidden {
			if strings.Contains(rendered, word) {
				t.Errorf("%s renders as %q, which contains %q", r, v.String(), word)
			}
		}
	}
}
