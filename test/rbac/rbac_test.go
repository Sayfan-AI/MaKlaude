// Package rbac_test guards MaKlaude's shipped RBAC manifests against drift.
//
// The e2e job already proves these grants behave correctly against a live
// apiserver (`kubectl auth can-i` for both identities, in .github/workflows/e2e.yml).
// This suite exists because that proof runs only on the e2e job and only where a
// kind cluster is available, while the property it protects — that MaKlaude's
// observation identity has no mutating verb — is the project's foundational
// safety claim and should fail a PR in seconds, not minutes, and without Docker.
//
// The two layers check different things and both are wanted: `can-i` answers
// "what does this cluster actually permit", these tests answer "did anyone widen
// a manifest". A rule added to maklaude-readonly would keep every existing
// `assert_no` in the workflow passing unless someone also remembered to write a
// new one; here it fails as a matter of course, because the assertion is over the
// SET of verbs granted rather than over a list of verbs someone thought to deny.
package rbac_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// bundleDir is the read-only bundle; writeDir is the optional write delta.
const (
	bundleDir = "../../deploy/rbac"
	writeDir  = "../../deploy/rbac/write"
)

// Identities MaKlaude authenticates as. The separation between them is the
// property most of this file is about.
const (
	observationSA = "maklaude"
	executorSA    = "maklaude-executor"
	saNamespace   = "maklaude"
)

// mutatingVerbs is every RBAC verb that can change cluster state, plus the three
// privilege-escalation verbs (bind, escalate, impersonate) which cannot mutate an
// object directly but can hand the holder a role that does.
//
// It is written out rather than derived as "not get/list/watch" so that the
// wildcard "*" is caught explicitly and a future read-only verb (Kubernetes has
// added subresource-shaped reads before) does not silently register as mutating.
var mutatingVerbs = map[string]bool{
	"create":           true,
	"update":           true,
	"patch":            true,
	"delete":           true,
	"deletecollection": true,
	"bind":             true,
	"escalate":         true,
	"impersonate":      true,
	"*":                true,
}

// grant is one (apiGroup, resource, verb) triple, flattened out of a PolicyRule
// so a whole role can be compared as a set.
type grant struct {
	group    string
	resource string
	verb     string
}

func (g grant) String() string {
	group := g.group
	if group == "" {
		group = "core"
	}
	return g.verb + " " + g.resource + "." + group
}

// grantsOf flattens a ClusterRole's rules into the set of triples it confers.
func grantsOf(role *rbacv1.ClusterRole) map[grant]bool {
	out := map[grant]bool{}
	for _, rule := range role.Rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					out[grant{group: group, resource: resource, verb: verb}] = true
				}
			}
		}
	}
	return out
}

// loadManifests reads every YAML document in dir and returns them keyed by
// "Kind/name". Reading the directory (rather than naming each file) is what makes
// a manifest added later subject to these assertions automatically — the same
// membership-over-members shape the dev-system guards use.
func loadManifests(t *testing.T, dir string) map[string][]byte {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	out := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		if entry.Name() == "kustomization.yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}

		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			t.Fatalf("%s/%s has no kind or no metadata.name", dir, entry.Name())
		}
		out[meta.Kind+"/"+meta.Metadata.Name] = raw
	}
	return out
}

func clusterRole(t *testing.T, docs map[string][]byte, name string) *rbacv1.ClusterRole {
	t.Helper()
	raw, ok := docs["ClusterRole/"+name]
	if !ok {
		t.Fatalf("no ClusterRole named %q in the bundle", name)
	}
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatalf("parsing ClusterRole %s: %v", name, err)
	}
	return &role
}

func clusterRoleBinding(t *testing.T, docs map[string][]byte, name string) *rbacv1.ClusterRoleBinding {
	t.Helper()
	raw, ok := docs["ClusterRoleBinding/"+name]
	if !ok {
		t.Fatalf("no ClusterRoleBinding named %q in the bundle", name)
	}
	var binding rbacv1.ClusterRoleBinding
	if err := yaml.Unmarshal(raw, &binding); err != nil {
		t.Fatalf("parsing ClusterRoleBinding %s: %v", name, err)
	}
	return &binding
}

