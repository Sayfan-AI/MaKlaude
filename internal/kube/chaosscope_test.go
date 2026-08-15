package kube

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// chaosTargetFor builds a chaos capability token for a cluster whose config
// carries the eligibility marker. It goes through cluster.NewRegistry rather than
// constructing a token directly, because a token cannot be constructed directly —
// the interface is sealed by an unexported method, which is the property T1 shipped.
func chaosTargetFor(t *testing.T, name, kubeconfig, kctx string) cluster.ChaosTarget {
	t.Helper()
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{
			Name:       name,
			Kubeconfig: kubeconfig,
			Context:    kctx,
			Chaos: &cluster.ChaosEligibility{
				Cluster:         name,
				Acknowledgement: cluster.ChaosAcknowledgementFor(name),
			},
		}},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	target, err := reg.ChaosTarget(name)
	if err != nil {
		t.Fatalf("minting chaos target: %v", err)
	}
	return target
}

const chaosCollectionPath = ChaosAPIPathPrefix + "v1alpha1/namespaces/maklaude-chaos/podchaos"

// TestChaosRestConfig_RequiresATarget proves the door cannot be opened without the
// capability token. A nil target is what a caller who skipped the eligibility check
// has, and it must fail as ineligible rather than defaulting to some cluster.
func TestChaosRestConfig_RequiresATarget(t *testing.T) {
	cfg, err := ChaosRestConfig(nil, WriteScope{Method: http.MethodPost, Path: chaosCollectionPath})
	if !errors.Is(err, cluster.ErrChaosIneligible) {
		t.Fatalf("expected ErrChaosIneligible for a nil target, got: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected no config alongside the error, got %+v", cfg)
	}
}

// TestChaosRestConfig_RefusesNonChaosMutations is the narrowing. Eligibility is
// permission to break a cluster with chaos experiments; it is not permission to
// patch its deployments, cordon its nodes, or delete its pods. Each of those is a
// real path from the executor's catalog, refused here even though the cluster IS
// eligible.
func TestChaosRestConfig_RefusesNonChaosMutations(t *testing.T) {
	target := chaosTargetFor(t, "kind-lab", writeKubeconfig(t, "https://127.0.0.1:1"), "maklaude")

	cases := map[string]WriteScope{
		"patch a deployment": {Method: http.MethodPatch, Path: "/apis/apps/v1/namespaces/prod/deployments/web"},
		"patch a node":       {Method: http.MethodPatch, Path: "/api/v1/nodes/node-a"},
		"delete a pod":       {Method: http.MethodDelete, Path: "/api/v1/namespaces/prod/pods/web-1"},
		"another CRD group":  {Method: http.MethodPost, Path: "/apis/other.example.com/v1alpha1/namespaces/x/things"},
		// A prefix test alone would admit this: it starts with the chaos prefix and
		// resolves somewhere else entirely.
		"traversal out of the group": {Method: http.MethodPost, Path: ChaosAPIPathPrefix + "../../api/v1/namespaces/prod/pods/web-1"},
		// The group root is not an object.
		"the group root": {Method: http.MethodPost, Path: ChaosAPIPathPrefix},
	}

	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := ChaosRestConfig(target, scope)
			if !errors.Is(err, ErrNotChaosScope) {
				t.Fatalf("expected ErrNotChaosScope, got: %v", err)
			}
			if cfg != nil {
				t.Fatalf("expected no config alongside the error, got %+v", cfg)
			}
			if !strings.Contains(err.Error(), "kind-lab") {
				t.Errorf("refusal should name the cluster, got: %v", err)
			}
		})
	}
}

// TestChaosRestConfig_ZeroScopeGrantsNoMutation proves the constructor's probe
// grants nothing. The zero scope is permitted so a caller can verify a handle
// yields a usable client, and the resulting client must still refuse every write.
func TestChaosRestConfig_ZeroScopeGrantsNoMutation(t *testing.T) {
	stub := newStubAPIServer(t, `{}`)
	target := chaosTargetFor(t, "kind-lab", writeKubeconfig(t, stub.URL), "maklaude")

	cfg, err := ChaosRestConfig(target, WriteScope{})
	if err != nil {
		t.Fatalf("zero scope should build a config: %v", err)
	}

	if err := doRequest(t, cfg, http.MethodPost, stub.URL+chaosCollectionPath); !errors.Is(err, ErrWriteOutOfScope) {
		t.Fatalf("expected ErrWriteOutOfScope under the zero scope, got: %v", err)
	}
	if err := doRequest(t, cfg, http.MethodGet, stub.URL+chaosCollectionPath); err != nil {
		t.Fatalf("reads must still pass under the zero scope: %v", err)
	}
}

// TestChaosRestConfig_AdmitsExactlyTheChaosRequest proves the admitted case is one
// request and not a class of them: the same method against a neighbouring object,
// and a different method against the same path, are both refused by the reused
// WriteScope guard.
func TestChaosRestConfig_AdmitsExactlyTheChaosRequest(t *testing.T) {
	stub := newStubAPIServer(t, `{}`)
	target := chaosTargetFor(t, "kind-lab", writeKubeconfig(t, stub.URL), "maklaude")

	scope := WriteScope{Method: http.MethodPost, Path: chaosCollectionPath}
	cfg, err := ChaosRestConfig(target, scope)
	if err != nil {
		t.Fatalf("building chaos config: %v", err)
	}

	if err := doRequest(t, cfg, http.MethodPost, stub.URL+chaosCollectionPath); err != nil {
		t.Fatalf("the pinned request must be admitted: %v", err)
	}

	refused := map[string]struct{ method, url string }{
		"a different namespace":           {http.MethodPost, stub.URL + ChaosAPIPathPrefix + "v1alpha1/namespaces/other/podchaos"},
		"a different kind":                {http.MethodPost, stub.URL + ChaosAPIPathPrefix + "v1alpha1/namespaces/maklaude-chaos/networkchaos"},
		"a delete on the same collection": {http.MethodDelete, stub.URL + chaosCollectionPath},
	}
	for name, req := range refused {
		t.Run(name, func(t *testing.T) {
			if err := doRequest(t, cfg, req.method, req.url); !errors.Is(err, ErrWriteOutOfScope) {
				t.Fatalf("expected ErrWriteOutOfScope, got: %v", err)
			}
		})
	}
}

// TestChaosRestConfig_PinsJSON proves the chaos path inherits the write path's
// JSON pinning, which is what keeps a DELETE's DeleteOptions body readable by the
// dry-run check and keeps an audited request legible.
func TestChaosRestConfig_PinsJSON(t *testing.T) {
	target := chaosTargetFor(t, "kind-lab", writeKubeconfig(t, "https://127.0.0.1:1"), "maklaude")

	cfg, err := ChaosRestConfig(target, WriteScope{Method: http.MethodPost, Path: chaosCollectionPath})
	if err != nil {
		t.Fatalf("building chaos config: %v", err)
	}
	if cfg.ContentType != "application/json" {
		t.Fatalf("expected JSON pinned on the chaos write path, got %q", cfg.ContentType)
	}
}

// doRequest sends one raw request through a config's guarded transport and returns
// the transport-level error, if any.
func doRequest(t *testing.T, cfg *rest.Config, method, url string) error {
	t.Helper()
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatalf("building http client: %v", err)
	}
	req, err := http.NewRequest(method, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}
