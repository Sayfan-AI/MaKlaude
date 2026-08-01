package remediate

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// fixedTime is the pinned collection time threaded through every test snapshot,
// so ProposedAt is deterministic and assertable.
var fixedTime = time.Date(2026, time.July, 5, 12, 0, 0, 0, time.UTC)

// baseSnapshot returns a reachable, empty snapshot for one cluster. Tests layer
// the specific failing objects onto it and then feed it through the REAL
// detect→correlate→diagnose→remediate pipeline rather than hand-built
// hypotheses. That is deliberate: the thing worth proving is that this layer
// plans correctly from the evidence the layer below actually produces, and a
// hand-built hypothesis would let a proposal look right against evidence no real
// diagnosis ever emits. The two dedup tests below are the exception, and say why.
func baseSnapshot() health.Snapshot {
	return health.Snapshot{
		Cluster:      "prod",
		CollectedAt:  fixedTime,
		Reachability: health.Reachability{Reachable: true, ServerVersion: "v1.30.0"},
	}
}

// proposeSnapshot runs the deterministic pipeline a real caller would: analyze
// the snapshot into findings, correlate those into incidents, diagnose them, and
// plan over the whole diagnosis at once.
func proposeSnapshot(snap health.Snapshot) []Proposal {
	incidents := correlate.Correlate(snap, detect.Analyze(snap))
	hyps := diagnose.Incidents(snap, incidents)
	return Hypotheses(snap, hyps)
}

// only returns the single proposal in the list, failing the test otherwise. Most
// scenarios are deliberately one-action situations, and "exactly one" is itself
// part of the claim — a rule that fires twice for one problem is a bug.
func only(t *testing.T, proposals []Proposal) Proposal {
	t.Helper()
	if len(proposals) != 1 {
		t.Fatalf("expected exactly one proposal, got %d: %s", len(proposals), summarize(proposals))
	}
	return proposals[0]
}

// summarize renders proposals compactly for failure messages: the full structs
// carry paragraphs of intent text that bury the thing under test.
func summarize(proposals []Proposal) string {
	if len(proposals) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(proposals))
	for _, p := range proposals {
		out = append(out, p.Operation.String()+" "+p.Target.String()+" ["+p.Reversibility.String()+"]")
	}
	return strings.Join(out, ", ")
}

// preconditionKinds returns a proposal's precondition kinds in order, for
// order-sensitive assertions.
func preconditionKinds(p Proposal) []PreconditionKind {
	out := make([]PreconditionKind, 0, len(p.Preconditions))
	for i := range p.Preconditions {
		out = append(out, p.Preconditions[i].Kind)
	}
	return out
}

// evidenceObjects returns the objects a proposal cites, for order-sensitive
// assertions.
func evidenceObjects(p Proposal) []detect.Object {
	out := make([]detect.Object, 0, len(p.Evidence))
	for i := range p.Evidence {
		out = append(out, p.Evidence[i].Object)
	}
	return out
}

// crashLoopingDeployment returns a snapshot holding one degraded Deployment
// whose pod is crashlooping for no diagnosable reason — no OOM termination, no
// image-pull failure — so the layer below reaches its generic fallback and this
// layer sees CauseUnknown. Revisions are set so the Deployment also has a
// rollback target available, proving the restart rule fires on the cause rather
// than on the mere existence of history.
func crashLoopingDeployment() health.Snapshot {
	s := baseSnapshot()
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "app", Name: "web", ResourceVersion: "100",
			DesiredReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "app", Name: "web-old", ResourceVersion: "101", Revision: 1,
			Owners: []health.OwnerRef{{Kind: "Deployment", Name: "web", Controller: true}}},
		{Namespace: "app", Name: "web-new", ResourceVersion: "102", Revision: 2,
			DesiredReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2,
			Owners: []health.OwnerRef{{Kind: "Deployment", Name: "web", Controller: true}}},
	}
	s.Pods = []health.PodSignal{
		{Namespace: "app", Name: "web-new-aaaa", ResourceVersion: "103", Phase: "Running",
			RestartCount: 5, CrashLoopingContainers: []string{"web"},
			Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "web-new", Controller: true}},
			Containers: []health.ContainerSignal{
				{Name: "web", RestartCount: 5, CrashLooping: true, WaitingReason: "CrashLoopBackOff"},
			}},
	}
	return s
}

