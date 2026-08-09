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
// # The promotion rule, and the one place this reading is stricter than the words
//
// The parameters were approved on the milestone plan and are not changed here:
//
//   - Promotion needs 3 human-approved executions of the shape that converged, with
//     zero failures or rollbacks among the last 10 recorded executions of that shape.
//   - Demotion is immediate on a single failure, rollback, or drift-abort.
//   - The shape is (cluster, operation) — see [autonomy.Shape] for why not per-object.
//
// This package reads "the last 10 executions" as the WINDOW the whole rule is
// evaluated over, so the 3 converged executions must themselves be inside it. The
// looser reading — 3 converged executions found anywhere in history, plus a clean
// last 10 — was rejected because of what it does with an outcome that is neither a
// success nor a failure. A [OutcomeInconclusive] execution does not demote (the
// approved parameters list what demotes, and this is not on it), so under the loose
// reading a shape that converged three times last year and has timed out on every
// attempt since would stay trusted forever. Under the window reading those timeouts
// push the converged runs out of the last 10 and trust lapses on its own. Both
// readings honor the sign-off; this one fails closed, so it is the one implemented.
//
// # Autonomy does not compound
//
// Only an execution a HUMAN approved can promote. An auto-applied execution that
// converged perfectly is not evidence for further autonomy: it neither promotes nor
// erodes, and it does not occupy a window slot at all. [Entry.Counts] is that rule,
// and it is consulted inside [Ledger.Record] and [Ledger.Rebuild] so the live append
// path and a rebuild from the artifacts cannot disagree about what the window holds.
// The decision is recorded on issue #166: flushing the human approvals that earned
// trust out of the window, so that a shape working perfectly revokes its own autonomy,
// would make a person re-approve a fix that has never once failed — the kind of
// nagging that gets a safety feature switched off.
//
// The asymmetry is not needed to stop trust being self-reinforcing, because an
// auto-approval IS a human approval. It is taken earlier and at a higher level of
// abstraction — over a policy rather than over one action — but the chain of authority
// still terminates at a person. What the asymmetry preserves is what the window
// measures: how much a human has sanctioned, not how much MaKlaude has done.
//
// The exposure that buys is written down rather than left implicit: with successes
// excluded from the window, no amount of clean unattended operation expires trust on
// its own. Once earned, it lapses only when an execution demotes the shape — a
// failure, rollback, or drift-abort — or when inconclusive outcomes crowd the
// approvals out of the window. A shape passing permanently out of human oversight is a
// real danger; a counting window was the wrong remedy for it, and condition-based
// invalidation — trust as a cached judgment that stays valid until something
// invalidates it — is the right one. That model is issue #167, and it is where the
// expiry question gets settled.
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
		OutcomeFailed, OutcomeDriftAborted, OutcomeRolledBack,
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
	// Key identifies the execution this entry describes. Recording the same key twice
	// is a no-op, which is what makes the live append path and a full [Ledger.Rebuild]
	// from the same artifacts produce the same ledger.
	//
	// It is the proposal identity, suffixed for a rollback, because undoing an action
	// is a second thing that happened to the same proposal and both belong in the
	// history. See [EntryFrom].
	Key string

	// Shape is the (cluster, operation) pair trust is earned at.
	Shape autonomy.Shape

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
