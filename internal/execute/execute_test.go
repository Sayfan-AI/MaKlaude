package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// TestExecute_ApprovedActionRunsExactlyOnceAndTheClusterConverges is the headline
// promise of this package, asserted from three independent directions rather than
// from the report alone: the write path received exactly one request, the cluster
// actually changed, and the approval trail was told so. A report claiming success is
// the easiest of the four things to produce and the least worth trusting on its own.
func TestExecute_ApprovedActionRunsExactlyOnceAndTheClusterConverges(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()

	rep, err := h.execute(p)
	if err != nil {
		t.Fatalf("executing an approved action: %v", err)
	}

	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("the write path received %d requests, want exactly 1: %+v", got, h.mutator.recorded())
	}
	sent := h.mutator.lastCall(t)
	if sent.Verb != "cordon" || sent.Name != "node-a" {
		t.Fatalf("sent %+v, want a cordon of node-a", sent)
	}
	if sent.ResourceVersion != "1001" {
		t.Fatalf("the request was conditioned on resourceVersion %q, want the approved 1001", sent.ResourceVersion)
	}

	if !model.node("node-a").Unschedulable {
		t.Fatal("the node is still schedulable; the action did not reach the cluster")
	}

	if !rep.Executed || rep.DryRun {
		t.Fatalf("report says executed=%t dryRun=%t, want a real execution", rep.Executed, rep.DryRun)
	}
	if rep.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", rep.Attempts)
	}
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s (%s), want converged", rep.Convergence, rep.ConvergenceDetail)
	}
	if rep.Failure != FailureNone {
		t.Fatalf("failure = %s (%s), want none", rep.Failure, rep.Error)
	}
	if !rep.Recorded || h.recorder.count() != 1 {
		t.Fatalf("recorded=%t with %d trail entries, want the execution recorded exactly once", rep.Recorded, h.recorder.count())
	}
	if rep.FinishedAt.Before(rep.StartedAt) {
		t.Fatal("the report finished before it started")
	}
}

// TestExecute_MarksTheApprovalTrailExecuted proves the single-execution enforcement
// reaches the real gatekeeper, not just a fake: after the runner reports success the
// artifact carries the executed label, which is what stops a later pass from
// authorizing the same action again.
func TestExecute_MarksTheApprovalTrailExecuted(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	p := cordonProposal()
	g := newGate(t, p)
	auth := g.authorize()

	mutator := newFakeMutator(model)
	runner, err := New(mutator, &fakeObserver{model: model}, g.gk, audit.NewTrail(), fastPolicy())
	if err != nil {
		t.Fatalf("building runner: %v", err)
	}

	if _, err := runner.Execute(context.Background(), auth, p); err != nil {
		t.Fatalf("executing: %v", err)
	}

	artifact := g.artifact()
	if !artifact.HasLabel(approve.ExecutedLabel) {
		t.Fatalf("the approval artifact does not carry %q; nothing durably prevents a second execution: %v",
			approve.ExecutedLabel, artifact.Labels)
	}
	if !containsSubstring(artifact.Comments, "Executed.") {
		t.Fatalf("the trail does not record the execution: %v", artifact.Comments)
	}
}

// TestExecute_DriftedPreconditionAbortsCleanly is the second done-criterion. Each
// case is a different way the world can move out from under an approval, and all
// three must produce the same shape: nothing sent, a distinct error class, and a
// report a human can read to see what changed.
//
// "Nothing sent" is asserted on the write path rather than inferred from the report,
// because a report is what a buggy implementation would still get right.
func TestExecute_DriftedPreconditionAbortsCleanly(t *testing.T) {
	cases := map[string]struct {
		drift      func(*clusterModel)
		wantDetail string
	}{
		// Every change to an object bumps its resourceVersion, so the cases that move a
		// FIELD trip the unchanged check too. That is faithful rather than sloppy: the
		// assertion is that the report names the specific thing that moved, which is
		// what a human needs, and the per-check coverage lives in precondition_test.go.
		"the object was modified after the proposal": {
			drift:      func(m *clusterModel) { m.mutateNode("node-a", func(*health.NodeSignal) {}) },
			wantDetail: "it changed after the proposal was computed",
		},
		"the node recovered": {
			drift:      func(m *clusterModel) { m.mutateNode("node-a", func(n *health.NodeSignal) { n.Ready = true }) },
			wantDetail: "Ready again",
		},
		"the node was already cordoned by someone else": {
			drift:      func(m *clusterModel) { m.mutateNode("node-a", func(n *health.NodeSignal) { n.Unschedulable = true }) },
			wantDetail: "already cordoned",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := newClusterModel().withNode("node-a")
			h := newHarness(t, model, fastPolicy())
			p := cordonProposal()
			tc.drift(model)

			rep, err := h.execute(p)

			if !errors.Is(err, ErrPreconditionDrift) {
				t.Fatalf("expected ErrPreconditionDrift, got: %v", err)
			}
			if got := h.mutator.callCount(); got != 0 {
				t.Fatalf("a drifted precondition still sent %d mutating requests: %+v", got, h.mutator.recorded())
			}
			if rep.Failure != FailureDrifted {
				t.Fatalf("failure = %s, want drifted", rep.Failure)
			}
			if !rep.CleanAbort() {
				t.Fatal("a drifted precondition must read as a clean abort, not as something that needs a human")
			}
			if rep.Executed || rep.Recorded || rep.Attempts != 0 {
				t.Fatalf("an aborted action reports executed=%t recorded=%t attempts=%d", rep.Executed, rep.Recorded, rep.Attempts)
			}
			if h.recorder.count() != 0 {
				t.Fatal("an aborted action was recorded on the approval trail as executed")
			}
			if rep.Convergence != ConvergenceUnobserved {
				t.Fatalf("convergence = %s, want unobserved — nothing ran", rep.Convergence)
			}

			drifted := rep.DriftedPreconditions()
			if len(drifted) == 0 {
				t.Fatal("the report does not name which precondition failed")
			}
			observed := make([]string, 0, len(drifted))
			for _, pc := range drifted {
				observed = append(observed, pc.Observed)
			}
			if !containsSubstring(observed, tc.wantDetail) {
				t.Fatalf("the drift report says %q, none of which mentions %q", observed, tc.wantDetail)
			}
			if !strings.Contains(err.Error(), tc.wantDetail) {
				t.Fatalf("the returned error does not explain the drift on its own: %v", err)
			}
			// Every precondition is evaluated, not just up to the first failure, so a
			// human sees everything that moved.
			if len(rep.Preconditions) != len(p.Preconditions) {
				t.Fatalf("evaluated %d of %d preconditions; all of them must be checked",
					len(rep.Preconditions), len(p.Preconditions))
			}
		})
	}
}

