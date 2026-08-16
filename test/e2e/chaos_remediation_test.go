//go:build e2e

// T5's timing case on a live cluster: a real Chaos Mesh experiment destroys the target
// of an approved remediation, and MaKlaude must abandon the action having sent nothing.
//
// # What this asserts that internal/execute/faultinjection_test.go cannot
//
// That file defines all three timing windows against a cluster model with a faithful
// optimistic-concurrency implementation, and it says plainly why its faults are
// modelled rather than injected: window 2 is the interval between one read and one
// write on the same goroutine, and no real injector can be aimed at microseconds. It
// routes the live half here, for the windows a real experiment CAN reach.
//
// Window 1 is that window. The fault lands before MaKlaude's own precondition re-check,
// so the mechanism under test is MaKlaude's — not the API server's — and the cost of
// getting it wrong is a mutating request against an object nobody approved in the state
// it is now in. What a model cannot supply here is the fault itself: the pod is removed
// by Chaos Mesh's controller with Chaos Mesh's privileges, on a schedule nothing in
// this process controls, and the snapshot MaKlaude re-reads comes from a real API
// server rather than from a fixture that was mutated by the test that asserts on it.
//
// # Why "nothing was sent" is asserted on the wire and not from the report
//
// A report saying "aborted" is the easiest thing for a broken implementation to
// produce — faultinjection_test.go makes the same point about its request counter. Its
// counter is a fake mutator it owns; the live equivalent is a recording reverse proxy,
// so the whole executor identity is registered through the proxy built for T8 step 1
// (chaos_eligibility_test.go) and every request MaKlaude makes for this scenario is
// visible with its method and its query.
//
// The query is what makes the assertion precise rather than merely strict. The approval
// artifact a human reads is backed by a server-side dry run, which is a genuine
// mutating request that client-go marks with `dryRun=All` in the URL. So the rule is
// not "no PATCH arrived" — one must, or the human approved nothing — it is: every
// mutating request on this route carries the dry-run marker. A real write is the one
// shape that must be absent, and its absence is corroborated from the object's own
// generation, which the API server increments on any spec change.
//
// # Why the remediated fault is seeded and the chaos fault is injected
//
// Two faults appear in this test and they have different jobs. The crashloop is what
// MaKlaude is asked to fix; it is seeded (manifests/chaos-remediation-target.yaml)
// because whether a chaos action can PRODUCE a remediable fault, and whether the fix
// then worked, is T6's question and is scored elsewhere. The pod-kill is the fault that
// lands *during* remediation, and it is the one that has to be real: this test exists
// because a hand-mutated fixture cannot prove anything about a cluster moving
// underneath a run.
package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	// chaosRemediationCluster is how the proxied executor identity is registered. It is
	// deliberately neither chaosClusterName nor executorClusterName: three registrations
	// of one physical cluster exist in this job, and a mix-up must surface as a missing
	// cluster or as an ErrClusterMismatch rather than as a request travelling a route
	// nobody is watching.
	chaosRemediationCluster = "maklaude-e2e-chaos-remediation"

	// chaosRemediationContext is the context name inside the proxied kubeconfig.
	chaosRemediationContext = "proxied"

	// chaosRemediationDeploy is the crashlooping Deployment this scenario remediates.
	// See manifests/chaos-remediation-target.yaml for why it crashloops rather than
	// carrying a bad image.
	chaosRemediationDeploy = "remediable"

	// crashLoopDeadline bounds the wait for MaKlaude's OWN detector to call the seeded
	// pod crashlooping. Asking the detector rather than kubectl is what makes the arming
	// condition the same fact the proposal will rest on: a pod that Kubernetes reports as
	// CrashLoopBackOff and that MaKlaude does not classify as crashlooping would produce
	// no proposal, and this wait would (correctly) expire.
	crashLoopDeadline = 180 * time.Second
	crashLoopInterval = 3 * time.Second

	// faultDeadline bounds the wait for the pod-kill to actually remove the pod. It is
	// the scenario's positive control: an experiment Chaos Mesh admitted but never acted
	// on would leave every assertion below satisfied by a cluster that never moved.
	faultDeadline = 120 * time.Second
	faultInterval = 3 * time.Second

	// driftCycles bounds how many times propose → preview → approve is re-driven when
	// the target moves before authorization. A crashlooping workload churns, so a stale
	// resourceVersion here is the system working; the scenario needs an authorization in
	// hand before the fault is injected, which is what these cycles buy.
	driftCycles = 4
)

