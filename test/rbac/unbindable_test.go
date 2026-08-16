// The unbindability half of Milestone 6's narrowed no-writes guarantee (task T7,
// issue #196). The done criterion it answers is "the chaos bundle is proven
// unbindable to the other two identities", and the property from the issue is
// stated over all three: not merely that each identity is least-privilege, but
// that no two are ever bound together.
//
// What "unbindable" can and cannot mean is worth saying plainly, because the
// literal reading is not achievable and asserting it anyway would be exactly the
// reworded-sentence failure this gate exists to prevent. Kubernetes has no
// mechanism that makes a Role unbindable: anybody holding `create` on
// rolebindings in maklaude-chaos can bind that Role to any subject they like, and
// a cluster-admin always can. So the provable property is not "no such binding
// can exist"; it is that every route by which one could come to exist is either
// closed or fails this build:
//
//  1. MaKlaude cannot author one itself. No identity it authenticates as holds
//     any authority over rbac.authorization.k8s.io, and none may impersonate — so
//     no code path, and no future bug in one, can widen the access model at
//     runtime. That is TestNoIdentityCanAuthorABinding, and it is the half that
//     makes the word "unbindable" mean something MaKlaude controls.
//  2. Nothing MaKlaude ships binds two identities to one mutating role, in either
//     direction, through any subject shape. That is
//     TestEveryMutatingRoleHasExactlyOnePermittedHolder.
//
// What remains after both is a person writing a binding by hand against a live
// cluster. No test in a repository can prevent that, and docs/rbac.md says so
// rather than implying the manifests forbid it.
//
// Both tests overlap existing ones on purpose, the way
// TestChaosRoleExcludesTheWorkloadItBreaks overlaps the catalog test: those check
// the two hand-picked escalation triples somebody thought to deny, and the
// bindings that exist today under the one subject shape they use. These check the
// CLASS and the SET, so a rule or a binding nobody anticipated fails as a matter
// of course.
package rbac_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// rbacGroup holds Roles, RoleBindings, ClusterRoles and ClusterRoleBindings —
// every object through which authority is conferred. Any mutating verb on any
// resource in it is the ability to rewrite the access model, which is why the
// check below is over the group rather than over a list of its resources: a verb
// on `roles` is as fatal as one on `rolebindings` (write a role, then bind it),
// and a future resource in this group is covered without anyone extending a list.
const rbacGroup = "rbac.authorization.k8s.io"

// roleDoc is one Role or ClusterRole with the bundle it shipped in, so a failure
// can name the file an operator has to look at.
type roleDoc struct {
	dir   string
	kind  string
	name  string
	rules []rbacv1.PolicyRule
}

// bindingDoc is one RoleBinding or ClusterRoleBinding, flattened to the two
// things a separation check needs: which role it confers and to whom.
type bindingDoc struct {
	dir      string
	kind     string
	name     string
	roleRef  string
	subjects []rbacv1.Subject
}

// allBundleDirs is every directory these assertions cover: the read-only base and
// both optional deltas. Iterating the three (rather than naming manifests) is
// what makes a role or binding added later subject to these tests without anyone
// remembering to extend them.
func allBundleDirs() []string { return []string{bundleDir, writeDir, chaosDir} }

