package escalate

import "os"

// SinkFromEnv selects the comms sink for the running process based on the
// MAKLAUDE_GITHUB_* environment (see [GitHubConfig]). It is the single seam the
// monitor / e2e harness uses to obtain an [Escalator] without caring how comms
// are backed:
//
//   - When GitHub is configured, it returns a live [GitHubSink] and live=true.
//   - When it is NOT configured, it returns an in-memory [MemorySink] and
//     live=false, so the whole system degrades to a safe, side-effect-free
//     dry-run. Tests and credential-less e2e runs rely on exactly this.
//
// Returning the no-op sink (rather than nil) means callers never have to
// nil-check before escalating: NewEscalator(sink).Reconcile(...) is always
// valid, and when not configured it simply records to memory and discards.
func SinkFromEnv() (sink IssueSink, live bool) {
	cfg := GitHubConfigFromEnv(os.Getenv)
	if gh, ok := NewGitHubSink(cfg); ok {
		return gh, true
	}
	return NewMemorySink(), false
}

// EscalatorFromEnv is a convenience over [SinkFromEnv] that returns a ready
// [Escalator] plus whether it is backed by a live comms system. The monitor can
// call this once at startup and reuse the escalator across reconcile cycles.
func EscalatorFromEnv() (esc *Escalator, live bool) {
	sink, live := SinkFromEnv()
	return NewEscalator(sink), live
}
