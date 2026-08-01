package execute

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// plan is everything this package knows about ONE catalog operation: how to send
// it, how to tell whether it worked, and what undoing it would take.
//
// The three are one value rather than three switch statements deliberately. They
// are the parts most likely to disagree with each other as the catalog grows — an
// operation gains a send path but no convergence check, or a convergence check
// written against a different post-state than the one the send path produces — and
// a single struct per operation means adding one is a single edit with a compiler
// and a test watching it.
type plan struct {
	// rollback classifies what undoing this operation would take, and rollbackNote
	// states it in the same plain language the approval artifact showed the human who
	// approved the action.
	rollback     RollbackKind
	rollbackNote string

	// unsupported, when non-empty, is why MaKlaude will not perform this operation at
	// all, in words a human can act on. A plan carrying it sends nothing, ever.
	unsupported string

	// mutate sends the single mutating request the operation consists of. It is nil
	// exactly when unsupported is set.
	//
	// at is the action's timestamp, supplied by the caller rather than read from a
	// clock in here, so a retried attempt sends a byte-identical request rather than a
	// second, subtly different one.
	mutate func(ctx context.Context, m Mutator, t remediate.Target, at time.Time) (*kube.Outcome, error)

	// converged reports whether the cluster has reached the post-state the action was
	// supposed to produce, and states what was actually seen. It reads the pre-state
	// as well as the live index, because for some operations "did it work?" is a
	// comparison against how things were rather than an absolute condition.
	converged func(idx *clusterIndex, pre PreState, t remediate.Target) (bool, string)

	// undo is the inverse action, set exactly when rollback is
	// [RollbackPerformable].
	undo *undoPlan
}

// undoPlan is the inverse of an operation: what [Runner.Rollback] sends to put the
// target back the way it was.
type undoPlan struct {
	// description states the inverse in plain language, for the report and the trail.
	description string

	// satisfied reports whether the target is ALREADY back at its pre-action state,
	// in which case the rollback sends nothing. A rollback that re-asserts a state
	// someone else has already restored is at best a redundant write in an audit log
	// and at worst a fight with a human doing the same job.
	satisfied func(idx *clusterIndex, pre PreState, t remediate.Target) (bool, string)

	// mutate sends the inverse request, conditioned on the target's CURRENT
	// resourceVersion — which is necessarily not the one the original action used,
	// because the original action changed it.
	mutate func(ctx context.Context, m Mutator, t remediate.Target, resourceVersion string) (*kube.Outcome, error)
}

// operationPlans is the complete set of operations this package can act on. It is a
// map rather than a switch for the same reason [preconditionChecks] is: the set of
// supported operations becomes a value the process can read, so
// TestEveryCatalogOperationHasAPlan can compare it against the operations the
// remediate package actually declares and fail the build when a new one appears
// with no plan here.
//
// An operation absent from this map is refused before anything is sent. That
// mirrors the approval gate's own refusal of an operation with no rollback plan
// (approve's ReasonNoRollbackPlan): extending the catalog must not silently widen
// what MaKlaude will do to a cluster, at either layer.
var operationPlans = map[remediate.Operation]plan{
	remediate.OpRolloutRestart: {
		rollback: RollbackNotRequired,
		rollbackNote: "Nothing to undo: the Deployment's spec is unchanged apart from the restart annotation, " +
			"and the pods it replaced are gone whether or not that annotation is put back. Reverting the annotation " +
			"would trigger a third rollout and restore nothing.",
		mutate: func(ctx context.Context, m Mutator, t remediate.Target, at time.Time) (*kube.Outcome, error) {
			return m.RestartDeploymentRollout(ctx, t.Namespace, t.Name, at.UTC().Format(time.RFC3339), t.ResourceVersion)
		},
		converged: rolloutRestartConverged,
	},

	remediate.OpCordonNode: {
		rollback:     RollbackPerformable,
		rollbackNote: "Uncordon the node to make it schedulable again. Pods already running on it were never touched, so uncordoning restores the prior state exactly.",
		mutate: func(ctx context.Context, m Mutator, t remediate.Target, _ time.Time) (*kube.Outcome, error) {
			return m.CordonNode(ctx, t.Name, t.ResourceVersion)
		},
		converged: cordonConverged,
		undo: &undoPlan{
			description: "uncordon the node",
			satisfied:   uncordonSatisfied,
			mutate: func(ctx context.Context, m Mutator, t remediate.Target, resourceVersion string) (*kube.Outcome, error) {
				return m.PatchNode(ctx, t.Name, []byte(`{"spec":{"unschedulable":false}}`), resourceVersion)
			},
		},
	},

	remediate.OpDeletePod: {
		rollback: RollbackImpossible,
		rollbackNote: "The pod cannot be restored — its name, its identity, and its logs are gone permanently. " +
			"Its controller recreates a replacement automatically, so no rollback is needed to restore function, but nothing restores that pod.",
		mutate: func(ctx context.Context, m Mutator, t remediate.Target, _ time.Time) (*kube.Outcome, error) {
			return m.DeletePod(ctx, t.Namespace, t.Name, t.ResourceVersion)
		},
		converged: deletePodConverged,
	},

	// A revision rollback is planned by the remediate layer and approved by the gate,
	// and this layer refuses to perform it. That is a deliberate, visible gap rather
	// than an oversight.
	//
	// A faithful rollback replaces the Deployment's whole pod template with the target
	// revision's, which requires a JSON-patch `replace` of /spec/template — a primitive
	// the write path does not expose. The primitive it DOES expose is a strategic-merge
	// patch, and strategic merge cannot remove things: applying the old template through
	// it merges containers and environment variables by name, so a container or an env
	// var that the current revision added would survive the "rollback". The result is a
	// Deployment running a template that is neither the old revision nor the new one,
	// reported as a successful rollback.
	//
	// Refusing is the safe half of that choice; making the refusal LOUD is the other
	// half. It surfaces as [FailureRefused] naming the missing primitive, so the gap is
	// something an operator reads rather than something they infer from a rollback that
	// did not work.
	remediate.OpRollbackRevision: {
		rollback:     RollbackPerformable,
		rollbackNote: "Roll forward again to the revision that was current before this action. Both revisions remain in the Deployment's history.",
		unsupported: "the write path exposes no primitive that can express a faithful revision rollback: " +
			"it needs a JSON-patch replace of spec.template, and a strategic-merge patch cannot remove fields, " +
			"so applying the previous template would produce a pod template that is neither revision",
		converged: rolloutRestartConverged,
	},
}

