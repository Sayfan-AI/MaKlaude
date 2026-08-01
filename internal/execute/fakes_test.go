package execute

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The fakes in this file are deliberately a MODEL of a cluster rather than a
// sequence of canned answers.
//
// The properties under test are things like "the action landed exactly once and the
// cluster then converged" and "the rollback put the node back". Both are statements
// about a world that changes when MaKlaude writes to it, and a fake that replays a
// fixed list of snapshots proves them only by construction — the test would pass
// against a runner that sent nothing at all. So the fake mutator mutates
// [clusterModel] the way the API server would, the fake observer reads it, and the
// assertions are about what the model ended up looking like.

// testCluster is the cluster name every fixture in this package uses.
const testCluster = "prod"

// call is one mutating request the fake write path received.
type call struct {
	Verb            string
	Namespace       string
	Name            string
	Patch           string
	RestartedAt     string
	ResourceVersion string
}

// clusterModel is a tiny mutable stand-in for a cluster: the objects the health
// snapshot would report, plus the resourceVersion bookkeeping that makes optimistic
// concurrency behave the way it does in reality.
type clusterModel struct {
	mu sync.Mutex

	name        string
	reachable   bool
	nodes       map[string]health.NodeSignal
	pods        map[string]health.PodSignal
	deployments map[string]health.DeploymentSignal
	replicaSets []health.ReplicaSetSignal

	// reads counts how many snapshots have been served, so a test can assert that an
	// aborted action still read the cluster exactly once.
	reads int

	// nextVersion produces the next resourceVersion a write assigns.
	nextVersion int
}

func newClusterModel() *clusterModel {
	return &clusterModel{
		name:        testCluster,
		reachable:   true,
		nodes:       make(map[string]health.NodeSignal),
		pods:        make(map[string]health.PodSignal),
		deployments: make(map[string]health.DeploymentSignal),
		nextVersion: 9000,
	}
}

// withNode adds a NotReady, schedulable node — the state a cordon proposal is made
// against.
func (c *clusterModel) withNode(name string) *clusterModel {
	c.nodes[name] = health.NodeSignal{Name: name, ResourceVersion: "1001", Ready: false, Unschedulable: false}
	return c
}

// withDeployment adds a fully-rolled-out deployment plus the ReplicaSet carrying its
// current revision.
func (c *clusterModel) withDeployment(namespace, name string, replicas int32, revision int64) *clusterModel {
	c.deployments[namespace+"/"+name] = health.DeploymentSignal{
		Namespace:         namespace,
		Name:              name,
		ResourceVersion:   "2002",
		DesiredReplicas:   replicas,
		ReadyReplicas:     replicas,
		UpdatedReplicas:   replicas,
		AvailableReplicas: replicas,
	}
	c.replicaSets = append(c.replicaSets, health.ReplicaSetSignal{
		Namespace:       namespace,
		Name:            name + "-old",
		ResourceVersion: "2003",
		Revision:        revision,
		Owners:          []health.OwnerRef{{Kind: "Deployment", Name: name, Controller: true}},
	})
	return c
}

// withCrashLoopingPod adds a crashlooping pod owned by a ReplicaSet, which is what a
// rollout-restart proposal's precondition names.
func (c *clusterModel) withCrashLoopingPod(namespace, name, owner string) *clusterModel {
	c.pods[namespace+"/"+name] = health.PodSignal{
		Namespace:       namespace,
		Name:            name,
		ResourceVersion: "3003",
		Phase:           "Running",
		Node:            "node-a",
		Owners:          []health.OwnerRef{{Kind: "ReplicaSet", Name: owner, Controller: true}},
		Containers:      []health.ContainerSignal{{Name: "app", CrashLooping: true, RestartCount: 7}},
	}
	return c
}

// withFailedPod adds a controller-owned pod in phase Failed — the state a delete
// proposal is made against.
func (c *clusterModel) withFailedPod(namespace, name, owner string) *clusterModel {
	c.pods[namespace+"/"+name] = health.PodSignal{
		Namespace:       namespace,
		Name:            name,
		ResourceVersion: "4004",
		Phase:           "Failed",
		Failed:          true,
		Node:            "node-a",
		Owners:          []health.OwnerRef{{Kind: "ReplicaSet", Name: owner, Controller: true}},
	}
	return c
}

// mutateNode applies a change to a node and bumps its resourceVersion, exactly as a
// successful write would.
func (c *clusterModel) mutateNode(name string, apply func(*health.NodeSignal)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	node, ok := c.nodes[name]
	if !ok {
		return
	}
	apply(&node)
	c.nextVersion++
	node.ResourceVersion = versionString(c.nextVersion)
	c.nodes[name] = node
}