// allRoles reads every Role and ClusterRole across the three bundles.
func allRoles(t *testing.T) []roleDoc {
	t.Helper()

	var out []roleDoc
	for _, dir := range allBundleDirs() {
		for key, raw := range loadManifests(t, dir) {
			kind, name, _ := strings.Cut(key, "/")
			switch kind {
			case "Role":
				var role rbacv1.Role
				if err := yaml.Unmarshal(raw, &role); err != nil {
					t.Fatalf("parsing %s/%s: %v", dir, key, err)
				}
				out = append(out, roleDoc{dir: dir, kind: kind, name: name, rules: role.Rules})
			case "ClusterRole":
				var role rbacv1.ClusterRole
				if err := yaml.Unmarshal(raw, &role); err != nil {
					t.Fatalf("parsing %s/%s: %v", dir, key, err)
				}
				out = append(out, roleDoc{dir: dir, kind: kind, name: name, rules: role.Rules})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// allBindings reads every RoleBinding and ClusterRoleBinding across the three
// bundles.
func allBindings(t *testing.T) []bindingDoc {
	t.Helper()

	var out []bindingDoc
	for _, dir := range allBundleDirs() {
		docs := loadManifests(t, dir)
		for key := range docs {
			kind, name, _ := strings.Cut(key, "/")
			switch kind {
			case "RoleBinding":
				b := namespacedRoleBinding(t, docs, name)
				out = append(out, bindingDoc{dir: dir, kind: kind, name: name, roleRef: b.RoleRef.Name, subjects: b.Subjects})
			case "ClusterRoleBinding":
				b := clusterRoleBinding(t, docs, name)
				out = append(out, bindingDoc{dir: dir, kind: kind, name: name, roleRef: b.RoleRef.Name, subjects: b.Subjects})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// subjectString renders a subject the way a reader has to think about it — kind
// first, because the kind is the part that decides how many accounts it reaches.
func subjectString(s rbacv1.Subject) string {
	if s.Namespace == "" {
		return fmt.Sprintf("%s %q", s.Kind, s.Name)
	}
	return fmt.Sprintf("%s %s/%s", s.Kind, s.Namespace, s.Name)
}

// TestNoIdentityCanAuthorABinding is what lets this suite use the word
// "unbindable" about a mechanism Kubernetes does not provide: MaKlaude cannot
// write the binding that would collapse its own identity separation.
//
// Two forbidden classes, and they fail differently:
//
//   - Any mutating verb on rbac.authorization.k8s.io. `create rolebindings` is
//     the direct route — bind maklaude-chaos to maklaude-executor and the third
//     identity was pointless. `escalate` and `bind` are the ones RBAC added
//     specifically to gate this, and `patch roles` is the quiet route: leave
//     every binding alone and widen the Role each already confers.
//   - `impersonate`, on anything. This one needs no binding at all: an executor
//     that may impersonate maklaude-chaos performs a chaos write under the chaos
//     identity's authority, and the audit log's answer to "which kind of act was
//     this" — the whole reason three accounts exist — becomes a fabrication.
//
// The check is over the SET of roles in all three bundles, so a fourth role
// shipping later is covered. Contrast TestWriteRoleExcludesTheDangerousNeighbours
// and TestChaosRoleExcludesTheWorkloadItBreaks, which each name two exact triples
// (`escalate clusterroles`, `bind rolebindings`) on one role: those say why a
// specific grant is dangerous, this one cannot be routed around by picking a
// resource or a verb nobody listed.
func TestNoIdentityCanAuthorABinding(t *testing.T) {
	roles := allRoles(t)

	// Vacuity guard. Every assertion below is a loop over roles, so an empty or
	// half-read scan passes while proving nothing — and this file reads
	// directories rather than named files precisely so that it cannot notice a
	// role going missing any other way.
	seen := map[string]bool{}
	for _, role := range roles {
		seen[role.name] = true
	}
	for _, want := range []string{"maklaude-readonly", "maklaude-write", chaosSA, chaosTargetRole} {
		if !seen[want] {
			t.Fatalf("the role scan did not find %q — every assertion here would pass vacuously; the bundle or this scan is wrong (found %v)", want, seen)
		}
	}

	for _, role := range roles {
		for g := range grantsOfRules(role.rules) {
			switch {
			case g.verb == "impersonate":
				t.Errorf("%s/%s %s grants %s — impersonation collapses the three identities without any binding: "+
					"the holder acts with another account's authority, and the audit log attributes the act to the account impersonated",
					role.dir, role.kind, role.name, g)
			case (g.group == rbacGroup || g.group == "*") && mutatingVerbs[g.verb]:
				t.Errorf("%s/%s %s grants %s — authority over %s is the authority to rewrite this access model at runtime, "+
					"which is what makes every other assertion in this suite a statement about the manifests only",
					role.dir, role.kind, role.name, g, rbacGroup)
			}
		}
	}
}

// permittedHolder names the ONE identity allowed to hold each role that carries a
// mutating verb.
//
// It is a declared map rather than a derived rule because "who should hold this"
// is a design decision, not a property of the YAML — and being declared is what
// makes the completeness check below possible: a mutating role absent from this
// map fails the build, so shipping one is a decision somebody has to write down
// here, in the same change.
//
// Only mutating roles appear. maklaude-readonly is deliberately absent, and its
// absence is the reason this test is framed over mutating authority rather than
// over "one role per identity": the read-only role is bound to BOTH the
// observation and the executor identity on purpose
// (deploy/rbac/write/clusterrolebinding-readonly.yaml — an executor must re-read
// a target to condition its action on a resourceVersion, and reusing the role
// verbatim beats a second copy that can drift). Sharing a role that cannot change
// anything is not a separation failure; sharing one that can is the only kind
// there is.
var permittedHolder = map[string]string{
	"maklaude-write": executorSA,
	// The chaos Role and its per-target-namespace companion share the chaos
	// identity's name; the constants are the same two the rest of this suite uses.
	chaosSA:         chaosSA,
	chaosTargetRole: chaosSA,
}

// TestEveryMutatingRoleHasExactlyOnePermittedHolder is the shipped-manifest half
// of unbindability, and it generalizes two existing tests in the three directions
// each of them leaves open.
//
// TestNoBindingGivesTheObservationIdentityWriteAccess asks whether the observation
// identity holds a mutating role; TestChaosGrantReachesNoOtherIdentity asks
// whether the observation or executor identity holds the chaos role. Both look for
// a named account in a ServiceAccount subject, which leaves:
//
//   - The other direction. Neither test would notice a ClusterRoleBinding that
//     binds maklaude-write to maklaude-chaos. The chaos identity acquiring the
//     executor's catalog is the same collapse seen from the other end: a
//     deliberate experiment would then be indistinguishable from an approved
//     remediation in the audit log.
//   - Subject kinds that name no account at all. A subject of `kind: Group, name:
//     system:serviceaccounts:maklaude` confers the role on all three identities
//     at once, and `system:authenticated` on every account in the cluster. Both
//     pass a check that only inspects ServiceAccount subjects. This is not a
//     hypothetical shape — it is the ordinary way an operator grants something to
//     a whole namespace's workloads.
//   - Roles nobody has written yet. Both tests are anchored to role names known
//     today; this one derives the set from the grants, so a fourth mutating role
//     is covered on the commit that adds it.
//
// The rule is an allowlist of exactly one subject per mutating role, which is
// stricter than "not the other two" and needs no judgment: a role that can change
// a cluster has exactly one intended holder, so every other subject — of every
// kind — is a finding.
func TestEveryMutatingRoleHasExactlyOnePermittedHolder(t *testing.T) {
	mutating := map[string]bool{}
	for _, role := range allRoles(t) {
		var verbs []string
		for g := range grantsOfRules(role.rules) {
			if mutatingVerbs[g.verb] {
				mutating[role.name] = true
				verbs = append(verbs, g.String())
			}
		}
		if len(verbs) == 0 {
			continue
		}
		if _, declared := permittedHolder[role.name]; !declared {
			sort.Strings(verbs)
			t.Errorf("%s/%s %s carries mutating grants (%s) and no permitted holder is declared for it; "+
				"add it to permittedHolder in the change that ships it, naming the one identity that may hold it",
				role.dir, role.kind, role.name, strings.Join(verbs, ", "))
		}
	}

	// Vacuity guard, two parts. The first is the same shape the existing bundle
	// tests use — a scan that finds no mutating role would report nothing at all.
	for name := range permittedHolder {
		if !mutating[name] {
			t.Errorf("%s is declared as a mutating role with a single permitted holder, but carries no mutating verb; "+
				"either the role was narrowed and this entry is stale, or the scan is wrong", name)
		}
	}

	// The second is the one that matters more: the loop below only fires on
	// bindings that reference a mutating role, so if a bundle stopped shipping
	// those bindings the separation check would be a no-op AND the bundle would
	// grant nothing. Both existing tests make this point per-role ("the bundle
	// grants nothing"); here it also keeps the assertion honest.
	checked := map[string]int{}
	for _, binding := range allBindings(t) {
		owner, isMutating := permittedHolder[binding.roleRef]
		if !isMutating {
			continue
		}
		checked[binding.roleRef]++

		for _, subject := range binding.subjects {
			if subject.Kind != "ServiceAccount" {
				t.Errorf("%s/%s %s binds the mutating role %s to %s — a Group or User subject confers it on accounts it never names: "+
					"system:serviceaccounts:%s reaches all three identities and system:authenticated reaches every account on the cluster. "+
					"Only ServiceAccount %s/%s may hold %s",
					binding.dir, binding.kind, binding.name, binding.roleRef, subjectString(subject),
					saNamespace, saNamespace, owner, binding.roleRef)
				continue
			}
			if subject.Name != owner || subject.Namespace != saNamespace {
				t.Errorf("%s/%s %s binds the mutating role %s to %s; only ServiceAccount %s/%s may hold it — "+
					"three identities exist so a mutating request says which KIND of act it was, and any account holding two of these roles erases that",
					binding.dir, binding.kind, binding.name, binding.roleRef, subjectString(subject), saNamespace, owner)
			}
		}
	}

	for name, owner := range permittedHolder {
		if checked[name] == 0 {
			t.Errorf("no binding in any bundle references the mutating role %s, so this test examined no subject for it "+
				"and %s/%s holds nothing; the bundle is inert or a binding was dropped",
				name, saNamespace, owner)
		}
	}
}
