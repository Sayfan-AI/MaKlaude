package execute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// TestRollback_RestoresThePreState is the third done-criterion. It runs the whole
// arc — approve, cordon, roll back — and asserts on the cluster rather than on the
// report: the node ends up back where it started, having been genuinely cordoned in
// between.
func TestRollback_RestoresThePreState(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}
	if !model.node("node-a").Unschedulable {
		t.Fatal("the action did not cordon the node, so there is nothing to test rolling back")
	}
	if !rep.Rollback.Available || rep.Rollback.Kind != RollbackPerformable {
		t.Fatalf("rollback available=%t kind=%s, want an available performable rollback", rep.Rollback.Available, rep.Rollback.Kind)
	}
	if was, ok := rep.PreState.Field(fieldUnschedulable); !ok || was != "false" {
		t.Fatalf("pre-state recorded unschedulable=%q (present=%t), want the schedulable state it had before", was, ok)
	}

	rb, err := h.runner.Rollback(context.Background(), auth, rep)
	if err != nil {
		t.Fatalf("rolling back: %v", err)
	}

	if model.node("node-a").Unschedulable {
		t.Fatal("the node is still cordoned; the rollback did not reach the cluster")
	}
	if !rb.Performed {
		t.Fatal("the rollback reports that it performed nothing")
	}
	if rb.Convergence != ConvergenceConverged {
		t.Fatalf("rollback convergence = %s (%s), want converged", rb.Convergence, rb.ConvergenceDetail)
	}
	if rb.Failure != FailureNone {
		t.Fatalf("rollback failure = %s (%s), want none", rb.Failure, rb.Error)
	}

	// Two requests in total: the action and its inverse. Anything more means the
	// rollback re-drove something.
	if got := h.mutator.callCount(); got != 2 {
		t.Fatalf("the write path received %d requests across action and rollback, want 2: %+v", got, h.mutator.recorded())
	}
	inverse := h.mutator.lastCall(t)
	if inverse.Verb != "patchnode" || !strings.Contains(inverse.Patch, `"unschedulable":false`) {
		t.Fatalf("the inverse request was %+v, want an uncordon patch", inverse)
	}
	// The inverse is conditioned on the version the object has NOW, not on the one the
	// action used — the action is what changed it.
	if inverse.ResourceVersion == p.Target.ResourceVersion {
		t.Fatalf("the rollback was conditioned on the pre-action resourceVersion %q, which no longer exists", inverse.ResourceVersion)
	}
	// The trail records both the execution and the undo, so the artifact reads as one
	// story rather than leaving the rollback invisible.
	if h.recorder.count() != 2 {
		t.Fatalf("the trail has %d entries, want the execution and the rollback", h.recorder.count())
	}
	if !strings.Contains(h.recorder.last(), "Rolled back") {
		t.Fatalf("the trail does not record the rollback: %q", h.recorder.last())
	}
}

// TestRollback_DoesNothingWhenTheTargetIsAlreadyRestored proves a rollback whose
// work is already done sends nothing and reports success. A machine and a person
// taking turns undoing each other is a worse outcome than a redundant no-op.
func TestRollback_DoesNothingWhenTheTargetIsAlreadyRestored(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}

	// A human uncordons the node before MaKlaude is asked to.
	model.mutateNode("node-a", func(n *health.NodeSignal) { n.Unschedulable = false })
	callsBefore := h.mutator.callCount()

	rb, err := h.runner.Rollback(context.Background(), auth, rep)
	if err != nil {
		t.Fatalf("rolling back an already-restored target: %v", err)
	}
	if !rb.AlreadyAtPreState {
		t.Fatal("the rollback did not notice the target was already restored")
	}
	if rb.Performed {
		t.Fatal("the rollback reports performing an action it did not need to send")
	}
	if got := h.mutator.callCount(); got != callsBefore {
		t.Fatalf("the rollback sent %d extra requests to re-assert a state that already held", got-callsBefore)
	}
	if rb.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s, want converged — the desired state holds", rb.Convergence)
	}
}

