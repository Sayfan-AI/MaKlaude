// Package approve is MaKlaude's human-in-the-loop gate over mutating actions: it
// turns a [remediate.Proposal] into a pending, approvable artifact on the comms
// trail, and turns an explicit human decision on that artifact back into an
// [Authorization] — the only thing an executor may act on.
//
// It is the hinge of Milestone 4. Everything below it is read-only: health
// collects, detect finds, correlate groups, diagnose ranks, and remediate PLANS a
// mutating action without ever performing one. Everything above it can change a
// cluster. This package is the single place where "MaKlaude thinks X should
// happen" becomes "a named human said X may happen, at this cluster state, once".
//
// # This package is itself read-only
//
// It gates; it does not execute. Nothing here holds a Kubernetes client, opens a
// connection to a cluster, or imports the write path. Its only side effects are on
// the comms trail (issues and chat), exactly as [escalate] — the trail is where
// the decision is asked for, recorded, and audited. An [Authorization] is a
// permission slip, not an action: it is handed back to the caller, who is
// responsible for executing it.
//
// # The approval signal is a label event, and that is deliberate
//
// A human authorizes an action by adding the `approved` label to its artifact, and
// declines by adding `rejected`. The label event is what makes the signal
// ATTRIBUTABLE: GitHub records who applied it and when, from an identity MaKlaude
// cannot forge and did not supply. A comment saying "looks good" would be prose
// this system would have to interpret; a label is a fact it can read. Rejection is
// the same mechanism in the opposite direction, so declining is as cheap and as
// auditable as approving.
//
// # An approval is scope-bound, never a standing grant
//
// One approval authorizes ONE [remediate.Operation] against ONE [remediate.Target]
// at ONE observed cluster state, once. Five independent conditions must all hold
// before [Decide] issues an [Authorization], and each closes a way a naive gate
// leaks authority:
//
//   - The artifact carries an explicit `approved` label with a recorded approver.
//     Absence of a decision is never consent.
//   - The target's resourceVersion still equals the one the artifact displayed.
//     A human approved an action against the cluster they were shown; if the object
//     changed, that cluster no longer exists (see [ReasonDrift]).
//   - The approval was recorded AFTER the preview it claims to authorize. Otherwise
//     a body refreshed between "human reads" and "human clicks" would silently
//     re-point a decision at a state nobody read (see [ReasonApprovalPredatesPreview]).
//   - The approval is younger than [Policy.ApprovalTTL]. Consent to act on a live
//     system is perishable; a week-old approval is a memory, not a decision.
//   - The action has not already run. An artifact whose execution is recorded is
//     never authorized a second time (see [ReasonAlreadyExecuted]).
//
// # Dedup, recurrence, and self-healing mirror the escalation trail
//
// The artifact is keyed on [remediate.ProposalIdentity], which is stable across
// collection cycles and across the several hypotheses that may independently
// arrive at the same action. So a recurring proposal REFRESHES its one artifact
// rather than opening a second, exactly as a recurring incident updates its one
// issue — a human is asked once per decision, not once per cycle.
//
// The counterpart matters more: a proposal that stops being proposed because the
// problem healed on its own is WITHDRAWN, not executed. A pending approval is not
// a queued job. If nobody has acted and the reason to act is gone, the artifact is
// closed with a note explaining why, and the authority it would have carried
// evaporates with it.
package approve

import (
	"sort"
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// ActionRef is an opaque, sink-specific handle to an existing approval artifact
// (for example a GitHub issue number). The pure decision layer never interprets
// it; it only threads it from a [PendingAction] into an [Action] so the
// [Gatekeeper] can hand it back to the [ApprovalSink].
type ActionRef string

// State is the decision a human has (or has not) recorded on an approval
// artifact. It is derived from the artifact's labels, never from prose, so it is
// a fact the gate reads rather than a judgment it makes.
//
// The zero value is [StatePending]: an artifact with no decision label, a sink
// that could not determine the state, and a freshly-constructed zero value all
// mean "no human has said yes", which is the only safe default.
type State int

const (
	// StatePending means no decision label is present. The action is not
	// authorized and will not run.
	StatePending State = iota

	// StateApproved means a human added the `approved` label. It is a NECESSARY
	// condition for authorization, never a sufficient one — see the package doc
	// for the other four.
	StateApproved

	// StateRejected means a human added the `rejected` label. Rejection is sticky
	// for as long as the proposal persists: the artifact stays open carrying the
	// label so the gate keeps seeing the decision and never re-asks. Closing it
	// instead would make the next cycle re-open a fresh artifact and quietly turn a
	// "no" into a repeated question.
	StateRejected
)

// String renders the state as a stable lowercase token used in bodies, comments,
// and test fixtures.
func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateApproved:
		return "approved"
	case StateRejected:
		return "rejected"
	default:
		return "state(" + strconv.Itoa(int(s)) + ")"
	}
}

