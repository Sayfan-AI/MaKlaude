package execute

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Convergence is the verdict of the bounded observation window: did the cluster
// actually reach the state the action was supposed to produce?
//
// The zero value is [ConvergenceUnobserved] — "nobody looked" — because every path
// that skips observation (a preview, an abort before the action ran) must report
// the absence rather than imply success by silence. A report that said "converged"
// by default would be the single most misleading thing this package could emit.
type Convergence int

const (
	// ConvergenceUnobserved means no observation was performed. It is the outcome
	// for a dry run (nothing changed, so there is nothing to converge to) and for
	// every abort that happens before the action is sent.
	ConvergenceUnobserved Convergence = iota

	// ConvergenceConverged means the expected post-state was observed within the
	// window. It is the only value that says the remediation worked.
	ConvergenceConverged

	// ConvergenceTimedOut means the window elapsed with the cluster read
	// successfully and the expected state never observed. The action DID run; it
	// just has not (yet) had the intended effect. It is reported and never retried:
	// re-sending a mutation because its effect was slow is how a system thrashes.
	ConvergenceTimedOut

	// ConvergenceUnobservable means the cluster could not be read during the window
	// at all, so nothing is known about the effect. It is deliberately distinct from
	// [ConvergenceTimedOut]: "we looked and it had not happened" and "we could not
	// look" call for different responses from a human.
	ConvergenceUnobservable
)

// String renders the verdict as a stable lowercase token, used in the audit trail,
// in escalations, and in test fixtures.
func (c Convergence) String() string {
	switch c {
	case ConvergenceUnobserved:
		return "unobserved"
	case ConvergenceConverged:
		return "converged"
	case ConvergenceTimedOut:
		return "timed-out"
	case ConvergenceUnobservable:
		return "unobservable"
	default:
		return "convergence(" + strconv.Itoa(int(c)) + ")"
	}
}

// FailureClass names how an execution attempt terminated. It is a closed enum
// rather than an error string because the two consumers of a [Report] both need to
// BRANCH on it: the audit trail records what happened, and the comms layer decides
// whether a human needs to be woken up. Neither should be parsing prose to do that.
//
// The zero value is [FailureNone], so a report that was never populated does not
// masquerade as a specific failure.
type FailureClass int

const (
	// FailureNone means the attempt ran to completion without a terminating error.
	// It says nothing about convergence — see [Convergence] for whether the action
	// actually worked.
	FailureNone FailureClass = iota

	// FailureNotAuthorized means the permission slip was missing, forged, or for a
	// different action than the one presented. Nothing was read and nothing was sent.
	FailureNotAuthorized

	// FailureClusterMismatch means the authorization, the proposal, and the write
	// client did not all name the same cluster. It is split out from
	// [FailureNotAuthorized] because it is the one failure whose consequence would be
	// acting on the wrong cluster — the worst thing this system could do — and a
	// class that appears in a report is a class someone can alert on.
	FailureClusterMismatch

	// FailureRefused means MaKlaude declined to run an action it was validly
	// authorized for: an irreversible one, or one the write path has no faithful
	// primitive for. It is a disagreement between the catalog and the executor, so it
	// wants a human, not a retry.
	FailureRefused

	// FailureKillSwitch means the global write kill switch did not permit the action
	// at the moment it would have run.
	FailureKillSwitch

	// FailureUnobservable means the cluster could not be read, so the preconditions
	// could not be evaluated. Nothing was sent: an unreadable cluster is never
	// treated as one where the preconditions hold.
	FailureUnobservable

	// FailureDrifted means at least one precondition no longer held. Nothing was
	// sent. This is a healthy outcome, not a malfunction — see [Report.CleanAbort].
	FailureDrifted

	// FailureConflict means the API server rejected the action because the target had
	// changed since the proposal was reasoned about. The request was sent and NOTHING
	// was applied; it is the same drift as [FailureDrifted], caught one layer later
	// and just as cleanly.
	FailureConflict

	// FailureExecute means the API server refused or failed the action for any other
	// reason (RBAC, admission, validation, connectivity). Whether the change landed
	// may be unknown, which is exactly why the runner stops rather than retrying.
	FailureExecute

	// FailureRecord means the action ran but its execution could not be recorded on
	// the approval trail. It is reported loudly because the consequence is specific:
	// the artifact is missing its executed label, so a later pass may ask a human to
	// approve something that already happened.
	FailureRecord

	// FailureNotRollbackable means a rollback was requested for something that cannot
	// be rolled back — an action that never ran, a preview, or an operation whose
	// effect has no inverse.
	FailureNotRollbackable
)

