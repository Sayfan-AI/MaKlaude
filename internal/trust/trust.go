// Package trust is the "earned" half of earned autonomy: given a recorded history
// of what MaKlaude has actually done on a cluster, it answers whether one
// [autonomy.Shape] has demonstrated enough of a track record to run unattended.
//
// It is the [autonomy.TrustOracle] that package declares and deliberately does not
// implement. The split is stated there and worth restating from this side: deciding
// whether a rule PERMITS an action is pure arithmetic over one proposal, while
// deciding whether a shape has EARNED it is arithmetic over a durable, restart-
// surviving history. Keeping the second one out of [autonomy.Decide] is what lets
// that function be a total pure function with no clock and no filesystem.
//
// # Trust is derived, never declared
//
// Nothing here accepts an operator's assertion that a shape is trustworthy. There is
// no seed, no bootstrap list, and no "start trusted" flag, because the human who
// signed off Milestone 5 declined one in those words: a config file that grants
// autonomy without evidence is the blank cheque [approve.AutoApproveEnv] already
// covers, and it is covered honestly there. A second one wearing the word "earned"
// would be strictly worse than the first, because it would look like a track record.
//
// The consequence is that on a fresh install NOTHING is trusted and every proposal
// gates. That is the correct day-one behavior, not a gap to close.
//
// # Trust is a cached judgment that stays valid until something invalidates it
//
// This is the model issue #167 settled, and it replaced a counting window. The window
// said trust was evaluated over a shape's last 10 recorded executions: promoting runs
// had to be inside it, so a well-behaved fix re-earned its standing on a schedule
// whether or not anything about it had changed. That is not a safety property, it is
// a timer, and it was wrong in both directions at once — it expired approvals that
// were still perfectly good, and it preserved them across changes that made them
// meaningless. The counter was not measuring the thing that matters.
//
// Two conditions end trust, and nothing else does:
//
//  1. THE FIX CHANGED. The approved thing and the proposed thing are no longer the
//     same thing, so the cached approval does not cover it. See
//     [remediate.Proposal.Fingerprint] for what "the same thing" means and which
//     differences are deliberately not counted.
//  2. THE FIX STOPPED WORKING. A failure, rollback, or drift-abort demotes the shape
//     outright, and so does a converged execution that turns out not to have held —
//     see [OutcomeRegressed] and [Ledger.NoteRecurrence].
//
// What does NOT end trust is a fix doing exactly what a person sanctioned,
// successfully, several times. An auto-approval is a human approval taken earlier and
// at a higher level of abstraction; the evidence chain terminates at a person either
// way, so nothing about repetition erodes the sanction.
//
// # The two scopes, and why they are different
//
// The parameters approved on the milestone plan are unchanged in magnitude —
// [PromotionThreshold] is still 3, a single bad outcome still demotes — but they no
// longer apply to the same unit, and the asymmetry is the safety argument:
//
//   - PROMOTION IS SCOPED TO THE FIX. Three human-approved converged executions
//     carrying the same [remediate.Fingerprint] promote that fingerprint, at any
//     distance in the past. This is the half issue #167 was filed about: under
//     (cluster, operation) alone, three approved rollout-restarts on prod earned the
//     right to restart ANY deployment on prod indefinitely, including workloads that
//     did not exist when the approvals were given.
//   - DEMOTION IS SCOPED TO THE SHAPE. One demoting outcome anywhere in the last
//     [DemotionScope] recorded executions of the (cluster, operation) pair blocks every
//     fingerprint of that shape, not merely the one that went wrong.
//
// Narrowing both would have been the obvious symmetry and it is the wrong one. A
// restart that failed is evidence about restarting things on that cluster, and if
// demotion were fingerprint-scoped then any change to the fix — a bumped
// [remediate.PlannerVersion], a different target — would produce a fresh fingerprint
// with a clean record, which turns "the fix changed" from a reason to re-earn trust
// into a way to launder a failure. So the narrow scope earns and the broad scope
// blocks, and each direction is the one that fails closed.
//
// [DemotionScope] is the counting window's one surviving job and is kept deliberately
// rather than by omission; see the constant for why an unbounded version would make a
// single failure permanent.
//
// # The risk this model carries that the window did not, and what carries it
//
// The window was dumb, but its dumbness was a backstop: it forced a person back into
// the loop on a schedule regardless of whether the health signal was telling the
// truth. Removing it puts the whole safety burden on MaKlaude's own convergence
// check — and a convergence check is a bounded observation immediately after an
// action, which is structurally the same thing as the milestone-1 crashloop detector
// that read a pod one instant after a restart, saw it was not currently in
// CrashLoopBackOff, and concluded it was fine.
//
// So convergence is not the last word here. Recurrence is: if the same fault is
// diagnosed again on the same object within [RecurrenceHorizon] of an execution that
// reported convergence, that execution is recorded as [OutcomeRegressed] and the shape
// is demoted. A fix that has to be applied again is not a fix, and a shape whose fixes
// keep being reapplied is the last one that should be acting with nobody watching.
// That is the strength the window used to supply, sourced from evidence rather than
// from a schedule.
//
// # Autonomy does not compound
//
// Only an execution a HUMAN approved can promote. An auto-applied execution that
// converged perfectly is not evidence for further autonomy: it neither promotes nor
// erodes, and it is not recorded at all. [Entry.Counts] is that rule, and it is
// consulted inside [Ledger.Record] and [Ledger.Rebuild] so the live append path and a
// rebuild from the artifacts cannot disagree about what the history holds. The
// decision is recorded on issue #166.
//
// The asymmetry is not needed to stop trust being self-reinforcing, because an
// auto-approval IS a human approval. It is taken earlier and at a higher level of
// abstraction — over a policy rather than over one action — but the chain of authority
// still terminates at a person. What the asymmetry preserves is the meaning of the
// count: how much a human has sanctioned, not how much MaKlaude has done.
//
// Note what this exclusion does NOT cover, because the two halves are easy to
// conflate. An auto-applied execution that converged is not recorded; an auto-applied
// execution that FAILED, rolled back, drift-aborted, or regressed is recorded and
// demotes exactly like a human-approved one. Autonomy cannot earn itself more
// autonomy, and it is fully able to lose the autonomy it has.
//
// # The ledger is a cache; the approval artifacts are the authority
//
// The durable record of a human's approval is the GitHub artifact, not this file.
// [Ledger] is a local, append-only projection of those artifacts, kept so that a
// trust decision does not depend on an API call that can fail or rate-limit —
// failing closed on a rate limit would mean autonomy silently stops working, and
// failing open is unthinkable. [Ledger.Rebuild] exists so the projection can always
// be discarded and recomputed from the artifacts, and [Ledger.Record] refuses an
// entry that claims human approval without naming the artifact it came from, so a
// hand-edited file cannot mint approvals that no rebuild would reproduce.
package trust

