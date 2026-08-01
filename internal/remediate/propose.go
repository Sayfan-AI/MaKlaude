package remediate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// evictedReason is the pod-level reason Kubernetes records when the kubelet
// evicts a pod under resource pressure. Such a pod is finished — it is kept only
// as a tombstone — so it is a delete candidate alongside a phase-Failed pod.
const evictedReason = "Evicted"

// Propose turns one hypothesis into the remediation actions this package is
// willing to plan for it.
//
// The result is frequently empty, and that is a correct outcome rather than a
// gap: a cause whose only real fix lies outside the catalog (an OOM kill, which
// needs a higher memory limit; insufficient capacity, which needs more nodes)
// yields nothing at all. See the package doc for why not over-reaching is the
// point.
//
// Propose is a pure function of (snap, hyp): it reads only those, calls no clock
// (every proposal inherits the hypothesis's DetectedAt), performs no I/O,
// contacts no cluster, invokes no LLM, and — most importantly — mutates nothing.
// Given the same inputs it always returns the same proposals in the same order.
func Propose(snap health.Snapshot, hyp diagnose.Hypothesis) []Proposal {
	return Hypotheses(snap, []diagnose.Hypothesis{hyp})
}

// Hypotheses plans over a whole diagnosis at once and returns one flat,
// deduplicated list of proposals.
//
// Deduplication is the reason this exists rather than callers looping over
// [Propose]: several hypotheses about one cluster routinely converge on the same
// action, and a human should be asked once. Proposals that share a
// [ProposalIdentity] collapse into one, attributed to the most-confident
// hypothesis that produced it (ties broken by hypothesis identity), so the
// result is independent of the order hypotheses are supplied in.
//
// The returned proposals are sorted by [Reversibility] ascending — safest first,
// which is the order a human should review them in — then by [ProposalIdentity]
// ascending as a fully decisive tiebreak. Because identity is unique per
// (operation, target), that is a total order and the output is byte-stable.
func Hypotheses(snap health.Snapshot, hyps []diagnose.Hypothesis) []Proposal {
	if len(hyps) == 0 {
		return nil
	}
	idx := newSnapshotIndex(snap)

	// Rank the hypotheses before planning, so that when two of them converge on
	// one action the survivor is attributed to the better-supported diagnosis
	// regardless of what order the caller happened to pass them in.
	ranked := make([]diagnose.Hypothesis, len(hyps))
	copy(ranked, hyps)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Confidence != ranked[j].Confidence {
			return ranked[i].Confidence > ranked[j].Confidence
		}
		return ranked[i].Identity < ranked[j].Identity
	})

	var out []Proposal
	seen := make(map[ProposalIdentity]struct{})
	for i := range ranked {
		for _, rule := range rules {
			for _, p := range rule(ranked[i], idx) {
				if _, dup := seen[p.Identity]; dup {
					continue
				}
				seen[p.Identity] = struct{}{}
				out = append(out, p)
			}
		}
	}
	sortProposals(out)
	return out
}

// rule is one deterministic planning rule: it inspects a hypothesis (against the
// snapshot index) and returns the proposals it is willing to make, which is
// routinely none. Rules are pure and independent; several may fire for one
// hypothesis.
type rule func(hyp diagnose.Hypothesis, idx *snapshotIndex) []Proposal

// rules is the fixed, ordered set of planning rules — one per catalog operation.
// The order here does not affect the final output (the final sort re-orders),
// but it is fixed so intermediate behaviour is reproducible.
var rules = []rule{
	rolloutRestartRule,
	rollbackRevisionRule,
	deleteFailedPodRule,
	cordonNodeRule,
}

