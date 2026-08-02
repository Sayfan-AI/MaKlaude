package disclose

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// The marker exists for exactly one consumer — a rebuild of the trust ledger from the
// artifacts — so the tests that matter most are not about the encoding. They are about
// whether a ledger rebuilt through the marker agrees, entry for entry, with the ledger
// built by appending as the executions happened. Everything else here is the failure
// modes that would make that agreement silently untrue.

// TestLifecycleMarker_RoundTripsTheFieldsThatDecideATrustEntry is the base property. It
// asserts the projection, not the encoding: the fields listed are exactly the ones
// [trust.EntryFrom] reads, and one dropped in transit is history that changes meaning.
func TestLifecycleMarker_RoundTripsTheFieldsThatDecideATrustEntry(t *testing.T) {
	recs := lifecycle("timed-out", "execute-failed", false, false)

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

// TestRebuildFromTheMarkersReproducesTheLiveLedger is the criterion T3 could not close.
//
// It runs both paths over the same executions: one ledger appended to as each lifecycle
// finished, and one rebuilt from nothing but the artifact bodies. The lifecycles cover
// every outcome the classifier distinguishes, in an order chosen so the two paths would
// disagree if the marker lost the finishing instant, the authority, the convergence
// verdict, the clean-abort flag or the rollback flag.
func TestRebuildFromTheMarkersReproducesTheLiveLedger(t *testing.T) {
	base := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)

	// Distinct actions so their entries do not collapse on key. Each pairs a shape with
	// an outcome, and the mix spans converged/inconclusive/failed/drift-aborted plus a
	// human-approved entry, which is the only kind that can promote.
	type execution struct {
		identity    string
		cluster     string
		operation   remediate.Operation
		authority   audit.Authority
		convergence string
		failure     string
		cleanAbort  bool
		rollback    bool
		at          time.Time
	}
	executions := []execution{
		{"e1", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base},
		{"e2", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base.Add(time.Minute)},
		{"e3", "prod", remediate.OpRolloutRestart, audit.AuthorityHuman, "converged", "", false, false, base.Add(2 * time.Minute)},
		{"e4", "prod", remediate.OpRolloutRestart, audit.AuthorityPolicy, "timed-out", "", false, false, base.Add(3 * time.Minute)},
		{"e5", "prod", remediate.OpDeletePod, audit.AuthorityHuman, "", "drifted", true, false, base.Add(4 * time.Minute)},
		{"e6", "staging", remediate.OpCordonNode, audit.AuthorityHuman, "converged", "", false, true, base.Add(5 * time.Minute)},
		{"e7", "staging", remediate.OpCordonNode, audit.AuthorityPolicy, "", "execute-failed", false, false, base.Add(6 * time.Minute)},
	}

	build := func(e execution) []audit.Record {
		action := audit.Action{
			Identity:  remediate.ProposalIdentity(e.identity),
			Cluster:   e.cluster,
			Operation: e.operation,
		}
		approver := audit.Approver{Authority: e.authority, Ref: "artifact-" + e.identity}
		change := audit.Change{Sent: true, Applied: true, FinishedAt: e.at}

		recs := []audit.Record{
			{RecordedAt: e.at, Phase: audit.PhaseExecuted, Action: action, Approver: approver, Change: change},
		}
		if e.convergence != "" {
			recs = append(recs, audit.Record{
				RecordedAt: e.at, Phase: audit.PhaseVerified, Action: action, Approver: approver, Change: change,
				Outcome: audit.Outcome{Convergence: e.convergence, Failure: "none"},
			})
		}
		if e.failure != "" {
			recs = append(recs, audit.Record{
				RecordedAt: e.at, Phase: audit.PhaseFailed, Action: action, Approver: approver, Change: change,
				Outcome: audit.Outcome{Failure: e.failure, CleanAbort: e.cleanAbort},
			})
		}
		if e.rollback {
			recs = append(recs, audit.Record{
				RecordedAt: e.at, Phase: audit.PhaseRolledBack, Action: action, Approver: approver, Change: change,
				Rollback: audit.Rollback{Attempted: true, Performed: true},
			})
		}
		for i := range recs {
			recs[i].Seq = i + 1
		}
		return recs
	}

	live := trust.NewMemory()
	var bodies []string
	for _, e := range executions {
		recs := build(e)
		if err := live.RecordLifecycle(recs); err != nil {
			t.Fatalf("live append for %s: %v", e.identity, err)
		}
		marker, err := LifecycleMarker(recs)
		if err != nil {
			t.Fatalf("marking %s: %v", e.identity, err)
		}
		bodies = append(bodies, "## Some rendered prose a person reads\n\n"+marker+"\n")
	}

	// The rebuild reads nothing but the bodies — no report, no in-process trail, no
	// knowledge of the order they were written in.
	var entries []trust.Entry
	for i, body := range bodies {
		recs, err := ParseLifecycleMarker(body)
		if err != nil {
			t.Fatalf("parsing body %d: %v", i, err)
		}
		entry, err := trust.EntryFrom(recs)
		if err != nil {
			t.Fatalf("projecting body %d: %v", i, err)
		}
		entries = append(entries, entry)
	}
	// Shuffled deliberately: a rebuild reads artifacts in whatever order the trail
	// returns them, and the ledger's ordering must come from the recorded instants.
	entries[0], entries[len(entries)-1] = entries[len(entries)-1], entries[0]

	rebuilt := trust.NewMemory()
	if err := rebuilt.Rebuild(entries); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	liveEntries, rebuiltEntries := live.Entries(), rebuilt.Entries()
	if len(liveEntries) != len(rebuiltEntries) {
		t.Fatalf("the rebuilt ledger has %d entries, the live one has %d", len(rebuiltEntries), len(liveEntries))
	}
	for i := range liveEntries {
		if liveEntries[i] != rebuiltEntries[i] {
			t.Errorf("entry %d differs:\n  live:    %+v\n  rebuilt: %+v", i, liveEntries[i], rebuiltEntries[i])
		}
	}

	// The arithmetic the entries exist for must also agree. Comparing the standing rather
	// than only the entries is what catches a difference that survives field equality —
	// an ordering that puts the same entries in a different window.
	for _, shape := range []autonomy.Shape{
		{Cluster: "prod", Operation: remediate.OpRolloutRestart},
		{Cluster: "prod", Operation: remediate.OpDeletePod},
		{Cluster: "staging", Operation: remediate.OpCordonNode},
	} {
		wantEv, gotEv := live.Trust(shape), rebuilt.Trust(shape)
		if wantEv != gotEv {
			t.Errorf("shape %s: rebuilt trust %+v, live trust %+v", shape, gotEv, wantEv)
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
	recs := convergedLifecycle()
	recs[0].Approver.Ref = "artifact--> injected"
	recs[1].Approver.Ref = "artifact--> injected"
	recs[2].Approver.Ref = "artifact--> injected"

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
	// An artifact opened before its action ran carries the proposal and shape markers and
	// no lifecycle marker — the exact in-flight case.
	if _, err := ParseLifecycleMarker(Body(earnedAction())); !errors.Is(err, ErrNoMarker) {
		t.Errorf("an in-flight disclosure returned %v, want ErrNoMarker", err)
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

// TestLifecycleMarker_RefusesRecordsThatDisagreeAboutTheAction. The audit package copies
// the action onto every record precisely so a disagreement is visible; writing one out
// anyway would put an artifact on a public trail attributing one cluster's mutation to
// another's history.
func TestLifecycleMarker_RefusesRecordsThatDisagreeAboutTheAction(t *testing.T) {
	recs := convergedLifecycle()
	recs[2].Action.Cluster = "staging"
	if _, err := LifecycleMarker(recs); err == nil {
		t.Fatal("a lifecycle whose records name two clusters was marked")
	}

	if _, err := LifecycleMarker(nil); err == nil {
		t.Error("an empty lifecycle was marked")
	}
	if _, err := LifecycleMarker([]audit.Record{{Phase: audit.PhaseExecuted}}); err == nil {
		t.Error("a lifecycle with no identified action was marked")
	}
}

// TestShapeMarker_RoundTripsAndSplitsFromTheRight. Splitting on the first separator would
// mis-attribute an action whenever a registered cluster name contains one, and the
// consequence is a revocation applied to the wrong cluster's shape.
func TestShapeMarker_RoundTripsAndSplitsFromTheRight(t *testing.T) {
	for _, want := range []autonomy.Shape{
		{Cluster: "prod", Operation: remediate.OpRolloutRestart},
		{Cluster: "eu/prod-1", Operation: remediate.OpCordonNode},
		{Cluster: "a/b/c", Operation: remediate.OpDeletePod},
	} {
		body := "prose\n" + shapeMarker(want) + "\n"
		got, ok := ParseShapeMarker(body)
		if !ok {
			t.Fatalf("shape %s did not round-trip", want)
		}
		if got != want {
			t.Errorf("ParseShapeMarker = %+v, want %+v", got, want)
		}
	}

	for name, body := range map[string]string{
		"absent":       "no marker here",
		"no separator": shapeMarkerPrefix + "prod" + shapeMarkerSuffix,
		"no operation": shapeMarkerPrefix + "prod/" + shapeMarkerSuffix,
		"no cluster":   shapeMarkerPrefix + "/rolloutrestart" + shapeMarkerSuffix,
	} {
		if got, ok := ParseShapeMarker(body); ok {
			t.Errorf("%s: parsed as %+v, want refused", name, got)
		}
	}
}

// TestParseProposalMarker_IsWhatTellsThisTrailsArtifactsApart. A body carrying the label
// and nothing else is not this trail's to manage.
func TestParseProposalMarker_IsWhatTellsThisTrailsArtifactsApart(t *testing.T) {
	id, ok := ParseProposalMarker(Body(earnedAction()))
	if !ok {
		t.Fatal("a disclosure body carries no readable proposal marker")
	}
	if id != testProposal().Identity {
		t.Errorf("ParseProposalMarker = %q, want %q", id, testProposal().Identity)
	}
	for _, body := range []string{"", "an issue a person opened by hand", proposalMarkerPrefix + proposalMarkerSuffix} {
		if _, ok := ParseProposalMarker(body); ok {
			t.Errorf("a body without a well-formed marker was claimed: %q", body)
		}
	}
}

// TestBodyWithOutcome_SaysSoWhenTheLifecycleCannotBeMarked. A silently missing marker is
// history the ledger will not know it lost, so absence is reported in the body.
func TestBodyWithOutcome_SaysSoWhenTheLifecycleCannotBeMarked(t *testing.T) {
	body := BodyWithOutcome(earnedAction(), Outcome{Report: convergedReport()})

	if strings.Contains(body, lifecycleMarkerPrefix) {
		t.Fatal("a marker was written for a lifecycle with no records")
	}
	if !strings.Contains(body, "cannot be rebuilt from this artifact") {
		t.Errorf("the body does not report the missing marker:\n%s", body)
	}
}
