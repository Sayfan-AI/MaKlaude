//go:build e2e

// The live half of the milestone's most important negative case: a cluster nobody
// marked chaos-eligible, registered alongside one that is, in the same run against
// the same real API server — and receiving no chaos write.
//
// # Why this exists when internal/chaos/leak_test.go already covers it
//
// That test (PR #222) drives every route into the chaos write path with an unmarked
// cluster and asserts its stub server recorded nothing. What a stub can prove is what
// MaKlaude *composes*: no request was built. What it cannot prove is anything about a
// cluster, because there is no cluster in it — the "API server" is a handler in the
// same process, and its silence is a statement about a fixture.
//
// This job has a real one. So the question becomes the one an operator actually asks:
// with a live API server, a real ServiceAccount token, a real Chaos Mesh admission
// chain and a registry holding both clusters, does the unmarked cluster get a write?
//
// # Why the two clusters are the same cluster
//
// Eligibility is a marker in MaKlaude's config, not a property a cluster can report.
// Two *different* clusters would give the negative control an environmental
// explanation — a missing CRD, an absent grant, an unreachable endpoint — and any of
// those would produce the same silence while proving nothing about the marker. So the
// same API server is registered twice, with the same credential, and the ONLY
// difference between the two registrations is the chaos block.
//
// Each registration goes through its own recording reverse proxy, which is what turns
// "nothing was written" into something observable on a real cluster. The chaos job
// deliberately mounts no audit policy — its whole purpose is mutating requests from
// the chaos identity, so an audit log here would be a list of the writes the other
// tests assert happen, and it could not tell a write *for* the marked cluster apart
// from one for the unmarked cluster anyway: both would be the same request, from the
// same identity, to the same server. The proxy pair can, because the route is the
// thing that differs.
//
// # The two controls, and why neither is optional
//
//   - The unmarked route is proven LIVE before anything else: a probe through it is
//     answered by the real API server. Without that, "the unmarked cluster saw
//     nothing" is also true of a proxy that is broken, deaf, or pointed at nowhere —
//     and the test would keep passing through the exact regression it exists to catch.
//   - The marked route takes a real, admitted create through its own proxy. Without
//     it, every assertion here is equally satisfied by a chaos path that is simply
//     broken for everyone, which is not what eligibility means.
package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// unmarkedClusterName is the second registration of the same physical cluster: no
// chaos block, which is the default and the shape of every config written before
// Milestone 6.
const unmarkedClusterName = "maklaude-e2e-unmarked"

// livenessProbePath is the unmarked route's proof-of-life. `/livez` mutates nothing,
// so the probe can establish that the route works without itself being a write.
const livenessProbePath = "/livez"

// eligibilityProbeDuration differs from the lifecycle test's fault duration on purpose.
// An experiment's object name is a digest of its own shape, duration included, so a
// different duration is a different name — and this test's dry-run preview therefore
// cannot collide with a live experiment another test in this package left running.
const eligibilityProbeDuration = 3 * time.Minute

// recordedRequest is one request that arrived at a proxy, reduced to the fields that
// decide whether it was a chaos write.
type recordedRequest struct {
	Method string
	Path   string
}

func (r recordedRequest) String() string { return r.Method + " " + r.Path }

// recordingProxy is a plain-HTTP reverse proxy in front of the real API server that
// records every request before forwarding it.
//
// Forwarding rather than answering is the point: a proxy that replied on its own would
// be a stub with extra steps, and this file already has a stub-based sibling. The
// upstream response is the real cluster's, so a request that reaches here reaches the
// API server too — which is what makes the marked route's create a genuine admitted
// create and the unmarked route's probe genuine evidence that the route is open.
type recordingProxy struct {
	// URL is the http:// address to put in a kubeconfig.
	URL string

	mu   sync.Mutex
	seen []recordedRequest
}

// newRecordingProxy starts a proxy in front of upstream, using tls for the hop to it.
func newRecordingProxy(t *testing.T, upstream *url.URL, tls *http.Transport) *recordingProxy {
	t.Helper()

	p := &recordingProxy{}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = tls
	// A proxy that fails silently would look exactly like a cluster that refused, so
	// say so loudly instead of letting the caller interpret a 502.
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		t.Errorf("the recording proxy could not reach the API server at %s: %v", upstream, err)
		w.WriteHeader(http.StatusBadGateway)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.seen = append(p.seen, recordedRequest{Method: r.Method, Path: r.URL.Path})
		p.mu.Unlock()
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	p.URL = srv.URL
	return p
}

// recorded returns what has arrived so far.
func (p *recordingProxy) recorded() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedRequest(nil), p.seen...)
}

