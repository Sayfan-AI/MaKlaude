// Package audit is the end-to-end record of every mutating action MaKlaude takes:
// what was proposed, who authorized it, what was actually sent, what the target
// looked like beforehand, whether it worked, and how to reverse it.
//
// It is the "what it did, and on whose authority" counterpart to the health
// packages' "what it saw". Milestones 1–3 made MaKlaude's observations auditable;
// this makes its ACTIONS auditable, which is a strictly harder requirement because
// the reader of an audit trail is usually someone reconstructing an incident after
// the fact, with no access to the process that produced it and no way to ask it a
// question.
//
// # One record per thing that happened, appended and never revised
//
// A [Record] is a complete, self-contained account of one lifecycle event. Complete
// means it stands alone: every record carries the proposal identity, the cluster,
// the operation, the target, AND the approver, so a reader who finds one record in
// isolation can still answer "which action is this and who allowed it" without
// joining it to anything. That redundancy is deliberate. A trail whose entries only
// make sense in sequence is a trail that becomes unreadable the moment one entry is
// lost, truncated, or quoted somewhere else.
//
// Appended and never revised means exactly that: [Trail] exposes no way to modify
// or remove a record, and [Trail.Append] stamps a monotonically increasing
// [Record.Seq] under a lock, so the order two concurrent executions were recorded
// in is a fact rather than a race. Wall-clock timestamps are recorded too, but Seq
// is what ordering is defined by — two records written in the same nanosecond, or
// written either side of a clock adjustment, still have a defined order.
//
// # Recorded time is not event time
//
// [Record.RecordedAt] is when the entry was WRITTEN, which is not when the thing it
// describes happened. A human's approval is recorded in [Approver.ApprovedAt]; the
// gate honoring it is [Approver.AuthorizedAt]; the proposal being computed is
// [Action.ProposedAt]; the attempt's own bounds are [Change.StartedAt] and
// [Change.FinishedAt]. Keeping the two apart is what lets an execution layer append
// its records once it knows the outcome, without the trail claiming that a human
// approved something at the moment MaKlaude got around to writing it down.
//
// # Nothing sensitive survives an append
//
// A record is rendered into a GitHub issue, which on a public repository is
// world-readable, and the free text it carries is cluster-derived — an API server
// error, a container's own message, a convergence observation. Any of those can
// contain a credential that leaked into the cluster. So [Trail.Append] passes every
// free-text field through [redact.String] before storing it, and [Lifecycle] does
// the same before rendering, using the same rules the M3 model-egress boundary uses
// rather than a second set that would drift from them.
//
// Structured identifiers are deliberately NOT redacted — the proposal identity, the
// target, the operation, the approver's login, resourceVersions, and the enum
// tokens. They are what makes the trail a trail; over-redacting them would destroy
// the linkage the package exists to provide, and none of them is a place a secret
// can plausibly hide. See [Record.redacted] for the exact split.
//
// # It is a value layer, not an actor
//
// Nothing here reads a cluster, calls an API, or decides anything. The execution
// layer builds records and appends them; this package sequences, sanitizes, stores,
// and renders. That keeps the audit trail testable without a network and — more to
// the point — keeps it incapable of the failures it exists to record.
package audit

import "strconv"

// Phase names where in an action's lifecycle a [Record] sits. The lifecycle an
// operator is promised is:
//
//	proposed → approved → executed → verified / failed → optionally rolled back
//
// The enum covers all of it, including the one phase the execution layer never
// emits ([PhaseProposed]), so a reader of the type sees the whole shape rather than
// only the part one caller happens to write.
//
// The zero value is [PhaseUnknown] rather than any real phase, so a record that was
// never populated cannot masquerade as a successful execution.
type Phase int

