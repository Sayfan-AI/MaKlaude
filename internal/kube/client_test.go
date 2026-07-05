package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// writeKubeconfig writes a minimal multi-context kubeconfig pointing at
// serverURL into a temp dir and returns its path. The two contexts let tests
// assert that the handle's selected context (not the file's current-context)
// is the one actually used.
func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig.yaml")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: other
clusters:
  - name: maklaude-test
    cluster:
      server: %s
      insecure-skip-tls-verify: true
  - name: elsewhere
    cluster:
      server: https://127.0.0.1:1
      insecure-skip-tls-verify: true
contexts:
  - name: maklaude
    context:
      cluster: maklaude-test
      user: tester
  - name: other
    context:
      cluster: elsewhere
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

// handleFor builds a real cluster.Handle (via the registry) for the named
// context backed by the given kubeconfig path.
func handleFor(t *testing.T, name, kubeconfig, kctx string) *cluster.Handle {
	t.Helper()
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{
			Name:       name,
			Kubeconfig: kubeconfig,
			Context:    kctx,
		}},
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

// TestNewClient_NilHandle proves a nil handle fails with a build-config error
// rather than panicking.
func TestNewClient_NilHandle(t *testing.T) {
	_, err := NewClient(nil)
	if !errors.Is(err, ErrBuildConfig) {
		t.Fatalf("expected ErrBuildConfig for nil handle, got: %v", err)
	}
}

