//go:build e2e

// Package e2e holds MaKlaude's end-to-end test: it drives the real read-only
// pipeline against a live kind cluster seeded with failure scenarios and proves
// three things at once —
//
//	(a) the expected findings are detected (crashloop -> critical, pending -> warning),
//	(b) an escalation is produced for them, and
//	(c) ZERO writes reached the cluster.
//
// The zero-writes proof (c) is layered, belt-and-suspenders:
//
//   - RBAC / runtime: the scan runs as MaKlaude's least-privilege, read-only
//     ServiceAccount (deploy/rbac), so the API server itself would reject a write.
//   - State invariance: the seeded objects' resourceVersion, generation, and
//     managedFields are captured before and after the scan and asserted unchanged
//     — a write would have bumped at least one of them.
//   - Active refusal: a deliberate write ATTEMPTED through the same guarded
//     transport every production client uses is asserted to fail with
//     kube.ErrWriteForbidden, proving the in-process guard is live on real
//     kubeconfig clients (the T9 builds on this).
//   - Audit log (optional): when MAKLAUDE_E2E_AUDIT_LOG points at the apiserver
//     audit log, the test asserts no mutating verb was ever attributed to
//     MaKlaude's user — the strongest external corroboration.
//
// The test is gated behind the `e2e` build tag so it never runs in the unit
// suite; the CI `e2e` job (and `task e2e`) sets the tag and the environment.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/scan"
)

const (
	e2eNamespace   = "maklaude-e2e"
	crashloopPod   = "crashloop"
	pendingPod     = "pending"
	clusterName    = "maklaude-e2e"
	collectTimeout = 60 * time.Second
)

// env reads a required environment variable or fails the test with a clear
// message about how the CI job is expected to set it.
func env(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s must be set for the e2e test (the CI e2e job / `task e2e` sets it)", key)
	}
	return v
}

// objectState captures the fields a write would necessarily change. Comparing it
// before and after the scan turns "did anything mutate?" into a precise equality
// check.
type objectState struct {
	resourceVersion string
	generation      int64
	managedFields   int
}

func podState(p *corev1.Pod) objectState {
	return objectState{
		resourceVersion: p.ResourceVersion,
		generation:      p.Generation,
		managedFields:   len(p.ManagedFields),
	}
}

// buildRegistry constructs a one-cluster registry from the SA kubeconfig the CI
// job minted. The kubeconfig file must exist (the registry validates this).
func buildRegistry(t *testing.T) *cluster.Registry {
	t.Helper()
	kubeconfig := env(t, "MAKLAUDE_E2E_KUBECONFIG")
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{
			{Name: clusterName, Kubeconfig: kubeconfig, Context: contextName},
		},
	})
	if err != nil {
		t.Fatalf("building registry from SA kubeconfig: %v", err)
	}
	return reg
}

// readPod fetches one seeded pod through a read-only client (the same client the
// pipeline uses), failing the test on error.
func readPod(t *testing.T, c *kube.Client, name string) *corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	pods, err := c.ListPods(ctx, e2eNamespace)
	if err != nil {
		t.Fatalf("listing pods in %s: %v", e2eNamespace, err)
	}
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	t.Fatalf("seeded pod %s/%s not found (was the seed applied and ready?)", e2eNamespace, name)
	return nil
}

func handle(t *testing.T, reg *cluster.Registry) *cluster.Handle {
	t.Helper()
	h, ok := reg.Get(clusterName)
	if !ok {
		t.Fatalf("cluster %q missing from registry", clusterName)
	}
	return h
}

