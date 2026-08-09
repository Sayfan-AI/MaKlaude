package operate

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// This file holds the promotion path's end-to-end coverage: a shape driven from
// untrusted to trusted through human approvals alone, in the live wiring, with a real
// [trust.Ledger] as both the oracle the decision layer consults and the recorder the
// execution layer writes. Issue #166 found that no such test existed, which is how a
// wiring gap that made autonomy unearnable by construction stayed invisible — every
// proposal gated forever, and "everything gates" is exactly what a correct fail-closed
// system looks like from the outside.
//
// The one concession to the fake clientset is convergence: nothing reconciles a fake,
// so a rollout restart would never produce the new-revision-and-all-ready state the
// convergence check demands, and a human-approved execution that never converges never
// promotes. [convergingMutator] therefore plays the deployment controller's part —
// when a real (non-dry-run) restart lands, it creates the next-revision ReplicaSet,
// marks the deployment fully rolled out, and removes the crashlooping pods. Everything
// downstream of that is the production path reading the cluster: the observation loop,
// the verdict, the audit lifecycle, and the ledger entry derived from it.

// crashloopingPair seeds one Deployment and one of its pods stuck in CrashLoopBackOff
// — the same fault [crashloopingWorkload] builds, parameterized so a test can run
// several instances of the shape side by side.
func crashloopingPair(name, resourceVersion string) []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, ResourceVersion: resourceVersion},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
			Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 0, UnavailableReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNamespace, Name: name + "-pod", ResourceVersion: resourceVersion + "1",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: name}},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:         "app",
					RestartCount: 7,
					State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				}},
			},
		},
	}
}

// convergingMutator is a [recordingMutator] whose rollout restarts actually take
// effect on the fake clientset, standing in for the deployment controller that a fake
// does not have. Dry-run requests change nothing, exactly as on a real API server.
type convergingMutator struct {
	recordingMutator
	clientset kubernetes.Interface
}

func (m *convergingMutator) RestartDeploymentRollout(ctx context.Context, ns, name, _, _ string) (*kube.Outcome, error) {
	out := m.record("restart deployment/" + ns + "/" + name)
	if m.mode == kube.ExecuteDryRun {
		return out, nil
	}
	if err := reconcileRestart(ctx, m.clientset, ns, name); err != nil {
		return nil, fmt.Errorf("reconciling the fake restart: %w", err)
	}
	return out, nil
}