const (
	// PhaseUnknown means the record's phase was never set. It is the zero value and
	// is rendered as such rather than being guessed at.
	PhaseUnknown Phase = iota

	// PhaseProposed means MaKlaude computed the action and put it to a human.
	//
	// The execution layer does not emit this: by the time it runs, the proposal is
	// minutes old, and a record stamped "proposed" at execution time would assert a
	// time that is simply wrong. [Lifecycle] renders the proposed stage from
	// [Action.ProposedAt], which is the real instant, so the stage is visible whether
	// or not a record for it exists. The phase is here so the approval layer can emit
	// one later without the enum having to grow.
	PhaseProposed

	// PhaseApproved means a permission slip existed and covered this exact action.
	// The record carries who granted it, when they granted it, and when the gate
	// honored it.
	PhaseApproved

	// PhaseExecuted means a mutating request was sent. It does NOT by itself mean the
	// cluster changed: a server-side preview is also an executed request, and
	// [Change.Applied] is the field that distinguishes them.
	PhaseExecuted

	// PhaseVerified means the bounded observation window completed and its verdict is
	// recorded in [Outcome.Convergence].
	//
	// The phase records that verification HAPPENED, not that it succeeded. A verdict
	// of "timed-out" or "unobservable" is still a verified phase — the action ran, the
	// cluster was watched, and this is what watching found. Collapsing a non-converged
	// verdict into [PhaseFailed] would be the more comfortable reading and the wrong
	// one: the execution did not fail, and calling it a failure is what would make an
	// operator roll back a remediation that was merely slow.
	PhaseVerified

	// PhaseFailed means the attempt terminated with a failure class. It covers both
	// the clean aborts (a drifted precondition, a precondition conflict at the API
	// server) and the real failures, because whether an abort was healthy is a
	// property of the failure class rather than of the phase — see
	// [Outcome.CleanAbort].
	//
	// A failed ROLLBACK is also this phase, distinguished by [Rollback.Attempted].
	PhaseFailed

	// PhaseRolledBack means an inverse action put the target back at its recorded
	// pre-state, either by sending the inverse or by finding someone had already
	// restored it ([Rollback.AlreadyAtPreState]).
	PhaseRolledBack
)

// String renders the phase as a stable lowercase token. The tokens are part of the
// package's contract: they appear in stored records, in rendered artifacts, and in
// test fixtures, so they must not change casually.
func (p Phase) String() string {
	switch p {
	case PhaseUnknown:
		return "unknown"
	case PhaseProposed:
		return "proposed"
	case PhaseApproved:
		return "approved"
	case PhaseExecuted:
		return "executed"
	case PhaseVerified:
		return "verified"
	case PhaseFailed:
		return "failed"
	case PhaseRolledBack:
		return "rolled-back"
	default:
		return "phase(" + strconv.Itoa(int(p)) + ")"
	}
}

// Authority says WHAT KIND of thing authorized an action, as distinct from which
// particular one did.
//
// It exists because "approved by" stopped meaning one thing. Almost every
// authorization traces to a person who applied a label; under the autonomous-mode
// bypass ([approve.AutoApproveEnv]) MaKlaude may act with no human in the loop at all —
// and the one thing such a mode must never do is write a record that reads like a
// person reviewed something no person saw. A trail that overstates human involvement is
// worse than no trail: it launders an unreviewed action into a reviewed one,
// permanently, in the artifact an incident review will trust.
//
// So the kind of authority is a field rather than an inference from whether
// [Approver.Identity] happens to look like a login. The bypass sets [AuthorityPolicy]
// and this package renders it differently; nothing about the record's shape had to
// change when it landed, and no record written before it was reinterpreted. That is
// what defining the enum ahead of its first writer bought.
//
// The zero value is [AuthorityUnattributed], so a record built without an approver
// says "nobody is named" rather than silently claiming a human.
type Authority int

const (
	// AuthorityUnattributed means no authority is recorded. It is the zero value, and
	// it is what an execution refused for lack of a valid permission slip carries —
	// which is precisely the case where claiming an approver would be a lie.
	AuthorityUnattributed Authority = iota

	// AuthorityHuman means a person authorized the action. [Approver.Identity] is
	// their login and [Approver.ApprovedAt] is when they recorded the decision.
	AuthorityHuman

	// AuthorityPolicy means configured policy waived the human approval and no person
	// reviewed the action. [Approver.Identity] names the policy, never a login, and
	// [Approver.ApprovedAt] is the zero time because no decision was made.
	//
	// It is written by the execution layer whenever the approval gate granted its
	// permission slip under [approve.AuthorityPolicy] — the autonomous-mode bypass. Note
	// that a process running in autonomous mode still records a genuine, attributable
	// human approval as [AuthorityHuman]: the authority describes what happened to THIS
	// action, not how the process was configured.
	AuthorityPolicy
)

// String renders the authority as a stable lowercase token.
func (a Authority) String() string {
	switch a {
	case AuthorityUnattributed:
		return "unattributed"
	case AuthorityHuman:
		return "human"
	case AuthorityPolicy:
		return "policy"
	default:
		return "authority(" + strconv.Itoa(int(a)) + ")"
	}
}

// HumanReviewed reports whether a person actually looked at the action. It is a
// method rather than a comparison spelled out at each call site because every
// consumer that renders an approver has to make this distinction, and the one that
// forgets is the one that claims a human reviewed a policy-waived action.
func (a Authority) HumanReviewed() bool { return a == AuthorityHuman }