// TestExecute_RefusesWithoutAValidMatchingAuthorization covers the permission gate.
// The zero-value case is the one that matters most: it is what a caller gets by
// constructing &approve.Authorization{} and is the closest thing to forging consent
// that the type system allows.
func TestExecute_RefusesWithoutAValidMatchingAuthorization(t *testing.T) {
	other := restartProposal()

	cases := map[string]struct {
		auth *approve.Authorization
		want error
	}{
		"no authorization at all":         {auth: nil, want: ErrNotAuthorized},
		"a zero-value authorization":      {auth: &approve.Authorization{}, want: ErrNotAuthorized},
		"an authorization for another op": {auth: authorizationFor(t, other), want: ErrNotAuthorized},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := newClusterModel().withNode("node-a")
			h := newHarness(t, model, fastPolicy())

			rep, err := h.runner.Execute(context.Background(), tc.auth, cordonProposal())
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got: %v", tc.want, err)
			}
			if rep.Failure != FailureNotAuthorized {
				t.Fatalf("failure = %s, want not-authorized", rep.Failure)
			}
			if h.mutator.callCount() != 0 {
				t.Fatalf("an unauthorized action reached the write path: %+v", h.mutator.recorded())
			}
			if model.readCount() != 0 {
				t.Fatal("an unauthorized action read the cluster; permission is checked before anything else")
			}
		})
	}
}

// TestExecute_RefusesWhenTheClustersDoNotAgree proves multi-cluster isolation is
// enforced rather than assumed, in both directions it can break: a permission slip
// for one cluster presented to a runner wired to another, and an observer reporting
// a different cluster than the write client can reach.
func TestExecute_RefusesWhenTheClustersDoNotAgree(t *testing.T) {
	t.Run("the write client reaches a different cluster", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		h.mutator.name = "staging"

		rep, err := h.execute(cordonProposal())
		if !errors.Is(err, ErrClusterMismatch) {
			t.Fatalf("expected ErrClusterMismatch, got: %v", err)
		}
		if rep.Failure != FailureClusterMismatch {
			t.Fatalf("failure = %s, want cluster-mismatch", rep.Failure)
		}
		if h.mutator.callCount() != 0 {
			t.Fatal("an action crossed clusters and reached the write path")
		}
	})

	t.Run("the observer reports a different cluster", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		model.name = "staging"
		h := newHarness(t, model, fastPolicy())

		rep, err := h.execute(cordonProposal())
		if !errors.Is(err, ErrClusterMismatch) {
			t.Fatalf("expected ErrClusterMismatch, got: %v", err)
		}
		if rep.Failure != FailureClusterMismatch {
			t.Fatalf("failure = %s, want cluster-mismatch", rep.Failure)
		}
		if h.mutator.callCount() != 0 {
			t.Fatal("preconditions were judged against one cluster and the action sent to another")
		}
	})
}

// TestExecute_HonorsTheKillSwitchAtExecutionTime proves the switch is re-read when
// the action runs rather than trusted from construction. The unknown-mode case is
// the fail-closed direction: a value this package cannot name is not one it may
// assume is safe.
func TestExecute_HonorsTheKillSwitchAtExecutionTime(t *testing.T) {
	for name, mode := range map[string]kube.ExecuteMode{
		"explicitly disabled": kube.ExecuteDisabled,
		"zero value":          kube.ExecuteMode(0),
		"unknown mode":        kube.ExecuteMode(99),
	} {
		t.Run(name, func(t *testing.T) {
			model := newClusterModel().withNode("node-a")
			h := newHarness(t, model, fastPolicy())
			h.mutator.mode = mode

			rep, err := h.execute(cordonProposal())
			if !errors.Is(err, ErrKillSwitch) {
				t.Fatalf("expected ErrKillSwitch, got: %v", err)
			}
			if rep.Failure != FailureKillSwitch {
				t.Fatalf("failure = %s, want kill-switch", rep.Failure)
			}
			if h.mutator.callCount() != 0 {
				t.Fatal("the kill switch did not stop the request")
			}
			if model.node("node-a").Unschedulable {
				t.Fatal("the cluster changed while the kill switch was off")
			}
		})
	}
}

