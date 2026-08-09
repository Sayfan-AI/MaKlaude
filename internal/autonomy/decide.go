package autonomy

import (
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Decide is the policy: one proposal in, one [Verdict] out.
//
// It is total — every input produces a verdict, including a corrupt proposal, a
// malformed ruleset, and a nil oracle — and it is deterministic: identical
// arguments produce an identical verdict, reason token, rule name and citation, on
// every call. It reads no clock, no environment, no file and no cluster.
//
// # The order of the questions is the safety argument
//
// The checks run in a fixed order, hardest first, and each one that fires ends the
// decision:
//
//  1. Do the caller, the proposal and the target agree on the cluster? A
//     disagreement is a corrupt proposal, and the worst possible failure of this
//     system is a mutation aimed at the wrong cluster. Refuse.
//  2. Is the operation in the catalog? An unknown operation cannot be previewed,
//     rolled back, or honestly described to a human. Refuse.
//  3. Is the action reversible enough to be classified at all? Irreversible, or
//     outside the defined range, is refused unconditionally.
//  4. Is the ruleset valid? A malformed configuration gates everything, entirely.
//  5. Is anything configured? An empty ruleset is the shipped posture: gate.
//  6. Does a rule cover this exact proposal? No rule, no autonomy.
//  7. Has the shape EARNED it? A rule is permission to consider, not permission to
//     act.
//
// Steps 1 to 3 come before the ruleset is even read, which is what makes their
// refusals unconditional: there is no configuration that reaches them, because no
// configuration is consulted. Step 7 comes last for the opposite reason — trust is
// the expensive question and the one that changes between cycles, and asking it
// only for proposals a rule already permits keeps the oracle's inputs narrow.
//
// # What a caller may conclude
//
// [DecisionAutoApply] means this proposal is ELIGIBLE to run unattended. It is not
// "go": this function sees one proposal with no memory, so it cannot bound how many
// actions a cycle takes, how often one target is touched, or whether a run of
// failures should trip everything back to fully gated. Those are the blast-radius
// layer's job (task T4), and a caller that acts on this verdict alone has autonomy
// with no ceiling.
func Decide(cluster string, p remediate.Proposal, rs Ruleset, trust TrustOracle) Verdict {
	if v, refused := refuse(cluster, p); refused {
		return v
	}
	// The validation error's text is deliberately dropped rather than carried on the
	// verdict. [Ruleset.Validate] is exported so the loader that reads an operator's
	// configuration can refuse to start and say exactly what is wrong, which is where
	// a person is actually looking; re-checking here is the runtime backstop against
	// a ruleset that reached this function unvalidated, and its whole job is to make
	// that case gate rather than to explain it on every proposal.
	if err := rs.Validate(); err != nil {
		return verdict(ReasonRulesetInvalid, "")
	}
	if len(rs) == 0 {
		return verdict(ReasonAutonomyNotConfigured, "")
	}

	matched, nearest, nearestReason := match(p, rs)
	if matched == nil {
		return verdict(nearestReason, nearest)
	}
	return decideTrust(cluster, p.Operation, matched.Name, trust)
}

// refuse runs the three unconditional checks — the ones no configuration can
// reach. It reports whether one fired, and the verdict if so.
func refuse(cluster string, p remediate.Proposal) (Verdict, bool) {
	if cluster == "" || p.Cluster != cluster || p.Target.Cluster != cluster {
		return verdict(ReasonClusterMismatch, ""), true
	}
	if !catalogOperation(p.Operation) {
		return verdict(ReasonOperationOffCatalog, ""), true
	}
	switch {
	case p.Reversibility == remediate.ReversibilityIrreversible:
		return verdict(ReasonIrreversible, ""), true
	case p.Reversibility < remediate.ReversibilityReversible, p.Reversibility > remediate.ReversibilityIrreversible:
		return verdict(ReasonReversibilityUnknown, ""), true
	}
	return Verdict{}, false
}

// nearMiss ranks how far a rule got before it failed to match, so a verdict can
// name the rule the operator most likely meant. Higher is closer.
//
// The ranking is what turns "no rule matched" into an actionable message. An
// operator debugging a rule that did not fire needs the dimension AND the rule;
// reporting the first rule in the file would usually name an unrelated one, and
// reporting none would leave them diffing their own config against a token.
func nearMiss(r Reason) int {
	switch r {
	case ReasonOperationNotAllowed:
		return 2
	case ReasonAboveReversibilityFloor:
		return 3
	case ReasonClusterScopedTarget, ReasonNamespaceOutOfScope:
		return 4
	default: // ReasonClusterOutOfScope, and anything unclassified.
		return 1
	}
}

// match finds the first rule covering the proposal, or the nearest miss.
//
// First-match-wins is deterministic and sufficient: rules only ever grant, so two
// rules that both cover a proposal permit exactly the same thing and differ only in
// the name that lands in the record. Naming the first keeps the verdict stable
// against a rule appended later.
func match(p remediate.Proposal, rs Ruleset) (matched *Rule, nearest string, nearestReason Reason) {
	nearestReason = ReasonNoRuleMatched
	best := 0
	for i := range rs {
		r := &rs[i]
		why, ok := covers(r, p)
		if ok {
			return r, "", ReasonNoRuleMatched
		}
		if rank := nearMiss(why); rank > best {
			best, nearest, nearestReason = rank, r.Name, why
		}
	}
	return nil, nearest, nearestReason
}

// covers reports whether one rule permits one proposal, and when it does not, which
// dimension refused. The dimensions are checked coarsest first so the reported
// near miss is the furthest the proposal got.
func covers(r *Rule, p remediate.Proposal) (Reason, bool) {
	switch {
	case !r.coversCluster(p.Cluster):
		return ReasonClusterOutOfScope, false
	case !r.coversOperation(p.Operation):
		return ReasonOperationNotAllowed, false
	case !r.permitsReversibility(p.Reversibility):
		return ReasonAboveReversibilityFloor, false
	case p.Target.Namespace == "":
		return ReasonClusterScopedTarget, false
	case !r.coversNamespace(p.Target.Namespace):
		return ReasonNamespaceOutOfScope, false
	}
	return ReasonNoRuleMatched, true
}

// decideTrust asks the oracle whether the matched rule may actually fire.
//
// The three ways to fail here are separate reasons rather than one, because they
// need three different responses from an operator: wire up the ledger, wait for the
// history to accumulate, or fix a ledger that is claiming trust it cannot evidence.
func decideTrust(cluster string, op remediate.Operation, rule string, trust TrustOracle) Verdict {
	if trust == nil {
		return verdict(ReasonNoTrustLedger, rule)
	}
	evidence := trust.Trust(Shape{Cluster: cluster, Operation: op})
	if !evidence.Trusted {
		// BREAK-VERIFICATION (issue #146, assertion (a)) — DO NOT MERGE: a shape with
		// NO trust history is granted autonomy anyway. assertUntrustedShapeGates must
		// fail the e2e on this branch; a green run means the assertion lacks teeth.
		v := verdict(ReasonEarnedTrust, rule)
		v.Evidence = "break-verification: fabricated trust for an untrusted shape"
		return v
	}
	citation := strings.TrimSpace(evidence.Citation)
	if citation == "" {
		return verdict(ReasonTrustEvidenceMissing, rule)
	}
	v := verdict(ReasonEarnedTrust, rule)
	v.Evidence = citation
	return v
}
