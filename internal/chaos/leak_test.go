package chaos

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// The leak test: a chaos write aimed at a cluster nobody marked must not happen.
//
// Milestone 6 narrows the no-writes guarantee to "no mutating verb except chaos
// CRDs, on chaos-eligible clusters." Everything that makes that hold is already
// tested one link at a time — internal/cluster proves an ineligible cluster mints
// no token, internal/kube proves the door refuses a non-chaos scope, and
// injector_test.go proves the injector sends what it says it sends. What none of
// those covers is the CLAIM, which is about the composition: given a registry that
// holds both an eligible and an ineligible cluster, nothing reaches the second one.
//
// A link-by-link proof and a composition proof fail differently. Every link can be
// individually correct while a new call site routes around all of them, and that is
// the failure this file is here to make loud. So there are two halves:
//
//  1. **The wire.** Two live stub API servers, one registry holding both clusters,
//     every route into the chaos write path attempted against the ineligible one,
//     and an assertion that its server received nothing. The eligible cluster runs
//     a real injection in the same test as the positive control — without it, a
//     test that asserts "no requests" passes just as well when the write path is
//     broken for everyone, which proves nothing about eligibility.
//
//  2. **The door count.** The wire half can only exercise the doors that exist
//     today. A structural assertion over the package's exported signatures fails
//     the build when a NEW door appears that takes a cluster identity instead of a
//     capability token, which is the shape the next leak would actually have.
//
// The stub servers are the ones injector_test.go already uses, on purpose: a
// recorder written specially for this test could be silently deaf, and then "the
// ineligible cluster saw nothing" would be a statement about the recorder.

const (
	// leakEligibleCluster carries a human's acknowledgement; leakIneligibleCluster
	// carries no chaos block at all, which is the default and the shape of every
	// config written before this milestone.
	leakEligibleCluster   = "kind-lab"
	leakIneligibleCluster = "prod"

	// leakProbePath is the negative control's request path. See newLeakFixture.
	leakProbePath = "/livez"
)

// leakFixture is one registry over two clusters, each pointed at its own recording
// stub through a single shared kubeconfig.
//
// One kubeconfig with two contexts rather than two files is deliberate: it makes
// "the wrong handle was used" a visible outcome instead of an impossible one. If
// eligibility were ever resolved against the wrong cluster, the request would land
// on the other stub and be recorded there, rather than failing to connect.
type leakFixture struct {
	reg        *cluster.Registry
	eligible   *chaosStub
	ineligible *chaosStub
}

func newLeakFixture(t *testing.T) leakFixture {
	t.Helper()

	f := leakFixture{eligible: newChaosStub(t), ineligible: newChaosStub(t)}

	// Negative control, run before anything else. The whole wire half rests on
	// "the ineligible stub recorded nothing", and that sentence is also true of a
	// server that is down, of a URL nothing could reach, and of a recorder that
	// does not record. Proving the instrument can register a hit is what makes its
	// silence afterwards evidence about eligibility rather than about the fixture.
	probeLeakStub(t, f.ineligible)

	kubeconfig := writeLeakKubeconfig(t, f.eligible.URL, f.ineligible.URL)
	reg, err := cluster.NewRegistry(&cluster.Config{Clusters: []cluster.Spec{
		{
			Name:       leakEligibleCluster,
			Kubeconfig: kubeconfig,
			Context:    "eligible",
			Chaos: &cluster.ChaosEligibility{
				Cluster:         leakEligibleCluster,
				Acknowledgement: cluster.ChaosAcknowledgementFor(leakEligibleCluster),
			},
		},
		{
			Name:       leakIneligibleCluster,
			Kubeconfig: kubeconfig,
			Context:    "ineligible",
			// No Chaos block. Nobody marked this cluster.
		},
	}})
	if err != nil {
		t.Fatalf("building the two-cluster registry: %v", err)
	}
	f.reg = reg
	return f
}

