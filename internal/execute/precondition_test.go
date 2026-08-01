package execute

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// remediatePackageDir is where the catalog this package must stay exhaustive over
// is declared. The two exhaustiveness tests below read it rather than trusting a
// hand-maintained list — see [TestEveryPreconditionKindHasACheck] for why.
const remediatePackageDir = "../remediate"

// TestEveryPreconditionKindHasACheck is the guard that makes "exhaustive over
// PreconditionKind" a build-time fact rather than a promise.
//
// It reads the kinds the remediate package actually DECLARES, out of its source, and
// compares them against the checks this package implements. A hand-written list of
// expected kinds would prove nothing: the failure mode being guarded against is
// someone adding a kind and not noticing this package, and that same person would
// not update a list here either. Parsing the declaration site is the only version of
// this test that fails when it should.
//
// The direction that matters is a declared kind with no check — that is a condition
// that would silently never be verified. A check for a kind nobody declares is also
// reported, because it means either a rename this package missed or a check nothing
// can ever reach.
func TestEveryPreconditionKindHasACheck(t *testing.T) {
	declared := declaredConstants(t, "PreconditionKind")
	if len(declared) < 7 {
		t.Fatalf("only %d precondition kinds were found in %s; the parser is not seeing the declarations",
			len(declared), remediatePackageDir)
	}

	implemented := make(map[string]bool, len(preconditionChecks))
	for kind := range preconditionChecks {
		implemented[string(kind)] = true
	}

	for _, kind := range declared {
		if !implemented[kind] {
			t.Errorf("precondition kind %q is declared in remediate but has no check in preconditionChecks; "+
				"a kind with no check is a condition that is never verified", kind)
		}
	}
	for kind := range implemented {
		if !contains(declared, kind) {
			t.Errorf("preconditionChecks implements %q, which the remediate package does not declare; "+
				"it is either a stale name or a check nothing can reach", kind)
		}
	}
}

// TestEveryCatalogOperationHasAPlan is the same guard over the operation catalog.
// An operation with no plan is refused at execution time, so the consequence of
// forgetting one is a human approving an action MaKlaude then declines — visible,
// but only after someone has been asked to make a decision that could not be
// honored. Failing the build is cheaper.
func TestEveryCatalogOperationHasAPlan(t *testing.T) {
	declared := declaredConstants(t, "Operation")
	if len(declared) < 4 {
		t.Fatalf("only %d operations were found in %s; the parser is not seeing the declarations",
			len(declared), remediatePackageDir)
	}

	for _, op := range declared {
		pl, ok := planFor(remediate.Operation(op))
		if !ok {
			t.Errorf("operation %q is declared in remediate but has no plan in operationPlans", op)
			continue
		}
		if pl.rollback == RollbackUnclassified {
			t.Errorf("operation %q has a plan with no rollback classification", op)
		}
		if strings.TrimSpace(pl.rollbackNote) == "" {
			t.Errorf("operation %q has no plain-language rollback note; the approver was shown one and the executor must agree with it", op)
		}
		if pl.converged == nil {
			t.Errorf("operation %q has no convergence check, so nothing could tell whether it worked", op)
		}
		if pl.unsupported == "" && pl.mutate == nil {
			t.Errorf("operation %q is supported but has no send path", op)
		}
		if pl.rollback == RollbackPerformable && pl.unsupported == "" && pl.undo == nil {
			t.Errorf("operation %q is classified performable but carries no inverse action", op)
		}
	}

	for op := range operationPlans {
		if !contains(declared, string(op)) {
			t.Errorf("operationPlans covers %q, which the remediate package does not declare", op)
		}
	}
}

// TestAnUnknownPreconditionKindFailsClosed proves the dispatch refuses rather than
// passes for a kind it does not recognize. This is the behaviour that makes the
// exhaustiveness test above a safety net rather than a tidiness check: even if a new
// kind ships without a check, it blocks the action instead of waving it through.
func TestAnUnknownPreconditionKindFailsClosed(t *testing.T) {
	idx := newClusterIndex(newClusterModel().withNode("node-a").snapshot())
	target := remediate.Target{Cluster: testCluster, Kind: "node", Name: "node-a", ResourceVersion: "1001"}

	held, observed := checkPrecondition(idx, remediate.Precondition{
		Kind:        remediate.PreconditionKind("nodehasnotaint"),
		Description: "a condition from a future version of the catalog",
	}, target)

	if held {
		t.Fatal("an unimplemented precondition kind reported that it holds")
	}
	if !strings.Contains(observed, "no check in the execution layer") {
		t.Fatalf("the refusal does not explain why the kind could not be verified: %q", observed)
	}
}