// chaosRemediationSelector matches the remediable deployment's pods and nothing else —
// in particular not the victim deployment the lifecycle test breaks in the same
// namespace.
var chaosRemediationSelector = map[string]string{"scenario": "chaos-remediation"}

// mutatingMethods are the HTTP methods a write travels as. Recorded requests are
// classified by method rather than by path, because a path allowlist would have to
// enumerate the API surface and would go stale the moment the catalog grew.
var mutatingMethods = map[string]bool{
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
}

// dryRunMarker is what client-go puts in the URL for a server-side dry run of a patch.
// It is the wire-visible difference between the preview a human approved and the write
// that must never have happened.
const dryRunMarker = "dryRun=All"

// podKillExperiment is the fault that lands mid-remediation: one-shot, no duration
// (Chaos Mesh ignores spec.duration for pod-kill and internal/chaos refuses to set
// one), aimed at the single pod the proposal names.
func podKillExperiment() chaos.Experiment {
	return chaos.Experiment{
		Action:    chaos.ActionPodKill,
		Namespace: chaosObjectNamespace,
		Mode:      chaos.ModeAll,
		Selector: chaos.Selector{
			Namespaces:     []string{chaosTargetNamespace},
			LabelSelectors: chaosRemediationSelector,
		},
	}
}

// TestE2E_ChaosDriftsAnApprovedRemediation is T5 window 1, live.
func TestE2E_ChaosDriftsAnApprovedRemediation(t *testing.T) {
	requireChaosCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	flow := newProxiedExecutorFlow(t)

	// --- Arm the scenario: the seeded fault is one MaKlaude sees. ---
	target := awaitCrashLoopingPod(t, flow.collector)
	t.Logf("armed: MaKlaude reports pod %s/%s crashlooping", target.Namespace, target.Name)

	// --- Propose, preview, and get a real human-signed authorization. ---
	proposal, auth := authorizeRestart(t, flow)
	t.Logf("authorized: %s on %s under %s", proposal.Operation, proposal.Target.String(), auth.Authority())

	// The state a real write would necessarily change, captured while the action is
	// authorized and before anything could have run.
	before := deployState(readNamespacedDeployment(t, flow.reader, chaosTargetNamespace, chaosRemediationDeploy))

	// --- The fault. A real experiment, through the chaos identity, on its own route. ---
	//
	// buildChaosInjector registers the cluster separately and unproxied, which is what
	// keeps the two identities' traffic separable: the proxy this test watches carries
	// the executor identity only, so a chaos write cannot be mistaken for a remediation
	// write or vice versa.
	injector := buildChaosInjector(t, kube.ExecuteEnabled)
	experiment := podKillExperiment()
	injected, err := injector.Inject(ctx, experiment)
	if err != nil {
		t.Fatalf("injecting the mid-remediation pod-kill: %v", err)
	}
	t.Cleanup(func() {
		if _, err := injector.Remove(context.Background(), *injected); err != nil {
			t.Logf("cleanup teardown of %s: %v (the fault is one-shot and already over)", injected.Name, err)
		}
	})
	t.Logf("injected %s %s/%s uid=%s", injected.Kind, injected.Namespace, injected.Name, injected.UID)

	awaitPodGone(t, flow.collector, target)

	// --- Execute. The approval is valid, the world is not what it described. ---
	report, err := flow.runner.Execute(ctx, auth, proposal)

	if !errors.Is(err, execute.ErrPreconditionDrift) {
		t.Fatalf("a pod-kill before the re-check returned %v, want execute.ErrPreconditionDrift (report: %s)", err, report)
	}
	if report.Failure != execute.FailureDrifted {
		t.Fatalf("failure = %s, want drifted", report.Failure)
	}
	if !report.CleanAbort() {
		t.Error("a fault caught by MaKlaude's own re-check must read as a clean abort: the next cycle re-proposes, a human is not woken")
	}
	if report.Executed || report.Recorded || report.Attempts != 0 {
		t.Errorf("an aborted action reports executed=%t recorded=%t attempts=%d, want false/false/0",
			report.Executed, report.Recorded, report.Attempts)
	}
	if report.Convergence != execute.ConvergenceUnobserved {
		t.Errorf("convergence = %s, want unobserved — nothing ran", report.Convergence)
	}

	// The refusal must name the vanished pod. "A precondition failed" leaves an operator
	// holding an escalation they cannot correlate against the experiment they just ran.
	drifted := report.DriftedPreconditions()
	if len(drifted) == 0 {
		t.Fatalf("the report claims drift and lists no drifted precondition: %+v", report.Preconditions)
	}
	if !driftMentions(drifted, target.Name) || !driftMentions(drifted, "no longer present") {
		t.Errorf("the drift report does not say pod %s is gone: %s", target.Name, renderDrift(drifted))
	}
	t.Logf("refused, naming what moved: %s", renderDrift(drifted))

	// --- The assertions that carry the weight. ---
	assertOnlyPreviewsReachedTheCluster(t, flow.proxy)
	assertDeploymentUntouched(t, flow.reader, before)
}

