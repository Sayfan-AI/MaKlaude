package audit

import (
	"context"
	"sync"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Sink is the narrow boundary an actor writes audit records through. One method,
// because an actor must be able to append and must be able to do nothing else: no
// reading back, no amending, no deleting. "Append-only" is a property of the
// interface, not a convention the implementation is trusted to keep.
//
// Append RETURNS the stored record, and that return value is load-bearing rather
// than a convenience. The stored copy is the sequenced, timestamped, redacted one,
// and it is the only version a caller may render for a human. A caller that renders
// what it passed IN would be rendering unsanitized text — the exact leak this
// package exists to prevent — so the interface is shaped to make the safe thing the
// one that is in front of you.
type Sink interface {
	// Append stores one record and returns it as stored: sequenced, timestamped, and
	// redacted. An error means the record was NOT stored.
	Append(ctx context.Context, rec Record) (Record, error)
}

// Trail is an in-memory, append-only, ordered audit trail. It is the reference
// [Sink]: it never mutates a stored record, exposes no way to remove one, and hands
// out copies so a reader cannot reach in and change what it holds.
//
// # It is not the durable trail, and that is deliberate
//
// The durable audit trail is the comms artifact — the approval issue, where every
// phase is rendered as a comment that outlives the process by design. This type is
// the in-process ordered record that the artifact is rendered FROM, and the thing a
// test can assert against without a network. A process restart loses the Trail and
// loses nothing that matters, because everything it held was already written to the
// artifact as it happened.
//
// Making it durable — a file, a database, a log sink — is a matter of writing
// another [Sink], which is why the execution layer depends on the interface and not
// on this type.
//
// A Trail is safe for concurrent use.
type Trail struct {
	// now supplies RecordedAt, replaceable in tests so a trail's timestamps can be
	// made deterministic without sleeping.
	now func() time.Time

	mu      sync.Mutex
	records []Record
}

// NewTrail returns an empty trail using the wall clock.
func NewTrail() *Trail {
	return &Trail{now: func() time.Time { return time.Now().UTC() }}
}

// WithClock replaces the trail's clock. It returns the receiver so it can be chained
// onto construction, matching [approve.Gatekeeper.WithClock].
func (t *Trail) WithClock(now func() time.Time) *Trail {
	if now != nil {
		t.now = now
	}
	return t
}

// Append stores one record and returns it as stored.
//
// Three things happen here and all three are the point:
//
//  1. The record is REDACTED. Doing it here rather than at the call site means no
//     actor can forget: whatever a caller assembles, what the trail holds has been
//     through [redact.String].
//  2. It is SEQUENCED, under the same lock that appends it, so [Record.Seq] is a
//     total order over everything this trail has ever stored — including records
//     written concurrently by different goroutines.
//  3. It is TIMESTAMPED with the recording instant, which the record's own event
//     times are deliberately kept separate from (see the package doc).
//
// It never fails. The signature returns an error because [Sink] implementations that
// cross a process boundary can, and a caller written against the interface must
// handle it; this one has nothing that can go wrong.
func (t *Trail) Append(_ context.Context, rec Record) (Record, error) {
	stored := rec.redacted().clone()

	t.mu.Lock()
	defer t.mu.Unlock()

	stored.Seq = len(t.records) + 1
	stored.RecordedAt = t.clock()
	t.records = append(t.records, stored)
	return stored.clone(), nil
}

// Records returns every record in append order. The returned slice and its records
// are copies: mutating them does not affect the trail.
func (t *Trail) Records() []Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	return copyAll(t.records)
}

// For returns the records concerning one proposal, in append order — the full
// lifecycle of a single action, which is what [Lifecycle] renders.
//
// It selects on the proposal identity rather than on the target, because identity is
// what stays stable while the object underneath ticks over: an action re-proposed
// against a bumped resourceVersion is the same action, and its records belong in one
// story.
func (t *Trail) For(id remediate.ProposalIdentity) []Record {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []Record
	for _, rec := range t.records {
		if rec.Action.Identity == id {
			out = append(out, rec.clone())
		}
	}
	return out
}

// Len returns how many records the trail holds.
func (t *Trail) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.records)
}

// clock reads the trail's clock, tolerating a zero-value Trail built by a caller who
// used a composite literal instead of [NewTrail]. A trail that panicked on a nil
// clock would turn a construction slip into a lost audit record, which is the wrong
// direction for this particular type.
func (t *Trail) clock() time.Time {
	if t.now == nil {
		return time.Now().UTC()
	}
	return t.now().UTC()
}

// copyAll deep-copies a slice of records.
func copyAll(recs []Record) []Record {
	out := make([]Record, 0, len(recs))
	for _, rec := range recs {
		out = append(out, rec.clone())
	}
	return out
}

// Ensure the reference implementation satisfies the interface at compile time.
var _ Sink = (*Trail)(nil)
