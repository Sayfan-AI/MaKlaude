package execute

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// previewAt is the fixed instant handed to [Preview], so the request a test inspects
// is byte-stable.
var previewAt = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// newPreviewer returns a mutator in dry-run mode over a cluster model holding every
// object the non-rollback fixtures target.
//
// The model has to be populated, not empty. A server-side dry run runs the full
// admission and precondition path and differs from a real request only in not
// persisting, so previewing an object that is not there returns 404 exactly as the
// real write would — which is faithful, and which an empty model would hide.
func newPreviewer() *fakeMutator {
	m := newFakeMutator(newClusterModel().
		withNode("node-a").
		withDeployment("shop", "web", 3, 5).
		withFailedPod("shop", "web-dead", "web-7d9"))
	m.mode = kube.ExecuteDryRun
	return m
}

// newRollbackPreviewer is newPreviewer over a model that HAS the revision the rollback
// proposal names. The rollback primitive is the only one that reads before it writes,
// so it is the only one whose preview needs a populated history to send anything at
// all — the same asymmetry [Mutator.RollbackDeploymentToRevision] documents.
func newRollbackPreviewer() *fakeMutator {
	m := newFakeMutator(rollbackModel())
	m.mode = kube.ExecuteDryRun
	return m
}

// TestPreview_SendsTheOperationAsADryRun checks the happy path: the request goes out,
// it is marked as a preview, and the cluster model is untouched.
func TestPreview_SendsTheOperationAsADryRun(t *testing.T) {
	m := newPreviewer()

	out, err := Preview(context.Background(), m, restartProposal(), previewAt)
	if err != nil {
		t.Fatalf("Preview returned error: %v", err)
	}
	if out == nil {
		t.Fatal("Preview returned neither an outcome nor an error")
	}
	if !out.DryRun {
		t.Error("the outcome is not marked as a dry run")
	}
	if m.callCount() != 1 {
		t.Fatalf("Preview sent %d requests, want exactly 1", m.callCount())
	}
}

// TestPreview_RefusesAWriteCapableClient is the safety assertion.
//
// A "preview" sent through an execution-enabled client is a real, unapproved mutation
// performed by the code path whose entire job is to show a human what would happen if
// they approved it. The refusal must land BEFORE anything is sent, so the test checks
// the call count rather than only the error.
func TestPreview_RefusesAWriteCapableClient(t *testing.T) {
	for _, mode := range []kube.ExecuteMode{kube.ExecuteEnabled, kube.ExecuteDisabled} {
		t.Run(mode.String(), func(t *testing.T) {
			m := newFakeMutator(newClusterModel())
			m.mode = mode

			out, err := Preview(context.Background(), m, restartProposal(), previewAt)
			if !errors.Is(err, ErrNotPreviewOnly) {
				t.Fatalf("Preview through a %s client returned %v, want ErrNotPreviewOnly", mode, err)
			}
			if out != nil {
				t.Errorf("Preview returned an outcome alongside the refusal: %+v", out)
			}
			if n := m.callCount(); n != 0 {
				t.Fatalf("Preview sent %d request(s) through a %s client; it must send none", n, mode)
			}
		})
	}
}

// TestPreview_RefusesAnOutcomeThatClaimsToBeReal covers the disagreement case: the mode
// says preview and the executor says it really mutated something.
//
// It cannot happen through [kube.Executor] — the scoped write client refuses a dry-run
// request that lacks dryRun=All — so this is a test of what happens if the two layers
// ever stop agreeing. The safe reading is the one that assumes a mutation landed.
func TestPreview_RefusesAnOutcomeThatClaimsToBeReal(t *testing.T) {
	m := &lyingMutator{mode: kube.ExecuteDryRun}

	out, err := Preview(context.Background(), m, restartProposal(), previewAt)
	if !errors.Is(err, ErrNotPreviewOnly) {
		t.Fatalf("Preview returned %v, want ErrNotPreviewOnly when the outcome says a real mutation landed", err)
	}
	if out != nil {
		t.Errorf("Preview returned an outcome it had just declared untrustworthy: %+v", out)
	}
}

// TestPreview_RefusesAnUnsupportedOperation checks that an operation with no plan is
// refused here for the same reason it is refused at execution: extending the catalog
// must not silently widen what MaKlaude sends to a cluster, at either layer.
func TestPreview_RefusesAnUnsupportedOperation(t *testing.T) {
	m := newPreviewer()
	p := restartProposal()
	p.Operation = remediate.Operation("frobnicate")

	if _, err := Preview(context.Background(), m, p, previewAt); !errors.Is(err, ErrUnsupportedOperation) {
		t.Fatalf("Preview of an unknown operation returned %v, want ErrUnsupportedOperation", err)
	}
	if n := m.callCount(); n != 0 {
		t.Errorf("Preview of an unknown operation sent %d request(s)", n)
	}
}