// TestExecute_DryRunPreviewsWithoutExecutingOrRecording covers the subtlest
// correctness requirement in the package: a preview must NOT be recorded as an
// execution. Recording one would apply the executed label to the artifact, and the
// gate never authorizes an executed artifact again — so a single preview would
// permanently block the real action a human had approved.
func TestExecute_DryRunPreviewsWithoutExecutingOrRecording(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.mutator.mode = kube.ExecuteDryRun

	rep, err := h.execute(cordonProposal())
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}

	if h.mutator.callCount() != 1 {
		t.Fatalf("a preview sent %d requests, want 1", h.mutator.callCount())
	}
	if !rep.DryRun || rep.Executed {
		t.Fatalf("report says dryRun=%t executed=%t, want a preview", rep.DryRun, rep.Executed)
	}
	if rep.Recorded || h.recorder.count() != 0 {
		t.Fatal("a preview was recorded on the approval trail as an execution, which would permanently block the real one")
	}
	if model.node("node-a").Unschedulable {
		t.Fatal("a preview changed the cluster")
	}
	if rep.Convergence != ConvergenceUnobserved {
		t.Fatalf("convergence = %s, want unobserved — a preview has nothing to converge to", rep.Convergence)
	}
	if rep.Rollback.Available {
		t.Fatal("a preview reports a rollback as available; there is nothing to undo")
	}
}

// TestExecute_AutoApprovedActionStillAbortsOnPreconditionDrift is the executor's half
// of "the bypass waives consent and nothing else".
//
// The precondition re-check is the last thing standing between an approval and a
// cluster, and it is the one that matters MOST under autonomous mode rather than least:
// with a human in the loop, somebody looked at the world a moment ago; with the bypass
// on, this re-read is the only look anything takes. So the assertion is made on the
// write path — zero requests sent — rather than on the report, because a report is what
// a buggy implementation would still get right.
func TestExecute_AutoApprovedActionStillAbortsOnPreconditionDrift(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	p := cordonProposal()

	// The node recovered between the proposal and the action. Nobody was asked, so
	// nobody could notice; the re-check has to.
	model.mutateNode("node-a", func(n *health.NodeSignal) { n.Ready = true })

	rep, err := h.executeAutoApproved(p)
	if !errors.Is(err, ErrPreconditionDrift) {
		t.Fatalf("expected ErrPreconditionDrift, got: %v", err)
	}
	if got := h.mutator.callCount(); got != 0 {
		t.Fatalf("an auto-approved action with a drifted precondition still sent %d mutating requests: %+v", got, h.mutator.recorded())
	}
	if model.node("node-a").Unschedulable {
		t.Fatal("an auto-approved action cordoned a node that had already recovered")
	}
	if rep.Failure != FailureDrifted || !rep.CleanAbort() {
		t.Fatalf("failure = %s cleanAbort = %t, want a clean drifted abort", rep.Failure, rep.CleanAbort())
	}
	if rep.Executed || rep.Recorded || rep.Attempts != 0 {
		t.Fatalf("an aborted action reports executed=%t recorded=%t attempts=%d", rep.Executed, rep.Recorded, rep.Attempts)
	}

	// The abandonment is audited, and audited as unreviewed. "MaKlaude was authorized
	// to cordon this node and did not, because the node had recovered" is exactly the
	// record an operator running unattended needs to be able to find.
	failed := h.recordFor(audit.PhaseFailed)
	if failed.Approver.Authority != audit.AuthorityPolicy {
		t.Errorf("authority = %s, want %s", failed.Approver.Authority, audit.AuthorityPolicy)
	}
	if !failed.Outcome.CleanAbort || failed.Change.Sent {
		t.Errorf("the record says cleanAbort=%t sent=%t, want an abort that sent nothing", failed.Outcome.CleanAbort, failed.Change.Sent)
	}
}