// badImageDeployment returns a snapshot holding a Deployment taken down by an
// image that cannot be pulled. revisions controls how many distinct revisions
// survive in its ReplicaSet history, which is what decides whether a rollback is
// possible at all.
func badImageDeployment(revisions int) health.Snapshot {
	s := baseSnapshot()
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "shop", Name: "api", ResourceVersion: "200", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "shop", Name: "api-new", ResourceVersion: "201", Revision: 4,
			DesiredReplicas: 3, AvailableReplicas: 0,
			Owners: []health.OwnerRef{{Kind: "Deployment", Name: "api", Controller: true}}},
	}
	if revisions > 1 {
		s.ReplicaSets = append(s.ReplicaSets, health.ReplicaSetSignal{
			Namespace: "shop", Name: "api-prev", ResourceVersion: "202", Revision: 3,
			Owners: []health.OwnerRef{{Kind: "Deployment", Name: "api", Controller: true}},
		})
	}
	s.Pods = []health.PodSignal{
		{Namespace: "shop", Name: "api-new-aaaa", ResourceVersion: "203", Phase: "Pending", Pending: true,
			Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-new", Controller: true}},
			Containers: []health.ContainerSignal{
				{Name: "api", WaitingReason: "ImagePullBackOff",
					WaitingMessage: `Back-off pulling image "api:nope"`},
			}},
	}
	return s
}

// failedPod returns a snapshot holding one finished pod. controller decides
// whether anything will recreate it, which is the whole difference between a
// recoverable deletion and an irreversible one.
func failedPod(controller bool) health.Snapshot {
	s := baseSnapshot()
	pod := health.PodSignal{
		Namespace: "batch", Name: "job-xyz", ResourceVersion: "300",
		Phase: "Failed", Failed: true, Reason: "Error",
	}
	if controller {
		pod.Owners = []health.OwnerRef{{Kind: "ReplicaSet", Name: "job-rs", Controller: true}}
	}
	s.Pods = []health.PodSignal{pod}
	return s
}

// notReadyNode returns a snapshot holding a NotReady node with pods on it —
// including a failed one, which is what makes the "cordon, never delete" split
// observable. cordoned reports whether the node is already unschedulable.
func notReadyNode(cordoned bool) health.Snapshot {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{
		{Name: "node-a", ResourceVersion: "400", Ready: false, Unschedulable: cordoned},
	}
	s.Pods = []health.PodSignal{
		{Namespace: "app", Name: "web-1", ResourceVersion: "401", Phase: "Running", Node: "node-a",
			RestartCount: 4, CrashLoopingContainers: []string{"web"},
			Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "web-rs", Controller: true}},
			Containers: []health.ContainerSignal{
				{Name: "web", RestartCount: 4, CrashLooping: true, WaitingReason: "CrashLoopBackOff"},
			}},
		{Namespace: "app", Name: "worker-1", ResourceVersion: "402", Phase: "Failed", Node: "node-a",
			Failed: true, Reason: "NodeLost",
			Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "worker-rs", Controller: true}}},
	}
	return s
}

