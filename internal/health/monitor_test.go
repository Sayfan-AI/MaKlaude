package health

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// manualTicker is a hand-driven Ticker: tests push ticks onto its channel
// explicitly, so the monitor loop advances exactly when the test says, without
// any real-time sleeping.
type manualTicker struct {
	ch chan time.Time
}

func (m *manualTicker) C() <-chan time.Time { return m.ch }
func (m *manualTicker) Stop()               {}

// TestMonitor_Defaults proves the monitor's zero-config defaults are the agreed
// 60-second interval.
func TestMonitor_Defaults(t *testing.T) {
	col := newTestCollector(t)
	m := NewMonitor(col)
	if m.interval != DefaultInterval {
		t.Fatalf("expected default interval %v, got %v", DefaultInterval, m.interval)
	}
	if DefaultInterval != 60*time.Second {
		t.Fatalf("expected DefaultInterval to be 60s, got %v", DefaultInterval)
	}
}

// TestMonitor_CollectsImmediatelyThenOnEachTick proves the loop delivers a
// snapshot at once on entry and then one per tick, all without sleeping in real
// time, driven by a hand-controlled ticker.
func TestMonitor_CollectsImmediatelyThenOnEachTick(t *testing.T) {
	col := newTestCollector(t)
	tick := &manualTicker{ch: make(chan time.Time, 1)}
	m := NewMonitor(col,
		WithInterval(10*time.Millisecond),
		WithTicker(func(time.Duration) Ticker { return tick }),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	results := make(chan Result, 8)
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx, func(r Result) { results <- r })
	}()

	// First delivery happens immediately, before any tick.
	first := <-results
	if !first.Snapshot.Reachability.Reachable || first.Err != nil {
		t.Fatalf("unexpected first result: %+v", first)
	}

	// Each tick yields exactly one more delivery.
	for i := 0; i < 3; i++ {
		tick.ch <- time.Now()
		r := <-results
		if r.Err != nil {
			t.Fatalf("tick %d returned error: %v", i, r.Err)
		}
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected Run to return the context cancellation error")
	}
}

// TestMonitor_CancelStopsLoop proves cancelling the context before any tick
// still produces the immediate snapshot and then returns promptly.
func TestMonitor_CancelStopsLoop(t *testing.T) {
	col := newTestCollector(t)
	tick := &manualTicker{ch: make(chan time.Time)}
	m := NewMonitor(col, WithTicker(func(time.Duration) Ticker { return tick }))

	ctx, cancel := context.WithCancel(context.Background())
	var got []Result
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx, func(r Result) { got = append(got, r); cancel() })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error from Run")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one immediate delivery, got %d", len(got))
	}
}

// TestMonitor_NilSink proves a nil sink is tolerated (treated as a no-op) and
// the loop still runs and stops cleanly.
func TestMonitor_NilSink(t *testing.T) {
	cs := fake.NewSimpleClientset()
	col := NewCollector(kube.NewClientWithInterface("fixture", cs), WithClock(fixedClock()))
	tick := &manualTicker{ch: make(chan time.Time)}
	m := NewMonitor(col, WithTicker(func(time.Duration) Ticker { return tick }))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run with nil sink did not return after cancellation")
	}
}
