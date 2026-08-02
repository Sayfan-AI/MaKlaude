// Package disclose is the oversight surface for actions MaKlaude took with nobody
// watching.
//
// Every other write path in this system ends at a human: [approve] puts a proposal to
// a person and waits, [escalate] tells a person something is wrong. This one has no
// person in it. A rule permitted the action, a recorded history earned it, a ceiling
// admitted it, and it happened. Nobody approved, which means the record is not a
// supporting artifact — it IS the entire oversight surface, and it has to be louder
// than the gated path rather than quieter.
//
// # Louder, concretely
//
// Four choices follow from that and each is a deliberate cost:
//
//   - ONE ARTIFACT PER ACTION, never batched. A digest of "MaKlaude auto-applied 4
//     things" is cheaper to read and is exactly the shape in which the fourth one goes
//     unnoticed. The noise is the oversight; a person who finds the volume annoying has
//     been given the information they need to narrow the rules, which is the outcome
//     this system wants.
//   - THE ARTIFACT IS OPENED BEFORE THE ACTION RUNS. An action that starts and never
//     reports back — a crashed process, an evicted runner, a hung API call — leaves an
//     open artifact with no outcome on it, which is a signal. Opening it afterwards
//     would make that case leave nothing at all, and "nothing at all" is
//     indistinguishable from "the system was idle".
//   - THE ARTIFACT STAYS OPEN. Closing it is a person's acknowledgement, not
//     MaKlaude's. An auto-applied action nobody has looked at is a real state and it
//     should be visible as one.
//   - IT IS NEVER RENDERED AS HUMAN-APPROVED. [audit.Approver] already carries the
//     authority as a field precisely so no renderer has to infer it, and every heading
//     here asks before it names anybody.
//
// # Its own label, its own query
//
// [ManagedLabel] is distinct from [escalate.ManagedLabel] and [approve.ManagedLabel]
// for the reason approve's own doc gives: three trails sharing one label would list
// each other's issues on every pass, surviving only because each parser skips bodies
// without its own marker — a coincidence of implementation rather than a boundary. A
// distinct label makes the trails disjoint at the QUERY. It also means an operator can
// subscribe to unattended actions specifically, which is the one feed a person who has
// granted autonomy most wants and least wants buried.
//
// # Revocation
//
// The single documented signal is [RevokedLabel] on any disclosure artifact: it revokes
// autonomy for that action's SHAPE — its (cluster, operation) pair, the granularity
// trust is earned at — and it takes effect on the next cycle, because the cycle reads
// the open artifacts before it decides anything. It is a label rather than a config
// change or a CLI invocation because the person who needs it is already looking at the
// artifact when they decide they want it, and a revocation that requires them to go
// somewhere else is one they perform late.
//
// Revocation here is an OVERRIDE, not a demotion: it does not rewrite the trust ledger,
// and lifting it is removing the label or closing the artifact. That split is on
// purpose — the ledger records what happened, and a person's decision to stop trusting
// a shape is not something that happened to a cluster.
//
// # This package renders and reports; it does not decide
//
// Nothing here consults a rule, admits an action, or executes one. It is handed a
// decision that was already made and an outcome that already happened, and its whole
// job is to make both legible and durable. The one exception is deliberate and narrow:
// [Trail] satisfies [execute.Recorder], because the execution layer must write its
// outcome SOMEWHERE and for an unattended action there is no approval artifact to write
// to. That is the same role [approve.Gatekeeper] plays on the gated path, pointed at
// this trail instead.
package disclose