// apiServerTransport builds the hop from a proxy to the real API server out of the
// chaos ServiceAccount's kubeconfig: its CA, and nothing else.
//
// Deliberately no bearer token. The credential travels in the client's own kubeconfig
// and passes through as an ordinary header, so the identity the API server sees is the
// one MaKlaude presented rather than one this fixture supplied. A proxy that injected
// its own token could make an unauthenticated request look authorized and would be
// testing the fixture.
func apiServerTransport(t *testing.T, kubeconfig, contextName string) (*url.URL, *http.Transport) {
	t.Helper()

	cfg := restConfigFor(t, kubeconfig, contextName)
	upstream, err := url.Parse(cfg.Host)
	if err != nil {
		t.Fatalf("parsing the API server URL %q: %v", cfg.Host, err)
	}
	tlsCfg, err := rest.TLSConfigFor(cfg)
	if err != nil {
		t.Fatalf("building TLS config for the API server: %v", err)
	}
	return upstream, &http.Transport{TLSClientConfig: tlsCfg}
}

// restConfigFor resolves one context out of a kubeconfig file.
func restConfigFor(t *testing.T, kubeconfig, contextName string) *rest.Config {
	t.Helper()

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: contextName},
	).ClientConfig()
	if err != nil {
		t.Fatalf("loading kubeconfig %q context %q: %v", kubeconfig, contextName, err)
	}
	return cfg
}

// bearerTokenFor returns the credential the chaos kubeconfig carries, so the proxied
// kubeconfig can present the same identity.
func bearerTokenFor(t *testing.T, kubeconfig, contextName string) string {
	t.Helper()

	token := strings.TrimSpace(restConfigFor(t, kubeconfig, contextName).BearerToken)
	if token == "" {
		t.Fatalf("the chaos kubeconfig %q carries no bearer token, so the proxied registration would be a different identity", kubeconfig)
	}
	return token
}

// writeProxiedKubeconfig writes one kubeconfig naming both proxies, under contexts
// "marked" and "unmarked".
//
// One file with two contexts rather than two files, for the reason
// internal/chaos/leak_test.go gives: it makes "the wrong handle was used" a visible
// outcome instead of an impossible one. current-context points at the UNMARKED proxy,
// so a code path that ignores the handle's selected context and falls back to the
// default lands on the route this test is watching rather than passing quietly.
func writeProxiedKubeconfig(t *testing.T, markedURL, unmarkedURL, token string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "proxied.kubeconfig")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: unmarked
clusters:
  - name: marked-cluster
    cluster:
      server: %s
  - name: unmarked-cluster
    cluster:
      server: %s
contexts:
  - name: marked
    context:
      cluster: marked-cluster
      user: maklaude-chaos
  - name: unmarked
    context:
      cluster: unmarked-cluster
      user: maklaude-chaos
users:
  - name: maklaude-chaos
    user:
      token: %s
