package execute

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// send performs one mutating request, retrying only within the narrow class defined
// by [isRetryable] and never more than [Policy.MaxAttempts] times in total.
//
// It returns the number of attempts alongside the result, because "how many
// mutating requests did this action produce?" belongs in the report rather than in a
// log: the promise this milestone makes is that a failure does not thrash, and a
// promise that can only be checked by reading logs is one nobody checks.
func (r *Runner) send(ctx context.Context, do func(context.Context) (*kube.Outcome, error)) (*kube.Outcome, int, error) {
	var lastErr error
	for attempt := 1; attempt <= r.policy.MaxAttempts; attempt++ {
		out, err := do(ctx)
		if err == nil {
			return out, attempt, nil
		}
		lastErr = err

		if !isRetryable(err) || attempt == r.policy.MaxAttempts {
			return nil, attempt, err
		}
		if werr := sleepFor(ctx, r.policy.RetryBackoff); werr != nil {
			// The caller gave up mid-backoff. Both errors are reported: the one that
			// caused the retry explains what went wrong, and the interruption explains
			// why nothing else was tried.
			return nil, attempt, errors.Join(err, werr)
		}
	}
	return nil, r.policy.MaxAttempts, lastErr
}

// isRetryable reports whether a failed mutation may be sent again.
//
// The class is deliberately tiny — one API server response — and the exclusions are
// the substance of this function, so they are stated rather than left implicit:
//
//   - A precondition conflict is NOT retryable. It is the healthy outcome of a stale
//     approval: the target moved, the approved action and the possible action are no
//     longer the same action, and the correct response is to let the next cycle
//     re-propose against the state that exists now. Retrying could only succeed by
//     abandoning the precondition, which is the one thing that must never happen.
//
//   - A timeout, a connection reset, or a 500 is NOT retryable, even though all three
//     are transient in shape. They share the property that matters: the outcome is
//     UNKNOWN. The request may have been applied and the acknowledgement lost, and a
//     mutation whose outcome is unknown must never be repeated. (The resourceVersion
//     precondition would in fact reject a duplicate — the version moves when the first
//     one lands — but that turns a successful action into a reported conflict, which
//     is a worse report than the truthful "this failed and I do not know whether it
//     landed".)
//
//   - An RBAC denial, an admission rejection, or a validation error is NOT retryable:
//     nothing about them improves by being asked twice, and each is a thing a human
//     needs to see rather than a thing to grind against.
//
// What is left is 429 Too Many Requests, which the API server's priority-and-fairness
// machinery returns having REJECTED the request outright — definitively unapplied,
// definitively worth trying again. It is the only response that is both certainly
// safe to repeat and plausibly different next time.
func isRetryable(err error) bool {
	if errors.Is(err, kube.ErrPreconditionConflict) {
		return false
	}
	return apierrors.IsTooManyRequests(err)
}