// TestExecute_AutoApprovedDryRunStillOnlyPreviews is the composition property the
// autonomous-mode bypass has to hold: the approval gate and the write-path kill switch
// are two INDEPENDENT gates, and waiving the first does not touch the second.
//
// The failure it guards against is the natural mental slip — "autonomous mode means
// MaKlaude acts on its own", read as "MaKlaude acts". They answer different questions.
// One asks whether this may run at all; the other asks whether a real write is
// permitted, and it is set by the operator on the write client, not by the approval
// policy. An operator who turns the bypass on while the executor is still in dry-run
// has asked for an unattended REHEARSAL, and getting that wrong would mutate a
// production cluster on a configuration that promised it would not.
func TestExecute_AutoApprovedDryRunStillOnlyPreviews(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.mutator.mode = kube.ExecuteDryRun

	rep, err := h.executeAutoApproved(cordonProposal())
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}

	if model.node("node-a").Unschedulable {
		t.Fatal("an auto-approved action changed the cluster while the write path was in dry-run")
	}
	if !rep.DryRun || rep.Executed {
		t.Fatalf("report says dryRun=%t executed=%t, want a preview", rep.DryRun, rep.Executed)
	}
	if rep.Recorded || h.recorder.count() != 0 {
		t.Fatal("a preview was recorded on the approval trail as an execution, which would permanently block the real one")
	}
	if rep.Mode != kube.ExecuteDryRun {
		t.Errorf("report mode = %s, want the kill switch's posture to be recorded as it was read", rep.Mode)
	}

	// The audit trail must show both facts at once: nobody reviewed it, and nothing
	// was applied. Either alone would be a misleading record.
	exec := h.recordFor(audit.PhaseExecuted)
	if exec.Approver.Authority != audit.AuthorityPolicy {
		t.Errorf("authority = %s, want %s", exec.Approver.Authority, audit.AuthorityPolicy)
	}
	if exec.Change.Applied || !exec.Change.DryRun {
		t.Errorf("the record says applied=%t dryRun=%t, want a preview", exec.Change.Applied, exec.Change.DryRun)
	}
	if rendered := audit.Lifecycle(h.records()); !strings.Contains(rendered, "previewed") {
		t.Errorf("the rendered lifecycle does not say the action was only previewed:\n%s", rendered)
	}
}

// TestExecute_RefusesAnIrreversibleAction covers the guard that fires only once the
// catalog grows an irreversible operation — which is exactly when it needs to
// already be there. The unclassified case is the same guard from the other side: a
// reversibility this layer cannot name is treated as the worst one.
func TestExecute_RefusesAnIrreversibleAction(t *testing.T) {
	for name, class := range map[string]remediate.Reversibility{
		"irreversible": remediate.ReversibilityIrreversible,
		"unclassified": remediate.Reversibility(42),
	} {
		t.Run(name, func(t *testing.T) {
			model := newClusterModel().withNode("node-a")
			h := newHarness(t, model, fastPolicy())

			p := cordonProposal()
			p.Reversibility = class

			rep, err := h.execute(p)
			if !errors.Is(err, ErrIrreversible) {
				t.Fatalf("expected ErrIrreversible, got: %v", err)
			}
			if rep.Failure != FailureRefused {
				t.Fatalf("failure = %s, want refused", rep.Failure)
			}
			if rep.CleanAbort() {
				t.Fatal("a refusal to act on an irreversible action must not read as a routine clean abort")
			}
			if h.mutator.callCount() != 0 {
				t.Fatal("an irreversible action reached the write path")
			}
		})
	}
}

// TestExecute_RollsBackToTheRevisionTheApproverWasShown covers the operation this
// layer refused until the write path could express it faithfully (issue #127).
//
// The assertion that matters is the REVISION the request carried. Everything else about
// a rollback — the target, the precondition, the convergence check — is shared with the
// restart it sits beside, but which revision gets restored is the whole action, and the
// only record of the one a human agreed to is the precondition they were shown. A
// rollback that sent a plausible number from a live read would pass a test that only
// checked "a rollback was sent".
func TestExecute_RollsBackToTheRevisionTheApproverWasShown(t *testing.T) {
	model := rollbackModel()
	h := newHarness(t, model, fastPolicy())

	rep, err := h.execute(revisionRollbackProposal())
	if err != nil {
		t.Fatalf("executing an approved rollback: %v", err)
	}
	if rep.Failure != FailureNone {
		t.Fatalf("failure = %s (%s), want none", rep.Failure, rep.Error)
	}
	if !rep.Executed || rep.DryRun {
		t.Fatalf("executed=%t dryRun=%t, want a real execution", rep.Executed, rep.DryRun)
	}

	sent := h.mutator.lastCall(t)
	if h.mutator.callCount() != 1 {
		t.Fatalf("the rollback produced %d requests, want exactly 1", h.mutator.callCount())
	}
	if sent.Verb != "rollback" {
		t.Fatalf("verb = %q, want the rollback primitive (a strategic-merge patch cannot restore a template)", sent.Verb)
	}
	if sent.Revision != 4 {
		t.Fatalf("rolled back to revision %d, want 4 — the revision named by the approved precondition", sent.Revision)
	}
	if sent.ResourceVersion != "2002" {
		t.Fatalf("conditioned on resourceVersion %q, want the one the proposal was computed against", sent.ResourceVersion)
	}

	// The restored ReplicaSet is re-annotated with the next revision, so the deployment's
	// highest revision moved — the same evidence a restart's convergence check reads.
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s (%s), want converged", rep.Convergence, rep.ConvergenceDetail)
	}
	if !rep.Rollback.Available {
		t.Fatal("a performed rollback reports no rollback of its own; rolling forward again is the inverse and the pre-state records it")
	}
}

