package audit

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/redact"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Action restates the proposal a record concerns. It is copied into every record
// rather than referenced, so a single record answers "what action is this?" without
// a lookup — see the package doc on why the redundancy is deliberate.
type Action struct {
	// Identity is the proposal's stable key. It is the join column: every record for
	// one action carries the same value, and [Trail.For] selects on it.
	Identity remediate.ProposalIdentity

	// Cluster is the registered cluster the action concerns. It is carried explicitly
	// rather than derived from the target because a record naming the wrong cluster is
	// the single worst thing this trail could get wrong, and duplicating it means a
	// disagreement is visible instead of impossible to spot.
	Cluster string

	// Operation is the exact catalog operation.
	Operation remediate.Operation

	// Target is the single object acted on, including the resourceVersion the
	// proposal was computed against.
	Target remediate.Target

	// Reversibility is the proposal's own classification of how hard the action is to
	// undo. It is what set the approver's level of scrutiny, so the record keeps it.
	Reversibility remediate.Reversibility

	// Title is the short human-readable label for the action, taken from the proposal.
	Title string

	// ProposedAt is when the proposal was computed — the "proposed" stage of the
	// lifecycle, and deliberately not the time any record was written.
	ProposedAt time.Time
}

// Approver is who authorized the action, and on what kind of authority.
//
// The two timestamps answer different questions and both belong in an audit
// record: ApprovedAt is when a human consented, AuthorizedAt is when MaKlaude acted
// on that consent. The gap between them is however long the next reconciliation
// pass took, and an operator reconstructing an incident needs to be able to see it.
type Approver struct {
	// Authority is the KIND of authority. See [Authority] — in particular why this is
	// a field rather than something a reader infers from Identity.
	Authority Authority

	// Identity names the authorizer: a login when [Authority] is [AuthorityHuman], a
	// policy name when it is [AuthorityPolicy], and empty when unattributed. It is
	// never redacted; attribution is the point of the record.
	Identity string

	// ApprovedAt is when the decision was recorded by whoever made it.
	ApprovedAt time.Time

	// AuthorizedAt is when the approval gate issued the permission slip the action ran
	// under.
	AuthorizedAt time.Time

	// Ref is the approval artifact the decision lives on, so the record points back at
	// the conversation a human can read.
	Ref string
}

// Attributed reports whether anything authorized this at all.
func (a Approver) Attributed() bool { return a.Authority != AuthorityUnattributed && a.Identity != "" }

// String renders the approver for a human, stating the kind of authority
// explicitly. A policy-waived action reads as one; it is never dressed up as a
// person.
func (a Approver) String() string {
	switch {
	case !a.Attributed():
		return "unattributed (no valid authorization)"
	case a.Authority.HumanReviewed():
		return "@" + a.Identity + " (human approval)"
	case a.Authority == AuthorityPolicy:
		return a.Identity + " (policy waived approval — no human reviewed this)"
	default:
		return a.Identity + " (" + a.Authority.String() + ")"
	}
}

// Change is what was actually sent to the API server, as distinct from what was
// planned. Everything here is a fact about the request, not about its effect.
type Change struct {
	// Sent reports that at least one mutating request left the process. It is false
	// for every abort that happened before the write path was reached.
	Sent bool

	// Applied reports that the cluster really changed. It is false for a server-side
	// preview, which is a sent request that changed nothing — the distinction that
	// must never be recorded wrong, because a record claiming a preview was an
	// execution permanently blocks the real one.
	Applied bool

	// DryRun reports that the request was a server-side preview.
	DryRun bool

	// Mode is the kill-switch posture the attempt ran under, read at execution time.
	Mode string

	// Scope is the rendered write scope the request travelled through — the exact
	// method and path the transport admitted. It survives redaction intact; see
	// [Record.redacted] for why a value that looks like free text is treated as a
	// structured identifier.
	Scope string

	// ResourceVersion is the optimistic-concurrency token the request was conditioned
	// on. Because the API server enforced it, its presence means the object had not
	// changed since the state the approver was shown.
	ResourceVersion string

	// Attempts is how many mutating requests the action produced, including the first.
	// It is in the record because "did this thrash?" must be answerable from the
	// trail rather than from a log.
	Attempts int

	// RecordedOnTrail reports whether the execution was written back to the approval
	// artifact, which is what durably prevents a second execution. A false here on an
	// applied change is the specific situation where a later pass might re-ask a human
	// to approve something that already ran.
	RecordedOnTrail bool

	// StartedAt and FinishedAt bound the attempt, including any observation window.
	StartedAt  time.Time
	FinishedAt time.Time
}

