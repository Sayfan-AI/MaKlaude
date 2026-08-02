// Package budget bounds autonomy ACROSS proposals and ACROSS time: how many actions
// may run unattended in one pass, how soon the same object may be touched again, and
// when a run of failures takes a whole cluster back to fully gated.
//
// It is the layer [autonomy] deliberately cannot be. That package decides one proposal
// at a time, with no memory and no clock, which is what makes it a total pure function
// — and is also why [autonomy.DecisionAutoApply] means "this proposal is ELIGIBLE",
// never "go". Eligibility with no ceiling is how a single bad diagnosis becomes fifty
// restarts: every one of them individually permitted, individually trusted, and
// collectively an outage. This package is the ceiling.
//
// # The three bounds, and why they are three
//
//   - A CAP on how many actions one cluster may auto-apply in one pass. Bounds the
//     damage a single bad scan can do, whatever the scan concluded.
//   - A COOLDOWN per target. Bounds the damage a repeated bad scan can do to one
//     object: a deployment restarted at 09:00 is not restarted again at 09:05 because
//     the same fault is still visible.
//   - A BREAKER per cluster. Bounds the damage of being wrong in a way MaKlaude cannot
//     detect: consecutive auto-apply failures mean the model of the cluster is wrong,
//     and the correct response to "my reasoning is unreliable here" is to stop
//     reasoning unattended at all until a person looks.
//
// They are separate because they fail differently. A cap resets every pass, a cooldown
// expires on its own, and a breaker does NEITHER — it stays tripped until a human
// clears it ([Budget.Clear]). Collapsing any pair would give the wrong one of those
// three lifetimes to something.
//
// # Fail closed, and the shape that forces it
//
// The state behind all three lives in a file, and a file can be missing, truncated,
// hand-edited or written by a build this one cannot read. The requirement from the
// milestone plan is explicit: unreadable state degrades to FULLY GATED, never to
// unbounded.
//
// So [Open] never refuses to return a [Budget]. That is the opposite of [trust.Open],
// which fails loudly on a corrupt ledger, and the asymmetry is deliberate: dropping a
// trust ledger means nothing is trusted and everything gates, which is safe, while
// dropping a budget means nothing is BOUNDED, which is the exact failure this package
// exists to prevent. A caller that writes the ordinary `if err != nil { return }` must
// not thereby delete the ceiling. So a corrupt file yields a SEALED budget — one that
// admits nothing, for [ReasonStateUnreadable] — and the loudness moves to where it can
// be seen without being discarded: [Status.Sealed] is rendered unconditionally in the
// operator-facing state summary.
//
// # Admission is a reservation, not a question
//
// [Budget.Admit] CONSUMES budget: it counts against the pass cap and starts the
// target's cooldown, whether or not the action that follows succeeds. A caller that
// asks without acting has spent something.
//
// That direction is chosen rather than tolerated. The alternative — charge the budget
// only once an action lands — loses the ceiling in exactly the case it is needed most:
// an action that hangs, crashes the process, or fails in a way that never reports back
// is an action the next pass would be free to repeat immediately. Starting the cooldown
// at admission is what makes "never retry autonomously" a property of the state rather
// than of a caller remembering to record something.
//
// # This package records consequences; it does not carry them out
//
// [Budget.RecordOutcome] returns a [Consequence] — roll back, demote the shape, escalate
// — and performs none of it. Rolling back is [execute]'s, demotion is [trust]'s, and
// escalation is the comms trail's. Keeping the decision here and the effects there is
// what lets the whole failure policy be asserted in a unit test with no cluster, no
// GitHub, and no clock but an injected one.
package budget

