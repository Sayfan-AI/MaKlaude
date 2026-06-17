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
		},
		{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true},
	}
	if !reflect.DeepEqual(snap.Pods, want) {
		t.Fatalf("pod signals mismatch:\n got %+v\nwant %+v", snap.Pods, want)
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

// fakeReactor wires a list error into the fake clientset so a mid-collection
// read failure can be exercised.
func failingListClient(t *testing.T, resource string) *kube.Client {
	t.Helper()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
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
