package disclose

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The lifecycle marker's own encoding tests live in internal/audit, which owns the format
// now that both trails write it, and the cross-trail rebuild property lives in
// internal/rebuild. What stays here is this trail's own markers, plus the two places a
// disclosure body and the shared marker meet.

// TestBody_CarriesNoLifecycleMarkerUntilTheActionFinishes is the in-flight case a rebuild
// must read as "nothing to contribute" rather than as corruption. An artifact is opened
// BEFORE the action runs, so a body carrying the proposal and shape markers and no
// lifecycle marker is the ordinary state of an action still in progress.
func TestBody_CarriesNoLifecycleMarkerUntilTheActionFinishes(t *testing.T) {
	if _, err := audit.ParseLifecycleMarker(Body(earnedAction())); !errors.Is(err, audit.ErrNoMarker) {
		t.Errorf("an in-flight disclosure returned %v, want audit.ErrNoMarker", err)
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

	if _, err := audit.ParseLifecycleMarker(body); !errors.Is(err, audit.ErrNoMarker) {
		t.Fatalf("a marker was written for a lifecycle with no records (parse returned %v)", err)
	}
	if !strings.Contains(body, "cannot be rebuilt from this artifact") {
		t.Errorf("the body does not report the missing marker:\n%s", body)
	}
}