// proxiedExecutorFlow is the whole remediation stack, wired so that every request it
// makes passes through one recording proxy.
type proxiedExecutorFlow struct {
	proxy     *recordingProxy
	reader    *kube.Client
	collector *health.Collector
	previewer *kube.Executor
	gate      *approve.Gatekeeper
	sink      *approve.MemorySink
	runner    *execute.Runner
}

// newProxiedExecutorFlow registers the maklaude-executor identity behind a recording
// proxy and builds the read, preview, approve and execute layers on that one handle.
//
// One handle for all of them is required rather than tidy: execute.Runner checks that
// the proposal, the permission slip and the write client all name the same registered
// cluster, so splitting observation onto the read-only kubeconfig would name a
// different cluster and be refused with ErrClusterMismatch (see remediation_test.go).
// The executor identity can read — deploy/rbac/write binds it the same read-only
// ClusterRole the observer uses — so one credential serves the whole flow.
func newProxiedExecutorFlow(t *testing.T) *proxiedExecutorFlow {
	t.Helper()

	kubeconfig := env(t, "MAKLAUDE_E2E_EXECUTOR_KUBECONFIG")
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")

	upstream, transport := apiServerTransport(t, kubeconfig, contextName)
	proxy := newRecordingProxy(t, upstream, transport)
	proxied := writeSingleProxiedKubeconfig(t, proxy, "maklaude-executor",
		bearerTokenFor(t, kubeconfig, contextName))

	reg, err := cluster.NewRegistry(&cluster.Config{Clusters: []cluster.Spec{{
		Name:       chaosRemediationCluster,
		Kubeconfig: proxied,
		Context:    chaosRemediationContext,
	}}})
	if err != nil {
		t.Fatalf("building a registry over the proxied executor kubeconfig: %v", err)
	}
	handle, ok := reg.Get(chaosRemediationCluster)
	if !ok {
		t.Fatalf("cluster %q missing from the proxied registry", chaosRemediationCluster)
	}

	reader, err := kube.NewClient(handle)
	if err != nil {
		t.Fatalf("building the read-only client on the proxied route: %v", err)
	}
	executor, err := kube.NewExecutor(handle, kube.ExecuteEnabled)
	if err != nil {
		t.Fatalf("building the write-enabled executor: %v", err)
	}
	previewer, err := kube.NewExecutor(handle, kube.ExecuteDryRun)
	if err != nil {
		t.Fatalf("building the preview-only executor: %v", err)
	}

	sink := approve.NewMemorySink()
	sink.SelfLogin = e2eSelfLogin
	gate := approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy())

	collector := health.NewCollector(reader)
	// The observation window is small on purpose. This action must never run, so a
	// window that is ever waited out is itself a failure — and a generous one would
	// only make that failure slow. Convergence after a real write is the gated
	// remediation suite's assertion, on the cluster built for it.
	runner, err := execute.New(executor, collector, gate, audit.NewTrail(), execute.Policy{
		ObserveWindow:   15 * time.Second,
		ObserveInterval: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("building the execution runner: %v", err)
	}

	return &proxiedExecutorFlow{
		proxy: proxy, reader: reader, collector: collector,
		previewer: previewer, gate: gate, sink: sink, runner: runner,
	}
}