// TestPropose_RolloutRestartUnexplainedCrashLoop proves the restart rule end to
// end: an unexplained crashloop under a Deployment yields exactly one reversible
// rollout-restart aimed at the Deployment (not the pod), carrying the
// Deployment's resourceVersion and a crashloop-still-present precondition.
//
// The Deployment is reached through ownerReferences (pod → ReplicaSet →
// Deployment), and the hypothesis's evidence names only the Deployment, so this
// also covers the transitive half of podsImplicated — without it the rule would
// see no pods and plan nothing.
func TestPropose_RolloutRestartUnexplainedCrashLoop(t *testing.T) {
	p := only(t, proposeSnapshot(crashLoopingDeployment()))

	if p.Operation != OpRolloutRestart {
		t.Fatalf("operation = %q, want %q", p.Operation, OpRolloutRestart)
	}
	if p.Reversibility != ReversibilityReversible {
		t.Fatalf("reversibility = %v, want reversible", p.Reversibility)
	}
	want := Target{Cluster: "prod", Kind: "deployment", Namespace: "app", Name: "web", ResourceVersion: "100"}
	if p.Target != want {
		t.Fatalf("target = %+v, want %+v", p.Target, want)
	}
	if p.Cause != diagnose.CauseUnknown {
		t.Fatalf("cause = %q, want %q", p.Cause, diagnose.CauseUnknown)
	}
	if got, want := preconditionKinds(p), []PreconditionKind{PreconditionUnchanged, PreconditionPodCrashLooping}; !reflect.DeepEqual(got, want) {
		t.Fatalf("precondition kinds = %v, want %v", got, want)
	}
	if got, want := p.Preconditions[1].Expect, "app/web-new-aaaa"; got != want {
		t.Fatalf("crashloop precondition expects %q, want the pod key %q", got, want)
	}
	if p.ProposedAt != fixedTime {
		t.Fatalf("ProposedAt = %v, want the snapshot's collection time %v", p.ProposedAt, fixedTime)
	}
	if !strings.Contains(p.ExpectedEffect, "not taken down") {
		t.Fatalf("expected effect %q should tell the operator the workload stays up", p.ExpectedEffect)
	}
}

// TestPropose_RolloutRestartOncePerDeployment proves several crashlooping pods
// of one Deployment collapse to a single restart: restarting the rollout already
// replaces every pod, so proposing one action per pod would ask a human to
// approve the same rollout three times.
func TestPropose_RolloutRestartOncePerDeployment(t *testing.T) {
	s := crashLoopingDeployment()
	for _, name := range []string{"web-new-bbbb", "web-new-cccc"} {
		pod := s.Pods[0]
		pod.Name = name
		pod.ResourceVersion = "1" + name
		s.Pods = append(s.Pods, pod)
	}

	p := only(t, proposeSnapshot(s))
	if p.Operation != OpRolloutRestart {
		t.Fatalf("operation = %q, want %q", p.Operation, OpRolloutRestart)
	}
}

