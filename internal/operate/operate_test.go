package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	testCluster   = "test-cluster"
	testNamespace = "default"
	testDeploy    = "api"
	testPod       = "api-7d9f"

	// testApprover is the login the simulated human decision is attributed to. It is
	// deliberately not testSelfLogin: an approval MaKlaude applied to its own artifact
	// is refused, and these tests must exercise the path where that refusal does not
	// fire.
	testApprover = "operator-alice"

	// testSelfLogin is the account the gate treats as MaKlaude, set rather than left
	// empty so the self-approval refusal is genuinely armed while these tests run.
	testSelfLogin = "maklaude-test-bot"
)

// fixedTime pins the clock so report output is reproducible.
var fixedTime = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// fastPolicy keeps the convergence observation window short. The fake clientset never
// converges — nothing reconciles a fake — so every execution test would otherwise sit
// out the full ninety-second default watching a state that cannot change.
var fastPolicy = execute.Policy{
	ObserveWindow:   10 * time.Millisecond,
	ObserveInterval: 5 * time.Millisecond,
	MaxAttempts:     1,
	RetryBackoff:    time.Millisecond,
}

// recordingMutator is a fake [execute.Mutator] that records every mutating request it
// is asked to make and performs none of them.
//
// It records rather than simulates for the reason the Mutator interface exists at all:
// the interface enumerates every write MaKlaude can issue, so a fake on the other side
// of it can prove that an aborted cycle attempted NOTHING — a claim no amount of
// asserting on the final cluster state can make, because "no change" and "a change
// that failed" look identical from there.
type recordingMutator struct {
	cluster string
	mode    kube.ExecuteMode
	calls   []string
}

func (m *recordingMutator) Name() string           { return m.cluster }
func (m *recordingMutator) Mode() kube.ExecuteMode { return m.mode }
func (m *recordingMutator) record(op string) *kube.Outcome {
	m.calls = append(m.calls, op)
	return &kube.Outcome{
		Cluster:         m.cluster,
		Target:          op,
		Scope:           "PATCH /apis/apps/v1/...",
		ResourceVersion: "1",
		DryRun:          m.mode == kube.ExecuteDryRun,
	}
}

func (m *recordingMutator) RestartDeploymentRollout(_ context.Context, ns, name, _, _ string) (*kube.Outcome, error) {
	return m.record("restart deployment/" + ns + "/" + name), nil
}

func (m *recordingMutator) PatchDeployment(_ context.Context, ns, name string, _ []byte, _ string) (*kube.Outcome, error) {
	return m.record("patch deployment/" + ns + "/" + name), nil
}

func (m *recordingMutator) RollbackDeploymentToRevision(_ context.Context, ns, name string, _ int64, _ string) (*kube.Outcome, error) {
	return m.record("rollback deployment/" + ns + "/" + name), nil
}

func (m *recordingMutator) CordonNode(_ context.Context, name, _ string) (*kube.Outcome, error) {
	return m.record("cordon node/" + name), nil
}

func (m *recordingMutator) PatchNode(_ context.Context, name string, _ []byte, _ string) (*kube.Outcome, error) {
	return m.record("patch node/" + name), nil
}

func (m *recordingMutator) DeletePod(_ context.Context, ns, name, _ string) (*kube.Outcome, error) {
	return m.record("delete pod/" + ns + "/" + name), nil
}

var _ execute.Mutator = (*recordingMutator)(nil)

// mutatorFactory hands out recordingMutators and remembers every mode it was asked
// for. The call log is the subject of the central safety test: "no executor is
// constructed without the opt-in" is a claim about whether this was CALLED.
type mutatorFactory struct {
	modes    []kube.ExecuteMode
	returned []*recordingMutator
}

func (f *mutatorFactory) build(h *cluster.Handle, mode kube.ExecuteMode) (execute.Mutator, error) {
	f.modes = append(f.modes, mode)
	m := &recordingMutator{cluster: h.Name(), mode: mode}
	f.returned = append(f.returned, m)
	return m, nil
}

// calls flattens every mutating request made through every mutator this factory
// handed out, so a test can assert on the total across preview and execution clients.
func (f *mutatorFactory) calls() []string {
	var out []string
	for _, m := range f.returned {
		out = append(out, m.calls...)
	}
	return out
}

// realCalls is the subset of calls that were sent through a client that could actually
// write. It is what "did a cluster change?" reduces to in these tests.
func (f *mutatorFactory) realCalls() []string {
	var out []string
	for _, m := range f.returned {
		if m.mode != kube.ExecuteDryRun {
			out = append(out, m.calls...)
		}
	}
	return out
}

