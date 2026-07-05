package aidiagnose

import (
	"context"
	"os"
	"strings"
	"sync/atomic"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// Refiner is the LLM-backed [github.com/Sayfan-AI/MaKlaude/internal/diagnose.Refiner]:
// it wraps a [Provider] to sharpen or extend the deterministic hypotheses, inside
// the T5 safety boundary. It is the concrete home of every guarantee the package
// doc promises — read-only, redaction-before-egress, cost-bounded, gracefully
// degrading, and audited — so a caller wires ONE of these into the diagnose seam
// and gets the whole boundary.
//
// A Refiner is scoped to a SINGLE scan cycle: its call budget is a live counter
// that only decreases, so construct a fresh one per cycle (that is exactly what
// [Builder] does). It is safe for concurrent Refine calls within that cycle — the
// budget is decremented atomically — though the pipeline drives it sequentially.
type Refiner struct {
	cfg      Config
	provider Provider
	auditor  Auditor
	// ctx is the surrounding scan's context; each provider call derives a
	// timeout-bounded child from it so a cancelled scan cancels in-flight calls.
	ctx context.Context
	// budget is the remaining per-cycle provider-call allowance. It is decremented
	// atomically and, once exhausted, every further incident degrades to its
	// deterministic hypotheses.
	budget int64
}

// newRefiner builds a per-cycle [Refiner]. It is unexported because a Refiner must
// be constructed fresh per scan (for a fresh budget); callers obtain one through a
// [Builder]. A nil provider or auditor is replaced with a safe default, so Refine
// never needs a nil check.
func newRefiner(ctx context.Context, cfg Config, provider Provider, auditor Auditor) *Refiner {
	if ctx == nil {
		ctx = context.Background()
	}
	if auditor == nil {
		auditor = nopAuditor{}
	}
	return &Refiner{
		cfg:      cfg,
		provider: provider,
		auditor:  auditor,
		ctx:      ctx,
		budget:   int64(cfg.callBudget()),
	}
}

// Refine implements [github.com/Sayfan-AI/MaKlaude/internal/diagnose.Refiner]. It
// consults the provider for one incident and folds any usable suggestions into a
// NEW hypothesis slice, stamping every added or rewritten hypothesis
// [github.com/Sayfan-AI/MaKlaude/internal/diagnose.SourceRefined]. It never mutates
// base or snap, and it degrades to returning base unchanged on ANY adverse
// condition — no provider, exhausted budget, timeout, provider error, or even a
// panic — so it can never fail the scan or drop the deterministic diagnosis. The
// core re-sorts the result, so ordering here is irrelevant.
func (r *Refiner) Refine(snap health.Snapshot, incident correlate.Incident, base []diagnose.Hypothesis) (result []diagnose.Hypothesis) {
	// Defence in depth: a misbehaving provider (or JSON parser) must never take
	// down a scan. Any panic collapses to the deterministic base.
	defer func() {
		if rec := recover(); rec != nil {
			r.auditor.Record(r.ctx, AuditRecord{
				Cluster:  incident.Cluster,
				Incident: incident.Identity,
				Purpose:  refinePurpose,
				Model:    r.cfg.model(),
				Outcome:  OutcomeError,
				Detail:   "recovered panic in refiner",
			})
			result = base
		}
	}()

	if r.provider == nil {
		return base
	}

	// Enforce the per-cycle call budget before doing any work (or any egress).
	if atomic.AddInt64(&r.budget, -1) < 0 {
		r.auditor.Record(r.ctx, AuditRecord{
			Cluster:  incident.Cluster,
			Incident: incident.Identity,
			Purpose:  refinePurpose,
			Model:    r.cfg.model(),
			Outcome:  OutcomeSkippedBudget,
		})
		return base
	}

	req := buildRequest(r.cfg, snap, incident, base)

	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.timeout())
	defer cancel()

	resp, err := r.provider.Suggest(ctx, req)
	if err != nil {
		r.auditor.Record(r.ctx, AuditRecord{
			Cluster:       incident.Cluster,
			Incident:      incident.Identity,
			Purpose:       refinePurpose,
			Model:         r.cfg.model(),
			EvidenceBytes: len(req.Evidence),
			Outcome:       OutcomeError,
			Detail:        err.Error(),
		})
		return base
	}

	refined, changed := applySuggestions(incident, base, resp.Suggestions)
	outcome := OutcomeNoChange
	if changed {
		outcome = OutcomeRefined
	}
	r.auditor.Record(r.ctx, AuditRecord{
		Cluster:       incident.Cluster,
		Incident:      incident.Identity,
		Purpose:       refinePurpose,
		Model:         r.cfg.model(),
		EvidenceBytes: len(req.Evidence),
		Outcome:       outcome,
	})
	return refined
}

// maxAppliedSuggestions caps how many suggestions one incident can absorb, so a
// verbose model cannot balloon the hypothesis list. It matches the number of
// hypotheses the evidence describes, which is more than any real incident needs.
const maxAppliedSuggestions = maxEvidenceHypotheses

