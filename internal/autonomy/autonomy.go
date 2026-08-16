// Package autonomy decides whether MaKlaude may carry out one proposed
// remediation WITHOUT asking a person: auto-apply, gate, or refuse.
//
// It is the first half of Milestone 5's answer to "may this run?", which until now
// had exactly one answer — ask a human. This package makes that a decision instead
// of a constant, and the decision defaults to the old answer: with nothing
// configured, every proposal gates, which is byte-for-byte today's behavior.
//
// # This package decides and does nothing else
//
// [Decide] is a pure function of its arguments. Nothing here holds a Kubernetes
// client, reads a cluster, opens a file, consults an environment variable, or
// reads a clock — not even indirectly. It cannot execute an action, cannot record
// one, and cannot approve one; it returns a [Verdict], and a caller decides what to
// do with it. That is the same discipline [remediate] applies to planning, for the
// same payoff: a decision that touches nothing is safe to compute continuously,
// cheap to unit-test for exact output, and impossible to get wrong in a way that
// reaches a cluster by itself.
//
// Purity also buys the property Milestone 5 needs most from this layer:
// determinism. Identical inputs produce an identical [Verdict], including the
// reason token and the rule name, on every call and in every process. A decision to
// mutate a cluster with nobody watching is exactly the wrong place for a
// probabilistic component, so there is no LLM anywhere in this path and no
// iteration over a map whose order could vary.
//
// # The three answers, and why "refuse" is not just a louder "gate"
//
//   - [DecisionGate] — autonomy was not granted. The proposal goes through the
//     human approval gate exactly as it does today. This is the default, the zero
//     value, and the outcome of every uncertainty.
//   - [DecisionAutoApply] — a configured rule permits this operation, in this
//     scope, and the shape has EARNED it (see [TrustOracle]). No human is asked.
//   - [DecisionRefuse] — the proposal is not something this system will act on at
//     all, by any authority. Not auto-applied, and not offered to a human either.
//
// The third answer exists because "ask a person" is only a safe fallback for a
// proposal MaKlaude can accurately describe and bound. An operation outside
// [remediate]'s closed catalog, or one classified irreversible, is neither: the
// catalog is what every downstream layer's rollback plans, preconditions and
// previews are written against, so a value outside it means the proposal is
// corrupt or the catalog grew without this layer being updated. Handing that to a
// human as an approval request would ask them to consent to something no part of
// the system can render honestly. Refusing is the fail-closed reading, and it is
// deliberately NOT configurable — see [Ruleset.Validate].
//
// Note what refuse does not cover. `deletepod`, `rollbackrevision` and `cordonnode`
// are all in the catalog and all absent from the shipped allowlist, and they GATE
// rather than refuse. Milestone 5 adds a sixth condition on top of Milestone 4's
// five and removes none of them: an operation this package declines to automate is
// still an operation a named human may approve.
//
// # Default-deny is structural, not a default value
//
// Every path to [DecisionAutoApply] runs through an explicit configured rule that
// names the cluster, the namespace, and the operation, plus a trust oracle that
// says the shape earned it. There is no wildcard, no "all namespaces" spelling, and
// no empty-means-everything field: an empty selector makes a rule INVALID rather
// than permissive, and one invalid rule gates the entire ruleset. So the ways to
// arrive at autonomy by accident — a half-written rule, a typo'd cluster name, a
// config file that failed to load — all converge on gate, which is the behavior the
// operator already has.
//
// # What this layer deliberately cannot do
//
// [Decide] sees one proposal at a time and has no memory, so it cannot bound
// autonomy ACROSS proposals or across time: rate caps, per-target cooldowns, and
// the circuit breaker are a separate concern with separate state, and belong to
// task T4. A caller must treat [DecisionAutoApply] as "this proposal is eligible",
// not as "go", until that layer exists.
//
// Loading rules from disk or environment is likewise not here. This package models
// and validates a [Ruleset]; turning an operator's file into one is
// [internal/rules], and the environment variable naming that file is
// [operate.AutonomyRulesEnv]. Keeping the loader out means [Decide] stays a pure
// function, and it means the format can change without touching the decision.
package autonomy

