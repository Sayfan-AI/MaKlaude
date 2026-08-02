package audit

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The marker exists for exactly one consumer — a rebuild of the trust ledger from the
// artifacts — so the property that matters most is not tested here: it is that a ledger
// rebuilt through the marker agrees, entry for entry, with the ledger built by appending as
// the executions happened, across BOTH trails, which lives in internal/rebuild because only
// that package can see both. What is here is the encoding contract that property rests on,
// and the failure modes that would make the agreement silently untrue.

// markerTime is a fixed instant so a round trip's equality is about the encoding rather
// than about a clock.
var markerTime = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

// markedLifecycle builds a three-record lifecycle covering the fields the marker carries:
// an execution, its convergence verdict, and a rollback.
func markedLifecycle() []Record {
	action := Action{Identity: "p-1", Cluster: "prod", Operation: remediate.OpRolloutRestart}
	approver := Approver{Authority: AuthorityHuman, Ref: "artifact-7"}
	change := Change{Sent: true, Applied: true, FinishedAt: markerTime}

	recs := []Record{
		{RecordedAt: markerTime, Phase: PhaseExecuted, Action: action, Approver: approver, Change: change},
		{
			RecordedAt: markerTime, Phase: PhaseVerified, Action: action, Approver: approver, Change: change,
			Outcome: Outcome{Convergence: "timed-out", Failure: "none"},
		},
		{
			RecordedAt: markerTime, Phase: PhaseRolledBack, Action: action, Approver: approver, Change: change,
			Outcome:  Outcome{Failure: "execute-failed", CleanAbort: true},
			Rollback: Rollback{Attempted: true, Performed: true},
		},
	}
	for i := range recs {
		recs[i].Seq = i + 1
	}
	return recs
}

// TestLifecycleMarker_RoundTripsTheFieldsThatDecideATrustEntry is the base property. It
// asserts the projection, not the encoding: the fields listed are exactly the ones a trust
// entry is derived from, and one dropped in transit is history that changes meaning.
func TestLifecycleMarker_RoundTripsTheFieldsThatDecideATrustEntry(t *testing.T) {
	recs := markedLifecycle()

	marker, err := LifecycleMarker(recs)
	if err != nil {
		t.Fatalf("LifecycleMarker: %v", err)
	}
	got, err := ParseLifecycleMarker("some body\n" + marker + "\ntrailing text")
	if err != nil {
		t.Fatalf("ParseLifecycleMarker: %v", err)
	}
	if len(got) != len(recs) {
		t.Fatalf("round-tripped %d records, wrote %d", len(got), len(recs))
	}
	for i := range recs {
		want, have := recs[i], got[i]
		switch {
		case have.Phase != want.Phase:
			t.Errorf("record %d: Phase = %v, want %v", i, have.Phase, want.Phase)
		case have.Action.Identity != want.Action.Identity:
			t.Errorf("record %d: Identity = %q, want %q", i, have.Action.Identity, want.Action.Identity)
		case have.Action.Cluster != want.Action.Cluster:
			t.Errorf("record %d: Cluster = %q, want %q", i, have.Action.Cluster, want.Action.Cluster)
		case have.Action.Operation != want.Action.Operation:
			t.Errorf("record %d: Operation = %q, want %q", i, have.Action.Operation, want.Action.Operation)
		case have.Approver.Authority != want.Approver.Authority:
			t.Errorf("record %d: Authority = %v, want %v", i, have.Approver.Authority, want.Approver.Authority)
		case have.Approver.Ref != want.Approver.Ref:
			t.Errorf("record %d: Ref = %q, want %q", i, have.Approver.Ref, want.Approver.Ref)
		case have.Outcome.Convergence != want.Outcome.Convergence:
			t.Errorf("record %d: Convergence = %q, want %q", i, have.Outcome.Convergence, want.Outcome.Convergence)
		case have.Outcome.Failure != want.Outcome.Failure:
			t.Errorf("record %d: Failure = %q, want %q", i, have.Outcome.Failure, want.Outcome.Failure)
		case have.Outcome.CleanAbort != want.Outcome.CleanAbort:
			t.Errorf("record %d: CleanAbort = %v, want %v", i, have.Outcome.CleanAbort, want.Outcome.CleanAbort)
		case have.Change.DryRun != want.Change.DryRun:
			t.Errorf("record %d: DryRun = %v, want %v", i, have.Change.DryRun, want.Change.DryRun)
		case !have.Change.FinishedAt.Equal(want.Change.FinishedAt):
			t.Errorf("record %d: FinishedAt = %v, want %v", i, have.Change.FinishedAt, want.Change.FinishedAt)
		case have.Rollback.Attempted != want.Rollback.Attempted:
			t.Errorf("record %d: Rollback.Attempted = %v, want %v", i, have.Rollback.Attempted, want.Rollback.Attempted)
		}
	}
}

// TestLifecycleMarker_SurvivesAPayloadContainingACommentTerminator pins the property the
// encoding choice rests on.
//
// [encoding/json.Marshal] escapes `>` by default, so a marshalled payload cannot contain
// the `-->` that would truncate the HTML comment and leave the marker unparseable — or,
// worse, parseable and short. That default is load-bearing rather than incidental, so it
// is asserted with a payload that contains the sequence.
func TestLifecycleMarker_SurvivesAPayloadContainingACommentTerminator(t *testing.T) {
	recs := markedLifecycle()
	for i := range recs {
		recs[i].Approver.Ref = "artifact--> injected"
	}

	marker, err := LifecycleMarker(recs)
	if err != nil {
		t.Fatalf("LifecycleMarker: %v", err)
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(marker, lifecycleMarkerPrefix), lifecycleMarkerSuffix)
	if strings.Contains(inner, "-->") {
		t.Fatalf("the marker payload contains a literal comment terminator:\n%s", inner)
	}
	got, err := ParseLifecycleMarker("prose\n" + marker + "\nmore prose")
	if err != nil {
		t.Fatalf("ParseLifecycleMarker: %v", err)
	}
	if got[0].Approver.Ref != "artifact--> injected" {
		t.Errorf("the escaped value did not survive the round trip: %q", got[0].Approver.Ref)
	}
}

