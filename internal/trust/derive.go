package trust

import (
	"errors"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
)

// This file is the one place that knows how an [audit.Record] lifecycle becomes a
// [Entry]. Everything else in the package works on entries, so the ledger's
// arithmetic never has to know what a phase is or which convergence tokens exist.
//
// It reads the trail's STABLE TOKENS rather than importing [execute]'s enums. The
// audit package documents those tokens as the trail's contract and carries them as
// strings for exactly this reason: a consumer that reconstructs meaning from a
// stored record must be able to do it without linking the package that produced it,
// or the trail is not really a record. The tokens are pinned against their source
// enums by test, so the indirection cannot rot silently.

// ErrNotAnExecution reports that a lifecycle describes nothing the trust ledger
// should count: an approval with no attempt behind it yet, or a server-side preview.
//
// A preview is the case worth naming. It is a real request that the API server
// accepted and it changed nothing, so counting it would let a shape earn autonomy
// out of executions that never touched the cluster — trust assembled from
// rehearsals. It is a sentinel rather than a silent skip so a caller can tell "this
// lifecycle contributes nothing" apart from "this lifecycle is malformed", which
// need different responses.
var ErrNotAnExecution = errors.New("trust: the lifecycle records no execution to learn from")

// Trail tokens this package branches on. They are the [execute] enums' rendered
// form, restated here as constants so every comparison in the file reads against a
// name and a test can pin the whole set against the enums in one place.
const (
	tokenConverged    = "converged"
	tokenTimedOut     = "timed-out"
	tokenUnobservable = "unobservable"
	tokenUnobserved   = "unobserved"
	tokenNoFailure    = "none"
)

// rollbackKeySuffix distinguishes the entry for undoing an action from the entry for
// the action itself. Both concern the same proposal identity and both belong in the
// history — the execution may well have converged before someone decided to reverse
// it — so they cannot share a key, or the idempotent [Ledger.Record] would drop the
// rollback as a duplicate of the thing it undid.
const rollbackKeySuffix = "#rollback"

// recurrenceKeySuffix distinguishes the entry recording that a converged execution did
// not hold from the entry recording the execution itself. Same reasoning as
// [rollbackKeySuffix]: both concern one proposal identity, the history needs both, and
// sharing a key would make the idempotent [Ledger.Record] drop the regression as a
// duplicate of the very convergence it contradicts. See [Ledger.NoteRecurrence].
const recurrenceKeySuffix = "#recurrence"

