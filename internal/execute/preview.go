package execute

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// ErrNotPreviewOnly is returned when [Preview] is handed a [Mutator] that is not in
// [kube.ExecuteDryRun] mode.
//
// It is a refusal rather than a warning because the caller's intent is unambiguous
// and getting it wrong is unrecoverable: a "preview" sent through an execution-enabled
// client is a real mutation that a human has not approved yet, performed by the code
// path whose entire job is to show them what would happen if they did. There is no
// degraded behavior worth having here.
var ErrNotPreviewOnly = errors.New("execute: a preview requires a dry-run-only write client")

// Preview sends the action a proposal describes as a server-side dry run and returns
// what the API server said.
//
// It is the evidence half of the approval gate. [approve.Preview] is supplied by the
// caller precisely so the approve package holds no cluster client (see its doc), which
// leaves the question of who actually sends the dry run — and the answer has to be
// this package, because this package owns [operationPlans], the single table that maps
// an operation to the one request it consists of. A preview built anywhere else would
// be a second implementation of that mapping, and the two would drift: the human would
// approve on the strength of a request that is not quite the request MaKlaude later
// sends. Reusing the table makes "the preview and the execution are the same action" a
// property of the code rather than of a reviewer's attention.
//
// It authorizes nothing and records nothing. A dry run is not a step in the approval
// sequence — it is information gathered before the sequence starts, which is why it
// takes no [approve.Authorization] and touches neither trail.
//
// at supplies the action's timestamp for the operations whose request embeds one (the
// restart annotation), for the same reason [plan.mutate] takes it rather than reading
// a clock: the preview a human is shown and the request eventually sent should differ
// in their dry-run marker and nothing else.
//
// The errors worth branching on are the caller's own:
//
//   - [ErrNotPreviewOnly] — the mutator can really write. Nothing was sent.
//   - [ErrUnsupportedOperation] — no plan, or a plan that refuses the operation
//     outright. Nothing was sent, and nothing ever would be.
//   - [ErrRefused] — the proposal does not carry a parameter the request cannot be
//     built without. Nothing was sent.
//   - [kube.ErrPreconditionConflict] / [kube.ErrRevisionNotFound] — the world moved
//     while we were looking. Expected, and the caller's cue to re-observe rather than
//     to report a failure.
func Preview(ctx context.Context, m Mutator, p remediate.Proposal, at time.Time) (*kube.Outcome, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: no write client", ErrRefused)
	}
	if mode := m.Mode(); mode != kube.ExecuteDryRun {
		return nil, fmt.Errorf("%w: the client is in %s mode", ErrNotPreviewOnly, mode)
	}

	pl, ok := planFor(p.Operation)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedOperation, p.Operation)
	}
	if pl.unsupported != "" {
		return nil, fmt.Errorf("%w: %s: %s", ErrUnsupportedOperation, p.Operation, pl.unsupported)
	}

	prm, err := resolveParams(pl, p.Preconditions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}

	out, err := pl.mutate(ctx, m, p.Target, prm, at)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, fmt.Errorf("%w: the preview of %s returned neither an outcome nor an error", ErrRefused, p.Operation)
	}
	// A dry-run scope that came back marked as a real mutation is the one result that
	// must not be reported as a successful preview. kube.WriteScope refuses a dry-run
	// request that lacks dryRun=All (ErrDryRunRequired), so reaching here means the
	// two layers disagree — and between "the executor says it was real" and "the mode
	// says it was a preview", the safe reading is the first.
	if !out.DryRun {
		return nil, fmt.Errorf("%w: the preview of %s on %s reported a REAL mutation",
			ErrNotPreviewOnly, p.Operation, p.Target.String())
	}
	return out, nil
}