// rolloutRestartRule proposes restarting a Deployment whose pods are
// crashlooping for a reason MaKlaude could not classify.
//
// It fires only for [diagnose.CauseUnknown], and that restriction is the whole
// design. Every classified cause either has a better-matched action (a bad image
// wants a rollback, not a restart) or has no safe action at all (a restart of an
// OOM-killing container simply OOMs again, wasting a rollout and teaching a human
// to distrust the proposals). "Something is crashlooping and we cannot say why"
// is precisely the situation where an operator's own first move is to restart it
// and watch — so it is the one case where the least-informed diagnosis still
// justifies the most-reversible action. Low confidence in the *cause* is
// acceptable here exactly because the *action* is reversible and gated on a
// human.
func rolloutRestartRule(hyp diagnose.Hypothesis, idx *snapshotIndex) []Proposal {
	if hyp.Cause != diagnose.CauseUnknown {
		return nil
	}
	var out []Proposal
	claimed := make(map[string]struct{})
	for _, pod := range podsImplicated(hyp, idx) {
		if !podCrashLooping(pod) {
			continue
		}
		dep, ok := deploymentForPod(pod, idx)
		if !ok {
			continue
		}
		// Several crashlooping pods of one Deployment are one restart, not several.
		key := deploymentKey(dep.Namespace, dep.Name)
		if _, dup := claimed[key]; dup {
			continue
		}
		claimed[key] = struct{}{}

		target := deploymentTarget(hyp.Cluster, dep)
		out = append(out, newProposal(hyp, OpRolloutRestart, target, ReversibilityReversible,
			"Restart deployment rollout",
			fmt.Sprintf("Pod %s/%s is crashlooping and no specialized diagnosis rule explained why. Restarting the rollout replaces every pod of deployment %s/%s with a fresh one, which clears transient in-process state (a wedged connection pool, a corrupt cache, a leaked lock) without changing the deployment's spec.",
				pod.Namespace, pod.Name, dep.Namespace, dep.Name),
			fmt.Sprintf("Deployment %s/%s performs a rolling restart under its existing update strategy: new pods are created and become ready before old ones are removed, so the workload is not taken down. If the crashloop has an external cause, the new pods will crashloop too and nothing is lost but a rollout.",
				dep.Namespace, dep.Name),
			[]Precondition{
				unchangedPrecondition(target),
				{
					Kind:        PreconditionPodCrashLooping,
					Expect:      podKey(pod.Namespace, pod.Name),
					Description: fmt.Sprintf("Pod %s/%s is still crashlooping (a crashloop that resolved itself needs no restart).", pod.Namespace, pod.Name),
				},
			},
			evidenceFor(hyp,
				detect.Object{Kind: "deployment", Namespace: dep.Namespace, Name: dep.Name},
				detect.Object{Kind: "pod", Namespace: pod.Namespace, Name: pod.Name},
			)))
	}
	return out
}

// rollbackRevisionRule proposes rolling a Deployment back one revision when a bad
// image is what broke it.
//
// [diagnose.CauseBadImage] is the cause with the clearest known-good prior state:
// the image reference changed, the new one cannot be pulled, and the previous
// revision was — demonstrably — running. The rule refuses unless a previous
// revision actually exists in the snapshot, because "roll back" is meaningless
// for a Deployment on its first revision or one whose history Kubernetes has
// already pruned, and proposing an action that would fail is worse than
// proposing nothing.
func rollbackRevisionRule(hyp diagnose.Hypothesis, idx *snapshotIndex) []Proposal {
	if hyp.Cause != diagnose.CauseBadImage {
		return nil
	}
	var out []Proposal
	for _, dep := range deploymentsInEvidence(hyp, idx) {
		prev, ok := previousRevision(dep, idx)
		if !ok {
			continue
		}
		target := deploymentTarget(hyp.Cluster, dep)
		out = append(out, newProposal(hyp, OpRollbackRevision, target, ReversibilityReversible,
			"Roll deployment back one revision",
			fmt.Sprintf("Deployment %s/%s is unavailable because a container image cannot be pulled. Revision %d of this deployment was running before the current one, so it is known-good; rolling back restores it while a human investigates the image.",
				dep.Namespace, dep.Name, prev),
			fmt.Sprintf("Deployment %s/%s returns to the pod template of revision %d and rolls out under its existing update strategy. The failing revision is not deleted — it stays in the deployment's history, so this can be rolled forward again once the image is fixed.",
				dep.Namespace, dep.Name, prev),
			[]Precondition{
				unchangedPrecondition(target),
				{
					Kind:        PreconditionRevisionExists,
					Expect:      strconv.FormatInt(prev, 10),
					Description: fmt.Sprintf("Revision %d of deployment %s/%s still exists (Kubernetes prunes old ReplicaSets past the revision history limit).", prev, dep.Namespace, dep.Name),
				},
			},
			evidenceFor(hyp, detect.Object{Kind: "deployment", Namespace: dep.Namespace, Name: dep.Name})))
	}
	return out
}