// TestPropose_RollbackBadImage proves the rollback rule: a bad-image cascade with
// surviving history yields one reversible rollback naming the previous revision,
// both in its precondition and in the text a human reads.
func TestPropose_RollbackBadImage(t *testing.T) {
	p := only(t, proposeSnapshot(badImageDeployment(2)))

	if p.Operation != OpRollbackRevision {
		t.Fatalf("operation = %q, want %q", p.Operation, OpRollbackRevision)
	}
	if p.Reversibility != ReversibilityReversible {
		t.Fatalf("reversibility = %v, want reversible", p.Reversibility)
	}
	if p.Cause != diagnose.CauseBadImage || p.Confidence != diagnose.ConfidenceHigh {
		t.Fatalf("cause/confidence = %q/%v, want badimage/high", p.Cause, p.Confidence)
	}
	want := Target{Cluster: "prod", Kind: "deployment", Namespace: "shop", Name: "api", ResourceVersion: "200"}
	if p.Target != want {
		t.Fatalf("target = %+v, want %+v", p.Target, want)
	}
	if got, want := preconditionKinds(p), []PreconditionKind{PreconditionUnchanged, PreconditionRevisionExists}; !reflect.DeepEqual(got, want) {
		t.Fatalf("precondition kinds = %v, want %v", got, want)
	}
	// Revision 4 is current, so "previous" is 3 — the second-highest surviving
	// revision, which is what Kubernetes' own rollback means.
	if got := p.Preconditions[1].Expect; got != "3" {
		t.Fatalf("rollback precondition expects revision %q, want %q", got, "3")
	}
	if !strings.Contains(p.Intent, "Revision 3") {
		t.Fatalf("intent %q should name the revision being restored", p.Intent)
	}
	if got, want := evidenceObjects(p), []detect.Object{{Kind: "deployment", Namespace: "shop", Name: "api"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence = %+v, want the deployment finding %+v", got, want)
	}
}

// TestPropose_NoRollbackWithoutPreviousRevision proves the rule refuses when
// there is nothing to roll back to. Proposing an action that would fail on
// execution is worse than proposing nothing: it spends a human's approval on a
// no-op and teaches them to distrust the preview.
func TestPropose_NoRollbackWithoutPreviousRevision(t *testing.T) {
	if got := proposeSnapshot(badImageDeployment(1)); len(got) != 0 {
		t.Fatalf("single-revision deployment yielded %s, want no proposals", summarize(got))
	}
}

// TestPropose_DeleteFailedPodWithController proves the delete rule and its
// distinctive reversibility class: a finished pod that a controller will rebuild
// is recreated-by-controller, and the proposal says plainly that the pod itself
// is unrecoverable.
func TestPropose_DeleteFailedPodWithController(t *testing.T) {
	p := only(t, proposeSnapshot(failedPod(true)))

	if p.Operation != OpDeletePod {
		t.Fatalf("operation = %q, want %q", p.Operation, OpDeletePod)
	}
	if p.Reversibility != ReversibilityRecreatedByController {
		t.Fatalf("reversibility = %v, want recreated-by-controller", p.Reversibility)
	}
	want := Target{Cluster: "prod", Kind: "pod", Namespace: "batch", Name: "job-xyz", ResourceVersion: "300"}
	if p.Target != want {
		t.Fatalf("target = %+v, want %+v", p.Target, want)
	}
	if got, want := preconditionKinds(p), []PreconditionKind{PreconditionUnchanged, PreconditionPodFailed, PreconditionPodHasController}; !reflect.DeepEqual(got, want) {
		t.Fatalf("precondition kinds = %v, want %v", got, want)
	}
	if got, want := p.Preconditions[2].Expect, "ReplicaSet/job-rs"; got != want {
		t.Fatalf("controller precondition expects %q, want %q", got, want)
	}
	if !strings.Contains(p.ExpectedEffect, "unrecoverable") {
		t.Fatalf("expected effect %q should state the pod itself cannot be recovered", p.ExpectedEffect)
	}
}

// TestPropose_NoDeleteWithoutController proves the rule refuses an unowned pod.
// The same deletion against a bare pod is irreversible rather than
// recreated-by-controller — a different action with a different risk profile,
// and one this catalog does not contain.
func TestPropose_NoDeleteWithoutController(t *testing.T) {
	if got := proposeSnapshot(failedPod(false)); len(got) != 0 {
		t.Fatalf("unowned failed pod yielded %s, want no proposals", summarize(got))
	}
}

// TestPropose_DeleteEvictedPod proves an Evicted pod counts as finished even
// though its phase is not Failed, and that the intent names the eviction — it
// reads very differently to an operator than a generic failure.
func TestPropose_DeleteEvictedPod(t *testing.T) {
	s := failedPod(true)
	s.Pods[0].Phase = "Failed"
	s.Pods[0].Reason = evictedReason

	p := only(t, proposeSnapshot(s))
	if p.Operation != OpDeletePod {
		t.Fatalf("operation = %q, want %q", p.Operation, OpDeletePod)
	}
	if !strings.Contains(p.Intent, "Evicted") {
		t.Fatalf("intent %q should name the eviction", p.Intent)
	}
}

// TestPropose_CordonNotReadyNode proves the node rule: a NotReady node yields one
// reversible cordon, and — the load-bearing half — the failed pod sitting on that
// node yields NO deletion. Deleting the pods off a NotReady node is eviction by
// another name, and draining is deliberately outside this catalog.
func TestPropose_CordonNotReadyNode(t *testing.T) {
	p := only(t, proposeSnapshot(notReadyNode(false)))

	if p.Operation != OpCordonNode {
		t.Fatalf("operation = %q, want %q", p.Operation, OpCordonNode)
	}
	if p.Reversibility != ReversibilityReversible {
		t.Fatalf("reversibility = %v, want reversible", p.Reversibility)
	}
	want := Target{Cluster: "prod", Kind: "node", Name: "node-a", ResourceVersion: "400"}
	if p.Target != want {
		t.Fatalf("target = %+v, want %+v", p.Target, want)
	}
	if p.Cause != diagnose.CauseNodeFailure {
		t.Fatalf("cause = %q, want %q", p.Cause, diagnose.CauseNodeFailure)
	}
	if got, want := preconditionKinds(p), []PreconditionKind{PreconditionUnchanged, PreconditionNodeNotReady, PreconditionNodeSchedulable}; !reflect.DeepEqual(got, want) {
		t.Fatalf("precondition kinds = %v, want %v", got, want)
	}
	if !strings.Contains(p.ExpectedEffect, "not a drain") {
		t.Fatalf("expected effect %q should say this is not a drain", p.ExpectedEffect)
	}
}

// TestPropose_NoCordonWhenAlreadyCordoned proves the rule does not re-propose an
// action already in effect — whether a human cordoned the node or an earlier run
// of this same proposal did.
func TestPropose_NoCordonWhenAlreadyCordoned(t *testing.T) {
	if got := proposeSnapshot(notReadyNode(true)); len(got) != 0 {
		t.Fatalf("already-cordoned node yielded %s, want no proposals", summarize(got))
	}
}

// TestPropose_NoOverReach is the catalog's negative space: causes that are
// diagnosed confidently but whose real fix is not an operation this package is
// willing to plan yield NOTHING. An OOM kill needs a higher memory limit (a
// restart would merely re-OOM), and insufficient capacity needs more nodes.
// Returning nothing is the correct outcome, not a gap.
func TestPropose_NoOverReach(t *testing.T) {
	oom := baseSnapshot()
	oom.Pods = []health.PodSignal{
		{Namespace: "svc", Name: "cache-1", ResourceVersion: "500", Phase: "Running",
			RestartCount: 6, CrashLoopingContainers: []string{"app"},
			Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "cache-rs", Controller: true}},
			Containers: []health.ContainerSignal{
				{Name: "app", RestartCount: 6, CrashLooping: true, WaitingReason: "CrashLoopBackOff",
					CurrentTermination: &health.TerminationSignal{Reason: "OOMKilled", ExitCode: 137}},
			}},
	}

	capacity := baseSnapshot()
	capacity.Pods = []health.PodSignal{
		{Namespace: "batch", Name: "big-job", ResourceVersion: "600", Phase: "Pending", Pending: true,
			Requests: health.ResourceList{CPU: "8", Memory: "16Gi"}},
	}
	capacity.WarningEvents = []health.EventSignal{
		{Namespace: "batch", Name: "big-job.1", Reason: "FailedScheduling", Count: 3,
			InvolvedObject: "Pod/batch/big-job", LastSeen: fixedTime,
			Message: "0/3 nodes are available: 3 Insufficient cpu, 3 Insufficient memory."},
	}

	for name, snap := range map[string]health.Snapshot{
		"oom kill":               oom,
		"insufficient resources": capacity,
		"healthy cluster":        baseSnapshot(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := proposeSnapshot(snap); len(got) != 0 {
				t.Fatalf("yielded %s, want no proposals", summarize(got))
			}
		})
	}
}