`, markedURL, unmarkedURL, token)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the proxied kubeconfig: %v", err)
	}
	return path
}

// TestE2E_ChaosSkipsTheClusterNobodyMarked is T8's negative case, live.
func TestE2E_ChaosSkipsTheClusterNobodyMarked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	kubeconfig := requireChaosCluster(t)
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")

	upstream, transport := apiServerTransport(t, kubeconfig, contextName)
	marked := newRecordingProxy(t, upstream, transport)
	unmarked := newRecordingProxy(t, upstream, transport)
	t.Logf("both registrations point at %s, through separate recording proxies", upstream)

	// --- Control 1: the unmarked route is open. ---
	//
	// Run first and against the real API server, so that everything after it is a
	// statement about the marker. A 200 here means requests sent this way arrive; the
	// silence asserted at the end therefore cannot be explained by a dead proxy, a bad
	// upstream, or a recorder that does not record.
	token := bearerTokenFor(t, kubeconfig, contextName)
	assertRouteIsLive(t, unmarked, token)

	proxied := writeProxiedKubeconfig(t, marked.URL, unmarked.URL, token)

	reg, err := cluster.NewRegistry(&cluster.Config{Clusters: []cluster.Spec{
		{
			Name:       chaosClusterName,
			Kubeconfig: proxied,
			Context:    "marked",
			Chaos: &cluster.ChaosEligibility{
				Cluster:         chaosClusterName,
				Acknowledgement: cluster.ChaosAcknowledgementFor(chaosClusterName),
			},
		},
		{
			Name:       unmarkedClusterName,
			Kubeconfig: proxied,
			Context:    "unmarked",
			// No chaos block. Nobody marked this cluster.
		},
	}})
	if err != nil {
		t.Fatalf("building a registry over both registrations: %v", err)
	}

	t.Run("the registry mints no token for the unmarked cluster", func(t *testing.T) {
		target, err := reg.ChaosTarget(unmarkedClusterName)
		if !errors.Is(err, cluster.ErrChaosIneligible) {
			t.Errorf("Registry.ChaosTarget(%s) error = %v, want ErrChaosIneligible", unmarkedClusterName, err)
		}
		if target == nil {
			return
		}
		// A token escaped. Spend it against the live cluster rather than stopping
		// here: the wire assertion at the end can only fire on a request that was
		// actually attempted, and reporting the escape without attempting one would
		// leave it decorative. Ignoring the error is how this leak happens in
		// practice, so that is what gets driven.
		t.Errorf("Registry.ChaosTarget(%s) returned a token: %#v", unmarkedClusterName, target)
		injector, err := chaos.NewInjector(target, kube.ExecuteEnabled)
		if err != nil {
			return
		}
		_, _ = injector.Inject(ctx, podFailureExperiment(eligibilityProbeDuration))
	})

	t.Run("its own handle mints no token either", func(t *testing.T) {
		h, ok := reg.Get(unmarkedClusterName)
		if !ok {
			t.Fatalf("cluster %s is not in the registry; the fixture is wrong", unmarkedClusterName)
		}
		target, err := h.ChaosTarget()
		if !errors.Is(err, cluster.ErrChaosIneligible) {
			t.Errorf("Handle.ChaosTarget() error = %v, want ErrChaosIneligible", err)
		}
		if target != nil {
			t.Errorf("Handle.ChaosTarget() returned a token: %#v", target)
		}
	})

	t.Run("the sweep cannot reach it", func(t *testing.T) {
		// ChaosTargets is what the reaper enumerates, and a reaper is a deleter. A
		// registry that offered the unmarked cluster here would hand DELETE authority
		// over a cluster nobody marked to the one component that runs unattended.
		var named []string
		for _, target := range reg.ChaosTargets() {
			named = append(named, target.Handle().Name())
		}
		if len(named) != 1 || named[0] != chaosClusterName {
			t.Errorf("ChaosTargets() = %v, want exactly [%s]", named, chaosClusterName)
		}
	})

	// --- Control 2: the marked route writes, for real. ---
	//
	// Dry-run, which on this cluster is a genuine admitted create: the real CRD schema
	// and Chaos Mesh's own webhook both run on a dryRun=All create — TestE2E_
	// ChaosLifecycle asserts that independently — and nothing is persisted, so this
	// test leaves no fault and no object behind and adds no wait to the job.
	t.Run("the marked cluster in the same registry still reaches the API server", func(t *testing.T) {
		target, err := reg.ChaosTarget(chaosClusterName)
		if err != nil {
			t.Fatalf("ChaosTarget(%s) error = %v, want a token", chaosClusterName, err)
		}
		injector, err := chaos.NewInjector(target, kube.ExecuteDryRun)
		if err != nil {
			t.Fatalf("building injector: %v", err)
		}
		preview, err := injector.Inject(ctx, podFailureExperiment(eligibilityProbeDuration))
		if err != nil {
			t.Fatalf("the marked cluster refused an experiment the other chaos tests admit, so this proves nothing about eligibility: %v", err)
		}
		if !preview.DryRun || preview.UID != "" {
			t.Fatalf("a preview must create nothing: %+v", preview)
		}

		posts := 0
		for _, r := range marked.recorded() {
			if r.Method == http.MethodPost {
				posts++
			}
		}
		if posts == 0 {
			t.Fatalf("the marked route recorded no create, so the proxy pair is not carrying MaKlaude's traffic: %v", marked.recorded())
		}
		t.Logf("marked route carried %d create(s) to the real API server", posts)
	})

	assertUnmarkedUntouched(t, unmarked)
}

// assertRouteIsLive requires the proxy to forward a request to the real API server and
// to have recorded it.
//
// Both halves matter and they fail differently: an unanswered probe means the route is
// dead, so a later silence would be meaningless, and an unrecorded probe means the
// instrument is deaf, so a later silence would be unmeasured.
func assertRouteIsLive(t *testing.T, p *recordingProxy, token string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL+livenessProbePath, nil)
	if err != nil {
		t.Fatalf("building the liveness probe: %v", err)
	}
	// The same credential MaKlaude would present. Anonymous would also be answered on
	// a default kind cluster, but then the probe would prove a route open to nobody in
	// particular, while the silence below is about this identity on this route.
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probing the unmarked route: %v; its silence below would not be evidence about eligibility", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the unmarked route did not reach a healthy API server (%s: %q); its silence below would not be evidence about eligibility",
			resp.Status, strings.TrimSpace(string(body)))
	}

	seen := p.recorded()
	if len(seen) != 1 || seen[0].Path != livenessProbePath {
		t.Fatalf("the unmarked route's proxy did not record its own liveness probe (got %v); every 'saw nothing' assertion below would be vacuous", seen)
	}
	t.Logf("unmarked route is live and recording: the real API server answered %s through it", livenessProbePath)
}

// assertUnmarkedUntouched fails if anything beyond the liveness probe arrived.
func assertUnmarkedUntouched(t *testing.T, p *recordingProxy) {
	t.Helper()

	for _, r := range p.recorded() {
		if r.Method == http.MethodGet && r.Path == livenessProbePath {
			continue
		}
		t.Errorf("a request reached the cluster nobody marked chaos-eligible, on a live API server: %s", r)
	}
}