// probeLeakStub sends one request to the stub and requires it to be recorded.
func probeLeakStub(t *testing.T, stub *chaosStub) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, stub.URL+leakProbePath, nil)
	if err != nil {
		t.Fatalf("building the liveness probe: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("probing the ineligible stub: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	seen := stub.recorded()
	if len(seen) != 1 || seen[0].Path != leakProbePath {
		t.Fatalf("the ineligible stub did not record its own liveness probe (got %+v); "+
			"every 'saw nothing' assertion below would be vacuous", seen)
	}
}

// assertIneligibleUntouched fails if the ineligible cluster's API server saw
// anything beyond the fixture's own liveness probe.
func (f leakFixture) assertIneligibleUntouched(t *testing.T) {
	t.Helper()

	for _, r := range f.ineligible.recorded() {
		if r.Path == leakProbePath {
			continue
		}
		t.Errorf("a request reached the cluster nobody marked chaos-eligible: %s %s (body %v)",
			r.Method, r.Path, r.Body)
	}
}

// writeLeakKubeconfig writes one kubeconfig naming both stubs, under contexts
// "eligible" and "ineligible". current-context points at the ineligible cluster so
// that a code path which ignores the handle's selected context and falls back to
// the default lands on the stub this test is watching, rather than passing quietly.
func writeLeakKubeconfig(t *testing.T, eligibleURL, ineligibleURL string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: ineligible
clusters:
  - name: eligible-cluster
    cluster:
      server: %s
      insecure-skip-tls-verify: true
  - name: ineligible-cluster
    cluster:
      server: %s
      insecure-skip-tls-verify: true
contexts:
  - name: eligible
    context:
      cluster: eligible-cluster
      user: tester
  - name: ineligible
    context:
      cluster: ineligible-cluster
      user: tester
users:
  - name: tester
    user:
      token: test-token
`, eligibleURL, ineligibleURL)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// TestChaosWriteCannotReachAnIneligibleCluster walks every route into the chaos
// write path with the unmarked cluster and requires its API server to stay silent,
// while the marked cluster in the same registry takes a real injection.
func TestChaosWriteCannotReachAnIneligibleCluster(t *testing.T) {
	f := newLeakFixture(t)

	t.Run("the registry mints no token for it", func(t *testing.T) {
		target, err := f.reg.ChaosTarget(leakIneligibleCluster)
		if !errors.Is(err, cluster.ErrChaosIneligible) {
			t.Errorf("Registry.ChaosTarget(%s) error = %v, want ErrChaosIneligible", leakIneligibleCluster, err)
		}
		if target == nil {
			return
		}
		t.Errorf("Registry.ChaosTarget(%s) returned a token: %#v", leakIneligibleCluster, target)

		// A token escaped, so spend it. Reporting the escape and stopping would
		// leave assertIneligibleUntouched decorative — it can only fire on a
		// request that was actually attempted, and nothing else in this test
		// attempts one against the unmarked cluster. Driving the real path here is
		// what makes "its API server saw nothing" a claim with teeth rather than a
		// restatement of "we never called it".
		injector, err := NewInjector(target, kube.ExecuteEnabled)
		if err != nil {
			return
		}
		_, _ = injector.Inject(context.Background(), podKill())
	})

	t.Run("its own handle mints no token either", func(t *testing.T) {
		h, ok := f.reg.Get(leakIneligibleCluster)
		if !ok {
			t.Fatalf("cluster %s is not in the registry; the fixture is wrong", leakIneligibleCluster)
		}
		target, err := h.ChaosTarget()
		if !errors.Is(err, cluster.ErrChaosIneligible) {
			t.Errorf("Handle.ChaosTarget() error = %v, want ErrChaosIneligible", err)
		}
		if target != nil {
			t.Errorf("Handle.ChaosTarget() returned a token: %#v", target)
		}
	})

	t.Run("it is absent from the eligible list", func(t *testing.T) {
		var named []string
		for _, target := range f.reg.ChaosTargets() {
			named = append(named, target.Handle().Name())
		}
		if len(named) != 1 || named[0] != leakEligibleCluster {
			t.Errorf("ChaosTargets() = %v, want exactly [%s]", named, leakEligibleCluster)
		}
	})

	t.Run("no token means no injector and no reaper", func(t *testing.T) {
		// The two constructors are the only ways to get something that can write.
		// Both are reached here with the nil a caller holds after ignoring the
		// error above, because "ignored the error" is how this leak would happen
		// in practice rather than by forging a token.
		if i, err := NewInjector(nil, kube.ExecuteEnabled); err == nil {
			t.Errorf("NewInjector(nil) built an injector: %#v", i)
		}
		if r, err := NewReaper(nil, DefaultOrphanGrace, nil); err == nil {
			t.Errorf("NewReaper(nil) built a reaper: %#v", r)
		}
	})

	t.Run("the transport door refuses a nil token", func(t *testing.T) {
		// A chaos-shaped scope, so the refusal is about the missing capability and
		// not about the scope: this is the call that would succeed if the door
		// treated "no target" as "no restriction".
		scope := kube.WriteScope{
			Method: http.MethodPost,
			Path:   kube.ChaosAPIPathPrefix + "v1alpha1/namespaces/maklaude-chaos/podchaos",
		}
		if cfg, err := kube.ChaosRestConfig(nil, scope); err == nil {
			t.Errorf("ChaosRestConfig(nil, %s) built a config: %#v", scope.String(), cfg)
		} else if !errors.Is(err, cluster.ErrChaosIneligible) {
			t.Errorf("ChaosRestConfig(nil, …) error = %v, want ErrChaosIneligible", err)
		}
	})

	// Positive control. Without a write that DOES land, every assertion above is
	// also satisfied by a chaos path that is simply broken, and the test would
	// keep passing through the exact regression it exists to catch.
	t.Run("the marked cluster in the same registry still takes a real injection", func(t *testing.T) {
		target, err := f.reg.ChaosTarget(leakEligibleCluster)
		if err != nil {
			t.Fatalf("ChaosTarget(%s) error = %v, want a token", leakEligibleCluster, err)
		}
		injector, err := NewInjector(target, kube.ExecuteEnabled)
		if err != nil {
			t.Fatalf("building injector: %v", err)
		}
		if _, err := injector.Inject(context.Background(), podKill()); err != nil {
			t.Fatalf("injecting into the eligible cluster: %v", err)
		}
		if got := f.eligible.requestsFor(http.MethodPost); len(got) != 1 {
			t.Fatalf("expected exactly 1 create on the eligible cluster, got %d", len(got))
		}
		f.eligible.assertOnlyChaosPaths(t)
	})

	f.assertIneligibleUntouched(t)
}

// TestChaosPackageOffersNoDoorButTheCapabilityToken is the structural half.
//
// The wire test can only try the doors that exist when it is written. This one
// fails the build when a new one appears, and the property it pins is narrow enough
// to be mechanical rather than a judgement call: **the only type from
// internal/cluster that may appear in this package's exported signatures is
// ChaosTarget**. A constructor taking a *cluster.Handle, a *cluster.Registry, or a
// cluster.Spec is a way to aim a chaos write by naming a cluster instead of by
// holding proof that a human marked it, and every leak of that shape trips this
// regardless of what the new function is called or what it does.
//
// A cluster NAME is a string, and this test deliberately says nothing about
// strings: no mechanical rule separates a parameter that names a cluster from any
// other string, and a guard whose true positives and false positives have the same
// syntax gets muted rather than fixed. The name-shaped door is covered instead by
// the wire test above, where Registry.ChaosTarget is the one function that turns a
// name into a token and it refuses.
func TestChaosPackageOffersNoDoorButTheCapabilityToken(t *testing.T) {
	fset := token.NewFileSet()
	found := map[string][]string{}
	sawNewInjector := false

	for _, name := range productionFilesHere(t) {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if !exportedFunc(d) {
					continue
				}
				if d.Name.Name == "NewInjector" && d.Recv == nil {
					sawNewInjector = true
					assertTakesAChaosTarget(t, fset, d)
				}
				where := fset.Position(d.Pos()).String()
				collectClusterRefs(d.Type.Params, where, found)
				collectClusterRefs(d.Type.Results, where, found)
			case *ast.GenDecl:
				collectExportedFieldRefs(fset, d, found)
			}
		}
	}

	if !sawNewInjector {
		t.Error("NewInjector is gone; this test's positive anchor no longer exists and the set below proves less than it looks")
	}

	for _, name := range sortedNames(found) {
		if name == "ChaosTarget" {
			continue
		}
		t.Errorf("exported signature exposes cluster.%s at %s: the chaos write path must be reachable only by "+
			"holding a cluster.ChaosTarget, because a cluster identity is something a caller can name without "+
			"a human ever having marked it eligible", name, strings.Join(found[name], ", "))
	}
}

// productionFilesHere lists the package's non-test .go files. It fails rather than
// returning an empty list: a structural assertion over nothing passes silently, and
// this one is meant to keep holding after the package is reorganised.
func productionFilesHere(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the chaos package directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		t.Fatal("no non-test .go files found in the chaos package; this assertion would pass over nothing")
	}
	sort.Strings(out)
	return out
}

// exportedFunc reports whether d is part of the package's exported surface: an
// exported function, or an exported method on an exported receiver type.
func exportedFunc(d *ast.FuncDecl) bool {
	if !d.Name.IsExported() {
		return false
	}
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return true
	}
	return ast.IsExported(receiverTypeName(d.Recv.List[0].Type))
}

func receiverTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr: // generic receiver, e.g. Foo[T]
		return receiverTypeName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// collectClusterRefs records every `cluster.X` selector appearing anywhere under
// node, keyed by X and carrying the source positions for the failure message.
func collectClusterRefs(node ast.Node, where string, into map[string][]string) {
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "cluster" {
			return true
		}
		into[sel.Sel.Name] = append(into[sel.Sel.Name], where)
		return true
	})
}

// collectExportedFieldRefs covers the doors that are not functions: an exported
// field on an exported struct type is settable from outside the package, so a
// `Cluster *cluster.Handle` field would be a way to aim a write that no signature
// mentions.
func collectExportedFieldRefs(fset *token.FileSet, d *ast.GenDecl, into map[string][]string) {
	if d.Tok != token.TYPE {
		return
	}
	for _, spec := range d.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok || !ts.Name.IsExported() {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || st.Fields == nil {
			continue
		}
		for _, field := range st.Fields.List {
			exported := len(field.Names) == 0 // embedded
			for _, n := range field.Names {
				if n.IsExported() {
					exported = true
				}
			}
			if !exported {
				continue
			}
			collectClusterRefs(field.Type, fset.Position(field.Pos()).String(), into)
		}
	}
}

// assertTakesAChaosTarget pins NewInjector's capability parameter by name, so the
// door cannot be widened to a bare interface or a cluster identity while the set
// assertion above still reads as satisfied.
func assertTakesAChaosTarget(t *testing.T, fset *token.FileSet, d *ast.FuncDecl) {
	t.Helper()

	if d.Type.Params == nil || len(d.Type.Params.List) == 0 {
		t.Fatalf("%s: NewInjector takes no parameters; it must take a cluster.ChaosTarget", fset.Position(d.Pos()))
	}
	first := d.Type.Params.List[0].Type
	sel, ok := first.(*ast.SelectorExpr)
	if !ok {
		t.Fatalf("%s: NewInjector's first parameter is not cluster.ChaosTarget", fset.Position(d.Pos()))
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "cluster" || sel.Sel.Name != "ChaosTarget" {
		t.Fatalf("%s: NewInjector's first parameter is %s.%s, want cluster.ChaosTarget",
			fset.Position(d.Pos()), identName(sel.X), sel.Sel.Name)
	}
}

func identName(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

func sortedNames(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
