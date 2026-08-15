//go:build e2e

// End-to-end proof of the chaos experiment lifecycle against a real cluster with a
// real Chaos Mesh installation: create → observe → terminate, plus the sweep that
// collects what a dead process would have left behind.
//
// # What this job proves that the unit tests cannot
//
// internal/chaos's unit tests drive the real transport against a stub API server, so
// they prove exactly what MaKlaude SENDS. They cannot prove that what it sends is a
// thing Chaos Mesh accepts. That gap is the failure mode the closed one-kind action
// catalog was built to avoid and is invisible from MaKlaude's side: a CR whose spec
// stanza is wrong for its kind is accepted by the API server, ignored by the
// controller, and reported by MaKlaude as injected — so a run measures behaviour under
// a fault that never happened. Only a real CRD and a real admission webhook close it.
//
// # What is asserted hard, and what is corroboration
//
// Hard, because they are claims about MaKlaude and about Chaos Mesh's contract with it:
// the dry-run preview is admitted by the real webhook and creates nothing; the real
// create is admitted; a replay collides instead of injecting a second copy; teardown
// succeeds and is idempotent; and the reaper removes MaKlaude's own leftover object
// while leaving an operator's alone.
//
// Corroboration, warned-and-skipped rather than failed: the pod-level EFFECT of the
// fault. MaKlaude's contract ends at the CR — Chaos Mesh's controller does the
// breaking, with its own privileges — and the fault's precise observable signature is
// Chaos Mesh's implementation detail across versions. Measuring MaKlaude's behaviour
// under a landed fault is T5/T6's job (issues #194, #195) with T8 (#197) as its gate;
// making that observation load-bearing here would buy nothing and would make this job
// fail on an upstream detail. The rule is the one this repo already applies to the
// apiserver audit log: optional corroboration degrades gracefully when the primary
// proof already holds.
package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

const (
	// chaosClusterName is what this cluster is registered as. It also travels in the
	// eligibility acknowledgement and onto every object MaKlaude creates, so a
	// mismatch anywhere shows up as a refusal rather than as a wrong write.
	chaosClusterName = "maklaude-e2e-chaos"

	// chaosObjectNamespace is where experiment OBJECTS live: MaKlaude's own chaos
	// namespace, the only one its Role covers. Not where any fault lands.
	chaosObjectNamespace = "maklaude-chaos"

	// chaosTargetNamespace holds the workload the experiments actually break. See
	// manifests/chaos-target.yaml for why it is separate from maklaude-e2e.
	chaosTargetNamespace = "maklaude-chaos-target"

	// foreignExperiment is the operator's own experiment, seeded by the CI job. The
	// reaper must leave it alone. See manifests/chaos-foreign.yaml.
	foreignExperiment = "operator-own-experiment"
)

// chaosTargetSelector matches the victim deployment's pods and nothing else.
var chaosTargetSelector = map[string]string{"scenario": "chaos-target"}

// requireChaosCluster skips unless the chaos job set up a Chaos Mesh installation.
//
// The skip is what lets this file live in the same package as the rest of the e2e
// suite. `task e2e` and the main `e2e on kind` job run every test in this package
// against a cluster with no Chaos Mesh, and a test that hard-failed there would report
// a missing dependency as a broken feature.
func requireChaosCluster(t *testing.T) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv("MAKLAUDE_E2E_CHAOS_KUBECONFIG"))
	if kubeconfig == "" {
		t.Skip("MAKLAUDE_E2E_CHAOS_KUBECONFIG unset; this test needs the chaos identity and a Chaos Mesh installation (the `chaos on kind` CI job, or `task chaos:install` locally)")
	}
	return kubeconfig
}

// buildChaosInjector constructs an injector for this cluster in the requested mode.
//
// The registry is built with a chaos eligibility marker naming this cluster, which is
// the only way a [cluster.ChaosTarget] comes into existence — so this function is also
// a live check that the eligibility path works against a real kubeconfig, not just in
// unit tests.
func buildChaosInjector(t *testing.T, mode kube.ExecuteMode) *chaos.Injector {
	t.Helper()
	kubeconfig := requireChaosCluster(t)
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")

	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{
			Name:       chaosClusterName,
			Kubeconfig: kubeconfig,
			Context:    contextName,
			Chaos: &cluster.ChaosEligibility{
				Cluster:         chaosClusterName,
				Acknowledgement: cluster.ChaosAcknowledgementFor(chaosClusterName),
			},
		}},
	})
	if err != nil {
		t.Fatalf("building a chaos-eligible registry: %v", err)
	}

	target, err := reg.ChaosTarget(chaosClusterName)
	if err != nil {
		t.Fatalf("minting a chaos target for %s: %v", chaosClusterName, err)
	}

	injector, err := chaos.NewInjector(target, mode)
	if err != nil {
		t.Fatalf("building injector in mode %v: %v", mode, err)
	}
	return injector
}