// String renders the class as a stable lowercase token.
func (c FailureClass) String() string {
	switch c {
	case FailureNone:
		return "none"
	case FailureNotAuthorized:
		return "not-authorized"
	case FailureClusterMismatch:
		return "cluster-mismatch"
	case FailureRefused:
		return "refused"
	case FailureKillSwitch:
		return "kill-switch"
	case FailureUnobservable:
		return "unobservable"
	case FailureDrifted:
		return "drifted"
	case FailureConflict:
		return "precondition-conflict"
	case FailureExecute:
		return "execute-failed"
	case FailureRecord:
		return "record-failed"
	case FailureNotRollbackable:
		return "not-rollbackable"
	default:
		return "failure(" + strconv.Itoa(int(c)) + ")"
	}
}

// CleanAbort reports whether this class is the EXPECTED outcome of a stale
// approval rather than something going wrong.
//
// Exactly two classes qualify, and they are the same event observed at two
// different moments: the runner noticed the drift before sending
// ([FailureDrifted]), or the API server noticed it on arrival
// ([FailureConflict]). In both, the cluster is untouched and the correct response
// is to let the next cycle re-propose against the state that actually exists — not
// to escalate, not to retry, and above all not to relax the precondition.
//
// Every other class means something a person should look at, which is why this is
// a method on the class rather than a judgment each consumer re-derives.
func (c FailureClass) CleanAbort() bool {
	return c == FailureDrifted || c == FailureConflict
}

// PreconditionResult is one precondition, re-evaluated against the cluster as it
// exists immediately before the action would run, with what was actually seen.
//
// The observation is carried alongside the verdict because "the action was
// aborted" is not, by itself, something a human can act on. "Pod prod/web-abc is
// no longer crashlooping" tells them the problem healed; "node-a is at
// resourceVersion 8891, expected 8712" tells them something else moved it. The
// artifact and the escalation both render this verbatim.
type PreconditionResult struct {
	// Kind is the check that was performed.
	Kind remediate.PreconditionKind

	// Expect is the value the proposal recorded as the expected one, copied verbatim
	// from the precondition.
	Expect string

	// Description is the proposal's own plain-language statement of the condition —
	// the same sentence the approver read.
	Description string

	// Held reports whether the condition still holds. Every precondition of a
	// proposal is evaluated, so a false here does not mean the others were skipped.
	Held bool

	// Observed states what was actually seen, in plain language, whether the check
	// passed or failed.
	Observed string
}

// PreStateField is one recorded fact about the target as it was immediately before
// the action. Fields are plain name/value strings, kept in a slice rather than a
// map so a report has a stable, deterministic rendering and serializes without
// map-ordering noise.
type PreStateField struct {
	Name  string
	Value string
}

// PreState is what the target looked like at the instant the action was authorized
// to proceed — the record that makes a rollback possible and an audit trail
// meaningful.
//
// It is captured from THE SAME snapshot the preconditions were judged against, not
// from a second read. That is what makes the two provably consistent: the state
// this records is the state that was checked, so a rollback restores the world the
// approver was reasoning about rather than whatever a later read happened to see.
//
// A target whose kind has no capture rule is refused rather than acted on: MaKlaude
// does not mutate an object it cannot describe the prior state of.
type PreState struct {
	// Captured reports whether a pre-state was recorded at all. The zero value is
	// false, so an unpopulated PreState cannot be mistaken for an empty object.
	Captured bool

	// Kind is the target's kind, so a consumer can interpret Fields without
	// re-deriving it.
	Kind string

	// ResourceVersion is the target's resourceVersion in the pre-action snapshot.
	ResourceVersion string

	// ObservedAt is the collection time of the snapshot this was captured from.
	ObservedAt time.Time

	// Fields are the kind-specific facts, in a fixed order per kind.
	Fields []PreStateField
}

