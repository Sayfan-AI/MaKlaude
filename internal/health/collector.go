package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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

// DefaultLogTailLines is the default number of trailing log lines
// [Collector.CollectPodLogs] requests per container. It bounds the read so
// gathering evidence for a single implicated pod can never pull an unbounded
// volume of logs.
const DefaultLogTailLines int64 = 50

// LogOptions configures a single [Collector.CollectPodLogs] call.
type LogOptions struct {
	// TailLines bounds how many trailing lines are read per container. A
	// non-positive value falls back to [DefaultLogTailLines].
	TailLines int64

	// Containers restricts which of the pod's containers to gather logs for. An
	// empty slice means all of the pod's containers (init and regular).
	Containers []string
}

// PodLogEvidence is the bounded log evidence gathered for a single implicated
// pod. It is deliberately scoped to one named pod: logs are fetched lazily, only
// for pods already implicated in a finding, never cluster-wide and never during
// the eager [Collector.Collect].
type PodLogEvidence struct {
	// Namespace and Name identify the pod the evidence was gathered for.
	Namespace string
	Name      string

	// Containers holds the per-container log reads, ordered deterministically by
	// container name and, within a container, current logs before previous-instance
	// logs.
	Containers []ContainerLogEvidence
}

// ContainerLogEvidence is the bounded log read for one container instance.
type ContainerLogEvidence struct {
	// Container is the container the logs came from.
	Container string

	// Previous is true when these are the logs of the previous (crashed) instance
	// of the container rather than the current one — the key evidence for a
	// crashlooping container that has already been restarted.
	Previous bool

	// TailLines is the tail bound that was applied to this read.
	TailLines int64

	// Logs is the bounded log content that was read. It is empty when the read
	// produced no output or failed (see Error).
	Logs string

	// Error is the text of a per-container fetch failure, if any, otherwise empty.
	// A failure to read one container's logs (for example, previous-instance logs
	// that do not exist) is recorded as a fact rather than failing the whole
	// collection, mirroring how unreachability is recorded elsewhere.
	Error string
}

// CollectPodLogs gathers bounded, recent logs for a single named pod that a
// downstream diagnosis step has already implicated. It is the lazy counterpart to
// [Collector.Collect]: it is never called cluster-wide, and Collect never fetches
// logs itself. Fetching logs is a GET on the pods/log subresource, which the
// read-only client permits and which is the one read requiring the pods/log RBAC
// grant.
//
// It reads the pod once to enumerate its containers and detect which are
// crashlooping, then, for each targeted container, reads the current logs bounded
// to opts.TailLines (defaulting to [DefaultLogTailLines]); for a crashlooping
// container it additionally reads the previous instance's logs. Per-container read
// failures are recorded on the entry rather than aborting the collection. The
// returned evidence is deterministic: containers are ordered by name and, within a
// container, current logs precede previous logs.
func (c *Collector) CollectPodLogs(ctx context.Context, namespace, podName string, opts LogOptions) (PodLogEvidence, error) {
	tail := opts.TailLines
	if tail <= 0 {
		tail = DefaultLogTailLines
	}

	pod, err := c.client.GetPod(ctx, namespace, podName)
	if err != nil {
		return PodLogEvidence{}, fmt.Errorf("collecting logs for pod %s/%s: %w", namespace, podName, err)
	}

	// Which containers is the caller interested in? An empty selection means all.
	var wanted map[string]bool
	if len(opts.Containers) > 0 {
		wanted = make(map[string]bool, len(opts.Containers))
		for _, name := range opts.Containers {
			wanted[name] = true
		}
	}

	// Crashloop is read from status; the set of containers is read from spec, so a
	// pod that has not produced statuses yet is still enumerated.
	crashlooping := crashLoopingSet(pod)
	names := containerNames(pod)

	ev := PodLogEvidence{Namespace: namespace, Name: podName}
	for _, name := range names {
		if wanted != nil && !wanted[name] {
			continue
		}
		ev.Containers = append(ev.Containers, c.readContainerLog(ctx, namespace, podName, name, false, tail))
		if crashlooping[name] {
			ev.Containers = append(ev.Containers, c.readContainerLog(ctx, namespace, podName, name, true, tail))
		}
	}
	return ev, nil
}

// readContainerLog performs one bounded log read for a container instance,
// capturing a failure as a fact on the returned entry rather than propagating it.
func (c *Collector) readContainerLog(ctx context.Context, namespace, pod, container string, previous bool, tail int64) ContainerLogEvidence {
	entry := ContainerLogEvidence{Container: container, Previous: previous, TailLines: tail}
	data, err := c.client.PodLogs(ctx, namespace, pod, container, previous, tail)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Logs = string(data)
	return entry
}

// crashLoopingSet returns the set of container names in the pod currently
// considered crashlooping, using the same oscillation-robust rule as the rest of
// the collector.
func crashLoopingSet(p *corev1.Pod) map[string]bool {
	out := make(map[string]bool)
	mark := func(statuses []corev1.ContainerStatus) {
		for i := range statuses {
			if containerCrashLooping(&statuses[i]) {
				out[statuses[i].Name] = true
			}
		}
	}
	mark(p.Status.InitContainerStatuses)
	mark(p.Status.ContainerStatuses)
	return out
}

