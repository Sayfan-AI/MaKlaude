package score

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// leakMarker is planted in every free-text field of a record so the assertion that a
// stored bundle carries no cluster-derived prose is a search for one string rather than
// an enumeration of the fields that exist today.
const leakMarker = "MARKERdonotleak7f3"

// verifiedRecord is a complete audit record for a human-approved delete-pod that landed
// and converged, with every free-text field carrying the leak marker.
func verifiedRecord() audit.Record {
	return audit.Record{
		Phase: audit.PhaseVerified,
		Action: audit.Action{
			Identity:      remediate.ProposalIdentity("proposal|deletepod|" + testCluster + "|pod/shop/web-dead"),
			Cluster:       testCluster,
			Operation:     remediate.OpDeletePod,
			Target:        remediate.Target{Cluster: testCluster, Kind: "pod", Namespace: testNamespace, Name: "web-dead", ResourceVersion: "4004"},
			Reversibility: remediate.ReversibilityRecreatedByController,
			Fingerprint:   "fp1:abc123",
			Title:         "Delete failed pod so its controller recreates it",
			ProposedAt:    scoreAt.Add(-2 * time.Minute),
		},
		Approver: audit.Approver{
			Authority:    audit.AuthorityHuman,
			Identity:     testApprover,
			ApprovedAt:   scoreAt.Add(-time.Minute),
			AuthorizedAt: scoreAt.Add(-30 * time.Second),
			Ref:          testRef,
		},
		Change: audit.Change{
			Sent:            true,
			Applied:         true,
			Mode:            "live",
			Scope:           "DELETE /api/v1/namespaces/shop/pods/web-dead",
			ResourceVersion: "4004",
			Attempts:        1,
			RecordedOnTrail: true,
			StartedAt:       scoreAt,
			FinishedAt:      scoreAt.Add(30 * time.Second),
		},
		PreState: audit.PreState{
			Captured:        true,
			Kind:            "pod",
			ResourceVersion: "4004",
			ObservedAt:      scoreAt,
			Fields:          []audit.PreStateField{{Name: "phase", Value: "Failed " + leakMarker}},
		},
		Outcome: audit.Outcome{
			Convergence: TokenConverged,
			Detail:      "a replacement pod appeared " + leakMarker,
			ObservedFor: 25 * time.Second,
			Failure:     "none",
		},
		Rollback: audit.Rollback{
			Kind:        "not-required",
			Note:        "the controller recreates it " + leakMarker,
			Description: "nothing to undo " + leakMarker,
		},
		Detail: "everything went fine " + leakMarker,
	}
}

// storedTrail appends the given records to a real trail, so the facts under test are
// projected from sequenced, timestamped, redacted records rather than from structs a
// test filled in.
func storedTrail(t *testing.T, recs ...audit.Record) []audit.Record {
	t.Helper()
	trail := audit.NewTrail().WithClock(func() time.Time { return scoreAt })
	for _, rec := range recs {
		if _, err := trail.Append(context.Background(), rec); err != nil {
			t.Fatalf("appending to the trail: %v", err)
		}
	}
	return trail.Records()
}

// TestFactFrom_ProjectsEveryFieldTheVerdictReads pins the projection. It is the step
// [Replay] cannot check — a replay re-runs the verdict over stored facts, so a
// projection that dropped a field would replay perfectly and be wrong about the world.
func TestFactFrom_ProjectsEveryFieldTheVerdictReads(t *testing.T) {
	recs := storedTrail(t, verifiedRecord())
	facts := FactsFrom(recs)

	if len(facts) != 1 {
		t.Fatalf("projected %d facts from 1 record", len(facts))
	}
	got := facts[0]

	want := Fact{
		Seq:               1,
		Identity:          remediate.ProposalIdentity("proposal|deletepod|" + testCluster + "|pod/shop/web-dead"),
		Cluster:           testCluster,
		Operation:         remediate.OpDeletePod,
		TargetCluster:     testCluster,
		TargetNamespace:   testNamespace,
		Reversibility:     remediate.ReversibilityRecreatedByController.String(),
		Authority:         audit.AuthorityHuman.String(),
		Approver:          testApprover,
		Ref:               testRef,
		Sent:              true,
		Applied:           true,
		DryRun:            false,
		CleanAbort:        false,
		RollbackAttempted: false,
		Convergence:       TokenConverged,
		StartedAt:         scoreAt,
		FinishedAt:        scoreAt.Add(30 * time.Second),
	}
	if got != want {
		t.Fatalf("projection:\n got %+v\nwant %+v", got, want)
	}
}

