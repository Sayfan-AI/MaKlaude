package health

import (
	"context"
	"time"
)

// DefaultInterval is the cadence at which [Monitor] collects a snapshot when no
// interval is configured.
const DefaultInterval = 60 * time.Second

// Result pairs a collected [Snapshot] with the error (if any) the collection
// returned. It is what a [Monitor] delivers to its sink on each tick, so a
// consumer sees both successful collections and collection failures without the
// loop having to stop on the first error.
type Result struct {
	// Snapshot is the collected snapshot. When Err is non-nil it is the zero
	// snapshot and should not be relied upon.
	Snapshot Snapshot

	// Err is the error returned by the collector for this tick, or nil on
	// success. Note that an unreachable cluster is reported as a successful
	// snapshot (with Reachability.Reachable false), not as an error here.
	Err error
}

// Monitor drives a [Collector] on a fixed interval, delivering each result to a
// sink callback. It is the simple scheduling layer on top of pure collection:
// the collection logic stays testable and deterministic, while the Monitor owns
// only the timing.
//
// The interval and clock-driven timing are configurable so tests can drive the
// loop without sleeping in real time, and the loop is fully cancellable via the
// context passed to [Monitor.Run].
type Monitor struct {
	collector *Collector
	interval  time.Duration

	// ticker builds the tick source. It is injectable so tests can supply a
	// channel they control instead of a real wall-clock ticker. It defaults to a
	// real [time.Ticker].
	ticker func(time.Duration) Ticker
}

// Ticker is the minimal tick source a [Monitor] consumes: a channel that emits
// on each interval and a Stop to release its resources. The standard
// [time.Ticker] satisfies it via [realTicker]; tests can supply a hand-driven
// implementation to advance time explicitly.
type Ticker interface {
	// C returns the channel on which ticks are delivered.
	C() <-chan time.Time
	// Stop halts the ticker and releases any associated resources.
	Stop()
}

// MonitorOption configures a [Monitor] at construction time.
type MonitorOption func(*Monitor)

// WithInterval sets the collection cadence. A non-positive interval is ignored,
// leaving [DefaultInterval] in place.
func WithInterval(d time.Duration) MonitorOption {
	return func(m *Monitor) {
		if d > 0 {
			m.interval = d
		}
	}
}

// WithTicker overrides the tick source factory. It exists so tests can drive
// the loop deterministically; a nil factory is ignored.
func WithTicker(f func(time.Duration) Ticker) MonitorOption {
	return func(m *Monitor) {
		if f != nil {
			m.ticker = f
		}
	}
}

// NewMonitor builds a [Monitor] that runs the given collector. By default it
// collects every [DefaultInterval] using a real wall-clock ticker; both are
// overridable via options.
func NewMonitor(collector *Collector, opts ...MonitorOption) *Monitor {
	m := &Monitor{
		collector: collector,
		interval:  DefaultInterval,
		ticker:    realTicker,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run collects snapshots on the monitor's interval until ctx is cancelled,
// delivering each [Result] to sink. It collects once immediately on entry so a
// caller need not wait a full interval for the first snapshot, then again on
// every tick.
//
// The sink is called synchronously on the loop's goroutine; a slow sink slows
// the loop rather than dropping results. A nil sink is treated as a no-op.
// Run returns the context's cancellation cause once the context is done.
func (m *Monitor) Run(ctx context.Context, sink func(Result)) error {
	if sink == nil {
		sink = func(Result) {}
	}

	deliver := func() {
		snap, err := m.collector.Collect(ctx)
		// A cancellation observed mid-collection is the caller stopping us, not a
		// signal worth delivering; let the loop exit cleanly instead.
		if ctx.Err() != nil {
			return
		}
		sink(Result{Snapshot: snap, Err: err})
	}

	// Collect immediately so the first snapshot is available without waiting a
	// full interval.
	deliver()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	t := m.ticker(m.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C():
			deliver()
		}
	}
}

// realTicker adapts the standard library's [time.Ticker] to the [Ticker]
// interface used by [Monitor].
func realTicker(d time.Duration) Ticker {
	return &stdTicker{t: time.NewTicker(d)}
}

type stdTicker struct {
	t *time.Ticker
}

func (s *stdTicker) C() <-chan time.Time { return s.t.C }
func (s *stdTicker) Stop()               { s.t.Stop() }