// reconcileRestart does what the deployment controller would: a restart bumps the pod
// template, so a new ReplicaSet appears at the next revision, the rollout completes,
// and the crashlooping pods are replaced. The ReplicaSet is created fully available so
// the detector does not read the controller's own artifact as a fresh fault.
func reconcileRestart(ctx context.Context, cs kubernetes.Interface, ns, name string) error {
	sets, err := cs.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	var highest int64
	for i := range sets.Items {
		rs := &sets.Items[i]
		for _, owner := range rs.OwnerReferences {
			if owner.Kind != "Deployment" || owner.Name != name {
				continue
			}
			if rev, err := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64); err == nil && rev > highest {
				highest = rev
			}
		}
	}
	next := highest + 1

	if _, err := cs.AppsV1().ReplicaSets(ns).Create(ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: fmt.Sprintf("%s-%d", name, next),
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": strconv.FormatInt(next, 10)},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name}},
		},
		Spec:   appsv1.ReplicaSetSpec{Replicas: ptr(int32(1))},
		Status: appsv1.ReplicaSetStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}, metav1.CreateOptions{}); err != nil {
		return err
	}

	dep, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	dep.Status = appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
	if _, err := cs.AppsV1().Deployments(ns).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
		return err
	}

	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		for _, owner := range pods.Items[i].OwnerReferences {
			if owner.Kind == "Deployment" && owner.Name == name {
				if err := cs.CoreV1().Pods(ns).Delete(ctx, pods.Items[i].Name, metav1.DeleteOptions{}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// promotionCycle builds an execution-enabled cycle whose read path and write path
// share one fake clientset, so a mutation the [convergingMutator] performs is what the
// convergence observation and the next pass's health scan actually see.
func promotionCycle(t *testing.T, objects ...runtime.Object) (*Cycle, kubernetes.Interface, *approve.MemorySink) {
	t.Helper()

	cs := fake.NewSimpleClientset(objects...)
	newClient := func(h *cluster.Handle) (*kube.Client, error) {
		return kube.NewClientWithInterface(h.Name(), cs), nil
	}
	newMutator := func(h *cluster.Handle, mode kube.ExecuteMode) (execute.Mutator, error) {
		return &convergingMutator{
			recordingMutator: recordingMutator{cluster: h.Name(), mode: mode},
			clientset:        cs,
		}, nil
	}

	sink := approve.NewMemorySink()
	sink.SelfLogin = testSelfLogin
	gate := approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy()).
		WithClock(func() time.Time { return fixedTime })

	c, err := NewForTest(kube.ExecuteEnabled, newClient, newMutator, gate, audit.NewTrail(), fastPolicy, false,
		func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("building the cycle: %v", err)
	}
	return c, cs, sink
}

// TestRun_ApprovalsAloneDriveAShapeFromUntrustedToTrusted is the fourth done criterion
// of issue #166, the one its decision comment singles out: promotion proven end to end
// rather than seeded. A real ledger starts empty, three human approvals of the shape
// execute and converge through the gated path, the ledger the executions wrote into is
// the oracle that then grants autonomy, and the auto-applied success that follows is
// reported to that same ledger and dropped by [trust.Entry.Counts] — so earning trust
// and not-compounding it are both asserted against the live wiring.
func TestRun_ApprovalsAloneDriveAShapeFromUntrustedToTrusted(t *testing.T) {
	deployments := []string{"api-a", "api-b", "api-c"}
	var objects []runtime.Object
	for i, name := range deployments {
		objects = append(objects, crashloopingPair(name, strconv.Itoa(100+10*i))...)
	}
	c, cs, sink := promotionCycle(t, objects...)

	ledger := trust.NewMemory()
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), ledger, mustTrail(t, disclose.NewMemorySink()), ledger)

	shape := autonomy.Shape{Cluster: testCluster, Operation: remediate.OpRolloutRestart}
	ctx := context.Background()
	if ledger.Trust(shape).Trusted {
		t.Fatal("a fresh ledger trusts the shape, so this test proves nothing")
	}

	// Pass one: the shape has earned nothing, so nothing is auto-applied and every
	// proposal takes the human gate.
	cr := run(t, c).Clusters[0]
	if len(cr.AutoApplied) != 0 {
		t.Fatalf("an untrusted shape was auto-applied: %+v", cr.AutoApplied)
	}
	if cr.Gate.Opened != len(deployments) {
		t.Fatalf("the gate opened %d artifacts, want %d (error: %s)", cr.Gate.Opened, len(deployments), cr.Error)
	}

	// A human approves all three.
	open, err := sink.ListOpen(ctx)
	if err != nil || len(open) != len(deployments) {
		t.Fatalf("expected %d open artifacts, got %d (err: %v)", len(deployments), len(open), err)
	}
	for _, a := range open {
		if err := sink.Decide(a.Ref, approve.ApprovedLabel, testApprover, fixedTime.Add(time.Second)); err != nil {
			t.Fatalf("recording the simulated approval on %s: %v", a.Ref, err)
		}
	}

	// Pass two: the gate honors the approvals, the executions converge, and each one
	// reaches the ledger on the live path — no rebuild, no seeding.
	cr = run(t, c).Clusters[0]
	if cr.Gate.Authorized != len(deployments) {
		t.Fatalf("the gate authorized %d executions, want %d (gate: %+v, error: %s)",
			cr.Gate.Authorized, len(deployments), cr.Gate, cr.Error)
	}
	if len(cr.Executions) != len(deployments) {
		t.Fatalf("expected %d executions, got %d: %+v", len(deployments), len(cr.Executions), cr.Executions)
	}
	for _, e := range cr.Executions {
		if !e.Executed || e.Convergence != "converged" {
			t.Fatalf("an approved execution did not land and converge: %+v", e)
		}
		if e.Authority != approve.AuthorityHuman.String() {
			t.Fatalf("execution authority = %q, want %q", e.Authority, approve.AuthorityHuman.String())
		}
		if e.Error != "" {
			t.Fatalf("the execution reports a problem — a failed ledger write would surface here: %s", e.Error)
		}
	}

	st := ledger.Standing(shape)
	if st.Approved != len(deployments) {
		t.Fatalf("the ledger holds %d approvals for the shape, want %d: %s", st.Approved, len(deployments), ledger.Explain(shape))
	}
	if !ledger.Trust(shape).Trusted {
		t.Fatalf("%d human-approved converged executions did not promote the shape: %s",
			len(deployments), ledger.Explain(shape))
	}

	// A fresh instance of the same shape crashloops. The trust earned above must now
	// apply — this is the moment that was impossible before issue #166's fix.
	fresh := crashloopingPair("api-d", "400")
	if _, err := cs.AppsV1().Deployments(testNamespace).Create(ctx, fresh[0].(*appsv1.Deployment), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the fourth deployment: %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(ctx, fresh[1].(*corev1.Pod), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the fourth pod: %v", err)
	}

	// Pass three: auto-applied without a gate, and the success is handed to the ledger
	// and dropped there — trust neither compounds nor erodes from unattended work.
	cr = run(t, c).Clusters[0]
	if len(cr.AutoApplied) != 1 {
		t.Fatalf("the trusted shape was not auto-applied: applied %+v, gate %+v, error %s",
			cr.AutoApplied, cr.Gate, cr.Error)
	}
	applied := cr.AutoApplied[0]
	if !applied.Execution.Executed || applied.Execution.Convergence != "converged" {
		t.Fatalf("the unattended action did not land and converge: %+v", applied.Execution)
	}
	if cr.Gate.Opened != 0 {
		t.Fatalf("the trusted shape was also put to a human: %+v", cr.Gate)
	}
	if got := ledger.Len(); got != len(deployments) {
		t.Errorf("the ledger holds %d entries after the auto-applied success, want %d — "+
			"the success must be reported and dropped, not stored", got, len(deployments))
	}
	if !ledger.Trust(shape).Trusted {
		t.Errorf("the auto-applied success revoked the shape's trust: %s", ledger.Explain(shape))
	}
}