// rollOut simulates the deployment controller reacting to a restart: a ReplicaSet
// with the next revision appears and the deployment reports it fully rolled out.
func (c *clusterModel) rollOut(namespace, name string, revision int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replicaSets = append(c.replicaSets, health.ReplicaSetSignal{
		Namespace: namespace,
		Name:      name + "-new",
		Revision:  revision,
		Owners:    []health.OwnerRef{{Kind: "Deployment", Name: name, Controller: true}},
	})
	dep := c.deployments[namespace+"/"+name]
	dep.UpdatedReplicas = dep.DesiredReplicas
	dep.ReadyReplicas = dep.DesiredReplicas
	dep.AvailableReplicas = dep.DesiredReplicas
	c.nextVersion++
	dep.ResourceVersion = versionString(c.nextVersion)
	c.deployments[namespace+"/"+name] = dep
}

// removePod deletes a pod from the model.
func (c *clusterModel) removePod(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pods, namespace+"/"+name)
}

// snapshot renders the model as a [health.Snapshot], with every slice sorted the way
// a real collector sorts it.
func (c *clusterModel) snapshot() health.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++

	snap := health.Snapshot{
		Cluster:     c.name,
		CollectedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Reachability: health.Reachability{
			Reachable:     c.reachable,
			ServerVersion: "v1.32.0",
		},
	}
	if !c.reachable {
		snap.Reachability.Error = "dial tcp 127.0.0.1:1: connect: connection refused"
		return snap
	}
	for _, n := range c.nodes {
		snap.Nodes = append(snap.Nodes, n)
	}
	for _, p := range c.pods {
		snap.Pods = append(snap.Pods, p)
	}
	for _, d := range c.deployments {
		snap.Deployments = append(snap.Deployments, d)
	}
	snap.ReplicaSets = append(snap.ReplicaSets, c.replicaSets...)

	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].Name < snap.Nodes[j].Name })
	sort.Slice(snap.Pods, func(i, j int) bool {
		if snap.Pods[i].Namespace != snap.Pods[j].Namespace {
			return snap.Pods[i].Namespace < snap.Pods[j].Namespace
		}
		return snap.Pods[i].Name < snap.Pods[j].Name
	})
	sort.Slice(snap.Deployments, func(i, j int) bool {
		if snap.Deployments[i].Namespace != snap.Deployments[j].Namespace {
			return snap.Deployments[i].Namespace < snap.Deployments[j].Namespace
		}
		return snap.Deployments[i].Name < snap.Deployments[j].Name
	})
	sort.Slice(snap.ReplicaSets, func(i, j int) bool { return snap.ReplicaSets[i].Name < snap.ReplicaSets[j].Name })
	return snap
}

// readCount returns how many snapshots have been served.
func (c *clusterModel) readCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// node returns a node from the model for assertions.
func (c *clusterModel) node(name string) health.NodeSignal {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodes[name]
}

// versionString renders a resourceVersion the way the API server's monotonic
// counter reads.
func versionString(n int) string {
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	if digits == "" {
		return "0"
	}
	return digits
}

// fakeObserver serves snapshots of a [clusterModel], with programmable failures.
type fakeObserver struct {
	model *clusterModel

	mu sync.Mutex
	// failNext is how many upcoming reads should fail before any succeed.
	failNext int
	// failFrom, when positive, fails every read from that read onwards — the shape of
	// a cluster that becomes unreadable after the action was sent.
	failFrom int
	// err is what a failing read returns.
	err error
	// beforeRead runs before each snapshot is taken, so a test can advance the model
	// on a chosen read — "the rollout completes on the third poll".
	beforeRead func(read int)
	reads      int
}

func (o *fakeObserver) Collect(_ context.Context) (health.Snapshot, error) {
	o.mu.Lock()
	o.reads++
	read := o.reads
	failing := o.failNext > 0 || (o.failFrom > 0 && read >= o.failFrom)
	if o.failNext > 0 {
		o.failNext--
	}
	before := o.beforeRead
	err := o.err
	o.mu.Unlock()

	if failing {
		return health.Snapshot{}, err
	}
	if before != nil {
		before(read)
	}
	return o.model.snapshot(), nil
}