// TestPropose_SortedSafestFirst proves the presentation order a human relies on:
// reversibility ascending, then identity. A cluster with both an unexplained
// crashloop (reversible restart) and an unrelated failed pod (recreated by its
// controller) must show the reversible action first.
func TestPropose_SortedSafestFirst(t *testing.T) {
	s := crashLoopingDeployment()
	s.Pods = append(s.Pods, health.PodSignal{
		Namespace: "batch", Name: "job-xyz", ResourceVersion: "300",
		Phase: "Failed", Failed: true, Reason: "Error",
		Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "job-rs", Controller: true}},
	})

	got := proposeSnapshot(s)
	if len(got) != 2 {
		t.Fatalf("got %d proposals (%s), want 2", len(got), summarize(got))
	}
	if got[0].Operation != OpRolloutRestart || got[1].Operation != OpDeletePod {
		t.Fatalf("order = %s, want the reversible restart before the recreated-by-controller delete", summarize(got))
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool {
		if got[i].Reversibility != got[j].Reversibility {
			return got[i].Reversibility < got[j].Reversibility
		}
		return got[i].Identity < got[j].Identity
	}) {
		t.Fatalf("proposals are not sorted safest-first: %s", summarize(got))
	}
}

// TestPropose_Deterministic proves the byte-stability the audit trail and the
// approval gate depend on: planning the same snapshot twice yields deeply equal
// proposals, and the result does not depend on the order the caller happened to
// pass hypotheses in.
func TestPropose_Deterministic(t *testing.T) {
	s := crashLoopingDeployment()
	s.Nodes = []health.NodeSignal{{Name: "node-b", ResourceVersion: "700", Ready: false}}
	s.Pods = append(s.Pods, health.PodSignal{
		Namespace: "batch", Name: "job-xyz", ResourceVersion: "300",
		Phase: "Failed", Failed: true, Reason: "Error",
		Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "job-rs", Controller: true}},
	})

	first := proposeSnapshot(s)
	if len(first) < 3 {
		t.Fatalf("fixture should exercise several rules, got %s", summarize(first))
	}
	if second := proposeSnapshot(s); !reflect.DeepEqual(first, second) {
		t.Fatalf("two runs over one snapshot differ:\n%s\n%s", summarize(first), summarize(second))
	}

	// Same hypotheses, reversed: ranking happens inside, so the output must not move.
	hyps := diagnose.Incidents(s, correlate.Correlate(s, detect.Analyze(s)))
	reversed := make([]diagnose.Hypothesis, 0, len(hyps))
	for i := len(hyps) - 1; i >= 0; i-- {
		reversed = append(reversed, hyps[i])
	}
	if got := Hypotheses(s, reversed); !reflect.DeepEqual(first, got) {
		t.Fatalf("output depends on hypothesis input order:\n%s\n%s", summarize(first), summarize(got))
	}
}