// deleteFailedPodRule proposes deleting a pod that has already failed, so its
// controller replaces it.
//
// Two conditions gate it, and both are load-bearing. The pod must already be
// failed or evicted — deleting a working pod is not remediation. And the pod must
// have a controller owner, because that is the entire difference between
// [ReversibilityRecreatedByController] and [ReversibilityIrreversible]: a bare
// pod that is deleted is gone with nothing to rebuild it, which is outside this
// catalog. A failed unowned pod therefore yields no proposal at all.
//
// [diagnose.CauseNodeFailure] is excluded even when its incident contains failed
// pods. Deleting the pods on a NotReady node is eviction by another name, and
// draining is explicitly outside this milestone's scope; the safe action for a
// failed node is to cordon it, which [cordonNodeRule] proposes.
func deleteFailedPodRule(hyp diagnose.Hypothesis, idx *snapshotIndex) []Proposal {
	if hyp.Cause == diagnose.CauseNodeFailure {
		return nil
	}
	var out []Proposal
	for _, pod := range podsImplicated(hyp, idx) {
		if !podFinished(pod) {
			continue
		}
		controller, ok := controllerOf(pod)
		if !ok {
			continue
		}
		target := Target{
			Cluster:         hyp.Cluster,
			Kind:            "pod",
			Namespace:       pod.Namespace,
			Name:            pod.Name,
			ResourceVersion: pod.ResourceVersion,
		}
		out = append(out, newProposal(hyp, OpDeletePod, target, ReversibilityRecreatedByController,
			"Delete failed pod so its controller recreates it",
			fmt.Sprintf("Pod %s/%s is %s and will not recover on its own; it is occupying its slot as a tombstone. Its %s %q will create a replacement as soon as it is removed.",
				pod.Namespace, pod.Name, podFinishedDescription(pod), controller.Kind, controller.Name),
			fmt.Sprintf("Pod %s/%s is deleted permanently — that pod, its name, and its logs are unrecoverable. Its controller %s %q observes the shortfall and schedules a fresh pod, so the workload's replica count is restored automatically.",
				pod.Namespace, pod.Name, controller.Kind, controller.Name),
			[]Precondition{
				unchangedPrecondition(target),
				{
					Kind:        PreconditionPodFailed,
					Description: fmt.Sprintf("Pod %s/%s is still failed or evicted (a pod that recovered must not be deleted).", pod.Namespace, pod.Name),
				},
				{
					Kind:        PreconditionPodHasController,
					Expect:      controller.Kind + "/" + controller.Name,
					Description: fmt.Sprintf("Pod %s/%s is still controlled by %s %q, which is what makes this deletion recoverable rather than permanent.", pod.Namespace, pod.Name, controller.Kind, controller.Name),
				},
			},
			evidenceFor(hyp, detect.Object{Kind: "pod", Namespace: pod.Namespace, Name: pod.Name})))
	}
	return out
}

