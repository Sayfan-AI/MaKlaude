// Package detect turns a judgment-free [health.Snapshot] into discrete,
// severity-ranked [Finding]s using deterministic rules only.
//
// It is the first interpretive layer above collection: where the health
// package gathers raw facts ("this node's Ready condition is False", "this pod
// has a container in CrashLoopBackOff"), this package decides what those facts
// mean ("node node-a is NotReady — critical"). The split is intentional.
// Collection stays pure and reusable; this package owns the opinions, and only
// the opinions — it reads a snapshot value and nothing else. It performs no
// I/O, holds no clock, and never touches a cluster. Like collection it is
// READ-ONLY in spirit: it inspects a value and emits findings, it never
// remediates.
//
// Why a separate package rather than another file under health? The boundary
// is conceptual, not just physical: health is verifiably judgment-free (a
// property its package doc promises and its tests guard), while detect is
// nothing but judgment. Keeping them apart lets each be reasoned about and
// evolved independently — a future LLM-backed diagnosis layer (a later
// milestone) can sit beside this deterministic one, both consuming the same
// snapshot, without entangling either with collection.
//
// Determinism is a first-class property, mirroring the health package. Given a
// fixed snapshot, [Analyze] always returns the same findings in the same
// order: the result is sorted by (severity descending, then identity), so two
// analyses of an unchanged cluster are directly comparable and an operator (or
// a downstream escalation step) sees stable output. The findings carry no
// wall-clock time of their own — every finding inherits the snapshot's
// [health.Snapshot.CollectedAt], so analysis is a pure function of its input.
//
// Stable identity is the other load-bearing property. Each finding has an
// [Identity] derived deterministically from the kind of problem plus the
// object it concerns (cluster / namespace / name). The SAME ongoing problem
// therefore yields the SAME identity on every collection cycle, so downstream
// escalation can dedup an active issue instead of re-alerting each cycle. The
// identity is intentionally independent of severity and free-text message, so
// a problem that worsens (or whose wording changes) keeps its identity.
package detect

import (
	"fmt"
	"strings"
	"time"
)

// Severity ranks how much operator attention a [Finding] warrants. The three
// levels are deliberately coarse — richer scoring is a downstream concern — and
// are ordered so that higher numeric values mean more urgent. The ordering
// exists so findings can be sorted most-urgent-first deterministically; it is
// not exposed as arithmetic anywhere else.
type Severity int

const (
	// SeverityInfo marks a noteworthy but non-actionable observation: something
	// an operator might want in the audit trail but that needs no response on
	// its own (for example a surfaced warning event that does not already map to
	// a more specific workload finding).
	SeverityInfo Severity = iota

	// SeverityWarning marks a degraded-but-not-down condition: the cluster is
	// still serving, but something is unhealthy and likely to worsen if ignored
	// (for example a deployment running below its desired replica count, or a
	// node reporting memory pressure).
	SeverityWarning

	// SeverityCritical marks a condition that is down or actively failing and
	// warrants immediate attention (for example an unreachable API server, a
	// NotReady node, or a crashlooping pod).
	SeverityCritical
)

// String renders the severity as a stable lowercase token ("info", "warning",
// "critical"). The tokens are part of the package's contract: identities and
// test fixtures rely on them, so they must not change casually.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Object identifies the Kubernetes object a [Finding] concerns. It is a small,
// typed value rather than a free-form string so consumers can group, filter, or
// render findings by object without re-parsing. Cluster-scoped objects (nodes,
// or the cluster itself) leave Namespace empty.
type Object struct {
	// Kind is the object's kind in lowercase, stable form ("cluster", "node",
	// "pod", "deployment", "replicaset", "event"). Lowercase is chosen so the
	// kind composes cleanly into the stable [Identity] key.
	Kind string

	// Namespace is the object's namespace, or empty for cluster-scoped objects
	// (and for the synthetic "cluster" object).
	Namespace string

	// Name is the object's name. For the synthetic "cluster" object this is the
	// cluster's registered name.
	Name string
}

// String renders the object as "kind/namespace/name", omitting the namespace
// segment for cluster-scoped objects. This is the same compact form the health
// package uses for event involved-objects, kept consistent on purpose.
func (o Object) String() string {
	if o.Namespace == "" {
		return o.Kind + "/" + o.Name
	}
	return o.Kind + "/" + o.Namespace + "/" + o.Name
}

// Identity is the stable, deterministic key for a [Finding]. It is what makes a
// problem the SAME problem across collection cycles: it is derived purely from
// the cluster, the rule that fired, and the object involved — never from the
// severity, the message, or any timestamp. So a problem that persists,
// worsens, or has its wording tweaked keeps one identity, and downstream
// escalation (a later task) can dedup an active issue rather than re-alerting
// every cycle.
//
// Identity is a comparable value (a plain string under the hood) so it can be
// used directly as a map key for deduplication.
type Identity string

// newIdentity composes a stable identity from the cluster, a rule key, and the
// involved object. The cluster is included so identities never collide across
// clusters (multi-cluster is a first-class concern); the rule key distinguishes
// different problems about the same object (for example a node that is both
// NotReady and under memory pressure yields two distinct identities). The
// segments are joined with a delimiter unlikely to appear in Kubernetes names.
func newIdentity(cluster, rule string, obj Object) Identity {
	return Identity(strings.Join([]string{cluster, rule, obj.String()}, "|"))
}

// Finding is a single, deterministic interpretation of a fact in a
// [health.Snapshot]: a named problem, its severity, the object it concerns, and
// a human-readable explanation. A finding is a plain value — it carries no
// behaviour and no live references — so it is trivially serializable and
// comparable, which downstream escalation and audit-trail layers rely on.
type Finding struct {
	// Identity is the stable dedup key. The same ongoing problem produces the
	// same Identity on every cycle. See [Identity].
	Identity Identity

	// Severity is how urgent the finding is. Findings are sorted by it
	// (descending) first.
	Severity Severity

	// Cluster is the registered name of the cluster the finding concerns,
	// carried through from [health.Snapshot.Cluster]. A finding is always scoped
	// to one cluster.
	Cluster string

	// Object is the Kubernetes object the finding is about (the cluster itself
	// for cluster-wide problems such as an unreachable API server).
	Object Object

	// Title is a short, stable human-readable label for the class of problem
	// (for example "Node NotReady", "Pod crashlooping"). It is suitable as an
	// alert subject line.
	Title string

	// Message is a fuller human-readable explanation including the specific
	// figures behind the finding (for example "2/3 replicas available"). It may
	// change as a problem evolves; it is deliberately NOT part of the identity.
	Message string

	// DetectedAt is the snapshot's collection time, carried through from
	// [health.Snapshot.CollectedAt]. Findings never read their own clock, so
	// analysis stays a pure function of its input.
	DetectedAt time.Time
}
