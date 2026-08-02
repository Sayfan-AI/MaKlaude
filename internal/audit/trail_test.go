package audit

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// TestTrail_AppendsInOrderAndSequencesFromOne pins the two properties the word
// "ordered" has to mean: sequence numbers start at 1 and increase by exactly one,
// and Records returns them in that order.
//
// Sequence is asserted separately from position because they are separate claims. A
// trail that returned records in the right order with duplicated or zeroed Seq
// values would still break every consumer that quotes a record by number, and one
// that numbered correctly but returned them shuffled would break the lifecycle
// rendering.
func TestTrail_AppendsInOrderAndSequencesFromOne(t *testing.T) {
	trail := NewTrail()
	ctx := context.Background()

	phases := []Phase{PhaseApproved, PhaseExecuted, PhaseVerified}
	for _, phase := range phases {
		rec := fullRecord()
		rec.Phase = phase
		if _, err := trail.Append(ctx, rec); err != nil {
			t.Fatalf("appending %s: %v", phase, err)
		}
	}

	got := trail.Records()
	if len(got) != len(phases) {
		t.Fatalf("the trail holds %d records, want %d", len(got), len(phases))
	}
	for i, rec := range got {
		if rec.Seq != i+1 {
			t.Errorf("record %d has Seq %d, want %d", i, rec.Seq, i+1)
		}
		if rec.Phase != phases[i] {
			t.Errorf("record %d is phase %s, want %s", i, rec.Phase, phases[i])
		}
		if rec.RecordedAt.IsZero() {
			t.Errorf("record %d was stored with no recording time", i)
		}
	}
	if trail.Len() != len(phases) {
		t.Errorf("Len = %d, want %d", trail.Len(), len(phases))
	}
}

// TestTrail_AppendReturnsWhatWasStored proves the returned record is the sequenced,
// timestamped, redacted copy rather than an echo of the input.
//
// This is what makes the [Sink] contract safe to build on: the execution layer
// renders the comms artifact from Append's return value, so if that value were the
// caller's own unsanitized record, every secret-redaction guarantee in this package
// would be bypassed by the one code path that actually publishes anything.
func TestTrail_AppendReturnsWhatWasStored(t *testing.T) {
	trail := NewTrail()
	rec := fullRecord()
	rec.Outcome.Error = "admission denied: " + seededSecret

	stored, err := trail.Append(context.Background(), rec)
	if err != nil {
		t.Fatalf("appending: %v", err)
	}

	if stored.Seq != 1 {
		t.Errorf("the returned record has Seq %d, want 1", stored.Seq)
	}
	if stored.RecordedAt.IsZero() {
		t.Error("the returned record has no recording time")
	}
	if strings.Contains(stored.Outcome.Error, seededSecret) {
		t.Fatalf("Append returned an unredacted record; anything rendered from it would leak: %q", stored.Outcome.Error)
	}
	if held := trail.Records(); held[0].Outcome.Error != stored.Outcome.Error {
		t.Fatalf("the returned record (%q) differs from the stored one (%q)", stored.Outcome.Error, held[0].Outcome.Error)
	}
}

// TestTrail_StoresNothingSensitive is the trail-level statement of the no-secrets
// requirement: whatever a caller assembles, what the trail HOLDS is redacted.
//
// Redaction lives in Append rather than at the call site precisely so no actor can
// forget it, and this asserts that placement rather than the regexes underneath it
// (which have their own tests).
func TestTrail_StoresNothingSensitive(t *testing.T) {
	trail := NewTrail()
	rec := fullRecord()
	rec.Detail = "operator note: " + seededSecret
	rec.Outcome.Detail = "the container logged " + seededSecret
	rec.PreState.Fields[0].Value = seededSecret

	if _, err := trail.Append(context.Background(), rec); err != nil {
		t.Fatalf("appending: %v", err)
	}

	held := trail.Records()[0]
	if leaked(held) {
		t.Fatalf("the trail stored a secret: %+v", held)
	}
	if !strings.Contains(Lifecycle(trail.Records()), "[REDACTED]") {
		t.Error("the rendered lifecycle does not show that material was removed")
	}
}

// TestTrail_HandsOutCopies is the append-only guarantee against its most likely
// breach: not a caller deleting a record, but a caller mutating one it was handed
// and silently rewriting history.
//
// Both directions are checked. The record the caller passed IN must not be
// reachable from the trail afterwards, and the record the trail hands OUT must not
// be a window onto its storage.
func TestTrail_HandsOutCopies(t *testing.T) {
	trail := NewTrail()
	rec := fullRecord()

	if _, err := trail.Append(context.Background(), rec); err != nil {
		t.Fatalf("appending: %v", err)
	}

	// Mutating the caller's own record must not reach the trail.
	rec.PreState.Fields[0].Value = "tampered-by-caller"
	// Mutating what the trail handed back must not reach it either.
	handed := trail.Records()
	handed[0].Detail = "tampered-after-read"
	handed[0].PreState.Fields[1].Value = "tampered-after-read"

	held := trail.Records()[0]
	if held.Detail == "tampered-after-read" {
		t.Error("mutating a returned record changed what the trail holds")
	}
	if held.PreState.Fields[0].Value != "false" || held.PreState.Fields[1].Value != "false" {
		t.Errorf("the trail's pre-state was reachable through a caller's slice: %+v", held.PreState.Fields)
	}
}