// writeSingleProxiedKubeconfig writes a one-cluster kubeconfig pointing at a proxy,
// carrying the identity's own bearer token.
//
// It is the single-route sibling of [writeProxiedKubeconfig], and the two properties
// that file explains hold here for the same reasons: https, because clientcmd reads a
// context's auth info only for a TLS transport and a plain-HTTP proxy would silently
// turn MaKlaude into system:anonymous; and a pinned certificate rather than
// insecure-skip-tls-verify, because a test about what reaches a cluster should not be
// the one place that stops checking which cluster it is.
func writeSingleProxiedKubeconfig(t *testing.T, p *recordingProxy, user, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "proxied-executor.kubeconfig")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
clusters:
  - name: proxied-cluster
    cluster:
      server: %s
      certificate-authority-data: %s
contexts:
  - name: %s
    context:
      cluster: proxied-cluster
      user: %s
users:
  - name: %s
    user:
      token: %s
`, chaosRemediationContext, p.URL, base64.StdEncoding.EncodeToString(p.caPEM),
		chaosRemediationContext, user, user, token)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the proxied executor kubeconfig: %v", err)
	}
	return path
}

// awaitCrashLoopingPod waits until MaKlaude's own collector reports a crashlooping pod
// belonging to the remediable Deployment, and returns it.
//
// The pod is identified by name here and nowhere else afterwards, because the name is
// what the proposal's precondition will carry and what the pod-kill will destroy.
func awaitCrashLoopingPod(t *testing.T, collector *health.Collector) health.PodSignal {
	t.Helper()

	deadline := time.Now().Add(crashLoopDeadline)
	var lastSeen string
	for {
		snap, err := collectSnapshot(t, collector)
		if err == nil {
			for _, pod := range snap.Pods {
				if pod.Namespace != chaosTargetNamespace || !strings.HasPrefix(pod.Name, chaosRemediationDeploy+"-") {
					continue
				}
				lastSeen = fmt.Sprintf("pod %s/%s phase %q", pod.Namespace, pod.Name, pod.Phase)
				for i := range pod.Containers {
					if pod.Containers[i].CrashLooping {
						return pod
					}
				}
			}
		} else {
			lastSeen = "collection failed: " + err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pod of deployment %s/%s was reported crashlooping within %s (last: %s). "+
				"Either the seed was not applied or MaKlaude's crashloop detector no longer classifies an exit-1 loop as one.",
				chaosTargetNamespace, chaosRemediationDeploy, crashLoopDeadline, lastSeen)
		}
		time.Sleep(crashLoopInterval)
	}
}

// awaitPodGone waits until the pod the proposal names is absent from MaKlaude's view.
//
// This is the positive control for the fault, and it fails the test rather than warning:
// unlike the pod-level EFFECT of a pod-failure — which chaos_test.go corroborates and
// deliberately does not require — a pod-kill that removed nothing makes every assertion
// in this test vacuous. There would be no drift to catch, and "MaKlaude sent no write"
// would be true of a run in which nothing had happened at all.
func awaitPodGone(t *testing.T, collector *health.Collector, target health.PodSignal) {
	t.Helper()

	deadline := time.Now().Add(faultDeadline)
	for {
		snap, err := collectSnapshot(t, collector)
		if err == nil {
			present := false
			for _, pod := range snap.Pods {
				if pod.Namespace == target.Namespace && pod.Name == target.Name {
					present = true
					break
				}
			}
			if !present {
				t.Logf("the experiment removed pod %s/%s; the approved action's target no longer exists",
					target.Namespace, target.Name)
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s still exists %s after the pod-kill was admitted, so this scenario is not armed: "+
				"there is no drift for the re-check to catch and every assertion below would pass vacuously",
				target.Namespace, target.Name, faultDeadline)
		}
		time.Sleep(faultInterval)
	}
}

// collectSnapshot takes one live snapshot, returning the collection error rather than
// failing, so the callers above can distinguish "not yet" from "never".
func collectSnapshot(t *testing.T, collector *health.Collector) (health.Snapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	snap, err := collector.Collect(ctx)
	if err != nil {
		return snap, err
	}
	if !snap.Reachability.Reachable {
		return snap, fmt.Errorf("cluster unreachable: %s", snap.Reachability.Error)
	}
	return snap, nil
}

// authorizeRestart drives the production pipeline to a proposal and the real gate to a
// human-signed authorization for it.
//
// Every stage is the shipped one — health.Collector, detect.Analyze,
// correlate.Correlate, diagnose.Diagnose, remediate.Hypotheses, approve.Gatekeeper —
// for the reason remediation_test.go gives about its own rollback: a hand-built
// proposal would authorize and execute identically while proving nothing about the
// layers underneath. The [approve.Authorization] in particular cannot be forged outside
// its package, which is the property that makes it worth having.
//
// The cycles exist because a crashlooping workload churns: a preview conditioned on a
// resourceVersion the API server has already moved past is drift caught before the
// fault, which is the system working rather than a flake to smooth over. It is also NOT
// the property under test, so it is re-driven until an authorization is in hand.
func authorizeRestart(t *testing.T, flow *proxiedExecutorFlow) (remediate.Proposal, *approve.Authorization) {
	t.Helper()
	ctx := context.Background()

	var lastAbort string
	for cycle := 1; cycle <= driftCycles; cycle++ {
		proposal := restartProposal(t, flow.collector)

		preview, ok := previewRestart(t, flow.previewer, proposal)
		if !ok {
			lastAbort = fmt.Sprintf("cycle %d: the preview hit a stale resourceVersion", cycle)
			t.Log(lastAbort + "; re-observing")
			continue
		}
		req := approve.Request{Proposal: proposal, Preview: preview}

		asked, err := flow.gate.Reconcile(ctx, []approve.Request{req})
		if err != nil {
			t.Fatalf("cycle %d: opening the approval request: %v", cycle, err)
		}
		if cycle == 1 && len(asked.Authorized) != 0 {
			t.Fatalf("the gate issued %d authorization(s) on the pass that merely ASKED; nothing may be authorized before a human acts",
				len(asked.Authorized))
		}
		ref := soleOpenArtifact(t, flow.sink)

		// The simulated human, an instant in the future so the approval is unambiguously
		// after the preview the artifact displays — the ordering the gate checks and that
		// an RFC3339-second wall clock could otherwise make a coin flip.
		if err := flow.sink.Decide(ref, approve.ApprovedLabel, e2eApprover, time.Now().UTC().Add(time.Second)); err != nil {
			t.Fatalf("cycle %d: recording the simulated approval on %q: %v", cycle, ref, err)
		}

		honored, err := flow.gate.Reconcile(ctx, []approve.Request{req})
		if err != nil {
			t.Fatalf("cycle %d: honoring the approval: %v", cycle, err)
		}
		if len(honored.Authorized) == 0 {
			lastAbort = fmt.Sprintf("cycle %d: the gate refused the approval (%s)", cycle, honored)
			t.Log(lastAbort + "; re-observing")
			continue
		}
		if len(honored.Authorized) != 1 {
			t.Fatalf("cycle %d: the gate issued %d authorizations for one proposal, want 1", cycle, len(honored.Authorized))
		}
		auth := honored.Authorized[0]
		assertHumanAuthority(t, auth, proposal)
		return proposal, auth
	}

	t.Fatalf("no authorization was obtained within %d cycles; last abort: %s", driftCycles, lastAbort)
	return remediate.Proposal{}, nil
}

// restartProposal runs the read-only pipeline and returns the rollout restart it
// proposes for the remediable Deployment.
func restartProposal(t *testing.T, collector *health.Collector) remediate.Proposal {
	t.Helper()

	snap, err := collectSnapshot(t, collector)
	if err != nil {
		t.Fatalf("collecting cluster health: %v", err)
	}

	findings := detect.Analyze(snap)
	var hypotheses []diagnose.Hypothesis
	for _, incident := range correlate.Correlate(snap, findings) {
		hypotheses = append(hypotheses, diagnose.Diagnose(snap, incident)...)
	}

	proposals := remediate.Hypotheses(snap, hypotheses)
	for _, p := range proposals {
		if p.Operation == remediate.OpRolloutRestart &&
			p.Target.Kind == "deployment" && p.Target.Namespace == chaosTargetNamespace && p.Target.Name == chaosRemediationDeploy {
			if !namesAPodPrecondition(p) {
				t.Fatalf("the proposal for %s carries no pod precondition, so a pod-kill could not drift it: %+v",
					p.Target.String(), p.Preconditions)
			}
			return p
		}
	}
	t.Fatalf("the pipeline proposed no %s for deployment %s/%s. It proposed: %s. "+
		"Either the seed is not crashlooping, or the diagnosis landed on a specialized cause — only %s reaches the restart rule.",
		remediate.OpRolloutRestart, chaosTargetNamespace, chaosRemediationDeploy,
		renderProposals(proposals), diagnose.CauseUnknown)
	return remediate.Proposal{}
}

// namesAPodPrecondition reports whether the proposal is drift-able by a fault that
// destroys a pod. Asserted rather than assumed: if the restart rule ever stopped
// pinning the crashlooping pod, this scenario would still pass — on the deployment's
// resourceVersion having moved, which a pod-kill does not reliably do — and would
// quietly stop testing window 1.
func namesAPodPrecondition(p remediate.Proposal) bool {
	for _, pc := range p.Preconditions {
		if pc.Kind == remediate.PreconditionPodCrashLooping {
			return true
		}
	}
	return false
}

// previewRestart sends the approved action as a server-side dry run: a real request to
// the real API server, admitted by real admission controllers, that applies nothing.
//
// ok is false only for a stale resourceVersion, which means "ask again". Any other
// rejection fails the test — a preview the API server refuses on its merits is a real
// problem with the action, and the gate would correctly decline to authorize it.
func previewRestart(t *testing.T, previewer *kube.Executor, p remediate.Proposal) (approve.Preview, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	restartedAt := time.Now().UTC().Format(time.RFC3339)
	out, err := previewer.RestartDeploymentRollout(ctx, p.Target.Namespace, p.Target.Name, restartedAt, p.Target.ResourceVersion)
	switch {
	case errors.Is(err, kube.ErrPreconditionConflict):
		return approve.Preview{}, false
	case err != nil:
		t.Fatalf("the dry-run preview of a rollout restart of %s was rejected: %v", p.Target.String(), err)
	case out == nil:
		t.Fatalf("the preview returned neither an outcome nor an error")
	case !out.DryRun:
		t.Fatalf("PREVIEW VIOLATION: the preview-only executor reported a REAL mutation of %s: %+v", p.Target.String(), out)
	}

	return approve.Preview{
		Performed: true,
		Summary: fmt.Sprintf("The API server accepted a dryRun=All rollout restart of %s under scope `%s`, conditioned on resourceVersion %s. Nothing was applied.",
			p.Target.String(), out.Scope, out.ResourceVersion),
	}, true
}

// assertOnlyPreviewsReachedTheCluster is the wire-level half of "nothing was sent".
//
// Three checks, and the two controls matter as much as the prohibition. A route that
// recorded nothing at all would satisfy the prohibition while proving that the proxy,
// not MaKlaude, was silent — so reads must be present. A route that recorded no
// mutating request at all would mean the preview never travelled it either, and a
// detector that has never seen the shape it is looking for has not been exercised — so
// a dry-run write must be present. Only then does the absence of an unmarked mutating
// request mean what it claims.
func assertOnlyPreviewsReachedTheCluster(t *testing.T, p *recordingProxy) {
	t.Helper()

	var reads, previews int
	for _, r := range p.recorded() {
		switch {
		case !mutatingMethods[r.Method]:
			reads++
		case strings.Contains(r.Query, dryRunMarker):
			previews++
		default:
			t.Errorf("A REAL WRITE reached the cluster for an action MaKlaude reported as aborted: %s", r)
		}
	}

	if reads == 0 {
		t.Error("the proxy recorded no read at all, so MaKlaude's traffic did not travel this route and every assertion here is vacuous")
	}
	if previews == 0 {
		t.Error("the proxy recorded no dry-run write, so the approval preview never travelled this route and the real-write detector was never armed")
	}
	t.Logf("the executor route carried %d read(s) and %d dry-run write(s), and no real write", reads, previews)
}

// assertDeploymentUntouched corroborates the wire assertion from the object itself. The
// generation is the sharper of the two signals: the API server increments it on any
// spec change, which is exactly what a rollout restart performs.
func assertDeploymentUntouched(t *testing.T, reader *kube.Client, before deploymentState) {
	t.Helper()

	after := deployState(readNamespacedDeployment(t, reader, chaosTargetNamespace, chaosRemediationDeploy))
	if after.generation != before.generation {
		t.Errorf("deployment %s/%s moved from generation %d to %d, so a spec change landed for an action that was aborted",
			chaosTargetNamespace, chaosRemediationDeploy, before.generation, after.generation)
	}
	if got := after.annotations["kubectl.kubernetes.io/restartedAt"]; got != before.annotations["kubectl.kubernetes.io/restartedAt"] {
		t.Errorf("the pod template's restartedAt annotation changed to %q, which is what a rollout restart stamps", got)
	}
}

// readNamespacedDeployment fetches one Deployment from an arbitrary namespace through a
// read-only client. It is the namespace-taking counterpart of readDeployment, which is
// pinned to the main suite's own namespace.
func readNamespacedDeployment(t *testing.T, c *kube.Client, namespace, name string) *appsv1.Deployment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	deploys, err := c.ListDeployments(ctx, namespace)
	if err != nil {
		t.Fatalf("listing deployments in %s: %v", namespace, err)
	}
	for i := range deploys {
		if deploys[i].Name == name {
			return &deploys[i]
		}
	}
	t.Fatalf("deployment %s/%s not found (was manifests/chaos-remediation-target.yaml applied?)", namespace, name)
	return nil
}

// driftMentions reports whether any drifted precondition's text contains want.
func driftMentions(drifted []execute.PreconditionResult, want string) bool {
	for _, pc := range drifted {
		if strings.Contains(pc.Observed, want) || strings.Contains(pc.Description, want) || strings.Contains(pc.Expect, want) {
			return true
		}
	}
	return false
}

// renderDrift renders the refusal the way an escalation would, for a failure message.
func renderDrift(drifted []execute.PreconditionResult) string {
	parts := make([]string, 0, len(drifted))
	for _, pc := range drifted {
		parts = append(parts, fmt.Sprintf("%s: %s", pc.Kind, pc.Observed))
	}
	return strings.Join(parts, "; ")
}
