package diagnose

import (
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
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
