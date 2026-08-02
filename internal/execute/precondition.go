package execute

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// preconditionCheck evaluates one [remediate.PreconditionKind] against the cluster
// as it exists now, returning whether it holds and a plain-language statement of
// what was actually seen. The statement is produced on BOTH branches: a human
// reading the trail needs "still crashlooping" as much as "no longer crashlooping".
type preconditionCheck func(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string)

// preconditionChecks is the complete set of conditions this package knows how to
// re-verify. It is a map rather than a switch statement so that the set of
// SUPPORTED kinds is a value the process can read — which is what lets
// TestEveryPreconditionKindHasACheck compare it against the kinds the remediate
// package actually declares, and fail the build when a new one appears without an
// implementation here.
//
// A kind absent from this map fails closed: [checkPrecondition] refuses rather than
// assuming an unrecognized condition holds. That direction matters more than it
// looks. Preconditions exist to stop MaKlaude acting on a world that has moved, so
// a kind nobody implemented is not a check that is missing — it is a check that
// would silently always pass, on exactly the actions someone cared enough about to
// add a new condition for.
var preconditionChecks = map[remediate.PreconditionKind]preconditionCheck{
	remediate.PreconditionUnchanged:        checkUnchanged,
	remediate.PreconditionPodCrashLooping:  checkPodCrashLooping,
	remediate.PreconditionPodFailed:        checkPodFailed,
	remediate.PreconditionPodHasController: checkPodHasController,
	remediate.PreconditionNodeNotReady:     checkNodeNotReady,
	remediate.PreconditionNodeSchedulable:  checkNodeSchedulable,
	remediate.PreconditionRevisionExists:   checkRevisionExists,
}

// recheckPreconditions re-evaluates a set of preconditions against a live snapshot,
// in the order they were given.
//
// It evaluates ALL of them rather than stopping at the first failure. The cost is a
// few map lookups against an index that is already built, and the benefit is that
// the refusal a human reads names everything that moved rather than only whichever
// check happened to be listed first — which is the difference between "the pod
// recovered" and "the pod recovered AND the deployment was edited".
//
// An empty set is refused by the caller rather than treated as unconditionally
// safe; see [Runner.Execute].
func recheckPreconditions(idx *clusterIndex, conditions []remediate.Precondition, target remediate.Target) []PreconditionResult {
	out := make([]PreconditionResult, 0, len(conditions))
	for _, pc := range conditions {
		held, observed := checkPrecondition(idx, pc, target)
		out = append(out, PreconditionResult{
			Kind:        pc.Kind,
			Expect:      pc.Expect,
			Description: pc.Description,
			Held:        held,
			Observed:    observed,
		})
	}
	return out
}

// checkPrecondition dispatches one precondition to its check, failing closed on any
// kind this package does not implement.
func checkPrecondition(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	check, ok := preconditionChecks[pc.Kind]
	if !ok {
		return false, fmt.Sprintf("precondition kind %q has no check in the execution layer, so it cannot be verified; refusing rather than assuming it holds", pc.Kind)
	}
	return check(idx, pc, t)
}

// allHeld reports whether every re-evaluated precondition still holds.
func allHeld(results []PreconditionResult) bool {
	for _, r := range results {
		if !r.Held {
			return false
		}
	}
	return true
}

// checkUnchanged verifies the optimistic-concurrency token: the target must still
// be at the resourceVersion the proposal was computed against.
//
// This duplicates a check the API server will also perform, and both are wanted. The
// server's is authoritative and cannot be skipped; this one runs BEFORE the request
// and is what makes a stale approval cost zero mutating requests, produce a report
// that names the drift in plain language, and never appear in an apiserver audit log
// as an attempted write.
func checkUnchanged(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	if strings.TrimSpace(pc.Expect) == "" {
		return false, "the precondition names no expected resourceVersion, so nothing can be compared"
	}
	rv, ok := idx.resourceVersion(t)
	if !ok {
		return false, fmt.Sprintf("%s is no longer present in the cluster", t.String())
	}
	if rv != pc.Expect {
		return false, fmt.Sprintf("%s is at resourceVersion %q, expected %q — it changed after the proposal was computed", t.String(), rv, pc.Expect)
	}
	return true, fmt.Sprintf("%s is still at resourceVersion %q", t.String(), rv)
}

