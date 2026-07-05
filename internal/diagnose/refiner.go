package diagnose

import (
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// Refiner is the extension seam that lets an optional fuzzy/LLM layer
// (Milestone 3 T5) improve on the deterministic hypotheses WITHOUT entangling
// this package's deterministic core.
//
// After the rule layer produces its byte-stable hypotheses for one incident,
// [DiagnoseWith] (and [DiagnoseAllWith]) hand that slice to a Refiner, which may
// re-rank it, adjust confidences, rewrite messages, drop weak hypotheses, or add
// new ones the deterministic rules could not express. The core imposes exactly
// one contract on a Refiner: anything it adds or rewrites should be stamped
// [SourceRefined] so a consumer can always tell a reproducible rule result from
// a fuzzier refinement. The core re-sorts whatever the Refiner returns, so a
// Refiner need not sort.
//
// A Refiner is entirely optional. The plain [Diagnose] and [DiagnoseAll]
// entrypoints use [NopRefiner], so the deterministic core is fully usable — and
// fully testable for byte-stable output — with no Refiner at all. This is what
// keeps the fuzzy layer strictly additive: if it is unavailable, misconfigured,
// or distrusted, MaKlaude still emits correct, auditable, deterministic
// hypotheses.
type Refiner interface {
	// Refine takes the deterministic hypotheses for one incident (in the snapshot
	// context) and returns a possibly-improved set. Implementations must not mutate
	// the input slice or the snapshot; they should return a new slice. The returned
	// hypotheses are re-sorted by the core, so ordering here does not matter.
	Refine(snap health.Snapshot, incident correlate.Incident, base []Hypothesis) []Hypothesis
}

// NopRefiner is the default [Refiner]: it returns the deterministic hypotheses
// unchanged. It is the identity element of the seam, used by [Diagnose] and
// [DiagnoseAll] so the deterministic core runs with no refinement and stays
// byte-stable.
type NopRefiner struct{}

// Refine returns base unchanged.
func (NopRefiner) Refine(_ health.Snapshot, _ correlate.Incident, base []Hypothesis) []Hypothesis {
	return base
}

// NewRefinedHypothesis constructs a [Hypothesis] attributed to the optional
// fuzzy/LLM [Refiner] layer (Milestone 3 T5), for a cause the deterministic rules
// could not themselves express. It is the public counterpart to this package's
// internal rule constructor: it fills in the same stable identity (derived from
// cause + the incident's already-stable identity), inherits the incident's
// cluster and detection time (so a refined hypothesis never reads its own clock),
// and — crucially — stamps the hypothesis [SourceRefined] so a consumer can always
// tell it apart from a reproducible deterministic result.
//
// A Refiner uses it so a refined hypothesis is indistinguishable in SHAPE from a
// deterministic one — same identity discipline, same cluster/time inheritance,
// same evidence citation — differing only in its [Source] marker and in not being
// guaranteed reproducible. Because identity is keyed on (cause, incident), a
// refined hypothesis whose cause matches an existing deterministic one shares its
// identity; a Refiner that means to ADD a distinct hypothesis should therefore use
// a cause not already present, and one that means to REWRITE an existing one
// should reuse that hypothesis's own identity (see the [Refiner] contract).
//
// The evidence slice is copied defensively so the caller cannot alias — or later
// mutate — the incident's own findings through the returned hypothesis.
func NewRefinedHypothesis(incident correlate.Incident, cause Cause, conf Confidence, title, message string, evidence []detect.Finding) Hypothesis {
	var ev []detect.Finding
	if len(evidence) > 0 {
		ev = make([]detect.Finding, len(evidence))
		copy(ev, evidence)
	}
	return Hypothesis{
		Identity:   newHypothesisIdentity(cause, incident.Identity),
		Incident:   incident.Identity,
		Cluster:    incident.Cluster,
		Cause:      cause,
		Confidence: conf,
		Title:      title,
		Message:    message,
		Evidence:   ev,
		Source:     SourceRefined,
		DetectedAt: incident.DetectedAt,
	}
}