import (
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Default bounds, approved on the Milestone 5 plan (#138) and restated on the task
// issue (#144). They are named constants rather than literals inside
// [DefaultLimits] so a reader grepping for the number lands on the sign-off.
const (
	// DefaultPerClusterPerPass is the most actions one cluster may auto-apply in one
	// pass over it.
	DefaultPerClusterPerPass = 2

	// DefaultCooldown is how long one target is off limits to autonomy after an
	// auto-apply is admitted for it.
	DefaultCooldown = 30 * time.Minute

	// DefaultFailureThreshold is how many CONSECUTIVE auto-apply failures on one
	// cluster trip that cluster's breaker.
	DefaultFailureThreshold = 2
)

// Limits are the three configured bounds. The zero value is INVALID rather than
// permissive — see [Limits.Validate], and note that an invalid ruleset gates
// everything in [autonomy] for the same reason.
type Limits struct {
	// PerClusterPerPass caps admissions per cluster per pass. Must be at least 1: a
	// zero would be a cap that permits nothing, which is a posture an operator gets by
	// not enabling autonomy at all, so reading it as a real value would hide a
	// half-written configuration behind a plausible-looking outcome.
	PerClusterPerPass int

	// Cooldown is the minimum interval between two admitted auto-applies against the
	// same target. Must be positive.
	//
	// It is a [time.Duration] rather than a count of seconds so that no call site has
	// to re-encode the unit, and so a misread cannot silently become milliseconds.
	Cooldown time.Duration

	// FailureThreshold is the number of consecutive auto-apply failures on a cluster
	// that trips its breaker. Must be at least 1.
	//
	// The count is CONSECUTIVE: one success resets it. A cluster where autonomy works
	// and occasionally does not is a different situation from one where it has stopped
	// working, and only the second is worth taking fully gated.
	FailureThreshold int
}

// DefaultLimits returns the approved defaults.
func DefaultLimits() Limits {
	return Limits{
		PerClusterPerPass: DefaultPerClusterPerPass,
		Cooldown:          DefaultCooldown,
		FailureThreshold:  DefaultFailureThreshold,
	}
}

// Validate reports whether these limits bound anything.
//
// Every field is rejected at zero, and the reason is the same one [autonomy.Rule]
// gives for refusing an empty selector: a forgotten field must not read as a value.
// Here the specific danger is a partially-populated struct — a caller that set the
// cooldown and forgot the cap — where two of the three bounds are real and the third
// silently is not.
func (l Limits) Validate() error {
	switch {
	case l.PerClusterPerPass < 1:
		return &LimitsError{Field: "PerClusterPerPass", Problem: "must be at least 1"}
	case l.Cooldown <= 0:
		return &LimitsError{Field: "Cooldown", Problem: "must be positive"}
	case l.FailureThreshold < 1:
		return &LimitsError{Field: "FailureThreshold", Problem: "must be at least 1"}
	}
	return nil
}

// LimitsError names the field that made a [Limits] unusable. It is a typed error
// rather than a formatted string so a configuration loader can report the exact knob
// an operator has to go and fix.
type LimitsError struct {
	Field   string
	Problem string
}

func (e *LimitsError) Error() string { return "budget: " + e.Field + " " + e.Problem }

// Reason is why one admission decision came out the way it did. It is a closed enum
// with stable lowercase tokens, matching [autonomy.Reason]'s discipline: the tokens
// reach the audit trail and the operator-facing summary, so they are contract.
//
// The zero value is [ReasonStateUnreadable] rather than [ReasonAdmitted]. A [Grant]
// that was never populated — a struct built by a test helper, a value that survived a
// refactor with a field dropped — therefore reads as "denied, state unknown" instead
// of as permission.
type Reason int

const (
	// ReasonStateUnreadable — the persisted state could not be read or parsed, so
	// nothing about the caps, cooldowns or breaker is known. Everything is denied. See
	// the package doc for why this is a sealed budget rather than a returned error.
	ReasonStateUnreadable Reason = iota

	// ReasonLimitsInvalid — the configured [Limits] do not validate, so no bound is
	// trustworthy. Denied, the same way [autonomy] gates an entire invalid ruleset
	// rather than honoring the well-formed parts of it.
	ReasonLimitsInvalid

	// ReasonClusterMismatch — the target names a different cluster than the admission
	// is being asked for. Multi-cluster isolation is a first-class property here as
	// everywhere else, and a disagreement means the caller is confused about which
	// cluster it is bounding.
	ReasonClusterMismatch

	// ReasonBreakerTripped — this cluster's breaker is open. Nothing auto-applies on it
	// until a human runs [Budget.Clear]. This outranks the cap and the cooldown because
	// it is the only one of the three that says something is WRONG rather than that
	// something is merely full.
	ReasonBreakerTripped

	// ReasonPassCapReached — this cluster has already had [Limits.PerClusterPerPass]
	// admissions in this pass. The next pass starts a fresh allowance.
	ReasonPassCapReached

	// ReasonTargetCoolingDown — this exact target was admitted less than
	// [Limits.Cooldown] ago. It is the bound that stops one object being acted on
	// repeatedly while the fault that provoked the action is still visible.
	ReasonTargetCoolingDown

	// ReasonNoPass — [Budget.Begin] has not been called, so there is no pass to count
	// against. Denied rather than defaulted, because a caller that never begins a pass
	// would otherwise get an unbounded sequence of admissions that each looked like the
	// first.
	ReasonNoPass

	// ReasonAdmitted — every bound holds. The one value that permits an unattended
	// action, and the only one for which [Grant.Admitted] is true.
	ReasonAdmitted
)

// String renders the reason as a stable lowercase token.
func (r Reason) String() string {
	switch r {
	case ReasonStateUnreadable:
		return "state-unreadable"
	case ReasonLimitsInvalid:
		return "limits-invalid"
	case ReasonClusterMismatch:
		return "cluster-mismatch"
	case ReasonBreakerTripped:
		return "breaker-tripped"
	case ReasonPassCapReached:
		return "pass-cap-reached"
	case ReasonTargetCoolingDown:
		return "target-cooling-down"
	case ReasonNoPass:
		return "no-pass"
	case ReasonAdmitted:
		return "admitted"
	default:
		return "reason(" + strconv.Itoa(int(r)) + ")"
	}
}

// Admits reports whether this reason permits an unattended action. Written as an
// equality against the single permitting value so that a reason added later denies
// until somebody says otherwise.
func (r Reason) Admits() bool { return r == ReasonAdmitted }

// Grant is the result of one admission decision.
//
// Detail carries the numbers behind a denial — the cap that was reached, the time the
// cooldown expires — because an operator asking "why did nothing run?" needs the bound
// AND its value, and a token alone gives only the first.
type Grant struct {
	// Reason is why. [Grant.Admitted] is derived from it so the two cannot disagree.
	Reason Reason

	// Cluster and Target name what was being asked about, echoed back so a grant is
	// self-describing in a report that lists several.
	Cluster string
	Target  string

	// Detail is a short human-facing clause explaining the denial, empty on an
	// admission. It never contains cluster-derived free text — only this package's own
	// numbers and timestamps — so it is safe in a world-readable artifact.
	Detail string
}

// Admitted reports whether the action may run unattended.
func (g Grant) Admitted() bool { return g.Reason.Admits() }

// String renders one grant as a stable line for a log, a report, or a test failure.
func (g Grant) String() string {
	s := g.Reason.String() + ": " + g.Cluster + " " + g.Target
	if g.Detail != "" {
		s += " (" + g.Detail + ")"
	}
	return s
}

// Outcome is what an admitted auto-apply did, reduced to the only distinction the
// breaker makes: did it work.
//
// It is coarser than [trust.Outcome] on purpose and must not be conflated with it.
// That type answers "did this build or destroy trust in the shape", over a window of
// ten executions, and treats a clean drift abort as demoting. This one answers "is
// autonomy on this cluster still working right now", and the zero value is the unsafe
// reading for the same reason: an outcome nobody recorded is one nobody can vouch for.
type Outcome int

const (
	// OutcomeUnrecorded is the zero value. It counts as a FAILURE — see the type doc.
	OutcomeUnrecorded Outcome = iota

	// OutcomeSucceeded means the action ran and converged. It resets the cluster's
	// consecutive-failure count to zero.
	OutcomeSucceeded

	// OutcomeFailed means the action did not do what it was supposed to: it errored,
	// was refused, or did not converge.
	OutcomeFailed
)

// String renders the outcome as a stable lowercase token.
func (o Outcome) String() string {
	switch o {
	case OutcomeUnrecorded:
		return "unrecorded"
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomeFailed:
		return "failed"
	default:
		return "outcome(" + strconv.Itoa(int(o)) + ")"
	}
}

// failed reports whether this outcome counts against the breaker. Written as an
// allowlist of the single safe value, so an outcome this build does not recognize
// counts as a failure rather than being ignored — the same direction
// [trust.Outcome.Demotes] takes.
func (o Outcome) failed() bool { return o != OutcomeSucceeded }

// Consequence is what must happen after an auto-applied action, as decided here and
// carried out elsewhere. See the package doc on why the two are separated.
//
// The zero value is what follows a success: nothing.
type Consequence struct {
	// RollBack asks the caller to undo the action if its operation is reversible.
	// This package does not know whether it is — [remediate.Reversibility] travels with
	// the proposal — so the instruction is conditional and the caller resolves it.
	RollBack bool

	// Demote asks the caller to record the failure against the shape's trust, which
	// re-gates it. It is set on every failure, independently of the breaker: one bad
	// auto-apply demotes the SHAPE even when the cluster stays closed-circuit.
	Demote bool

	// Escalate asks the caller to raise this to a human, `needs:human`. No person was
	// watching when the action ran, so nobody learns about the failure unless it is
	// pushed to them.
	Escalate bool

	// Tripped reports that this outcome took the cluster's breaker from closed to
	// open. It is true on the transition only, never on subsequent failures against an
	// already-open breaker, so a caller can announce the trip exactly once.
	Tripped bool

	// ConsecutiveFailures is the cluster's failure count after recording this outcome,
	// so an escalation can say "2 of 2" rather than "a failure happened".
	ConsecutiveFailures int
}

// Acted reports whether this consequence asks the caller to do anything. A success
// produces a zero [Consequence], and a caller can branch on this rather than testing
// four fields.
func (c Consequence) Acted() bool {
	return c.RollBack || c.Demote || c.Escalate || c.Tripped
}

// Breaker is one cluster's circuit-breaker state, as reported to an operator.
type Breaker struct {
	// Cluster is the cluster this breaker governs.
	Cluster string `json:"cluster"`

	// Tripped reports whether autonomy is currently blocked on this cluster.
	Tripped bool `json:"tripped"`

	// TrippedAt is when it opened (UTC), zero when it is closed.
	TrippedAt time.Time `json:"trippedAt,omitzero"`

	// ConsecutiveFailures is the current run of failures. It is reported even when the
	// breaker is closed, because "one failure away from tripping" is worth seeing
	// before the trip rather than after.
	ConsecutiveFailures int `json:"consecutiveFailures"`

	// Detail is a short clause naming what tripped it, empty when closed. Like
	// [Grant.Detail] it contains only this package's own numbers.
	Detail string `json:"detail,omitempty"`
}

// Suppression is one auto-apply this pass declined to admit: an action that was
// ELIGIBLE — a rule permitted it and the shape had earned it — and that a bound held
// back anyway.
//
// It exists as a recorded value rather than only a returned one because of what it is
// evidence of. A suppression is the blast-radius layer doing its job, and the whole
// class of failure the milestone's dev-system learnings keep draining is the one where
// a system correctly does nothing and says nothing. See [Status].
type Suppression struct {
	// Cluster and Target name what was held back.
	Cluster string `json:"cluster"`
	Target  string `json:"target"`

	// Reason is the bound that held it back, and Detail explains it in one clause.
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`

	// At is when the admission was refused (UTC).
	At time.Time `json:"at"`
}

// Status is the whole blast-radius posture at one instant, shaped for the
// operator-facing state summary.
//
// Both lists are reported EMPTY rather than omitted when there is nothing to say, and
// the summary prints both unconditionally. That is a requirement from the task issue
// rather than a rendering preference: a tripped breaker that nobody notices is
// indistinguishable from a system with nothing to do, and this repository has paid for
// that shape of invisibility several times over. Empty means all-clear, and the reader
// is told so in words.
type Status struct {
	// Sealed reports that the persisted state was unreadable, so every admission is
	// denied. This is the fail-closed posture and it must be visible: a sealed budget
	// blocks all autonomy, which LOOKS exactly like a quiet, healthy system.
	Sealed bool `json:"sealed"`

	// SealDetail explains the seal, empty when not sealed. It names the path and the
	// parse problem, never the file's contents.
	SealDetail string `json:"sealDetail,omitempty"`

	// Path is the state file backing this budget, empty for an in-memory one. Reported
	// so an escalation can tell an operator which file to look at.
	Path string `json:"path,omitempty"`

	// Limits are the bounds in force.
	Limits Limits `json:"limits"`

	// Breakers is one entry per cluster with recorded state, sorted by cluster name.
	// Closed breakers are included so a reader sees the failure count building.
	Breakers []Breaker `json:"breakers"`

	// Suppressions are the auto-applies held back during the current pass, in the
	// order they were refused.
	Suppressions []Suppression `json:"suppressions"`
}

// Tripped returns just the open breakers, which is what a summary's "tripped breakers"
// section lists. It allocates a fresh slice, so a caller cannot mutate the status.
func (s Status) Tripped() []Breaker {
	out := make([]Breaker, 0, len(s.Breakers))
	for _, b := range s.Breakers {
		if b.Tripped {
			out = append(out, b)
		}
	}
	return out
}

// targetKey is the cooldown's key for one object: the target's compact form, which is
// kind/namespace/name and deliberately excludes the resourceVersion.
//
// Excluding it is the whole point. A cooldown keyed on resourceVersion would expire
// the instant anything touched the object — including MaKlaude's own restart, which
// bumps it — so the bound would be defeated by exactly the action it is meant to
// throttle.
func targetKey(t remediate.Target) string { return t.String() }
