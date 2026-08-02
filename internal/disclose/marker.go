package disclose

import (
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file carries the markers that identify a disclosure artifact as this trail's and
// name the shape whose autonomy it ran under.
//
// The third marker a finished artifact carries — the machine-readable lifecycle a rebuild
// reads — is NOT here. It was, and moving it out is the point: the approval trail has to
// write the same marker, `disclose` imports `approve`, and so no marker owned by this
// package can ever be written by that one. It lives in [audit.LifecycleMarker], the package
// that owns the records it serializes and the only one both trails already import. The
// reasoning that produced its format is in that file.

// Hidden HTML-comment markers embedded in a disclosure body. They are separate markers
// rather than one because they are written at different times and mean different things
// when absent: the proposal and shape markers exist from the instant the artifact is
// opened, and the lifecycle marker only appears once the action has finished. An
// artifact with the first two and not the third is an action that started and never
// reported — see [AppliedLabel].
const (
	proposalMarkerPrefix = "<!-- maklaude:autonomous-proposal="
	proposalMarkerSuffix = " -->"

	shapeMarkerPrefix = "<!-- maklaude:autonomous-shape="
	shapeMarkerSuffix = " -->"
)

// proposalMarker renders the hidden marker embedding a proposal identity.
func proposalMarker(id remediate.ProposalIdentity) string {
	return proposalMarkerPrefix + string(id) + proposalMarkerSuffix
}

// ParseProposalMarker extracts the embedded proposal identity from a disclosure body,
// ok=false when no well-formed marker is present. A sink uses it to tell its own
// artifacts from anything else that happens to carry [ManagedLabel].
func ParseProposalMarker(body string) (remediate.ProposalIdentity, bool) {
	raw, ok := betweenMarkers(body, proposalMarkerPrefix, proposalMarkerSuffix)
	if !ok || raw == "" {
		return "", false
	}
	return remediate.ProposalIdentity(raw), true
}

// shapeMarker renders the hidden marker embedding the shape whose autonomy this action
// ran under — the granularity [RevokedLabel] revokes at.
func shapeMarker(s autonomy.Shape) string {
	return shapeMarkerPrefix + s.String() + shapeMarkerSuffix
}

// ParseShapeMarker extracts the embedded [autonomy.Shape], ok=false when absent or
// malformed.
//
// It splits on the LAST separator rather than the first. [autonomy.Shape.String]
// renders "cluster/operation", the operation is a catalog token that never contains a
// separator, and a registered cluster name is not constrained not to — so splitting
// from the right is the reading that cannot mis-attribute an action to a cluster whose
// name happens to contain a slash.
func ParseShapeMarker(body string) (autonomy.Shape, bool) {
	raw, ok := betweenMarkers(body, shapeMarkerPrefix, shapeMarkerSuffix)
	if !ok {
		return autonomy.Shape{}, false
	}
	cut := strings.LastIndex(raw, "/")
	if cut <= 0 || cut == len(raw)-1 {
		return autonomy.Shape{}, false
	}
	return autonomy.Shape{
		Cluster:   raw[:cut],
		Operation: remediate.Operation(raw[cut+1:]),
	}, true
}

// betweenMarkers extracts the text between a marker's prefix and the first suffix that
// follows it. ok is false when the prefix is absent or the marker is unterminated.
func betweenMarkers(body, prefix, suffix string) (string, bool) {
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