// TestRollback_RefusesWithoutTheAuthorizationForTheActionItUndoes pins the rule that
// the authority to take an action includes the authority to undo it AND NOTHING
// ELSE. Each case is a different way a caller could try to get a mutating request
// out of this method without holding the right slip.
func TestRollback_RefusesWithoutTheAuthorizationForTheActionItUndoes(t *testing.T) {
	setup := func(t *testing.T) (*harness, Report, *approve.Authorization) {
		t.Helper()
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		p := cordonProposal()
		auth := authorizationFor(t, p)
		rep, err := h.runner.Execute(context.Background(), auth, p)
		if err != nil {
			t.Fatalf("executing: %v", err)
		}
		return h, rep, auth
	}

	t.Run("no authorization", func(t *testing.T) {
		h, rep, _ := setup(t)
		before := h.mutator.callCount()
		rb, err := h.runner.Rollback(context.Background(), nil, rep)
		if !errors.Is(err, ErrNotAuthorized) {
			t.Fatalf("expected ErrNotAuthorized, got: %v", err)
		}
		if rb.Failure != FailureNotAuthorized {
			t.Fatalf("failure = %s, want not-authorized", rb.Failure)
		}
		if h.mutator.callCount() != before {
			t.Fatal("an unauthorized rollback reached the write path")
		}
	})

	t.Run("another action's authorization", func(t *testing.T) {
		h, rep, _ := setup(t)
		before := h.mutator.callCount()
		other := authorizationFor(t, restartProposal())

		rb, err := h.runner.Rollback(context.Background(), other, rep)
		if !errors.Is(err, ErrNotAuthorized) {
			t.Fatalf("expected ErrNotAuthorized, got: %v", err)
		}
		if rb.Failure != FailureNotAuthorized {
			t.Fatalf("failure = %s, want not-authorized", rb.Failure)
		}
		if h.mutator.callCount() != before {
			t.Fatal("one action's permission slip undid another action")
		}
	})

	t.Run("a runner pointed at another cluster", func(t *testing.T) {
		h, rep, auth := setup(t)
		before := h.mutator.callCount()
		h.mutator.name = "staging"

		rb, err := h.runner.Rollback(context.Background(), auth, rep)
		if !errors.Is(err, ErrClusterMismatch) {
			t.Fatalf("expected ErrClusterMismatch, got: %v", err)
		}
		if rb.Failure != FailureClusterMismatch {
			t.Fatalf("failure = %s, want cluster-mismatch", rb.Failure)
		}
		if h.mutator.callCount() != before {
			t.Fatal("a rollback crossed clusters")
		}
	})

	t.Run("the kill switch is off", func(t *testing.T) {
		h, rep, auth := setup(t)
		before := h.mutator.callCount()
		h.mutator.mode = kube.ExecuteDisabled

		rb, err := h.runner.Rollback(context.Background(), auth, rep)
		if !errors.Is(err, ErrKillSwitch) {
			t.Fatalf("expected ErrKillSwitch, got: %v", err)
		}
		if rb.Failure != FailureKillSwitch {
			t.Fatalf("failure = %s, want kill-switch", rb.Failure)
		}
		if h.mutator.callCount() != before {
			t.Fatal("a rollback ran with the kill switch off")
		}
	})
}

