package autonomy

import (
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Decide is the policy for a remediation: one proposal in, one [Verdict] out.
//
// It is the [ClassRemediation] projection of [DecideRequest], which is the actual
// decision function and the one every class goes through. The two exist because a
// remediation proposal is the overwhelmingly common input and carries everything the
// policy needs, so making every remediation call site build a [Request] by hand would
// add a place to get the class wrong for no gain.
//
// See [DecideRequest] for the ladder, the guarantees, and what a caller may conclude.
func Decide(cluster string, p remediate.Proposal, rs Ruleset, trust TrustOracle) Verdict {
	return DecideRequest(Remediation(cluster, p), rs, trust)
}

// DecideRequest is the policy: one classified request in, one [Verdict] out.
//
// It is total — every input produces a verdict, including a corrupt request, an
// unclassified one, a malformed ruleset, and a nil oracle — and it is deterministic:
// identical arguments produce an identical verdict, reason token, rule name and
// citation, on every call. It reads no clock, no environment, no file and no cluster.
//
// # The order of the questions is the safety argument
//
// The checks run in a fixed order, hardest first, and each one that fires ends the
// decision:
//
//  1. Do the caller, the proposal and the target agree on the cluster? A
//     disagreement is a corrupt proposal, and the worst possible failure of this
//     system is a mutation aimed at the wrong cluster. Refuse.
//  2. Can this CLASS of action ever run unattended? A deliberate fault cannot, and
//     an unclassified request cannot. Gate, before any rule or ledger is consulted.
//  3. Is the operation in the catalog? An unknown operation cannot be previewed,
//     rolled back, or honestly described to a human. Refuse.
//  4. Is the action reversible enough to be classified at all? Irreversible, or
//     outside the defined range, is refused unconditionally.
//  5. Is the ruleset valid? A malformed configuration gates everything, entirely.
//  6. Is anything configured? An empty ruleset is the shipped posture: gate.
//  7. Does a rule cover this exact proposal? No rule, no autonomy.
//  8. Has the shape EARNED it? A rule is permission to consider, not permission to
//     act.
//
// Steps 1 to 4 come before the ruleset is even read, which is what makes their
// answers unconditional: there is no configuration that reaches them, because no
// configuration is consulted. Step 2's placement is the structural half of "chaos
// never promotes" — the trust question in step 8 is not merely answered no for an
// experiment, it is unreachable — and steps 3 and 4 sit BELOW it because they ask
// remediation's questions: a chaos action is not in the remediation catalog and its
// fault ends by a declared self-limit rather than by being undone, so putting them
// first would refuse every experiment outright instead of gating it. Step 8 comes
// last for the opposite reason to the rest — trust is the expensive question and the
// one that changes between cycles, and asking it only for proposals a rule already
// permits keeps the oracle's inputs narrow.
//
// # What a caller may conclude
//
// [DecisionAutoApply] means this proposal is ELIGIBLE to run unattended. It is not
// "go": this function sees one proposal with no memory, so it cannot bound how many
// actions a cycle takes, how often one target is touched, or whether a run of
// failures should trip everything back to fully gated. Those are the blast-radius
// layer's job, and a caller that acts on this verdict alone has autonomy with no
// ceiling.
//
// [DecisionGate] means a human decides. For [ClassChaos] that is the ONLY answer
// this function can give, and it still leaves the ceiling to be asked about
// separately: an approved experiment is subject to the same budget, cooldown and
// breaker as an unattended remediation, which is a bound on top of the human's
// consent rather than a substitute for it.
func DecideRequest(req Request, rs Ruleset, trust TrustOracle) Verdict {
	if v, decided := unconditional(req); decided {
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

	matched, nearest, nearestReason := match(req, rs)
	if matched == nil {
		return verdict(nearestReason, nearest)
	}
	return decideTrust(req, matched.Name, trust)
}

// unconditional runs the checks no configuration can reach: the cluster agreement,
// the class's eligibility, and the two remediation admissibility questions. It
// reports whether one fired, and the verdict if so.
//
// The class check is deliberately in the middle. Above it sits the one question that
// is universal — every class must agree with its caller about which cluster it is
// touching — and below it sit the two that are remediation's alone. Ordering it this
// way is what makes a chaos request GATE rather than be refused as off-catalog, while
// keeping the remediation ladder byte-for-byte the same as before the class existed.
func unconditional(req Request) (Verdict, bool) {
	if req.Cluster == "" || req.ProposalCluster != req.Cluster || req.Target.Cluster != req.Cluster {
		return verdict(ReasonClusterMismatch, ""), true
	}
	if !req.Class.MayAutoApply() {
		return verdict(req.Class.gateReason(), ""), true
	}
	if !catalogOperation(req.Operation) {
		return verdict(ReasonOperationOffCatalog, ""), true
	}
	switch {
	case req.Reversibility == remediate.ReversibilityIrreversible:
		return verdict(ReasonIrreversible, ""), true
	case req.Reversibility < remediate.ReversibilityReversible, req.Reversibility > remediate.ReversibilityIrreversible:
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
func match(req Request, rs Ruleset) (matched *Rule, nearest string, nearestReason Reason) {
	nearestReason = ReasonNoRuleMatched
	best := 0
	for i := range rs {
		r := &rs[i]
		why, ok := covers(r, req)
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
func covers(r *Rule, req Request) (Reason, bool) {
	switch {
	case !r.coversCluster(req.Cluster):
		return ReasonClusterOutOfScope, false
	case !r.coversOperation(req.Operation):
		return ReasonOperationNotAllowed, false
	case !r.permitsReversibility(req.Reversibility):
		return ReasonAboveReversibilityFloor, false
	case req.Target.Namespace == "":
		return ReasonClusterScopedTarget, false
	case !r.coversNamespace(req.Target.Namespace):
		return ReasonNamespaceOutOfScope, false
	}
	return ReasonNoRuleMatched, true
}

// decideTrust asks the oracle whether the matched rule may actually fire.
//
// The three ways to fail here are separate reasons rather than one, because they
// need three different responses from an operator: wire up the ledger, wait for the
// history to accumulate, or fix a ledger that is claiming trust it cannot evidence.
//
// The whole request is taken rather than its operation, because the question is not
// only "has this shape earned autonomy" but "did a human approve THIS fix" — see
// [Subject] and [remediate.Proposal.Fingerprint]. The fingerprint travels on the
// request, computed by the projection that built it, which keeps [DecideRequest] pure:
// it is a hash of fields already in hand, with no clock and no I/O.
//
// Only [ClassRemediation] reaches here. A class that cannot auto-apply is answered in
// [unconditional], several steps above, so this function never sees an experiment and
// the oracle is never asked a question about one.
func decideTrust(req Request, rule string, trust TrustOracle) Verdict {
	if trust == nil {
		return verdict(ReasonNoTrustLedger, rule)
	}
	evidence := trust.Trust(Subject{
		Shape:       Shape{Cluster: req.Cluster, Operation: req.Operation},
		Fingerprint: req.Fingerprint,
	})
	if !evidence.Trusted {
		return verdict(ReasonUntrustedShape, rule)
	}
	citation := strings.TrimSpace(evidence.Citation)
	if citation == "" {
		return verdict(ReasonTrustEvidenceMissing, rule)
	}
	v := verdict(ReasonEarnedTrust, rule)
	v.Evidence = citation
	return v
}