// Duration is how long the attempt took end to end, observation window included.
func (c Change) Duration() time.Duration {
	if c.StartedAt.IsZero() || c.FinishedAt.IsZero() || c.FinishedAt.Before(c.StartedAt) {
		return 0
	}
	return c.FinishedAt.Sub(c.StartedAt)
}

// PreStateField is one recorded fact about the target as it was immediately before
// the action. Fields are name/value strings in a slice rather than a map so a
// record has a stable, deterministic rendering with no map-ordering noise.
type PreStateField struct {
	Name  string
	Value string
}

// PreState is what the target looked like at the instant the action was authorized
// to proceed. It is the half of the record that makes a rollback possible: without
// it, "put it back the way it was" has no referent.
type PreState struct {
	// Captured reports whether a pre-state was recorded at all, so an unpopulated
	// value cannot be mistaken for an object that had no state worth recording.
	Captured bool

	// Kind is the target's kind, so Fields can be interpreted without re-deriving it.
	Kind string

	// ResourceVersion is the target's resourceVersion in the pre-action read.
	ResourceVersion string

	// ObservedAt is when that read was taken.
	ObservedAt time.Time

	// Fields are the kind-specific facts, in a fixed order per kind.
	Fields []PreStateField
}

// Outcome is how the attempt ended: what the bounded observation window saw, and
// what (if anything) terminated the attempt.
type Outcome struct {
	// Convergence is the observation verdict as a stable token — "converged",
	// "timed-out", "unobservable", or "unobserved" when nothing was watched. It is
	// carried as a token rather than a typed enum because the execution layer's enum
	// already documents these strings as its audit-trail form, and duplicating the
	// type here would create two enumerations to keep in step.
	Convergence string

	// Detail states what was actually seen, in plain language. It is cluster-derived
	// free text and is redacted.
	Detail string

	// ObservedFor is how long the window was watched before the verdict was reached.
	ObservedFor time.Duration

	// Failure is the terminating failure class as a stable token, "none" when the
	// attempt completed.
	Failure string

	// CleanAbort reports that the failure was the EXPECTED outcome of a stale approval
	// — the target moved, nothing was sent, and the right response is to re-propose
	// rather than to escalate. It is carried as a fact rather than re-derived from the
	// token, so a reader (and an alerting rule) does not have to know which classes
	// qualify.
	CleanAbort bool

	// Error is the rendered text of the terminating error, empty when there was none.
	// It is cluster-derived free text and is redacted.
	Error string
}

// Failed reports whether the attempt terminated with a failure of any kind,
// including a clean abort.
func (o Outcome) Failed() bool { return o.Failure != "" && o.Failure != "none" }

// Rollback is what undoing the action would take, and what was actually done about
// it.
//
// Kind and Note are properties of the OPERATION and are known before anything runs.
// Available, Attempted, Performed, and AlreadyAtPreState are properties of what
// happened. Keeping them apart is what lets an aborted attempt still record "this
// operation would have been reversible" without implying there is something to
// reverse.
type Rollback struct {
	// Kind is the rollback classification as a stable token: "performable",
	// "not-required", "impossible", or "unclassified".
	Kind string

	// Note states the plan in plain language, matching what the approval artifact
	// showed the human who approved the action.
	Note string

	// Available reports that MaKlaude could perform this rollback on request: the
	// operation is reversible by it, the action really ran, and a pre-state was
	// captured.
	Available bool

	// Attempted reports that this record describes a ROLLBACK attempt rather than the
	// original action. It is what distinguishes a failed rollback from a failed
	// execution, both of which are [PhaseFailed].
	Attempted bool

	// Performed reports that an inverse mutation actually landed.
	Performed bool

	// AlreadyAtPreState reports that no request was sent because the target was
	// already back where it started. It is a success with nothing to do, not a
	// failure.
	AlreadyAtPreState bool

	// Description is the plain-language inverse that was (or would have been)
	// performed.
	Description string
}