// TestPreconditionChecks covers every implemented kind from both sides. The holding
// cases matter as much as the drifting ones: a check that refused everything would
// pass a test that only looked for refusals, and would block every remediation in
// the system.
func TestPreconditionChecks(t *testing.T) {
	nodeTarget := remediate.Target{Cluster: testCluster, Kind: "node", Name: "node-a", ResourceVersion: "1001"}
	podTarget := remediate.Target{Cluster: testCluster, Kind: "pod", Namespace: "shop", Name: "web-dead", ResourceVersion: "4004"}
	depTarget := remediate.Target{Cluster: testCluster, Kind: "deployment", Namespace: "shop", Name: "web", ResourceVersion: "2002"}

	fullModel := func() *clusterModel {
		return newClusterModel().
			withNode("node-a").
			withDeployment("shop", "web", 3, 4).
			withCrashLoopingPod("shop", "web-abc", "web-7d9").
			withFailedPod("shop", "web-dead", "web-7d9")
	}

	cases := []struct {
		name     string
		pc       remediate.Precondition
		target   remediate.Target
		mutate   func(*clusterModel)
		wantHeld bool
		wantSaid string
	}{
		{
			name:     "unchanged holds at the same resourceVersion",
			pc:       remediate.Precondition{Kind: remediate.PreconditionUnchanged, Expect: "1001"},
			target:   nodeTarget,
			wantHeld: true,
		},
		{
			name:     "unchanged fails when the object moved",
			pc:       remediate.Precondition{Kind: remediate.PreconditionUnchanged, Expect: "1001"},
			target:   nodeTarget,
			mutate:   func(m *clusterModel) { m.mutateNode("node-a", func(*health.NodeSignal) {}) },
			wantSaid: "changed after the proposal",
		},
		{
			name:     "unchanged fails when the object is gone",
			pc:       remediate.Precondition{Kind: remediate.PreconditionUnchanged, Expect: "1001"},
			target:   nodeTarget,
			mutate:   func(m *clusterModel) { delete(m.nodes, "node-a") },
			wantSaid: "no longer present",
		},
		{
			name:     "unchanged fails closed with no expected version",
			pc:       remediate.Precondition{Kind: remediate.PreconditionUnchanged},
			target:   nodeTarget,
			wantSaid: "names no expected resourceVersion",
		},
		{
			name:     "crashlooping holds while a container is still looping",
			pc:       remediate.Precondition{Kind: remediate.PreconditionPodCrashLooping, Expect: "shop/web-abc"},
			target:   depTarget,
			wantHeld: true,
		},
		{
			name:   "crashlooping fails once the pod recovers",
			pc:     remediate.Precondition{Kind: remediate.PreconditionPodCrashLooping, Expect: "shop/web-abc"},
			target: depTarget,
			mutate: func(m *clusterModel) {
				pod := m.pods["shop/web-abc"]
				pod.Containers = []health.ContainerSignal{{Name: "app", CrashLooping: false}}
				m.pods["shop/web-abc"] = pod
			},
			wantSaid: "no longer crashlooping",
		},
		{
			name:     "crashlooping fails closed when it names nothing",
			pc:       remediate.Precondition{Kind: remediate.PreconditionPodCrashLooping},
			target:   depTarget,
			wantSaid: "does not name a pod",
		},
		{
			name:     "podfailed holds for a pod still in phase Failed",
			pc:       remediate.Precondition{Kind: remediate.PreconditionPodFailed},
			target:   podTarget,
			wantHeld: true,
		},
		{
			name:   "podfailed holds for an evicted pod",
			pc:     remediate.Precondition{Kind: remediate.PreconditionPodFailed},
			target: podTarget,
			mutate: func(m *clusterModel) {
				pod := m.pods["shop/web-dead"]
				pod.Failed = false
				pod.Phase = "Running"
				pod.Reason = "Evicted"
				m.pods["shop/web-dead"] = pod
			},
			wantHeld: true,
		},
		{
			name:   "podfailed fails once the pod is running again",
			pc:     remediate.Precondition{Kind: remediate.PreconditionPodFailed},
			target: podTarget,
			mutate: func(m *clusterModel) {
				pod := m.pods["shop/web-dead"]
				pod.Failed = false
				pod.Phase = "Running"
				m.pods["shop/web-dead"] = pod
			},
			wantSaid: "must not be deleted",
		},
		{
			name:     "hascontroller holds for the same controller",
			pc:       remediate.Precondition{Kind: remediate.PreconditionPodHasController, Expect: "ReplicaSet/web-7d9"},
			target:   podTarget,
			wantHeld: true,
		},
		{
			name:   "hascontroller fails when the controller changed",
			pc:     remediate.Precondition{Kind: remediate.PreconditionPodHasController, Expect: "ReplicaSet/web-7d9"},
			target: podTarget,
			mutate: func(m *clusterModel) {
				pod := m.pods["shop/web-dead"]
				pod.Owners = []health.OwnerRef{{Kind: "ReplicaSet", Name: "web-999", Controller: true}}
				m.pods["shop/web-dead"] = pod
			},
			wantSaid: "not the pod the approval covered",
		},
		{
			name:   "hascontroller fails for a bare pod",
			pc:     remediate.Precondition{Kind: remediate.PreconditionPodHasController, Expect: "ReplicaSet/web-7d9"},
			target: podTarget,
			mutate: func(m *clusterModel) {
				pod := m.pods["shop/web-dead"]
				pod.Owners = nil
				m.pods["shop/web-dead"] = pod
			},
			wantSaid: "no controlling owner",
		},
		{
			name:     "nodenotready holds while the node is down",
			pc:       remediate.Precondition{Kind: remediate.PreconditionNodeNotReady},
			target:   nodeTarget,
			wantHeld: true,
		},
		{
			name:     "nodenotready fails once the node recovers",
			pc:       remediate.Precondition{Kind: remediate.PreconditionNodeNotReady},
			target:   nodeTarget,
			mutate:   func(m *clusterModel) { m.mutateNode("node-a", func(n *health.NodeSignal) { n.Ready = true }) },
			wantSaid: "Ready again",
		},
		{
			name:     "nodeschedulable holds while the node is uncordoned",
			pc:       remediate.Precondition{Kind: remediate.PreconditionNodeSchedulable},
			target:   nodeTarget,
			wantHeld: true,
		},
		{
			name:     "nodeschedulable fails once someone else cordons it",
			pc:       remediate.Precondition{Kind: remediate.PreconditionNodeSchedulable},
			target:   nodeTarget,
			mutate:   func(m *clusterModel) { m.mutateNode("node-a", func(n *health.NodeSignal) { n.Unschedulable = true }) },
			wantSaid: "already cordoned",
		},
		{
			name:     "revisionexists holds while the revision survives",
			pc:       remediate.Precondition{Kind: remediate.PreconditionRevisionExists, Expect: "4"},
			target:   depTarget,
			wantHeld: true,
		},
		{
			name:     "revisionexists fails once the revision is pruned",
			pc:       remediate.Precondition{Kind: remediate.PreconditionRevisionExists, Expect: "4"},
			target:   depTarget,
			mutate:   func(m *clusterModel) { m.replicaSets = nil },
			wantSaid: "no longer exists",
		},
		{
			name:     "revisionexists fails closed on a non-numeric expectation",
			pc:       remediate.Precondition{Kind: remediate.PreconditionRevisionExists, Expect: "latest"},
			target:   depTarget,
			wantSaid: "is not a number",
		},
		{
			name:     "revisionexists fails closed for a non-deployment target",
			pc:       remediate.Precondition{Kind: remediate.PreconditionRevisionExists, Expect: "4"},
			target:   nodeTarget,
			wantSaid: "only applies to a deployment",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := fullModel()
			if tc.mutate != nil {
				tc.mutate(model)
			}
			idx := newClusterIndex(model.snapshot())

			held, observed := checkPrecondition(idx, tc.pc, tc.target)
			if held != tc.wantHeld {
				t.Fatalf("held = %t, want %t (observed: %s)", held, tc.wantHeld, observed)
			}
			if observed == "" {
				t.Fatal("the check said nothing about what it saw; both branches must explain themselves")
			}
			if tc.wantSaid != "" && !strings.Contains(observed, tc.wantSaid) {
				t.Fatalf("observed %q, which does not mention %q", observed, tc.wantSaid)
			}
		})
	}
}