// TestBundle_CarriesNoClusterDerivedProse. Every free-text field of the source record
// holds the marker; none of them is a field a verdict reads, so none of them may reach
// the stored artifact. The trail redacts free text already — this is about a scorecard
// not being one more artifact whose publishability has to be reasoned about.
func TestBundle_CarriesNoClusterDerivedProse(t *testing.T) {
	bundle := NewBundle(EvidenceFrom(storedTrail(t, verifiedRecord()), nil))

	var buf strings.Builder
	if err := Write(&buf, bundle); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if strings.Contains(buf.String(), leakMarker) {
		t.Fatalf("the stored scorecard carries record free text:\n%s", buf.String())
	}
	// The card must still be there — a bundle that leaked nothing because it stored
	// nothing would pass the assertion above and be useless.
	if !strings.Contains(buf.String(), `"grade": "clean"`) {
		t.Fatalf("the stored scorecard holds no verdict:\n%s", buf.String())
	}
}

// TestBundle_RoundTripsAndReplays is task T6's third criterion: the verdict is
// reproducible from stored records, after the fact, without re-running anything.
func TestBundle_RoundTripsAndReplays(t *testing.T) {
	recs := storedTrail(t, verifiedRecord())
	original := NewBundle(EvidenceFrom(recs, []Window{openWindow()}))

	var buf strings.Builder
	if err := Write(&buf, original); err != nil {
		t.Fatalf("Write: %v", err)
	}

	restored, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	replayed, err := Replay(restored)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replayed) != 1 || !replayed[0].Equal(original.Cards[0]) {
		t.Fatalf("replaying the stored bundle produced %+v, want %+v", replayed, original.Cards)
	}
	// The window survived the round trip, which is what makes the verdict reproducible
	// rather than merely recomputable: without it this action re-scores as plain
	// converged.
	if replayed[0].Fix != FixConvergedUnderChaos {
		t.Fatalf("fix = %s after the round trip, want converged-under-chaos — the recorded window did not survive", replayed[0].Fix)
	}
}

// TestReplay_ReportsADisagreementRatherThanQuietlyUpdatingTheCard. A stored verdict and
// this build's verdict disagreeing about the same facts is always something a person has
// to look at: either a regression here, or a bundle written when the rules differed.
func TestReplay_ReportsADisagreementRatherThanQuietlyUpdatingTheCard(t *testing.T) {
	bundle := NewBundle(EvidenceFrom(storedTrail(t, verifiedRecord()), nil))

	tampered := bundle
	tampered.Cards = append([]Card(nil), bundle.Cards...)
	tampered.Cards[0].Grade = GradeOverPermitted

	if _, err := Replay(tampered); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("Replay of a tampered card returned %v, want ErrReplayMismatch", err)
	}

	dropped := bundle
	dropped.Cards = nil
	if _, err := Replay(dropped); !errors.Is(err, ErrReplayMismatch) {
		t.Fatalf("Replay of a bundle with no cards returned %v, want ErrReplayMismatch", err)
	}
}

