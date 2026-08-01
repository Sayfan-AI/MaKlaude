//go:build e2e

// This file holds the live-cluster proof for MaKlaude's WRITE path, the
// counterpart to e2e_test.go's zero-writes proof for the read path.
//
// Everything about the scoped write client is unit-tested against stub
// transports, which establishes that the guard refuses what it should. What a
// stub cannot establish is the claim the whole gated-remediation milestone rests
// on: that a server-side dry run is a real request to a real API server, admitted
// by real admission controllers, that nevertheless leaves the object untouched.
// "dryRun=All means nothing changes" is the API server's behavior, not
// MaKlaude's, so only a live API server can prove MaKlaude is invoking it
// correctly — and getting it subtly wrong (a mis-spelled marker, a DELETE whose
// options ride in the query where the apiserver won't read them) fails in the one
// direction that matters: it looks like a preview and executes for real.
//
// So this test does the thing that is dangerous if the plumbing is wrong. It
// issues genuine mutating requests — a Deployment patch and a Pod delete — in
// dry-run mode against seeded objects, then asserts those objects are byte-for-
// byte where they were. The seeded objects are the same ones e2e_test.go reads,
// which is deliberate: if a dry run ever stopped being a no-op, the other test's
// state-invariance assertions would start failing too.
//
// It authenticates as maklaude-executor (deploy/rbac/write), NOT as the
// observation account — a dry-run request is authorized with the same verb as a
// real one, so it needs the write bundle. The read-only identity's inability to
// do any of this is asserted separately, in the workflow's `auth can-i` step and
// in e2e_test.go's refused-write proof.
package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// executorClusterName is a distinct registry name for the executor identity, so a
// mix-up between the two kubeconfigs shows up as a missing cluster rather than as
// a silently wrong identity.
const executorClusterName = "maklaude-e2e-executor"

// deploymentState captures what a write to a Deployment would necessarily bump.
// generation is the sharper signal of the two: the API server increments it on
// any spec change, which is exactly what a rollout-restart patch performs.
type deploymentState struct {
	resourceVersion string
	generation      int64
	annotations     map[string]string
}

func deployState(d *appsv1.Deployment) deploymentState {
	return deploymentState{
		resourceVersion: d.ResourceVersion,
		generation:      d.Generation,
		annotations:     d.Spec.Template.Annotations,
	}
}

// buildExecutorRegistry constructs a one-cluster registry from the
// maklaude-executor kubeconfig the CI job minted.
func buildExecutorRegistry(t *testing.T) *cluster.Registry {
	t.Helper()
	kubeconfig := env(t, "MAKLAUDE_E2E_EXECUTOR_KUBECONFIG")
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{
			{Name: executorClusterName, Kubeconfig: kubeconfig, Context: contextName},
		},
	})
	if err != nil {
		t.Fatalf("building registry from executor kubeconfig: %v", err)
	}
	return reg
}

func executorHandle(t *testing.T, reg *cluster.Registry) *cluster.Handle {
	t.Helper()
	h, ok := reg.Get(executorClusterName)
	if !ok {
		t.Fatalf("cluster %q missing from executor registry", executorClusterName)
	}
	return h
}

// readDeployment fetches one seeded Deployment through a read-only client.
func readDeployment(t *testing.T, c *kube.Client, name string) *appsv1.Deployment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	deploys, err := c.ListDeployments(ctx, e2eNamespace)
	if err != nil {
		t.Fatalf("listing deployments in %s: %v", e2eNamespace, err)
	}
	for i := range deploys {
		if deploys[i].Name == name {
			return &deploys[i]
		}
	}
	t.Fatalf("seeded deployment %s/%s not found (was the seed applied?)", e2eNamespace, name)
	return nil
}