// planFor returns the plan for an operation, and false for one this package has
// never heard of.
func planFor(op remediate.Operation) (plan, bool) {
	p, ok := operationPlans[op]
	return p, ok
}

// rolloutRestartConverged reports whether a Deployment has finished rolling out the
// template the restart stamped.
//
// It requires a NEW revision to exist before it looks at the replica counts, and
// that ordering is the whole check. A rollout restart bumps the pod template, so the
// Deployment's status still describes the PREVIOUS rollout for the moment after the
// patch lands — every replica updated, ready, and available, because the old ones
// still are. Reading the counts alone would therefore report "converged" on the first
// poll, before the deployment controller had so much as noticed, and would go on
// reporting it if the rollout then stalled. A restart creates a ReplicaSet with the
// next revision, so a higher revision than the pre-state saw is the evidence that the
// action actually took effect rather than merely being accepted.
func rolloutRestartConverged(idx *clusterIndex, pre PreState, t remediate.Target) (bool, string) {
	dep, ok := idx.deployment(t.Namespace, t.Name)
	if !ok {
		return false, fmt.Sprintf("deployment %s/%s is no longer present in the cluster", t.Namespace, t.Name)
	}

	before := preRevision(pre)
	now := maxRevision(idx.revisions(t.Namespace, t.Name))
	if now <= before {
		return false, fmt.Sprintf("deployment %s/%s is still at revision %d; no new rollout has appeared yet", t.Namespace, t.Name, now)
	}

	counts := fmt.Sprintf("revision %d, %d/%d ready, %d updated, %d available",
		now, dep.ReadyReplicas, dep.DesiredReplicas, dep.UpdatedReplicas, dep.AvailableReplicas)
	if dep.UpdatedReplicas != dep.DesiredReplicas ||
		dep.ReadyReplicas != dep.DesiredReplicas ||
		dep.AvailableReplicas != dep.DesiredReplicas {
		return false, fmt.Sprintf("deployment %s/%s is mid-rollout: %s", t.Namespace, t.Name, counts)
	}
	return true, fmt.Sprintf("deployment %s/%s has completed its rollout: %s", t.Namespace, t.Name, counts)
}

// cordonConverged reports whether the node is actually unschedulable now. Cordoning
// sets a spec field and no controller has to agree, so unlike a rollout there is
// nothing to reconcile and the check is the field itself.
func cordonConverged(idx *clusterIndex, _ PreState, t remediate.Target) (bool, string) {
	node, ok := idx.node(t.Name)
	if !ok {
		return false, fmt.Sprintf("node %q is no longer present in the cluster", t.Name)
	}
	if !node.Unschedulable {
		return false, fmt.Sprintf("node %q is still schedulable", t.Name)
	}
	return true, fmt.Sprintf("node %q is cordoned; the scheduler will place no new pods on it", t.Name)
}

// deletePodConverged reports whether the pod is gone.
//
// It checks for the absence of THAT pod and deliberately not for the arrival of its
// replacement. The replacement is the controller's job on the controller's schedule,
// it carries a different name, and waiting for it would make a successful deletion
// look like a failure whenever the cluster has no room to schedule the new pod — a
// separate problem, with a separate diagnosis, that this action never claimed to fix.
func deletePodConverged(idx *clusterIndex, _ PreState, t remediate.Target) (bool, string) {
	if _, ok := idx.pod(t.Namespace, t.Name); ok {
		return false, fmt.Sprintf("pod %s/%s is still present", t.Namespace, t.Name)
	}
	return true, fmt.Sprintf("pod %s/%s is gone; its controller schedules the replacement", t.Namespace, t.Name)
}

// uncordonSatisfied reports whether a node is already back at the schedulability it
// had before MaKlaude cordoned it.
func uncordonSatisfied(idx *clusterIndex, pre PreState, t remediate.Target) (bool, string) {
	node, ok := idx.node(t.Name)
	if !ok {
		return false, fmt.Sprintf("node %q is no longer present in the cluster", t.Name)
	}
	was, _ := pre.Field(fieldUnschedulable)
	if strconv.FormatBool(node.Unschedulable) == was {
		return true, fmt.Sprintf("node %q is already back at unschedulable=%s", t.Name, was)
	}
	return false, fmt.Sprintf("node %q is unschedulable=%t, was %s before the action", t.Name, node.Unschedulable, was)
}

// preRevision reads the deployment revision recorded in a pre-state, defaulting to 0
// — which is below every real revision, so a missing or unparseable record makes the
// convergence check demand evidence of a rollout rather than skip the requirement.
func preRevision(pre PreState) int64 {
	raw, ok := pre.Field(fieldMaxRevision)
	if !ok {
		return 0
	}
	rev, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return rev
}

// maxRevision returns the highest revision in a list, or 0 for an empty one.
func maxRevision(revisions []int64) int64 {
	var highest int64
	for _, rev := range revisions {
		if rev > highest {
			highest = rev
		}
	}
	return highest
}
