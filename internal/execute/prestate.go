package execute

import (
	"fmt"
	"strconv"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The pre-state field names. They are constants because a rollback reads back what
// a capture wrote, and a rollback that silently finds nothing because one side
// spelled the key differently would restore nothing while reporting success.
const (
	fieldUnschedulable     = "unschedulable"
	fieldReady             = "ready"
	fieldPhase             = "phase"
	fieldNode              = "node"
	fieldController        = "controller"
	fieldDesiredReplicas   = "desiredReplicas"
	fieldReadyReplicas     = "readyReplicas"
	fieldUpdatedReplicas   = "updatedReplicas"
	fieldAvailableReplicas = "availableReplicas"
	fieldMaxRevision       = "maxRevision"
	fieldCurrentReplicaSet = "currentReplicaSet"
)

// capturePreState records what the target looked like immediately before the
// action, from the snapshot the preconditions were judged against.
//
// It fails — rather than returning an empty record — for a target kind it has no
// capture rule for. That is the same fail-closed direction as an unknown
// precondition, and it enforces a rule worth stating plainly: MaKlaude does not
// mutate an object it cannot describe the prior state of. A capture that quietly
// returned nothing would leave a rollback with nothing to restore and an audit
// trail that cannot say what was changed, which is precisely the situation this
// milestone exists to prevent.
//
// The fields are per-kind and in a fixed order, so two captures of the same
// unchanged object render identically.
func capturePreState(idx *clusterIndex, t remediate.Target) (PreState, error) {
	pre := PreState{
		Kind:       t.Kind,
		ObservedAt: idx.snapshot.CollectedAt,
	}

	switch t.Kind {
	case kindNode:
		node, ok := idx.node(t.Name)
		if !ok {
			return PreState{}, fmt.Errorf("node %q is not in the snapshot", t.Name)
		}
		pre.ResourceVersion = node.ResourceVersion
		pre.Fields = []PreStateField{
			{Name: fieldUnschedulable, Value: strconv.FormatBool(node.Unschedulable)},
			{Name: fieldReady, Value: strconv.FormatBool(node.Ready)},
		}

	case kindDeployment:
		dep, ok := idx.deployment(t.Namespace, t.Name)
		if !ok {
			return PreState{}, fmt.Errorf("deployment %s/%s is not in the snapshot", t.Namespace, t.Name)
		}
		pre.ResourceVersion = dep.ResourceVersion
		// The highest surviving revision is recorded because it is the only evidence a
		// later read has that a rollout actually STARTED: the replica counts describe the
		// previous rollout until the deployment controller catches up, so convergence is
		// judged against this number rather than against the counts alone. See
		// [rolloutRestartConverged].
		//
		// The current ReplicaSet's NAME is recorded alongside it because the number does
		// not survive its own restoration: rolling back to a revision re-uses that
		// revision's ReplicaSet and re-annotates it with the next number, so a rollback's
		// own rollback has to identify "the template that was running" by object rather
		// than by revision. See [clusterIndex.currentReplicaSet].
		current, ok := idx.currentReplicaSet(t.Namespace, t.Name)
		if !ok {
			// Recorded as a word rather than as an empty value: a reader of the trail must
			// be able to tell "no ReplicaSet was visible" from "this field was never
			// populated", and the rollback path refuses on it rather than comparing a name
			// against nothing. See [rollForwardSatisfied].
			current = noReplicaSet
		}
		pre.Fields = []PreStateField{
			{Name: fieldDesiredReplicas, Value: strconv.FormatInt(int64(dep.DesiredReplicas), 10)},
			{Name: fieldReadyReplicas, Value: strconv.FormatInt(int64(dep.ReadyReplicas), 10)},
			{Name: fieldUpdatedReplicas, Value: strconv.FormatInt(int64(dep.UpdatedReplicas), 10)},
			{Name: fieldAvailableReplicas, Value: strconv.FormatInt(int64(dep.AvailableReplicas), 10)},
			{Name: fieldMaxRevision, Value: strconv.FormatInt(maxRevision(idx.revisions(t.Namespace, t.Name)), 10)},
			{Name: fieldCurrentReplicaSet, Value: current},
		}

	case kindPod:
		pod, ok := idx.pod(t.Namespace, t.Name)
		if !ok {
			return PreState{}, fmt.Errorf("pod %s/%s is not in the snapshot", t.Namespace, t.Name)
		}
		pre.ResourceVersion = pod.ResourceVersion
		// The controller is recorded even though deleting a pod cannot be undone,
		// because it is what a human needs in order to check that the replacement they
		// are now looking at came from the same place the deleted one did.
		controller := "none"
		for i := range pod.Owners {
			if pod.Owners[i].Controller {
				controller = pod.Owners[i].Kind + "/" + pod.Owners[i].Name
				break
			}
		}
		pre.Fields = []PreStateField{
			{Name: fieldPhase, Value: pod.Phase},
			{Name: fieldNode, Value: pod.Node},
			{Name: fieldController, Value: controller},
		}

	default:
		return PreState{}, fmt.Errorf("no pre-state capture rule for kind %q", t.Kind)
	}

	pre.Captured = true
	return pre, nil
}