// fakeMutator records every mutating request and applies it to the model, so an
// assertion about the cluster is an assertion about what MaKlaude actually sent.
type fakeMutator struct {
	model *clusterModel

	mu   sync.Mutex
	name string
	mode kube.ExecuteMode

	calls []call

	// err is returned instead of performing the action. failFirst limits it to that
	// many leading calls, so a test can express "fails once, then succeeds".
	err       error
	failFirst int
}

func newFakeMutator(model *clusterModel) *fakeMutator {
	return &fakeMutator{model: model, name: testCluster, mode: kube.ExecuteEnabled}
}

func (m *fakeMutator) Name() string           { return m.name }
func (m *fakeMutator) Mode() kube.ExecuteMode { return m.mode }
func (m *fakeMutator) callCount() int         { m.mu.Lock(); defer m.mu.Unlock(); return len(m.calls) }
func (m *fakeMutator) recorded() []call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]call(nil), m.calls...)
}
func (m *fakeMutator) lastCall(t *testing.T) call {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		t.Fatal("no mutating request was sent")
	}
	return m.calls[len(m.calls)-1]
}

// record registers a call and decides whether it fails, returning the outcome the
// write path would produce.
func (m *fakeMutator) record(c call, target string, apply func()) (*kube.Outcome, error) {
	m.mu.Lock()
	m.calls = append(m.calls, c)
	failing := m.err != nil && (m.failFirst == 0 || len(m.calls) <= m.failFirst)
	err := m.err
	mode := m.mode
	m.mu.Unlock()

	if failing {
		return nil, err
	}
	if apply != nil && mode != kube.ExecuteDryRun {
		apply()
	}
	return &kube.Outcome{
		Cluster:         m.name,
		Target:          target,
		Scope:           "PATCH /fake/" + target,
		ResourceVersion: c.ResourceVersion,
		DryRun:          mode == kube.ExecuteDryRun,
	}, nil
}

func (m *fakeMutator) RestartDeploymentRollout(_ context.Context, namespace, name, restartedAt, resourceVersion string) (*kube.Outcome, error) {
	return m.record(call{
		Verb: "restart", Namespace: namespace, Name: name,
		RestartedAt: restartedAt, ResourceVersion: resourceVersion,
	}, "deployment/"+namespace+"/"+name, nil)
}

func (m *fakeMutator) PatchDeployment(_ context.Context, namespace, name string, patch []byte, resourceVersion string) (*kube.Outcome, error) {
	return m.record(call{
		Verb: "patchdeployment", Namespace: namespace, Name: name,
		Patch: string(patch), ResourceVersion: resourceVersion,
	}, "deployment/"+namespace+"/"+name, nil)
}

func (m *fakeMutator) CordonNode(_ context.Context, name, resourceVersion string) (*kube.Outcome, error) {
	return m.record(call{Verb: "cordon", Name: name, ResourceVersion: resourceVersion}, "node/"+name, func() {
		m.model.mutateNode(name, func(n *health.NodeSignal) { n.Unschedulable = true })
	})
}

func (m *fakeMutator) PatchNode(_ context.Context, name string, patch []byte, resourceVersion string) (*kube.Outcome, error) {
	body := string(patch)
	return m.record(call{Verb: "patchnode", Name: name, Patch: body, ResourceVersion: resourceVersion}, "node/"+name, func() {
		if body == `{"spec":{"unschedulable":false}}` {
			m.model.mutateNode(name, func(n *health.NodeSignal) { n.Unschedulable = false })
		}
	})
}

func (m *fakeMutator) DeletePod(_ context.Context, namespace, name, resourceVersion string) (*kube.Outcome, error) {
	return m.record(call{
		Verb: "deletepod", Namespace: namespace, Name: name, ResourceVersion: resourceVersion,
	}, "pod/"+namespace+"/"+name, func() { m.model.removePod(namespace, name) })
}

// fakeRecorder stands in for the approval trail.
type fakeRecorder struct {
	mu      sync.Mutex
	details []string
	err     error
}

func (r *fakeRecorder) RecordExecution(_ context.Context, auth *approve.Authorization, detail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !auth.Valid() {
		panic("the runner recorded an execution against an authorization the gate never issued")
	}
	if r.err != nil {
		return r.err
	}
	r.details = append(r.details, detail)
	return nil
}

func (r *fakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.details)
}

func (r *fakeRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.details) == 0 {
		return ""
	}
	return r.details[len(r.details)-1]
}

// fastPolicy keeps the bounded waits short enough that a test finishes in
// milliseconds, while leaving the window generous relative to the interval so a slow
// machine cannot turn "converged on the third poll" into a timeout. The one test
// that WANTS a timeout sets its own window.
func fastPolicy() Policy {
	return Policy{
		ObserveWindow:   5 * time.Second,
		ObserveInterval: time.Millisecond,
		MaxAttempts:     3,
		RetryBackoff:    time.Millisecond,
	}
}

