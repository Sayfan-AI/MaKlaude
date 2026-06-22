// Package escalate turns the deterministic [detect.Finding]s MaKlaude observes
// into a durable, auditable human-facing comms trail — one tracked external
// issue per active problem — and keeps that trail in sync as problems recur and
// clear.
//
// Where the detect package decides what a cluster's facts MEAN (a node is
// NotReady — critical), this package decides what to TELL the human about them
// and how to keep that telling honest over time. It is the "communicate" role
// of the multi-agent operations pattern, implemented deterministically: there
// is no LLM judgment here, only a stable mapping from the current set of
// findings to the set of issues that should exist.
//
// # Why issue-per-problem keyed by identity
//
// A health monitor runs in a loop: the SAME ongoing problem is re-detected on
// every cycle. Naively notifying per cycle would bury an operator in duplicate
// alerts for a single unhealthy pod. The detect package already gives us the
// primitive that fixes this: [detect.Identity], a stable key that is the same
// across cycles for the same problem and independent of severity, message, or
// timestamp. This package builds the comms trail directly on that key:
//
//   - A newly seen identity OPENS one issue.
//   - A recurring identity (seen again on a later cycle) UPDATES the existing
//     issue — a refreshed body and a recurrence comment — it never opens a
//     second one. This is the load-bearing dedup guarantee.
//   - An identity that was tracked but is no longer in the current findings has
//     CLEARED: its issue is closed with a closing comment, so the record stays
//     auditable rather than silently vanishing.
//
// # Why state is rediscovered, not just remembered
//
// The monitor process may restart at any time, so in-memory "which issue maps
// to which identity" cannot be the source of truth. Instead the identity is
// embedded inside the issue itself (a hidden marker line in the body, see
// [identityMarker]) and the issues carry a management label. The escalator
// rediscovers its own open issues by listing them through the sink and parsing
// the marker, so reconciliation is correct across separate process runs. The
// in-memory map is only a convenience cache layered on top of that durable
// truth.
//
// # The reconcile core is pure
//
// The diff that decides open/update/close — [Reconcile] — is a pure function of
// (current findings, currently-tracked issues). It performs no I/O, holds no
// clock, and never talks to GitHub or a cluster. That is deliberate: the
// interesting logic (dedup, clearance, severity-change handling, multi-cluster
// isolation) is exactly the part that must be exhaustively unit-tested, so it is
// kept free of network and side effects. The side-effecting shell — listing,
// creating, commenting, closing — lives behind the [IssueSink] interface and is
// driven by [Escalator], which simply executes whatever plan Reconcile returns.
//
// # READ-ONLY toward Kubernetes is sacred
//
// This package touches an external comms system (GitHub) and NOTHING ELSE. It
// never reads from or writes to a Kubernetes cluster — it only consumes
// [detect.Finding] values that an upstream read-only layer already produced.
// The one-directional data flow (cluster -> health -> detect -> escalate ->
// GitHub) keeps MaKlaude's safety boundary obvious: no code path here can
// mutate a cluster.
package escalate

import (
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// ActionKind enumerates the three things reconciliation can decide to do to the
// external comms trail. The set is intentionally closed and minimal: every
// difference between "the problems that exist now" and "the issues that exist
// now" reduces to opening, updating, or closing exactly one tracked issue.
type ActionKind int

const (
	// ActionOpen creates a brand-new issue for a finding whose identity is not
	// yet tracked by any open issue.
	ActionOpen ActionKind = iota

	// ActionUpdate refreshes an already-open issue for a recurring finding: its
	// body is rewritten to the latest state and a recurrence comment is added.
	// This is what makes recurrence an update instead of a duplicate.
	ActionUpdate

	// ActionClose closes a tracked issue whose identity is no longer present in
	// the current findings — the problem has cleared — leaving a closing comment
	// so the trail remains auditable.
	ActionClose
)

// String renders the action kind as a stable lowercase token, used in logs and
// test fixtures.
func (k ActionKind) String() string {
	switch k {
	case ActionOpen:
		return "open"
	case ActionUpdate:
		return "update"
	case ActionClose:
		return "close"
	default:
		return "action(?)"
	}
}

// TrackedIssue is the escalator's view of one open, MaKlaude-managed issue in
// the external comms system. It is a plain value carrying just enough to drive
// reconciliation: the identity it represents (parsed from the issue's hidden
// marker) and the sink-specific handle used to comment on or close it.
//
// It deliberately does NOT carry the issue's full body or history — reconcile
// decides what to do from the identity alone, and the body it would write is
// derived freshly from the current finding.
type TrackedIssue struct {
	// Identity is the problem this issue represents, recovered from the issue's
	// embedded marker. It is the join key against current findings.
	Identity detect.Identity

	// Ref is the sink-specific reference to the live issue (for example its
	// number). The reconcile core treats it as an opaque value; only the sink
	// interprets it.
	Ref IssueRef

	// ThreadTS is the Slack thread timestamp recovered from the issue's hidden
	// thread marker, or empty when none is present (Slack unconfigured, or the
	// issue predates the marker). It is what gives the comms layer durable,
	// cross-restart chat-thread continuity: the escalator threads it back into the
	// [Action] so a recurrence/clearance replies into the original thread. The pure
	// reconcile layer treats it as opaque and merely carries it through.
	ThreadTS string
}

// IssueRef is an opaque, sink-specific handle to an existing issue (for example
// a GitHub issue number). The pure reconcile layer never inspects it; it merely
// threads it from a [TrackedIssue] into the [Action] so the [Escalator] can
// hand it back to the [IssueSink].
type IssueRef string

// Action is one unit of work the [Escalator] should perform against the sink to
// bring the comms trail in line with the current findings. It is produced only
// by [Reconcile] and consumed only by the escalator; it is a pure description,
// not the side effect itself.
type Action struct {
	// Kind is what to do (open, update, or close).
	Kind ActionKind

	// Identity is the problem this action concerns. Present for every kind so
	// callers and tests can key on it uniformly.
	Identity detect.Identity

	// Finding is the current finding driving an open or update. It is the zero
	// value for [ActionClose], which is driven by absence rather than a finding.
	Finding detect.Finding

	// Ref is the existing issue to act on for update/close. It is empty for
	// [ActionOpen], where no issue exists yet.
	Ref IssueRef

	// ThreadTS is the Slack thread timestamp recovered from the tracked issue, set
	// for [ActionUpdate] and [ActionClose] so the escalator can reply into the
	// original chat thread. It is empty for [ActionOpen] (no thread exists yet) and
	// whenever the tracked issue carried no thread marker.
	ThreadTS string
}
