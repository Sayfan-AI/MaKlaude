// Package correlate turns a flat, deterministic list of [detect.Finding]s into
// a smaller set of [Incident]s by grouping findings that are almost certainly
// facets of the same underlying problem.
//
// It is the second interpretive layer above collection. The health package
// gathers raw facts; the detect package turns each fact into an independent,
// severity-ranked finding ("node node-a is NotReady", "pod web-x is
// crashlooping", "deployment web has 0/3 replicas"). But a real cluster failure
// almost never announces itself as one finding — a single root cause radiates
// into a fan of downstream symptoms. A bad image reference shows up as a
// crashlooping pod AND a stalled ReplicaSet AND an unavailable Deployment; a
// node going NotReady shows up as that node's finding AND every pod stranded on
// it. Presenting an operator ten findings for what is really one incident buries
// the signal and, worse, invites ten separate remediations for a single cause.
// This package collapses that fan back into incidents so the layers above
// reason about (and, later, escalate or remediate) one issue at a time.
//
// Like detect, correlation is pure and judgment-light. [Correlate] reads only
// the snapshot and the findings it is handed: it performs no I/O, holds no
// clock, contacts no cluster, and — deliberately — invokes no LLM. Every grouping
// decision comes from a deterministic *structural* signal that is already
// present in the snapshot:
//
//   - Owner references. A pod carries the ownerReferences the health collector
//     captured ([health.PodSignal.Owners]); a ReplicaSet created by a Deployment
//     is named "<deployment>-<hash>". So a failing pod can be walked back to its
//     ReplicaSet's finding and, by that naming convention, to its Deployment's
//     finding, folding the whole workload cascade into one incident.
//   - Node placement. A pod records the node it was scheduled onto
//     ([health.PodSignal.Node]); a pod on a node that itself has a finding
//     (NotReady, under pressure, cordoned) is very likely a victim of that node,
//     so its findings correlate with the node's.
//   - Namespace and same-snapshot co-occurrence, used only as *guards*, never as
//     standalone grouping edges. Owner matching is scoped to a single namespace
//     (a ReplicaSet only adopts a Deployment in its own namespace), and because
//     [Correlate] only ever relates findings that appear together in one
//     snapshot, co-occurrence is intrinsic. These weaker signals are deliberately
//     NOT used to merge everything sharing a namespace: that would fuse unrelated
//     problems and lose exactly the resolution this package exists to provide.
//
// Why deterministic-only, and why a separate package? The same reasons detect
// gives. A reproducible, side-effect-free grouping can be unit-tested for exact
// output and trusted as the stable substrate a later, fuzzier layer builds on.
// The intended consumer is the Milestone 3 T3 hypothesis/root-cause step: it
// takes these already-grouped incidents — each a primary suspected cause plus its
// likely effects — and reasons about *why*, without having to first rediscover
// *what goes with what*. Keeping the structural grouping here, deterministic and
// LLM-free, means that even if the hypothesis layer is unavailable or distrusted,
// operators still get correct, auditable incidents.
//
// Determinism is a first-class property. Given a fixed snapshot and a fixed list
// of findings, [Correlate] always returns the same incidents, each with the same
// primary, the same effects in the same order, and the whole list in the same
// order. Incidents carry no wall-clock of their own: each inherits the snapshot's
// [health.Snapshot.CollectedAt], exactly as findings inherit it, so correlation
// stays a pure function of its input.
//
// Stable identity is the other load-bearing property, mirroring [detect.Identity].
// An incident's identity is derived solely from its primary finding's (already
// cluster-scoped, already stable) identity — never from severity, message,
// timestamps, or the set of effects, all of which shift as an incident evolves.
// The same ongoing incident therefore keeps one identity across collection
// cycles, so a downstream escalation layer can dedup an active incident instead
// of re-alerting every cycle, even as symptoms come and go around a persistent
// root cause.
package correlate

import (
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// IncidentIdentity is the stable, deterministic key for an [Incident]. It is
// what makes an incident the SAME incident across collection cycles: it is
// derived purely from the primary finding's [detect.Identity], which is itself
// cluster-scoped and stable. It intentionally ignores severity, messages,
// timestamps, and the changing set of effect findings, so an incident whose
// symptoms come and go — but whose root cause persists — keeps one identity, and
// a downstream escalation layer can dedup it rather than re-alerting each cycle.
//
// IncidentIdentity is a comparable value (a plain string under the hood) so it
// can be used directly as a map key for deduplication.
type IncidentIdentity string

// newIncidentIdentity composes an incident identity from its primary finding's
// identity. The "incident" prefix keeps the key namespaced and self-describing
// (so it never collides with a bare finding identity if the two are ever stored
// side by side), and reusing the primary's already-stable, already-cluster-scoped
// identity means an incident is stable and multi-cluster-safe for free.
func newIncidentIdentity(primary detect.Identity) IncidentIdentity {
	return IncidentIdentity("incident|" + string(primary))
}

// Incident groups findings that are, deterministically and structurally, facets
// of one underlying problem: a single [Incident.Primary] suspected cause plus
// the findings judged to be its downstream effects. It is a plain value — no
// behaviour, no live references — so it is trivially serializable and comparable,
// which the downstream escalation, audit-trail, and hypothesis layers rely on.
//
// A finding that correlates with nothing becomes a singleton incident: itself as
// primary, no effects. Every input finding appears in exactly one incident —
// correlation partitions the findings, it never drops one.
type Incident struct {
	// Identity is the stable dedup key. The same ongoing incident produces the
	// same Identity on every cycle. See [IncidentIdentity].
	Identity IncidentIdentity

	// Cluster is the registered name of the cluster the incident concerns,
	// carried through from the findings (which are each scoped to one cluster). An
	// incident never spans clusters: findings from different clusters have
	// different, cluster-scoped identities and never correlate.
	Cluster string

	// Primary is the finding selected as the most likely root cause of the group:
	// the most structural (a NotReady node, an unavailable deployment) over its
	// symptoms. It is chosen deterministically — see [Correlate] for the exact
	// rule — so it is stable across cycles for an unchanged cascade.
	Primary detect.Finding

	// Effects are the remaining findings in the group — the primary's likely
	// downstream symptoms — sorted by finding identity for a stable order. It is
	// empty for a singleton incident.
	Effects []detect.Finding

	// DetectedAt is the snapshot's collection time, carried through from
	// [health.Snapshot.CollectedAt] (via the findings). Incidents never read their
	// own clock, so correlation stays a pure function of its input.
	DetectedAt time.Time
}

// Severity reports the incident's severity, defined as its primary finding's
// severity. Incidents are ranked most-urgent-first by this value, and it is the
// natural severity to surface to an operator: the severity of the thing most
// likely to be the cause.
func (in Incident) Severity() detect.Severity {
	return in.Primary.Severity
}

// Findings returns every finding in the incident — the primary followed by its
// effects — as a fresh slice. It is a convenience for consumers (and tests) that
// need to iterate or count all findings in an incident without special-casing
// the primary. The returned slice is a copy; mutating it does not affect the
// incident.
func (in Incident) Findings() []detect.Finding {
	out := make([]detect.Finding, 0, 1+len(in.Effects))
	out = append(out, in.Primary)
	out = append(out, in.Effects...)
	return out
}
