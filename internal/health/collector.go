package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// DefaultEventLookback is the default window used to decide which warning
// events count as "recent": events whose last occurrence is older than this
// relative to the collection time are dropped. It is deliberately generous so
// no genuinely recent signal is missed; consumers wanting a tighter window can
// configure one.
const DefaultEventLookback = 1 * time.Hour

// Clock returns the current time. It is injected into a [Collector] so the
// otherwise time-varying [Snapshot.CollectedAt] field — and the event lookback
// cutoff derived from it — can be pinned in tests, making collection fully
// deterministic.
type Clock func() time.Time

// Collector turns the read-only signals exposed by a [kube.Client] into a
// structured [Snapshot]. It performs a pure read-and-transform: it issues only
// reads, assigns no severity, and detects no problems. Given fixed API
// responses and a fixed clock it produces an identical snapshot every time.
//
// A Collector is safe for concurrent use; it holds no mutable state.
type Collector struct {
	client *kube.Client

	// now supplies the collection timestamp and the basis for the event
	// lookback cutoff. It is never nil after construction.
	now Clock

	// eventLookback bounds which warning events are considered recent.
	eventLookback time.Duration
}

// Option configures a [Collector] at construction time.
type Option func(*Collector)

// WithClock overrides the clock used to stamp snapshots and compute the event
// lookback cutoff. It exists primarily so tests can pin time and assert exact,
// reproducible output. A nil clock is ignored.
func WithClock(c Clock) Option {
	return func(col *Collector) {
		if c != nil {
			col.now = c
		}
	}
}

// WithEventLookback overrides the window used to decide which warning events
// count as recent. A non-positive duration is ignored, leaving the default in
// place.
func WithEventLookback(d time.Duration) Option {
	return func(col *Collector) {
		if d > 0 {
			col.eventLookback = d
		}
	}
}