// TestE2E_ReadOnlyScan is the whole end-to-end assertion: detect, escalate, and
// prove zero writes against a live kind cluster.
func TestE2E_ReadOnlyScan(t *testing.T) {
	reg := buildRegistry(t)
	h := handle(t, reg)

	// A read-only client used both to capture object state and to confirm the
	// cluster really is reachable as the SA.
	roClient, err := kube.NewClient(h)
	if err != nil {
		t.Fatalf("building read-only client: %v", err)
	}
	if _, err := roClient.CheckReachable(context.Background()); err != nil {
		t.Fatalf("cluster not reachable as MaKlaude SA: %v", err)
	}

	// --- Capture pre-scan state of the seeded objects. ---
	before := map[string]objectState{
		crashloopPod: podState(readPod(t, roClient, crashloopPod)),
		pendingPod:   podState(readPod(t, roClient, pendingPod)),
	}

	// --- Run the real pipeline once, in-memory escalation (no external writes). ---
	// Explicitly do NOT set MAKLAUDE_GITHUB_*, so the escalator uses the safe
	// in-memory sink: escalation is produced and counted, but nothing is written
	// to GitHub or the cluster.
	report, err := scan.NewPipeline().Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("pipeline run: %v", err)
	}
	if report.Live {
		t.Fatalf("expected dry-run escalation (live=false); GitHub env must be unset for e2e")
	}
	logReport(t, report)

	if len(report.Clusters) != 1 {
		t.Fatalf("expected 1 cluster report, got %d", len(report.Clusters))
	}
	cr := report.Clusters[0]
	if cr.Error != "" {
		t.Fatalf("cluster scan error: %s", cr.Error)
	}
	if !cr.Reachable {
		t.Fatalf("cluster reported unreachable during scan")
	}

	// --- (a) Expected findings detected. ---
	assertFinding(t, cr.Findings, "critical", "pod.crashloop", "pod/"+e2eNamespace+"/"+crashloopPod)
	assertFinding(t, cr.Findings, "warning", "pod.pending", "pod/"+e2eNamespace+"/"+pendingPod)

	// --- (b) An escalation was produced. ---
	if cr.Escalation.Opened < 2 {
		t.Errorf("expected at least 2 issues opened (crashloop + pending), got %d", cr.Escalation.Opened)
	}
	if report.Totals.Opened < 2 {
		t.Errorf("expected report totals to count >= 2 opened, got %d", report.Totals.Opened)
	}

	// --- (c) ZERO writes: object state is unchanged. ---
	after := map[string]objectState{
		crashloopPod: podState(readPod(t, roClient, crashloopPod)),
		pendingPod:   podState(readPod(t, roClient, pendingPod)),
	}
	for name, b := range before {
		a := after[name]
		if a.resourceVersion != b.resourceVersion {
			t.Errorf("ZERO-WRITES VIOLATION: pod %q resourceVersion changed %s -> %s during scan",
				name, b.resourceVersion, a.resourceVersion)
		}
		if a.generation != b.generation {
			t.Errorf("ZERO-WRITES VIOLATION: pod %q generation changed %d -> %d during scan",
				name, b.generation, a.generation)
		}
		if a.managedFields != b.managedFields {
			t.Errorf("ZERO-WRITES VIOLATION: pod %q managedFields count changed %d -> %d during scan",
				name, b.managedFields, a.managedFields)
		}
	}

	// --- (c) ZERO writes: an attempted write is actively refused at the wire. ---
	assertWriteRefused(t, h)

	// --- (c) ZERO writes: audit-log corroboration (optional). ---
	assertNoMutatingAudit(t)
}

