package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// fixedTime is the pinned clock used so report output is reproducible.
var fixedTime = time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

// newFakePipeline builds a Pipeline whose clients are backed by the supplied
// fake clientset objects and whose escalator writes to an in-memory sink. It
// returns the pipeline and the sink so a test can assert on the comms trail.
func newFakePipeline(t *testing.T, objects ...runtime.Object) (*Pipeline, *escalate.MemorySink) {
	t.Helper()
	sink := escalate.NewMemorySink()
	esc := escalate.NewEscalator(sink)

	builder := func(h *cluster.Handle) (*kube.Client, error) {
		cs := fake.NewSimpleClientset(objects...)
		// The fake discovery client returns an empty version by default, which is
		// a successful (reachable) probe — exactly what we want for these tests.
		return kube.NewClientWithInterface(h.Name(), cs), nil
	}

	p := NewPipelineForTest(builder, esc, false, func() time.Time { return fixedTime })
	return p, sink
}

// singleClusterRegistry builds a one-cluster registry pointing at this test
// file (any existing regular file satisfies the kubeconfig existence check; the
// fake client builder ignores the path entirely).
func singleClusterRegistry(t *testing.T, name string) *cluster.Registry {
	t.Helper()
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{
			{Name: name, Kubeconfig: "scan_test.go", Context: "ctx"},
		},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	return reg
}

// crashloopingPod returns a pod with a container stuck in CrashLoopBackOff.
func crashloopingPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
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
			},
		},
	}
}

// pendingPod returns a pod stuck in the Pending phase.
func pendingPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
}

func TestRun_DetectsAndEscalates(t *testing.T) {
	p, sink := newFakePipeline(t,
		crashloopingPod("default", "crasher"),
		pendingPod("default", "pender"),
	)
	reg := singleClusterRegistry(t, "test-cluster")

	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if got := len(report.Clusters); got != 1 {
		t.Fatalf("expected 1 cluster report, got %d", got)
	}
	cr := report.Clusters[0]
	if cr.Error != "" {
		t.Fatalf("unexpected cluster error: %s", cr.Error)
	}
	if !cr.Reachable {
		t.Fatalf("expected cluster to be reachable")
	}

	// (a) Expected findings detected: a critical crashloop and a warning pending.
	if !hasFinding(cr.Findings, "critical", "pod.crashloop", "pod/default/crasher") {
		t.Errorf("missing critical crashloop finding; got %+v", cr.Findings)
	}
	if !hasFinding(cr.Findings, "warning", "pod.pending", "pod/default/pender") {
		t.Errorf("missing warning pending finding; got %+v", cr.Findings)
	}

	// Findings must be sorted most-urgent-first (critical before warning).
	if len(cr.Findings) >= 2 && cr.Findings[0].Severity != "critical" {
		t.Errorf("expected critical finding first, got %q", cr.Findings[0].Severity)
	}

	// (a2) The findings correlate into incidents, each carrying at least one ranked
	// hypothesis. The two unrelated pods form two singleton incidents.
	if len(cr.Incidents) != 2 {
		t.Fatalf("expected 2 incidents (one per unrelated pod), got %d: %+v", len(cr.Incidents), cr.Incidents)
	}
	for _, in := range cr.Incidents {
		if len(in.Hypotheses) == 0 {
			t.Errorf("incident %q should carry at least one hypothesis", in.Identity)
		}
		if in.PrimaryObject == "" || in.Severity == "" {
			t.Errorf("incident report not well-formed: %+v", in)
		}
	}
	// Incidents are ranked most-urgent-first: the critical crashloop before the
	// warning pending.
	if cr.Incidents[0].Severity != "critical" {
		t.Errorf("expected the critical incident first, got %q", cr.Incidents[0].Severity)
	}

	// (b) An escalation was produced: one issue opened per INCIDENT (not per raw
	// finding). This is the T4 incident-granularity contract.
	wantOpened := len(cr.Incidents)
	if cr.Escalation.Opened != wantOpened {
		t.Errorf("expected %d issues opened (one per incident), got %d", wantOpened, cr.Escalation.Opened)
	}
	if sink.OpenCount() != wantOpened {
		t.Errorf("expected %d open issues in sink, got %d", wantOpened, sink.OpenCount())
	}

	// Totals roll up correctly.
	if report.Totals.Findings != len(cr.Findings) {
		t.Errorf("totals.findings = %d, want %d", report.Totals.Findings, len(cr.Findings))
	}
	if report.Totals.Incidents != len(cr.Incidents) {
		t.Errorf("totals.incidents = %d, want %d", report.Totals.Incidents, len(cr.Incidents))
	}
	if !report.HasSeverity("critical") {
		t.Errorf("report should report a critical severity present")
	}
}

