package execute

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// TestRunner_AgainstTheRealWritePathAndCollector wires the production types
// together — the scoped [kube.Executor], the real [health.Collector], and the real
// [approve.Gatekeeper] — and drives one approved cordon through them.
//
// The fakes elsewhere in this package prove the runner's LOGIC. This proves the
// seams: that the three interfaces in deps.go describe types that actually exist,
// that a proposal's target survives being turned into a scoped request path, and
// that the executed label reaches a real artifact. Those are the failures that a
// hand-written fake reproduces perfectly and a real call does not.
//
// The API server is an httptest stub rather than a fake clientset because the write
// path builds its own client-go clientset per action from a rest.Config; there is no
// seam to inject a fake into, which is itself the property [kube.Executor] is built
// around. Reads go through a fake clientset, and the stub applies the write to it —
// so the observation window sees the effect of the request that was actually put on
// the wire.
func TestRunner_AgainstTheRealWritePathAndCollector(t *testing.T) {
	const nodeName = "node-a"

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, ResourceVersion: "1001"},
		Spec:       corev1.NodeSpec{Unschedulable: false},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
		},
	})

	var patches []map[string]any
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/nodes/"+nodeName {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			patches = append(patches, body)

			// The write landed, so the world the collector reads must reflect it.
			node, err := clientset.CoreV1().Nodes().Get(r.Context(), nodeName, metav1.GetOptions{})
			if err == nil {
				node.Spec.Unschedulable = true
				node.ResourceVersion = "1002"
				_, _ = clientset.CoreV1().Nodes().Update(r.Context(), node, metav1.UpdateOptions{})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Node","metadata":{"name":"` + nodeName + `","resourceVersion":"1002"}}`))
	}))
	t.Cleanup(stub.Close)

	handle := testHandle(t, testCluster, writeTestKubeconfig(t, stub.URL), "maklaude")
	executor, err := kube.NewExecutor(handle, kube.ExecuteEnabled)
	if err != nil {
		t.Fatalf("building the scoped write client: %v", err)
	}
	collector := health.NewCollector(kube.NewClientWithInterface(testCluster, clientset))

	p := cordonProposal()
	g := newGate(t, p)
	auth := g.authorize()

	runner, err := New(executor, collector, g.gk, fastPolicy())
	if err != nil {
		t.Fatalf("building runner: %v", err)
	}

	rep, err := runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing through the real write path: %v", err)
	}

	if len(patches) != 1 {
		t.Fatalf("the API server received %d patches, want exactly 1: %+v", len(patches), patches)
	}
	spec, ok := patches[0]["spec"].(map[string]any)
	if !ok || spec["unschedulable"] != true {
		t.Fatalf("the request did not cordon the node: %+v", patches[0])
	}
	meta, ok := patches[0]["metadata"].(map[string]any)
	if !ok || meta["resourceVersion"] != "1001" {
		t.Fatalf("the request was not conditioned on the approved resourceVersion: %+v", patches[0])
	}

	if !rep.Executed || rep.Convergence != ConvergenceConverged {
		t.Fatalf("report says executed=%t convergence=%s (%s), want an executed, converged action",
			rep.Executed, rep.Convergence, rep.ConvergenceDetail)
	}
	if rep.Outcome == nil || !strings.Contains(rep.Outcome.Scope, "/api/v1/nodes/"+nodeName) {
		t.Fatalf("the outcome does not name the scoped request that was admitted: %+v", rep.Outcome)
	}

	artifact := g.artifact()
	if !artifact.HasLabel(approve.ExecutedLabel) {
		t.Fatalf("the real approval artifact is missing %q: %v", approve.ExecutedLabel, artifact.Labels)
	}
}

// TestRunner_DryRunAgainstTheRealWritePath proves a preview travels all the way to
// the API server carrying dryRun=All — the property the scoped transport refuses a
// mutating request without — and that the runner still declines to record it as an
// execution.
func TestRunner_DryRunAgainstTheRealWritePath(t *testing.T) {
	const nodeName = "node-a"

	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName, ResourceVersion: "1001"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
		},
	})

	var queries []string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			queries = append(queries, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Node","metadata":{"name":"` + nodeName + `","resourceVersion":"1001"}}`))
	}))
	t.Cleanup(stub.Close)

	handle := testHandle(t, testCluster, writeTestKubeconfig(t, stub.URL), "maklaude")
	executor, err := kube.NewExecutor(handle, kube.ExecuteDryRun)
	if err != nil {
		t.Fatalf("building the preview-only write client: %v", err)
	}

	p := cordonProposal()
	sink := approve.NewMemorySink()
	recorder := approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy())
	runner, err := New(executor, health.NewCollector(kube.NewClientWithInterface(testCluster, clientset)), recorder, fastPolicy())
	if err != nil {
		t.Fatalf("building runner: %v", err)
	}

	rep, err := runner.Execute(context.Background(), authorizationFor(t, p), p)
	if err != nil {
		t.Fatalf("previewing through the real write path: %v", err)
	}

	if len(queries) != 1 || queries[0] != "dryRun=All" {
		t.Fatalf("the preview sent queries %v, want exactly one dryRun=All", queries)
	}
	if rep.Executed || rep.Recorded {
		t.Fatalf("a preview reported executed=%t recorded=%t", rep.Executed, rep.Recorded)
	}
	node, err := clientset.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the node: %v", err)
	}
	if node.Spec.Unschedulable {
		t.Fatal("a preview changed the cluster")
	}
}

// writeTestKubeconfig writes a minimal kubeconfig pointing at serverURL and returns
// its path. It mirrors the kube package's own test setup; the handle must carry an
// explicit path and context because the write path deliberately never falls back to
// $KUBECONFIG or in-cluster config.
func writeTestKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: maklaude
clusters:
  - name: maklaude-test
    cluster:
      server: %s
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
`, serverURL)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// testHandle builds a real cluster.Handle through the registry.
func testHandle(t *testing.T, name, kubeconfig, kctx string) *cluster.Handle {
	t.Helper()
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{Name: name, Kubeconfig: kubeconfig, Context: kctx}},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	h, ok := reg.Get(name)
	if !ok {
		t.Fatalf("handle %q not found in registry", name)
	}
	return h
}