// checkPodCrashLooping verifies the pod is still crashlooping, reading the
// collector's oscillation-robust per-container flag rather than re-deriving the
// judgment from an instantaneous state. See [Observer] for why re-deriving it here
// would be a bug rather than a duplication.
func checkPodCrashLooping(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	namespace, name, ok := podRef(pc.Expect, t)
	if !ok {
		return false, "the precondition does not name a pod to check"
	}
	pod, ok := idx.pod(namespace, name)
	if !ok {
		return false, fmt.Sprintf("pod %s/%s is no longer present in the cluster", namespace, name)
	}
	for i := range pod.Containers {
		if pod.Containers[i].CrashLooping {
			return true, fmt.Sprintf("pod %s/%s container %q is still crashlooping", namespace, name, pod.Containers[i].Name)
		}
	}
	return false, fmt.Sprintf("pod %s/%s is no longer crashlooping — the problem this action addresses has resolved", namespace, name)
}

// checkPodFailed verifies the pod is still in a terminal failed or evicted state.
// Deleting a pod that has recovered would destroy a working one, which is the exact
// mistake this condition exists to prevent.
func checkPodFailed(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	namespace, name, ok := podRef(pc.Expect, t)
	if !ok {
		return false, "the precondition does not name a pod to check"
	}
	pod, ok := idx.pod(namespace, name)
	if !ok {
		return false, fmt.Sprintf("pod %s/%s is no longer present in the cluster — there is nothing left to delete", namespace, name)
	}
	if pod.Failed {
		return true, fmt.Sprintf("pod %s/%s is still in phase Failed", namespace, name)
	}
	if pod.Reason == evictedReason {
		return true, fmt.Sprintf("pod %s/%s is still Evicted", namespace, name)
	}
	return false, fmt.Sprintf("pod %s/%s is in phase %q and is neither Failed nor Evicted — it must not be deleted", namespace, name, pod.Phase)
}

// evictedReason is the pod-level reason the kubelet records when it evicts a pod
// under resource pressure. Such a pod is finished and kept only as a tombstone, so
// it counts as failed for the purposes of [checkPodFailed] — matching the rule the
// remediate layer used when it proposed the deletion.
const evictedReason = "Evicted"

// checkPodHasController verifies the pod is still owned by a controller that will
// recreate it, and — when the proposal named a specific one — that it is still the
// SAME controller.
//
// The identity comparison is the point rather than a refinement. Without a
// controller the deletion is irreversible rather than recreated-by-controller: a
// materially different action than the one a human approved. With a DIFFERENT
// controller it is a different action again — the name was reused by another
// workload — and the approval covered neither.
func checkPodHasController(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	namespace, name, ok := podRef("", t)
	if !ok {
		return false, "the precondition does not name a pod to check"
	}
	pod, ok := idx.pod(namespace, name)
	if !ok {
		return false, fmt.Sprintf("pod %s/%s is no longer present in the cluster", namespace, name)
	}
	for i := range pod.Owners {
		owner := pod.Owners[i]
		if !owner.Controller {
			continue
		}
		got := owner.Kind + "/" + owner.Name
		if want := strings.TrimSpace(pc.Expect); want != "" && got != want {
			return false, fmt.Sprintf("pod %s/%s is now controlled by %s, not %s — this is not the pod the approval covered", namespace, name, got, want)
		}
		return true, fmt.Sprintf("pod %s/%s is still controlled by %s, which will recreate it", namespace, name, got)
	}
	return false, fmt.Sprintf("pod %s/%s has no controlling owner — deleting it would be permanent, which is not the action that was approved", namespace, name)
}

// checkNodeNotReady verifies the node is still NotReady. Cordoning a node that has
// recovered would needlessly remove capacity from a healthy cluster.
func checkNodeNotReady(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	name, ok := nodeRef(pc.Expect, t)
	if !ok {
		return false, "the precondition does not name a node to check"
	}
	node, ok := idx.node(name)
	if !ok {
		return false, fmt.Sprintf("node %q is no longer present in the cluster", name)
	}
	if node.Ready {
		return false, fmt.Sprintf("node %q is Ready again — its capacity must not be removed", name)
	}
	return true, fmt.Sprintf("node %q is still NotReady", name)
}

// checkNodeSchedulable verifies the node is not already cordoned — by a human, or by
// an earlier run of this same proposal. A node that is already unschedulable needs
// no cordon, and sending one anyway would put a redundant write in the audit log.
func checkNodeSchedulable(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	name, ok := nodeRef(pc.Expect, t)
	if !ok {
		return false, "the precondition does not name a node to check"
	}
	node, ok := idx.node(name)
	if !ok {
		return false, fmt.Sprintf("node %q is no longer present in the cluster", name)
	}
	if node.Unschedulable {
		return false, fmt.Sprintf("node %q is already cordoned — there is nothing left to do", name)
	}
	return true, fmt.Sprintf("node %q is still schedulable", name)
}