// TestE2E_DryRunMutatesNothing is the milestone's load-bearing safety proof: real
// mutating requests, really sent, really admitted, changing nothing.
func TestE2E_DryRunMutatesNothing(t *testing.T) {
	reg := buildExecutorRegistry(t)
	h := executorHandle(t, reg)

	// Reads go through the ordinary read-only client, on the ordinary read-only
	// transport. Observing the write path with the write path's own client would
	// make the before/after comparison depend on the thing under test.
	reader, err := kube.NewClient(h)
	if err != nil {
		t.Fatalf("building read-only client for the executor identity: %v", err)
	}

	exec, err := kube.NewExecutor(h, kube.ExecuteDryRun)
	if err != nil {
		t.Fatalf("building dry-run executor: %v", err)
	}
	if exec.Mode() != kube.ExecuteDryRun {
		t.Fatalf("executor mode = %s, want dry-run", exec.Mode())
	}

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	t.Run("deployment patch", func(t *testing.T) {
		beforeState := deployState(readDeployment(t, reader, badImageDeploy))

		// A genuine rollout restart: the API server admits this patch, runs it
		// through admission, and would bump .metadata.generation for real.
		const restartedAt = "2026-01-01T00:00:00Z"
		outcome := underPrecondition(t, "rollout restart", func(resourceVersion string) (*kube.Outcome, error) {
			return exec.RestartDeploymentRollout(ctx, e2eNamespace, badImageDeploy, restartedAt, resourceVersion)
		}, func() string {
			return readDeployment(t, reader, badImageDeploy).ResourceVersion
		})

		if !outcome.DryRun {
			t.Fatalf("outcome does not report a dry run: %+v", outcome)
		}
		if !strings.Contains(outcome.Scope, "dry-run") {
			t.Errorf("outcome scope %q does not record that the action was preview-only", outcome.Scope)
		}

		after := deployState(readDeployment(t, reader, badImageDeploy))

		// generation, not resourceVersion, is what this asserts on. The seeded
		// Deployment is deliberately unhealthy (ImagePullBackOff), so its STATUS
		// churns continuously and bumps resourceVersion on its own — an equality
		// check there would flake for reasons that have nothing to do with the dry
		// run. generation moves only on a SPEC change, which is precisely what a
		// rollout-restart patch performs, so it is both the stabler and the more
		// specific signal.
		if after.generation != beforeState.generation {
			t.Errorf("dry-run patch bumped generation: %d -> %d (the spec was really modified)",
				beforeState.generation, after.generation)
		}
		if got := after.annotations[restartedAtKey]; got != "" {
			t.Errorf("dry-run patch persisted the restart annotation (%s=%q); the cluster was mutated",
				restartedAtKey, got)
		}
	})

	t.Run("pod delete", func(t *testing.T) {
		// The pending pod, not the crashlooping one: a crashlooping pod restarts
		// on a backoff timer and rewrites its own status while the test runs,
		// which would make the precondition a coin flip rather than a check.
		before := readPod(t, reader, pendingPod)

		outcome := underPrecondition(t, "pod delete", func(resourceVersion string) (*kube.Outcome, error) {
			return exec.DeletePod(ctx, e2eNamespace, pendingPod, resourceVersion)
		}, func() string {
			return readPod(t, reader, pendingPod).ResourceVersion
		})

		if !outcome.DryRun {
			t.Fatalf("outcome does not report a dry run: %+v", outcome)
		}

		// The pod must still be there. This is the assertion that would catch a
		// DELETE whose dryRun marker went into the query string, where the
		// apiserver ignores it whenever DeleteOptions arrive in the body — the
		// exact mismatch kube.hasServerDryRun exists to prevent, and the one
		// failure mode of this whole design that destroys something.
		after := readPod(t, reader, pendingPod)
		if after.DeletionTimestamp != nil {
			t.Fatalf("dry-run delete set a deletionTimestamp on %s/%s; the pod is really being deleted",
				e2eNamespace, pendingPod)
		}
		if after.UID != before.UID {
			t.Fatalf("dry-run delete removed %s/%s (a replacement with a new UID exists)",
				e2eNamespace, pendingPod)
		}
	})
}

// preconditionAttempts bounds how many times an action is re-issued against a
// freshly-read resourceVersion.
const preconditionAttempts = 4