// podFailureExperiment is a bounded fault on the victim deployment: the action whose
// effect persists, and therefore the one whose mandatory server-side duration is the
// mechanism that ends it if MaKlaude dies.
func podFailureExperiment(duration time.Duration) chaos.Experiment {
	return chaos.Experiment{
		Action:    chaos.ActionPodFailure,
		Namespace: chaosObjectNamespace,
		Mode:      chaos.ModeAll,
		Selector: chaos.Selector{
			Namespaces:     []string{chaosTargetNamespace},
			LabelSelectors: chaosTargetSelector,
		},
		Duration: duration,
	}
}

// TestE2E_ChaosLifecycle drives create → observe → terminate against a real Chaos Mesh.
func TestE2E_ChaosLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Two minutes: long enough that the fault is unambiguously live for the whole
	// observe-and-terminate sequence, short enough that a test abandoning it leaves a
	// cluster that heals itself well inside the job's own lifetime. That second property
	// is the milestone's guarantee, applied to this test as much as to production.
	experiment := podFailureExperiment(2 * time.Minute)
	if experiment.SelfLimit() != chaos.SelfLimitServerDuration {
		t.Fatalf("this test needs a fault that persists, got %s", experiment.SelfLimit())
	}

	// --- The dry-run preview, against the real admission webhook. ---
	//
	// This is the assertion the stub cannot make. The object is validated by the actual
	// PodChaos CRD schema and by Chaos Mesh's own webhook, and then discarded. It is
	// what turns "MaKlaude composes a plausible-looking CR" into "Chaos Mesh accepts
	// this exact CR", with no fault injected to prove it.
	preview, err := buildChaosInjector(t, kube.ExecuteDryRun).Inject(ctx, experiment)
	if err != nil {
		t.Fatalf("the real CRD and webhook rejected MaKlaude's PodChaos: %v", err)
	}
	if !preview.DryRun || preview.UID != "" {
		t.Fatalf("a preview must create nothing: %+v", preview)
	}
	if preview.Name != experiment.ObjectName() {
		t.Errorf("preview named %q, want the derived name %q", preview.Name, experiment.ObjectName())
	}
	t.Logf("dry-run preview admitted: %s %s/%s under scope %s",
		preview.Kind, preview.Namespace, preview.Name, preview.Scope)

	injector := buildChaosInjector(t, kube.ExecuteEnabled)

	// --- Create. ---
	injected, err := injector.Inject(ctx, experiment)
	if err != nil {
		t.Fatalf("injecting: %v", err)
	}
	if injected.DryRun || injected.UID == "" {
		t.Fatalf("a real injection must yield a UID: %+v", injected)
	}
	t.Logf("injected %s %s/%s uid=%s duration=%s",
		injected.Kind, injected.Namespace, injected.Name, injected.UID, experiment.Duration)

	// Teardown runs even if an assertion below fails, so a red test does not leave a
	// fault behind. This is belt-and-braces rather than the guarantee: the fault expires
	// on its own within the duration above, and the object is collected by the reaper —
	// which is the whole point, because a cleanup that only runs on the happy path is
	// exactly what a SIGKILL skips.
	t.Cleanup(func() {
		if _, err := injector.Remove(context.Background(), *injected); err != nil {
			t.Logf("cleanup teardown of %s: %v (the fault still expires on its own)", injected.Name, err)
		}
	})

	// --- Observe, through a verb MaKlaude actually has. ---
	//
	// The chaos identity can `get` its own experiments and nothing else — no pods, no
	// deployments — so the observation available to MaKlaude itself is the
	// create-shaped precondition: replaying the identical experiment must now collide.
	// That is a real read of the real object through the real API server, and it is also
	// the property that makes a retry safe, so asserting it here covers both.
	replay, err := injector.Inject(ctx, experiment)
	if !errors.Is(err, chaos.ErrExperimentExists) {
		t.Fatalf("a replay must collide with the live experiment, got (%+v, %v)", replay, err)
	}
	t.Logf("replay correctly refused: %v", err)

	// Corroboration: the fault visibly landed. Warned rather than failed — see the file
	// header for why MaKlaude's contract ends at the CR.
	observeChaosEffect(t, "pod-failure made the victim pods unready")

	// --- Terminate. ---
	removal, err := injector.Remove(ctx, *injected)
	if err != nil {
		t.Fatalf("tearing down %s: %v", injected.Name, err)
	}
	if removal.AlreadyAbsent {
		t.Errorf("the experiment was gone before teardown, so this proved nothing: %+v", removal)
	}
	t.Logf("torn down under scope %s", removal.Scope)

	// Teardown is idempotent, and says so rather than smoothing it over: "torn down" and
	// "was never there" are different facts about a cluster, and a reaper racing an
	// explicit teardown produces the second one routinely.
	again, err := injector.Remove(ctx, *injected)
	if err != nil {
		t.Fatalf("a second teardown must succeed: %v", err)
	}
	if !again.AlreadyAbsent {
		t.Errorf("the second teardown must report AlreadyAbsent, got %+v", again)
	}

	// And with the object gone, the same experiment can be injected again — which is the
	// other half of the derived name being a precondition rather than a lock.
	reinjected, err := injector.Inject(ctx, experiment)
	if err != nil {
		t.Fatalf("re-injecting after teardown must succeed: %v", err)
	}
	if _, err := injector.Remove(ctx, *reinjected); err != nil {
		t.Fatalf("tearing down the re-injected experiment: %v", err)
	}
}