// checkRevisionExists verifies the rollback target revision still exists among the
// Deployment's ReplicaSets. Kubernetes prunes old ReplicaSets past a Deployment's
// revision history limit, so a revision that existed when the proposal was computed
// can be gone by the time a human approves it.
func checkRevisionExists(idx *clusterIndex, pc remediate.Precondition, t remediate.Target) (bool, string) {
	if t.Kind != kindDeployment {
		return false, fmt.Sprintf("a revision precondition only applies to a deployment, not to %s", t.String())
	}
	want, err := strconv.ParseInt(strings.TrimSpace(pc.Expect), 10, 64)
	if err != nil {
		return false, fmt.Sprintf("the precondition's expected revision %q is not a number", pc.Expect)
	}
	revisions := idx.revisions(t.Namespace, t.Name)
	for _, rev := range revisions {
		if rev == want {
			return true, fmt.Sprintf("revision %d of deployment %s/%s still exists", want, t.Namespace, t.Name)
		}
	}
	return false, fmt.Sprintf("revision %d of deployment %s/%s no longer exists (surviving revisions: %s)",
		want, t.Namespace, t.Name, renderRevisions(revisions))
}

// renderRevisions formats a revision list for a refusal notice, saying "none"
// explicitly rather than rendering an empty bracket a reader has to interpret.
func renderRevisions(revisions []int64) string {
	if len(revisions) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(revisions))
	for _, rev := range revisions {
		parts = append(parts, strconv.FormatInt(rev, 10))
	}
	return strings.Join(parts, ", ")
}

// podRef resolves which pod a precondition is about: the one its Expect names as
// "namespace/name", or the target itself when Expect is empty and the target is a
// pod. It reports false when neither applies, so a precondition pointed at nothing
// fails closed rather than silently checking the wrong object.
func podRef(expect string, t remediate.Target) (string, string, bool) {
	if ref := strings.TrimSpace(expect); ref != "" {
		namespace, name, found := strings.Cut(ref, "/")
		if !found || namespace == "" || name == "" {
			return "", "", false
		}
		return namespace, name, true
	}
	if t.Kind != kindPod || t.Namespace == "" || t.Name == "" {
		return "", "", false
	}
	return t.Namespace, t.Name, true
}

// nodeRef resolves which node a precondition is about, with the same
// Expect-then-target rule and the same fail-closed default as [podRef].
func nodeRef(expect string, t remediate.Target) (string, bool) {
	if name := strings.TrimSpace(expect); name != "" {
		return name, true
	}
	if t.Kind != kindNode || t.Name == "" {
		return "", false
	}
	return t.Name, true
}

// The target kinds this package understands, in the lowercase stable form
// [remediate.Target] and [detect.Object] use. They are constants rather than
// literals because they are compared in three separate concerns — precondition
// checks, pre-state capture, and convergence — and a typo in any one of them would
// fail closed silently in a different place each time.
const (
	kindDeployment = "deployment"
	kindPod        = "pod"
	kindNode       = "node"
)

// clusterIndex is the read-only lookup this package builds once per snapshot, so a
// precondition sweep, a pre-state capture, and a convergence check all read the same
// view without rescanning slices.
//
// It holds no clock and no client and is never mutated after construction, which is
// what makes "the pre-state is the state the preconditions were judged against" a
// property of the code rather than a comment.
type clusterIndex struct {
	// snapshot is the view this index was built from, kept so callers can carry its
	// collection time into a pre-state record.
	snapshot health.Snapshot

	pods        map[string]health.PodSignal
	nodes       map[string]health.NodeSignal
	deployments map[string]health.DeploymentSignal

	// replicaSetsByDeployment maps a deployment's namespace/name to the ReplicaSets it
	// owns, resolved through ownerReferences rather than the "<deployment>-<hash>"
	// naming convention — a mutating action must not guess its target from a name.
	//
	// The ReplicaSet's NAME is kept alongside its revision because a rollback needs an
	// identity that survives a rollout, and a revision number does not: rolling back
	// re-uses the target revision's ReplicaSet and re-annotates it with the next
	// revision number, so "revision 2" ceases to exist the moment it is restored. The
	// object it lived on is still there, under the same name.
	replicaSetsByDeployment map[string][]replicaSetRevision
}

// replicaSetRevision is one of a deployment's ReplicaSets, reduced to the two facts
// this package reasons about: which object it is, and which rollout it represents.
type replicaSetRevision struct {
	name     string
	revision int64
}

