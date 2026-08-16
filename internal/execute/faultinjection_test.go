package execute

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// Milestone 6's timing case: what happens when a cluster breaks WHILE MaKlaude is
// remediating it.
//
// Everything proving remediation works elsewhere seeds a fault, lets it settle, and
// then acts. That proves the detectors read a broken cluster correctly and proves
// nothing about the only condition production actually supplies, which is that the
// world keeps moving during the seconds an action takes. [Runner.execute] has three
// windows in which it can move, and each has a different mechanism catching it:
//
//	                         ┌─ window 1 ─┐            ┌─ window 2 ─┐
//	 approval ─── plan ───── │            │ re-check ─ │            │ send ─┐
//	                         └────────────┘            └────────────┘       │
//	   caught by: MaKlaude's own precondition re-check    the API server's   │
//	              (nothing is sent)                       resourceVersion   │
//	                                                       precondition     │
//	                                                                        │
//	                                          ┌──────── window 3 ───────────┘
//	                                          │  bounded convergence watch
//	                                          └─ caught by: nothing. It is REPORTED.
//
// # Why the faults here are modelled rather than injected through Chaos Mesh
//
// Window 2 is the interval between one read and one write on the same goroutine —
// microseconds to low milliseconds. No real injector can be aimed at it: Chaos Mesh
// takes a CR through an admission webhook and a controller reconcile before its
// fault lands, which is orders of magnitude wider than the window it would have to
// hit, and an experiment that missed would land in window 1 or window 3 and pass a
// test written for window 2. A scenario whose timing is a coin flip is not a
// scenario, so the injection point is deterministic here and the live-cluster half
// is T8's job (issue #197), which covers the windows a real experiment can reach.
//
// What is NOT modelled away is the consequence. The faults below move the same
// [clusterModel] every other test in this package reads, the write path evaluates a
// real resourceVersion precondition against it (see [clusterModel.admit]), and the
// assertions are about what the runner did — requests sent, cluster state, trail
// entries — rather than about what its report claims.
//
// # The fault shapes come from the chaos catalog, not from imagination
//
// Each fault is tied to a [chaos.Action] this project can actually ask for. That
// matters for what these scenarios can claim: today's catalog is PodChaos only, so
// the objects a real experiment can perturb are pods and, through them, deployments.
// A node cannot be perturbed by any catalogued action, which is why the cordon
// operation appears nowhere below despite being the package's most-tested one —
// asserting an outcome for a fault nobody can inject would be fiction.

// chaosFault is one modelled Chaos Mesh action: what MaKlaude would ask for, and what
// the cluster looks like afterwards.
//
// The [chaos.Action] is a field rather than a comment so the tie is checked by the
// compiler: renaming or retiring an action in the catalog breaks this file, which is
// the point at which someone should ask whether the scenario still describes
// something MaKlaude can do.
type chaosFault struct {
	action chaos.Action
	effect string
	apply  func(*clusterModel)
}

func (f chaosFault) String() string { return fmt.Sprintf("%s (%s)", f.action, f.effect) }

// podKill models [chaos.ActionPodKill]: Chaos Mesh deletes the pod outright and its
// controller schedules a replacement under a different name. The object MaKlaude was
// reasoning about is gone, not modified.
func podKill(namespace, name string) chaosFault {
	return chaosFault{
		action: chaos.ActionPodKill,
		effect: "pod " + namespace + "/" + name + " is deleted",
		apply:  func(m *clusterModel) { m.removePod(namespace, name) },
	}
}

// podFailure models [chaos.ActionPodFailure]: Chaos Mesh replaces the pod's
// containers with a pause image for the experiment's duration. The pod survives under
// the same name, so what MaKlaude sees is the other half of the fault space — the
// object still exists and has MOVED.
func podFailure(namespace, name string) chaosFault {
	return chaosFault{
		action: chaos.ActionPodFailure,
		effect: "pod " + namespace + "/" + name + " has its containers replaced with a pause image",
		apply: func(m *clusterModel) {
			m.mutatePod(namespace, name, func(p *health.PodSignal) {
				for i := range p.Containers {
					p.Containers[i].CrashLooping = false
					p.Containers[i].RestartCount++
					p.Containers[i].WaitingReason = "ContainerCreating"
				}
			})
		},
	}
}

// mutatePod applies a change to a pod and bumps its resourceVersion, exactly as any
// write to the object would. It is the pod counterpart of [clusterModel.mutateNode]
// and lives here because the fault scenarios are what needed it.
func (c *clusterModel) mutatePod(namespace, name string, apply func(*health.PodSignal)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := namespace + "/" + name
	pod, ok := c.pods[key]
	if !ok {
		return
	}
	apply(&pod)
	c.nextVersion++
	pod.ResourceVersion = versionString(c.nextVersion)
	c.pods[key] = pod
}