// assertWriteRefused builds a write-capable clientset over the SAME guarded
// transport every production client uses and proves a mutating call is refused
// before it reaches the API server. This is the in-process structural proof that
// the read-only guard is active on real-kubeconfig clients.
func assertWriteRefused(t *testing.T, h *cluster.Handle) {
	t.Helper()
	cs, err := kube.WriteProbeClientForHandle(h)
	if err != nil {
		t.Fatalf("building write-probe client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	// Attempt to delete a seeded pod. The guard must reject the DELETE before any
	// request reaches the network, so this fails with ErrWriteForbidden — and the
	// pod is therefore never touched.
	err = cs.CoreV1().Pods(e2eNamespace).Delete(ctx, pendingPod, metav1.DeleteOptions{})
	if err == nil {
		t.Fatal("ZERO-WRITES VIOLATION: a DELETE through the guarded client succeeded; the read-only guard is not active")
	}
	if !isWriteForbidden(err) {
		t.Fatalf("expected the DELETE to be refused by the read-only guard (kube.ErrWriteForbidden), got: %v", err)
	}
	t.Logf("write attempt correctly refused by the read-only transport guard: %v", err)
}

// isWriteForbidden reports whether err (possibly wrapped by client-go) ultimately
// stems from the read-only transport guard. client-go obscures the underlying
// transport error type, so we match on the sentinel's stable message as a
// fallback after the errors.Is check.
func isWriteForbidden(err error) bool {
	if err == nil {
		return false
	}
	// errors.Is across client-go's wrapping does not always reach our sentinel
	// (client-go reconstructs errors), so also match the stable message text.
	return strings.Contains(err.Error(), kube.ErrWriteForbidden.Error()) ||
		strings.Contains(err.Error(), "mutating request blocked")
}

// assertNoMutatingAudit scans the apiserver audit log (when provided) for any
// mutating verb attributed to MaKlaude's ServiceAccount. Finding one fails the
// build. When MAKLAUDE_E2E_AUDIT_LOG is unset the check is skipped (the
// resourceVersion/managedFields invariance and the refused write already prove
// the guarantee; the audit log is corroboration).
func assertNoMutatingAudit(t *testing.T) {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("MAKLAUDE_E2E_AUDIT_LOG"))
	if path == "" {
		t.Log("MAKLAUDE_E2E_AUDIT_LOG unset; skipping audit-log corroboration (state-invariance + refused-write proofs still hold)")
		return
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is supplied by the CI harness, not user input.
	if err != nil {
		// The audit log is corroboration, not the primary proof: the apiserver
		// writes it as root, so a CI runner may be unable to read it. Treat an
		// unreadable log like an unset one — warn and skip, rather than failing
		// the build, since the state-invariance and refused-write proofs hold.
		t.Logf("audit log %q unreadable (%v); skipping audit-log corroboration (state-invariance + refused-write proofs still hold)", path, err)
		return
	}

	// MaKlaude authenticates as this ServiceAccount username.
	const saUser = "system:serviceaccount:maklaude:maklaude"
	mutating := map[string]bool{
		"create": true, "update": true, "patch": true,
		"delete": true, "deletecollection": true,
	}

	var violations []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, saUser) {
			continue
		}
		var ev struct {
			Verb string `json:"verb"`
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			ObjectRef struct {
				Resource  string `json:"resource"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"objectRef"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // not a JSON audit event line
		}
		if ev.User.Username != saUser {
			continue
		}
		if mutating[strings.ToLower(ev.Verb)] {
			violations = append(violations, ev.Verb+" "+ev.ObjectRef.Resource+"/"+ev.ObjectRef.Namespace+"/"+ev.ObjectRef.Name)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("ZERO-WRITES VIOLATION (audit log): MaKlaude SA issued mutating verb(s): %s", strings.Join(violations, "; "))
	}
	t.Logf("audit log clean: no mutating verb attributed to %s", saUser)
}

// assertFinding fails the test unless findings contains an entry with the given
// severity, identity rule, and object string.
func assertFinding(t *testing.T, findings []scan.FindingReport, severity, rule, object string) {
	t.Helper()
	for _, f := range findings {
		if f.Severity != severity || f.Object != object {
			continue
		}
		parts := strings.Split(f.Identity, "|")
		if len(parts) == 3 && parts[1] == rule {
			return
		}
	}
	t.Errorf("expected a %s finding (rule %q) for %q; got %d findings: %+v",
		severity, rule, object, len(findings), findings)
}

// logReport prints the full report JSON to the test log for CI debuggability.
func logReport(t *testing.T, r *scan.Report) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Logf("(could not marshal report: %v)", err)
		return
	}
	t.Logf("scan report:\n%s", string(b))
}
