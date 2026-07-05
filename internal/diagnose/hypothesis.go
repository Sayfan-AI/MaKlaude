// Package diagnose turns each [correlate.Incident] into a small set of ranked,
// evidence-backed root-cause [Hypothesis]es, using deterministic rules only.
//
// It is the third interpretive layer above collection. The health package
// gathers raw facts; the detect package turns each fact into an independent,
// severity-ranked finding; the correlate package groups those findings into
// incidents — a single suspected-cause primary plus its likely downstream
// effects. But an incident still only says *what goes with what*: it names the
// most structural object as the primary, without committing to *why* the
// cascade happened. This package takes that final step. Given an incident whose
// primary is an unavailable Deployment, it decides whether the underlying cause
// is a bad image, an OOM-killing memory limit, insufficient cluster capacity, a
// failed node, or something it cannot classify — and it says so explicitly, as a
// ranked hypothesis that cites the exact findings supporting it.
//
// Why a distinct layer, and why deterministic? The same reasons detect and
// correlate give. A reproducible, side-effect-free diagnosis can be unit-tested
// for exact output and trusted as the stable substrate a later, fuzzier layer
// builds on. [Diagnose] is a pure function: it reads only the incident and the
// snapshot it is handed, performs no I/O, holds no clock (every hypothesis
// inherits the incident's [correlate.Incident.DetectedAt], which itself inherits
// the snapshot's collection time), contacts no cluster, and — deliberately —
// invokes no LLM. Given the same inputs it always returns the same hypotheses,
// each with the same evidence in the same order, and the whole list in the same
// order.
//
// The rule layer covers the well-understood cascades a Kubernetes operator would
// recognise on sight, each keyed off structural signals already present in the
// snapshot rather than free-text guessing:
//
//   - Bad or unpullable image ([CauseBadImage]). A container stuck with a waiting
//     reason such as "ImagePullBackOff" or "ErrImagePull" cannot start, so its
//     ReplicaSet never becomes available and its Deployment goes unavailable.
//   - Insufficient resources ([CauseInsufficientResources]). A Pending pod whose
//     FailedScheduling event reports "Insufficient cpu"/"Insufficient memory"
//     cannot be placed until capacity frees up or is added.
//   - Node failure ([CauseNodeFailure]). A NotReady node disrupts every pod
//     scheduled onto it; the node is the cause and the pods are the victims.
//   - OOM kill ([CauseOOMKill]). A container terminated with reason "OOMKilled"
//     is being restarted (and may be crashlooping) because it exceeds its memory
//     limit.
//
// When no specialized rule matches, the incident is never dropped: it still
// yields a single generic [CauseUnknown] hypothesis naming the primary finding as
// the suspected cause, at [ConfidenceLow]. So every incident produces at least
// one hypothesis, and an operator always has something to act on.
//
// Ranking is deterministic and coarse. Each hypothesis carries a [Confidence]
// enum (not a computed float that could drift), and [Diagnose] returns them
// sorted by confidence descending, then by [HypothesisIdentity] ascending as a
// fully decisive tiebreak — so the output is byte-stable for a fixed input and
// independent of the order incidents are fed in.
//
// Stable identity is the load-bearing property, mirroring the layers below. A
// hypothesis's identity is derived solely from its [Cause] and the incident's
// (already stable, already cluster-scoped) identity, never from confidence,
// title, message, or evidence — all of which shift as an incident evolves. The
// same ongoing hypothesis therefore keeps one identity across collection cycles,
// so a downstream escalation layer can dedup it rather than re-alerting.
//
// The [Refiner] seam is where the optional fuzzy/LLM layer (Milestone 3 T5)
// plugs in WITHOUT entangling this deterministic core: it may re-rank, adjust,
// or add hypotheses after the rules run, and every hypothesis carries a [Source]
// marker so a consumer can always tell a deterministic result from a refined
// one. The default refiner is a no-op, so the deterministic core is fully usable
// on its own — even if the fuzzy layer is unavailable or distrusted, operators
// still get correct, auditable hypotheses.
package diagnose