// dropReadyReplica models the deployment status the API server publishes moments
// after a pod-kill takes one of its replicas: the rollout it just completed is no
// longer fully available.
//
// It is separate from [podKill] rather than folded into it because the two are
// separate events in a real cluster — the pod goes, and then the ReplicaSet
// controller notices — and only the second one is what a convergence check reads.
func (c *clusterModel) dropReadyReplica(namespace, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := namespace + "/" + name
	dep, ok := c.deployments[key]
	if !ok || dep.ReadyReplicas == 0 {
		return
	}
	dep.ReadyReplicas--
	dep.AvailableReplicas--
	c.nextVersion++
	dep.ResourceVersion = versionString(c.nextVersion)
	c.deployments[key] = dep
}

// shortWindow is a convergence window small enough that a scenario which is SUPPOSED
// to time out does so in milliseconds. Every other fixture uses [fastPolicy], whose
// window is deliberately generous so a slow machine cannot turn a converging run into
// a spurious timeout; here the timeout is the assertion, so the bound is tight.
func shortWindow() Policy {
	policy := fastPolicy()
	policy.ObserveWindow = 40 * time.Millisecond
	policy.ObserveInterval = 5 * time.Millisecond
	return policy
}

// TestFaultBeforeTheRecheck_AbortsCleanlyHavingSentNothing is window 1.
//
// The fault lands in the instant before the runner takes its one live read, which is
// what separates this from the ordinary stale-approval case: the approval was fresh,
// the world was intact when the run started, and the object still disappeared before
// anything could be sent. MaKlaude's own re-check is what catches it, so the cost is
// zero mutating requests and zero entries in the cluster's audit log.
//
// The assertion that carries the weight is the request count, not the error. A report
// saying "aborted" is the easiest thing for a broken implementation to produce.
func TestFaultBeforeTheRecheck_AbortsCleanlyHavingSentNothing(t *testing.T) {
	model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
	h := newHarness(t, model, fastPolicy())
	fault := podKill("shop", "web-dead")

	// Read 1 is the precondition re-check; the fault lands immediately before it.
	h.observer.beforeRead = func(read int) {
		if read == 1 {
			fault.apply(model)
		}
	}

	rep, err := h.execute(deletePodProposal())

	if !errors.Is(err, ErrPreconditionDrift) {
		t.Fatalf("%s before the re-check returned %v, want ErrPreconditionDrift", fault, err)
	}
	if got := h.mutator.callCount(); got != 0 {
		t.Fatalf("%s before the re-check still sent %d mutating request(s): %+v", fault, got, h.mutator.recorded())
	}
	if rep.Failure != FailureDrifted || !rep.Failure.CleanAbort() {
		t.Fatalf("failure = %s (clean abort = %t), want drifted and clean", rep.Failure, rep.Failure.CleanAbort())
	}
	if rep.Executed || rep.Recorded || rep.Attempts != 0 {
		t.Fatalf("an aborted action reports executed=%t recorded=%t attempts=%d", rep.Executed, rep.Recorded, rep.Attempts)
	}
	if rep.Convergence != ConvergenceUnobserved {
		t.Fatalf("convergence = %s, want unobserved — nothing ran", rep.Convergence)
	}
	// The report must name the vanished pod, because "a precondition failed" leaves an
	// operator with nothing to correlate against the experiment they just ran.
	if !mentions(rep.DriftedPreconditions(), "no longer present") {
		t.Fatalf("the drift report does not say the target is gone: %+v", rep.DriftedPreconditions())
	}
}

// TestFaultBetweenRecheckAndWrite_MovingTheTargetIsACleanAbort is window 2, healthy
// half.
//
// The preconditions held when MaKlaude looked, and the object moved in the interval
// between that look and the API server's own evaluation. There is no way to close
// that interval — it is the reason optimistic concurrency exists — so the property
// being asserted is that the request MaKlaude sends still carries the version it
// checked, and therefore fails rather than applying to a state nobody approved.
//
// [FailureConflict] and [FailureDrifted] are the same event caught one layer apart,
// and both must read as clean aborts: the correct response is for the next cycle to
// re-propose, not for a human to be woken.
func TestFaultBetweenRecheckAndWrite_MovingTheTargetIsACleanAbort(t *testing.T) {
	model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
	h := newHarness(t, model, fastPolicy())
	fault := podFailure("shop", "web-dead")

	h.mutator.beforeWrite = func() { fault.apply(model) }

	rep, err := h.execute(deletePodProposal())

	if !errors.Is(err, kube.ErrPreconditionConflict) {
		t.Fatalf("%s between the re-check and the write returned %v, want kube.ErrPreconditionConflict", fault, err)
	}
	if rep.Failure != FailureConflict {
		t.Fatalf("failure = %s, want precondition-conflict", rep.Failure)
	}
	if !rep.Failure.CleanAbort() {
		t.Fatal("a conflict caught by the API server must read as a clean abort, exactly as one caught by the re-check does")
	}
	if rep.Executed || rep.Recorded {
		t.Fatalf("a rejected request reports executed=%t recorded=%t", rep.Executed, rep.Recorded)
	}
	// The request WAS sent — unlike window 1 — and exactly once. A conflict is not
	// retryable, so a second attempt here would mean the runner had abandoned the
	// precondition to make the action succeed.
	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("the write path received %d requests, want exactly 1: %+v", got, h.mutator.recorded())
	}
	if rep.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 — a precondition conflict must never be retried", rep.Attempts)
	}
	// Nothing was applied: the pod the action would have deleted is still there.
	if _, present := model.liveVersion("pod/shop/web-dead"); !present {
		t.Fatal("the pod was deleted by a request the API server rejected")
	}
}

