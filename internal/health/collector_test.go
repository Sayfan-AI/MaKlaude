package health

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// unreachableClient builds a real kube.Client whose API server is a closed port
// on localhost, so the reachability probe fails fast. It mirrors the kube
// package's own test setup.
func unreachableClient(t *testing.T) *kube.Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig.yaml")
	contents := `apiVersion: v1
kind: Config
current-context: maklaude
clusters:
  - name: maklaude-test
    cluster:
      server: https://127.0.0.1:1
      insecure-skip-tls-verify: true
contexts:
  - name: maklaude
    context:
      cluster: maklaude-test
      user: tester
users:
  - name: tester
    user:
      token: test-token
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{Name: "broken", Kubeconfig: path, Context: "maklaude"}},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	h, ok := reg.Get("broken")
	if !ok {
		t.Fatal("handle not found")
	}
	client, err := kube.NewClient(h)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return client
}

// fixedTime is the pinned collection time used across the tests so snapshots
// (and the event lookback cutoff derived from the time) are fully deterministic.
var fixedTime = time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)

// fixedClock returns a Clock that always reports fixedTime.
func fixedClock() Clock { return func() time.Time { return fixedTime } }

// newTestCollector builds a Collector over a fake clientset seeded with objs,
// pinned to fixedTime and the default lookback.
func newTestCollector(t *testing.T, objs ...runtime.Object) *Collector {
	t.Helper()
	cs := fake.NewSimpleClientset(objs...)
	client := kube.NewClientWithInterface("fixture", cs)
	return NewCollector(client, WithClock(fixedClock()))
}

// ptr is a tiny helper for the *int32 replica fields.
func ptr(v int32) *int32 { return &v }

// node builds a node with the given Ready/pressure condition statuses.
func node(name string, ready, mem, disk, pid corev1.ConditionStatus, unschedulable bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: unschedulable},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: ready},
				{Type: corev1.NodeMemoryPressure, Status: mem},
				{Type: corev1.NodeDiskPressure, Status: disk},
				{Type: corev1.NodePIDPressure, Status: pid},
			},
		},
	}
}

func TestCollect_Nodes(t *testing.T) {
	col := newTestCollector(t,
		// Declared out of name order to prove sorting.
		node("node-b", corev1.ConditionFalse, corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionFalse, true),
		node("node-a", corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionFalse, corev1.ConditionFalse, false),
	)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	want := []NodeSignal{
		{Name: "node-a", Ready: true},
		{Name: "node-b", Ready: false, MemoryPressure: true, Unschedulable: true},
	}
	if !reflect.DeepEqual(snap.Nodes, want) {
		t.Fatalf("node signals mismatch:\n got %+v\nwant %+v", snap.Nodes, want)
	}
}

// TestCollect_NoNodes proves an empty cluster yields an empty (non-nil) node
// slice and a successful, reachable snapshot.
func TestCollect_NoNodes(t *testing.T) {
	col := newTestCollector(t)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(snap.Nodes) != 0 {
		t.Fatalf("expected no nodes, got %+v", snap.Nodes)
	}
	if !snap.Reachability.Reachable {
		t.Fatalf("expected reachable snapshot, got %+v", snap.Reachability)
	}
	if !snap.CollectedAt.Equal(fixedTime) {
		t.Fatalf("expected CollectedAt %v, got %v", fixedTime, snap.CollectedAt)
	}
	if snap.Cluster != "fixture" {
		t.Fatalf("expected cluster %q, got %q", "fixture", snap.Cluster)
	}
}

func TestCollect_Pods(t *testing.T) {
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "crash"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 7,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					},
				},
				{Name: "sidecar", RestartCount: 1},
			},
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init", RestartCount: 2},
			},
		},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "pending"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	failed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "failed"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"},
	}

	col := newTestCollector(t, crashing, pending, failed)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	want := []PodSignal{
		{Namespace: "default", Name: "failed", Phase: "Failed", Reason: "Evicted", Failed: true},
		{
			Namespace:              "team",
			Name:                   "crash",
			Phase:                  "Running",
			RestartCount:           10, // 7 + 1 + 2
			CrashLoopingContainers: []string{"app"},
			// Containers are sorted by name: app, init, sidecar.
			Containers: []ContainerSignal{
				{Name: "app", RestartCount: 7, CrashLooping: true, WaitingReason: "CrashLoopBackOff"},
				{Name: "init", Init: true, RestartCount: 2},
				{Name: "sidecar", RestartCount: 1},
			},
		},
		{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true},
	}
	if !reflect.DeepEqual(snap.Pods, want) {
		t.Fatalf("pod signals mismatch:\n got %+v\nwant %+v", snap.Pods, want)
	}
}

// TestCollect_CrashLoopMidCycle covers the oscillation race: a crashlooping pod
// is frequently caught between backoff windows, with its container Terminated
// (non-zero exit) rather than Waiting/CrashLoopBackOff at the instant of the
// scan. It must still be reported as crashlooping. Regression for the T8 e2e
// failure, where a point-in-time scan caught the container mid-restart and
// under-reported an actively-crashing pod.
func TestCollect_CrashLoopMidCycle(t *testing.T) {
	// app: restarted twice, just exited with an error -> crashlooping.
	// flaky: restarted once, exited with an error -> below threshold, NOT yet
	//        crashlooping (don't false-positive on a single transient restart).
	crashing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "crash"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 2,
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
					},
				},
				{
					Name:         "flaky",
					RestartCount: 1,
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
					},
				},
			},
		},
	}

	col := newTestCollector(t, crashing)
	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(snap.Pods) != 1 {
		t.Fatalf("expected 1 pod signal, got %d", len(snap.Pods))
	}
	if got := snap.Pods[0].CrashLoopingContainers; !reflect.DeepEqual(got, []string{"app"}) {
		t.Fatalf("expected only app flagged crashlooping mid-cycle, got %+v", got)
	}
}

// TestCollect_PodEvidence proves the deepened per-pod evidence is captured:
// node assignment, ownerReferences (with the controller flag), per-container
// waiting reason/message, last-termination exit code/reason, per-container and
// pod-level resource requests, and node allocatable. It asserts the full
// structured signal and re-collects to confirm the new fields stay deterministic.
func TestCollect_PodEvidence(t *testing.T) {
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team", Name: "web-abc",
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "web", Controller: &controller},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			InitContainers: []corev1.Container{{
				Name: "setup",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("32Mi"),
				}},
			}},
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					}},
				},
				{Name: "puller"}, // no requests
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "setup",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"},
				},
			}},
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "app",
					RestartCount: 3,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off"},
					},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled", Signal: 9},
					},
				},
				{
					Name: "puller",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "Back-off pulling image"},
					},
				},
			},
		},
	}
	nodeA := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}

	col := newTestCollector(t, pod, nodeA)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	wantPod := PodSignal{
		Namespace:              "team",
		Name:                   "web-abc",
		Phase:                  "Pending",
		RestartCount:           3,
		CrashLoopingContainers: []string{"app"},
		Pending:                true,
		Node:                   "node-a",
		Owners:                 []OwnerRef{{Kind: "ReplicaSet", Name: "web", Controller: true}},
		Requests:               ResourceList{CPU: "250m", Memory: "128Mi"},
		// Containers sorted by name: app, puller, setup.
		Containers: []ContainerSignal{
			{
				Name:            "app",
				RestartCount:    3,
				CrashLooping:    true,
				WaitingReason:   "CrashLoopBackOff",
				WaitingMessage:  "back-off",
				LastTermination: &TerminationSignal{ExitCode: 137, Reason: "OOMKilled", Signal: 9},
				Requests:        ResourceList{CPU: "250m", Memory: "128Mi"},
			},
			{
				Name:           "puller",
				WaitingReason:  "ImagePullBackOff",
				WaitingMessage: "Back-off pulling image",
			},
			{
				Name:               "setup",
				Init:               true,
				CurrentTermination: &TerminationSignal{ExitCode: 0, Reason: "Completed"},
				Requests:           ResourceList{CPU: "50m", Memory: "32Mi"},
			},
		},
	}
	if len(snap.Pods) != 1 {
		t.Fatalf("expected 1 pod signal, got %d", len(snap.Pods))
	}
	if !reflect.DeepEqual(snap.Pods[0], wantPod) {
		t.Fatalf("pod evidence mismatch:\n got %+v\nwant %+v", snap.Pods[0], wantPod)
	}

	wantNode := NodeSignal{
		Name:        "node-a",
		Ready:       true,
		Allocatable: ResourceList{CPU: "2", Memory: "4Gi"},
	}
	if len(snap.Nodes) != 1 || !reflect.DeepEqual(snap.Nodes[0], wantNode) {
		t.Fatalf("node allocatable mismatch:\n got %+v\nwant %+v", snap.Nodes, wantNode)
	}

	// The new evidence fields must stay deterministic across collections.
	second, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect failed: %v", err)
	}
	if !reflect.DeepEqual(snap, second) {
		t.Fatalf("collection with deep evidence is not deterministic:\nfirst:  %+v\nsecond: %+v", snap, second)
	}
}

// TestCollect_ContainerWaitingReasons proves the range of waiting reasons a
// downstream analyzer relies on are captured verbatim per container.
func TestCollect_ContainerWaitingReasons(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "waiters"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "a", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}}},
				{Name: "b", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}},
				{Name: "c", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			},
		},
	}
	col := newTestCollector(t, pod)
	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	got := map[string]string{}
	for _, cs := range snap.Pods[0].Containers {
		got[cs.Name] = cs.WaitingReason
	}
	want := map[string]string{"a": "ErrImagePull", "b": "CreateContainerConfigError", "c": "ImagePullBackOff"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting reasons mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestCollect_Workloads(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(3)},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:     2,
			AvailableReplicas: 2,
			UpdatedReplicas:   3,
		},
	}
	// Replicas unset => Kubernetes default of 1.
	deployDefault := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "api"},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-1"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr(3)},
		Status:     appsv1.ReplicaSetStatus{ReadyReplicas: 1, AvailableReplicas: 1},
	}

	col := newTestCollector(t, deploy, deployDefault, rs)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	wantDeploys := []DeploymentSignal{
		{Namespace: "default", Name: "api", DesiredReplicas: 1},
		{Namespace: "default", Name: "web", DesiredReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2, UpdatedReplicas: 3},
	}
	if !reflect.DeepEqual(snap.Deployments, wantDeploys) {
		t.Fatalf("deployment signals mismatch:\n got %+v\nwant %+v", snap.Deployments, wantDeploys)
	}

	wantRS := []ReplicaSetSignal{
		{Namespace: "default", Name: "web-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	if !reflect.DeepEqual(snap.ReplicaSets, wantRS) {
		t.Fatalf("replicaset signals mismatch:\n got %+v\nwant %+v", snap.ReplicaSets, wantRS)
	}
}

func TestCollect_WarningEvents(t *testing.T) {
	recentWarn := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "team", Name: "evt-recent"},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedScheduling",
		Message:       "0/3 nodes are available",
		Count:         4,
		LastTimestamp: metav1.NewTime(fixedTime.Add(-5 * time.Minute)),
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "team", Name: "pending",
		},
	}
	// Same last-seen time as recentWarn to exercise the namespace/name tiebreak.
	recentWarn2 := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "team", Name: "evt-recent-2"},
		Type:          corev1.EventTypeWarning,
		Reason:        "BackOff",
		LastTimestamp: metav1.NewTime(fixedTime.Add(-5 * time.Minute)),
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "team", Name: "crash",
		},
	}
	// Newest of all — must sort first.
	newest := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "default", Name: "evt-newest"},
		Type:          corev1.EventTypeWarning,
		Reason:        "Failed",
		LastTimestamp: metav1.NewTime(fixedTime.Add(-1 * time.Minute)),
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node", Name: "node-a", // cluster-scoped: no namespace segment
		},
	}
	// Normal (non-warning) event must be excluded.
	normal := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "team", Name: "evt-normal"},
		Type:          corev1.EventTypeNormal,
		Reason:        "Scheduled",
		LastTimestamp: metav1.NewTime(fixedTime.Add(-2 * time.Minute)),
	}
	// Old warning, outside the lookback window, must be excluded.
	stale := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "team", Name: "evt-stale"},
		Type:          corev1.EventTypeWarning,
		Reason:        "BackOff",
		LastTimestamp: metav1.NewTime(fixedTime.Add(-2 * time.Hour)),
	}

	col := newTestCollector(t, recentWarn, recentWarn2, newest, normal, stale)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	want := []EventSignal{
		{
			Namespace: "default", Name: "evt-newest", Reason: "Failed",
			Count: 0, InvolvedObject: "Node/node-a",
			LastSeen: fixedTime.Add(-1 * time.Minute),
		},
		{
			Namespace: "team", Name: "evt-recent", Reason: "FailedScheduling",
			Message: "0/3 nodes are available", Count: 4,
			InvolvedObject: "Pod/team/pending",
			LastSeen:       fixedTime.Add(-5 * time.Minute),
		},
		{
			Namespace: "team", Name: "evt-recent-2", Reason: "BackOff",
			InvolvedObject: "Pod/team/crash",
			LastSeen:       fixedTime.Add(-5 * time.Minute),
		},
	}
	if !reflect.DeepEqual(snap.WarningEvents, want) {
		t.Fatalf("warning events mismatch:\n got %+v\nwant %+v", snap.WarningEvents, want)
	}
}

// TestCollect_EventSeriesTimestamps proves the modern Events API representation
// (eventTime + series) is honoured for both last-seen time and count.
func TestCollect_EventSeriesTimestamps(t *testing.T) {
	seriesEvt := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "evt-series"},
		Type:       corev1.EventTypeWarning,
		Reason:     "Unhealthy",
		EventTime:  metav1.NewMicroTime(fixedTime.Add(-30 * time.Minute)),
		Series: &corev1.EventSeries{
			Count:            12,
			LastObservedTime: metav1.NewMicroTime(fixedTime.Add(-2 * time.Minute)),
		},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "team", Name: "probe"},
	}

	col := newTestCollector(t, seriesEvt)

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(snap.WarningEvents) != 1 {
		t.Fatalf("expected 1 warning event, got %+v", snap.WarningEvents)
	}
	got := snap.WarningEvents[0]
	if got.Count != 12 {
		t.Fatalf("expected series count 12, got %d", got.Count)
	}
	if !got.LastSeen.Equal(fixedTime.Add(-2 * time.Minute)) {
		t.Fatalf("expected last-seen from series, got %v", got.LastSeen)
	}
}

// TestCollect_EventLookbackConfigurable proves a tighter lookback excludes an
// event the default window would have kept.
func TestCollect_EventLookbackConfigurable(t *testing.T) {
	evt := &corev1.Event{
		ObjectMeta:    metav1.ObjectMeta{Namespace: "team", Name: "evt"},
		Type:          corev1.EventTypeWarning,
		Reason:        "BackOff",
		LastTimestamp: metav1.NewTime(fixedTime.Add(-10 * time.Minute)),
	}
	cs := fake.NewSimpleClientset(evt)
	client := kube.NewClientWithInterface("fixture", cs)
	col := NewCollector(client, WithClock(fixedClock()), WithEventLookback(5*time.Minute))

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(snap.WarningEvents) != 0 {
		t.Fatalf("expected event outside 5m window to be excluded, got %+v", snap.WarningEvents)
	}
}

// TestCollect_Unreachable proves an unreachable cluster is recorded as a fact
// (Reachable=false with the error text) rather than failing the collection, and
// that the signal slices stay empty. It uses a real client pointed at a closed
// port so the reachability probe genuinely fails.
func TestCollect_Unreachable(t *testing.T) {
	client := unreachableClient(t)
	col := NewCollector(client, WithClock(fixedClock()))

	snap, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should not error on an unreachable cluster, got: %v", err)
	}
	if snap.Reachability.Reachable {
		t.Fatalf("expected Reachable=false, got %+v", snap.Reachability)
	}
	if snap.Reachability.Error == "" {
		t.Fatal("expected a non-empty reachability error message")
	}
	if snap.Reachability.ServerVersion != "" {
		t.Fatalf("expected empty server version, got %q", snap.Reachability.ServerVersion)
	}
	if len(snap.Nodes) != 0 || len(snap.Pods) != 0 || len(snap.Deployments) != 0 ||
		len(snap.ReplicaSets) != 0 || len(snap.WarningEvents) != 0 {
		t.Fatalf("expected empty signal slices for unreachable cluster, got %+v", snap)
	}
	if snap.Cluster != "broken" || !snap.CollectedAt.Equal(fixedTime) {
		t.Fatalf("expected cluster/timestamp still populated, got %q / %v", snap.Cluster, snap.CollectedAt)
	}
}

// TestCollect_Reachability_AllSignalTypes is an integration-style check that a
// fully-populated fixture produces a single deterministic snapshot covering
// every signal type at once.
func TestCollect_Reachability_AllSignalTypes(t *testing.T) {
	col := newTestCollector(t,
		node("node-a", corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionFalse, corev1.ConditionFalse, false),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "d"}, Spec: appsv1.DeploymentSpec{Replicas: ptr(1)}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "r"}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr(1)}},
		&corev1.Event{
			ObjectMeta:    metav1.ObjectMeta{Namespace: "default", Name: "e"},
			Type:          corev1.EventTypeWarning,
			Reason:        "BackOff",
			LastTimestamp: metav1.NewTime(fixedTime.Add(-time.Minute)),
		},
	)

	first, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("first Collect failed: %v", err)
	}
	second, err := col.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect failed: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("collection is not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if len(first.Nodes) != 1 || len(first.Pods) != 1 || len(first.Deployments) != 1 ||
		len(first.ReplicaSets) != 1 || len(first.WarningEvents) != 1 {
		t.Fatalf("expected one of each signal type, got %+v", first)
	}
}

// logCall records the PodLogOptions a GetLogs (pods/log GET) call carried, so a
// test can assert the bounding and previous-instance flags reached the API.
type logCall struct {
	container string
	previous  bool
	tail      int64
}

// logCapturingClient wraps a fake clientset seeded with pod, recording every
// pods/log GET's options into *calls while still returning the fake's canned
// "fake logs" body. It also lets tests inspect the raw action stream.
func logCapturingClient(t *testing.T, pod *corev1.Pod, calls *[]logCall) (*kube.Client, *fake.Clientset) {
	t.Helper()
	cs := fake.NewSimpleClientset(pod)
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			// Not a log read (e.g. the GetPod enumeration): let the tracker serve it.
			return false, nil, nil
		}
		opts, ok := action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		if !ok {
			t.Fatalf("pods/log action carried unexpected value %T", action.(k8stesting.GenericAction).GetValue())
		}
		var tail int64
		if opts.TailLines != nil {
			tail = *opts.TailLines
		}
		*calls = append(*calls, logCall{container: opts.Container, previous: opts.Previous, tail: tail})
		return true, &corev1.Pod{}, nil
	})
	return kube.NewClientWithInterface("fixture", cs), cs
}

// TestCollectPodLogs_TailAndPrevious proves the lazy log path bounds the tail to
// the default, fetches previous-instance logs for a crashlooping container (and
// only current logs for a healthy one), and targets a single named pod rather
// than listing pods cluster-wide.
func TestCollectPodLogs_TailAndPrevious(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "crash"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 5, State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				}},
				{Name: "sidecar"},
			},
		},
	}
	var calls []logCall
	client, cs := logCapturingClient(t, pod, &calls)
	col := NewCollector(client, WithClock(fixedClock()))

	ev, err := col.CollectPodLogs(context.Background(), "team", "crash", LogOptions{})
	if err != nil {
		t.Fatalf("CollectPodLogs failed: %v", err)
	}
	if ev.Namespace != "team" || ev.Name != "crash" {
		t.Fatalf("evidence scoped to wrong pod: %+v", ev)
	}

	// app (current), app (previous — crashlooping), sidecar (current).
	want := []ContainerLogEvidence{
		{Container: "app", Previous: false, TailLines: DefaultLogTailLines, Logs: "fake logs"},
		{Container: "app", Previous: true, TailLines: DefaultLogTailLines, Logs: "fake logs"},
		{Container: "sidecar", Previous: false, TailLines: DefaultLogTailLines, Logs: "fake logs"},
	}
	if !reflect.DeepEqual(ev.Containers, want) {
		t.Fatalf("log evidence mismatch:\n got %+v\nwant %+v", ev.Containers, want)
	}

	// The bounding and previous flags must have reached the API verbatim.
	wantCalls := []logCall{
		{container: "app", previous: false, tail: DefaultLogTailLines},
		{container: "app", previous: true, tail: DefaultLogTailLines},
		{container: "sidecar", previous: false, tail: DefaultLogTailLines},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("pods/log calls mismatch:\n got %+v\nwant %+v", calls, wantCalls)
	}

	// Single-pod targeting: the pod was read by name (get), never listed.
	var sawGetByName, sawList bool
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "pods" {
			continue
		}
		switch a.GetVerb() {
		case "list":
			sawList = true
		case "get":
			if ga, ok := a.(k8stesting.GetAction); ok && a.GetSubresource() == "" && ga.GetName() == "crash" {
				sawGetByName = true
			}
		}
	}
	if sawList {
		t.Fatal("CollectPodLogs must not list pods cluster-wide")
	}
	if !sawGetByName {
		t.Fatal("expected the pod to be read by name before fetching its logs")
	}
}

// TestCollectPodLogs_ContainerFilterAndTail proves an explicit container
// selection and tail bound are honoured, and that a non-crashlooping container
// yields only current logs.
func TestCollectPodLogs_ContainerFilterAndTail(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}, {Name: "sidecar"}},
		},
	}
	var calls []logCall
	client, _ := logCapturingClient(t, pod, &calls)
	col := NewCollector(client, WithClock(fixedClock()))

	ev, err := col.CollectPodLogs(context.Background(), "team", "web", LogOptions{
		TailLines:  10,
		Containers: []string{"sidecar"},
	})
	if err != nil {
		t.Fatalf("CollectPodLogs failed: %v", err)
	}

	want := []ContainerLogEvidence{
		{Container: "sidecar", Previous: false, TailLines: 10, Logs: "fake logs"},
	}
	if !reflect.DeepEqual(ev.Containers, want) {
		t.Fatalf("filtered log evidence mismatch:\n got %+v\nwant %+v", ev.Containers, want)
	}
	if len(calls) != 1 || calls[0].container != "sidecar" || calls[0].tail != 10 {
		t.Fatalf("expected a single sidecar log read tailed to 10, got %+v", calls)
	}
}

// TestCollectPodLogs_GetPodError proves a failure to read the implicated pod is
// surfaced (logs cannot be gathered for a pod that cannot be read).
func TestCollectPodLogs_GetPodError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	col := NewCollector(kube.NewClientWithInterface("fixture", cs), WithClock(fixedClock()))

	_, err := col.CollectPodLogs(context.Background(), "team", "missing", LogOptions{})
	if err == nil {
		t.Fatal("expected an error when the implicated pod cannot be read")
	}
	if !errors.Is(err, kube.ErrUnreachable) {
		t.Fatalf("expected wrapped ErrUnreachable, got: %v", err)
	}
}

// TestCollect_DoesNotFetchLogs is the regression guard for the safety-critical
// invariant that the eager Collect never fetches pod logs — logs are read only
// by the explicit, lazy CollectPodLogs path.
func TestCollect_DoesNotFetchLogs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "crash"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 5, State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				}},
			},
		},
	}
	cs := fake.NewSimpleClientset(pod)
	var logFetched bool
	cs.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "log" {
			logFetched = true
		}
		return false, nil, nil
	})
	col := NewCollector(kube.NewClientWithInterface("fixture", cs), WithClock(fixedClock()))

	if _, err := col.Collect(context.Background()); err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if logFetched {
		t.Fatal("Collect must never fetch pod logs (logs are lazy, per implicated pod only)")
	}
}

// fakeReactor wires a list error into the fake clientset so a mid-collection
// read failure can be exercised.
func failingListClient(t *testing.T, res string) *kube.Client {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", res, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	return kube.NewClientWithInterface("fixture", cs)
}

// TestCollect_ReadErrorPropagates proves that once a cluster is reachable, a
// failing read mid-collection is surfaced as a wrapped error rather than being
// silently dropped.
func TestCollect_ReadErrorPropagates(t *testing.T) {
	col := NewCollector(failingListClient(t, "pods"), WithClock(fixedClock()))

	_, err := col.Collect(context.Background())
	if err == nil {
		t.Fatal("expected an error from a failing pod list, got nil")
	}
	if !errors.Is(err, kube.ErrUnreachable) {
		t.Fatalf("expected wrapped ErrUnreachable, got: %v", err)
	}
}
