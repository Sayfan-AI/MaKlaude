package execute

import "time"

// Default bounds for the two things this package must never do without a limit:
// wait, and try again.
const (
	// DefaultObserveWindow is how long convergence is watched before the runner
	// reports what it saw and returns. Ninety seconds is chosen against the slowest
	// action in the catalog: a rolling restart of a Deployment with a readiness probe
	// typically completes inside a minute, so the window is long enough that
	// "converged" is the usual verdict, and short enough that a caller polling
	// clusters on a cycle is never blocked by one action.
	//
	// Whatever it is set to, the window is a BOUND and not a requirement:
	// [ConvergenceTimedOut] is a report, not a failure of the action, and never
	// triggers another request.
	DefaultObserveWindow = 90 * time.Second

	// DefaultObserveInterval is how often the cluster is re-read inside the window.
	// Five seconds is well inside the API server's ability to serve a list and far
	// enough apart that an 18-read window costs nothing worth measuring.
	DefaultObserveInterval = 5 * time.Second

	// DefaultMaxAttempts is how many mutating requests one action may produce,
	// INCLUDING the first. Three is a bound rather than a tuning: the retryable class
	// is deliberately tiny (see [isRetryable]), so a second attempt is already
	// unusual and a fourth would say more about the cluster than about the action.
	DefaultMaxAttempts = 3

	// DefaultRetryBackoff is the fixed pause between attempts. It is fixed rather
	// than exponential on purpose: with a cap of three attempts, exponential backoff
	// is two extra parameters that change nothing about the worst case.
	DefaultRetryBackoff = 2 * time.Second
)

// Policy is the operator-tunable part of execution: how long to watch, how often,
// and how many times to try.
//
// Every knob is a [time.Duration] or a count, and every one of them has a shipped
// default that a zero value falls back to — the same contract [approve.Policy]
// carries, for the same reason. A forgotten field must behave like a configured
// one, because both readings of zero are wrong in ways that are hard to notice: a
// zero observation window read literally would report every action as unobserved,
// and a zero attempt count read literally would execute nothing at all while
// looking like it tried.
//
// There is deliberately NO knob for "do not observe". The one case where observing
// is meaningless — a dry run, where the cluster did not change — is derived from
// the write path's mode rather than configured, so it cannot be set wrong and
// cannot be forgotten. And there is no way to express an unbounded window: the
// bound is the property the issue this implements asks for, so it is not
// negotiable by configuration.
type Policy struct {
	// ObserveWindow bounds how long convergence is watched after a real mutation.
	// Zero or negative takes [DefaultObserveWindow].
	ObserveWindow time.Duration

	// ObserveInterval is how long to wait between reads inside the window. Zero or
	// negative takes [DefaultObserveInterval]. An interval longer than the window is
	// harmless: the window is always checked at least once, immediately.
	ObserveInterval time.Duration

	// MaxAttempts bounds how many mutating requests one action may produce, including
	// the first. Zero or negative takes [DefaultMaxAttempts]; it is clamped to at
	// least 1, because "attempt the action zero times" is not a posture this package
	// offers — that is what the kill switch is for.
	MaxAttempts int

	// RetryBackoff is the pause between attempts. Zero or negative takes
	// [DefaultRetryBackoff].
	RetryBackoff time.Duration
}

// DefaultPolicy returns the shipped policy.
func DefaultPolicy() Policy {
	return Policy{
		ObserveWindow:   DefaultObserveWindow,
		ObserveInterval: DefaultObserveInterval,
		MaxAttempts:     DefaultMaxAttempts,
		RetryBackoff:    DefaultRetryBackoff,
	}
}

// normalized returns the policy with every unset or nonsensical field replaced by
// its default, so the rest of the package can read the fields directly and no call
// site has to remember which ones can be zero.
func (p Policy) normalized() Policy {
	if p.ObserveWindow <= 0 {
		p.ObserveWindow = DefaultObserveWindow
	}
	if p.ObserveInterval <= 0 {
		p.ObserveInterval = DefaultObserveInterval
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.RetryBackoff <= 0 {
		p.RetryBackoff = DefaultRetryBackoff
	}
	return p
}