// Preview is the dry-run evidence attached to a proposal: what the API server
// said when the action was sent with `dryRun=All`, if it was sent at all.
//
// It is supplied BY THE CALLER rather than computed here, and that is the whole
// reason this package can promise it holds no cluster client. Producing a preview
// requires a write-capable (preview-only) path — [kube.Executor] in
// [kube.ExecuteDryRun] mode — which belongs to the execution layer. The gate
// renders whatever evidence it is handed and states plainly when it was handed
// none, so a human is never shown a confident-looking preview that nobody
// actually ran.
type Preview struct {
	// Performed reports whether a dry-run was actually attempted. False means the
	// body says so explicitly, in those words, rather than implying by silence that
	// the action was validated.
	Performed bool

	// Summary is the one-line result of the dry-run, for example what the API
	// server accepted and under which scope. Empty when Performed is false.
	Summary string

	// Diff optionally renders what would change. It is free-form because its shape
	// depends on the operation; it is displayed verbatim and never parsed.
	Diff string

	// Error is the dry-run's failure message, empty on success. A non-empty value
	// is DISQUALIFYING: the gate refuses to authorize an action the API server
	// already rejected as a preview (see [ReasonPreviewFailed]). The artifact is
	// still opened, because "MaKlaude wanted to do this and the server said no" is
	// exactly the kind of thing an operator should see.
	Error string
}

// Failed reports whether the dry-run was attempted and came back an error.
func (p Preview) Failed() bool { return p.Error != "" }

// Request is one proposal put to a human for decision: the planned action plus
// whatever dry-run evidence exists for it. It is the input unit of
// [Gatekeeper.Reconcile] and [Reconcile].
type Request struct {
	// Proposal is the planned mutating action. Its Identity is the durable key the
	// artifact is tracked by, and its Target.ResourceVersion is the cluster state
	// the decision will be bound to.
	Proposal remediate.Proposal

	// Preview is the dry-run evidence for this proposal. The zero value means no
	// dry-run was performed, which the artifact states explicitly.
	Preview Preview
}

// Identity returns the request's durable key — the proposal's identity — which is
// what the trail is keyed on and what [Reconcile] joins against tracked artifacts.
func (r Request) Identity() remediate.ProposalIdentity { return r.Proposal.Identity }