// Record is one complete, ordered, append-only entry in the audit trail: one thing
// that happened to one action, with everything needed to understand it in
// isolation.
//
// It is a plain value with no behaviour beyond rendering, holding no client, no
// context, and no error interface, for the same reason [execute.Report] is: the
// consumers of a record outlive the call that produced it. Anything that could only
// be understood by dereferencing a live object would be meaningless by the time a
// human reads it.
type Record struct {
	// Seq is the record's position in the trail, assigned by [Trail.Append] starting
	// at 1. It is the authoritative ordering — see the package doc on why RecordedAt
	// is not. A record that has not been appended has Seq 0.
	Seq int

	// RecordedAt is when the record was written to the trail, NOT when the event it
	// describes happened. The event's own instants live in Action, Approver, and
	// Change.
	RecordedAt time.Time

	// Phase is where in the lifecycle this record sits. See [Phase].
	Phase Phase

	// Action is the proposal this record concerns.
	Action Action

	// Approver is who authorized it, and on what authority.
	Approver Approver

	// Change is what was sent. It is zero for a record describing something that never
	// reached the write path.
	Change Change

	// PreState is what the target looked like immediately before the action.
	PreState PreState

	// Outcome is how the attempt ended.
	Outcome Outcome

	// Rollback is the reversal story: what undoing this would take and what was done.
	Rollback Rollback

	// Detail is an optional extra sentence for a human, used where the structured
	// fields do not say everything. It is treated as free text and is redacted.
	Detail string
}

// String renders a compact, log-friendly line. It leads with the sequence number and
// the phase, because in a trail the two questions are always "where am I" and "what
// happened here".
func (r Record) String() string {
	return fmt.Sprintf("audit #%d %s: %s %s on cluster %s by %s (%s)",
		r.Seq, r.Phase, r.Action.Operation, r.Action.Target.String(), r.Action.Cluster,
		r.Approver.String(), r.summary())
}

// summary renders the phase-specific one-liner: what this particular record says
// happened. It is the "What" column of the rendered lifecycle and the tail of
// [Record.String], written once so the two cannot disagree.
func (r Record) summary() string {
	switch r.Phase {
	case PhaseProposed:
		return fmt.Sprintf("proposed at %s (%s)", stamp(r.Action.ProposedAt), r.Action.Reversibility)

	case PhaseApproved:
		when := joinNonEmpty("; ",
			stampedPhrase("the decision was recorded", r.Approver.ApprovedAt),
			stampedPhrase("the gate honored it", r.Approver.AuthorizedAt))
		return joinNonEmpty(" — ", "authorized: "+r.Approver.String(), when)

	case PhaseExecuted:
		return r.changeSummary()

	case PhaseVerified:
		detail := r.Outcome.Detail
		if detail == "" {
			detail = "no detail was recorded"
		}
		return fmt.Sprintf("%s after watching for %s — %s", r.Outcome.Convergence, r.Outcome.ObservedFor, detail)

	case PhaseFailed:
		what := "the action"
		if r.Rollback.Attempted {
			what = "the rollback"
		}
		lead := fmt.Sprintf("%s failed (%s)", what, r.Outcome.Failure)
		if r.Outcome.CleanAbort {
			lead = fmt.Sprintf("%s was abandoned cleanly (%s); nothing was applied", what, r.Outcome.Failure)
		}
		if r.Outcome.Error != "" {
			return lead + ": " + r.Outcome.Error
		}
		return lead

	case PhaseRolledBack:
		if r.Rollback.AlreadyAtPreState {
			return "nothing was sent: the target was already back at its pre-action state"
		}
		return fmt.Sprintf("inverse action performed (%s); %s", r.Rollback.Description, r.changeSummary())

	default:
		if r.Detail != "" {
			return r.Detail
		}
		return "no phase was recorded for this entry"
	}
}

// changeSummary renders what was sent, or says plainly that nothing was.
func (r Record) changeSummary() string {
	if !r.Change.Sent {
		return "no mutating request was sent"
	}
	what := "applied"
	if !r.Change.Applied {
		what = "previewed only (the cluster is unchanged)"
	}
	return fmt.Sprintf("%s via `%s`, conditioned on resourceVersion `%s`, after %d attempt(s)",
		what, r.Change.Scope, r.Change.ResourceVersion, r.Change.Attempts)
}

