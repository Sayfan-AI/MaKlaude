package execute

import (
	"context"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// Mutator is the write path this package drives: the scoped, per-action,
// precondition-carrying primitives of [kube.Executor], and nothing else.
//
// It is an interface rather than a *kube.Executor for one reason that matters more
// than testability: it makes the authority this package holds enumerable. Every
// mutating request MaKlaude can issue is one of the five methods below, so "what
// can the execution layer do to a cluster?" is answered by reading this interface
// rather than by auditing a concrete type that might grow a sixth method nobody
// noticed. A fake in a test is the same enumeration from the other side — it can
// record exactly what was attempted, and prove that an aborted action attempted
// nothing.
//
// Name and Mode are on the interface because both are safety checks this layer
// performs rather than assumes: the cluster must match the one the authorization
// names, and the kill switch is re-read at execution time rather than trusted from
// construction.
type Mutator interface {
	// Name is the registered name of the single cluster this mutator can reach.
	Name() string

	// Mode is the kill-switch posture the write path is currently in. It is read at
	// execution time, not cached: see [Runner.Execute].
	Mode() kube.ExecuteMode

	// RestartDeploymentRollout stamps the restart annotation onto a Deployment's pod
	// template, conditioned on resourceVersion.
	RestartDeploymentRollout(ctx context.Context, namespace, name, restartedAt, resourceVersion string) (*kube.Outcome, error)

	// PatchDeployment applies a strategic-merge patch to one Deployment, conditioned
	// on resourceVersion.
	PatchDeployment(ctx context.Context, namespace, name string, patch []byte, resourceVersion string) (*kube.Outcome, error)

	// CordonNode marks one node unschedulable, conditioned on resourceVersion.
	CordonNode(ctx context.Context, name, resourceVersion string) (*kube.Outcome, error)

	// PatchNode applies a strategic-merge patch to one node, conditioned on
	// resourceVersion. It is what an uncordon rollback travels through.
	PatchNode(ctx context.Context, name string, patch []byte, resourceVersion string) (*kube.Outcome, error)

	// DeletePod deletes one pod, conditioned on resourceVersion.
	DeletePod(ctx context.Context, namespace, name, resourceVersion string) (*kube.Outcome, error)
}

// Observer supplies the live view of the cluster this package checks itself
// against — before acting (do the preconditions still hold, what is the state I am
// about to change?) and after acting (did the change take?).
//
// It is deliberately the [health.Snapshot] layer rather than raw API reads. A
// snapshot is a judgment-free but *normalized* view: it has already resolved the
// facts this package would otherwise re-derive, and re-deriving them is precisely
// where a second implementation would drift from the first. The crashloop rule is
// the concrete case — a crashlooping container oscillates through Running and
// Terminated, so a fresh instantaneous check would disagree with the collector
// about half the time, and the precondition that stops MaKlaude restarting a
// recovered workload would be the thing that got it wrong. One collector, one
// answer.
//
// The method is named Collect so *[health.Collector] satisfies this interface
// directly, with no adapter to keep in sync.
type Observer interface {
	Collect(ctx context.Context) (health.Snapshot, error)
}

// Recorder is the durable single-execution enforcement this package must not be
// able to skip: after a real mutation lands, the approval artifact is marked
// executed, and no later pass will authorize it again.
//
// It is narrowed to the one gatekeeper method this package is allowed to call.
// Nothing here may open, refuse, or withdraw an approval — the gate decides, the
// runner acts and reports back — and an interface with exactly one method is the
// cheapest way to make that structural rather than conventional.
type Recorder interface {
	// RecordExecution posts the outcome onto the approval trail and applies the
	// executed label. See [approve.Gatekeeper.RecordExecution] for why it is called
	// after the action rather than before.
	RecordExecution(ctx context.Context, auth *approve.Authorization, detail string) error
}

// The production implementations, asserted at compile time. If any of these three
// stops satisfying its interface the build fails here rather than at a wiring site
// far away.
var (
	_ Mutator  = (*kube.Executor)(nil)
	_ Observer = (*health.Collector)(nil)
	_ Recorder = (*approve.Gatekeeper)(nil)
)