// TestE2E_ChaosReaperSweepsOnlyItsOwn proves the teardown mechanism that runs when no
// process is left to run a teardown, against a real cluster holding two experiments:
// MaKlaude's and the operator's.
//
// The clock is pushed forward rather than the test waiting out a real grace period. The
// grace exists to guarantee no live fault is reachable by a sweep, and its floor is
// derived from [chaos.MaxDuration]; waiting fifteen real minutes in CI would assert the
// same comparison at fifteen minutes' cost. What is NOT faked is anything about the
// cluster: both objects are really there, the sweep really lists them, and the delete
// really goes through the API server with a UID precondition.
func TestE2E_ChaosReaperSweepsOnlyItsOwn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	injector := buildChaosInjector(t, kube.ExecuteEnabled)

	// A one-shot fault: its effect is over as soon as it happens, so what remains is
	// purely the residue a reaper exists to collect — the same thing a SIGKILLed run
	// leaves behind, with none of the ambiguity of a fault that might still be running.
	experiment := chaos.Experiment{
		Action:    chaos.ActionPodKill,
		Namespace: chaosObjectNamespace,
		Mode:      chaos.ModeOne,
		Selector: chaos.Selector{
			Namespaces:     []string{chaosTargetNamespace},
			LabelSelectors: chaosTargetSelector,
		},
	}
	if experiment.SelfLimit() != chaos.SelfLimitInstant {
		t.Fatalf("this test needs a one-shot fault, got %s", experiment.SelfLimit())
	}

	injected, err := injector.Inject(ctx, experiment)
	if err != nil {
		t.Fatalf("injecting the orphan-to-be: %v", err)
	}
	t.Logf("left behind %s %s/%s uid=%s", injected.Kind, injected.Namespace, injected.Name, injected.UID)
	t.Cleanup(func() {
		// If the sweep below fails to remove it, this does — an e2e that leaks a chaos
		// object into a torn-down cluster is harmless, but one that leaks the habit is not.
		_, _ = injector.Remove(context.Background(), *injected)
	})

	// The future clock stands in for the passage of time between a dead run and the next
	// scheduled one.
	future := func() time.Time { return time.Now().Add(chaos.DefaultOrphanGrace + time.Hour) }
	reaper, err := chaos.NewReaper(injector, chaos.DefaultOrphanGrace, future)
	if err != nil {
		t.Fatalf("building reaper: %v", err)
	}

	sweep, err := reaper.Reap(ctx, chaosObjectNamespace)
	if err != nil {
		t.Fatalf("sweeping: %v", err)
	}
	t.Logf("sweep scanned %d, reaped %d, skipped %d", sweep.Scanned, len(sweep.Reaped), len(sweep.Skipped))

	var reapedMine bool
	for _, r := range sweep.Reaped {
		if r.Name == injected.Name {
			reapedMine = true
		}
		if r.Name == foreignExperiment {
			t.Fatalf("THE SWEEP DELETED THE OPERATOR'S OWN EXPERIMENT (%q); this is worse than the leak it prevents", r.Name)
		}
	}
	if !reapedMine {
		t.Errorf("the sweep did not remove MaKlaude's own orphan %q: %+v", injected.Name, sweep)
	}

	// The operator's experiment must have been SEEN and left alone. "Not deleted" alone
	// would also be satisfied by a sweep that never listed it, which would mean the
	// ownership test was never exercised.
	var skippedForeign *string
	for i := range sweep.Skipped {
		if sweep.Skipped[i].Name == foreignExperiment {
			skippedForeign = &sweep.Skipped[i].Reason
		}
	}
	if skippedForeign == nil {
		t.Fatalf("the operator's experiment %q was never enumerated, so the ownership test was not exercised (did the CI job apply manifests/chaos-foreign.yaml?): %+v",
			foreignExperiment, sweep)
	}
	t.Logf("left the operator's experiment alone: %s", *skippedForeign)

	// A second sweep proves it is still on the cluster rather than merely un-deleted by
	// the first: an object that has been removed cannot be listed again.
	second, err := reaper.Reap(ctx, chaosObjectNamespace)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	stillThere := false
	for _, s := range second.Skipped {
		if s.Name == foreignExperiment {
			stillThere = true
		}
	}
	if !stillThere {
		t.Errorf("the operator's experiment is no longer on the cluster after a sweep: %+v", second)
	}
	if len(second.Reaped) != 0 {
		t.Errorf("the second sweep should have nothing of MaKlaude's left to remove, got %+v", second.Reaped)
	}
}