// TestNewClient_BuildsAndSelectsContext proves a client builds from a temporary
// kubeconfig+context and that the SELECTED context (maklaude → the test server)
// is the one used, not the file's current-context ("other" → an unreachable
// address). It confirms this by issuing a read that the fake API server serves.
func TestNewClient_BuildsAndSelectsContext(t *testing.T) {
	var sawGet bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/version" {
			sawGet = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"major":"1","minor":"30","gitVersion":"v1.30.0"}`))
	}))
	defer srv.Close()

	kubeconfig := writeKubeconfig(t, srv.URL)
	h := handleFor(t, "prod", kubeconfig, "maklaude")

	client, err := NewClient(h)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	if client.Name() != "prod" {
		t.Fatalf("expected client name %q, got %q", "prod", client.Name())
	}

	info, err := client.CheckReachable(context.Background())
	if err != nil {
		t.Fatalf("CheckReachable failed: %v", err)
	}
	if info.GitVersion != "v1.30.0" {
		t.Fatalf("expected server version v1.30.0, got %q", info.GitVersion)
	}
	if !sawGet {
		t.Fatal("the selected context did not route to the test server (context selection broken)")
	}
}

// TestNewClient_UnknownContext proves selecting a context absent from the
// kubeconfig fails at build time with a wrapped ErrBuildConfig.
func TestNewClient_UnknownContext(t *testing.T) {
	kubeconfig := writeKubeconfig(t, "https://example.test")
	h := handleFor(t, "prod", kubeconfig, "does-not-exist")

	_, err := NewClient(h)
	if !errors.Is(err, ErrBuildConfig) {
		t.Fatalf("expected ErrBuildConfig for unknown context, got: %v", err)
	}
}

// TestClient_Unreachable proves that a cluster whose API server cannot be
// reached surfaces a clear, wrapped ErrUnreachable error that names the
// cluster — connectivity failures are never swallowed.
func TestClient_Unreachable(t *testing.T) {
	// Point at a closed port on localhost so the connection is refused fast.
	kubeconfig := writeKubeconfig(t, "https://127.0.0.1:1")
	h := handleFor(t, "broken", kubeconfig, "maklaude")

	client, err := NewClient(h)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	_, err = client.CheckReachable(context.Background())
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected ErrUnreachable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected error to name the cluster %q, got: %v", "broken", err)
	}
}

// TestClient_ListReadsAgainstFakeClientset proves the apps/v1 and events read
// methods (added for health-signal collection) return the objects the API
// server holds, exercised here against a fake clientset wired in via
// NewClientWithInterface.
func TestClient_ListReadsAgainstFakeClientset(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-abc"}},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "evt-1"},
			Type:       corev1.EventTypeWarning,
			Reason:     "BackOff",
		},
	)
	client := NewClientWithInterface("fixture", fakeClient)
	if client.Name() != "fixture" {
		t.Fatalf("expected client name %q, got %q", "fixture", client.Name())
	}

	deps, err := client.ListDeployments(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDeployments failed: %v", err)
	}
	if len(deps) != 1 || deps[0].Name != "web" {
		t.Fatalf("unexpected deployments: %+v", deps)
	}

	rss, err := client.ListReplicaSets(context.Background(), "")
	if err != nil {
		t.Fatalf("ListReplicaSets failed: %v", err)
	}
	if len(rss) != 1 || rss[0].Name != "web-abc" {
		t.Fatalf("unexpected replicasets: %+v", rss)
	}

	events, err := client.ListEvents(context.Background(), "")
	if err != nil {
		t.Fatalf("ListEvents failed: %v", err)
	}
	if len(events) != 1 || events[0].Reason != "BackOff" {
		t.Fatalf("unexpected events: %+v", events)
	}
}

// TestClient_GetPod proves the single-pod read returns the named pod from the
// API server, exercised against a fake clientset.
func TestClient_GetPod(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}},
	)
	client := NewClientWithInterface("fixture", fakeClient)

	pod, err := client.GetPod(context.Background(), "team", "web")
	if err != nil {
		t.Fatalf("GetPod failed: %v", err)
	}
	if pod.Namespace != "team" || pod.Name != "web" {
		t.Fatalf("unexpected pod: %+v", pod)
	}

	_, err = client.GetPod(context.Background(), "team", "absent")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("expected wrapped ErrUnreachable for a missing pod, got: %v", err)
	}
}

// TestClient_PodLogs proves the log read issues a GET on the pods/log subresource
// with the container, previous, and tail bound plumbed through, and returns the
// (bounded) body. It asserts on the plumbing, not exact cluster output — the fake
// clientset serves a canned body.
func TestClient_PodLogs(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}},
	)
	var gotOpts *corev1.PodLogOptions
	fakeClient.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		gotOpts, _ = action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		return true, &corev1.Pod{}, nil
	})
	client := NewClientWithInterface("fixture", fakeClient)

	data, err := client.PodLogs(context.Background(), "team", "web", "app", true, 25)
	if err != nil {
		t.Fatalf("PodLogs failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty log body from the fake clientset")
	}
	if gotOpts == nil {
		t.Fatal("expected a pods/log GET to be issued")
	}
	if gotOpts.Container != "app" {
		t.Fatalf("expected container %q, got %q", "app", gotOpts.Container)
	}
	if !gotOpts.Previous {
		t.Fatal("expected the previous-instance flag to be plumbed through")
	}
	if gotOpts.TailLines == nil || *gotOpts.TailLines != 25 {
		t.Fatalf("expected tail bound 25 to be plumbed through, got %v", gotOpts.TailLines)
	}
}

// TestClient_PodLogsNoTail proves a non-positive tail leaves the API tail bound
// unset (the caller declined an explicit line bound); the byte cap still applies.
func TestClient_PodLogsNoTail(t *testing.T) {
	fakeClient := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "web"}},
	)
	var gotOpts *corev1.PodLogOptions
	fakeClient.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "log" {
			return false, nil, nil
		}
		gotOpts, _ = action.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		return true, &corev1.Pod{}, nil
	})
	client := NewClientWithInterface("fixture", fakeClient)

	if _, err := client.PodLogs(context.Background(), "team", "web", "app", false, 0); err != nil {
		t.Fatalf("PodLogs failed: %v", err)
	}
	if gotOpts == nil {
		t.Fatal("expected a pods/log GET to be issued")
	}
	if gotOpts.TailLines != nil {
		t.Fatalf("expected no tail bound for a non-positive tailLines, got %v", *gotOpts.TailLines)
	}
}

// TestClient_ReadOperationsBlockedFromWriting proves the live client cannot
// mutate: the read-only transport guard is installed on the client's config,
// so even a hand-built mutating request through that transport is refused. This
// is the live-client counterpart to the transport unit tests.
func TestClient_ReadOperationsBlockedFromWriting(t *testing.T) {
	kubeconfig := writeKubeconfig(t, "https://example.test")
	h := handleFor(t, "guarded", kubeconfig, "maklaude")

	restCfg, err := restConfigForHandle(h)
	if err != nil {
		t.Fatalf("restConfigForHandle failed: %v", err)
	}
	if restCfg.WrapTransport == nil {
		t.Fatal("expected WrapTransport to be set (write-guard missing)")
	}

	guarded := restCfg.WrapTransport(http.DefaultTransport)
	req, err := http.NewRequest(http.MethodDelete, "https://example.test/api/v1/namespaces/default", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := guarded.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("expected nil response for blocked DELETE, got %+v", resp)
	}
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("expected client transport to block DELETE with ErrWriteForbidden, got: %v", err)
	}
}
