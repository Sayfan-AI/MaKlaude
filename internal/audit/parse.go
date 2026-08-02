package audit

// This file is the reverse of the String methods on [Phase] and [Authority].
//
// # Why a trail needs readers, not only writers
//
// Both enums document their tokens as this package's contract: they appear in stored
// records, in rendered artifacts, and in anything that reconstructs an action's
// lifecycle later. A contract that can be written and not read is half a contract —
// and the missing half is the one a REBUILD needs. [trust.EntryFrom] already reads a
// lifecycle back out of records, and the trust ledger is explicitly a cache of the
// artifacts rather than the authority over them, so something has to be able to turn
// "verified" back into [PhaseVerified] without guessing.
//
// Before this, the only reader was a private token switch inside [trust], which meant
// each new consumer would write its own. Two independent parsers for one enum are two
// chances to disagree about what a stored record says, and the disagreement would be
// invisible: both would compile, both would pass their own tests, and they would
// differ only on the tokens nobody thought to write a case for.
//
// # Absence is reported, never defaulted
//
// Each parser returns ok=false for a token it does not recognize rather than falling
// back to the zero value. The zero values here are [PhaseUnknown] and
// [AuthorityUnattributed], which are both safe to render — but they are not safe to
// SILENTLY substitute for a real value, because a lifecycle rebuilt with the phases
// quietly zeroed reads as "nothing happened" rather than as "this could not be read".
// A caller that wants the safe default can take it explicitly; one that is rebuilding
// history must be able to fail loudly, which is what [trust.Open] already does with a
// ledger line it cannot parse.

// ParsePhase turns a stable phase token back into a [Phase]. ok is false for any
// token this build does not recognize, including the empty string.
//
// It accepts exactly what [Phase.String] emits, [PhaseUnknown]'s "unknown" included:
// a record that genuinely recorded no phase round-trips as one that recorded no
// phase, which is different from a token that could not be read at all.
func ParsePhase(token string) (Phase, bool) {
	switch token {
	case "unknown":
		return PhaseUnknown, true
	case "proposed":
		return PhaseProposed, true
	case "approved":
		return PhaseApproved, true
	case "executed":
		return PhaseExecuted, true
	case "verified":
		return PhaseVerified, true
	case "failed":
		return PhaseFailed, true
	case "rolled-back":
		return PhaseRolledBack, true
	default:
		return PhaseUnknown, false
	}
}

// ParseAuthority turns a stable authority token back into an [Authority]. ok is false
// for any token this build does not recognize.
//
// The unrecognized case matters more here than it does for a phase. An authority that
// failed to parse and defaulted to [AuthorityHuman] would manufacture human review
// where none happened, which this package's doc calls the one thing a trail must never
// do; defaulting to [AuthorityPolicy] would do the opposite and erase a real person's
// approval. Neither is a defensible guess, so there is no guess.
func ParseAuthority(token string) (Authority, bool) {
	switch token {
	case "unattributed":
		return AuthorityUnattributed, true
	case "human":
		return AuthorityHuman, true
	case "policy":
		return AuthorityPolicy, true
	default:
		return AuthorityUnattributed, false
	}
}