import (
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// Confidence is the coarse, deterministic strength MaKlaude assigns to a
// [Hypothesis]. It is intentionally a small enum rather than a computed float:
// a float invites arithmetic that drifts between versions and breaks the
// byte-stability this package promises, whereas three named levels are stable,
// explainable to an operator, and trivially sortable. Higher numeric values mean
// more confident, so hypotheses can be ranked most-confident-first
// deterministically; the ordering is not exposed as arithmetic anywhere else.
type Confidence int

const (
	// ConfidenceLow is the strength of the generic fallback hypothesis: no
	// specialized rule matched, so the primary finding is offered as the
	// suspected cause without further commitment.
	ConfidenceLow Confidence = iota

	// ConfidenceMedium marks a well-understood cascade whose evidence is
	// suggestive but no longer current — for example a container that was
	// OOM-killed on a previous restart but is not presently terminated.
	ConfidenceMedium

	// ConfidenceHigh marks a well-understood cascade backed by a strong, current
	// structural signal — a bad-image waiting reason, an "Insufficient" scheduling
	// event, a NotReady node, or a container presently terminated OOMKilled.
	ConfidenceHigh
)

// String renders the confidence as a stable lowercase token ("low", "medium",
// "high"). The tokens are part of the package's contract: test fixtures and
// human-facing renderings rely on them, so they must not change casually.
func (c Confidence) String() string {
	switch c {
	case ConfidenceLow:
		return "low"
	case ConfidenceMedium:
		return "medium"
	case ConfidenceHigh:
		return "high"
	default:
		return fmt.Sprintf("confidence(%d)", int(c))
	}
}

// Cause classifies the kind of root cause a [Hypothesis] proposes. It is a
// small, stable string enum (rather than free text) so consumers can branch on
// it, dedup by it, and compose it into a stable [HypothesisIdentity]. The string
// values are lowercase and delimiter-free on purpose, so they slot cleanly into
// an identity key.
type Cause string

const (
	// CauseBadImage is a container image that cannot be pulled or is invalid,
	// leaving the workload unable to start (waiting reason "ImagePullBackOff",
	// "ErrImagePull", "InvalidImageName", and the like).
	CauseBadImage Cause = "badimage"

	// CauseInsufficientResources is a pod that cannot be scheduled because the
	// cluster lacks the cpu/memory it requests (a FailedScheduling event reporting
	// "Insufficient ...").
	CauseInsufficientResources Cause = "insufficientresources"

	// CauseNodeFailure is a NotReady node disrupting the pods scheduled onto it.
	CauseNodeFailure Cause = "nodefailure"

	// CauseOOMKill is a container terminated by the kernel out-of-memory killer
	// (termination reason "OOMKilled"), driving restarts and crashloops.
	CauseOOMKill Cause = "oomkill"

	// CauseUnknown is the fallback when no specialized rule matches: the incident's
	// primary finding is offered as the suspected cause. It guarantees every
	// incident yields at least one hypothesis.
	CauseUnknown Cause = "unknown"
)

// Source records where a [Hypothesis] came from. It is the visible edge of the
// [Refiner] seam: the deterministic rule layer stamps every hypothesis it
// produces [SourceDeterministic], and the optional fuzzy/LLM layer (T5) stamps
// anything it adds or rewrites [SourceRefined]. A consumer can therefore always
// distinguish a reproducible, auditable rule result from a fuzzier refinement —
// which matters for trust and for the audit trail.
type Source string

const (
	// SourceDeterministic marks a hypothesis produced by this package's
	// deterministic rule layer. It is reproducible and byte-stable for a fixed
	// input.
	SourceDeterministic Source = "deterministic"

	// SourceRefined marks a hypothesis produced or rewritten by an optional
	// [Refiner] (the fuzzy/LLM layer). It is NOT guaranteed to be reproducible.
	SourceRefined Source = "refined"
)

// HypothesisIdentity is the stable, deterministic key for a [Hypothesis]. It is
// what makes a proposed cause the SAME hypothesis across collection cycles: it
// is derived purely from the [Cause] and the incident's (already stable,
// already cluster-scoped) [correlate.IncidentIdentity]. It intentionally ignores
// confidence, title, message, and the changing set of evidence findings, so a
// hypothesis whose confidence or wording shifts — but whose cause and incident
// persist — keeps one identity, and a downstream escalation layer can dedup it
// rather than re-alerting each cycle.
//
// HypothesisIdentity is a comparable value (a plain string under the hood) so it
// can be used directly as a map key for deduplication.
type HypothesisIdentity string

// newHypothesisIdentity composes a hypothesis identity from its cause and the
// incident it explains. The "hypothesis" prefix keeps the key namespaced and
// self-describing (so it never collides with a finding or incident identity if
// the three are stored side by side), the cause distinguishes multiple
// hypotheses about one incident, and reusing the incident's already-stable,
// already-cluster-scoped identity means a hypothesis is stable and
// multi-cluster-safe for free.
func newHypothesisIdentity(cause Cause, incident correlate.IncidentIdentity) HypothesisIdentity {
	return HypothesisIdentity("hypothesis|" + string(cause) + "|" + string(incident))
}

// Hypothesis is a single, deterministic proposal for the root cause of one
// [correlate.Incident], together with the evidence that supports it and a coarse
// confidence. It is a plain value — it carries no behaviour and no live
// references — so it is trivially serializable and comparable, which the
// downstream escalation, audit-trail, and refinement layers rely on.
type Hypothesis struct {
	// Identity is the stable dedup key. The same ongoing hypothesis produces the
	// same Identity on every cycle. See [HypothesisIdentity].
	Identity HypothesisIdentity

	// Incident is the identity of the incident this hypothesis explains. It ties
	// the hypothesis back to its incident (and, transitively, to that incident's
	// findings) without embedding the whole incident.
	Incident correlate.IncidentIdentity

	// Cluster is the registered name of the cluster the hypothesis concerns,
	// carried through from the incident. A hypothesis is always scoped to one
	// cluster.
	Cluster string

	// Cause classifies the proposed root cause. See [Cause].
	Cause Cause

	// Confidence is how strongly the evidence supports this cause. Hypotheses are
	// ranked by it (descending) first. See [Confidence].
	Confidence Confidence

	// Title is a short, stable human-readable label for the class of cause (for
	// example "Node failure disrupting its pods"). It is suitable as an alert
	// subject line and is deliberately NOT part of the identity.
	Title string

	// Message is a fuller human-readable explanation, including the specific
	// signal that triggered the rule (for example which container is OOM-killed).
	// It may change as an incident evolves; it is deliberately NOT part of the
	// identity.
	Message string

	// Evidence is the subset of the incident's findings that support this
	// hypothesis, in the incident's own stable finding order. It is what makes a
	// hypothesis auditable: an operator can see exactly which observations led to
	// the proposed cause. The slice is a fresh copy; mutating it does not affect
	// the incident.
	Evidence []detect.Finding

	// Source records whether this hypothesis came from the deterministic rule
	// layer or from a later [Refiner]. See [Source].
	Source Source

	// DetectedAt is the incident's detection time, carried through from
	// [correlate.Incident.DetectedAt] (and thus from the snapshot). Hypotheses
	// never read their own clock, so diagnosis stays a pure function of its input.
	DetectedAt time.Time
}
