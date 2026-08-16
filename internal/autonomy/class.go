package autonomy

import (
	"strconv"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file adds the second thing this package can be asked about.
//
// Until Milestone 6 there was one kind of proposal — a remediation, produced from a
// diagnosis of a fault MaKlaude did not cause — and [Decide] could take a
// [remediate.Proposal] directly. Chaos introduces a second kind: a fault MaKlaude
// causes on purpose, on a cluster explicitly marked eligible. It has to reach the
// same decision function, because the alternative is a second policy path that can
// drift from the reviewed one, and a write path nobody compares against the gated
// half is exactly what this project spends its safety argument avoiding.
//
// So the decision's input becomes a [Request] carrying a [Class], and the class is
// what the policy branches on. Two properties follow from that and are the whole
// reason the class is a type rather than a bool:
//
//   - A class that may never run unattended says so once, in [Class.MayAutoApply],
//     rather than at each of the call sites that would have to remember.
//   - A class nobody has classified is not silently the promotable one. The zero
//     value is [classUnset], which never auto-applies, so a [Request] built by a
//     future caller that forgets the field gates rather than inheriting remediation's
//     authority.

// Class is what kind of action a proposal is: something MaKlaude does to FIX a
// cluster, or something it does to BREAK one on purpose.
//
// The distinction is load-bearing rather than descriptive. Autonomy is earned by
// evidence — a shape whose fixes converged repeatedly under human approval may
// eventually run without one — and that inference is only valid for remediation. See
// [ClassChaos] for why the same evidence means nothing for a deliberate fault.
type Class int

const (
	// classUnset is the zero value and belongs to no proposal.
	//
	// It is unexported so nothing outside this package can name it, and it exists so
	// that forgetting to set [Request.Class] fails safe: an unclassified request can
	// never auto-apply, and [Decide] gates it with [ReasonProposalClassUnknown] rather
	// than reading the ruleset as though it were a remediation.
	classUnset Class = iota

	// ClassRemediation is a proposal produced from a diagnosis: a fix for a fault
	// MaKlaude observed and did not cause. It is the only class that can ever reach
	// [DecisionAutoApply], and only then through the full ladder in [Decide].
	ClassRemediation

	// ClassChaos is a deliberate fault: an experiment MaKlaude injects to find out how
	// the system behaves while a cluster is broken.
	//
	// It NEVER promotes, whatever the trust ledger says, and the reasoning belongs here
	// rather than only in issue #193 because an issue is not a carrier the next reader
	// of this code reaches:
	//
	// Promotion's evidence means "this fix worked three times". For chaos the same
	// evidence only means "the injection succeeded three times", which is a statement
	// about Chaos Mesh rather than about safety. An experiment's value is that its
	// outcome is unknown. A track record of clean injections is not evidence the next
	// one is safe.
	//
	// So an injection always gates. What it DOES inherit is every bound: the same
	// decision function refuses the same corrupt input, and the blast-radius budget,
	// the per-target cooldown and the per-cluster circuit breaker apply to it exactly
	// as they apply to an unattended remediation. Chaos is bounded by M5's ceiling
	// without being eligible for M5's autonomy.
	ClassChaos
)

// String renders the class as a stable lowercase token. The tokens reach audit
// records and human-facing renderings, so they must not change casually.
func (c Class) String() string {
	switch c {
	case ClassRemediation:
		return "remediation"
	case ClassChaos:
		return "chaos"
	case classUnset:
		return "unclassified"
	default:
		return "class(" + strconv.Itoa(int(c)) + ")"
	}
}

// MayAutoApply reports whether a proposal of this class is even ELIGIBLE to run
// unattended — before any rule, any trust evidence, or any ceiling is consulted.
//
// This is the structural half of "chaos never promotes". It is a property of the
// class, checked in [Decide] before the ruleset is read, so no configuration and no
// ledger state can produce an auto-applied injection: there is no path through the
// decision that reaches the trust question for a class that answers false here.
// [TestFullyTrustedClusterStillGatesChaos] is the executable form of that claim.
//
// An unrecognized class answers false, which is the same fail-closed direction
// everything else in this package takes.
func (c Class) MayAutoApply() bool { return c == ClassRemediation }

// gateReason is why a class that cannot auto-apply is being gated. It exists so the
// two non-promotable cases are distinguishable in the record: one is a deliberate
// design property an operator should read as normal, the other is a caller that
// built a request wrong and needs fixing.
func (c Class) gateReason() Reason {
	if c == ClassChaos {
		return ReasonChaosNeverPromotes
	}
	return ReasonProposalClassUnknown
}

// Request is one question for [Decide]: may this action run, and may it run without
// a human?
//
// It is the narrow projection of a proposal that the policy actually reads — cluster,
// operation, target, reversibility, fingerprint — plus the class. Deciding on a
// projection rather than on a whole [remediate.Proposal] is what lets a chaos
// experiment reach the same function without being dressed up as a remediation: a
// chaos proposal has no hypothesis, no incident, no cause and no confidence, and
// forcing it into a remediation-shaped value to satisfy a signature would put a lie
// in the record. See [Remediation] for the remediation projection; the chaos one is
// the internal/chaos package's own Proposal.Request, which sets [ClassChaos].
//
// It is a plain comparable value with no behaviour beyond the projections below, so a
// test can build one literally and compare verdicts exactly.
type Request struct {
	// Class is what kind of action this is. The zero value never auto-applies; see
	// [classUnset].
	Class Class

	// Cluster is the cluster the CALLER believes it is acting on.
	Cluster string

	// ProposalCluster is the proposal's OWN record of the cluster it is for.
	//
	// It is a separate field from Cluster and from Target.Cluster, and all three are
	// compared, because they are three independent facts that a confused caller, a
	// mis-plumbed registry, or a proposal built for one cluster and handed to another
	// pass can make disagree. This system's audit layer duplicates the cluster onto
	// every record for exactly that reason: a disagreement must be visible rather than
	// impossible to spot. Collapsing them into one field would delete the check by
	// making it tautological.
	ProposalCluster string

	// Operation is the action, in its own class's vocabulary: a [remediate] catalog
	// operation for a remediation, and a chaos-namespaced token for an experiment.
	//
	// The type is shared with remediation deliberately, because [Shape] is keyed on it
	// and a chaos action must be nameable in the same records. The tokens cannot
	// collide: a chaos operation carries a prefix no catalog operation has, and
	// [Rule.validate] refuses any operation outside [catalogOperation] — so no
	// operator can write a rule that names one, which is a second structural bar in
	// front of chaos autonomy, independent of [Class.MayAutoApply].
	Operation remediate.Operation

	// Target is the object the action touches, or for an experiment the scope its
	// fault may reach. Its Cluster must agree with Cluster above.
	Target remediate.Target

	// Reversibility is how recoverable the action is, for the classes where that is a
	// meaningful question. It is only read for [ClassRemediation]: a chaos class gates
	// before the reversibility ladder, because "reversible" describes undoing a change
	// MaKlaude made, whereas a fault ends by its own declared self-limit — Chaos Mesh
	// reverting it on expiry, or the fault being a single event with nothing to revert.
	Reversibility remediate.Reversibility

	// Fingerprint identifies the specific fix a human approved, and is what makes
	// trust per-fix rather than per-shape. It is only read for [ClassRemediation],
	// since no other class reaches the trust question.
	Fingerprint remediate.Fingerprint
}

// Remediation projects a remediation proposal onto a decision request.
//
// The cluster is passed separately from the proposal for the same reason [Decide]
// checks them against each other: the caller's belief about which cluster it is
// operating and the proposal's own record of it are two facts, and this system's
// audit layer duplicates the cluster onto every record precisely so a disagreement
// is visible rather than impossible to spot.
func Remediation(cluster string, p remediate.Proposal) Request {
	return Request{
		Class:           ClassRemediation,
		Cluster:         cluster,
		ProposalCluster: p.Cluster,
		Operation:       p.Operation,
		Target:          p.Target,
		Reversibility:   p.Reversibility,
		// Computed here rather than taken as an argument so [Decide] stays pure: it is a
		// hash of fields already in hand, with no clock and no I/O.
		Fingerprint: p.Fingerprint(),
	}
}