// cordonNodeRule proposes cordoning a node that is NotReady, so the scheduler
// stops placing new work on it.
//
// Cordoning is the strongest action this catalog takes against a node, and it is
// deliberately far short of what an operator might reach for: the pods already
// running on the node are left completely alone. Draining evicts them, which is
// out of scope for this milestone; cordoning only stops the bleeding by keeping
// new pods off a node that cannot run them, and it is undone by a single
// uncordon.
func cordonNodeRule(hyp diagnose.Hypothesis, idx *snapshotIndex) []Proposal {
	if hyp.Cause != diagnose.CauseNodeFailure {
		return nil
	}
	var out []Proposal
	for _, f := range hyp.Evidence {
		if f.Object.Kind != "node" {
			continue
		}
		node, ok := idx.nodeByName[f.Object.Name]
		if !ok || node.Ready || node.Unschedulable {
			continue
		}
		target := Target{
			Cluster:         hyp.Cluster,
			Kind:            "node",
			Name:            node.Name,
			ResourceVersion: node.ResourceVersion,
		}
		out = append(out, newProposal(hyp, OpCordonNode, target, ReversibilityReversible,
			"Cordon NotReady node",
			fmt.Sprintf("Node %q is NotReady, so it cannot run new work, yet the scheduler will keep assigning pods to it until it is marked unschedulable. Cordoning stops new pods from being placed on a node that will only fail them.",
				node.Name),
			fmt.Sprintf("Node %q is marked unschedulable and receives no new pods. Pods already running on it are NOT touched — this is not a drain. A single uncordon reverses this once the node is healthy.",
				node.Name),
			[]Precondition{
				unchangedPrecondition(target),
				{
					Kind:        PreconditionNodeNotReady,
					Description: fmt.Sprintf("Node %q is still NotReady (a recovered node must not have its capacity removed).", node.Name),
				},
				{
					Kind:        PreconditionNodeSchedulable,
					Description: fmt.Sprintf("Node %q is not already cordoned.", node.Name),
				},
			},
			evidenceFor(hyp, detect.Object{Kind: "node", Name: node.Name})))
	}
	return out
}

// newProposal assembles a proposal, filling in the stable identity (from
// operation + target), the hypothesis attribution, and the inherited cluster,
// cause, confidence, and time. Centralising construction keeps the
// identity/inheritance rules in one place so every rule obeys them.
func newProposal(hyp diagnose.Hypothesis, op Operation, target Target, rev Reversibility,
	title, intent, effect string, preconditions []Precondition, evidence []detect.Finding) Proposal {
	return Proposal{
		Identity:       newProposalIdentity(op, target),
		Hypothesis:     hyp.Identity,
		Incident:       hyp.Incident,
		Cause:          hyp.Cause,
		Confidence:     hyp.Confidence,
		Cluster:        hyp.Cluster,
		Operation:      op,
		Target:         target,
		Reversibility:  rev,
		Title:          title,
		Intent:         intent,
		ExpectedEffect: effect,
		Preconditions:  preconditions,
		Evidence:       evidence,
		ProposedAt:     hyp.DetectedAt,
	}
}

// unchangedPrecondition builds the optimistic-concurrency check every proposal
// carries: the target must still be at the resourceVersion the snapshot saw.
func unchangedPrecondition(t Target) Precondition {
	return Precondition{
		Kind:        PreconditionUnchanged,
		Expect:      t.ResourceVersion,
		Description: fmt.Sprintf("%s is still at resourceVersion %q — it has not been modified since this proposal was computed.", t.String(), t.ResourceVersion),
	}
}

// deploymentTarget builds the target for a Deployment-scoped action.
func deploymentTarget(cluster string, dep health.DeploymentSignal) Target {
	return Target{
		Cluster:         cluster,
		Kind:            "deployment",
		Namespace:       dep.Namespace,
		Name:            dep.Name,
		ResourceVersion: dep.ResourceVersion,
	}
}

// podCrashLooping reports whether any of a pod's containers is crashlooping,
// reading the collector's oscillation-robust per-container flag rather than
// re-deriving the judgment. A crashlooping pod cycles through Running and
// Terminated states, so anything keyed on an instantaneous CrashLoopBackOff
// misses it roughly half the time.
func podCrashLooping(pod health.PodSignal) bool {
	for i := range pod.Containers {
		if pod.Containers[i].CrashLooping {
			return true
		}
	}
	return false
}