// TestObservationRoleGrantsNoMutatingVerb is the M1 no-writes promise expressed
// over the manifest: whatever maklaude-readonly grants, none of it can change a
// cluster. Now that a write role ships in the same tree, this is also what makes
// "the read-only path is unchanged" checkable rather than asserted in prose.
func TestObservationRoleGrantsNoMutatingVerb(t *testing.T) {
	role := clusterRole(t, loadManifests(t, bundleDir), "maklaude-readonly")

	var offenders []string
	for g := range grantsOf(role) {
		if mutatingVerbs[g.verb] {
			offenders = append(offenders, g.String())
		}
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("maklaude-readonly grants mutating verbs, which breaks MaKlaude's foundational no-writes promise: %s",
			strings.Join(offenders, ", "))
	}
}

// TestWriteRoleGrantsExactlyTheExecutorCatalog pins the write role to the three
// primitives in internal/kube/executor.go. Both directions matter: a missing
// grant means an approved action fails at the API server with a confusing
// Forbidden, and an extra grant means MaKlaude holds authority no code path
// exercises — which is precisely the authority that gets abused if a later bug
// finds it.
func TestWriteRoleGrantsExactlyTheExecutorCatalog(t *testing.T) {
	role := clusterRole(t, loadManifests(t, writeDir), "maklaude-write")

	// One entry per Executor method:
	//   PatchDeployment -> patch deployments.apps
	//   PatchNode       -> patch nodes.core
	//   DeletePod       -> delete pods.core
	want := map[grant]bool{
		{group: "apps", resource: "deployments", verb: "patch"}: true,
		{group: "", resource: "nodes", verb: "patch"}:           true,
		{group: "", resource: "pods", verb: "delete"}:           true,
	}
	got := grantsOf(role)

	var missing, extra []string
	for g := range want {
		if !got[g] {
			missing = append(missing, g.String())
		}
	}
	for g := range got {
		if !want[g] {
			extra = append(extra, g.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("maklaude-write is missing grants the executor needs: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("maklaude-write grants authority no Executor method uses: %s\n"+
			"Add the grant only alongside the executor primitive that issues it, and update this test's catalog in the same change.",
			strings.Join(extra, ", "))
	}
}

// TestWriteRoleExcludesTheDangerousNeighbours names the verbs an over-broad
// version of this role would most plausibly acquire, and asserts each is absent.
//
// This overlaps TestWriteRoleGrantsExactlyTheExecutorCatalog on purpose. That
// test fails on ANY extra grant but says only "extra"; this one fails with the
// reason the specific extra is dangerous, so whoever added deletecollection
// learns why it is excluded rather than just that a list disagreed with them.
func TestWriteRoleExcludesTheDangerousNeighbours(t *testing.T) {
	got := grantsOf(clusterRole(t, loadManifests(t, writeDir), "maklaude-write"))

	forbidden := []struct {
		g      grant
		reason string
	}{
		{grant{"", "pods", "deletecollection"}, "one request could remove every pod matching a selector; the executor's WriteScope pins an exact object path and cannot express it"},
		{grant{"apps", "deployments", "deletecollection"}, "bulk deletion of workloads is an outage, not a remediation"},
		{grant{"apps", "deployments", "delete"}, "nothing in the executor catalog deletes a workload"},
		{grant{"", "nodes", "delete"}, "nothing in the executor catalog deletes a node"},
		{grant{"apps", "deployments", "update"}, "update replaces a whole spec; the executor only ever patches"},
		{grant{"", "nodes", "update"}, "update replaces a whole spec; the executor only ever patches"},
		{grant{"", "pods", "patch"}, "the pod primitive is delete-only"},
		{grant{"", "pods", "create"}, "the executor never creates anything"},
		{grant{"", "pods/eviction", "create"}, "cordoning is not draining; eviction is outside the catalog"},
		{grant{"", "secrets", "get"}, "the write role adds no read of any kind, least of all secrets"},
		{grant{"rbac.authorization.k8s.io", "clusterroles", "escalate"}, "would let this identity grant itself everything above that is missing"},
		{grant{"rbac.authorization.k8s.io", "clusterrolebindings", "bind"}, "would let this identity grant itself everything above that is missing"},
		{grant{"*", "*", "*"}, "a wildcard rule defeats the entire point of a scoped write path"},
	}

	for _, f := range forbidden {
		if got[f.g] {
			t.Errorf("maklaude-write grants %s — %s", f.g, f.reason)
		}
	}
}

// TestNoBindingGivesTheObservationIdentityWriteAccess is the property that makes
// the write bundle safe to install: it is additive to a SEPARATE identity, so an
// operator who applies it has not widened the account every read in MaKlaude uses.
//
// It scans every ClusterRoleBinding in BOTH directories rather than checking the
// two bindings that exist today, so a third binding added later is covered without
// anyone remembering to extend this.
func TestNoBindingGivesTheObservationIdentityWriteAccess(t *testing.T) {
	writeDocs := loadManifests(t, writeDir)

	// Roles that carry a mutating verb, gathered from both bundles.
	mutatingRoles := map[string]bool{}
	for _, dir := range []string{bundleDir, writeDir} {
		for key, raw := range loadManifests(t, dir) {
			if !strings.HasPrefix(key, "ClusterRole/") {
				continue
			}
			var role rbacv1.ClusterRole
			if err := yaml.Unmarshal(raw, &role); err != nil {
				t.Fatalf("parsing %s: %v", key, err)
			}
			for g := range grantsOf(&role) {
				if mutatingVerbs[g.verb] {
					mutatingRoles[role.Name] = true
				}
			}
		}
	}
	if !mutatingRoles["maklaude-write"] {
		t.Fatal("maklaude-write carries no mutating verb — this test would pass vacuously; the write role or this scan is wrong")
	}

	for _, dir := range []string{bundleDir, writeDir} {
		for key := range loadManifests(t, dir) {
			if !strings.HasPrefix(key, "ClusterRoleBinding/") {
				continue
			}
			binding := clusterRoleBinding(t, loadManifests(t, dir), strings.TrimPrefix(key, "ClusterRoleBinding/"))
			if !mutatingRoles[binding.RoleRef.Name] {
				continue
			}
			for _, subject := range binding.Subjects {
				if subject.Kind == "ServiceAccount" && subject.Name == observationSA && subject.Namespace == saNamespace {
					t.Errorf("ClusterRoleBinding %s binds the mutating role %s to the OBSERVATION identity %s/%s; "+
						"write access belongs to %s alone",
						binding.Name, binding.RoleRef.Name, saNamespace, observationSA, executorSA)
				}
			}
		}
	}

	// And the executor identity must actually be the one holding it, or the
	// bundle is inert and the e2e's assert_yes checks would be the only thing
	// noticing.
	writeBinding := clusterRoleBinding(t, writeDocs, "maklaude-write")
	var bound bool
	for _, subject := range writeBinding.Subjects {
		if subject.Kind == "ServiceAccount" && subject.Name == executorSA && subject.Namespace == saNamespace {
			bound = true
		}
	}
	if !bound {
		t.Errorf("ClusterRoleBinding maklaude-write does not bind %s/%s; the write bundle grants nothing",
			saNamespace, executorSA)
	}
}

// TestBaseBundleDoesNotIncludeTheWriteDelta keeps `kubectl apply -k deploy/rbac`
// meaning "read-only, provably".
//
// The quickstart, docs/rbac.md, docs/no-writes.md and the e2e job all run that
// exact command and every one of them describes the result as an identity that
// cannot write. A one-line addition to the base kustomization would falsify all
// four at once, and nothing else in the repo would notice — the e2e's read-only
// assert_no checks are on the maklaude SA, which this bundle keeps clean either
// way, since the write bundle binds a different account.
func TestBaseBundleDoesNotIncludeTheWriteDelta(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading base kustomization: %v", err)
	}

	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatalf("parsing base kustomization: %v", err)
	}

	for _, resource := range kustomization.Resources {
		if strings.Contains(resource, "write") {
			t.Fatalf("the base RBAC bundle references %q; `kubectl apply -k deploy/rbac` must stay read-only, "+
				"and the write delta must be opted into with `kubectl apply -k deploy/rbac/write`", resource)
		}
	}
}