// TestTrail_ForSelectsOneActionsLifecycle proves records are grouped by proposal
// identity, which is what lets one trail carry several concurrent actions and still
// render each one's story separately.
func TestTrail_ForSelectsOneActionsLifecycle(t *testing.T) {
	trail := NewTrail()
	ctx := context.Background()

	const other = remediate.ProposalIdentity("proposal|deletepod|prod|pod/shop/web-dead")
	cordon := fullRecord()

	interleaved := []Record{cordon, withIdentity(cordon, other), cordon, withIdentity(cordon, other)}
	for _, rec := range interleaved {
		if _, err := trail.Append(ctx, rec); err != nil {
			t.Fatalf("appending: %v", err)
		}
	}

	got := trail.For(cordon.Action.Identity)
	if len(got) != 2 {
		t.Fatalf("For returned %d records for the cordon, want 2", len(got))
	}
	if got[0].Seq != 1 || got[1].Seq != 3 {
		t.Fatalf("For returned sequences %d and %d, want 1 and 3 — the other action's records were mixed in",
			got[0].Seq, got[1].Seq)
	}
	if none := trail.For("proposal|nothing|prod|node/none"); len(none) != 0 {
		t.Fatalf("For returned %d records for an unknown identity, want none", len(none))
	}
}

// TestTrail_ConcurrentAppendsAreTotallyOrdered is why Seq exists at all.
//
// Wall-clock timestamps do not order concurrent writes: two goroutines can land in
// the same nanosecond, and a clock adjustment mid-run can order them backwards. The
// assertion is that the sequence numbers form exactly 1..N with no gaps and no
// duplicates, which is a claim a lock either satisfies or does not.
func TestTrail_ConcurrentAppendsAreTotallyOrdered(t *testing.T) {
	const writers = 32
	trail := NewTrail()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := trail.Append(ctx, fullRecord()); err != nil {
				t.Errorf("appending: %v", err)
			}
		}()
	}
	wg.Wait()

	got := trail.Records()
	if len(got) != writers {
		t.Fatalf("the trail holds %d records, want %d — an append was lost", len(got), writers)
	}
	seen := make(map[int]bool, writers)
	for i, rec := range got {
		if rec.Seq != i+1 {
			t.Fatalf("record at position %d has Seq %d; Records is not in sequence order", i, rec.Seq)
		}
		if seen[rec.Seq] {
			t.Fatalf("sequence %d was assigned twice", rec.Seq)
		}
		seen[rec.Seq] = true
	}
}

// TestTrail_ExposesNoWayToRewriteHistory is a structural assertion rather than a
// behavioural one: the append-only promise is kept by the type having no mutating
// method, so what is checked is that appending twice leaves the first record exactly
// as it was, byte for byte in every field a later append could plausibly touch.
func TestTrail_ExposesNoWayToRewriteHistory(t *testing.T) {
	trail := NewTrail()
	ctx := context.Background()

	first, err := trail.Append(ctx, fullRecord())
	if err != nil {
		t.Fatalf("appending: %v", err)
	}

	second := fullRecord()
	second.Phase = PhaseFailed
	second.Outcome = Outcome{Failure: "execute-failed", Error: "the API server said no"}
	if _, err := trail.Append(ctx, second); err != nil {
		t.Fatalf("appending: %v", err)
	}

	held := trail.Records()[0]
	if held.Phase != first.Phase || held.Outcome != first.Outcome || held.Seq != first.Seq {
		t.Fatalf("the first record changed after a later append:\n was: %+v\n now: %+v", first, held)
	}
}

// TestTrail_ZeroValueStillRecords guards a construction slip in the direction that
// matters. A trail built with a composite literal instead of [NewTrail] has a nil
// clock, and panicking there would turn a wiring mistake into a lost audit record
// mid-execution — the one moment this package must not be the thing that fails.
func TestTrail_ZeroValueStillRecords(t *testing.T) {
	var trail Trail
	stored, err := trail.Append(context.Background(), fullRecord())
	if err != nil {
		t.Fatalf("appending to a zero-value trail: %v", err)
	}
	if stored.Seq != 1 || stored.RecordedAt.IsZero() {
		t.Fatalf("a zero-value trail stored %+v, want a sequenced and timestamped record", stored)
	}
}

// TestTrail_WithClockIsDeterministicAndRefusesNil proves the test seam works and
// that passing nil leaves the working clock in place rather than disabling it.
func TestTrail_WithClockIsDeterministicAndRefusesNil(t *testing.T) {
	at := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	trail := NewTrail().WithClock(func() time.Time { return at })

	stored, err := trail.Append(context.Background(), fullRecord())
	if err != nil {
		t.Fatalf("appending: %v", err)
	}
	if !stored.RecordedAt.Equal(at) {
		t.Fatalf("RecordedAt = %s, want the injected %s", stored.RecordedAt, at)
	}

	trail.WithClock(nil)
	again, err := trail.Append(context.Background(), fullRecord())
	if err != nil {
		t.Fatalf("appending after WithClock(nil): %v", err)
	}
	if !again.RecordedAt.Equal(at) {
		t.Fatalf("WithClock(nil) replaced the clock; RecordedAt = %s, want %s", again.RecordedAt, at)
	}
}

// withIdentity returns a copy of a record belonging to a different action.
func withIdentity(rec Record, id remediate.ProposalIdentity) Record {
	rec.Action.Identity = id
	return rec
}