// PendingAction is the gate's view of one open approval artifact on the comms
// trail, reconstructed on every pass from the artifact itself rather than from
// memory. The monitor process may restart at any time, so in-memory "which issue
// holds which decision" cannot be the source of truth; the identity, the preview
// it displayed, and the decision are all recovered from the artifact's body
// markers and labels.
type PendingAction struct {
	// Identity is the proposal this artifact represents, recovered from the body's
	// hidden proposal marker. It is the join key against current requests.
	Identity remediate.ProposalIdentity

	// Ref is the sink-specific handle to the live artifact.
	Ref ActionRef

	// ThreadTS is the chat thread handle recovered from the body's hidden thread
	// marker, empty when none is present (chat unconfigured, or no root posted yet).
	// It gives the approval conversation the same durable, cross-restart continuity
	// the escalation trail has.
	ThreadTS string

	// State is the human decision recorded on the artifact. See [State].
	State State

	// Approver is the login of the human who applied the decision label, recovered
	// from the label event. It is empty for [StatePending] — and an EMPTY approver
	// on an approved artifact is disqualifying, because an approval nobody can be
	// named for is not attributable and this gate's whole promise is attribution.
	Approver string

	// DecidedAt is when the decision label was applied, from the label event. It is
	// the clock the approval's freshness and ordering are judged against — the
	// artifact's own record, not MaKlaude's guess.
	DecidedAt time.Time

	// ApproverIsSelf reports that the decision label was applied by MaKlaude's own
	// account rather than by a person. It is the narrowest and most important form
	// of forgery this gate has to survive: every other check in [disqualify] assumes
	// the approval came from someone other than the thing being approved, and an
	// automation holding an issues:write token can add a label to its own issue.
	//
	// Detecting it requires knowing MaKlaude's own login, which only the sink can
	// know, so the sink reports the fact and [Decide] refuses on it
	// ([ReasonSelfApproval]).
	ApproverIsSelf bool

	// Executed reports whether this action has already run, recovered from the
	// artifact's execution label. It is the durable idempotency flag: it survives a
	// process restart, so a crash between "executed" and "recorded" cannot produce a
	// second execution on the next cycle.
	Executed bool

	// PreviewedResourceVersion is the target resourceVersion the artifact currently
	// displays, recovered from the body's hidden preview marker. Drift detection
	// compares the CURRENT proposal against this — the state the human was shown —
	// rather than against some earlier value the gate happens to remember.
	PreviewedResourceVersion string

	// PreviewedAt is when the body last displayed that resourceVersion. An approval
	// recorded before this instant refers to a preview that has since been replaced,
	// so it authorizes a state its approver never read.
	PreviewedAt time.Time

	// PreviewedState is the dry-run outcome the artifact currently displays — one of
	// the tokens [previewStateToken] renders — recovered from the body's hidden
	// preview-state marker, and empty when no marker is present.
	//
	// It exists so a change in the dry-run's OUTCOME forces a refresh even when the
	// target has not moved. Without it, a dry-run that starts failing against an
	// unchanged object would leave a body that never mentions the failure, while
	// [RefusalComment] told the reader "the preview error is in the issue body" —
	// pointing a human at evidence that is not there. An empty value is read as
	// "unknown", which refreshes: re-rendering a current body is cheap, and showing a
	// stale one is what this field exists to prevent.
	PreviewedState string
}

// ActionKind enumerates what a reconciliation pass can decide to do about one
// proposal. The set is closed and small: every difference between "the actions
// MaKlaude proposes now" and "the decisions recorded on the trail now" reduces to
// asking, re-asking, allowing, refusing, or dropping exactly one artifact.
type ActionKind int

const (
	// ActionOpen creates a new pending artifact for a proposal that has none. It is
	// the only action that asks a human a question they have not been asked.
	ActionOpen ActionKind = iota

	// ActionRefresh rewrites a still-pending artifact so it displays the latest
	// preview and resourceVersion. Recurrence refreshes; it never opens a duplicate.
	ActionRefresh

	// ActionAuthorize reports that every condition for execution holds and carries
	// the resulting [Authorization]. It is the ONLY action that produces one.
	ActionAuthorize

	// ActionRefuse declines to honor an approval that is present but not usable —
	// the object drifted, the approval predates the preview, it expired, or the
	// dry-run failed. It withdraws the approval and re-asks with fresh evidence
	// rather than closing the artifact, because the action may still be the right
	// one against the state that actually exists now.
	ActionRefuse

	// ActionWithdraw closes an artifact whose proposal is no longer being made. The
	// reason to act is gone, so the pending authority to act goes with it.
	ActionWithdraw

	// ActionHold records that an artifact is in a state the gate deliberately does
	// nothing about — a human rejected it, or the action already ran — and touches
	// the sink not at all.
	//
	// It exists as an explicit action rather than as a gap in the plan because
	// "MaKlaude decided to leave this alone" and "MaKlaude never looked at this" are
	// different facts, and only one of them is correct behavior. Emitting it also
	// keeps the plan a total account of every tracked artifact, which is what lets a
	// test assert that no path silently skips one.
	ActionHold
)

// String renders the action kind as a stable lowercase token, used in logs and
// test fixtures.
func (k ActionKind) String() string {
	switch k {
	case ActionOpen:
		return "open"
	case ActionRefresh:
		return "refresh"
	case ActionAuthorize:
		return "authorize"
	case ActionRefuse:
		return "refuse"
	case ActionWithdraw:
		return "withdraw"
	case ActionHold:
		return "hold"
	default:
		return "action(" + strconv.Itoa(int(k)) + ")"
	}
}

// Reason explains WHY an [Action] was chosen. It is a closed enum rather than a
// prose field so consumers can branch on it and tests can assert the exact
// decision path, and every value has a human-facing rendering that lands in the
// artifact so an operator reading the trail sees the same reason the code saw.
type Reason int