// EntryFrom projects one action's audit lifecycle onto a single ledger entry.
//
// The input is every record for one proposal identity, in trail order — exactly what
// [audit.Trail.For] returns and exactly what a reader of one approval artifact can
// reconstruct. One lifecycle yields at most one entry: the audit trail records four
// or five things about a single execution, and the promotion arithmetic counts
// executions, not records.
//
// It returns [ErrNotAnExecution] for a lifecycle with nothing to learn from.
func EntryFrom(recs []audit.Record) (Entry, error) {
	if len(recs) == 0 {
		return Entry{}, ErrNotAnExecution
	}

	// Every record in a lifecycle restates the same action and the same approver, by
	// deliberate redundancy in the audit package, so any record answers the identity
	// questions. The last is used because it is the one whose Change carries the
	// finishing instant.
	last := recs[len(recs)-1]
	head := recs[0]

	outcome, rollback := classify(recs)
	if outcome == OutcomeUnrecorded {
		return Entry{}, ErrNotAnExecution
	}

	identity := head.Action.Identity
	if identity == "" {
		return Entry{}, fmt.Errorf("trust: the lifecycle carries no proposal identity")
	}

	at := finishedAt(recs)
	if at.IsZero() {
		return Entry{}, fmt.Errorf("trust: lifecycle %s carries no usable instant", identity)
	}

	// The key discriminates one EXECUTION of a fix from the next. See [Entry.Key] for
	// what the identity-only key was quietly doing.
	//
	// Two discriminators, because neither is sufficient alone. The finish instant
	// separates repeated runs, but only as finely as the clock behind it — an injected
	// or coarse clock can stamp two genuine executions identically, and the failure mode
	// is the collapse this is fixing, silently back again. The approval artifact is
	// unique per approval and so separates them regardless of the clock, but a
	// policy-authorized execution has none. Together they cover both, and both are
	// recoverable from the artifact, so the same lifecycle read twice still produces the
	// same key: [Ledger.Record] stays idempotent and a rebuild still reproduces the live
	// history exactly.
	key := string(identity) + "@" + at.UTC().Format(time.RFC3339Nano)
	if ref := last.Approver.Ref; ref != "" {
		key += "@" + ref
	}
	if rollback {
		key += rollbackKeySuffix
	}

	e := Entry{
		Key:      key,
		Identity: identity,
		Shape: autonomy.Shape{
			Cluster:   head.Action.Cluster,
			Operation: head.Action.Operation,
		},
		// Read off the record rather than recomputed from it. The fingerprint has to be
		// the one that was current when the action ran, and recomputing would produce
		// this build's answer under this build's [remediate.PlannerVersion] — which is
		// the one thing the version exists to distinguish. An older record carries no
		// fingerprint and gets the empty one, which promotes nothing; see
		// [Entry.Fingerprint].
		Fingerprint: head.Action.Fingerprint,
		Authority:   last.Approver.Authority,
		Outcome:     outcome,
		At:          at,
		Ref:         last.Approver.Ref,
	}
	if err := validate(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// classify reduces a lifecycle to one outcome, and reports whether the lifecycle
// describes a rollback rather than an original action.
//
// The precedence is worst-first and it is the safety argument: a lifecycle that both
// converged and was later rolled back is recorded as rolled back, and one that
// converged and then failed to write its record is recorded as failed. Reading the
// happy record and stopping would let an execution that ended badly contribute to
// promotion, which is the one direction this function must never get wrong.
func classify(recs []audit.Record) (outcome Outcome, rollback bool) {
	outcome = OutcomeUnrecorded

	// worseThan ranks outcomes so the loop can keep the worst it has seen without
	// depending on the order records appear in.
	// [OutcomeRegressed] is ranked but never produced here: a regression is not
	// visible in one action's lifecycle, only in the recurrence of the fault a LATER
	// cycle diagnoses, so it arrives through [Ledger.NoteRecurrence] instead. It is in
	// the table anyway so that adding a lifecycle path that can detect one does not
	// silently rank it at zero and lose it to any other outcome.
	rank := map[Outcome]int{
		OutcomeUnrecorded:   0,
		OutcomeConverged:    1,
		OutcomeInconclusive: 2,
		OutcomeDriftAborted: 3,
		OutcomeFailed:       4,
		OutcomeRolledBack:   5,
		OutcomeRegressed:    6,
	}
	keep := func(candidate Outcome) {
		if rank[candidate] > rank[outcome] {
			outcome = candidate
		}
	}

	for _, rec := range recs {
		if rec.Rollback.Attempted {
			rollback = true
		}

		switch rec.Phase {
		case audit.PhaseRolledBack:
			keep(OutcomeRolledBack)

		case audit.PhaseFailed:
			if rec.Outcome.CleanAbort {
				keep(OutcomeDriftAborted)
				continue
			}
			keep(OutcomeFailed)

		case audit.PhaseVerified:
			// A verified record on a preview cannot happen — the executor returns before
			// observing — but a record whose change was a dry run must not be read as
			// evidence about the cluster even if one appears.
			if rec.Change.DryRun {
				continue
			}
			switch rec.Outcome.Convergence {
			case tokenConverged:
				keep(OutcomeConverged)
			case tokenTimedOut, tokenUnobservable:
				keep(OutcomeInconclusive)
			}
		}
	}
	return outcome, rollback
}

// finishedAt picks the instant the execution ended, preferring the recorded event
// time over the time any record happened to be written.
//
// The order is deliberate. [audit.Change.FinishedAt] is when the attempt actually
// ended, which is what the ledger's ordering means; [audit.Record.RecordedAt] is
// when the trail wrote the entry, which the audit package is at pains to say is a
// different thing. The fallback exists because a lifecycle reconstructed from a
// rendered artifact may have lost the finer instants, and an entry that can be
// ordered approximately is worth more than one that is dropped — but it is a
// fallback, so the two are never mixed within one entry.
func finishedAt(recs []audit.Record) time.Time {
	var latest time.Time
	for _, rec := range recs {
		if t := rec.Change.FinishedAt; !t.IsZero() && t.After(latest) {
			latest = t
		}
	}
	if !latest.IsZero() {
		return latest
	}
	for _, rec := range recs {
		if t := rec.RecordedAt; !t.IsZero() && t.After(latest) {
			latest = t
		}
	}
	return latest
}

// RecordLifecycle projects a lifecycle and records it, skipping the ones that
// describe no execution.
//
// It is the convenience the execution layer will call once T4 gives it a call site:
// one function, so no caller has to remember that [ErrNotAnExecution] is normal and
// must not be surfaced as a failure. Every other error is real and is returned.
func (l *Ledger) RecordLifecycle(recs []audit.Record) error {
	e, err := EntryFrom(recs)
	if errors.Is(err, ErrNotAnExecution) {
		return nil
	}
	if err != nil {
		return err
	}
	return l.Record(e)
}
