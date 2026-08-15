package operate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
//
// The clock is pinned but ADVANCEABLE, via the returned pointer. A wholly frozen clock
// makes two executions of one fix indistinguishable in the record — same finish
// instant, same reused approval artifact — so the ledger collapses them into one entry
// and a test that approves the same fix three times observes one approval. That is the
// pinned-clock trap issue #171 hit from the other direction: a frozen clock is right
// for anything measuring an interval and wrong for anything counting events.
func promotionCycle(t *testing.T, objects ...runtime.Object) (*Cycle, kubernetes.Interface, *approve.MemorySink, *time.Time) {
	t.Helper()

	now := fixedTime
	clock := func() time.Time { return now }

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
		WithClock(clock)

	c, err := NewForTest(kube.ExecuteEnabled, newClient, newMutator, gate, audit.NewTrail(), fastPolicy, false, clock)
	if err != nil {
		t.Fatalf("building the cycle: %v", err)
	}
	return c, cs, sink, &now
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
	c, cs, sink, clock := promotionCycle(t, objects...)

	ledger := trust.NewMemory()
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), ledger, mustTrail(t, disclose.NewMemorySink()), ledger)

	shape := autonomy.Shape{Cluster: testCluster, Operation: remediate.OpRolloutRestart}
	// Since issue #167 trust is keyed on the fix, so the shape alone no longer names
	// anything the oracle answers about — see [autonomy.Subject]. Fingerprints are read
	// off the proposals the cycle actually made rather than reconstructed here: a
	// hand-built proposal differing in any fingerprinted field would make every
	// assertion below pass against a subject the cycle never asks about.
	fingerprints := map[string]remediate.Fingerprint{}
	observe := func(cr ClusterReport) ClusterReport {
		for _, p := range cr.Proposals {
			fingerprints[p.Target] = remediate.Fingerprint(p.Fingerprint)
		}
		return cr
	}
	subjectFor := func(name string) autonomy.Subject {
		fp, ok := fingerprints["deployment/"+testNamespace+"/"+name]
		if !ok {
			t.Fatalf("no proposal was ever observed for %s", name)
		}
		return autonomy.Subject{Shape: shape, Fingerprint: fp}
	}
	ctx := context.Background()

	// Pass one: the shape has earned nothing, so nothing is auto-applied and every
	// proposal takes the human gate.
	cr := observe(run(t, c).Clusters[0])
	if ledger.Trust(subjectFor(deployments[0])).Trusted {
		t.Fatal("a fresh ledger trusts the fix, so this test proves nothing")
	}
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
	cr = observe(run(t, c).Clusters[0])
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

	// Each approval is of a DIFFERENT deployment, so each carries its own fingerprint and
	// each fix has exactly one approval. This is the fact issue #167 turns on: three
	// approvals on three objects are not three approvals of anything.
	for _, name := range deployments {
		if st := ledger.Standing(subjectFor(name)); st.Approved != 1 {
			t.Fatalf("%s has %d approvals, want 1: %s", name, st.Approved, ledger.Explain(subjectFor(name)))
		}
	}
	if got := ledger.Len(); got != len(deployments) {
		t.Fatalf("the ledger holds %d entries, want %d", got, len(deployments))
	}

	// A fresh instance of the same shape crashloops. Under the (cluster, operation) trust
	// key this inherited the three approvals above and ran unattended — the exact
	// behavior issue #167 was filed about, restated: "three approved rollout-restarts on
	// prod earn the right to restart ANY deployment on prod, including workloads that did
	// not exist when the approvals were given". It must now gate.
	fresh := crashloopingPair("api-d", "400")
	if _, err := cs.AppsV1().Deployments(testNamespace).Create(ctx, fresh[0].(*appsv1.Deployment), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the fourth deployment: %v", err)
	}
	if _, err := cs.CoreV1().Pods(testNamespace).Create(ctx, fresh[1].(*corev1.Pod), metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the fourth pod: %v", err)
	}

	// Pass three: the new workload is NOT auto-applied. It reaches the human gate like any
	// fix nobody has approved, and the explanation says why in those terms.
	cr = observe(run(t, c).Clusters[0])
	if len(cr.AutoApplied) != 0 {
		t.Fatalf("a workload that had never been approved was auto-applied on its predecessors' "+
			"record: %+v", cr.AutoApplied)
	}
	if cr.Gate.Opened != 1 {
		t.Fatalf("the new workload did not reach the human gate: gate %+v, error %s", cr.Gate, cr.Error)
	}
	if got := ledger.Explain(subjectFor("api-d")); !strings.Contains(got, "given for a different action") {
		t.Errorf("Explain() = %q, want it to say the approvals on record were for other fixes", got)
	}

	// Now earn it the way the model requires: the SAME fix, approved the threshold number
	// of times. api-d keeps crashlooping, so each pass re-proposes the identical fix and
	// each approval accumulates against one fingerprint.
	for round := 0; round < trust.PromotionThreshold; round++ {
		// Break api-d again. The previous round's fix genuinely worked — the converging
		// mutator heals the pod — so without a fresh fault there is nothing to propose and
		// nothing to approve. That is the shape of the real thing: an approval per
		// occurrence, not three approvals of one occurrence.
		//
		// The new pod has a new name, and that is deliberately invisible to the
		// fingerprint. [remediate.PreconditionPodCrashLooping]'s expectation names the
		// crashlooping pod, which for a Deployment target is a different object every
		// time; if it bound the fingerprint, every occurrence would be a new fix and this
		// loop could never accumulate anything. See [remediate.PreconditionKind.BindsFingerprint].
		if round > 0 {
			pod := crashloopingPair("api-d", strconv.Itoa(500+round))[1].(*corev1.Pod)
			pod.Name = fmt.Sprintf("api-d-pod-%d", round)
			if _, err := cs.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
				t.Fatalf("round %d: re-breaking api-d: %v", round, err)
			}
			dep, err := cs.AppsV1().Deployments(testNamespace).Get(ctx, "api-d", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("round %d: reading api-d: %v", round, err)
			}
			dep.Status = appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 0, UnavailableReplicas: 1}
			if _, err := cs.AppsV1().Deployments(testNamespace).UpdateStatus(ctx, dep, metav1.UpdateOptions{}); err != nil {
				t.Fatalf("round %d: re-breaking api-d's status: %v", round, err)
			}
		}

		// A pass to raise the artifact for this occurrence. Round 0's is already open from
		// the pass above, so this one only re-reads it.
		observe(run(t, c).Clusters[0])

		open, err := sink.ListOpen(ctx)
		if err != nil {
			t.Fatalf("round %d: listing open artifacts: %v", round, err)
		}
		if len(open) != 1 {
			t.Fatalf("round %d: %d open artifacts, want 1", round, len(open))
		}
		// Stamped at the CURRENT clock, not at fixedTime. The rounds deliberately sit
		// hours apart, and an approval stamped in the distant past is one the gate expires
		// — correctly, per issue #171 — so a frozen approval instant would make every
		// round after the first refuse rather than authorize.
		if err := sink.Decide(open[0].Ref, approve.ApprovedLabel, testApprover, clock.Add(time.Second)); err != nil {
			t.Fatalf("round %d: recording the approval: %v", round, err)
		}
		if crr := observe(run(t, c).Clusters[0]); crr.Error != "" {
			t.Fatalf("round %d: %s", round, crr.Error)
		}

		// Retire the artifact and move the clock on. Together these make the next round a
		// SEPARATE INCIDENT rather than a re-run of this one, which is what accumulating
		// approvals of one fix means in production: the fault returns days later, a fresh
		// artifact opens, a person approves again. An approval authorizes exactly one
		// execution — the gate is right to refuse to re-fire a spent one — so without the
		// close, rounds two and three authorize nothing at all.
		if err := sink.Close(ctx, open[0].Ref); err != nil {
			t.Fatalf("round %d: retiring the artifact: %v", round, err)
		}
		// Well past trust.RecurrenceHorizon, so the returning fault reads as a new
		// incident and not as the previous fix having failed to hold. Inside the horizon
		// this loop would record three regressions instead of three approvals — which is
		// the correct behaviour, and is asserted directly in TestRun_ARecurringFaultDemotes.
		*clock = clock.Add(2 * trust.RecurrenceHorizon)
	}

	if st := ledger.Standing(subjectFor("api-d")); st.Approved != trust.PromotionThreshold {
		t.Fatalf("api-d has %d approvals of its fix, want %d: %s",
			st.Approved, trust.PromotionThreshold, ledger.Explain(subjectFor("api-d")))
	}
	if !ledger.Trust(subjectFor("api-d")).Trusted {
		t.Fatalf("%d human-approved converged executions of one fix did not promote it: %s",
			trust.PromotionThreshold, ledger.Explain(subjectFor("api-d")))
	}

	// And the neighbours it never earned anything for are still gating, which is the
	// other half of "trust is scoped to the fix".
	if ledger.Trust(subjectFor(deployments[0])).Trusted {
		t.Errorf("%s inherited trust earned by api-d", deployments[0])
	}
}

