// Package aidiagnose is MaKlaude's OPTIONAL, gated, LLM-assisted diagnosis layer
// (Milestone 3 T5). It plugs into the deterministic diagnose package through the
// [github.com/Sayfan-AI/MaKlaude/internal/diagnose.Refiner] seam and refines the
// rule-based hypotheses for incidents the deterministic rules handle poorly —
// but only when a human has explicitly configured and enabled it.
//
// This is the one place in MaKlaude where cluster-derived data could leave the
// process, so the package is built as a strict, isolated safety boundary. Every
// guarantee below is enforced here and unit-tested; if ANY of them cannot hold,
// the layer silently falls back to the deterministic hypotheses rather than
// taking a risk:
//
//   - Read-only by construction. The [Provider] interface exposes exactly one
//     capability — turn a redacted text prompt into text suggestions. It has no
//     handle to a cluster, no client, no way to mutate anything. The refined
//     hypotheses it yields are, like the deterministic ones, purely informational.
//   - Redaction / data minimization BEFORE egress. Evidence is assembled from the
//     snapshot and incident and then passed through [Redact] at the egress
//     boundary, stripping secret values, tokens, credentials, and obvious PII, so
//     nothing sensitive is ever handed to a [Provider]. This is proven against
//     seeded secrets in the tests.
//   - Cost-bounded. Evidence is size-capped, the response token count is capped,
//     each call is deadline-bounded via [context.Context], and a per-cycle call
//     budget caps how many provider calls one scan can make.
//   - Graceful degradation. Unconfigured, disabled, over budget, erroring, or
//     timing out all resolve to the same safe outcome: return the deterministic
//     base hypotheses unchanged. The refiner never panics and never fails a scan.
//   - Audited. Every provider call's purpose and outcome is recorded through an
//     [Auditor], so the trail always shows what was sent for refinement and what
//     came back — including when the layer degraded and why.
//
// The default real provider is Claude (see [ClaudeProvider]); a [FakeProvider] is
// provided for tests. Nothing in this package is reached unless a human sets the
// MAKLAUDE_LLM_* environment (see [Config]); the deterministic core ships and
// runs entirely without it.
package aidiagnose

import "context"

// Provider is the minimal, provider-agnostic seam over a large language model.
// It is deliberately tiny and READ-ONLY BY CONSTRUCTION: its single method turns
// an already-redacted, size-bounded text [Request] into text [Suggestion]s (or an
// error). It is handed no cluster client, no snapshot, and no mutating capability
// of any kind, so an LLM can inform a diagnosis but can never act on a cluster —
// the safety property that lets this layer exist at all.
//
// A Provider must treat the request as untrusted, egress-sensitive data: by the
// time evidence reaches it, it has already passed through [Redact], and a Provider
// must not widen that boundary (for example by echoing the raw request into logs).
// Implementations should honour the request's deadline via ctx and return
// promptly on cancellation; a slow or failing Provider is expected and handled by
// the refiner's graceful degradation, so returning an error is always safe.
type Provider interface {
	// Suggest sends the redacted request to the model and returns its refinement
	// suggestions. It must respect ctx's deadline/cancellation and must return an
	// error (rather than partial or fabricated data) on any failure.
	Suggest(ctx context.Context, req Request) (Response, error)
}

// Request is the redacted, cost-bounded payload handed to a [Provider]. It
// carries only text: a static System instruction describing the task and the
// output contract, and the Evidence — the incident's already-redacted,
// size-capped description. It NEVER carries a cluster handle, credentials, or raw
// unredacted signal; [buildRequest] is the only constructor and it applies
// [Redact] to the evidence before the request is ever returned.
type Request struct {
	// System is the static task instruction and output contract for the model. It
	// contains no cluster-derived data, so it needs no redaction.
	System string

	// Evidence is the incident's description — findings, deterministic hypotheses,
	// and the relevant signals — AFTER redaction and size-capping. It is the only
	// cluster-derived text that egresses, and it has already crossed the [Redact]
	// boundary by the time a Provider sees it.
	Evidence string

	// MaxTokens caps the size of the model's response, bounding cost per call. A
	// Provider must pass it through to the model as the response token limit.
	MaxTokens int
}

// Response is a [Provider]'s structured refinement of one incident's diagnosis:
// zero or more [Suggestion]s. An empty response is valid and common — it means
// "the deterministic hypotheses already look right", and the refiner simply keeps
// the base unchanged.
type Response struct {
	// Suggestions are the model's proposed refinements, each either sharpening an
	// existing deterministic hypothesis or proposing one the rules could not
	// express. The refiner is the sole authority on how they are applied and stamps
	// every result [github.com/Sayfan-AI/MaKlaude/internal/diagnose.SourceRefined].
	Suggestions []Suggestion
}

// Suggestion is a single proposed refinement from a [Provider]. It is plain data:
// the model proposes a cause slug, a human-readable title and explanation, and a
// coarse confidence token, and the refiner decides — deterministically and
// defensively — how (and whether) to fold it into the hypotheses. A Suggestion can
// never carry an action; it only ever proposes an explanation.
//
// The JSON tags are the wire contract stated in the system prompt, so a
// [Provider] parsing a model reply can unmarshal directly into this type.
type Suggestion struct {
	// Cause is a short, lowercase, delimiter-free slug naming the proposed root
	// cause (for example "misconfiguredprobe"). When it matches an existing
	// deterministic hypothesis's cause, the refiner rewrites that hypothesis in
	// place; otherwise it adds a new refined hypothesis for the novel cause.
	Cause string `json:"cause"`

	// Title is a short human-readable label for the cause, suitable as a hypothesis
	// title. An empty title makes the suggestion unusable and is dropped.
	Title string `json:"title"`

	// Message is the fuller human-readable explanation of the proposed cause.
	Message string `json:"message"`

	// Confidence is the coarse strength token — "low", "medium", or "high",
	// matching diagnose.Confidence's own tokens. Anything unrecognized is treated
	// as the lowest confidence, so a malformed suggestion can never masquerade as a
	// strong one.
	Confidence string `json:"confidence"`
}