// TestExecute_RefusesARollbackTheApprovalDoesNotParameterize is the fail-closed half.
//
// A slip with no revision precondition, or one naming something that is not a revision,
// leaves nothing recording WHICH pod template a human agreed to restore. There is no
// safe default — every candidate is a guess at a template nobody previewed — so the
// action is refused before the cluster is even read. "Before the cluster is read" is
// asserted rather than assumed: a refusal that still costs a snapshot would mean the
// resolution had drifted back down into the send path.
func TestExecute_RefusesARollbackTheApprovalDoesNotParameterize(t *testing.T) {
	cases := map[string][]remediate.Precondition{
		"no revision precondition": {
			{Kind: remediate.PreconditionUnchanged, Expect: "2002", Description: "deployment/shop/web is still at resourceVersion 2002."},
		},
		"revision is not a number": {
			{Kind: remediate.PreconditionUnchanged, Expect: "2002", Description: "deployment/shop/web is still at resourceVersion 2002."},
			{Kind: remediate.PreconditionRevisionExists, Expect: "latest", Description: "Revision latest still exists."},
		},
		"revision is zero": {
			{Kind: remediate.PreconditionUnchanged, Expect: "2002", Description: "deployment/shop/web is still at resourceVersion 2002."},
			{Kind: remediate.PreconditionRevisionExists, Expect: "0", Description: "Revision 0 still exists."},
		},
	}

	for name, conditions := range cases {
		t.Run(name, func(t *testing.T) {
			model := rollbackModel()
			h := newHarness(t, model, fastPolicy())

			p := revisionRollbackProposal()
			p.Preconditions = conditions

			rep, err := h.execute(p)
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("expected ErrRefused, got: %v", err)
			}
			if rep.Failure != FailureRefused {
				t.Fatalf("failure = %s, want refused", rep.Failure)
			}
			if rep.CleanAbort() {
				t.Fatal("an unparameterized approval is a refusal, not the routine clean abort of a stale one")
			}
			if h.mutator.callCount() != 0 {
				t.Fatal("a rollback with no approved revision reached the write path")
			}
			if h.observer.reads != 0 {
				t.Fatalf("the refusal read the cluster %d time(s); it is resolvable from the approval alone", h.observer.reads)
			}
		})
	}
}

// TestExecute_AbortsCleanlyWhenTheRevisionIsPrunedAfterTheCheck covers the one race the
// precondition cannot close.
//
// [checkRevisionExists] verifies the revision against the runner's snapshot, and the
// write path then does its OWN read to find the template — Kubernetes prunes ReplicaSets
// past a Deployment's history limit, and it can happen in between. The outcome must be
// the clean abort a caller re-proposes from rather than the execution failure a human is
// paged for: nothing was sent, because the read precedes the patch.
func TestExecute_AbortsCleanlyWhenTheRevisionIsPrunedAfterTheCheck(t *testing.T) {
	model := rollbackModel()
	h := newHarness(t, model, fastPolicy())
	h.mutator.beforeRollback = func() { model.pruneRevision("shop", "web", 4) }

	rep, err := h.execute(revisionRollbackProposal())
	if !errors.Is(err, ErrPreconditionDrift) {
		t.Fatalf("expected ErrPreconditionDrift, got: %v", err)
	}
	if rep.Failure != FailureDrifted || !rep.CleanAbort() {
		t.Fatalf("failure = %s (cleanAbort=%t), want a clean drifted abort", rep.Failure, rep.CleanAbort())
	}
	if rep.Attempts != 0 {
		t.Fatalf("attempts = %d, want 0: the revision is read before any patch is composed, so nothing was sent", rep.Attempts)
	}
	if h.mutator.callCount() != 0 {
		t.Fatal("a pruned revision still produced a mutating request")
	}
	if rep.Executed || rep.Recorded {
		t.Fatalf("executed=%t recorded=%t; an abort must not be marked on the approval trail", rep.Executed, rep.Recorded)
	}
}

// TestAnUnknownOperationCannotReachTheRunnerAndStillFailsClosed covers the two
// layers that both have to hold for a catalog that grows carelessly.
//
// The outer one is that an operation with no rollback plan is never AUTHORIZED at
// all — approve refuses it with ReasonNoRollbackPlan — so a valid permission slip
// for one cannot exist. That is asserted here rather than assumed, because this
// package's own refusal is written on the premise that it is a second line rather
// than the first.
//
// The inner one is this package's own fail-closed default, exercised directly since
// no authorization can carry it here.
func TestAnUnknownOperationCannotReachTheRunnerAndStillFailsClosed(t *testing.T) {
	unknown := cordonProposal()
	unknown.Operation = remediate.Operation("deletenamespace")
	unknown.Identity = remediate.ProposalIdentity("proposal|deletenamespace|prod|node/node-a")

	if issued := newGate(t, unknown).tryAuthorize(); len(issued) != 0 {
		t.Fatalf("the approval gate issued %d permission slips for an operation with no rollback plan", len(issued))
	}

	if _, ok := planFor(unknown.Operation); ok {
		t.Fatalf("operation %q has a plan in the execution layer; the fixture is no longer unknown", unknown.Operation)
	}
	if _, err := checkActionable(plan{}, unknown); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("an unclassified operation was not refused: %v", err)
	}
}