// containerNames returns the pod's container names (init and regular) sorted for
// a deterministic iteration order.
func containerNames(p *corev1.Pod) []string {
	names := make([]string, 0, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for i := range p.Spec.InitContainers {
		names = append(names, p.Spec.InitContainers[i].Name)
	}
	for i := range p.Spec.Containers {
		names = append(names, p.Spec.Containers[i].Name)
	}
	sort.Strings(names)
	return names
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
			Allocatable:    resourceListFrom(n.Status.Allocatable),
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

		// Requests come from the pod spec (keyed by container name); crashloop,
		// restart, waiting, and termination facts come from the pod status. Names
		// are unique across a pod's init and regular containers, so a single map
		// correlates the two.
		requestsByName := containerRequestsByName(p)

		var restarts int32
		var crashlooping []string
		var containers []ContainerSignal
		appendContainers := func(statuses []corev1.ContainerStatus, init bool) {
			for j := range statuses {
				cs := &statuses[j]
				restarts += cs.RestartCount
				if containerCrashLooping(cs) {
					crashlooping = append(crashlooping, cs.Name)
				}
				containers = append(containers, containerSignal(cs, init, requestsByName[cs.Name]))
			}
		}
		appendContainers(p.Status.InitContainerStatuses, true)
		appendContainers(p.Status.ContainerStatuses, false)
		sort.Strings(crashlooping)
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })

		out = append(out, PodSignal{
			Namespace:              p.Namespace,
			Name:                   p.Name,
			Phase:                  phase,
			Reason:                 p.Status.Reason,
			RestartCount:           restarts,
			CrashLoopingContainers: crashlooping,
			Pending:                p.Status.Phase == corev1.PodPending,
			Failed:                 p.Status.Phase == corev1.PodFailed,
			Node:                   p.Spec.NodeName,
			Owners:                 ownerRefs(p),
			Containers:             containers,
			Requests:               sumRequests(p.Spec.Containers),
		})
	}
	sortByNamespaceName(out, func(s PodSignal) (string, string) { return s.Namespace, s.Name })
	return out
}

// ownerRefs transforms a pod's ownerReferences into sorted [OwnerRef]s. It
// returns nil (not an empty slice) when the pod is unowned, matching the
// collector's convention for absent repeated signals.
func ownerRefs(p *corev1.Pod) []OwnerRef {
	if len(p.OwnerReferences) == 0 {
		return nil
	}
	out := make([]OwnerRef, 0, len(p.OwnerReferences))
	for i := range p.OwnerReferences {
		o := &p.OwnerReferences[i]
		out = append(out, OwnerRef{
			Kind:       o.Kind,
			Name:       o.Name,
			Controller: o.Controller != nil && *o.Controller,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// containerSignal transforms a single container status (plus its spec requests)
// into a [ContainerSignal], reading waiting/termination state verbatim.
func containerSignal(cs *corev1.ContainerStatus, init bool, requests corev1.ResourceList) ContainerSignal {
	sig := ContainerSignal{
		Name:         cs.Name,
		Init:         init,
		RestartCount: cs.RestartCount,
		CrashLooping: containerCrashLooping(cs),
		Requests:     resourceListFrom(requests),
	}
	if w := cs.State.Waiting; w != nil {
		sig.WaitingReason = w.Reason
		sig.WaitingMessage = w.Message
	}
	if t := cs.LastTerminationState.Terminated; t != nil {
		sig.LastTermination = terminationSignal(t)
	}
	if t := cs.State.Terminated; t != nil {
		sig.CurrentTermination = terminationSignal(t)
	}
	return sig
}

// terminationSignal captures a terminated container state as a [TerminationSignal].
func terminationSignal(t *corev1.ContainerStateTerminated) *TerminationSignal {
	return &TerminationSignal{
		ExitCode: t.ExitCode,
		Reason:   t.Reason,
		Signal:   t.Signal,
	}
}

// containerRequestsByName indexes a pod's container resource requests by
// container name, covering both init and regular containers.
func containerRequestsByName(p *corev1.Pod) map[string]corev1.ResourceList {
	out := make(map[string]corev1.ResourceList, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for i := range p.Spec.InitContainers {
		out[p.Spec.InitContainers[i].Name] = p.Spec.InitContainers[i].Resources.Requests
	}
	for i := range p.Spec.Containers {
		out[p.Spec.Containers[i].Name] = p.Spec.Containers[i].Resources.Requests
	}
	return out
}

// sumRequests aggregates the cpu/memory requests across the given containers into
// a single [ResourceList]. It is used for the pod-level convenience aggregate over
// regular containers; per-container requests are captured separately.
func sumRequests(containers []corev1.Container) ResourceList {
	var cpu, mem resource.Quantity
	var haveCPU, haveMem bool
	for i := range containers {
		req := containers[i].Resources.Requests
		if q, ok := req[corev1.ResourceCPU]; ok {
			cpu.Add(q)
			haveCPU = true
		}
		if q, ok := req[corev1.ResourceMemory]; ok {
			mem.Add(q)
			haveMem = true
		}
	}
	out := ResourceList{}
	if haveCPU {
		out.CPU = cpu.String()
	}
	if haveMem {
		out.Memory = mem.String()
	}
	return out
}

// resourceListFrom extracts cpu/memory from a Kubernetes resource list into a
// canonical-string [ResourceList], leaving unset quantities as empty strings.
func resourceListFrom(rl corev1.ResourceList) ResourceList {
	out := ResourceList{}
	if q, ok := rl[corev1.ResourceCPU]; ok {
		out.CPU = q.String()
	}
	if q, ok := rl[corev1.ResourceMemory]; ok {
		out.Memory = q.String()
	}
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