// podFinished reports whether a pod has reached a terminal failed state — phase
// Failed, or evicted by the kubelet — and so will never recover on its own.
func podFinished(pod health.PodSignal) bool {
	return pod.Failed || pod.Reason == evictedReason
}

// podFinishedDescription renders why a pod counts as finished, for the intent
// text. Evicted is called out by name because it reads very differently to an
// operator than a generic failure.
func podFinishedDescription(pod health.PodSignal) string {
	if pod.Reason == evictedReason {
		return "Evicted"
	}
	return "in phase Failed"
}

// controllerOf returns the pod's managing controller, if it has one. A pod with
// owners but no *controlling* owner is treated as uncontrolled: only the
// controller rebuilds a deleted pod.
func controllerOf(pod health.PodSignal) (health.OwnerRef, bool) {
	for i := range pod.Owners {
		if pod.Owners[i].Controller {
			return pod.Owners[i], true
		}
	}
	return health.OwnerRef{}, false
}

// deploymentForPod resolves the Deployment that ultimately owns a pod, by
// walking ownerReferences: pod → ReplicaSet → Deployment, or pod → Deployment
// directly. It resolves through the snapshot's actual owner references rather
// than the "<deployment>-<hash>" ReplicaSet naming convention on purpose — a
// name-prefix match is a good enough heuristic for grouping findings, but this
// name is about to be handed to a mutating action, and guessing the wrong
// Deployment there is not a cosmetic error.
//
// It reports false when no Deployment owns the pod, or when the owning
// Deployment is not in the snapshot (so nothing is proposed against an object
// this run never observed).
func deploymentForPod(pod health.PodSignal, idx *snapshotIndex) (health.DeploymentSignal, bool) {
	for i := range pod.Owners {
		o := pod.Owners[i]
		switch o.Kind {
		case "Deployment":
			if dep, ok := idx.deploymentByKey[deploymentKey(pod.Namespace, o.Name)]; ok {
				return dep, true
			}
		case "ReplicaSet":
			rs, ok := idx.replicaSetByKey[replicaSetKey(pod.Namespace, o.Name)]
			if !ok {
				continue
			}
			for j := range rs.Owners {
				ro := rs.Owners[j]
				if ro.Kind != "Deployment" {
					continue
				}
				if dep, ok := idx.deploymentByKey[deploymentKey(rs.Namespace, ro.Name)]; ok {
					return dep, true
				}
			}
		}
	}
	return health.DeploymentSignal{}, false
}

// podsImplicated returns the snapshot pods a hypothesis implicates: those its
// evidence names directly, plus those owned by a workload its evidence names.
//
// The transitive half is not a convenience — without it the pod-scoped rules
// would rarely fire at all. A hypothesis's evidence is curated by the diagnose
// layer for explaining a *cause*, and its generic fallback deliberately cites
// only the incident's primary finding. Since correlation ranks the most
// structural object as primary, an incident about crashlooping pods under a
// broken Deployment presents as evidence naming the Deployment and nothing else.
// Reading only the evidence would therefore see no pods and plan nothing, for
// exactly the situation the pod-scoped rules exist to handle. So evidence is used
// to identify what is implicated, and the snapshot is used to resolve what that
// implicates in turn.
//
// The order is deterministic: evidence order (already stable) at the outer level,
// and the snapshot's own namespace/name pod order within each owner. Pods absent
// from the snapshot are skipped — this package never plans an action against an
// object it cannot see.
func podsImplicated(hyp diagnose.Hypothesis, idx *snapshotIndex) []health.PodSignal {
	var out []health.PodSignal
	seen := make(map[string]struct{})
	add := func(pod health.PodSignal) {
		key := podKey(pod.Namespace, pod.Name)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, pod)
	}
	for _, f := range hyp.Evidence {
		switch f.Object.Kind {
		case "pod":
			if pod, ok := idx.podByKey[podKey(f.Object.Namespace, f.Object.Name)]; ok {
				add(pod)
			}
		case "deployment", "replicaset":
			for _, pod := range idx.podsByOwner[ownerKey(f.Object.Kind, f.Object.Namespace, f.Object.Name)] {
				add(pod)
			}
		}
	}
	return out
}