// newCycle builds a cycle over a fake clientset seeded with objects, at the given
// mode. It returns the cycle, the mutator factory (for the call log), and the approval
// sink (nil when the mode is disabled, matching what [New] produces).
func newCycle(t *testing.T, mode kube.ExecuteMode, objects ...runtime.Object) (*Cycle, *mutatorFactory, *approve.MemorySink) {
	t.Helper()

	newClient := func(h *cluster.Handle) (*kube.Client, error) {
		return kube.NewClientWithInterface(h.Name(), fake.NewSimpleClientset(objects...)), nil
	}
	factory := &mutatorFactory{}

	var gate *approve.Gatekeeper
	var sink *approve.MemorySink
	if mode != kube.ExecuteDisabled {
		sink = approve.NewMemorySink()
		sink.SelfLogin = testSelfLogin
		// The gate's clock must be the cycle's pinned clock, not the wall clock. With
		// the default time.Now here, the simulated approval below is stamped at
		// fixedTime while the gate reconciles at the real instant — so the moment the
		// wall clock passes fixedTime plus ApprovalTTL, every approval in this file is
		// refused as expired. That is exactly how these tests went green in CI on
		// 2026-08-02 and red on every machine a week later.
		gate = approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy()).
			WithClock(func() time.Time { return fixedTime })
	}

	c, err := NewForTest(mode, newClient, factory.build, gate, audit.NewTrail(), fastPolicy, false,
		func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("building the cycle: %v", err)
	}
	return c, factory, sink
}

// singleClusterRegistry builds a one-cluster registry. Any existing regular file
// satisfies the kubeconfig existence check; the fake client builder ignores the path.
func singleClusterRegistry(t *testing.T) *cluster.Registry {
	t.Helper()
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{Name: testCluster, Kubeconfig: "operate_test.go", Context: "ctx"}},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	return reg
}

// crashloopingWorkload seeds a Deployment and one of its pods stuck in
// CrashLoopBackOff — the fault the deterministic pipeline answers with an
// OpRolloutRestart proposal, which is the most reversible action in the catalog and
// the one these tests drive.
func crashloopingWorkload() []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testDeploy, ResourceVersion: "100"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
			Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 0, UnavailableReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNamespace, Name: testPod, ResourceVersion: "101",
				OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: testDeploy}},
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

func ptr[T any](v T) *T { return &v }

// --- The central safety test. ---------------------------------------------------

// TestRun_DisabledBuildsNoExecutor is the assertion the whole opt-in rests on.
//
// The claim documented on this package and in config.example.yaml is not "execution is
// off by default" — that would be satisfied by a flag checked before each send, which
// is a thing a future edit can forget. The claim is that without the opt-in there is no
// write-capable object in the process AT ALL, so there is no call site to audit and
// nothing to hold onto by accident. That is a statement about whether the mutator
// builder is CALLED, and this is the test that reads the call log.
//
// It deliberately runs against a cluster that HAS a fault and DOES yield a proposal,
// because a disabled cycle that built no executor merely because it had nothing to
// propose would pass a weaker test while proving nothing.
func TestRun_DisabledBuildsNoExecutor(t *testing.T) {
	c, factory, sink := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	if sink != nil {
		t.Fatal("a disabled cycle must not build an approval sink")
	}

	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	cr := report.Clusters[0]
	if cr.Error != "" {
		t.Fatalf("unexpected cluster error: %s", cr.Error)
	}

	// The premise: this run had something it could have acted on.
	if len(cr.Proposals) == 0 {
		t.Fatal("the seeded crashloop produced no proposal, so this test proves nothing about the opt-in")
	}

	if len(factory.modes) != 0 {
		t.Errorf("a disabled cycle constructed %d write client(s) (modes: %v); it must construct none", len(factory.modes), factory.modes)
	}
	if len(factory.calls()) != 0 {
		t.Errorf("a disabled cycle sent %v; it must send nothing", factory.calls())
	}
}

// TestRun_DisabledProposesWithoutAsking checks the other half of the default posture:
// no approval artifact is opened either.
//
// Asking would be worse than useless. A decision collected while nothing can execute is
// a decision made in one context that would be honored in another — the operator who
// approved a restart on a propose-only deployment did not approve it on the day someone
// set MAKLAUDE_EXECUTE_MODE=enabled.
func TestRun_DisabledProposesWithoutAsking(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)

	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	cr := report.Clusters[0]

	if cr.Gate != (GateReport{}) {
		t.Errorf("a disabled cycle touched the approval gate: %+v", cr.Gate)
	}
	if len(cr.Executions) != 0 {
		t.Errorf("a disabled cycle reported %d execution(s); want none", len(cr.Executions))
	}
	if report.Mode != "disabled" {
		t.Errorf("report mode = %q, want %q", report.Mode, "disabled")
	}
	if report.Totals.Previewed != 0 || report.Totals.Mutated != 0 || report.Totals.Asked != 0 {
		t.Errorf("disabled totals should be all-zero for asked/previewed/mutated, got %+v", report.Totals)
	}
	if report.Totals.Proposals == 0 {
		t.Error("a disabled cycle should still report what it WOULD do")
	}
}