// TestRollback_RefusesWhatCannotBeUndone enumerates the four "there is nothing to
// roll back" situations. They are separate cases because they are genuinely
// different, and a human reading the error needs to know which one they are in.
func TestRollback_RefusesWhatCannotBeUndone(t *testing.T) {
	t.Run("an action that never ran", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		p := cordonProposal()
		auth := authorizationFor(t, p)

		// Drift the node so the action aborts cleanly, leaving a report of an action
		// that did not happen.
		model.mutateNode("node-a", func(n *health.NodeSignal) { n.Unschedulable = true })
		rep, err := h.runner.Execute(context.Background(), auth, p)
		if !errors.Is(err, ErrPreconditionDrift) {
			t.Fatalf("setup: expected a drifted abort, got: %v", err)
		}

		rb, rberr := h.runner.Rollback(context.Background(), auth, rep)
		if !errors.Is(rberr, ErrNotRollbackable) {
			t.Fatalf("expected ErrNotRollbackable, got: %v", rberr)
		}
		if rb.Failure != FailureNotRollbackable {
			t.Fatalf("failure = %s, want not-rollbackable", rb.Failure)
		}
		if h.mutator.callCount() != 0 {
			t.Fatal("rolling back an action that never ran sent a mutating request")
		}
	})

	t.Run("a preview", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		h.mutator.mode = kube.ExecuteDryRun
		p := cordonProposal()
		auth := authorizationFor(t, p)

		rep, err := h.runner.Execute(context.Background(), auth, p)
		if err != nil {
			t.Fatalf("previewing: %v", err)
		}
		before := h.mutator.callCount()

		if _, rberr := h.runner.Rollback(context.Background(), auth, rep); !errors.Is(rberr, ErrNotRollbackable) {
			t.Fatalf("expected ErrNotRollbackable for a preview, got: %v", rberr)
		}
		if h.mutator.callCount() != before {
			t.Fatal("rolling back a preview sent a mutating request")
		}
	})

	t.Run("a deleted pod", func(t *testing.T) {
		model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
		h := newHarness(t, model, fastPolicy())
		p := deletePodProposal()
		auth := authorizationFor(t, p)

		rep, err := h.runner.Execute(context.Background(), auth, p)
		if err != nil {
			t.Fatalf("deleting: %v", err)
		}
		before := h.mutator.callCount()

		_, rberr := h.runner.Rollback(context.Background(), auth, rep)
		if !errors.Is(rberr, ErrNotRollbackable) {
			t.Fatalf("expected ErrNotRollbackable, got: %v", rberr)
		}
		if !strings.Contains(rberr.Error(), "impossible") {
			t.Fatalf("the refusal does not say the effect cannot be undone: %v", rberr)
		}
		if h.mutator.callCount() != before {
			t.Fatal("rolling back a pod deletion sent a mutating request")
		}
	})

	t.Run("a rollout restart", func(t *testing.T) {
		model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
		h := newHarness(t, model, fastPolicy())
		p := restartProposal()
		auth := authorizationFor(t, p)
		h.observer.beforeRead = func(read int) {
			if read == 2 {
				model.rollOut("shop", "web", 5)
			}
		}

		rep, err := h.runner.Execute(context.Background(), auth, p)
		if err != nil {
			t.Fatalf("restarting: %v", err)
		}
		before := h.mutator.callCount()

		_, rberr := h.runner.Rollback(context.Background(), auth, rep)
		if !errors.Is(rberr, ErrNotRollbackable) {
			t.Fatalf("expected ErrNotRollbackable, got: %v", rberr)
		}
		if !strings.Contains(rberr.Error(), "not-required") {
			t.Fatalf("the refusal does not say there is nothing to undo: %v", rberr)
		}
		if h.mutator.callCount() != before {
			t.Fatal("rolling back a restart sent a mutating request")
		}
	})
}

// TestRollback_AbortsWhenTheTargetHasVanished proves a rollback against an object
// that no longer exists refuses rather than sending a request that could only fail.
func TestRollback_AbortsWhenTheTargetHasVanished(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if err != nil {
		t.Fatalf("executing: %v", err)
	}
	before := h.mutator.callCount()
	delete(model.nodes, "node-a")

	rb, rberr := h.runner.Rollback(context.Background(), auth, rep)
	if !errors.Is(rberr, ErrNotRollbackable) {
		t.Fatalf("expected ErrNotRollbackable, got: %v", rberr)
	}
	if rb.Failure != FailureNotRollbackable {
		t.Fatalf("failure = %s, want not-rollbackable", rb.Failure)
	}
	if h.mutator.callCount() != before {
		t.Fatal("a rollback was sent for an object that no longer exists")
	}
}