// harness bundles a runner with the fakes behind it, so a scenario reads as a
// sequence of events rather than as wiring.
type harness struct {
	t        *testing.T
	model    *clusterModel
	mutator  *fakeMutator
	observer *fakeObserver
	recorder *fakeRecorder
	runner   *Runner
}

func newHarness(t *testing.T, model *clusterModel, policy Policy) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		model:    model,
		mutator:  newFakeMutator(model),
		observer: &fakeObserver{model: model},
		recorder: &fakeRecorder{},
	}
	runner, err := New(h.mutator, h.observer, h.recorder, policy)
	if err != nil {
		t.Fatalf("building runner: %v", err)
	}
	h.runner = runner
	return h
}

// execute runs the proposal with a real permission slip minted by the real approval
// gate.
func (h *harness) execute(p remediate.Proposal) (Report, error) {
	h.t.Helper()
	return h.runner.Execute(context.Background(), authorizationFor(h.t, p), p)
}

// gate drives the real approval gate over an in-memory trail. Tests use it rather
// than a hand-built Authorization because the type cannot be forged outside the
// approve package — which is the property that makes it worth having, and would be
// worth nothing if the tests routed around it.
type gate struct {
	t    *testing.T
	sink *approve.MemorySink
	gk   *approve.Gatekeeper
	req  approve.Request
	now  time.Time
}

func newGate(t *testing.T, p remediate.Proposal) *gate {
	t.Helper()
	g := &gate{
		t:    t,
		sink: approve.NewMemorySink(),
		req:  approve.Request{Proposal: p},
		now:  time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
	}
	g.sink.SelfLogin = "maklaude-bot"
	g.gk = approve.NewGatekeeper(g.sink, notify.NewNopNotifier(), approve.DefaultPolicy()).
		WithClock(func() time.Time { return g.now })
	return g
}

// tryAuthorize opens the approval artifact, has a human approve it, and reconciles
// again — returning whatever the gate decided to issue, including nothing. It is
// separate from [gate.authorize] so a test can assert that the gate REFUSES
// something without the helper failing first.
func (g *gate) tryAuthorize() []*approve.Authorization {
	g.t.Helper()
	ctx := context.Background()

	if _, err := g.gk.Reconcile(ctx, []approve.Request{g.req}); err != nil {
		g.t.Fatalf("opening the approval request: %v", err)
	}
	open, err := g.sink.ListOpen(ctx)
	if err != nil || len(open) != 1 {
		g.t.Fatalf("expected exactly one open approval artifact, got %d (err=%v)", len(open), err)
	}
	if err := g.sink.Decide(open[0].Ref, approve.ApprovedLabel, "the-gigi", g.now.Add(time.Second)); err != nil {
		g.t.Fatalf("recording the human decision: %v", err)
	}
	g.now = g.now.Add(2 * time.Second)

	res, err := g.gk.Reconcile(ctx, []approve.Request{g.req})
	if err != nil {
		g.t.Fatalf("honoring the approval: %v", err)
	}
	return res.Authorized
}

// authorize collects the single permission slip the gate is expected to issue.
func (g *gate) authorize() *approve.Authorization {
	g.t.Helper()
	auths := g.tryAuthorize()
	if len(auths) != 1 {
		g.t.Fatalf("the gate issued %d authorizations, want 1", len(auths))
	}
	return auths[0]
}

// artifact returns the current state of the single approval artifact.
func (g *gate) artifact() approve.ArtifactView {
	g.t.Helper()
	open, err := g.sink.ListOpen(context.Background())
	if err != nil || len(open) == 0 {
		g.t.Fatalf("no open approval artifact (err=%v)", err)
	}
	view, ok := g.sink.Snapshot(open[0].Ref)
	if !ok {
		g.t.Fatalf("no snapshot for %q", open[0].Ref)
	}
	return view
}

// authorizationFor mints a real permission slip for a proposal.
func authorizationFor(t *testing.T, p remediate.Proposal) *approve.Authorization {
	t.Helper()
	return newGate(t, p).authorize()
}