// --- The opted-in path. ----------------------------------------------------------

// TestRun_OptedInAsksAndExecutesNothingUnapproved is the first pass of the two-pass
// gate: MaKlaude previews, opens an artifact, and stops.
//
// The assertion that matters is not "nothing executed" but WHY nothing executed: an
// artifact exists, it carries no decision, and no authorization was issued. A cycle
// that silently failed to propose would also execute nothing.
func TestRun_OptedInAsksAndExecutesNothingUnapproved(t *testing.T) {
	c, factory, sink := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)

	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	cr := report.Clusters[0]
	if cr.Error != "" {
		t.Fatalf("unexpected cluster error: %s (previews: %v)", cr.Error, cr.PreviewErrors)
	}

	if cr.Gate.Opened == 0 {
		t.Fatalf("the cycle opened no approval request for %d proposal(s)", len(cr.Proposals))
	}
	if cr.Gate.Authorized != 0 {
		t.Fatalf("the gate issued %d authorization(s) on the pass that merely ASKED", cr.Gate.Authorized)
	}
	if len(cr.Executions) != 0 {
		t.Fatalf("an unapproved action executed: %+v", cr.Executions)
	}

	open, err := sink.ListOpen(context.Background())
	if err != nil {
		t.Fatalf("listing open artifacts: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected exactly 1 open approval artifact, got %d", len(open))
	}

	// The preview client was built, and it was built PREVIEW-ONLY. An execution-mode
	// cycle previewing through an execution-mode client would perform the action it
	// was showing to a human.
	if len(factory.modes) != 1 || factory.modes[0] != kube.ExecuteDryRun {
		t.Fatalf("write clients built with modes %v; want exactly one, dry-run", factory.modes)
	}
	if got := factory.realCalls(); len(got) != 0 {
		t.Fatalf("a real mutation was sent before anyone approved anything: %v", got)
	}
}