// TestRun_ARecurringFaultDemotes is issue #167's heaviest done criterion proven against
// the live wiring: a fix that appears to converge and does not must lose its trust.
//
// The counting window used to supply this property by accident — it dragged a shape
// back to a person on a schedule whether or not the health signal was telling the truth
// — and removing it would otherwise leave a fix that silently does not work trusted
// forever, since nothing would demote it. What replaces it is recurrence: the fault
// coming back is strictly better evidence than a counter rolling over.
//
// The order matters and is asserted: the demotion must land BEFORE the pass decides, or
// a fix already known not to work would authorize itself one last time.
//
// Trust is SEEDED here rather than earned pass by pass, and the reason is a real clock
// mismatch rather than convenience. A recorded execution's instant comes from
// [execute.Report.FinishedAt], which reads the wall clock because it measures an
// elapsed interval, while the cycle's own clock is injectable and pinned in tests. The
// recurrence horizon compares the two, so a pinned cycle clock makes every recorded
// execution look like it happened in the distant future and no recurrence can ever be
// detected. Both clocks are the wall clock in production, so the mismatch is a test-only
// artifact today — but a deployment that injects a clock would silently lose recurrence
// detection, which is filed separately. Earning trust end to end is already proven by
// TestRun_ApprovalsAloneDriveAShapeFromUntrustedToTrusted; the subject here is what
// happens after.
func TestRun_ARecurringFaultDemotes(t *testing.T) {
	c, _, _, clock := promotionCycle(t, crashloopingPair("api-a", "100")...)
	// One clock for the whole test: the wall clock, which is what stamps the executions
	// the ledger holds. See the comment above.
	*clock = time.Now().UTC()

	ledger := trust.NewMemory()
	c.UseBudget(memoryBudget())
	c.UseAutonomy(permissiveRuleset(), ledger, mustTrail(t, disclose.NewMemorySink()), ledger)

	shape := autonomy.Shape{Cluster: testCluster, Operation: remediate.OpRolloutRestart}

	// One pass to learn what the cycle actually proposes. Reconstructing the identity and
	// fingerprint by hand would let this test seed a history the cycle never asks about.
	first := run(t, c).Clusters[0]
	if first.Error != "" {
		t.Fatalf("the opening pass failed: %s", first.Error)
	}
	if len(first.Proposals) != 1 {
		t.Fatalf("the opening pass made %d proposals, want 1", len(first.Proposals))
	}
	identity := remediate.ProposalIdentity(first.Proposals[0].Identity)
	subject := autonomy.Subject{Shape: shape, Fingerprint: remediate.Fingerprint(first.Proposals[0].Fingerprint)}

	// A history in which this exact fix was approved and converged the threshold number
	// of times, the most recent of them well inside the recurrence horizon.
	for i := 0; i < trust.PromotionThreshold; i++ {
		at := clock.Add(-time.Duration(trust.PromotionThreshold-i) * 10 * time.Minute)
		if err := ledger.Record(trust.Entry{
			Key:         string(identity) + "@" + at.Format(time.RFC3339Nano),
			Identity:    identity,
			Shape:       shape,
			Fingerprint: subject.Fingerprint,
			Authority:   audit.AuthorityHuman,
			Outcome:     trust.OutcomeConverged,
			At:          at,
			Ref:         fmt.Sprintf("https://github.com/Sayfan-AI/MaKlaude/issues/%d", 900+i),
		}); err != nil {
			t.Fatalf("seeding approval %d: %v", i, err)
		}
	}
	if !ledger.Trust(subject).Trusted {
		t.Fatalf("precondition failed: %d approved converged executions of one fix should be trusted: %s",
			trust.PromotionThreshold, ledger.Explain(subject))
	}

	// The fault is still there. Every one of those executions claimed to have fixed it,
	// the most recent ten minutes ago, so the convergence check was wrong.
	cr := run(t, c).Clusters[0]
	if cr.Error != "" {
		t.Fatalf("the pass failed: %s", cr.Error)
	}

	// Not auto-applied. This is the whole point: the demotion lands before the pass
	// decides, so the fix does not get one more unattended run on the strength of a
	// convergence this same pass has just disproved.
	if len(cr.AutoApplied) != 0 {
		t.Fatalf("a fix that had just been shown not to hold was auto-applied anyway: %+v", cr.AutoApplied)
	}
	if ledger.Trust(subject).Trusted {
		t.Errorf("the shape kept its trust through a regression: %s", ledger.Explain(subject))
	}
	if st := ledger.Standing(subject); st.Blocker.Outcome != trust.OutcomeRegressed {
		t.Errorf("Blocker.Outcome = %s, want %s", st.Blocker.Outcome, trust.OutcomeRegressed)
	}

	// And the operator is told, in those words. A demotion's only other trace is a shape
	// that silently stopped auto-applying, which reads exactly like one that never earned
	// anything.
	if len(cr.Regressions) != 1 {
		t.Fatalf("the report carries %d regressions, want 1: %+v", len(cr.Regressions), cr.Regressions)
	}
	if !strings.Contains(cr.Regressions[0], "did not hold") {
		t.Errorf("the regression line does not say the fix did not hold: %q", cr.Regressions[0])
	}

	// One regression per event, not per pass. A cycle that kept re-reading the same
	// unfixed fault must not stack demotions that all describe one thing.
	before := ledger.Len()
	if again := run(t, c).Clusters[0]; len(again.Regressions) != 0 {
		t.Errorf("the second pass re-reported the same regression: %+v", again.Regressions)
	}
	if got := ledger.Len(); got != before {
		t.Errorf("the ledger grew from %d to %d entries on a re-read of one recurrence", before, got)
	}
}