// TestExecute_FailureStopsAndDoesNotRetryLoop is the fourth done-criterion, and the
// assertion that carries it is the attempt COUNT — a claim about not thrashing that
// is only worth making if something counts.
//
// The three cases are the three answers a mutation can get, and they get three
// different treatments on purpose.
func TestExecute_FailureStopsAndDoesNotRetryLoop(t *testing.T) {
	forbidden := fmt.Errorf("%w: PATCH \"node/node-a\": %w", kube.ErrExecute,
		apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "node-a", errors.New("RBAC")))
	throttled := fmt.Errorf("%w: PATCH \"node/node-a\": %w", kube.ErrExecute,
		apierrors.NewTooManyRequests("busy", 1))
	conflict := fmt.Errorf("%w: PATCH \"node/node-a\": %w", kube.ErrPreconditionConflict,
		apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node-a", errors.New("modified")))

	cases := map[string]struct {
		err          error
		wantAttempts int
		wantFailure  FailureClass
		wantClean    bool
	}{
		// A denial is final: nothing about it improves by being asked twice.
		"an RBAC denial is not retried": {err: forbidden, wantAttempts: 1, wantFailure: FailureExecute},
		// A conflict is the healthy outcome of a stale approval. Retrying could only
		// succeed by abandoning the precondition.
		"a precondition conflict is not retried": {err: conflict, wantAttempts: 1, wantFailure: FailureConflict, wantClean: true},
		// Throttling is the one retryable answer, and even it is bounded.
		"throttling is retried but bounded": {err: throttled, wantAttempts: 3, wantFailure: FailureExecute},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			model := newClusterModel().withNode("node-a")
			h := newHarness(t, model, fastPolicy())
			h.mutator.err = tc.err

			rep, err := h.execute(cordonProposal())
			if err == nil {
				t.Fatal("a failed action reported success")
			}
			if got := h.mutator.callCount(); got != tc.wantAttempts {
				t.Fatalf("the write path received %d requests, want %d — this is the no-thrash bound", got, tc.wantAttempts)
			}
			if rep.Attempts != tc.wantAttempts {
				t.Fatalf("report says %d attempts, want %d", rep.Attempts, tc.wantAttempts)
			}
			if rep.Failure != tc.wantFailure {
				t.Fatalf("failure = %s, want %s", rep.Failure, tc.wantFailure)
			}
			if rep.CleanAbort() != tc.wantClean {
				t.Fatalf("cleanAbort = %t, want %t", rep.CleanAbort(), tc.wantClean)
			}
			if rep.Executed || rep.Recorded {
				t.Fatal("a failed action reported itself executed or recorded")
			}
			if rep.Error == "" {
				t.Fatal("the report carries no rendered error, so a serialized copy says nothing about what went wrong")
			}
		})
	}
}

// TestExecute_RetriesOnceThenSucceeds proves the bound is a CAP rather than a fixed
// count: a throttled attempt that succeeds on the retry sends two requests, not
// three, and reports a real execution.
func TestExecute_RetriesOnceThenSucceeds(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.mutator.err = fmt.Errorf("%w: %w", kube.ErrExecute, apierrors.NewTooManyRequests("busy", 1))
	h.mutator.failFirst = 1

	rep, err := h.execute(cordonProposal())
	if err != nil {
		t.Fatalf("a retried action failed: %v", err)
	}
	if rep.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", rep.Attempts)
	}
	if !rep.Executed || !model.node("node-a").Unschedulable {
		t.Fatal("the retried action did not land")
	}
}

// TestExecute_UnreadableClusterAbortsBeforeActing proves an unreadable cluster is
// never treated as one where the preconditions hold. Both shapes are covered
// because they arrive differently: a failed read errors, while an unreachable
// cluster is a successful read reporting unreachability.
func TestExecute_UnreadableClusterAbortsBeforeActing(t *testing.T) {
	t.Run("the read fails", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		h := newHarness(t, model, fastPolicy())
		h.observer.failNext = 1
		h.observer.err = errors.New("connection reset by peer")

		rep, err := h.execute(cordonProposal())
		if !errors.Is(err, ErrUnobservable) {
			t.Fatalf("expected ErrUnobservable, got: %v", err)
		}
		if rep.Failure != FailureUnobservable {
			t.Fatalf("failure = %s, want unobservable", rep.Failure)
		}
		if h.mutator.callCount() != 0 {
			t.Fatal("an action was sent to a cluster whose preconditions could not be checked")
		}
	})

	t.Run("the cluster is unreachable", func(t *testing.T) {
		model := newClusterModel().withNode("node-a")
		model.reachable = false
		h := newHarness(t, model, fastPolicy())

		rep, err := h.execute(cordonProposal())
		if !errors.Is(err, ErrUnobservable) {
			t.Fatalf("expected ErrUnobservable, got: %v", err)
		}
		if rep.Failure != FailureUnobservable {
			t.Fatalf("failure = %s, want unobservable", rep.Failure)
		}
		if h.mutator.callCount() != 0 {
			t.Fatal("an action was sent to a cluster reported as unreachable")
		}
	})
}

