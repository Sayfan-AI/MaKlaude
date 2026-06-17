// Package health collects the core observability signals MaKlaude needs to
// reason about a cluster, and structures them into a typed, deterministic
// [Snapshot].
//
// This package is a pure read-and-transform layer over the read-only
// [kube.Client]. It is deliberately judgment-free: it gathers raw facts —
// which nodes are ready, which pods are restarting, what the API server
// reported — and arranges them into explicit, typed fields. It assigns no
// severity, raises no alerts, and detects no problems. Turning these facts
// into diagnoses is a separate, downstream concern; keeping collection pure
// means the same snapshot can feed many different analyses and can be tested
// for exact, reproducible output.
//
// Determinism is a first-class property. Given a fixed set of API responses, a
// [Collector] always produces the same [Snapshot]: every slice is sorted by a
// stable key, so two collections of an unchanged cluster are byte-for-byte
// comparable. The only intentionally non-deterministic field is the collection
// timestamp, which the collector takes from an injectable clock so tests can
// pin it.
//
// A snapshot is always scoped to exactly one cluster (named in [Snapshot]) and
// is built entirely through the read-only client, so collecting health can
// never mutate a cluster and one cluster's collection can never affect
// another.
package health

import "time"

// Snapshot is a structured, point-in-time view of one cluster's health
// signals. It is the typed output of a [Collector]: a faithful, judgment-free
// record of what the cluster reported at [Snapshot.CollectedAt].
//
// Every field is explicit and typed — there are no opaque blobs — so consumers
// can rely on the shape without re-parsing strings. All slices are sorted by a
// stable key, making two snapshots of an unchanged cluster directly
// comparable.
type Snapshot struct {
	// Cluster is the registered name of the cluster this snapshot describes.
	// A snapshot is always scoped to a single cluster.
	Cluster string

	// CollectedAt is the instant the collection ran, taken from the collector's
	// clock. It is the only intentionally time-varying field.
	CollectedAt time.Time

	// Reachability records whether the cluster's API server answered, and (on
	// success) the version it reported. When the API server is unreachable the
	// rest of the snapshot's signal slices are empty: there was nothing to read.
	Reachability Reachability

	// Nodes holds one entry per node, sorted by name, capturing readiness and
	// the standard node pressure conditions.
	Nodes []NodeSignal

	// Pods holds one entry per pod, sorted by namespace then name, capturing
	// phase and per-container restart/crashloop facts.
	Pods []PodSignal

	// Deployments holds one entry per deployment, sorted by namespace then name,
	// capturing desired vs. ready/available/updated replica counts.
	Deployments []DeploymentSignal

	// ReplicaSets holds one entry per replica set, sorted by namespace then name,
	// capturing desired vs. ready/available replica counts.
	ReplicaSets []ReplicaSetSignal

	// WarningEvents holds the recent warning-type events, sorted by last
	// occurrence then by a stable key. "Recent" is bounded by the collector's
	// configured lookback window relative to [Snapshot.CollectedAt].
	WarningEvents []EventSignal
}

// Reachability captures whether the cluster's control plane answered a
// lightweight read at collection time. It records a plain fact: reachable or
// not, with the reported version on success and the error text on failure. It
// carries no notion of severity.
type Reachability struct {
	// Reachable is true if the API server responded to the version probe.
	Reachable bool

	// ServerVersion is the git version string the API server reported (for
	// example "v1.30.0"). It is empty when the cluster was unreachable.
	ServerVersion string

	// Error is the human-readable text of the failure when the cluster was
	// unreachable; it is empty on success. It is stored as a string rather than
	// an error so a snapshot is a plain, serializable value.
	Error string
}

// NodeSignal captures the readiness and pressure conditions of a single node.
// The fields mirror the node's reported conditions verbatim; no node is judged
// healthy or unhealthy here.
type NodeSignal struct {
	// Name is the node's name; node signals are sorted by it.
	Name string

	// Ready reflects the node's Ready condition: true only when the condition is
	// explicitly present and True. A node whose Ready condition is False,
	// Unknown, or absent reports false.
	Ready bool

	// MemoryPressure, DiskPressure, and PIDPressure reflect the node's
	// corresponding pressure conditions, true only when the condition is present
	// and True.
	MemoryPressure bool
	DiskPressure   bool
	PIDPressure    bool

	// Unschedulable reflects the node's spec: true when the node has been
	// cordoned.
	Unschedulable bool
}

// PodSignal captures the lifecycle phase and per-container restart facts of a
// single pod. The crashloop and restart-count fields are derived directly from
// container statuses; they are raw structured facts, not assessments.
type PodSignal struct {
	// Namespace and Name identify the pod; pod signals are sorted by namespace
	// then name.
	Namespace string
	Name      string

	// Phase is the pod's lifecycle phase as reported by the API server (for
	// example "Running", "Pending", "Failed").
	Phase string

	// Reason is the pod-level reason string when set (for example "Evicted"),
	// otherwise empty.
	Reason string

	// RestartCount is the sum of RestartCount across all of the pod's containers
	// (both regular and init containers).
	RestartCount int32

	// CrashLoopingContainers lists, sorted by name, the containers currently in
	// a waiting state with reason "CrashLoopBackOff".
	CrashLoopingContainers []string

	// Pending is true when the pod's phase is "Pending".
	Pending bool

	// Failed is true when the pod's phase is "Failed".
	Failed bool
}

// DeploymentSignal captures the desired and observed replica counts of a single
// deployment, as reported in its spec and status.
type DeploymentSignal struct {
	// Namespace and Name identify the deployment; signals are sorted by
	// namespace then name.
	Namespace string
	Name      string

	// DesiredReplicas is the spec's replica count (defaulting to 1 when the spec
	// leaves it unset, matching Kubernetes' own default).
	DesiredReplicas int32

	// ReadyReplicas, AvailableReplicas, and UpdatedReplicas mirror the
	// deployment's status counts verbatim.
	ReadyReplicas     int32
	AvailableReplicas int32
	UpdatedReplicas   int32
}

// ReplicaSetSignal captures the desired and observed replica counts of a single
// replica set, as reported in its spec and status.
type ReplicaSetSignal struct {
	// Namespace and Name identify the replica set; signals are sorted by
	// namespace then name.
	Namespace string
	Name      string

	// DesiredReplicas is the spec's replica count (defaulting to 1 when unset,
	// matching Kubernetes' own default).
	DesiredReplicas int32

	// ReadyReplicas and AvailableReplicas mirror the replica set's status counts
	// verbatim.
	ReadyReplicas     int32
	AvailableReplicas int32
}

// EventSignal captures a single recent warning event in a compact, typed form.
// It surfaces the cluster's own narration of a noteworthy occurrence without
// interpreting it.
type EventSignal struct {
	// Namespace and Name are the event object's own namespace and name.
	Namespace string
	Name      string

	// Reason is the short machine reason (for example "FailedScheduling",
	// "BackOff", "Failed").
	Reason string

	// Message is the human-readable detail the cluster recorded.
	Message string

	// Count is how many times the underlying occurrence has been observed and
	// coalesced into this event.
	Count int32

	// InvolvedObject identifies what the event is about, formatted as
	// "Kind/namespace/name" (namespace omitted for cluster-scoped objects).
	InvolvedObject string

	// LastSeen is the most recent time the occurrence was observed. Warning
	// events are sorted by it (then by namespace/name) for a stable order.
	LastSeen time.Time
}