// TestRun_CorrelatesCascadeIntoOneIncident proves the incident-granularity payoff:
// a bad-image cascade (a Deployment, its ReplicaSet, and its pod all failing) is
// escalated as ONE diagnostic issue, not three, and the diagnosis names the bad
// image as the leading root cause.
func TestRun_CorrelatesCascadeIntoOneIncident(t *testing.T) {
	// A pod owned (via its ReplicaSet) by deployment "web", stuck on ImagePullBackOff,
	// plus the failing ReplicaSet and unavailable Deployment it belongs to.
	badPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team", Name: "web-abc-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 0,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web-abc"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptrInt32(3)},
		Status:     appsv1.ReplicaSetStatus{Replicas: 3, ReadyReplicas: 0, AvailableReplicas: 0},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptrInt32(3)},
		Status:     appsv1.DeploymentStatus{Replicas: 3, ReadyReplicas: 0, AvailableReplicas: 0},
	}

	p, sink := newFakePipeline(t, badPod, rs, dep)
	reg := singleClusterRegistry(t, "cascade")

	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cr := report.Clusters[0]

	// Several raw findings, but they collapse into ONE incident.
	if len(cr.Findings) < 2 {
		t.Fatalf("expected multiple raw findings for the cascade, got %+v", cr.Findings)
	}
	if len(cr.Incidents) != 1 {
		t.Fatalf("cascade should correlate into exactly ONE incident, got %d: %+v", len(cr.Incidents), cr.Incidents)
	}
	// And exactly one issue opened for that one incident.
	if cr.Escalation.Opened != 1 || sink.OpenCount() != 1 {
		t.Errorf("cascade should open exactly one issue, got opened=%d sinkOpen=%d", cr.Escalation.Opened, sink.OpenCount())
	}
	// The diagnosis names the bad image as the leading (most-confident) cause.
	hyps := cr.Incidents[0].Hypotheses
	if len(hyps) == 0 || hyps[0].Cause != "badimage" {
		t.Errorf("expected the leading hypothesis to be badimage, got %+v", hyps)
	}
}

// ptrInt32 returns a pointer to v, for the fake replica-count specs above.
func ptrInt32(v int32) *int32 { return &v }

func TestRun_NoFindings_NoEscalation(t *testing.T) {
	// A single healthy running pod produces no findings.
	healthy := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ok"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	p, sink := newFakePipeline(t, healthy)
	reg := singleClusterRegistry(t, "healthy")

	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	cr := report.Clusters[0]
	if len(cr.Findings) != 0 {
		t.Errorf("expected no findings, got %+v", cr.Findings)
	}
	if cr.Escalation.Opened != 0 || sink.OpenCount() != 0 {
		t.Errorf("expected no escalation, got opened=%d sinkOpen=%d", cr.Escalation.Opened, sink.OpenCount())
	}
}

func TestRun_MultiClusterIsolation(t *testing.T) {
	// Cluster A is unhealthy, cluster B is healthy. Each gets its own fake
	// clientset; we assert findings appear only for A.
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{
			{Name: "cluster-a", Kubeconfig: "scan_test.go", Context: "a"},
			{Name: "cluster-b", Kubeconfig: "scan_test.go", Context: "b"},
		},
	})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	sink := escalate.NewMemorySink()
	esc := escalate.NewEscalator(sink)
	builder := func(h *cluster.Handle) (*kube.Client, error) {
		var objs []runtime.Object
		if h.Name() == "cluster-a" {
			objs = append(objs, crashloopingPod("default", "crasher"))
		}
		cs := fake.NewSimpleClientset(objs...)
		return kube.NewClientWithInterface(h.Name(), cs), nil
	}
	p := NewPipelineForTest(builder, esc, false, func() time.Time { return fixedTime })

	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Clusters) != 2 {
		t.Fatalf("expected 2 cluster reports, got %d", len(report.Clusters))
	}
	// Registry order is preserved.
	if report.Clusters[0].Cluster != "cluster-a" || report.Clusters[1].Cluster != "cluster-b" {
		t.Fatalf("cluster order not preserved: %q, %q", report.Clusters[0].Cluster, report.Clusters[1].Cluster)
	}
	if len(report.Clusters[0].Findings) == 0 {
		t.Errorf("cluster-a should have findings")
	}
	if len(report.Clusters[1].Findings) != 0 {
		t.Errorf("cluster-b should have no findings, got %+v", report.Clusters[1].Findings)
	}
	// Every finding is scoped to its own cluster.
	for _, f := range report.Clusters[0].Findings {
		if f.Cluster != "cluster-a" {
			t.Errorf("finding leaked across clusters: %+v", f)
		}
	}
}