// observeChaosEffect waits for the victim deployment to stop being fully available, and
// WARNS rather than fails if it does not.
//
// It reads through the ordinary read-only identity, which is the faithful arrangement:
// MaKlaude injects with one ServiceAccount and observes with another, and the chaos
// identity deliberately cannot read a pod at all. If the read-only kubeconfig is not
// present in this job the observation is skipped entirely — it is corroboration, and
// the primary proofs in the caller do not depend on it.
func observeChaosEffect(t *testing.T, what string) {
	t.Helper()

	kubeconfig := strings.TrimSpace(os.Getenv("MAKLAUDE_E2E_KUBECONFIG"))
	if kubeconfig == "" {
		t.Logf("MAKLAUDE_E2E_KUBECONFIG unset; skipping fault-effect corroboration (%s)", what)
		return
	}
	contextName := strings.TrimSpace(os.Getenv("MAKLAUDE_E2E_CONTEXT"))
	if contextName == "" {
		t.Logf("MAKLAUDE_E2E_CONTEXT unset; skipping fault-effect corroboration (%s)", what)
		return
	}

	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{Name: "chaos-observer", Kubeconfig: kubeconfig, Context: contextName}},
	})
	if err != nil {
		t.Logf("could not build an observation registry, skipping corroboration (%s): %v", what, err)
		return
	}
	handle, ok := reg.Get("chaos-observer")
	if !ok {
		t.Logf("observation cluster missing, skipping corroboration (%s)", what)
		return
	}
	client, err := kube.NewClient(handle)
	if err != nil {
		t.Logf("could not build a read-only client, skipping corroboration (%s): %v", what, err)
		return
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		deploys, err := client.ListDeployments(ctx, chaosTargetNamespace)
		cancel()
		if err != nil {
			t.Logf("listing deployments in %s: %v", chaosTargetNamespace, err)
			time.Sleep(3 * time.Second)
			continue
		}
		for i := range deploys {
			d := &deploys[i]
			if d.Name != "victim" {
				continue
			}
			desired := int32(1)
			if d.Spec.Replicas != nil {
				desired = *d.Spec.Replicas
			}
			if d.Status.AvailableReplicas < desired {
				t.Logf("corroborated (%s): victim has %d/%d available replicas",
					what, d.Status.AvailableReplicas, desired)
				return
			}
		}
		time.Sleep(3 * time.Second)
	}

	// Deliberately not a failure. See the file header: the CR is MaKlaude's contract,
	// and every proof that MaKlaude held it is asserted hard in the caller.
	t.Logf("WARNING: could not corroborate the fault's pod-level effect within 90s (%s). "+
		"MaKlaude's own guarantees are asserted separately and still hold; measuring behaviour "+
		"under a landed fault is T5/T6's job (issues #194, #195).", what)
}