const (
	// ReasonNewProposal — a proposal with no artifact. Ask.
	ReasonNewProposal Reason = iota

	// ReasonPreviewChanged — a still-pending proposal whose displayed preview or
	// resourceVersion is out of date. Re-render so the human decides against what
	// is true now.
	ReasonPreviewChanged

	// ReasonPreviewCurrent — a still-pending proposal whose artifact already displays
	// the current state. The question stands unanswered and unchanged, so the pass
	// leaves it strictly alone.
	//
	// Doing nothing here is load-bearing rather than an optimization. Re-rendering an
	// unchanged artifact would re-stamp [PendingAction.PreviewedAt] on every cycle,
	// and that instant is the clock [Policy.PendingTTL] measures against — so an
	// unconditional refresh silently makes the pending-expiry knob unreachable no
	// matter how it is configured. It also spares an operator an "edited" notification
	// every cycle on an issue whose content did not change.
	ReasonPreviewCurrent

	// ReasonApprovalValid — every condition holds; the action is authorized.
	ReasonApprovalValid

	// ReasonDrift — the target's resourceVersion changed since the preview the
	// approval was given against. The approved action and the possible action are no
	// longer the same action.
	ReasonDrift

	// ReasonApprovalPredatesPreview — the approval label was applied before the
	// artifact last displayed its current preview, so it cannot be consent to that
	// preview.
	ReasonApprovalPredatesPreview

	// ReasonApprovalExpired — the approval is older than [Policy.ApprovalTTL].
	ReasonApprovalExpired

	// ReasonUnattributedApproval — the artifact is approved but the sink recovered
	// no approver identity. An approval nobody can be named for fails this gate's
	// core promise, so it is refused rather than honored anonymously.
	ReasonUnattributedApproval

	// ReasonSelfApproval — the approval label was applied by MaKlaude's own account.
	// A system that can approve its own proposals has no gate at all, so this is
	// refused before any other consideration.
	ReasonSelfApproval

	// ReasonPreviewFailed — the dry-run for this action came back an error. The API
	// server has already said the action would not succeed.
	ReasonPreviewFailed

	// ReasonNoRollbackPlan — the operation has no defined rollback plan (a new
	// catalog entry nobody wrote one for). Refused rather than executed, so
	// extending the catalog cannot silently widen what a human can approve blind.
	ReasonNoRollbackPlan

	// ReasonAlreadyExecuted — the action has already run. It never produces an
	// authorization, and the artifact is held (not re-asked) until the proposal
	// stops being made.
	ReasonAlreadyExecuted

	// ReasonRejected — a human declined the action. The artifact is held so the
	// decision stays visible to every later pass; the question is not re-asked while
	// the proposal persists.
	ReasonRejected

	// ReasonSelfHealed — the proposal is no longer being made and was never
	// executed. The problem cleared on its own; withdraw without acting.
	ReasonSelfHealed

	// ReasonCompleted — the proposal is no longer being made and the action DID run.
	// The remediation appears to have worked; close the artifact as done.
	ReasonCompleted

	// ReasonPendingExpired — nobody decided within [Policy.PendingTTL]. Withdraw the
	// stale question; if the proposal is still being made next cycle, a fresh
	// artifact re-asks it with current evidence.
	ReasonPendingExpired
)

// String renders the reason as a stable lowercase token.
func (r Reason) String() string {
	switch r {
	case ReasonNewProposal:
		return "new-proposal"
	case ReasonPreviewChanged:
		return "preview-changed"
	case ReasonPreviewCurrent:
		return "preview-current"
	case ReasonApprovalValid:
		return "approval-valid"
	case ReasonDrift:
		return "drift"
	case ReasonApprovalPredatesPreview:
		return "approval-predates-preview"
	case ReasonApprovalExpired:
		return "approval-expired"
	case ReasonUnattributedApproval:
		return "unattributed-approval"
	case ReasonSelfApproval:
		return "self-approval"
	case ReasonPreviewFailed:
		return "preview-failed"
	case ReasonNoRollbackPlan:
		return "no-rollback-plan"
	case ReasonAlreadyExecuted:
		return "already-executed"
	case ReasonRejected:
		return "rejected"
	case ReasonSelfHealed:
		return "self-healed"
	case ReasonCompleted:
		return "completed"
	case ReasonPendingExpired:
		return "pending-expired"
	default:
		return "reason(" + strconv.Itoa(int(r)) + ")"
	}
}