// deploymentsInEvidence returns the snapshot Deployments a hypothesis implicates
// — those named directly by a deployment finding, plus those reached by walking
// the ownership of any pod in the evidence — deduplicated, in the hypothesis's
// own stable evidence order.
func deploymentsInEvidence(hyp diagnose.Hypothesis, idx *snapshotIndex) []health.DeploymentSignal {
	var out []health.DeploymentSignal
	seen := make(map[string]struct{})
	add := func(dep health.DeploymentSignal) {
		key := deploymentKey(dep.Namespace, dep.Name)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, dep)
	}
	for _, f := range hyp.Evidence {
		switch f.Object.Kind {
		case "deployment":
			if dep, ok := idx.deploymentByKey[deploymentKey(f.Object.Namespace, f.Object.Name)]; ok {
				add(dep)
			}
		case "pod":
			pod, ok := idx.podByKey[podKey(f.Object.Namespace, f.Object.Name)]
			if !ok {
				continue
			}
			if dep, ok := deploymentForPod(pod, idx); ok {
				add(dep)
			}
		}
	}
	return out
}

// previousRevision reports the revision a Deployment would roll back to: the
// second-highest revision among the ReplicaSets it controls, which is what
// Kubernetes' own rollback means by "the previous revision". It reports false
// when fewer than two revisions survive — a first-ever rollout, or a history
// Kubernetes has already pruned — so no rollback is proposed that could not
// actually be performed.
func previousRevision(dep health.DeploymentSignal, idx *snapshotIndex) (int64, bool) {
	var revisions []int64
	seen := make(map[int64]struct{})
	for _, rs := range idx.replicaSetsByDeployment[deploymentKey(dep.Namespace, dep.Name)] {
		if rs.Revision <= 0 {
			continue
		}
		if _, dup := seen[rs.Revision]; dup {
			continue
		}
		seen[rs.Revision] = struct{}{}
		revisions = append(revisions, rs.Revision)
	}
	if len(revisions) < 2 {
		return 0, false
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i] > revisions[j] })
	return revisions[1], true
}

// evidenceFor returns, as a fresh slice, the hypothesis's findings about any of
// the given objects, preserving the hypothesis's own stable evidence order. It is
// how each proposal cites exactly the observations that bear on its own target
// rather than re-attaching the whole incident.
func evidenceFor(hyp diagnose.Hypothesis, objects ...detect.Object) []detect.Finding {
	want := make(map[detect.Object]struct{}, len(objects))
	for _, o := range objects {
		want[o] = struct{}{}
	}
	var out []detect.Finding
	for _, f := range hyp.Evidence {
		if _, ok := want[f.Object]; ok {
			out = append(out, f)
		}
	}
	return out
}

// sortProposals orders proposals safest-first: by [Reversibility] ascending,
// then by [ProposalIdentity] ascending as a fully decisive tiebreak. Reversibility
// leads because it is the axis a human approver triages on — the actions that can
// be undone should be the ones they see first. Because identity is itself fully
// deterministic and unique per (operation, target), the resulting order is
// reproducible for any input and independent of the order proposals were produced
// in.
func sortProposals(proposals []Proposal) {
	sort.Slice(proposals, func(i, j int) bool {
		if proposals[i].Reversibility != proposals[j].Reversibility {
			return proposals[i].Reversibility < proposals[j].Reversibility
		}
		return proposals[i].Identity < proposals[j].Identity
	})
}