// applySuggestions folds provider suggestions into a fresh copy of base, returning
// the new slice and whether anything changed. It is pure and defensive: it copies
// base (never mutating the input), validates each suggestion, and stamps every
// result SourceRefined. A suggestion whose cause matches an existing hypothesis
// REWRITES that hypothesis in place (preserving its stable identity and evidence);
// a suggestion with a novel cause ADDS a new refined hypothesis. Invalid or empty
// suggestions are skipped, so a malformed model reply degrades to no change rather
// than corrupt output.
func applySuggestions(incident correlate.Incident, base []diagnose.Hypothesis, suggestions []Suggestion) ([]diagnose.Hypothesis, bool) {
	out := make([]diagnose.Hypothesis, len(base))
	copy(out, base)

	// Index by cause so a suggestion can find and rewrite an existing hypothesis.
	idx := make(map[diagnose.Cause]int, len(out))
	for i := range out {
		idx[out[i].Cause] = i
	}

	changed := false
	applied := 0
	for _, s := range suggestions {
		if applied >= maxAppliedSuggestions {
			break
		}
		cause := normalizeCause(s.Cause)
		title := strings.TrimSpace(s.Title)
		if cause == "" || title == "" {
			continue // an unusable suggestion is dropped, never applied blindly
		}
		conf := parseConfidence(s.Confidence)
		message := strings.TrimSpace(s.Message)

		if i, ok := idx[cause]; ok {
			// Rewrite the existing hypothesis, keeping its identity and evidence and
			// only re-attributing it to the refined layer.
			h := out[i]
			h.Title = title
			h.Message = message
			h.Confidence = conf
			h.Source = diagnose.SourceRefined
			out[i] = h
			changed = true
			applied++
			continue
		}

		// A cause the deterministic rules did not express: add it, citing the whole
		// incident as its evidence.
		out = append(out, diagnose.NewRefinedHypothesis(incident, cause, conf, title, message, incident.Findings()))
		idx[cause] = len(out) - 1
		changed = true
		applied++
	}
	return out, changed
}

// normalizeCause coerces a model-supplied cause into the lowercase, delimiter-free
// slug convention diagnose.Cause uses, so it composes cleanly into a stable
// identity and cannot smuggle whitespace or punctuation into the key. It keeps
// ASCII letters and digits only; an empty result means "unusable".
func normalizeCause(s string) diagnose.Cause {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return diagnose.Cause(b.String())
}

// parseConfidence maps a model-supplied confidence token to a
// diagnose.Confidence, defaulting to the LOWEST level for anything unrecognized so
// a malformed or over-eager reply can never masquerade as high confidence.
func parseConfidence(s string) diagnose.Confidence {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "high":
		return diagnose.ConfidenceHigh
	case "medium", "med":
		return diagnose.ConfidenceMedium
	default:
		return diagnose.ConfidenceLow
	}
}

// Builder constructs a fresh per-cycle [Refiner] bound to a scan's context. The
// pipeline calls it once at the start of each scan so every cycle gets its own
// call budget. It returns a diagnose.Refiner (the seam type) so the pipeline need
// not know the concrete type.
type Builder func(ctx context.Context) diagnose.Refiner

// RefinerFromEnv is the single wiring seam the pipeline uses to obtain the
// optional LLM refinement layer, mirroring
// [github.com/Sayfan-AI/MaKlaude/internal/escalate.EscalatorFromEnv]:
//
//   - When the layer is NOT configured/enabled (the default), it returns a nil
//     [Builder] and live=false, so the pipeline runs the deterministic core with
//     no refinement and zero behaviour change versus T5 being absent.
//   - When it IS active ([Config.Active]), it returns a Builder that mints a fresh,
//     budgeted [Refiner] over a live [ClaudeProvider] for each scan cycle, and
//     live=true.
//
// The API key is read from the environment at runtime and never logged. Returning
// a nil Builder (rather than an inert refiner) lets the pipeline cheaply skip all
// refinement wiring when the feature is off.
func RefinerFromEnv() (build Builder, live bool) {
	cfg := ConfigFromEnv(os.Getenv)
	if !cfg.Active() {
		return nil, false
	}
	provider := NewClaudeProvider(cfg)
	auditor := NewLogAuditor(nil)
	build = func(ctx context.Context) diagnose.Refiner {
		return newRefiner(ctx, cfg, provider, auditor)
	}
	return build, true
}

// NewRefinerForTest builds a [Refiner] with explicit seams for unit tests: an
// injected provider and auditor and a context. It is the test counterpart to the
// env path, letting tests exercise redaction, budgeting, degradation, and audit
// without any network. A nil auditor uses the no-op auditor.
func NewRefinerForTest(ctx context.Context, cfg Config, provider Provider, auditor Auditor) *Refiner {
	return newRefiner(ctx, cfg, provider, auditor)
}
