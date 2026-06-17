package kube

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ErrWriteForbidden is the sentinel error returned by the read-only transport
// guard whenever a request carrying a mutating HTTP verb is attempted. It wraps
// every such rejection so callers (and tests) can detect a blocked write with
// errors.Is(err, ErrWriteForbidden).
//
// This is MaKlaude's read-only guarantee made structural: the guard sits at the
// very bottom of the client stack, so even a future caller mistake — or a code
// path that somehow obtains a writable client — cannot put a mutating request
// on the wire. The request is rejected before it ever reaches the network.
var ErrWriteForbidden = errors.New("read-only kube client: mutating request blocked")

// readOnlyMethods is the closed set of HTTP verbs the read-only client is
// permitted to send. The Kubernetes API surfaces every read verb (get, list,
// watch) over HTTP GET, so GET is the only method MaKlaude ever needs in a
// read-only posture. HEAD and OPTIONS are non-mutating discovery/metadata verbs
// and are allowed too; everything else (POST, PUT, PATCH, DELETE, CONNECT,
// TRACE, and any non-standard verb) is refused.
var readOnlyMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// readOnlyRoundTripper is an http.RoundTripper that enforces the read-only
// guarantee at the transport layer. It permits only the verbs in
// [readOnlyMethods] and fails every other request with [ErrWriteForbidden]
// before delegating to the wrapped transport — so a blocked write never touches
// the network.
//
// It holds no per-cluster state of its own and delegates all real work to the
// inner RoundTripper, so wrapping is side-effect free and clusters stay
// isolated: each client builds its own guard around its own transport.
type readOnlyRoundTripper struct {
	// inner is the underlying transport that performs the actual HTTP exchange
	// for permitted (read-only) requests.
	inner http.RoundTripper
}

// RoundTrip enforces the read-only verb allowlist. For a permitted verb it
// delegates to the inner transport unchanged; for any mutating verb it returns
// a wrapped [ErrWriteForbidden] without making a network call.
func (rt *readOnlyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	// An empty method defaults to GET per net/http semantics, so treat it as a
	// read. Any explicit method must be in the allowlist.
	if method == "" {
		method = http.MethodGet
	}
	if !readOnlyMethods[method] {
		return nil, fmt.Errorf("%w: refusing %s %s (allowed: %s)",
			ErrWriteForbidden, method, requestTarget(req), allowedMethodsList())
	}
	return rt.inner.RoundTrip(req)
}

// newReadOnlyTransport wraps inner in a read-only guard. If inner is nil it
// falls back to http.DefaultTransport so the guard is always backed by a usable
// transport.
func newReadOnlyTransport(inner http.RoundTripper) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &readOnlyRoundTripper{inner: inner}
}

// requestTarget returns a safe, log-friendly description of the request target
// (path only, never query/host) for use in error messages. It avoids leaking
// query parameters that could carry sensitive selectors or tokens.
func requestTarget(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "<unknown>"
	}
	if req.URL.Path == "" {
		return "/"
	}
	return req.URL.Path
}

// allowedMethodsList renders the permitted verbs as a stable, comma-separated
// string for error messages.
func allowedMethodsList() string {
	methods := make([]string, 0, len(readOnlyMethods))
	for m := range readOnlyMethods {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	return strings.Join(methods, ", ")
}
