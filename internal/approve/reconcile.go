package approve

import (
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Decide is the pure heart of the gate: given one current proposal, the artifact
// tracking it, and the time, it returns the single [Action] to take — including,
// in exactly one branch, an [Authorization].
//
// It performs no I/O, holds no client, and reads no clock of its own (now is
// injected), so the decision that governs whether MaKlaude may change a production
// cluster is a total function of values a test can construct exhaustively. That is
// the point: this is the highest-stakes logic in the system, so it is the logic
// with no network, no ordering, and no hidden state to reason around.
//
// # The order of the checks is part of the contract
//
// Disqualifications are evaluated before the grant, and among themselves in the
// order a human would want to read them. An artifact that has already executed is
// reported as such even if its approval has also expired, because "this already
// ran" is the more useful thing to know; a failed dry-run is reported before drift,
// because an action the server refuses is not worth discussing the freshness of.
// The one invariant that matters more than the ordering: every path that is not
// the single authorize branch produces NO authorization, and the authorize branch
// is reachable only when all of the conditions in the package doc hold.
//
// # Where the bypass fits into that order, and where it deliberately does not
//
// [Policy.AutoApprove] is read in exactly one place — the undecided branch, and the
// consent half of [disqualify] — and it is read AFTER the three checks that outrank
// consent entirely. An executed artifact is still held, a human's `rejected` label is
// still honored, and an artifact whose displayed evidence has moved is still refreshed
// before anything is authorized against it. Putting the bypass earlier would have been
// simpler and would have made "policy says go" beat "a person said no", which is the
// one ordering this gate can never have.
func Decide(req Request, pending PendingAction, policy Policy, now time.Time) Action {
	policy = policy.normalized()

	base := Action{
		Identity: req.Identity(),
		Request:  req,
		Ref:      pending.Ref,
		ThreadTS: pending.ThreadTS,
	}

	// No artifact yet: this is a question nobody has been asked.
	if pending.Ref == "" {
		base.Kind = ActionOpen
		base.Reason = ReasonNewProposal
		return base
	}

	// Already ran. Nothing re-authorizes it, and nothing re-asks: the artifact is
	// held, frozen at the state that was actually executed, until the proposal stops
	// being made — at which point it closes as completed.
	if pending.Executed {
		base.Kind = ActionHold
		base.Reason = ReasonAlreadyExecuted
		return base
	}

	// A rejection is a decision. It is sticky for as long as the proposal persists
	// — the artifact keeps the label and the gate keeps reading it — so a "no" is
	// never quietly re-asked next cycle. Holding rather than refreshing also freezes
	// the body at what was actually declined, so the record says what the human
	// turned down rather than what MaKlaude would propose today.
	if pending.State == StateRejected {
		base.Kind = ActionHold
		base.Reason = ReasonRejected
		return base
	}

	// Undecided. Either it has waited too long to still be a fair question, its
	// evidence has moved and must be re-rendered, or it is already current and is
	// left strictly alone — see [ReasonPreviewCurrent] for why doing nothing here is
	// required rather than merely tidy.
	if pending.State != StateApproved {
		if expired(pending, policy, now) {
			base.Kind = ActionWithdraw
			base.Reason = ReasonPendingExpired
			return base
		}
		if !previewCurrent(req, pending, policy) {
			base.Kind = ActionRefresh
			base.Reason = ReasonPreviewChanged
			return base
		}
		if !policy.AutoApprove {
			base.Kind = ActionHold
			base.Reason = ReasonPreviewCurrent
			return base
		}

		// Autonomous mode. Consent is waived; every other reason not to act still
		// applies, so the same disqualification chain runs with only its consent half
		// switched off.
		//
		// A disqualified auto-approval HOLDS rather than refuses, which is the one place
		// the two paths differ in shape. Refusing exists to strip an approval label and
		// tell the person who applied it why their decision was not honored, and here
		// there is no label and no such person — while the artifact's body already states
		// the problem prominently (a failed dry-run, a missing rollback plan) to whoever
		// reads it. Refusing anyway would post an identical comment every reconciliation
		// pass forever, which is how a trail teaches its readers to stop reading it.
		if reason, ok := disqualify(req, pending, policy, now, true); !ok {
			base.Kind = ActionHold
			base.Reason = reason
			return base
		}
		base.Kind = ActionAuthorize
		base.Reason = ReasonAutoApproved
		base.Authorization = grant(req, pending, AuthorityPolicy, now)
		return base
	}

	// From here the artifact is approved. Every remaining check is a reason NOT to
	// honor it; each refuses, withdraws the approval, and re-asks with current
	// evidence rather than closing, because the action may still be right against
	// the state that actually exists now.
	if reason, ok := disqualify(req, pending, policy, now, policy.AutoApprove); !ok {
		base.Kind = ActionRefuse
		base.Reason = reason
		return base
	}

	authority := authorityFor(pending)
	base.Kind = ActionAuthorize
	base.Reason = ReasonApprovalValid
	if !authority.HumanReviewed() {
		base.Reason = ReasonAutoApproved
	}
	base.Authorization = grant(req, pending, authority, now)
	return base
}

// authorityFor decides what kind of authority an APPROVED artifact's label event
// actually establishes.
//
// It is only ever called on the approved branch, and only after [disqualify] has
// passed — which, with the bypass off, already guarantees a named non-self approver,
// so it returns [AuthorityHuman] and the bypass changes nothing about a real human
// approval. What it exists for is the case the bypass creates: with consent waived,
// [Decide] reaches this point on artifacts whose approval is self-applied or carries no
// recoverable actor, and those must not be recorded as human approvals merely because
// a label happens to be present.
//
// The test is positive rather than negative — "a named actor who is demonstrably not
// MaKlaude" rather than "not obviously MaKlaude" — because the two differ exactly where
// it matters. Under `genesis serve`, MaKlaude acts as the operator's own account, so a
// label it applied is indistinguishable from one the operator applied; the sink reports
// ApproverIsSelf for both, and both correctly come out as policy-waived. Attributing a
// human approval requires evidence, not the absence of counter-evidence. Giving the
// local agent its own identity, which is what would make attribution possible there
// again, is issue #125.
func authorityFor(pending PendingAction) Authority {
	if pending.Approver != "" && !pending.ApproverIsSelf {
		return AuthorityHuman
	}
	return AuthorityPolicy
}

// disqualify runs the checks that can invalidate an otherwise-approved artifact,
// returning ok=true only when none of them fires. Splitting it out keeps [Decide]'s
// control flow readable and makes the disqualification set enumerable in one place
// — which matters, because a check that is accidentally dropped here is a check
// that silently stops protecting anything.
//
// # waived is the bypass, and it is scoped to four named checks
//
// When waived is true, the four checks that ask something about a HUMAN'S DECISION are
// skipped: self-approval, attribution, approval-before-preview ordering, and approval
// freshness. Each is meaningless without a decision to judge — there is no approver to
// name, no approval instant to order against the preview, and nothing to go stale.
//
// Everything else runs unchanged, and the split is by question rather than by
// convenience: a check belongs in the waivable set only if it answers "did somebody say
// yes, and does that yes still count?". The failed dry-run, the missing rollback plan,
// and the resourceVersion drift answer "does this still make sense to run?", which is
// a question about the cluster, and no amount of configured trust makes it go away. The
// relative ORDER of the surviving checks is unchanged too, so an artifact wrong in more
// than one way reports the same reason with the bypass on as it would with it off.
func disqualify(req Request, pending PendingAction, policy Policy, now time.Time, waived bool) (Reason, bool) {
	// The API server already said this would fail. Nothing downstream improves that.
	if req.Preview.Failed() {
		return ReasonPreviewFailed, false
	}

	// An operation with no rollback plan was never described honestly to the
	// approver, so their approval cannot cover it. This fires only if the catalog
	// grows without the plan being written — refusing is what makes that a visible
	// bug rather than an invisibly widened blast radius.
	if _, ok := rollbackPlan(req.Proposal.Operation); !ok {
		return ReasonNoRollbackPlan, false
	}

	if !waived {
		// MaKlaude approving its own proposal is not a gate. This is checked before the
		// attribution check because a self-approval DOES carry an identity — it is
		// attributable and still worthless, so "we know who" is not the question here.
		if pending.ApproverIsSelf {
			return ReasonSelfApproval, false
		}

		// An approval nobody can be named for is not attributable, and attribution is
		// the whole reason the signal is a label event rather than a comment.
		if pending.Approver == "" {
			return ReasonUnattributedApproval, false
		}
	}

	// The object moved since the artifact displayed it. The approved action and the
	// available action are no longer the same action — which is as true of an action
	// policy waved through as of one a person approved, so this is never waived.
	if pending.PreviewedResourceVersion != req.Proposal.Target.ResourceVersion {
		return ReasonDrift, false
	}

	if !waived {
		// The approval was recorded before the artifact last displayed this preview, so
		// it is consent to something that has since been replaced. Without this check a
		// body refreshed between "human reads" and "human clicks approve" would
		// re-point a decision at a state nobody read — and drift detection alone would
		// not notice, because by then the displayed and current versions agree.
		if !pending.PreviewedAt.IsZero() && pending.DecidedAt.Before(pending.PreviewedAt) {
			return ReasonApprovalPredatesPreview, false
		}

		// Consent to mutate a live system is perishable.
		if !pending.DecidedAt.IsZero() && now.Sub(pending.DecidedAt) > policy.ApprovalTTL {
			return ReasonApprovalExpired, false
		}
	}

	return ReasonApprovalValid, true
}

// previewCurrent reports whether the artifact already displays what a reader needs
// to see: the target at its current resourceVersion, the current dry-run outcome, and
// the approval posture the gate is actually operating under.
//
// All three halves fail CLOSED — an unrecoverable marker leaves its field empty, which
// matches no real resourceVersion and no token, so an unparseable body re-renders
// rather than being assumed current. The alternative failure direction would freeze a
// corrupt artifact in place forever.
//
// It deliberately does not compare the dry-run's wording. A summary or diff that
// changes while the object does not is the same state described slightly
// differently, and re-rendering on it would put this back to refreshing every pass —
// which is exactly what [ReasonPreviewCurrent] exists to stop.
//
// The gate-mode half is the one thing here that can change while the CLUSTER does not:
// an operator turning the autonomous-mode bypass on rewrites what every open artifact
// means without touching a single object. Including it costs one refresh pass across
// the trail at the moment the mode flips, and buys the guarantee that no artifact ever
// promises "nothing runs until a human adds the `approved` label" while MaKlaude is
// about to run it unattended. See [PendingAction.GateMode].
func previewCurrent(req Request, pending PendingAction, policy Policy) bool {
	if pending.PreviewedResourceVersion != req.Proposal.Target.ResourceVersion {
		return false
	}
	if pending.GateMode != gateModeToken(policy) {
		return false
	}
	return pending.PreviewedState == previewStateToken(req.Preview)
}

// expired reports whether an undecided artifact has waited longer than the
// configured [Policy.PendingTTL]. A nil TTL (the default) means an undecided
// proposal waits indefinitely, so this always reports false — see
// [Policy.PendingTTL] for why "off" is a nil pointer rather than a zero duration.
func expired(pending PendingAction, policy Policy, now time.Time) bool {
	if policy.PendingTTL == nil || *policy.PendingTTL <= 0 {
		return false
	}
	if pending.PreviewedAt.IsZero() {
		return false
	}
	return now.Sub(pending.PreviewedAt) > *policy.PendingTTL
}

// Reconcile computes the deterministic plan of [Action]s that brings the approval
// trail into agreement with the proposals MaKlaude is currently making. Like
// [Decide] it is pure — no I/O, no clock, no cluster — and like
// [escalate.Reconcile] it is a three-way set comparison, here keyed on
// [remediate.ProposalIdentity]:
//
//   - Proposed, not tracked -> ask (see [Decide], which returns [ActionOpen]).
//   - Proposed and tracked  -> whatever [Decide] makes of the recorded decision.
//   - Tracked, not proposed -> [ActionWithdraw]. THIS IS THE IMPORTANT ONE: a
//     pending approval is not a queued job. If the reason to act has gone away, so
//     has the authority to act, whether or not a human already said yes.
//
// Duplicates are handled defensively, as the escalation trail does, because the
// external system is not under this package's control. Two requests sharing an
// identity act on the first only. Two open artifacts claiming the same identity —
// possible after a crash between create and label, or a human opening a colliding
// issue — keep the first and withdraw the rest, so the trail self-heals toward
// exactly one artifact per decision. Collapsing duplicates matters more here than
// on the escalation trail: two artifacts for one action are two chances to approve
// it, and therefore two executions of something a human meant to allow once.
//
// The returned plan is ordered deterministically, and the ordering is a safety
// property rather than a cosmetic one: withdrawals first (drop authority that
// should no longer exist), then refusals (revoke approvals that cannot be
// honored), then opens and refreshes (ask and re-ask), and authorizations LAST, so
// that when a caller starts acting on the plan the trail has already been settled.
// Each group is sorted by identity for stable, reviewable output.
func Reconcile(reqs []Request, tracked []PendingAction, policy Policy, now time.Time) []Action {
	// Index the current proposals, keeping the first occurrence of each identity so
	// a duplicated request cannot produce two decisions.
	currentByID := make(map[remediate.ProposalIdentity]Request, len(reqs))
	for i := range reqs {
		id := reqs[i].Identity()
		if _, seen := currentByID[id]; !seen {
			currentByID[id] = reqs[i]
		}
	}

	seenTracked := make(map[remediate.ProposalIdentity]bool, len(tracked))
	var withdraws, refuses, opens, refreshes, authorizes []Action

	for i := range tracked {
		pa := tracked[i]
		req, stillProposed := currentByID[pa.Identity]

		switch {
		case !stillProposed:
			// The proposal is gone. Whether it healed on its own or the action already
			// ran and fixed it, the artifact closes — and it closes WITHOUT executing,
			// which is the self-heal guarantee.
			reason := ReasonSelfHealed
			if pa.Executed {
				reason = ReasonCompleted
			}
			withdraws = append(withdraws, Action{
				Kind:     ActionWithdraw,
				Reason:   reason,
				Identity: pa.Identity,
				Ref:      pa.Ref,
				ThreadTS: pa.ThreadTS,
			})

		case seenTracked[pa.Identity]:
			// A duplicate artifact for a proposal already handled above. Withdraw it so
			// one action can never collect two approvals.
			withdraws = append(withdraws, Action{
				Kind:     ActionWithdraw,
				Reason:   ReasonSelfHealed,
				Identity: pa.Identity,
				Ref:      pa.Ref,
				ThreadTS: pa.ThreadTS,
			})

		default:
			seenTracked[pa.Identity] = true
			a := Decide(req, pa, policy, now)
			switch a.Kind {
			case ActionAuthorize:
				authorizes = append(authorizes, a)
			case ActionRefuse:
				refuses = append(refuses, a)
			case ActionWithdraw:
				withdraws = append(withdraws, a)
			default:
				// ActionRefresh and ActionHold. Both are grouped here because neither
				// changes the trail's authority; the gatekeeper distinguishes them when
				// it decides whether to touch the sink at all.
				refreshes = append(refreshes, a)
			}
		}
	}

	// Anything proposed with no surviving artifact is a question nobody has been
	// asked. Iterating the input slice (not the map) keeps it deterministic before
	// the sort.
	for i := range reqs {
		id := reqs[i].Identity()
		if seenTracked[id] {
			continue
		}
		if plannedOpen(opens, id) {
			continue
		}
		opens = append(opens, Decide(reqs[i], PendingAction{}, policy, now))
	}

	sortActions(withdraws)
	sortActions(refuses)
	sortActions(opens)
	sortActions(refreshes)
	sortActions(authorizes)

	plan := make([]Action, 0, len(withdraws)+len(refuses)+len(opens)+len(refreshes)+len(authorizes))
	plan = append(plan, withdraws...)
	plan = append(plan, refuses...)
	plan = append(plan, opens...)
	plan = append(plan, refreshes...)
	plan = append(plan, authorizes...)
	return plan
}

// plannedOpen reports whether an open action for the identity is already in the
// slice, so a duplicated request yields a single artifact.
func plannedOpen(opens []Action, id remediate.ProposalIdentity) bool {
	for i := range opens {
		if opens[i].Identity == id {
			return true
		}
	}
	return false
}