// NewCollector builds a [Collector] for the given read-only client. By default
// it stamps snapshots with the wall clock and treats warning events within
// [DefaultEventLookback] as recent; both are overridable via options.
func NewCollector(client *kube.Client, opts ...Option) *Collector {
	c := &Collector{
		client:        client,
		now:           time.Now,
		eventLookback: DefaultEventLookback,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Collect gathers the cluster's current health signals into a single
// [Snapshot]. It first probes reachability; if the API server does not answer,
// it returns a snapshot recording the unreachability with empty signal slices
// and a nil error — an unreachable cluster is a fact to record, not a failure
// of collection.
//
// When the cluster is reachable, Collect reads nodes, pods, deployments,
// replica sets, and events across all namespaces, transforms each into its
// typed signal, and sorts every slice by a stable key so the result is
// deterministic. If any read fails after the cluster was found reachable (for
// example a transient error mid-collection), Collect returns the wrapped
// underlying error so the caller can decide how to react.
func (c *Collector) Collect(ctx context.Context) (Snapshot, error) {
	collectedAt := c.now()
	snap := Snapshot{
		Cluster:     c.client.Name(),
		CollectedAt: collectedAt,
	}

	info, err := c.client.CheckReachable(ctx)
	if err != nil {
		// Unreachability is a recorded signal, not a collection error. The
		// signal slices stay empty because there was nothing to read.
		snap.Reachability = Reachability{Reachable: false, Error: err.Error()}
		return snap, nil
	}
	snap.Reachability = Reachability{Reachable: true, ServerVersion: info.GitVersion}

	nodes, err := c.client.ListNodes(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("collecting nodes: %w", err)
	}
	snap.Nodes = nodeSignals(nodes)

	pods, err := c.client.ListPods(ctx, "")
	if err != nil {
		return Snapshot{}, fmt.Errorf("collecting pods: %w", err)
	}
	snap.Pods = podSignals(pods)

	deployments, err := c.client.ListDeployments(ctx, "")
	if err != nil {
		return Snapshot{}, fmt.Errorf("collecting deployments: %w", err)
	}
	snap.Deployments = deploymentSignals(deployments)

	replicaSets, err := c.client.ListReplicaSets(ctx, "")
	if err != nil {
		return Snapshot{}, fmt.Errorf("collecting replicasets: %w", err)
	}
	snap.ReplicaSets = replicaSetSignals(replicaSets)

	events, err := c.client.ListEvents(ctx, "")
	if err != nil {
		return Snapshot{}, fmt.Errorf("collecting events: %w", err)
	}
	snap.WarningEvents = warningEventSignals(events, collectedAt.Add(-c.eventLookback))

	return snap, nil
}

// nodeSignals transforms raw nodes into sorted [NodeSignal]s, reading each
// node's standard conditions verbatim.
func nodeSignals(nodes []corev1.Node) []NodeSignal {
	out := make([]NodeSignal, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		out = append(out, NodeSignal{
			Name:           n.Name,
			Ready:          nodeConditionTrue(n, corev1.NodeReady),
			MemoryPressure: nodeConditionTrue(n, corev1.NodeMemoryPressure),
			DiskPressure:   nodeConditionTrue(n, corev1.NodeDiskPressure),
			PIDPressure:    nodeConditionTrue(n, corev1.NodePIDPressure),
			Unschedulable:  n.Spec.Unschedulable,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nodeConditionTrue reports whether the node carries the given condition with
// status True. An absent condition (or any non-True status) reports false.
func nodeConditionTrue(n *corev1.Node, t corev1.NodeConditionType) bool {
	for i := range n.Status.Conditions {
		cond := &n.Status.Conditions[i]
		if cond.Type == t {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// crashLoopMinRestarts is the per-container restart count at or above which a
// container that has most recently exited with an error is treated as
// crashlooping even when it is caught between backoff windows.
const crashLoopMinRestarts int32 = 2

// containerCrashLooping reports whether a container status represents a
// crashlooping container.
//
// A crashlooping container oscillates: the kubelet parks it in
// Waiting/CrashLoopBackOff, restarts it (briefly Running), it exits again
// (briefly Terminated), then back to CrashLoopBackOff. Keying solely on the
// instantaneous Waiting/CrashLoopBackOff state makes detection a coin flip for a
// point-in-time scan — the scan is just as likely to catch the container
// mid-restart, miss it, and under-report a genuinely failing pod. So we also
// treat a container that has restarted repeatedly and most recently terminated
// with a non-zero exit code as crashlooping.
func containerCrashLooping(cs *corev1.ContainerStatus) bool {
	if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
		return true
	}
	if cs.RestartCount >= crashLoopMinRestarts {
		if t := cs.LastTerminationState.Terminated; t != nil && t.ExitCode != 0 {
			return true
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return true
		}
	}
	return false
}

// podSignals transforms raw pods into sorted [PodSignal]s, deriving restart
// counts and crashloop facts from container statuses.
func podSignals(pods []corev1.Pod) []PodSignal {
	out := make([]PodSignal, 0, len(pods))
	for i := range pods {
		p := &pods[i]
		phase := string(p.Status.Phase)

		var restarts int32
		var crashlooping []string
		statuses := make([]corev1.ContainerStatus, 0,
			len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
		statuses = append(statuses, p.Status.InitContainerStatuses...)
		statuses = append(statuses, p.Status.ContainerStatuses...)
		for j := range statuses {
			cs := &statuses[j]
			restarts += cs.RestartCount
			if containerCrashLooping(cs) {
				crashlooping = append(crashlooping, cs.Name)
			}
		}
		sort.Strings(crashlooping)

		out = append(out, PodSignal{
			Namespace:              p.Namespace,
			Name:                   p.Name,
			Phase:                  phase,
			Reason:                 p.Status.Reason,
			RestartCount:           restarts,
			CrashLoopingContainers: crashlooping,
			Pending:                p.Status.Phase == corev1.PodPending,
			Failed:                 p.Status.Phase == corev1.PodFailed,
		})
	}
	sortByNamespaceName(out, func(s PodSignal) (string, string) { return s.Namespace, s.Name })
	return out
}

// deploymentSignals transforms raw deployments into sorted [DeploymentSignal]s.
func deploymentSignals(deployments []appsv1.Deployment) []DeploymentSignal {
	out := make([]DeploymentSignal, 0, len(deployments))
	for i := range deployments {
		d := &deployments[i]
		out = append(out, DeploymentSignal{
			Namespace:         d.Namespace,
			Name:              d.Name,
			DesiredReplicas:   desiredReplicas(d.Spec.Replicas),
			ReadyReplicas:     d.Status.ReadyReplicas,
			AvailableReplicas: d.Status.AvailableReplicas,
			UpdatedReplicas:   d.Status.UpdatedReplicas,
		})
	}
	sortByNamespaceName(out, func(s DeploymentSignal) (string, string) { return s.Namespace, s.Name })
	return out
}

// replicaSetSignals transforms raw replica sets into sorted [ReplicaSetSignal]s.
func replicaSetSignals(replicaSets []appsv1.ReplicaSet) []ReplicaSetSignal {
	out := make([]ReplicaSetSignal, 0, len(replicaSets))
	for i := range replicaSets {
		r := &replicaSets[i]
		out = append(out, ReplicaSetSignal{
			Namespace:         r.Namespace,
			Name:              r.Name,
			DesiredReplicas:   desiredReplicas(r.Spec.Replicas),
			ReadyReplicas:     r.Status.ReadyReplicas,
			AvailableReplicas: r.Status.AvailableReplicas,
		})
	}
	sortByNamespaceName(out, func(s ReplicaSetSignal) (string, string) { return s.Namespace, s.Name })
	return out
}

// desiredReplicas resolves a workload's spec replica count, applying
// Kubernetes' own default of 1 when the field is unset (nil).
func desiredReplicas(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// warningEventSignals filters events down to warnings at or after the cutoff,
// transforms them into sorted [EventSignal]s, and returns them ordered by last
// occurrence (most recent first) then by namespace/name for stability.
//
// An event's "last seen" time is the most recent of its modern (eventTime /
// series) and legacy (lastTimestamp / firstTimestamp) timestamps, so events
// recorded by either the old or new Events API are handled uniformly.
func warningEventSignals(events []corev1.Event, cutoff time.Time) []EventSignal {
	out := make([]EventSignal, 0)
	for i := range events {
		e := &events[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		lastSeen := eventLastSeen(e)
		if lastSeen.Before(cutoff) {
			continue
		}
		out = append(out, EventSignal{
			Namespace:      e.Namespace,
			Name:           e.Name,
			Reason:         e.Reason,
			Message:        e.Message,
			Count:          eventCount(e),
			InvolvedObject: involvedObjectRef(e.InvolvedObject),
			LastSeen:       lastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// eventLastSeen returns the most recent timestamp recorded on an event,
// reconciling the legacy (lastTimestamp/firstTimestamp) and modern
// (eventTime/series.lastObservedTime) representations.
func eventLastSeen(e *corev1.Event) time.Time {
	var t time.Time
	consider := func(c time.Time) {
		if c.After(t) {
			t = c
		}
	}
	consider(e.LastTimestamp.Time)
	consider(e.FirstTimestamp.Time)
	consider(e.EventTime.Time)
	if e.Series != nil {
		consider(e.Series.LastObservedTime.Time)
	}
	return t
}

// eventCount returns how many times the underlying occurrence was observed,
// preferring the modern series count and falling back to the legacy count.
func eventCount(e *corev1.Event) int32 {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	return e.Count
}

// involvedObjectRef renders an object reference as "Kind/namespace/name",
// omitting the namespace segment for cluster-scoped objects.
func involvedObjectRef(ref corev1.ObjectReference) string {
	parts := make([]string, 0, 3)
	parts = append(parts, ref.Kind)
	if ref.Namespace != "" {
		parts = append(parts, ref.Namespace)
	}
	parts = append(parts, ref.Name)
	return strings.Join(parts, "/")
}

// sortByNamespaceName sorts a slice of signals by (namespace, name) using the
// supplied key extractor, giving every workload-style signal slice a single,
// stable ordering rule.
func sortByNamespaceName[T any](items []T, key func(T) (namespace, name string)) {
	sort.Slice(items, func(i, j int) bool {
		ni, nmi := key(items[i])
		nj, nmj := key(items[j])
		if ni != nj {
			return ni < nj
		}
		return nmi < nmj
	})
}