// TestPropose_StableIdentityAcrossCycles proves a proposal keeps one identity
// while the cluster ticks over underneath it. Every resourceVersion is bumped —
// as a real cluster bumps them continuously — and the identities must be
// unchanged even though the targets are not. This is what lets the approval gate
// track one pending decision instead of re-asking a human every cycle.
func TestPropose_StableIdentityAcrossCycles(t *testing.T) {
	s := crashLoopingDeployment()
	before := proposeSnapshot(s)

	for i := range s.Deployments {
		s.Deployments[i].ResourceVersion += "9"
	}
	for i := range s.ReplicaSets {
		s.ReplicaSets[i].ResourceVersion += "9"
	}
	for i := range s.Pods {
		s.Pods[i].ResourceVersion += "9"
	}
	s.CollectedAt = fixedTime.Add(time.Hour)
	after := proposeSnapshot(s)

	if len(before) != len(after) || len(before) == 0 {
		t.Fatalf("proposal count changed: %s vs %s", summarize(before), summarize(after))
	}
	for i := range before {
		if before[i].Identity != after[i].Identity {
			t.Fatalf("identity changed with resourceVersion: %q vs %q", before[i].Identity, after[i].Identity)
		}
		if before[i].Target.ResourceVersion == after[i].Target.ResourceVersion {
			t.Fatalf("target resourceVersion should track the snapshot, still %q", after[i].Target.ResourceVersion)
		}
	}
}

