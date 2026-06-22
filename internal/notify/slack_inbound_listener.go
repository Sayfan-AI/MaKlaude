package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
)

// SocketStream is one open Socket Mode connection: a source of raw Slack event
// payloads. It is the narrow seam the Socket Mode loop reads through, abstracting
// away the WebSocket so the listener's loop logic is unit-tested with a fake that
// replays canned events and ZERO network. A live implementation wraps a real
// WebSocket dialed with the app-level token; that wrapper is a thin deployment
// concern, not a unit-test concern, which is exactly why the seam exists.
//
// Next returns the next raw event payload. It blocks until an event is available,
// returns ([io.EOF]) when the stream is cleanly closed, and any other error to
// signal a transport failure the loop should surface. Ack acknowledges an event
// by its envelope id (Socket Mode requires acking each events_api envelope);
// implementations that do not need acking may make it a no-op.
type SocketStream interface {
	Next(ctx context.Context) (payload []byte, err error)
	Close() error
}

// SocketDialer opens a [SocketStream] for Socket Mode. It is injected so a test
// supplies a fake stream and the live process supplies a real WebSocket dialer
// using the app-level token. Dial is given the [SlackConfig] so a live dialer can
// read the app token; it must NEVER log the token.
type SocketDialer interface {
	Dial(ctx context.Context, cfg SlackConfig) (SocketStream, error)
}

// InboundListener captures human replies in escalation threads and mirrors them
// to the audit trail, over either Socket Mode (the approved default) or the
// optional HTTP Events API. It is comms-only: see the safety boundary in
// [slack_inbound.go].
type InboundListener struct {
	cfg     SlackConfig
	proc    *InboundProcessor
	dialer  SocketDialer
	onError func(error)
}

// InboundOption customizes an [InboundListener].
type InboundOption func(*InboundListener)

// WithSocketDialer injects the Socket Mode dialer (a fake in tests, a real
// WebSocket dialer in production). When unset, [InboundListener.RunSocketMode]
// returns an error rather than dialing — a live deployment must supply one
// explicitly, keeping the WebSocket dependency out of this package's core.
func WithSocketDialer(d SocketDialer) InboundOption {
	return func(l *InboundListener) { l.dialer = d }
}

// WithErrorHandler injects a sink for non-fatal errors (a failed mirror of a
// single reply, say) so the listener can log-and-continue without coupling to a
// logger. When unset, such errors are silently dropped — the listener never
// crashes on a single bad event.
func WithErrorHandler(fn func(error)) InboundOption {
	return func(l *InboundListener) {
		if fn != nil {
			l.onError = fn
		}
	}
}

// NewInboundListener builds an inbound listener from cfg and the mirror that
// writes captured replies to the trail. ok is false when cfg is not
// [SlackConfig.Configured]: in that case the returned listener is nil and the
// caller starts NOTHING — no connection, no HTTP handler, no error — mirroring the
// outbound notifier's graceful-degradation seam so an unconfigured deployment is a
// pure no-op.
//
// A nil mirror with a configured cfg is a programming error and panics, exactly as
// [NewInboundProcessor] does: a configured listener with nowhere to mirror to is
// nonsensical.
func NewInboundListener(cfg SlackConfig, mirror ReplyMirror, opts ...InboundOption) (*InboundListener, bool) {
	if !cfg.Configured() {
		return nil, false
	}
	l := &InboundListener{
		cfg:     cfg,
		proc:    NewInboundProcessor(mirror),
		onError: func(error) {},
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, true
}

// RunSocketMode is the approved-default inbound loop: it dials a Socket Mode
// stream and processes events until the context is cancelled or the stream closes
// cleanly ([io.EOF]). A per-event mirror failure is routed to the error handler
// and the loop continues; only a transport-level stream error (or a dial failure)
// is returned. It requires a [SocketDialer] (see [WithSocketDialer]); without one
// it returns an error rather than guessing a transport.
//
// Safety: this loop only ever reads events and calls the mirror; there is no path
// from here to a cluster mutation.
func (l *InboundListener) RunSocketMode(ctx context.Context) error {
	if l.dialer == nil {
		return errNoDialer
	}
	stream, err := l.dialer.Dial(ctx, l.cfg)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	for {
		// Honor cancellation between events even if the stream blocks.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		payload, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if _, perr := l.proc.Process(ctx, payload); perr != nil {
			l.onError(perr)
		}
	}
}

// errNoDialer is returned by RunSocketMode when no [SocketDialer] was injected.
// It is a plain error (not a sentinel) since callers either supply a dialer or do
// not run Socket Mode at all.
var errNoDialer = errorString("notify/slack: Socket Mode requires a SocketDialer (see WithSocketDialer)")

// errorString is a tiny stdlib-only error type so this file needs no errors
// import beyond what it already uses; it mirrors the package's no-dependency
// posture.
type errorString string

func (e errorString) Error() string { return string(e) }

// readAllLimited reads up to limit bytes of an HTTP request body. It is defined
// here (rather than in slack_inbound.go) to keep the net/http import local to the
// transport files; the cap defends against a hostile caller.
func readAllLimited(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(io.LimitReader(r.Body, limit))
}