// snapshotIndex holds the lookups the rules need to resolve a hypothesis's
// evidence back to the structural facts the collector captured — a pod's
// containers and owners, a node's readiness, a Deployment's ReplicaSet history —
// without rescanning the snapshot per rule. It is built once per call and
// read-only thereafter.
type snapshotIndex struct {
	podByKey                map[string]health.PodSignal
	nodeByName              map[string]health.NodeSignal
	deploymentByKey         map[string]health.DeploymentSignal
	replicaSetByKey         map[string]health.ReplicaSetSignal
	replicaSetsByDeployment map[string][]health.ReplicaSetSignal

	// podsByOwner maps an owning workload — keyed by [ownerKey], and covering both
	// a pod's direct owners and the Deployment reached through its ReplicaSet — to
	// that workload's pods, in the snapshot's own namespace/name order.
	podsByOwner map[string][]health.PodSignal
}

// newSnapshotIndex builds the read-only index over a snapshot. Because the
// snapshot's slices are already deterministically sorted, and this only groups
// them into maps for lookup (never iterating a map to produce output), it
// introduces no nondeterminism.
func newSnapshotIndex(snap health.Snapshot) *snapshotIndex {
	idx := &snapshotIndex{
		podByKey:                make(map[string]health.PodSignal, len(snap.Pods)),
		nodeByName:              make(map[string]health.NodeSignal, len(snap.Nodes)),
		deploymentByKey:         make(map[string]health.DeploymentSignal, len(snap.Deployments)),
		replicaSetByKey:         make(map[string]health.ReplicaSetSignal, len(snap.ReplicaSets)),
		replicaSetsByDeployment: make(map[string][]health.ReplicaSetSignal, len(snap.Deployments)),
		podsByOwner:             make(map[string][]health.PodSignal, len(snap.Deployments)+len(snap.ReplicaSets)),
	}
	for i := range snap.Pods {
		idx.podByKey[podKey(snap.Pods[i].Namespace, snap.Pods[i].Name)] = snap.Pods[i]
	}
	for i := range snap.Nodes {
		idx.nodeByName[snap.Nodes[i].Name] = snap.Nodes[i]
	}
	for i := range snap.Deployments {
		idx.deploymentByKey[deploymentKey(snap.Deployments[i].Namespace, snap.Deployments[i].Name)] = snap.Deployments[i]
	}
	for i := range snap.ReplicaSets {
		rs := snap.ReplicaSets[i]
		idx.replicaSetByKey[replicaSetKey(rs.Namespace, rs.Name)] = rs
		for j := range rs.Owners {
			o := rs.Owners[j]
			if o.Kind == "Deployment" {
				key := deploymentKey(rs.Namespace, o.Name)
				idx.replicaSetsByDeployment[key] = append(idx.replicaSetsByDeployment[key], rs)
			}
		}
	}

	// Pods are indexed by owner in a second pass, because resolving a pod's
	// Deployment walks through the ReplicaSet index built just above. Iterating
	// snap.Pods (already namespace/name sorted) keeps each owner's pod slice in a
	// deterministic order.
	for i := range snap.Pods {
		pod := snap.Pods[i]
		keys := make(map[string]struct{}, len(pod.Owners)+1)
		for j := range pod.Owners {
			keys[ownerKey(strings.ToLower(pod.Owners[j].Kind), pod.Namespace, pod.Owners[j].Name)] = struct{}{}
		}
		if dep, ok := deploymentForPod(pod, idx); ok {
			keys[ownerKey("deployment", dep.Namespace, dep.Name)] = struct{}{}
		}
		for key := range keys {
			idx.podsByOwner[key] = append(idx.podsByOwner[key], pod)
		}
	}
	return idx
}

// ownerKey is the "kind/namespace/name" key pods are indexed by owner under. The
// kind is lowercase so it matches [detect.Object.Kind] directly, letting a
// finding be turned into a lookup without translating its kind.
func ownerKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// podKey, deploymentKey, and replicaSetKey are the namespace/name keys used to
// index each kind. They are separate functions (rather than one shared helper)
// only so a call site reads as what it looks up; the form is identical and
// matches the one the layers below use.
func podKey(namespace, name string) string        { return namespace + "/" + name }
func deploymentKey(namespace, name string) string { return namespace + "/" + name }
func replicaSetKey(namespace, name string) string { return namespace + "/" + name }
