package approve

import (
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Authorization is the permission slip for exactly one mutating action: proof
// that a named human explicitly allowed this [remediate.Operation] against this
// [remediate.Target] at this observed cluster state, and that every condition in
// the package doc held when the gate checked. It is the ONLY thing an executor may
// act on.
//
// # It cannot be forged outside this package
//
// Every field is unexported and there is no exported constructor, so the type
// system — not a convention, not a review comment — is what stops another package
// from manufacturing consent. A caller can write `&approve.Authorization{}`; what
// it cannot do is make [Authorization.Valid] report true, because `granted` is
// unexported and only [grant] sets it. An executor that checks Valid (and it must)
// therefore cannot be handed a permission slip that no human signed, even by
// mistake, even by a future contributor who has not read this comment.
//
// That is a deliberately stronger guarantee than "the executor checks a boolean
// before acting". A boolean argument is easy to pass true; a value only one
// package can construct is not. It is the same reasoning as [kube.NewExecutor]
// refusing to build a write-capable client at all when execution is disabled: make
// the unsafe state unrepresentable rather than merely unreached.
//
// # It is a snapshot, and it is single-use by construction
//
// An Authorization carries the resourceVersion it was granted against, so an
// executor can send it as an optimistic-concurrency precondition and have the API
// server refuse the action if the object moved in the meantime. It carries the
// proposal's preconditions for the same reason: the gate's checks answer "may
// this run?", the preconditions answer "does it still make sense to run?", and both
// must hold. It carries the approver and the decision time so the audit trail can
// name who allowed what, without re-reading anything.
//
// Nothing about the value itself prevents a caller calling execute twice with it —
// values cannot enforce that. Single-execution is enforced durably instead, on the
// trail: [Gatekeeper.RecordExecution] marks the artifact executed, and a later pass
// never authorizes an executed artifact again (see [ReasonAlreadyExecuted]). That
// survives a process restart, which an in-memory "used" flag would not.
type Authorization struct {
	// granted is the unforgeable marker. Only [grant] sets it, so the zero value
	// built by any other package is invalid by construction.
	granted bool

	identity      remediate.ProposalIdentity
	cluster       string
	operation     remediate.Operation
	target        remediate.Target
	reversibility remediate.Reversibility
	preconditions []remediate.Precondition
	approver      string
	approvedAt    time.Time
	authorizedAt  time.Time
	ref           ActionRef
}

// grant builds a valid Authorization. It is unexported on purpose: this function
// is the single place in MaKlaude where permission to mutate a cluster comes into
// existence, and it is called from exactly one place — the authorize branch of
// [Decide], after every condition has been checked.
func grant(req Request, pending PendingAction, now time.Time) *Authorization {
	p := req.Proposal
	return &Authorization{
		granted:       true,
		identity:      p.Identity,
		cluster:       p.Cluster,
		operation:     p.Operation,
		target:        p.Target,
		reversibility: p.Reversibility,
		preconditions: append([]remediate.Precondition(nil), p.Preconditions...),
		approver:      pending.Approver,
		approvedAt:    pending.DecidedAt,
		authorizedAt:  now,
		ref:           pending.Ref,
	}
}

// Valid reports whether this is a real authorization issued by the gate. A nil
// receiver and a zero value both report false, so an executor's guard is a single
// `if !auth.Valid()` with no nil-check in front of it.
//
// An executor MUST call this before acting. It is the cheap half of the contract;
// [Authorization.Matches] is the other half.
func (a *Authorization) Valid() bool { return a != nil && a.granted }

// Matches reports whether this authorization actually covers the proposal an
// executor is about to run — same identity, same operation, same target, and the
// same resourceVersion it was granted against.
//
// It exists because [Authorization.Valid] answers "is this a real permission
// slip?" and not "is it a permission slip for THIS action". An executor holding
// several authorizations, or re-reading a proposal between authorization and
// execution, must not rely on positional correspondence. The resourceVersion
// comparison also makes a late-arriving drift visible before the request is sent,
// rather than relying solely on the API server rejecting it.
func (a *Authorization) Matches(p remediate.Proposal) bool {
	if !a.Valid() {
		return false
	}
	return a.identity == p.Identity &&
		a.operation == p.Operation &&
		a.target == p.Target
}

// Identity returns the proposal this authorization covers.
func (a *Authorization) Identity() remediate.ProposalIdentity {
	if !a.Valid() {
		return ""
	}
	return a.identity
}

// Cluster returns the registered cluster name the action may run against. An
// executor should compare it with its own cluster before acting: an authorization
// is scoped to one cluster and multi-cluster isolation is a first-class concern.
func (a *Authorization) Cluster() string {
	if !a.Valid() {
		return ""
	}
	return a.cluster
}

// Operation returns the exact operation authorized.
func (a *Authorization) Operation() remediate.Operation {
	if !a.Valid() {
		return ""
	}
	return a.operation
}

// Target returns the single object the action may touch, including the
// resourceVersion the authorization is bound to.
func (a *Authorization) Target() remediate.Target {
	if !a.Valid() {
		return remediate.Target{}
	}
	return a.target
}

// Reversibility returns how hard the authorized action is to undo, carried through
// so an executor can apply extra care (or a stricter mode gate) to the riskier
// classes without dereferencing the proposal.
func (a *Authorization) Reversibility() remediate.Reversibility {
	if !a.Valid() {
		return remediate.ReversibilityReversible
	}
	return a.reversibility
}

// Preconditions returns a copy of the conditions that must still hold at execution
// time. The copy means an executor cannot mutate the authorization's record of
// what it was allowed to assume.
func (a *Authorization) Preconditions() []remediate.Precondition {
	if !a.Valid() {
		return nil
	}
	return append([]remediate.Precondition(nil), a.preconditions...)
}

// Approver returns the login of the human who approved the action. It is never
// empty on a valid authorization: an unattributed approval is refused rather than
// granted (see [ReasonUnattributedApproval]).
func (a *Authorization) Approver() string {
	if !a.Valid() {
		return ""
	}
	return a.approver
}

// ApprovedAt returns when the human recorded the approval, from the artifact's
// label event.
func (a *Authorization) ApprovedAt() time.Time {
	if !a.Valid() {
		return time.Time{}
	}
	return a.approvedAt
}

// AuthorizedAt returns when the gate issued this authorization, which is later
// than ApprovedAt by however long it took the next reconciliation pass to run.
// Both timestamps are in the audit record because they answer different questions:
// when a human consented, and when MaKlaude acted on that consent.
func (a *Authorization) AuthorizedAt() time.Time {
	if !a.Valid() {
		return time.Time{}
	}
	return a.authorizedAt
}

// Ref returns the approval artifact this authorization came from, so an executor's
// outcome can be recorded back onto the same auditable trail.
func (a *Authorization) Ref() ActionRef {
	if !a.Valid() {
		return ""
	}
	return a.ref
}

// String renders a compact, log-friendly audit line. An invalid authorization
// renders as such rather than as an empty-looking valid one, so a log never
// suggests a grant that does not exist.
func (a *Authorization) String() string {
	if !a.Valid() {
		return "authorization: INVALID (not granted by the approval gate)"
	}
	return fmt.Sprintf("authorization: %s on cluster %s target %s rv=%s approved by %s at %s",
		a.operation, a.cluster, a.target.String(), a.target.ResourceVersion,
		a.approver, a.approvedAt.UTC().Format(time.RFC3339))
}