func TestRun_ClientBuildError_RecordedPerCluster(t *testing.T) {
	sink := escalate.NewMemorySink()
	esc := escalate.NewEscalator(sink)
	builder := func(_ *cluster.Handle) (*kube.Client, error) {
		return nil, errors.New("boom")
	}
	p := NewPipelineForTest(builder, esc, false, func() time.Time { return fixedTime })
	reg := singleClusterRegistry(t, "broken")

	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run should not fail on per-cluster client error: %v", err)
	}
	cr := report.Clusters[0]
	if !strings.Contains(cr.Error, "building client") {
		t.Errorf("expected client build error recorded, got %q", cr.Error)
	}
	if cr.Reachable {
		t.Errorf("cluster should not be reachable on build error")
	}
}

func TestRun_NilRegistry(t *testing.T) {
	p, _ := newFakePipeline(t)
	if _, err := p.Run(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil registry")
	}
}

func TestRun_ReadOnly_NoWritesToCluster(t *testing.T) {
	// Belt-and-suspenders at the unit level: install a reactor on the fake
	// clientset that fails the test if any mutating verb is ever issued, then run
	// the full pipeline. The pipeline must complete without tripping it.
	cs := fake.NewSimpleClientset(
		crashloopingPod("default", "crasher"),
		pendingPod("default", "pender"),
	)
	cs.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		switch action.GetVerb() {
		case "get", "list", "watch":
			return false, nil, nil // let the default tracker handle reads
		default:
			t.Fatalf("read-only violation: pipeline issued a %q on %q", action.GetVerb(), action.GetResource())
			return true, nil, errors.New("unreachable")
		}
	})

	sink := escalate.NewMemorySink()
	esc := escalate.NewEscalator(sink)
	builder := func(h *cluster.Handle) (*kube.Client, error) {
		return kube.NewClientWithInterface(h.Name(), cs), nil
	}
	p := NewPipelineForTest(builder, esc, false, func() time.Time { return fixedTime })
	reg := singleClusterRegistry(t, "ro")

	if _, err := p.Run(context.Background(), reg); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestReport_JSONRoundTrip(t *testing.T) {
	p, _ := newFakePipeline(t,
		crashloopingPod("default", "crasher"),
		pendingPod("default", "pender"),
	)
	reg := singleClusterRegistry(t, "json-cluster")
	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal report JSON: %v", err)
	}
	if len(decoded.Clusters) != 1 {
		t.Fatalf("expected 1 cluster after round-trip, got %d", len(decoded.Clusters))
	}
	if decoded.Totals.Findings != report.Totals.Findings {
		t.Errorf("totals not preserved: %d vs %d", decoded.Totals.Findings, report.Totals.Findings)
	}
	if decoded.Live {
		t.Errorf("expected live=false for in-memory escalation")
	}
}

func TestReport_WriteText(t *testing.T) {
	p, _ := newFakePipeline(t, crashloopingPod("default", "crasher"))
	reg := singleClusterRegistry(t, "text-cluster")
	report, err := p.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"text-cluster", "CRITICAL", "Pod crashlooping", "escalation: opened=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

// hasFinding reports whether findings contains an entry matching severity,
// identity-rule, and object string. The identity is "cluster|rule|object", so we
// match on the rule segment and the object.
func hasFinding(findings []FindingReport, severity, rule, object string) bool {
	for _, f := range findings {
		if f.Severity != severity || f.Object != object {
			continue
		}
		parts := strings.Split(f.Identity, "|")
		if len(parts) == 3 && parts[1] == rule {
			return true
		}
	}
	return false
}