// TestRun_ExecutesWhatAHumanApproved drives the second pass: a human labels the
// artifact, the next cycle honors it, and exactly one real mutation is sent.
func TestRun_ExecutesWhatAHumanApproved(t *testing.T) {
	c, factory, sink := newCycle(t, kube.ExecuteEnabled, crashloopingWorkload()...)
	reg := singleClusterRegistry(t)
	ctx := context.Background()

	// Pass one: ask.
	if _, err := c.Run(ctx, reg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	open, err := sink.ListOpen(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected 1 open artifact after the first pass, got %d (err: %v)", len(open), err)
	}

	// The simulated human. MemorySink.Decide records an actor and an instant exactly as
	// a GitHub label event does; the instant is placed after the preview the body
	// displays, which is the ordering the gate checks.
	if err := sink.Decide(open[0].Ref, approve.ApprovedLabel, testApprover, fixedTime.Add(time.Second)); err != nil {
		t.Fatalf("recording the simulated approval: %v", err)
	}

	// Pass two: act.
	report, err := c.Run(ctx, reg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	cr := report.Clusters[0]
	if cr.Gate.Authorized != 1 {
		t.Fatalf("the gate issued %d authorization(s) after a human approved one action; want 1 (gate: %+v, error: %s)",
			cr.Gate.Authorized, cr.Gate, cr.Error)
	}
	if len(cr.Executions) != 1 {
		t.Fatalf("expected exactly 1 execution, got %d: %+v", len(cr.Executions), cr.Executions)
	}

	e := cr.Executions[0]
	if !e.Executed {
		t.Fatalf("the approved action did not execute: failure=%s error=%s", e.Failure, e.Error)
	}
	if e.Previewed {
		t.Error("an execution-enabled cycle reported its approved action as a preview")
	}
	if e.Approver != testApprover {
		t.Errorf("execution approver = %q, want %q", e.Approver, testApprover)
	}
	if e.Authority != approve.AuthorityHuman.String() {
		t.Errorf("execution authority = %q, want %q — a human approved this, not a policy",
			e.Authority, approve.AuthorityHuman.String())
	}
	if e.Operation != string(remediate.OpRolloutRestart) {
		t.Errorf("executed operation = %q, want %q", e.Operation, remediate.OpRolloutRestart)
	}

	// Exactly one real mutation, and it is the approved one. The preview client's
	// requests are excluded because they changed nothing.
	mutations := factory.realCalls()
	if len(mutations) != 1 {
		t.Fatalf("expected exactly 1 real mutating request, got %d: %v", len(mutations), mutations)
	}
	if want := "restart deployment/" + testNamespace + "/" + testDeploy; mutations[0] != want {
		t.Errorf("the real mutation was %q, want %q", mutations[0], want)
	}

	// The attempt reached the audit trail. An unaudited write path is a different
	// product, so this is checked rather than assumed.
	trail, ok := c.Trail().(*audit.Trail)
	if !ok {
		t.Fatalf("expected the test cycle's sink to be an *audit.Trail, got %T", c.Trail())
	}
	if trail.Len() == 0 {
		t.Error("a mutation landed and the audit trail is empty")
	}
	if report.Totals.Mutated != 1 {
		t.Errorf("report totals mutated = %d, want 1", report.Totals.Mutated)
	}
}

// TestRun_DryRunModeChangesNothing checks the rehearsal posture: the whole sequence
// runs, an approved action "executes", and no request that could change a cluster is
// ever sent.
//
// This is the mode most likely to be misread, which is why the report keeps Executed
// and DryRun as separate fields — the run genuinely did execute an approved action,
// and the cluster genuinely did not change.
func TestRun_DryRunModeChangesNothing(t *testing.T) {
	c, factory, sink := newCycle(t, kube.ExecuteDryRun, crashloopingWorkload()...)
	reg := singleClusterRegistry(t)
	ctx := context.Background()

	if _, err := c.Run(ctx, reg); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	open, err := sink.ListOpen(ctx)
	if err != nil || len(open) != 1 {
		t.Fatalf("expected 1 open artifact, got %d (err: %v)", len(open), err)
	}
	if err := sink.Decide(open[0].Ref, approve.ApprovedLabel, testApprover, fixedTime.Add(time.Second)); err != nil {
		t.Fatalf("recording the simulated approval: %v", err)
	}

	report, err := c.Run(ctx, reg)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	cr := report.Clusters[0]
	if len(cr.Executions) != 1 {
		t.Fatalf("expected 1 execution, got %d: %+v (gate: %+v)", len(cr.Executions), cr.Executions, cr.Gate)
	}
	// The distinction this mode exists to make: the action ran and was accepted
	// (Previewed), and no real mutation landed (Executed false). A report that set
	// Executed here would be the one lie that could block the real execution later.
	if e := cr.Executions[0]; !e.Previewed || e.Executed {
		t.Errorf("dry-run execution: previewed=%v executed=%v, want true and false (failure=%s error=%s)",
			e.Previewed, e.Executed, e.Failure, e.Error)
	}
	if got := factory.realCalls(); len(got) != 0 {
		t.Errorf("a dry-run cycle sent %v through a write-capable client", got)
	}
	for i, mode := range factory.modes {
		if mode != kube.ExecuteDryRun {
			t.Errorf("write client %d built in %s mode; a dry-run cycle must build only dry-run clients", i, mode)
		}
	}
	if report.Totals.Previewed != 1 || report.Totals.Mutated != 0 {
		t.Errorf("dry-run totals: previewed=%d mutated=%d, want 1 and 0", report.Totals.Previewed, report.Totals.Mutated)
	}
}

// TestRun_HealthyClusterBuildsNothing checks that a cluster with nothing wrong does not
// hold write authority open. An executor built for a pass with no proposals would carry
// the union of every action's authority for the life of the process, for no benefit.
func TestRun_HealthyClusterBuildsNothing(t *testing.T) {
	c, factory, _ := newCycle(t, kube.ExecuteEnabled)

	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := len(report.Clusters[0].Proposals); n != 0 {
		t.Fatalf("a healthy cluster produced %d proposal(s)", n)
	}
	if len(factory.modes) != 0 {
		t.Errorf("a pass with nothing to propose built %d write client(s): %v", len(factory.modes), factory.modes)
	}
}

// TestRun_PerClusterFailureIsIsolated checks multi-cluster isolation at this layer: an
// unreachable cluster is recorded against itself and the others are still processed.
func TestRun_PerClusterFailureIsIsolated(t *testing.T) {
	failing := "broken"
	newClient := func(h *cluster.Handle) (*kube.Client, error) {
		if h.Name() == failing {
			return nil, context.DeadlineExceeded
		}
		return kube.NewClientWithInterface(h.Name(), fake.NewSimpleClientset(crashloopingWorkload()...)), nil
	}
	factory := &mutatorFactory{}
	c, err := NewForTest(kube.ExecuteDisabled, newClient, factory.build, nil, audit.NewTrail(), fastPolicy, false,
		func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("building the cycle: %v", err)
	}

	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{
			{Name: failing, Kubeconfig: "operate_test.go", Context: "ctx"},
			{Name: testCluster, Kubeconfig: "operate_test.go", Context: "ctx"},
		},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}

	report, err := c.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(report.Clusters) != 2 {
		t.Fatalf("expected 2 cluster reports, got %d", len(report.Clusters))
	}
	if report.Clusters[0].Error == "" {
		t.Error("the unreachable cluster reported no error")
	}
	if report.Clusters[1].Error != "" {
		t.Errorf("the healthy cluster inherited an error: %s", report.Clusters[1].Error)
	}
	if len(report.Clusters[1].Proposals) == 0 {
		t.Error("one cluster's failure suppressed another cluster's proposals")
	}
}

// --- Construction. ----------------------------------------------------------------

func TestNewForTest_RejectsAnOptedInCycleWithNoGate(t *testing.T) {
	newClient := func(h *cluster.Handle) (*kube.Client, error) {
		return kube.NewClientWithInterface(h.Name(), fake.NewSimpleClientset()), nil
	}
	factory := &mutatorFactory{}

	for _, mode := range []kube.ExecuteMode{kube.ExecuteDryRun, kube.ExecuteEnabled} {
		if _, err := NewForTest(mode, newClient, factory.build, nil, audit.NewTrail(), fastPolicy, false, nil); err == nil {
			t.Errorf("NewForTest(%s, gate=nil) succeeded; an opted-in cycle with no gate could never ask anyone", mode)
		}
	}
	if _, err := NewForTest(kube.ExecuteDisabled, newClient, factory.build, nil, audit.NewTrail(), fastPolicy, false, nil); err != nil {
		t.Errorf("NewForTest(disabled, gate=nil) failed: %v — that is the shape New() produces", err)
	}
}

func TestExecuteModeFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    kube.ExecuteMode
		wantErr bool
	}{
		{"unset is disabled", "", kube.ExecuteDisabled, false},
		{"explicit disabled", "disabled", kube.ExecuteDisabled, false},
		{"dry-run", "dry-run", kube.ExecuteDryRun, false},
		{"enabled", "enabled", kube.ExecuteEnabled, false},
		{"case and space tolerant", "  ENABLED ", kube.ExecuteEnabled, false},
		// The important one: an unreadable value must not be guessed in either
		// direction. Guessing low silently ignores an operator who meant to enable
		// execution; guessing high needs no explanation.
		{"typo is an error, not a default", "enabledd", kube.ExecuteDisabled, true},
		{"true is not the vocabulary", "true", kube.ExecuteDisabled, true},
		{"1 is not the vocabulary", "1", kube.ExecuteDisabled, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExecuteModeFromEnv(func(k string) string {
				if k != ExecuteModeEnv {
					t.Fatalf("read the wrong environment variable: %q", k)
				}
				return tt.value
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExecuteModeFromEnv(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ExecuteModeFromEnv(%q) = %s, want %s", tt.value, got, tt.want)
			}
			if tt.wantErr && !strings.Contains(err.Error(), ExecuteModeEnv) {
				t.Errorf("the error does not name the variable an operator has to fix: %v", err)
			}
		})
	}
}

// --- Reporting. --------------------------------------------------------------------

// TestReport_TextStatesThePostureOutright checks the header, because the difference
// between a run that could have changed a cluster and one that could not is the single
// most consequential thing on the page and must not have to be inferred from the
// absence of a section further down.
func TestReport_TextStatesThePostureOutright(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"DISABLED", "no executor was built", "proposals (", "executions: none"} {
		if !strings.Contains(out, want) {
			t.Errorf("text report is missing %q:\n%s", want, out)
		}
	}
}

func TestReport_JSONRoundTrips(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("the JSON report does not round-trip: %v", err)
	}
	if back.Mode != "disabled" {
		t.Errorf("round-tripped mode = %q, want %q", back.Mode, "disabled")
	}
	if len(back.Clusters) != 1 || len(back.Clusters[0].Proposals) == 0 {
		t.Errorf("round-tripped report lost its proposals: %+v", back)
	}
}

func TestRun_NilRegistryIsAnError(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled)
	if _, err := c.Run(context.Background(), nil); err == nil {
		t.Error("Run(nil registry) returned no error")
	}
}