// TestExecute_ARecordingFailureIsReportedAndTheActionIsNotRepeated covers the
// nastiest ordering in the package. The action HAS run; only the bookkeeping failed.
// Repeating it would be a second execution of something a human approved once, so
// the runner must finish the attempt, report the failure loudly, and send nothing
// further.
func TestExecute_ARecordingFailureIsReportedAndTheActionIsNotRepeated(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())
	h.recorder.err = errors.New("502 from the issue trail")

	rep, err := h.execute(cordonProposal())
	if !errors.Is(err, ErrRecord) {
		t.Fatalf("expected ErrRecord, got: %v", err)
	}
	if rep.Failure != FailureRecord {
		t.Fatalf("failure = %s, want record-failed", rep.Failure)
	}
	if !rep.Executed {
		t.Fatal("the report denies that the action ran; it did, and a caller must know")
	}
	if rep.Recorded {
		t.Fatal("the report claims the execution was recorded when the recorder failed")
	}
	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("the write path received %d requests after a recording failure, want exactly 1", got)
	}
	// The action still converged, and the report still says so: a bookkeeping failure
	// must not cost the operator the one piece of information they wanted.
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s, want converged", rep.Convergence)
	}
}

// TestExecute_ObservationIsBoundedAndReportsWhatItSaw proves the runner never blocks
// indefinitely on an action that does not take effect. The fake write path here
// accepts the request and changes nothing, which is the shape of a remediation that
// was applied and simply did not work.
func TestExecute_ObservationIsBoundedAndReportsWhatItSaw(t *testing.T) {
	model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
	policy := fastPolicy()
	policy.ObserveWindow = 40 * time.Millisecond
	policy.ObserveInterval = 5 * time.Millisecond
	h := newHarness(t, model, policy)

	start := time.Now()
	rep, err := h.execute(restartProposal())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a non-converging action must report, not fail: %v", err)
	}
	if rep.Convergence != ConvergenceTimedOut {
		t.Fatalf("convergence = %s, want timed-out", rep.Convergence)
	}
	if rep.ConvergenceDetail == "" {
		t.Fatal("the report does not say what was actually seen during the window")
	}
	if !rep.Executed {
		t.Fatal("the action ran; a timed-out observation does not un-run it")
	}
	if got := h.mutator.callCount(); got != 1 {
		t.Fatalf("a non-converging action sent %d requests; watching must never re-drive the action", got)
	}
	// The bound is the property under test: a generous ceiling still fails a runner
	// that waits for convergence rather than for the window.
	if elapsed > 5*time.Second {
		t.Fatalf("the observation took %s against a %s window; it is not bounded", elapsed, policy.ObserveWindow)
	}
	if rep.ObservedFor <= 0 {
		t.Fatal("the report does not say how long the window was watched")
	}
}

// TestExecute_ObservationDistinguishesUnobservableFromNotConverged covers the
// second negative verdict. A cluster that becomes unreadable right after the action
// lands is a different situation from one that was read and had not changed, and
// collapsing the two would tell an operator "the fix did not take" when the truth is
// "nobody could see whether it took".
func TestExecute_ObservationDistinguishesUnobservableFromNotConverged(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	policy := fastPolicy()
	policy.ObserveWindow = 40 * time.Millisecond
	policy.ObserveInterval = 5 * time.Millisecond
	h := newHarness(t, model, policy)

	// Read 1 checks the preconditions and succeeds; every read after it fails.
	h.observer.failFrom = 2
	h.observer.err = errors.New("connection reset by peer")

	rep, err := h.execute(cordonProposal())
	if err != nil {
		t.Fatalf("an unobservable window must report, not fail: %v", err)
	}
	if rep.Convergence != ConvergenceUnobservable {
		t.Fatalf("convergence = %s (%s), want unobservable", rep.Convergence, rep.ConvergenceDetail)
	}
	if !rep.Executed || !rep.Recorded {
		t.Fatal("the action ran and was recorded; being unable to watch it does not change either")
	}
	if h.mutator.callCount() != 1 {
		t.Fatalf("an unobservable window re-drove the action: %d requests", h.mutator.callCount())
	}
}

// TestExecute_ARestartConvergesOnlyOnceANewRolloutAppears pins the reasoning in
// [rolloutRestartConverged]. A Deployment's status describes the PREVIOUS rollout in
// the moments after a restart is accepted, so replica counts alone report success
// immediately — before the deployment controller has done anything. This test fails
// against that mistake: the counts are complete from the very first read, and the
// verdict must still wait for a new revision to exist.
func TestExecute_ARestartConvergesOnlyOnceANewRolloutAppears(t *testing.T) {
	model := newClusterModel().withDeployment("shop", "web", 3, 4).withCrashLoopingPod("shop", "web-abc", "web-7d9")
	h := newHarness(t, model, fastPolicy())

	// The deployment controller only reacts on the third read of the window.
	h.observer.beforeRead = func(read int) {
		if read == 4 {
			model.rollOut("shop", "web", 5)
		}
	}

	rep, err := h.execute(restartProposal())
	if err != nil {
		t.Fatalf("executing a restart: %v", err)
	}
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s (%s), want converged", rep.Convergence, rep.ConvergenceDetail)
	}
	if !strings.Contains(rep.ConvergenceDetail, "revision 5") {
		t.Fatalf("the convergence detail does not name the new revision: %q", rep.ConvergenceDetail)
	}
	// Reads: one for the preconditions, then the observation polls. Convergence on the
	// fourth read proves the earlier ones did NOT report success against complete
	// replica counts and a stale revision.
	if model.readCount() < 4 {
		t.Fatalf("the cluster was read %d times; the restart converged before a new rollout could appear", model.readCount())
	}
	if h.mutator.callCount() != 1 {
		t.Fatalf("a restart sent %d requests, want 1", h.mutator.callCount())
	}
	sent := h.mutator.lastCall(t)
	if sent.RestartedAt == "" {
		t.Fatal("the restart carried no timestamp, so it would not trigger a rollout")
	}
}