import (
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Outcome is what one recorded execution did, reduced to the only distinctions the
// promotion arithmetic makes. It is deliberately coarser than
// [execute.Convergence] and [execute.FailureClass] together: those enumerate what
// happened for a human reading an incident, this one answers "did this build trust,
// destroy it, or neither".
//
// The zero value is [OutcomeUnrecorded] and it DESTROYS trust rather than being
// ignored. An entry whose outcome was never populated is an entry nobody can vouch
// for, and the fail-closed direction for an unvouched-for execution is to treat it
// as a bad one — the same direction every other unknown in this milestone takes.
type Outcome int

const (
	// OutcomeUnrecorded means the outcome was never set. It is the zero value, it
	// demotes, and it never appears on an entry this package derived — see
	// [EntryFrom], which refuses to build an entry it cannot classify rather than
	// storing one of these.
	OutcomeUnrecorded Outcome = iota

	// OutcomeConverged means the action ran and the bounded observation window saw
	// the cluster reach the intended state. It is the ONLY outcome that builds trust,
	// and only when a human approved it.
	OutcomeConverged

	// OutcomeInconclusive means the action ran and the window did not confirm the
	// effect: it timed out, or the cluster could not be read. The action did not
	// fail, so it does not demote; it did not demonstrably work, so it does not
	// promote. It occupies a slot in the window, which is the whole of its effect —
	// see the package doc on why that matters.
	OutcomeInconclusive

	// OutcomeFailed means the attempt terminated with a real failure: the API server
	// refused it, the kill switch stopped it, the authorization was bad, the record
	// could not be written. Demotes immediately.
	OutcomeFailed

	// OutcomeDriftAborted means the attempt was abandoned cleanly because the target
	// had moved since the proposal was reasoned about. Nothing was sent and nothing
	// is broken — [execute.Report.CleanAbort] calls this a healthy outcome, and for
	// the human-gated path it is.
	//
	// It demotes anyway, by explicit sign-off, and the reason is that the two paths
	// want opposite things from drift. A person who is shown a stale proposal
	// re-proposes and moves on. A shape whose targets keep moving under it is a shape
	// where the cluster is changing faster than MaKlaude reasons about it, and that
	// is precisely the condition under which acting with nobody watching is worst.
	OutcomeDriftAborted

	// OutcomeRolledBack means an action of this shape had to be undone. Demotes: a
	// remediation someone reversed is one that should not have run, whatever the
	// convergence window said at the time.
	OutcomeRolledBack

	// OutcomeRegressed means an execution that reported convergence did not actually
	// fix anything: the same fault was diagnosed again, on the same object, soon
	// enough afterwards that the fix cannot be said to have worked.
	//
	// It is the outcome issue #167 required and it exists because of what removing the
	// counting window gave up. The window was dumb, but its dumbness was a backstop: it
	// forced a shape back to a person on a schedule whether or not the health signal
	// was telling the truth. With expiry moved to invalidation, the entire safety
	// burden lands on the convergence check — and a convergence check is a bounded
	// observation immediately after an action, which is exactly the shape of the
	// milestone-1 crashloop bug, where reading a pod one instant after a restart and
	// finding it not currently in CrashLoopBackOff was mistaken for health.
	//
	// So convergence is no longer the last word on whether a fix worked. Recurrence is.
	// [Ledger.NoteRecurrence] is how one gets recorded, and it demotes: a fix that has
	// to be applied again is not a fix, and a shape whose fixes keep being reapplied is
	// the last shape that should be acting with nobody watching.
	OutcomeRegressed
)

// String renders the outcome as a stable lowercase token. The tokens are written to
// the ledger file and read back from it, so they are an on-disk format and must not
// change casually.
func (o Outcome) String() string {
	switch o {
	case OutcomeUnrecorded:
		return "unrecorded"
	case OutcomeConverged:
		return "converged"
	case OutcomeInconclusive:
		return "inconclusive"
	case OutcomeFailed:
		return "failed"
	case OutcomeDriftAborted:
		return "drift-aborted"
	case OutcomeRolledBack:
		return "rolled-back"
	case OutcomeRegressed:
		return "regressed"
	default:
		return "outcome(" + strconv.Itoa(int(o)) + ")"
	}
}

// Promotes reports whether this outcome can contribute to promotion. Only
// [OutcomeConverged] does, and even then only with human authority — see
// [Entry.Promotes], which is the check callers should use.
func (o Outcome) Promotes() bool { return o == OutcomeConverged }

// Demotes reports whether this outcome, anywhere in the evaluation window, blocks
// trust outright.
//
// It is written as an allowlist of the two non-demoting values rather than as a list
// of the demoting ones, so that an outcome value this build has never heard of — a
// newer MaKlaude's ledger file read by an older binary, a hand-edited number —
// demotes instead of being silently tolerated.
func (o Outcome) Demotes() bool {
	switch o {
	case OutcomeConverged, OutcomeInconclusive:
		return false
	default:
		return true
	}
}

// parseOutcome maps a token back to its [Outcome].
//
// An unrecognized token is an error rather than [OutcomeUnrecorded], even though
// that value would also demote. The two situations need different responses: a
// demoting entry means the shape had a bad execution, while an unreadable token
// means the ledger file is not what this build thinks it is, and quietly downgrading
// the second into the first would let a corrupt file present as an ordinary history.
// [Open] fails on it instead.
func parseOutcome(token string) (Outcome, bool) {
	for _, o := range []Outcome{
		OutcomeUnrecorded, OutcomeConverged, OutcomeInconclusive,
		OutcomeFailed, OutcomeDriftAborted, OutcomeRolledBack, OutcomeRegressed,
	} {
		if o.String() == token {
			return o, true
		}
	}
	return OutcomeUnrecorded, false
}

// parseAuthority maps a token back to its [audit.Authority], against that type's own
// String method so the two spellings cannot drift.
func parseAuthority(token string) (audit.Authority, bool) {
	for _, a := range []audit.Authority{
		audit.AuthorityUnattributed, audit.AuthorityHuman, audit.AuthorityPolicy,
	} {
		if a.String() == token {
			return a, true
		}
	}
	return audit.AuthorityUnattributed, false
}

// Entry is one recorded execution in the ledger: which shape it was, who authorized
// it, how it ended, when, and which approval artifact says so.
//
// It is a projection of an [audit.Record] lifecycle, not a second copy of it. The
// audit trail exists to let a human reconstruct one incident in full; this holds the
// five fields the promotion arithmetic reads and nothing else, because every extra
// field would be a second place for the cluster-derived free text that trail is
// careful to redact to end up.
type Entry struct {
	// Key identifies the EXECUTION this entry describes. Recording the same key twice
	// is a no-op, which is what makes the live append path and a full [Ledger.Rebuild]
	// from the same artifacts produce the same ledger.
	//
	// It is the proposal identity, the instant the attempt finished, and a suffix for a
	// rollback or a recurrence. See [EntryFrom] for the composition.
	//
	// The instant is load-bearing and was not always there. The key used to be the
	// proposal identity alone, which silently collapsed EVERY execution of one fix on
	// one object into a single entry: a deployment that crashlooped and was approved and
	// restarted five times recorded one approval, not five. Under the old
	// (cluster, operation) trust key that under-count was invisible, because the three
	// approvals promotion needs came from three different objects. Under issue #167 the
	// approvals must share a fingerprint, an object's fingerprint is stable across its
	// own repeated faults, and so the collapse would have made promotion unreachable
	// rather than merely inaccurate — trust that can never be earned, presenting as
	// autonomy that is configured and simply never fires.
	Key string

	// Identity is the proposal this execution was of. It is carried rather than parsed
	// back out of Key, because Key is a composed string and a cluster name is
	// operator-chosen text that can contain the separator. A key is for equality; a
	// field is for asking questions.
	Identity remediate.ProposalIdentity

	// Shape is the (cluster, operation) pair. It is the scope a demoting outcome
	// poisons and the scope a person revokes; it is NOT by itself the scope trust is
	// earned at — see Fingerprint below and the package doc.
	Shape autonomy.Shape

	// Fingerprint identifies the fix this execution was of. See
	// [remediate.Proposal.Fingerprint]. Only entries carrying the fingerprint of the
	// proposal in hand can promote it.
	//
	// It is EMPTY on two legitimate kinds of entry and both must be unable to promote
	// anything, which is what an empty fingerprint already means to [Ledger.Standing]:
	// an entry written by a build from before issue #167, and one rebuilt from an
	// artifact whose lifecycle marker predates the field. Neither is corrupt and
	// neither should be rejected — a shape's failures still count from them, which is
	// the half of the history that must survive a format change — so [validate] does
	// not require it. The consequence is stated rather than hidden: on upgrade, every
	// shape that had earned autonomy returns to the human gate and re-earns it against
	// a fingerprint. That is the correct reading of "we no longer know which fix those
	// approvals were for".
	Fingerprint remediate.Fingerprint

	// Authority is the kind of authority the execution ran under. Only
	// [audit.AuthorityHuman] can build trust; see the package doc on why an
	// auto-applied success is worth nothing here.
	Authority audit.Authority

	// Outcome is how it ended.
	Outcome Outcome

	// At is when the execution finished — event time, not the time this entry was
	// written. Ordering the ledger by it is what lets a rebuild from the approval
	// artifacts reproduce the same window as the live append path; see [Ledger.Trust].
	At time.Time

	// Ref is the approval artifact the authorization lives on. It is REQUIRED on a
	// human-approved entry: the artifact is the authority and this file is a cache of
	// it, so an entry claiming a human approval it cannot point at is the hand-edited
	// blank cheque this package exists to not be.
	Ref string
}

// Promotes reports whether this entry counts toward the 3 needed for promotion: a
// converged execution that an actual person approved.
func (e Entry) Promotes() bool {
	return e.Outcome.Promotes() && e.Authority.HumanReviewed()
}

// Demotes reports whether this entry blocks trust for its shape while it remains in
// the window.
func (e Entry) Demotes() bool { return e.Outcome.Demotes() }

// Counts reports whether this entry belongs in the evaluation window at all. It is
// the window-membership rule, in exactly one place: [Ledger.Record] and
// [Ledger.Rebuild] both consult it, so the live append path and a rebuild from the
// artifacts cannot disagree about what the window holds.
//
// Exactly one thing is excluded: a policy-authorized execution that converged — the
// auto-applied success. It cannot promote ([Entry.Promotes] requires a person) and,
// per the decision on issue #166, it must not erode either; see the package doc.
//
// Everything else counts, and the shape of the condition is what makes the unknowns
// fall on the fail-closed side: an outcome this build has never heard of is not
// [OutcomeConverged], so it counts (and demotes); an entry whose authority was never
// attributed counts; a policy-authorized failure, rollback, drift-abort, or
// inconclusive counts. The excluded case is named exactly, and nothing else is.
func (e Entry) Counts() bool {
	return !(e.Authority == audit.AuthorityPolicy && e.Outcome == OutcomeConverged)
}

// before reports whether e sorts earlier than other in the ledger's total order:
// event time first, then key.
//
// The key tiebreak is not cosmetic. Two executions can carry the same finish
// instant — a coarse clock, a restored backup, two records derived in the same
// nanosecond — and an order that left them unspecified would make the last-10 window
// depend on map or slice happenstance, which is exactly the non-determinism a
// decision to mutate a cluster unattended must not have.
func (e Entry) before(other Entry) bool {
	if !e.At.Equal(other.At) {
		return e.At.Before(other.At)
	}
	return e.Key < other.Key
}