// Action is one unit of work the [Gatekeeper] should perform against the sink to
// bring the approval trail in line with the current proposals and decisions. It is
// produced only by [Reconcile] and consumed only by the gatekeeper: a pure
// description, never the side effect itself.
type Action struct {
	// Kind is what to do. See [ActionKind].
	Kind ActionKind

	// Reason is why. See [Reason].
	Reason Reason

	// Identity is the proposal this action concerns, present for every kind.
	Identity remediate.ProposalIdentity

	// Request is the current proposal plus preview driving an open, refresh,
	// refuse, or authorize. It is the zero value for an [ActionWithdraw] triggered
	// by absence, which by definition has no current proposal.
	Request Request

	// Ref is the existing artifact to act on. It is empty for [ActionOpen], where no
	// artifact exists yet.
	Ref ActionRef

	// ThreadTS is the chat thread handle recovered from the artifact, so a refresh,
	// refusal, authorization, or withdrawal replies into the original conversation.
	// Empty for [ActionOpen] and whenever no thread marker was present.
	ThreadTS string

	// Authorization is the permission slip, set only for [ActionAuthorize] and
	// always nil otherwise. It is the single object that grants execution.
	Authorization *Authorization
}

// Policy is the operator-tunable part of the gate: how long consent stays good
// for, and whether an unanswered question eventually expires.
//
// Both knobs are [time.Duration] rather than integer seconds so a unit confusion
// cannot survive compilation, and the nullable one is a pointer rather than a
// zero sentinel — see [Policy.PendingTTL] for why that distinction is
// load-bearing here rather than stylistic.
type Policy struct {
	// ApprovalTTL bounds how long an approval remains honorable after it was
	// recorded. Consent to mutate a live system is perishable: an operator who
	// approved a pod deletion on Monday did not approve deleting whatever occupies
	// that name on Friday.
	//
	// A zero value falls back to [DefaultApprovalTTL] rather than meaning "expires
	// instantly" or "never expires". Both readings of zero are wrong in a way that
	// is hard to notice: instant expiry makes the gate look broken (every approval
	// refused, no explanation an operator would believe), and never-expires silently
	// removes a safety property a reader would assume is on. Falling back to the
	// default makes a forgotten field behave like a configured one.
	ApprovalTTL time.Duration

	// PendingTTL, when non-nil, withdraws an artifact nobody has decided within that
	// long, so a stale question is re-asked against current evidence instead of
	// sitting with a preview that has drifted out from under it. Nil means an
	// undecided proposal waits indefinitely, which is the shipped default: a
	// pending action is harmless, and churning artifacts trains operators to ignore
	// them.
	//
	// It is a pointer, not a zero-means-off duration, because forgetting the
	// zero-check here is destructive rather than benign: a zero PendingTTL read as a
	// real value withdraws EVERY artifact on the first pass, including ones a human
	// is in the middle of reading, and the trail would show a system that opens and
	// closes questions in a loop. Nil cannot be mistaken for a duration.
	PendingTTL *time.Duration
}

// DefaultApprovalTTL is how long an approval stays honorable when [Policy] does
// not say otherwise. Two hours is long enough for an operator to approve an action
// and let the next scan cycle pick it up (cycles are minutes, not hours), and short
// enough that consent does not outlive the operator's memory of the cluster state
// they gave it against.
const DefaultApprovalTTL = 2 * time.Hour

// DefaultPolicy returns the shipped policy: approvals expire after
// [DefaultApprovalTTL], and undecided proposals wait indefinitely.
func DefaultPolicy() Policy { return Policy{ApprovalTTL: DefaultApprovalTTL} }

// normalized returns the policy with a usable ApprovalTTL, substituting the
// default for a zero or negative value. See [Policy.ApprovalTTL] for why a
// forgotten field must not be read as either extreme.
func (p Policy) normalized() Policy {
	if p.ApprovalTTL <= 0 {
		p.ApprovalTTL = DefaultApprovalTTL
	}
	return p
}

// sortActions orders a group of actions by proposal identity ascending. Identity
// is fully deterministic, so the resulting order is reproducible for any input.
func sortActions(actions []Action) {
	sort.Slice(actions, func(i, j int) bool {
		return actions[i].Identity < actions[j].Identity
	})
}