// TestExecute_DeletingAPodConvergesWhenThePodIsGone covers the
// recreated-by-controller class end to end, including that its rollback is reported
// as impossible rather than merely absent.
func TestExecute_DeletingAPodConvergesWhenThePodIsGone(t *testing.T) {
	model := newClusterModel().withFailedPod("shop", "web-dead", "web-7d9")
	h := newHarness(t, model, fastPolicy())

	rep, err := h.execute(deletePodProposal())
	if err != nil {
		t.Fatalf("deleting a failed pod: %v", err)
	}
	if rep.Convergence != ConvergenceConverged {
		t.Fatalf("convergence = %s (%s), want converged", rep.Convergence, rep.ConvergenceDetail)
	}
	if rep.Rollback.Kind != RollbackImpossible {
		t.Fatalf("rollback kind = %s, want impossible — a deleted pod cannot be restored", rep.Rollback.Kind)
	}
	if rep.Rollback.Available {
		t.Fatal("the report offers a rollback for a deleted pod")
	}
	// The pre-state still records what was destroyed, which is the whole point of
	// capturing it for an action that cannot be undone.
	if controller, ok := rep.PreState.Field(fieldController); !ok || controller != "ReplicaSet/web-7d9" {
		t.Fatalf("pre-state controller = %q (present=%t), want the pod's controller recorded", controller, ok)
	}
}

// TestExecute_RefusesAnAuthorizationCarryingNoPreconditions proves an approval with
// nothing to re-check is refused rather than treated as unconditionally safe.
func TestExecute_RefusesAnAuthorizationCarryingNoPreconditions(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())

	p := cordonProposal()
	p.Preconditions = nil
	auth := authorizationFor(t, p)

	rep, err := h.runner.Execute(context.Background(), auth, p)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("expected a refusal, got: %v", err)
	}
	if rep.Failure != FailureRefused {
		t.Fatalf("failure = %s, want refused", rep.Failure)
	}
	if h.mutator.callCount() != 0 {
		t.Fatal("an action with nothing to re-check was sent anyway")
	}
}

// TestExecute_RecordsBeforeObserving pins the ordering that bounds the window in
// which a crash could lose the executed label. The recording must already have
// happened by the time the first convergence poll runs, so a process that dies
// mid-observation still leaves a trail that prevents a second execution.
func TestExecute_RecordsBeforeObserving(t *testing.T) {
	model := newClusterModel().withNode("node-a")
	h := newHarness(t, model, fastPolicy())

	recordedByFirstPoll := -1
	// Read 1 is the precondition read; read 2 is the first convergence poll.
	h.observer.beforeRead = func(read int) {
		if read == 2 {
			recordedByFirstPoll = h.recorder.count()
		}
	}

	if _, err := h.execute(cordonProposal()); err != nil {
		t.Fatalf("executing: %v", err)
	}
	if recordedByFirstPoll != 1 {
		t.Fatalf("the trail had %d entries when observation began; the execution must be recorded before the window, not after", recordedByFirstPoll)
	}
	if detail := h.recorder.last(); !strings.Contains(detail, "Convergence is being watched") {
		t.Fatalf("the trail note does not say the convergence verdict is still pending: %q", detail)
	}
}

// TestNew_RequiresEveryDependency proves none of the four can be left out. Two of
// them matter beyond testability: without the recorder nothing marks the approval
// artifact executed, so "exactly once" would quietly become "once per cycle"; and
// without the audit sink a mutation happens with no record of who authorized it,
// which fails silently and is therefore the worst kind of missing dependency.
func TestNew_RequiresEveryDependency(t *testing.T) {
	model := newClusterModel()
	mutator := newFakeMutator(model)
	observer := &fakeObserver{model: model}
	recorder := &fakeRecorder{}
	trail := audit.NewTrail()

	cases := map[string]struct {
		mutator  Mutator
		observer Observer
		recorder Recorder
		trail    audit.Sink
	}{
		"no write client": {nil, observer, recorder, trail},
		"no observer":     {mutator, nil, recorder, trail},
		"no recorder":     {mutator, observer, nil, trail},
		"no audit sink":   {mutator, observer, recorder, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(tc.mutator, tc.observer, tc.recorder, tc.trail, Policy{}); err == nil {
				t.Fatal("a runner was built with a missing dependency")
			}
		})
	}

	runner, err := New(mutator, observer, recorder, trail, Policy{})
	if err != nil {
		t.Fatalf("building a runner with every dependency: %v", err)
	}
	if runner.Cluster() != testCluster {
		t.Fatalf("runner cluster = %q, want %q", runner.Cluster(), testCluster)
	}
	if runner.Policy() != DefaultPolicy() {
		t.Fatalf("a zero policy resolved to %+v, want the shipped defaults %+v", runner.Policy(), DefaultPolicy())
	}
}

// containsSubstring reports whether any entry contains the substring.
func containsSubstring(entries []string, want string) bool {
	for _, e := range entries {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}