// TestPropose_UnchangedPreconditionOnEveryProposal proves the
// optimistic-concurrency token is universal, not per-rule. Every proposal must
// carry the target's resourceVersion as an "unchanged" precondition, because a
// proposal is always computed against a snapshot that is already stale by the
// time a human reads it.
func TestPropose_UnchangedPreconditionOnEveryProposal(t *testing.T) {
	snapshots := []health.Snapshot{
		crashLoopingDeployment(),
		badImageDeployment(2),
		failedPod(true),
		notReadyNode(false),
	}
	var seen int
	for _, snap := range snapshots {
		for _, p := range proposeSnapshot(snap) {
			seen++
			if len(p.Preconditions) == 0 || p.Preconditions[0].Kind != PreconditionUnchanged {
				t.Fatalf("%s: first precondition = %v, want %q", summarize([]Proposal{p}), preconditionKinds(p), PreconditionUnchanged)
			}
			if p.Preconditions[0].Expect != p.Target.ResourceVersion || p.Target.ResourceVersion == "" {
				t.Fatalf("%s: unchanged precondition expects %q, want the target's resourceVersion %q",
					summarize([]Proposal{p}), p.Preconditions[0].Expect, p.Target.ResourceVersion)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("covered %d proposals, want one per catalog operation (4)", seen)
	}
}

// TestPropose_SingleHypothesisEntrypoint proves Propose is exactly Hypotheses
// over a one-element slice — the convenience entrypoint must not be a second
// implementation that can drift.
func TestPropose_SingleHypothesisEntrypoint(t *testing.T) {
	s := crashLoopingDeployment()
	hyps := diagnose.Incidents(s, correlate.Correlate(s, detect.Analyze(s)))
	if len(hyps) != 1 {
		t.Fatalf("fixture should yield one hypothesis, got %d", len(hyps))
	}
	if got, want := Propose(s, hyps[0]), Hypotheses(s, hyps); !reflect.DeepEqual(got, want) {
		t.Fatalf("Propose diverges from Hypotheses:\n%s\n%s", summarize(got), summarize(want))
	}
}

// TestPropose_Empty proves the no-diagnosis case returns nil rather than an empty
// slice, matching the convention the layers below use for absent results.
func TestPropose_Empty(t *testing.T) {
	if got := Hypotheses(baseSnapshot(), nil); got != nil {
		t.Fatalf("Hypotheses(nil) = %+v, want nil", got)
	}
}

// TestPropose_DedupsConvergingHypotheses proves two hypotheses that arrive at the
// same action collapse to one proposal, attributed to the better-supported
// diagnosis — so a human is asked once, and the "why" they read is the strongest
// one available.
//
// The hypotheses are hand-built here rather than pipelined, deliberately: the
// deterministic layer below emits one hypothesis per (cause, incident), so making
// two of them converge on one target through real fixtures would test the
// correlator's grouping rather than this package's dedup. What is under test is
// the collapse rule itself.
func TestPropose_DedupsConvergingHypotheses(t *testing.T) {
	s := failedPod(true)
	finding := detect.Finding{
		Severity: detect.SeverityWarning,
		Cluster:  "prod",
		Object:   detect.Object{Kind: "pod", Namespace: "batch", Name: "job-xyz"},
		Title:    "Pod failed",
	}
	weak := diagnose.Hypothesis{
		Identity: "hyp|a", Incident: "inc|a", Cluster: "prod",
		Cause: diagnose.CauseUnknown, Confidence: diagnose.ConfidenceLow,
		Evidence: []detect.Finding{finding}, DetectedAt: fixedTime,
	}
	strong := diagnose.Hypothesis{
		Identity: "hyp|b", Incident: "inc|b", Cluster: "prod",
		Cause: diagnose.CauseBadImage, Confidence: diagnose.ConfidenceHigh,
		Evidence: []detect.Finding{finding}, DetectedAt: fixedTime,
	}

	// Both orders must give the same single proposal, attributed to the strong one.
	for name, hyps := range map[string][]diagnose.Hypothesis{
		"weak first":   {weak, strong},
		"strong first": {strong, weak},
	} {
		t.Run(name, func(t *testing.T) {
			p := only(t, Hypotheses(s, hyps))
			if p.Hypothesis != strong.Identity {
				t.Fatalf("attributed to %q, want the most-confident %q", p.Hypothesis, strong.Identity)
			}
			if p.Confidence != diagnose.ConfidenceHigh || p.Cause != diagnose.CauseBadImage {
				t.Fatalf("carried cause/confidence %q/%v, want the attributed hypothesis's badimage/high", p.Cause, p.Confidence)
			}
		})
	}
}

// TestPropose_EvidenceIsACopy proves a proposal's evidence slice is detached from
// the hypothesis it came from: the audit trail must not be mutable through a
// caller that happens to hold the hypothesis.
func TestPropose_EvidenceIsACopy(t *testing.T) {
	s := badImageDeployment(2)
	hyps := diagnose.Incidents(s, correlate.Correlate(s, detect.Analyze(s)))
	proposals := Hypotheses(s, hyps)
	if len(proposals) == 0 || len(proposals[0].Evidence) == 0 {
		t.Fatalf("fixture should yield a proposal citing evidence, got %s", summarize(proposals))
	}

	proposals[0].Evidence[0].Title = "mutated"
	for _, h := range hyps {
		for _, f := range h.Evidence {
			if f.Title == "mutated" {
				t.Fatal("mutating a proposal's evidence reached back into the hypothesis")
			}
		}
	}
}

// TestReversibilityString pins the tokens fixtures and human-facing renderings
// rely on, including the out-of-range fallback — a new level added without a case
// here must render visibly wrong rather than silently as the zero value.
func TestReversibilityString(t *testing.T) {
	cases := map[Reversibility]string{
		ReversibilityReversible:            "reversible",
		ReversibilityRecreatedByController: "recreated-by-controller",
		ReversibilityIrreversible:          "irreversible",
		Reversibility(9):                   "reversibility(9)",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("Reversibility(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}

// TestReversibilityOrdering pins the ordering the safest-first sort depends on:
// the levels must be ordered by increasing risk, with the safest as the zero
// value.
func TestReversibilityOrdering(t *testing.T) {
	if !(ReversibilityReversible < ReversibilityRecreatedByController &&
		ReversibilityRecreatedByController < ReversibilityIrreversible) {
		t.Fatal("reversibility levels are not ordered by increasing risk")
	}
	if ReversibilityReversible != 0 {
		t.Fatal("the safest level must be the zero value, so an unset field is not silently risky")
	}
}

// TestTargetString proves the compact rendering matches detect.Object's, dropping
// the namespace segment for cluster-scoped objects.
func TestTargetString(t *testing.T) {
	namespaced := Target{Kind: "deployment", Namespace: "app", Name: "web"}
	if got, want := namespaced.String(), "deployment/app/web"; got != want {
		t.Errorf("namespaced target = %q, want %q", got, want)
	}
	clusterScoped := Target{Kind: "node", Name: "node-a"}
	if got, want := clusterScoped.String(), "node/node-a"; got != want {
		t.Errorf("cluster-scoped target = %q, want %q", got, want)
	}
}

// TestProposalIdentity proves what identity is and is not made of: the operation,
// cluster, and target coordinates distinguish proposals, while the
// resourceVersion is deliberately excluded.
func TestProposalIdentity(t *testing.T) {
	base := Target{Cluster: "prod", Kind: "node", Name: "node-a", ResourceVersion: "1"}
	id := newProposalIdentity(OpCordonNode, base)

	bumped := base
	bumped.ResourceVersion = "2"
	if got := newProposalIdentity(OpCordonNode, bumped); got != id {
		t.Errorf("identity changed with resourceVersion: %q vs %q", got, id)
	}

	otherCluster := base
	otherCluster.Cluster = "staging"
	if got := newProposalIdentity(OpCordonNode, otherCluster); got == id {
		t.Errorf("identity collides across clusters: %q", got)
	}
	if got := newProposalIdentity(OpRolloutRestart, base); got == id {
		t.Errorf("identity collides across operations: %q", got)
	}
	if !strings.HasPrefix(string(id), "proposal|") {
		t.Errorf("identity %q should be namespaced with a proposal| prefix", id)
	}
}

// TestOperationString pins the stable tokens the identity key and any future
// allowlist are written in terms of.
func TestOperationString(t *testing.T) {
	for _, op := range []Operation{OpRolloutRestart, OpRollbackRevision, OpDeletePod, OpCordonNode} {
		if op.String() != string(op) {
			t.Errorf("Operation(%q).String() = %q", string(op), op.String())
		}
		if strings.ContainsAny(string(op), "|/ ") {
			t.Errorf("operation token %q contains a delimiter used by the identity key", op)
		}
	}
}