// TestPreview_RefusesAProposalMissingASendParameter checks the resolve step: a rollback
// whose preconditions do not name a revision cannot be built into a request, and saying
// so is better than sending something and finding out.
func TestPreview_RefusesAProposalMissingASendParameter(t *testing.T) {
	m := newRollbackPreviewer()
	p := revisionRollbackProposal()
	p.Preconditions = nil

	if _, err := Preview(context.Background(), m, p, previewAt); !errors.Is(err, ErrRefused) {
		t.Fatalf("Preview of a rollback with no revision precondition returned %v, want ErrRefused", err)
	}
	if n := m.callCount(); n != 0 {
		t.Errorf("Preview sent %d request(s) for a proposal it could not build", n)
	}
}

// TestPreview_PropagatesDrift checks that the two "the world moved" responses reach the
// caller intact, because the correct reaction to them is to re-observe rather than to
// report a failed action.
func TestPreview_PropagatesDrift(t *testing.T) {
	m := newPreviewer()
	m.err = kube.ErrPreconditionConflict

	if _, err := Preview(context.Background(), m, restartProposal(), previewAt); !errors.Is(err, kube.ErrPreconditionConflict) {
		t.Fatalf("Preview returned %v, want the precondition conflict to propagate", err)
	}
}

// TestPreview_RefusesANilMutator keeps the nil case an error rather than a panic in the
// one code path a caller reaches before any approval exists.
func TestPreview_RefusesANilMutator(t *testing.T) {
	if _, err := Preview(context.Background(), nil, restartProposal(), previewAt); !errors.Is(err, ErrRefused) {
		t.Fatalf("Preview(nil mutator) returned %v, want ErrRefused", err)
	}
}

// TestPreview_CoversEveryPlannedOperation asserts membership rather than a member: every
// operation the execution layer has a plan for must be previewable, so a new operation
// cannot ship with an execution path and no way to show a human what it would do.
func TestPreview_CoversEveryPlannedOperation(t *testing.T) {
	proposals := map[remediate.Operation]remediate.Proposal{
		remediate.OpRolloutRestart:   restartProposal(),
		remediate.OpCordonNode:       cordonProposal(),
		remediate.OpDeletePod:        deletePodProposal(),
		remediate.OpRollbackRevision: revisionRollbackProposal(),
	}

	for op, pl := range operationPlans {
		if pl.unsupported != "" {
			continue
		}
		p, ok := proposals[op]
		if !ok {
			t.Errorf("operation %q has an execution plan but no preview test case; add one to this map", op)
			continue
		}
		m := newPreviewer()
		if op == remediate.OpRollbackRevision {
			m = newRollbackPreviewer()
		}
		out, err := Preview(context.Background(), m, p, previewAt)
		if err != nil {
			t.Errorf("Preview of %s failed: %v", op, err)
			continue
		}
		if !out.DryRun {
			t.Errorf("Preview of %s produced an outcome not marked as a dry run", op)
		}
	}
}

// lyingMutator reports dry-run mode and returns outcomes that claim a real mutation
// landed. It exists only for TestPreview_RefusesAnOutcomeThatClaimsToBeReal.
type lyingMutator struct{ mode kube.ExecuteMode }

func (m *lyingMutator) Name() string           { return testCluster }
func (m *lyingMutator) Mode() kube.ExecuteMode { return m.mode }

func (m *lyingMutator) real() *kube.Outcome {
	return &kube.Outcome{Cluster: testCluster, Target: "deployment", Scope: "PATCH", ResourceVersion: "1", DryRun: false}
}

func (m *lyingMutator) RestartDeploymentRollout(context.Context, string, string, string, string) (*kube.Outcome, error) {
	return m.real(), nil
}

func (m *lyingMutator) PatchDeployment(context.Context, string, string, []byte, string) (*kube.Outcome, error) {
	return m.real(), nil
}

func (m *lyingMutator) RollbackDeploymentToRevision(context.Context, string, string, int64, string) (*kube.Outcome, error) {
	return m.real(), nil
}

func (m *lyingMutator) CordonNode(context.Context, string, string) (*kube.Outcome, error) {
	return m.real(), nil
}

func (m *lyingMutator) PatchNode(context.Context, string, []byte, string) (*kube.Outcome, error) {
	return m.real(), nil
}

func (m *lyingMutator) DeletePod(context.Context, string, string, string) (*kube.Outcome, error) {
	return m.real(), nil
}

var _ Mutator = (*lyingMutator)(nil)
