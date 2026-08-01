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
		if !previewCurrent(req, pending) {
			base.Kind = ActionRefresh
			base.Reason = ReasonPreviewChanged
			return base
		}
		base.Kind = ActionHold
		base.Reason = ReasonPreviewCurrent
		return base
	}

	// From here the artifact is approved. Every remaining check is a reason NOT to
	// honor it; each refuses, withdraws the approval, and re-asks with current
	// evidence rather than closing, because the action may still be right against
	// the state that actually exists now.
	if reason, ok := disqualify(req, pending, policy, now); !ok {
		base.Kind = ActionRefuse
		base.Reason = reason
		return base
	}

	base.Kind = ActionAuthorize
	base.Reason = ReasonApprovalValid
	base.Authorization = grant(req, pending, now)
	return base
}

// disqualify runs the checks that can invalidate an otherwise-approved artifact,
// returning ok=true only when none of them fires. Splitting it out keeps [Decide]'s
// control flow readable and makes the disqualification set enumerable in one place
// — which matters, because a check that is accidentally dropped here is a check
// that silently stops protecting anything.
func disqualify(req Request, pending PendingAction, policy Policy, now time.Time) (Reason, bool) {
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

	// The object moved since the artifact displayed it. The approved action and the
	// available action are no longer the same action.
	if pending.PreviewedResourceVersion != req.Proposal.Target.ResourceVersion {
		return ReasonDrift, false
	}

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

	return ReasonApprovalValid, true
}

// previewCurrent reports whether the artifact already displays what a reader needs
// to see: the target at its current resourceVersion, and the current dry-run
// outcome.
//
// Both halves fail CLOSED — an unrecoverable marker leaves its field empty, which
// matches no real resourceVersion and no state token, so an unparseable body
// re-renders rather than being assumed current. The alternative failure direction
// would freeze a corrupt artifact in place forever.
//
// It deliberately does not compare the dry-run's wording. A summary or diff that
// changes while the object does not is the same state described slightly
// differently, and re-rendering on it would put this back to refreshing every pass —
// which is exactly what [ReasonPreviewCurrent] exists to stop.
func previewCurrent(req Request, pending PendingAction) bool {
	if pending.PreviewedResourceVersion != req.Proposal.Target.ResourceVersion {
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