// newClusterIndex builds the index over a snapshot. It iterates the snapshot's
// already-sorted slices and never iterates a map to produce output, so it
// introduces no nondeterminism.
func newClusterIndex(snap health.Snapshot) *clusterIndex {
	idx := &clusterIndex{
		snapshot:                snap,
		pods:                    make(map[string]health.PodSignal, len(snap.Pods)),
		nodes:                   make(map[string]health.NodeSignal, len(snap.Nodes)),
		deployments:             make(map[string]health.DeploymentSignal, len(snap.Deployments)),
		replicaSetsByDeployment: make(map[string][]replicaSetRevision, len(snap.Deployments)),
	}
	for i := range snap.Pods {
		idx.pods[objectKey(snap.Pods[i].Namespace, snap.Pods[i].Name)] = snap.Pods[i]
	}
	for i := range snap.Nodes {
		idx.nodes[snap.Nodes[i].Name] = snap.Nodes[i]
	}
	for i := range snap.Deployments {
		idx.deployments[objectKey(snap.Deployments[i].Namespace, snap.Deployments[i].Name)] = snap.Deployments[i]
	}
	for i := range snap.ReplicaSets {
		rs := snap.ReplicaSets[i]
		if rs.Revision <= 0 {
			continue
		}
		for j := range rs.Owners {
			if rs.Owners[j].Kind != "Deployment" {
				continue
			}
			key := objectKey(rs.Namespace, rs.Owners[j].Name)
			idx.replicaSetsByDeployment[key] = append(idx.replicaSetsByDeployment[key],
				replicaSetRevision{name: rs.Name, revision: rs.Revision})
		}
	}
	return idx
}

// pod returns the named pod, if the snapshot saw it.
func (idx *clusterIndex) pod(namespace, name string) (health.PodSignal, bool) {
	pod, ok := idx.pods[objectKey(namespace, name)]
	return pod, ok
}

// node returns the named node, if the snapshot saw it.
func (idx *clusterIndex) node(name string) (health.NodeSignal, bool) {
	node, ok := idx.nodes[name]
	return node, ok
}

// deployment returns the named deployment, if the snapshot saw it.
func (idx *clusterIndex) deployment(namespace, name string) (health.DeploymentSignal, bool) {
	dep, ok := idx.deployments[objectKey(namespace, name)]
	return dep, ok
}

// revisions returns the deployment's surviving ReplicaSet revisions, in the
// snapshot's own stable order.
func (idx *clusterIndex) revisions(namespace, name string) []int64 {
	owned := idx.replicaSetsByDeployment[objectKey(namespace, name)]
	out := make([]int64, 0, len(owned))
	for _, rs := range owned {
		out = append(out, rs.revision)
	}
	return out
}

// currentReplicaSet returns the name of the ReplicaSet carrying the deployment's
// HIGHEST surviving revision — the one whose pod template the deployment is running
// now — and false when the snapshot saw none.
//
// The name is the identity a rollback needs. The Deployment controller creates exactly
// one ReplicaSet per distinct pod template and re-uses it whenever that template comes
// back, so "the current ReplicaSet is the same object as before" and "the pod template
// is the one from before" are the same statement. Comparing revision NUMBERS cannot say
// that: restoring revision 2 re-annotates its ReplicaSet as revision 4, so the number a
// pre-state recorded is gone precisely when the rollback worked.
func (idx *clusterIndex) currentReplicaSet(namespace, name string) (string, bool) {
	var current replicaSetRevision
	for _, rs := range idx.replicaSetsByDeployment[objectKey(namespace, name)] {
		if rs.revision > current.revision {
			current = rs
		}
	}
	return current.name, current.revision > 0
}

// resourceVersion returns the target's current resourceVersion, and false when the
// snapshot does not contain the object — or when its kind is one this package does
// not index, which fails closed for the same reason an unknown precondition does.
func (idx *clusterIndex) resourceVersion(t remediate.Target) (string, bool) {
	switch t.Kind {
	case kindDeployment:
		dep, ok := idx.deployment(t.Namespace, t.Name)
		return dep.ResourceVersion, ok
	case kindPod:
		pod, ok := idx.pod(t.Namespace, t.Name)
		return pod.ResourceVersion, ok
	case kindNode:
		node, ok := idx.node(t.Name)
		return node.ResourceVersion, ok
	default:
		return "", false
	}
}

// objectKey is the namespace/name key objects are indexed under, matching the form
// the layers below use. A cluster-scoped object has an empty namespace, which is
// harmless here because nodes are indexed by name in their own map.
func objectKey(namespace, name string) string { return namespace + "/" + name }