// TestCapturePreStateRefusesAnUnknownKind proves MaKlaude will not change an object
// whose prior state it cannot describe.
func TestCapturePreStateRefusesAnUnknownKind(t *testing.T) {
	idx := newClusterIndex(newClusterModel().withNode("node-a").snapshot())

	if _, err := capturePreState(idx, remediate.Target{Cluster: testCluster, Kind: "persistentvolumeclaim", Namespace: "shop", Name: "data"}); err == nil {
		t.Fatal("a pre-state was captured for a kind with no capture rule")
	}
	if _, err := capturePreState(idx, remediate.Target{Cluster: testCluster, Kind: "node", Name: "node-b"}); err == nil {
		t.Fatal("a pre-state was captured for an object the snapshot never saw")
	}
}

// declaredConstants reads the string constants of a named type out of the remediate
// package's source. Parsing the declaration site — rather than listing the values
// here — is what makes the exhaustiveness tests fail when the catalog grows.
func declaredConstants(t *testing.T, typeName string) []string {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, remediatePackageDir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", remediatePackageDir, err)
	}

	var values []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					ident, ok := vs.Type.(*ast.Ident)
					if !ok || ident.Name != typeName {
						continue
					}
					for _, value := range vs.Values {
						lit, ok := value.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						unquoted, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquoting %s: %v", lit.Value, err)
						}
						values = append(values, unquoted)
					}
				}
			}
		}
	}
	sort.Strings(values)
	return values
}

// contains reports whether a sorted slice holds a value.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