// TestParseLifecycleMarker_DistinguishesAbsenceFromCorruption is the rule a rebuild
// depends on: an action still in flight contributes nothing and is normal, while a marker
// that exists and cannot be read is history about to be lost.
func TestParseLifecycleMarker_DistinguishesAbsenceFromCorruption(t *testing.T) {
	if _, err := ParseLifecycleMarker("a body with no marker at all"); !errors.Is(err, ErrNoMarker) {
		t.Errorf("a body with no marker returned %v, want ErrNoMarker", err)
	}
	if _, err := ParseLifecycleMarker(""); !errors.Is(err, ErrNoMarker) {
		t.Errorf("an empty body returned %v, want ErrNoMarker", err)
	}

	for name, body := range map[string]string{
		"malformed json":       lifecycleMarkerPrefix + `{"v":1,"identity":` + lifecycleMarkerSuffix,
		"wrong version":        lifecycleMarkerPrefix + `{"v":99,"identity":"a","cluster":"c","operation":"o","records":[{"phase":"executed","authority":"policy"}]}` + lifecycleMarkerSuffix,
		"no records":           lifecycleMarkerPrefix + `{"v":1,"identity":"a","cluster":"c","operation":"o","records":[]}` + lifecycleMarkerSuffix,
		"no identity":          lifecycleMarkerPrefix + `{"v":1,"cluster":"c","operation":"o","records":[{"phase":"executed","authority":"policy"}]}` + lifecycleMarkerSuffix,
		"unreadable phase":     lifecycleMarkerPrefix + `{"v":1,"identity":"a","cluster":"c","operation":"o","records":[{"phase":"finished","authority":"policy"}]}` + lifecycleMarkerSuffix,
		"unreadable authority": lifecycleMarkerPrefix + `{"v":1,"identity":"a","cluster":"c","operation":"o","records":[{"phase":"executed","authority":"robot"}]}` + lifecycleMarkerSuffix,
	} {
		_, err := ParseLifecycleMarker(body)
		if err == nil {
			t.Errorf("%s: parsed without error", name)
			continue
		}
		if errors.Is(err, ErrNoMarker) {
			t.Errorf("%s: reported as absent, which a rebuild would skip silently", name)
		}
	}
}

// TestLifecycleMarker_RefusesRecordsThatDisagreeAboutTheAction. This package copies the
// action onto every record precisely so a disagreement is visible; writing one out anyway
// would put an artifact on a public trail attributing one cluster's mutation to another's
// history.
func TestLifecycleMarker_RefusesRecordsThatDisagreeAboutTheAction(t *testing.T) {
	recs := markedLifecycle()
	recs[2].Action.Cluster = "staging"
	if _, err := LifecycleMarker(recs); err == nil {
		t.Fatal("a lifecycle whose records name two clusters was marked")
	}

	if _, err := LifecycleMarker(nil); err == nil {
		t.Error("an empty lifecycle was marked")
	}
	if _, err := LifecycleMarker([]Record{{Phase: PhaseExecuted}}); err == nil {
		t.Error("a lifecycle with no identified action was marked")
	}
}

// TestWithLifecycleMarker_ReplacesRatherThanAccumulates is the property that keeps a body
// from carrying two answers.
//
// The gated trail writes its marker onto a body that has already been re-rendered several
// times, and a caller cannot promise the write happens once — a retried pass, a re-recorded
// outcome, or a rollback landing after the execution all reach the same artifact. Two
// markers in one body would make the reconstructed history depend on which one the parser
// reached first, so the second write must replace the first rather than sit beside it.
func TestWithLifecycleMarker_ReplacesRatherThanAccumulates(t *testing.T) {
	first, err := LifecycleMarker(markedLifecycle())
	if err != nil {
		t.Fatalf("LifecycleMarker: %v", err)
	}

	rolled := markedLifecycle()
	rolled[1].Outcome.Convergence = "converged"
	second, err := LifecycleMarker(rolled)
	if err != nil {
		t.Fatalf("LifecycleMarker: %v", err)
	}
	if first == second {
		t.Fatal("the two markers are identical, so this test cannot tell replacement from accumulation")
	}

	body := WithLifecycleMarker(WithLifecycleMarker("## Prose a person reads", first), second)

	if n := strings.Count(body, lifecycleMarkerPrefix); n != 1 {
		t.Fatalf("the body carries %d lifecycle markers, want exactly 1:\n%s", n, body)
	}
	if !strings.Contains(body, "## Prose a person reads") {
		t.Errorf("the rendered prose was lost:\n%s", body)
	}
	got, err := ParseLifecycleMarker(body)
	if err != nil {
		t.Fatalf("ParseLifecycleMarker: %v", err)
	}
	if got[1].Outcome.Convergence != "converged" {
		t.Errorf("the body reports convergence %q, want the most recently written %q",
			got[1].Outcome.Convergence, "converged")
	}
}
