package execute

import (
	"context"
	"fmt"
	"time"
)

// observation is the result of one bounded watch: the verdict, what was last seen,
// and how long the watching took.
type observation struct {
	verdict Convergence
	detail  string
	elapsed time.Duration
}

// convergencePredicate reports whether the cluster has reached the state an action
// was supposed to produce, and states what was actually seen either way.
type convergencePredicate func(idx *clusterIndex) (bool, string)

// observe watches the cluster until the predicate holds or the window runs out,
// whichever comes first, and then RETURNS — always.
//
// The bound is the contract. An action that has not converged is a thing to report,
// not a thing to wait for: the caller is a monitoring loop with other clusters to
// look at, and a remediation that is taking longer than expected is exactly the
// situation in which blocking the loop is worst. So there is no "wait until it
// works" mode, no way to configure an unbounded window (see [Policy]), and no path
// out of this function that is not one of the three [Convergence] verdicts.
//
// The first read happens immediately, before any sleep, so an action whose effect is
// instantaneous — cordoning sets a spec field no controller has to agree with —
// costs no wall-clock time at all.
//
// Read failures do not end the watch. A single failed list mid-window is far more
// likely to be a blip than a verdict, so the loop keeps trying and only reports
// [ConvergenceUnobservable] if it never managed a single successful read. That
// distinction is the point of having two negative verdicts: "we looked and it had
// not happened" and "we could not look" call for different responses from a human.
func (r *Runner) observe(ctx context.Context, converged convergencePredicate) observation {
	start := time.Now()
	var (
		sawCluster bool
		detail     string
	)

	for {
		snap, err := r.observer.Collect(ctx)
		switch {
		case err != nil:
			detail = fmt.Sprintf("the cluster could not be read: %v", err)
		case !snap.Reachability.Reachable:
			detail = fmt.Sprintf("the cluster is unreachable: %s", snap.Reachability.Error)
		default:
			sawCluster = true
			held, seen := converged(newClusterIndex(snap))
			detail = seen
			if held {
				return observation{verdict: ConvergenceConverged, detail: detail, elapsed: time.Since(start)}
			}
		}

		if !r.roomForAnotherRead(start) {
			break
		}
		if werr := sleepFor(ctx, r.policy.ObserveInterval); werr != nil {
			detail = fmt.Sprintf("%s (the watch was cut short: %v)", detail, werr)
			break
		}
	}

	if sawCluster {
		return observation{verdict: ConvergenceTimedOut, detail: detail, elapsed: time.Since(start)}
	}
	return observation{verdict: ConvergenceUnobservable, detail: detail, elapsed: time.Since(start)}
}

// roomForAnotherRead reports whether waiting one more interval would still land
// inside the window.
//
// The window is measured with a monotonic elapsed time rather than against a wall
// clock deadline, so a clock adjustment during the watch — an NTP step, a container
// resuming after a suspend — can neither extend the bound nor collapse it.
func (r *Runner) roomForAnotherRead(start time.Time) bool {
	return time.Since(start)+r.policy.ObserveInterval <= r.policy.ObserveWindow
}

// sleepFor waits for d, or until ctx is done, whichever comes first. It returns the
// context's error when it is the one that fired, so a caller can tell "the wait
// finished" from "the caller gave up" — which is the difference between a verdict
// and an interruption.
func sleepFor(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