import (
	"strconv"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Decision is what the policy concluded about one proposal. It is a closed enum so
// callers branch on it exhaustively, and its zero value is [DecisionGate] so a
// forgotten or partially-constructed [Verdict] reads as "ask a human" rather than
// as permission.
type Decision int

const (
	// DecisionGate means no autonomy was granted: the proposal takes the ordinary
	// human approval path, unchanged. It is the zero value on purpose — every
	// uncertainty in this package resolves here.
	DecisionGate Decision = iota

	// DecisionAutoApply means a configured rule permits the action and the shape has
	// earned it. It is the only value that lets an action proceed without a person,
	// and the only one [ReasonEarnedTrust] produces.
	DecisionAutoApply

	// DecisionRefuse means the proposal must not be acted on by any authority,
	// including a human's. See the package doc for why this is a distinct answer
	// rather than an emphatic gate.
	DecisionRefuse
)

// String renders the decision as a stable lowercase token. The tokens are part of
// this package's contract — they land in audit records and human-facing renderings,
// so they must not change casually.
func (d Decision) String() string {
	switch d {
	case DecisionGate:
		return "gate"
	case DecisionAutoApply:
		return "auto-apply"
	case DecisionRefuse:
		return "refuse"
	default:
		return "decision(" + strconv.Itoa(int(d)) + ")"
	}
}

// AutoApplies reports whether this decision permits acting without a human. It
// exists so call sites read as a question about authority rather than as an
// equality check a future third auto-apply-ish value could silently escape.
func (d Decision) AutoApplies() bool { return d == DecisionAutoApply }

// Reason is WHY the policy decided as it did. It is a closed enum rather than a
// prose field for the same reasons [approve.Reason] is: consumers can branch on it,
// tests can assert the exact decision path, and every value has one stable
// human-facing token that reaches the audit trail, so an operator reads the same
// reason the code saw.
//
// Each reason implies exactly one [Decision] — see [Reason.Decision]. That mapping
// is the single table the whole package agrees on, which is what makes it
// impossible for a new reason to be introduced without stating what it authorizes.
type Reason int

const (
	// ReasonNoRuleMatched — rules are configured and valid, none of them covers this
	// proposal, and no dimension was identified as the near miss.
	//
	// It is the zero value so that a zero [Verdict] pairs with the zero [Decision]
	// and reads as a safe one. In the current selector ladder it is not reachable
	// from [Decide]: every configured rule fails on some dimension, and each of those
	// has its own reason. It is kept as the unclassified fallback so that adding a
	// selector dimension cannot produce a verdict with an unset reason.
	ReasonNoRuleMatched Reason = iota

	// ReasonAutonomyNotConfigured — the ruleset is empty or absent. This is the
	// shipped posture and the one an operator who has never opted in will see on
	// every proposal.
	ReasonAutonomyNotConfigured

	// ReasonRulesetInvalid — the ruleset does not validate, so NOTHING in it is
	// honored, including its well-formed rules. Partially honoring a malformed
	// configuration would grant autonomy under rules the operator did not finish
	// writing, and would do it silently; gating everything makes the mistake visible
	// on the first proposal instead of on the first unattended mutation.
	ReasonRulesetInvalid

	// ReasonClusterMismatch — the caller's cluster, the proposal's cluster, and the
	// target's cluster do not all agree. The audit layer duplicates the cluster onto
	// every record precisely so a disagreement is visible rather than impossible to
	// spot; a proposal that fails that check is not one to reason about further.
	ReasonClusterMismatch

	// ReasonOperationOffCatalog — the operation is not one of [remediate]'s catalog
	// values. Refused rather than gated: see the package doc.
	ReasonOperationOffCatalog

	// ReasonIrreversible — the proposal is classified [remediate.ReversibilityIrreversible].
	// No catalog operation carries that class today, so reaching it means the catalog
	// grew past what this layer was written against. Refused unconditionally, and no
	// configuration can permit it.
	ReasonIrreversible

	// ReasonReversibilityUnknown — the proposal's reversibility is outside the
	// defined range, so its risk cannot be compared against any floor. Refused,
	// because an unclassifiable action is strictly worse than an irreversible one:
	// at least the irreversible case knows what it is.
	ReasonReversibilityUnknown

	// ReasonClusterOutOfScope — no rule names this proposal's cluster. Autonomy is
	// granted per cluster and never globally, so an unlisted cluster is untouched by
	// the whole ruleset.
	ReasonClusterOutOfScope

	// ReasonOperationNotAllowed — a rule covers this cluster but does not list this
	// operation. This is the shipped answer for `deletepod`, `rollbackrevision` and
	// `cordonnode` under the default allowlist: gated, not refused, and widenable by
	// an operator who decides otherwise.
	ReasonOperationNotAllowed

	// ReasonAboveReversibilityFloor — a rule covers this cluster and operation, but
	// the proposal's reversibility class is riskier than that rule permits. The floor
	// is a second, independent bound on the allowlist: widening the operation list
	// does not widen the risk class.
	ReasonAboveReversibilityFloor

	// ReasonClusterScopedTarget — the target has no namespace (a node, say), so the
	// namespace dimension cannot bound its blast radius. A cluster-scoped action is
	// therefore never auto-applied, under any rule and any trust. This is a real
	// consequence rather than an oversight: every rule must name namespaces, and
	// there is no namespace to name.
	ReasonClusterScopedTarget

	// ReasonNamespaceOutOfScope — a rule covers this cluster and operation, and does
	// not list the target's namespace.
	ReasonNamespaceOutOfScope

	// ReasonNoTrustLedger — a rule matched, and no trust oracle was supplied, so no
	// shape can have earned anything. It is a SEPARATE reason from
	// [ReasonUntrustedShape] because the two need different fixes: this one says the
	// ledger is not wired up, that one says the history is not there yet.
	ReasonNoTrustLedger

	// ReasonUntrustedShape — a rule matched and the shape has not earned autonomy.
	// This is the expected steady state on a fresh install: the machinery is real and
	// everything still gates until a history of human-approved, converged executions
	// exists.
	ReasonUntrustedShape

	// ReasonTrustEvidenceMissing — the oracle called the shape trusted but cited no
	// evidence. Trust here is derived from a recorded history, so a claim with
	// nothing to point at is a declaration, and a declaration is exactly the blank
	// cheque [approve.AutoApproveEnv] already covers honestly. Refusing to honor it
	// keeps "earned" meaning earned.
	ReasonTrustEvidenceMissing

	// ReasonEarnedTrust — a rule permits the action and the shape earned it. The one
	// and only reason that produces [DecisionAutoApply].
	ReasonEarnedTrust

	// ReasonChaosNeverPromotes — the proposal is a deliberate fault ([ClassChaos]), so
	// it gates no matter what the ruleset and the ledger say.
	//
	// This is a normal outcome an operator should read as the design working, not as a
	// misconfiguration: an experiment's value is that its outcome is unknown, so a
	// history of clean injections is not evidence the next one is safe. See
	// [ClassChaos]. It is decided before any rule is read, which is why it carries no
	// rule name.
	ReasonChaosNeverPromotes

	// ReasonProposalClassUnknown — the request's class is unset or is a value this
	// build does not recognize, so what the proposal even IS cannot be established.
	//
	// It gates rather than refusing, because the fault is in the caller rather than in
	// the action: an unclassified request is a wiring bug, and the human gate is the
	// posture that keeps the action reviewable while somebody fixes it. Unlike every
	// other reason here it should never appear in a healthy system, and its presence
	// in a report means a [Request] was built without its class.
	ReasonProposalClassUnknown
)

// String renders the reason as a stable lowercase token, for audit records, logs,
// and anything an operator reads.
func (r Reason) String() string {
	switch r {
	case ReasonNoRuleMatched:
		return "no-rule-matched"
	case ReasonAutonomyNotConfigured:
		return "autonomy-not-configured"
	case ReasonRulesetInvalid:
		return "ruleset-invalid"
	case ReasonClusterMismatch:
		return "cluster-mismatch"
	case ReasonOperationOffCatalog:
		return "operation-off-catalog"
	case ReasonIrreversible:
		return "irreversible"
	case ReasonReversibilityUnknown:
		return "reversibility-unknown"
	case ReasonClusterOutOfScope:
		return "cluster-out-of-scope"
	case ReasonOperationNotAllowed:
		return "operation-not-allowed"
	case ReasonAboveReversibilityFloor:
		return "above-reversibility-floor"
	case ReasonClusterScopedTarget:
		return "cluster-scoped-target"
	case ReasonNamespaceOutOfScope:
		return "namespace-out-of-scope"
	case ReasonNoTrustLedger:
		return "no-trust-ledger"
	case ReasonUntrustedShape:
		return "untrusted-shape"
	case ReasonTrustEvidenceMissing:
		return "trust-evidence-missing"
	case ReasonEarnedTrust:
		return "earned-trust"
	case ReasonChaosNeverPromotes:
		return "chaos-never-promotes"
	case ReasonProposalClassUnknown:
		return "proposal-class-unknown"
	default:
		return "reason(" + strconv.Itoa(int(r)) + ")"
	}
}

// Decision reports what this reason authorizes. It is the package's single source
// of truth for that mapping: [Decide] never assembles a [Verdict] by naming a
// decision and a reason independently, so the two can never disagree, and adding a
// reason without extending this switch produces a gate rather than an accident.
//
// An unrecognized value gates, which is the same fail-closed direction everything
// else in this package takes.
func (r Reason) Decision() Decision {
	switch r {
	case ReasonEarnedTrust:
		return DecisionAutoApply
	case ReasonClusterMismatch, ReasonOperationOffCatalog, ReasonIrreversible, ReasonReversibilityUnknown:
		return DecisionRefuse
	default:
		return DecisionGate
	}
}

// Shape is the granularity autonomy is earned at: one operation on one cluster.
//
// It is deliberately not per-object. Pods are ephemeral and a per-object history
// would never accumulate — the object whose restarts went well last week does not
// exist this week — so per-object trust would be permanently empty and the whole
// mechanism inert. It is also deliberately not per-operation-globally: trust earned
// on a staging cluster says nothing about production, and multi-cluster isolation
// is a first-class property everywhere else in this system.
//
// Shape is a comparable value so it can be used directly as a map key, which is
// what [StaticTrust] relies on.
type Shape struct {
	// Cluster is the registered cluster name.
	Cluster string

	// Operation is the catalog operation. See [remediate.Operation].
	Operation remediate.Operation
}

// String renders the shape as "cluster/operation", a stable form suitable for a
// rule name suffix, a log line, or a map dump in a test failure.
func (s Shape) String() string { return s.Cluster + "/" + string(s.Operation) }

// Verdict is the complete result of one policy decision: what to do, why, and the
// attribution a later audit record needs to answer "who permitted this?" without
// re-deriving anything.
//
// It is a plain value with no behaviour beyond rendering, so it is trivially
// comparable in a test and serializable onto a trail.
type Verdict struct {
	// Decision is what to do. It is always [Reason.Decision] of the reason below —
	// see there for why the two cannot drift.
	Decision Decision

	// Reason is why. See [Reason].
	Reason Reason

	// Rule names the rule this verdict is about: the rule that permitted the action
	// when the decision is [DecisionAutoApply], the rule that matched when the
	// proposal was held back only by trust, and otherwise the rule that came closest
	// to matching. It is empty when no rule was even considered — an empty or invalid
	// ruleset, or a refusal, which is decided before any rule is read.
	//
	// A near-miss rule is named because the operator's next question after "why did
	// my rule not fire?" is "which one did I get wrong?", and a reason token alone
	// names the dimension without naming the rule. [Verdict.String] renders the two
	// cases differently so a reader is never told a rule permitted something it did
	// not.
	Rule string

	// Evidence is the trust oracle's citation for why this shape had earned autonomy.
	// It is set only on [DecisionAutoApply], where it is the record of the oversight
	// that replaced a human's: nobody approved this action, so the history that
	// stood in for approval is the thing an incident review reads.
	Evidence string
}

// AutoApplies reports whether this verdict permits acting without a human.
func (v Verdict) AutoApplies() bool { return v.Decision.AutoApplies() }

// PolicyPrefix namespaces a rule name where it is recorded as an authorizing
// policy, alongside a human's login in the same field. It matches the prefix the
// blanket bypass already records under, so a reader (or a script) sees one shape
// for "not a person" rather than two.
const PolicyPrefix = "policy:"

// PolicyIdentity renders the authorizing policy for an audit record: the rule name
// under [PolicyPrefix], or empty when this verdict authorized nothing.
//
// It is what makes an earned rule distinguishable from the blanket bypass, which
// records the same prefix over an environment variable name. The distinction is the
// whole point of this milestone — the bypass means a human waived review, an earned
// rule means a human approved this shape repeatedly and it worked — so a renderer
// that collapsed the two would be a bug, and naming the specific rule is what makes
// collapsing them impossible.
//
// The two can never collide: [Rule.Name] is validated lowercase, and the bypass's
// marker is an upper-case environment variable name.
func (v Verdict) PolicyIdentity() string {
	if !v.AutoApplies() || v.Rule == "" {
		return ""
	}
	return PolicyPrefix + v.Rule
}

// String renders the verdict as one stable line for a trail, a log, or an issue
// comment. The three decisions read differently on purpose: an auto-apply names the
// rule and the evidence that replaced human review, a gate that matched a rule
// names it as matched-but-untrusted, and a near miss names the rule without
// implying it granted anything.
func (v Verdict) String() string {
	switch {
	case v.Decision == DecisionAutoApply:
		return "auto-apply: " + v.Reason.String() + " (rule " + v.Rule + "; evidence: " + v.Evidence + ")"
	case v.Rule == "":
		return v.Decision.String() + ": " + v.Reason.String()
	case v.Reason == ReasonUntrustedShape || v.Reason == ReasonNoTrustLedger || v.Reason == ReasonTrustEvidenceMissing:
		return v.Decision.String() + ": " + v.Reason.String() + " (rule " + v.Rule + " matched)"
	default:
		return v.Decision.String() + ": " + v.Reason.String() + " (closest rule: " + v.Rule + ")"
	}
}

// verdict builds a [Verdict] from a reason, deriving the decision so no call site
// can pair a reason with a decision that contradicts it.
func verdict(r Reason, rule string) Verdict {
	return Verdict{Decision: r.Decision(), Reason: r, Rule: rule}
}