import (
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Labels applied to, and read from, a disclosure artifact.
const (
	// ManagedLabel marks an issue as a MaKlaude disclosure of an unattended action. It
	// is the coarse query filter; the authoritative key is still the embedded marker.
	// See the package doc on why it is distinct from the other two trails' labels.
	ManagedLabel = "maklaude-autonomous"

	// AppliedLabel records that a real mutation landed for this artifact.
	//
	// It is applied by [Trail.RecordExecution], which the execution layer calls the
	// instant the write returns and BEFORE the observation window — so an artifact
	// without it describes an action that did not change the cluster, whether because it
	// aborted, because it was a rehearsal, or because the process died mid-flight. That
	// last case is the one worth having a label for: it is the only evidence that would
	// otherwise not exist.
	AppliedLabel = "maklaude-applied"

	// RevokedLabel is the revocation signal. See the package doc.
	RevokedLabel = "autonomy:revoked"

	// NeedsHumanLabel marks a disclosure that needs someone, matching the label the
	// other two trails use so an operator has one thing to watch. It is applied when a
	// failed unattended action produces a [budget.Consequence] asking for escalation —
	// nobody was watching when it ran, so nobody learns unless it is pushed.
	NeedsHumanLabel = "needs:human"
)

// Ref is an opaque reference to one disclosure artifact — an issue number for the
// GitHub sink. Callers treat it as a value; only the sink interprets it.
type Ref string

// Action is everything known about one unattended action BEFORE it runs: what MaKlaude
// is about to do, which rule permitted it, what history earned it, and under which
// ceiling it was admitted.
//
// It is a value rather than a set of arguments because the same four facts are needed
// by the title, the body, the marker and the chat message, and threading them
// separately is how one of them ends up rendered in three places and updated in two.
type Action struct {
	// Proposal is the action itself, carried whole so the artifact can restate the
	// intent, the expected effect and the evidence without a lookup.
	Proposal remediate.Proposal

	// Verdict is the policy decision that permitted it. Its Rule and Evidence are the
	// two facts that replace a human's name on this artifact.
	Verdict autonomy.Verdict

	// Grant is the blast-radius admission. It is recorded even though an admitted grant
	// carries no detail, because "which ceiling let this through" is part of the answer
	// to "why did this happen" and the cluster/target echo makes the artifact
	// self-describing.
	Grant budget.Grant

	// Mode is the kill-switch posture the action ran under, as a stable token. A
	// disclosure under "dry-run" describes a rehearsal that changed nothing, and saying
	// so at the top is what stops it being read as a change.
	Mode string

	// At is when the action was admitted and this artifact opened (UTC).
	At time.Time
}

// Shape is the (cluster, operation) pair this action's autonomy is earned — and
// revoked — at.
func (a Action) Shape() autonomy.Shape {
	return autonomy.Shape{Cluster: a.Proposal.Cluster, Operation: a.Proposal.Operation}
}

// Valid reports whether this action carries the minimum a disclosure needs: an
// identified proposal, a verdict that actually auto-applies, and an admitted grant.
//
// It is checked before an artifact is opened rather than trusted, because the artifact
// is the record that says an unattended mutation was permitted, and one asserting a
// permission that was never granted is worse than a missing record.
func (a Action) Valid() bool {
	return a.Proposal.Identity != "" &&
		a.Verdict.AutoApplies() &&
		a.Verdict.Rule != "" &&
		strings.TrimSpace(a.Verdict.Evidence) != "" &&
		a.Grant.Admitted()
}

// Outcome is everything known about one unattended action AFTER it ran: what the
// execution layer reported, the audit lifecycle it produced, and what the blast-radius
// layer decided must follow.
//
// Report and Records overlap and both are kept. The report is the shape a renderer
// reads (it has the pre-state, the rollback plan, the convergence detail); the records
// are the shape a REBUILD reads, and they are what the machine-readable marker is
// projected from. Deriving the marker from the report instead would mean this package
// owning a second copy of the report-to-record projection that [execute] already owns,
// and two projections of one lifecycle is how a rebuilt ledger comes to disagree with
// the live one.
type Outcome struct {
	// Report is the execution layer's account of the attempt.
	Report execute.Report

	// Records is the audit lifecycle the attempt produced, in trail order, as returned
	// by the sink that stored them — so the values here are the REDACTED ones, which is
	// what may be written to a world-readable artifact.
	Records []audit.Record

	// Consequence is what the blast-radius layer decided must follow. Its zero value is
	// what follows a success: nothing.
	Consequence budget.Consequence

	// Rollback is the account of the reversal, when the consequence asked for one and
	// one was possible. RolledBack reports that a reversal was attempted at all, so an
	// absent report is not read as a rollback that quietly did nothing.
	RolledBack bool
	Rollback   execute.RollbackReport

	// RollbackSkipped explains, in one clause, why a requested rollback was not
	// attempted — the operation has no inverse, nothing landed to undo. It is empty when
	// no rollback was asked for or when one ran.
	RollbackSkipped string

	// DemotionErr and EscalationErr record a follow-up that could not be carried out.
	// They are reported on the artifact rather than swallowed: a demotion that failed
	// silently leaves a shape trusted after an unattended failure, which is the one
	// direction this system must not fail in.
	DemotionErr   string
	EscalationErr string

	// At is when the outcome was recorded (UTC).
	At time.Time
}

// Converged reports whether the action did what it was supposed to do: a real mutation
// landed and the bounded observation window saw the expected state.
//
// Written as a conjunction over the two facts rather than as a check on the convergence
// verdict alone, because [execute.ConvergenceUnobserved] is what a preview reports and
// a rehearsal must never read as a success.
func (o Outcome) Converged() bool {
	return o.Report.Executed && o.Report.Convergence == execute.ConvergenceConverged
}