// redacted returns a copy of the record with every cluster-derived free-text field
// passed through [redact.String].
//
// # What is redacted, and what deliberately is not
//
// Redacted: the fields whose content originates in the cluster or in an API server
// response and is therefore outside MaKlaude's control — the convergence detail, the
// terminating error, the pre-state values, the rollback note and description, and
// the free-form Detail. Any of those can carry a credential that leaked into a
// container message, an annotation, or an error string, and all of them end up in a
// world-readable artifact.
//
// Not redacted: the structured identifiers — the proposal identity, cluster,
// operation, target, reversibility, title, the approver's identity and authority,
// resourceVersions, phase tokens, counts, and timestamps. These are what make the
// trail navigable, none of them is a plausible hiding place for a secret, and
// redacting them would actively destroy the linkage this package exists to provide:
// the high-entropy sweep would blank any object name over 24 characters, and a
// record whose target reads "[REDACTED]" is not an audit record.
//
// [Change.Scope] is in that second group, and it is the one entry whose membership
// is not obvious from its type, so it is argued rather than listed (#132). It looks
// like free text and reads like a URL, but it is assembled by
// [kube.WriteScope.String] from a mutating HTTP method and an API path built out of
// a fixed group/version/resource triple plus a namespace and object name the API
// server has already validated as DNS-1123 — and never a query string, which is
// where a token-bearing parameter would live. So there is nothing in it for the
// sweep to protect, while the sweep's own character class ([A-Za-z0-9+/_-]) matches
// `/`, `_` and `-`, which makes every real path one unbroken 24+ character run. The
// field that pins an action to a concrete request against a concrete resource
// collapsed to "PATCH /[REDACTED]" in every record until this exemption; a trail
// that can say a PATCH happened but not what it was addressed to answers half the
// question the trail exists for.
//
// The split is a judgment, so it is stated here rather than left to be inferred
// from the code, and it is asserted by test rather than by reading.
func (r Record) redacted() Record {
	r.Detail = redact.String(r.Detail)
	r.Outcome.Detail = redact.String(r.Outcome.Detail)
	r.Outcome.Error = redact.String(r.Outcome.Error)
	r.Rollback.Note = redact.String(r.Rollback.Note)
	r.Rollback.Description = redact.String(r.Rollback.Description)

	if len(r.PreState.Fields) > 0 {
		fields := make([]PreStateField, len(r.PreState.Fields))
		for i, f := range r.PreState.Fields {
			fields[i] = PreStateField{Name: f.Name, Value: redact.String(f.Value)}
		}
		r.PreState.Fields = fields
	}
	return r
}

// clone returns a copy whose slices are not shared with the original, so a record
// handed back by the trail cannot be used to mutate what the trail holds.
func (r Record) clone() Record {
	if len(r.PreState.Fields) > 0 {
		r.PreState.Fields = append([]PreStateField(nil), r.PreState.Fields...)
	}
	return r
}

// stamp renders a timestamp in the trail's fixed UTC form, or says plainly that it
// was never set. An unset time rendered as "0001-01-01T00:00:00Z" is a value a
// reader has to decode; "not recorded" is one they can read.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "not recorded"
	}
	return t.UTC().Format(time.RFC3339)
}

// stampedPhrase renders "<prefix> <timestamp>", or nothing at all when the instant
// was never set.
//
// It exists because [stamp]'s "not recorded" is right for a standalone field and
// wrong inside a sentence: "the decision was recorded not recorded" is what a
// policy-waived action produces, since nothing decided it and there is no decision
// time. Dropping the clause is the honest rendering — the approver line already says
// who authorized it and on what authority — and [joinNonEmpty] is what lets a caller
// assemble the sentence without emitting a stray separator where a clause used to be.
func stampedPhrase(prefix string, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return prefix + " " + t.UTC().Format(time.RFC3339)
}

// joinNonEmpty joins the non-empty parts with sep, so a renderer can assemble a
// sentence from optional pieces without emitting stray separators.
func joinNonEmpty(sep string, parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