// cordonProposal is the canonical reversible action: cordon a NotReady node. It is
// the only operation in the catalog MaKlaude can undo itself, so it carries most of
// the rollback coverage.
func cordonProposal() remediate.Proposal {
	target := remediate.Target{Cluster: testCluster, Kind: "node", Name: "node-a", ResourceVersion: "1001"}
	return remediate.Proposal{
		Identity:       remediate.ProposalIdentity("proposal|cordonnode|" + testCluster + "|node/node-a"),
		Cluster:        testCluster,
		Operation:      remediate.OpCordonNode,
		Target:         target,
		Reversibility:  remediate.ReversibilityReversible,
		Title:          "Cordon NotReady node",
		Intent:         "Node node-a is NotReady and the scheduler keeps assigning pods to it.",
		ExpectedEffect: "Node node-a is marked unschedulable. Running pods are not touched.",
		Preconditions: []remediate.Precondition{
			{Kind: remediate.PreconditionUnchanged, Expect: "1001", Description: "node/node-a is still at resourceVersion 1001."},
			{Kind: remediate.PreconditionNodeNotReady, Description: "Node node-a is still NotReady."},
			{Kind: remediate.PreconditionNodeSchedulable, Description: "Node node-a is not already cordoned."},
		},
		ProposedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	}
}

// restartProposal is the canonical rollout restart.
func restartProposal() remediate.Proposal {
	target := remediate.Target{Cluster: testCluster, Kind: "deployment", Namespace: "shop", Name: "web", ResourceVersion: "2002"}
	return remediate.Proposal{
		Identity:       remediate.ProposalIdentity("proposal|rolloutrestart|" + testCluster + "|deployment/shop/web"),
		Cluster:        testCluster,
		Operation:      remediate.OpRolloutRestart,
		Target:         target,
		Reversibility:  remediate.ReversibilityReversible,
		Title:          "Restart deployment rollout",
		Intent:         "Pod shop/web-abc is crashlooping and nothing explained why.",
		ExpectedEffect: "Deployment shop/web performs a rolling restart.",
		Preconditions: []remediate.Precondition{
			{Kind: remediate.PreconditionUnchanged, Expect: "2002", Description: "deployment/shop/web is still at resourceVersion 2002."},
			{Kind: remediate.PreconditionPodCrashLooping, Expect: "shop/web-abc", Description: "Pod shop/web-abc is still crashlooping."},
		},
		ProposedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	}
}

// deletePodProposal is the canonical recreated-by-controller action.
func deletePodProposal() remediate.Proposal {
	target := remediate.Target{Cluster: testCluster, Kind: "pod", Namespace: "shop", Name: "web-dead", ResourceVersion: "4004"}
	return remediate.Proposal{
		Identity:       remediate.ProposalIdentity("proposal|deletepod|" + testCluster + "|pod/shop/web-dead"),
		Cluster:        testCluster,
		Operation:      remediate.OpDeletePod,
		Target:         target,
		Reversibility:  remediate.ReversibilityRecreatedByController,
		Title:          "Delete failed pod so its controller recreates it",
		Intent:         "Pod shop/web-dead is Failed and will not recover.",
		ExpectedEffect: "Pod shop/web-dead is deleted; its ReplicaSet creates a replacement.",
		Preconditions: []remediate.Precondition{
			{Kind: remediate.PreconditionUnchanged, Expect: "4004", Description: "pod/shop/web-dead is still at resourceVersion 4004."},
			{Kind: remediate.PreconditionPodFailed, Description: "Pod shop/web-dead is still failed."},
			{Kind: remediate.PreconditionPodHasController, Expect: "ReplicaSet/web-7d9", Description: "Pod shop/web-dead is still controlled by ReplicaSet web-7d9."},
		},
		ProposedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	}
}

// revisionRollbackProposal is the operation this layer deliberately refuses; see the
// note on [operationPlans].
func revisionRollbackProposal() remediate.Proposal {
	target := remediate.Target{Cluster: testCluster, Kind: "deployment", Namespace: "shop", Name: "web", ResourceVersion: "2002"}
	return remediate.Proposal{
		Identity:      remediate.ProposalIdentity("proposal|rollbackrevision|" + testCluster + "|deployment/shop/web"),
		Cluster:       testCluster,
		Operation:     remediate.OpRollbackRevision,
		Target:        target,
		Reversibility: remediate.ReversibilityReversible,
		Title:         "Roll deployment back one revision",
		Preconditions: []remediate.Precondition{
			{Kind: remediate.PreconditionUnchanged, Expect: "2002", Description: "deployment/shop/web is still at resourceVersion 2002."},
			{Kind: remediate.PreconditionRevisionExists, Expect: "4", Description: "Revision 4 still exists."},
		},
		ProposedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
	}
}