// TestRead_RefusesWhatItCannotFullyUnderstand. Each of these is a bundle whose verdict
// was derived from something this build cannot see or cannot read, and scoring it anyway
// would produce a confident answer from partial input.
func TestRead_RefusesWhatItCannotFullyUnderstand(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "a version this build does not write",
			body: `{"version":99,"evidence":{"facts":[]},"cards":[]}`,
		},
		{
			name: "a field this build does not know, so the verdict rested on evidence it cannot see",
			body: `{"version":1,"evidence":{"facts":[]},"cards":[],"weather":"fine"}`,
		},
		{
			name: "a grade token this build cannot read",
			body: `{"version":1,"evidence":{"facts":[]},"cards":[{"identity":"x","cluster":"prod","operation":"deletepod","fix":"converged","grade":"A+"}]}`,
		},
		{
			name: "a fix token this build cannot read",
			body: `{"version":1,"evidence":{"facts":[]},"cards":[{"identity":"x","cluster":"prod","operation":"deletepod","fix":"mostly-fine","grade":"clean"}]}`,
		},
		{
			name: "a fault token this build cannot read",
			body: `{"version":1,"evidence":{"facts":[]},"cards":[{"identity":"x","cluster":"prod","operation":"deletepod","fix":"converged","faults":["vibes"],"grade":"over-permitted"}]}`,
		},
		{
			name: "not a bundle at all",
			body: `["nope"]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Read(strings.NewReader(tc.body)); !errors.Is(err, ErrBundle) {
				t.Fatalf("Read returned %v, want ErrBundle", err)
			}
		})
	}
}

// TestReplay_RefusesAVersionItDoesNotWrite covers the path a caller reaches when it
// hand-builds a bundle rather than reading one, which is the only way to get past
// [Read]'s own version check.
func TestReplay_RefusesAVersionItDoesNotWrite(t *testing.T) {
	bundle := NewBundle(EvidenceFrom(storedTrail(t, verifiedRecord()), nil))
	bundle.Version = BundleVersion + 1

	if _, err := Replay(bundle); !errors.Is(err, ErrBundle) {
		t.Fatalf("Replay of a future bundle returned %v, want ErrBundle", err)
	}
}

// TestWriteFile_StoresAReadableScorecard checks the artifact an operator actually keeps:
// on disk, at the same permissions as the durable stores beside it, and replayable
// straight back off the file.
func TestWriteFile_StoresAReadableScorecard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "scorecard.json")
	bundle := NewBundle(EvidenceFrom(storedTrail(t, verifiedRecord()), []Window{openWindow()}))

	if err := WriteFile(path, bundle); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != fs.FileMode(0o600) {
		t.Errorf("scorecard mode = %o, want 600 to match the durable stores in trust and budget", perm)
	}

	restored, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := Replay(restored); err != nil {
		t.Fatalf("Replay of the stored file: %v", err)
	}

	if _, err := ReadFile(filepath.Join(t.TempDir(), "absent.json")); !errors.Is(err, ErrBundle) {
		t.Errorf("ReadFile of a missing scorecard returned %v, want ErrBundle", err)
	}
}

// TestEvidenceFrom_CopiesTheWindowsItWasHanded. The evidence outlives the call that
// built it, and a caller that goes on to close a window must not retroactively change a
// stored verdict's input.
func TestEvidenceFrom_CopiesTheWindowsItWasHanded(t *testing.T) {
	windows := []Window{openWindow()}
	ev := EvidenceFrom(storedTrail(t, verifiedRecord()), windows)

	windows[0].Cluster = "somewhere-else"

	if ev.Windows[0].Cluster != testCluster {
		t.Fatalf("the evidence's window changed under it: %+v", ev.Windows[0])
	}
}

// TestFact_CitesNothing walks the three shapes of a missing citation, because an
// unattended action's citation is its entire oversight record and each of these is a way
// of having none.
func TestFact_CitesNothing(t *testing.T) {
	cases := []struct {
		name string
		fact Fact
		want bool
	}{
		{name: "a rule and an artifact", fact: earnedFact(), want: false},
		{name: "no approver at all", fact: func() Fact { f := earnedFact(); f.Approver = ""; return f }(), want: true},
		{name: "the bare policy prefix with no rule after it", fact: func() Fact { f := earnedFact(); f.Approver = "policy:"; return f }(), want: true},
		{name: "whitespace where a rule should be", fact: func() Fact { f := earnedFact(); f.Approver = "   "; return f }(), want: true},
		{name: "no artifact", fact: func() Fact { f := earnedFact(); f.Ref = ""; return f }(), want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fact.CitesNothing(); got != tc.want {
				t.Fatalf("CitesNothing() = %t, want %t", got, tc.want)
			}
		})
	}
}