// TestFaultBetweenRecheckAndWrite_DestroyingTheTargetIsNotACleanAbort is window 2,
// and it is the finding this task exists to produce rather than a property being
// confirmed.
//
// A pod-kill in the same window as the scenario above produces a materially different
// outcome, because [kube.Executor.act] maps exactly one API server response to
// [kube.ErrPreconditionConflict] — a 409 — and everything else, a 404 included, to
// [kube.ErrExecute]. So "the target moved" is a clean abort and "the target ceased to
// exist" is [FailureExecute]: the class whose documented meaning is "whether the
// change landed may be unknown", routed to a human.
//
// For a delete, that is the wrong verdict twice over. The outcome is not unknown — a
// 404 is proof the request was evaluated and nothing was applied — and the action's
// entire goal was for that pod to be gone, which it now is. MaKlaude escalates a
// remediation whose desired end state has already been reached by other means.
//
// This test asserts the behaviour that EXISTS, deliberately, rather than the
// behaviour that would be nice; changing the classification is a decision about the
// write path's error taxonomy that reaches further than one operation. Filed as
// issue #214.
func TestFaultBetweenRecheckAndWrite_DestroyingTheTargetIsNotACleanAbort(t *testing.T) {
	model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
	h := newHarness(t, model, fastPolicy())
	fault := podKill("shop", "web-dead")

	h.mutator.beforeWrite = func() { fault.apply(model) }

	rep, err := h.execute(deletePodProposal())

	if !errors.Is(err, kube.ErrExecute) {
		t.Fatalf("%s between the re-check and the write returned %v, want kube.ErrExecute", fault, err)
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the error does not carry the API server's not-found response: %v", err)
	}
	if errors.Is(err, kube.ErrPreconditionConflict) {
		t.Fatal("a 404 must not be reported as a precondition conflict; only a 409 is one")
	}

	// The finding, stated as an assertion so it cannot rot into prose nobody re-reads.
	// When issue #214 is resolved these two lines are what change.
	if rep.Failure != FailureExecute {
		t.Fatalf("failure = %s, want execute-failed (see issue #214)", rep.Failure)
	}
	if rep.Failure.CleanAbort() {
		t.Fatal("issue #214 has been fixed but this test was not updated: a vanished delete target now reads as a clean abort")
	}

	// Everything the classification implies, checked rather than assumed — because the
	// argument for issue #214 is that these facts and that classification disagree.
	if _, present := model.liveVersion("pod/shop/web-dead"); present {
		t.Fatal("the pod is still present, so this is not the scenario under test")
	}
	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("the write path received %d requests, want exactly 1: %+v", got, h.mutator.recorded())
	}
	if rep.Executed {
		t.Fatal("the report claims the action executed; the API server rejected it")
	}
	if rep.Recorded {
		t.Fatal("a rejected action was marked executed on the approval trail")
	}
}