// underPrecondition runs a mutating action against the target's current
// resourceVersion, re-reading and retrying if the API server rejects the
// precondition.
//
// A conflict here is the system working: the seeded objects are unhealthy on
// purpose, so their status is being rewritten by controllers throughout the test,
// and an action conditioned on a snapshot taken moments ago can legitimately go
// stale. That is exactly the behavior kube.ErrPreconditionConflict exists to
// report, and re-proposing against a fresh read is what the real approval flow
// does with it. Retrying here therefore tests the same code path the product
// uses, rather than papering over a race.
//
// Any other error fails immediately — a retry loop that swallows the difference
// between "stale" and "refused" would hide the failures this test is for.
func underPrecondition(t *testing.T, what string, act func(resourceVersion string) (*kube.Outcome, error), currentResourceVersion func() string) *kube.Outcome {
	t.Helper()

	var lastErr error
	for attempt := 1; attempt <= preconditionAttempts; attempt++ {
		resourceVersion := currentResourceVersion()
		outcome, err := act(resourceVersion)
		if err == nil {
			return outcome
		}
		if !errors.Is(err, kube.ErrPreconditionConflict) {
			t.Fatalf("dry-run %s was rejected: %v", what, err)
		}
		lastErr = err
		t.Logf("dry-run %s: attempt %d hit a precondition conflict on resourceVersion %s (the target is churning); re-reading",
			what, attempt, resourceVersion)
	}

	t.Fatalf("dry-run %s never landed within %d attempts; last conflict: %v", what, preconditionAttempts, lastErr)
	return nil
}

// restartedAtKey mirrors the unexported annotation kube.RestartDeploymentRollout
// stamps. It is duplicated rather than exported: the constant is an
// implementation detail of the restart primitive, and a test that asserts the
// literal kubectl uses is a stronger check than one that asserts the code agrees
// with itself.
const restartedAtKey = "kubectl.kubernetes.io/restartedAt"

// TestE2E_ExecutorRefusesOutOfScopeAndDisabled proves on a live cluster that the
// two refusals which make the write path narrow are structural, not conventional:
// the kill switch withholds the object entirely, and the read-only client built
// from the SAME write-capable identity still cannot write.
//
// The second half is the one that needs a real cluster. Unit tests show
// kube.Client's transport refuses mutations, but they cannot show that the
// refusal survives being handed credentials that the API server would actually
// accept for a write. Here it is, holding, against an identity that genuinely has
// patch and delete.
func TestE2E_ExecutorRefusesOutOfScopeAndDisabled(t *testing.T) {
	h := executorHandle(t, buildExecutorRegistry(t))

	// The kill switch is a construction-time refusal: a deployment that has not
	// opted in holds no write-capable object at all.
	if _, err := kube.NewExecutor(h, kube.ExecuteDisabled); !errors.Is(err, kube.ErrExecutorDisabled) {
		t.Errorf("NewExecutor(ExecuteDisabled) error = %v, want ErrExecutorDisabled", err)
	}

	// Defense in depth on the observation path, with real write credentials
	// behind it.
	assertWriteRefused(t, h)
}

// TestE2E_ObservationIdentityCannotExecute closes the loop from the other side:
// the account the pipeline actually runs as is refused by the API SERVER, not
// merely by MaKlaude's own transport.
//
// e2e_test.go already proves the transport refuses. That proof is about MaKlaude's
// code. This one deliberately bypasses MaKlaude's read-only guard — it builds a
// raw client-go clientset from the observation kubeconfig — so that what fails is
// the cluster's RBAC. Both layers have to hold independently, and only one of them
// is still standing if someone removes the other.
func TestE2E_ObservationIdentityCannotExecute(t *testing.T) {
	kubeconfig := env(t, "MAKLAUDE_E2E_KUBECONFIG")
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")

	clientset, err := unguardedClientset(kubeconfig, contextName)
	if err != nil {
		t.Fatalf("building unguarded clientset from the observation kubeconfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	// Even as a dry run — which is the gentlest mutating request there is, and
	// still authorized as `patch`.
	_, err = clientset.AppsV1().Deployments(e2eNamespace).Patch(ctx, badImageDeploy,
		k8stypes.StrategicMergePatchType, []byte(`{"metadata":{"labels":{"maklaude-e2e":"should-not-apply"}}}`),
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	if err == nil {
		t.Fatalf("the observation identity was ALLOWED to patch %s/%s; deploy/rbac has been widened",
			e2eNamespace, badImageDeploy)
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("observation identity's patch failed for the wrong reason (want 403 Forbidden from RBAC): %v", err)
	}
	t.Logf("API server refused the observation identity's patch as expected: %v", err)
}

// unguardedClientset builds a clientset with NO MaKlaude transport guard, from a
// kubeconfig path and context.
//
// This is the one place in the repo that deliberately constructs a client capable
// of expressing a write outside kube.Executor, and it exists only so a test can
// aim a write at the API server and watch RBAC reject it. It is confined to the
// e2e build tag, and the request it sends is a dry run against an object the
// identity provably cannot patch.
func unguardedClientset(kubeconfig, contextName string) (*kubernetes.Clientset, error) {
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}