// Field returns the recorded value of a named field, and whether it was present.
func (p PreState) Field(name string) (string, bool) {
	for _, f := range p.Fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

// RollbackKind classifies what undoing an action would take. It exists so
// "reversible" is not a single bit: the catalog contains an action that needs no
// undo, an action that CANNOT be undone but repairs itself, and an action MaKlaude
// can genuinely reverse — and treating those three as one would either overpromise
// or refuse things that are perfectly safe.
//
// The zero value is [RollbackUnclassified], which is refused. An operation added to
// the catalog without a classification therefore fails closed, exactly as it does
// at the approval gate (see approve's ReasonNoRollbackPlan).
type RollbackKind int

const (
	// RollbackUnclassified means no classification exists for the operation. It is
	// the zero value and is refused before anything is sent.
	RollbackUnclassified RollbackKind = iota

	// RollbackNotRequired means the action leaves nothing to undo. A rollout restart
	// is the case: the Deployment's spec is unchanged apart from a timestamp
	// annotation, and the pods it replaced are gone whether or not that annotation is
	// put back.
	RollbackNotRequired

	// RollbackImpossible means the effect cannot be undone by anything MaKlaude could
	// send. Deleting a pod is the case: the pod's name, identity, and logs are
	// unrecoverable, even though its controller restores the workload's function.
	RollbackImpossible

	// RollbackPerformable means MaKlaude can undo the action itself, from the
	// captured pre-state, through the same scoped write path — and [Runner.Rollback]
	// will do exactly that when asked.
	RollbackPerformable
)

// String renders the kind as a stable lowercase token.
func (k RollbackKind) String() string {
	switch k {
	case RollbackUnclassified:
		return "unclassified"
	case RollbackNotRequired:
		return "not-required"
	case RollbackImpossible:
		return "impossible"
	case RollbackPerformable:
		return "performable"
	default:
		return "rollback(" + strconv.Itoa(int(k)) + ")"
	}
}

// RollbackPlan is what this package would do to undo the action, and whether it can
// actually do it right now.
//
// Note and Kind are properties of the OPERATION and are known before anything runs;
// Available is a property of THIS attempt and is only true once a real mutation has
// landed with a captured pre-state. Keeping them separate is what lets an aborted
// attempt still report "this operation would have been reversible" without claiming
// there is something to reverse.
type RollbackPlan struct {
	// Kind is the classification. See [RollbackKind].
	Kind RollbackKind

	// Note states the plan in plain language, matching what the approval artifact
	// showed the human who approved the action.
	Note string

	// Available reports whether [Runner.Rollback] would attempt anything for this
	// report: the operation is [RollbackPerformable], the action really ran (not a
	// preview), and a pre-state was captured.
	Available bool
}

// Report is the complete, plain, serializable account of one execution attempt:
// what was authorized, what was checked, what was actually sent, what the cluster
// looked like before, whether the fix took, and how the attempt ended.
//
// It is a value with no behaviour beyond rendering, and it holds no client, no
// context, and no error interface — the terminating error is carried as a class
// plus its rendered text. That is deliberate: both consumers of this type outlive
// the call that produced it. The audit trail stores it, and the comms layer
// serializes it to a human somewhere else, possibly after a restart. Anything that
// could only be understood by dereferencing a live object would be lost by then.
//
// A report is returned on EVERY path, including the ones where nothing was sent, so
// a caller never has to distinguish "no report" from "nothing happened".
type Report struct {
	// Identity, Cluster, Operation, Target, and Reversibility restate the proposal
	// that was attempted, so the report stands alone without a lookup.
	Identity      remediate.ProposalIdentity
	Cluster       string
	Operation     remediate.Operation
	Target        remediate.Target
	Reversibility remediate.Reversibility

	// ProposedAt is when the proposal was computed. It is carried so the report — and
	// the audit record derived from it — can state the whole lifecycle from proposal
	// to outcome without a second lookup, including how long the action waited for a
	// human.
	ProposedAt time.Time

	// Approver is the human whose approval authorized this, and ApprovalRef is the
	// artifact that carried it. Both are empty when the authorization was not valid.
	Approver    string
	ApprovalRef string

	// Mode is the kill-switch posture the attempt ran under, read at execution time.
	Mode kube.ExecuteMode

	// StartedAt and FinishedAt bound the attempt, including any observation window.
	StartedAt  time.Time
	FinishedAt time.Time

	// Preconditions is every precondition of the proposal, re-evaluated against the
	// live cluster, in the proposal's own order. It is empty when the attempt was
	// refused before the cluster was read.
	Preconditions []PreconditionResult

	// PreState is what the target looked like immediately before the action.
	PreState PreState

	// Rollback describes what undoing this would take, and whether it can be done.
	Rollback RollbackPlan

	// Attempts is how many mutating requests were sent. It is 0 for every abort, 1
	// for the overwhelming majority of executions, and greater than 1 only for the
	// narrow retryable class in [Policy.MaxAttempts]. It is in the report because
	// "did this thrash?" must be answerable from the record rather than from a log.
	Attempts int

	// Executed reports that a real mutation landed. It is false for a preview: a dry
	// run changes nothing, and a report claiming otherwise would be the one lie that
	// could cause a second execution.
	Executed bool

	// DryRun reports that the attempt was a server-side preview.
	DryRun bool

	// Outcome is what the write path recorded about the request it sent — scope,
	// target, precondition, preview flag — or nil when nothing was sent.
	Outcome *kube.Outcome

	// Recorded reports whether the execution was written to the approval trail, which
	// is what durably prevents a second execution. It is false for a preview (nothing
	// ran, so nothing is recorded) and for a failed recording, which sets
	// [FailureRecord].
	Recorded bool

	// Convergence is the verdict of the bounded observation window, and
	// ConvergenceDetail states what was actually seen. ObservedFor is how long the
	// window was watched before the verdict was reached.
	Convergence       Convergence
	ConvergenceDetail string
	ObservedFor       time.Duration

	// Failure is how the attempt terminated, and Error is the rendered text of the
	// terminating error, empty when Failure is [FailureNone].
	Failure FailureClass
	Error   string
}

// CleanAbort reports whether this attempt ended in the expected way for a stale
// approval — nothing ran, nothing broke, re-propose. See [FailureClass.CleanAbort].
func (r Report) CleanAbort() bool { return r.Failure.CleanAbort() }

// DriftedPreconditions returns the preconditions that no longer held, in the
// proposal's own order. It is what a refusal notice renders, and it is empty for
// every outcome other than [FailureDrifted].
func (r Report) DriftedPreconditions() []PreconditionResult {
	var out []PreconditionResult
	for _, pc := range r.Preconditions {
		if !pc.Held {
			out = append(out, pc)
		}
	}
	return out
}

// String renders a compact, log-friendly line. It leads with whether anything
// actually changed, because that is the first thing a reader needs to know and the
// only thing that is irreversible if it is wrong.
func (r Report) String() string {
	state := "not executed"
	switch {
	case r.Executed:
		state = "executed"
	case r.DryRun:
		state = "previewed"
	}
	return fmt.Sprintf("execution: %s %s on cluster %s (%s) attempts=%d convergence=%s outcome=%s",
		r.Operation, r.Target.String(), r.Cluster, state, r.Attempts, r.Convergence, r.Failure)
}

// RollbackReport is the account of one attempt to undo an action, in the same plain
// serializable shape as [Report] and for the same reasons.
type RollbackReport struct {
	// Identity, Cluster, Operation, and Target restate the action being undone.
	Identity  remediate.ProposalIdentity
	Cluster   string
	Operation remediate.Operation
	Target    remediate.Target

	// Description is the plain-language inverse that was (or would have been)
	// performed.
	Description string

	// StartedAt and FinishedAt bound the attempt.
	StartedAt  time.Time
	FinishedAt time.Time

	// Performed reports that an inverse mutation actually landed.
	Performed bool

	// AlreadyAtPreState reports that no request was sent because the target was
	// already back at its pre-action state — someone else undid it, or it never
	// took. It is a success, not a failure: a rollback that fights a human is worse
	// than one that notices its work is done.
	AlreadyAtPreState bool

	// Attempts is how many mutating requests were sent.
	Attempts int

	// Outcome is what the write path recorded about the inverse request, or nil.
	Outcome *kube.Outcome

	// Convergence is the verdict of the bounded observation window over the inverse
	// action, with the same meaning as [Report.Convergence].
	Convergence       Convergence
	ConvergenceDetail string
	ObservedFor       time.Duration

	// Failure and Error report how the attempt terminated.
	Failure FailureClass
	Error   string
}

// String renders a compact, log-friendly line.
func (r RollbackReport) String() string {
	state := "not performed"
	switch {
	case r.AlreadyAtPreState:
		state = "already at pre-state"
	case r.Performed:
		state = "performed"
	}
	return fmt.Sprintf("rollback: %s %s on cluster %s (%s) attempts=%d convergence=%s outcome=%s",
		r.Operation, r.Target.String(), r.Cluster, state, r.Attempts, r.Convergence, r.Failure)
}

// fail stamps a terminating failure onto a report and returns it with its error, so
// every abort path in this package is one line and cannot forget to record the
// class, the text, or the finishing time.
func (r Report) fail(class FailureClass, err error) (Report, error) {
	r.Failure = class
	r.Error = err.Error()
	r.FinishedAt = time.Now().UTC()
	return r, err
}

// done stamps a report that reached the end without a terminating failure.
func (r Report) done() (Report, error) {
	r.FinishedAt = time.Now().UTC()
	return r, nil
}

// fail is the [RollbackReport] counterpart of [Report.fail].
func (r RollbackReport) fail(class FailureClass, err error) (RollbackReport, error) {
	r.Failure = class
	r.Error = err.Error()
	r.FinishedAt = time.Now().UTC()
	return r, err
}

// done is the [RollbackReport] counterpart of [Report.done].
func (r RollbackReport) done() (RollbackReport, error) {
	r.FinishedAt = time.Now().UTC()
	return r, nil
}