// TestFaultDuringConvergence_TimesOutWithoutRetryingOrRollingBack is window 3.
//
// Nothing catches a fault here, and nothing should: the action has already landed and
// been recorded, so there is no precondition left to fail. What the window produces is
// a verdict, and the fault makes that verdict wrong in the operator's favour — the
// restart genuinely took effect, and a pod-kill on one of the new replicas is what
// keeps the deployment from reporting itself fully rolled out.
//
// The two things that must not happen are the two a system trying to be helpful would
// do: re-send the action because its effect looks incomplete, or undo it because the
// window expired. [ConvergenceTimedOut] is a report, not a trigger.
func TestFaultDuringConvergence_TimesOutWithoutRetryingOrRollingBack(t *testing.T) {
	model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
	h := newHarness(t, model, shortWindow())
	fault := podKill("shop", "web-abc")

	// Read 1 is the precondition re-check. On the first observation read the restart has
	// taken effect — a new revision exists and the rollout completed — and the fault
	// lands on one of the replicas it just produced, in that order, because a fault that
	// arrived before the rollout would be testing window 1 again.
	h.observer.beforeRead = func(read int) {
		if read == 2 {
			model.rollOut("shop", "web", 5)
			fault.apply(model)
			model.dropReadyReplica("shop", "web")
		}
	}

	rep, err := h.execute(restartProposal())

	if err != nil {
		t.Fatalf("a fault during the observation window must be reported, not returned as a failure: %v", err)
	}
	if rep.Failure != FailureNone {
		t.Fatalf("failure = %s, want none — the action itself succeeded", rep.Failure)
	}
	if !rep.Executed || !rep.Recorded {
		t.Fatalf("the action ran; a disturbed observation does not un-run it (executed=%t recorded=%t)", rep.Executed, rep.Recorded)
	}
	if rep.Convergence != ConvergenceTimedOut {
		t.Fatalf("convergence = %s (%s), want timed-out after %s", rep.Convergence, rep.ConvergenceDetail, fault)
	}
	// "We looked and it had not happened", not "we could not look". The cluster was
	// readable throughout, and collapsing the two would tell an operator the fix failed
	// when the truth is that something else broke while it was settling.
	if rep.Convergence == ConvergenceUnobservable {
		t.Fatal("a readable cluster reported as unobservable")
	}
	if !strings.Contains(rep.ConvergenceDetail, "mid-rollout") {
		t.Fatalf("the report does not say what was actually seen: %q", rep.ConvergenceDetail)
	}
	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("the write path received %d requests, want exactly 1 — a slow effect must never re-drive the action: %+v",
			got, h.mutator.recorded())
	}
	// One request and one execution record. A second of either would be the runner
	// reacting to the verdict, which is the thing [Runner.Rollback] exists to keep as a
	// caller's decision.
	if got := h.recorder.count(); got != 1 {
		t.Fatalf("the approval trail carries %d execution records, want exactly 1", got)
	}
}

// TestFaultDuringConvergence_AfterTheVerdictIsNotSeen states the limit of the window
// 3 verdict, which is the thing most easily mistaken for a stronger guarantee than it
// is.
//
// [Runner.observe] returns the moment its predicate first holds. So the report says
// "converged" about an INSTANT, and a fault landing one millisecond later — well
// inside the nominal window — is invisible to it. That is the right bound for this
// layer: the alternative is watching for the whole window on every success, which
// makes every remediation cost the full observation budget and still guarantees
// nothing about the millisecond after it ends.
//
// It is stated here because M6 is the milestone that makes the distinction load
// bearing. A chaos run re-injects a fault precisely to see whether a fix held, and
// "did it hold?" is answered by the trust ledger's recurrence horizon across cycles,
// never by one execution's convergence verdict.
func TestFaultDuringConvergence_AfterTheVerdictIsNotSeen(t *testing.T) {
	model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
	h := newHarness(t, model, shortWindow())
	fault := podKill("shop", "web-abc")

	h.observer.beforeRead = func(read int) {
		if read == 2 {
			model.rollOut("shop", "web", 5)
		}
	}

	rep, err := h.execute(restartProposal())
	if err != nil {
		t.Fatalf("executing a restart: %v", err)
	}
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s (%s), want converged on the first observation read", rep.Convergence, rep.ConvergenceDetail)
	}
	// The window closed as soon as the predicate held rather than running to its bound:
	// one precondition read plus one observation read.
	if got := model.readCount(); got != 2 {
		t.Fatalf("the cluster was read %d times, want 2 — the watch must stop at the first success, not run the window out", got)
	}

	// Now the fault lands, still inside the nominal 40ms window.
	fault.apply(model)
	model.dropReadyReplica("shop", "web")

	// The report is unchanged, and that is not a bug — it is a verdict about a moment
	// that has passed. The same predicate against the cluster as it is NOW disagrees,
	// which is what makes the staleness a fact rather than a worry.
	if rep.Convergence != ConvergenceConverged {
		t.Fatal("the report mutated after the run returned")
	}
	held, seen := rolloutRestartConverged(newClusterIndex(model.snapshot()), rep.PreState, restartProposal().Target)
	if held {
		t.Fatalf("%s did not disturb the deployment, so this scenario proves nothing: %s", fault, seen)
	}
	if !strings.Contains(seen, "mid-rollout") {
		t.Fatalf("the post-fault reading is %q, want the deployment reported as mid-rollout", seen)
	}
}

// mentions reports whether any precondition's observation contains the substring, so a
// scenario can assert that a report names what happened without pinning a whole
// sentence.
func mentions(results []PreconditionResult, substring string) bool {
	for _, pc := range results {
		if strings.Contains(pc.Observed, substring) {
			return true
		}
	}
	return false
}
