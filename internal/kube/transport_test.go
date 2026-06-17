package kube

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingRoundTripper records whether it was ever invoked and returns a
// canned 200 response. It lets the tests prove that a blocked request never
// reaches the inner transport (and thus never the network).
type recordingRoundTripper struct {
	called atomic.Bool
}

func (r *recordingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	r.called.Store(true)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("ok")),
		Header:     make(http.Header),
	}, nil
}

func newRequest(t *testing.T, method string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, "https://example.test/api/v1/namespaces", nil)
	if err != nil {
		t.Fatalf("building %s request: %v", method, err)
	}
	return req
}

// TestReadOnlyTransport_BlocksMutatingVerbs is the core proof of the read-only
// guarantee: every mutating verb is rejected with ErrWriteForbidden and the
// inner transport is never invoked — so no mutating request hits the network.
func TestReadOnlyTransport_BlocksMutatingVerbs(t *testing.T) {
	mutating := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodConnect,
		http.MethodTrace,
		"DELETECOLLECTION", // not a real HTTP verb, but proves the allowlist is closed
	}

	for _, method := range mutating {
		t.Run(method, func(t *testing.T) {
			inner := &recordingRoundTripper{}
			rt := newReadOnlyTransport(inner)

			resp, err := rt.RoundTrip(newRequest(t, method))
			if resp != nil {
				_ = resp.Body.Close()
				t.Fatalf("expected nil response for blocked request, got %+v", resp)
			}
			if err == nil {
				t.Fatalf("expected %s to be blocked, got nil error", method)
			}
			if !errors.Is(err, ErrWriteForbidden) {
				t.Fatalf("expected error to wrap ErrWriteForbidden, got: %v", err)
			}
			if inner.called.Load() {
				t.Fatalf("inner transport was called for blocked %s request — write reached the network", method)
			}
		})
	}
}

// TestReadOnlyTransport_AllowsReadVerbs verifies the allowlisted read verbs are
// delegated to the inner transport unchanged.
func TestReadOnlyTransport_AllowsReadVerbs(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		t.Run(method, func(t *testing.T) {
			inner := &recordingRoundTripper{}
			rt := newReadOnlyTransport(inner)

			resp, err := rt.RoundTrip(newRequest(t, method))
			if err != nil {
				t.Fatalf("expected %s to be allowed, got error: %v", method, err)
			}
			if resp == nil {
				t.Fatalf("expected a response for allowed %s request", method)
			}
			_ = resp.Body.Close()
			if !inner.called.Load() {
				t.Fatalf("inner transport was not called for allowed %s request", method)
			}
		})
	}
}

// TestReadOnlyTransport_EmptyMethodTreatedAsGet confirms an empty method
// (which net/http treats as GET) is allowed rather than rejected.
func TestReadOnlyTransport_EmptyMethodTreatedAsGet(t *testing.T) {
	inner := &recordingRoundTripper{}
	rt := newReadOnlyTransport(inner)

	req := newRequest(t, http.MethodGet)
	req.Method = "" // simulate a request with no explicit method

	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("expected empty method to be treated as GET, got error: %v", err)
	}
	_ = resp.Body.Close()
	if !inner.called.Load() {
		t.Fatal("inner transport was not called for empty-method request")
	}
}

// TestReadOnlyTransport_BlockEndToEnd wires the guard around a real
// httptest.Server transport to prove a mutating request is refused before the
// server is ever contacted.
func TestReadOnlyTransport_BlockEndToEnd(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Transport: newReadOnlyTransport(http.DefaultTransport)}

	// A GET should succeed and reach the server.
	getResp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET through guard failed: %v", err)
	}
	_ = getResp.Body.Close()

	// A POST must be blocked before the server is contacted.
	postResp, err := client.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("expected POST to be blocked")
	}
	if postResp != nil {
		_ = postResp.Body.Close()
	}
	if !errors.Is(err, ErrWriteForbidden) {
		// http.Client wraps transport errors in *url.Error; errors.Is must still
		// unwrap to our sentinel.
		t.Fatalf("expected blocked POST to wrap ErrWriteForbidden, got: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server was hit %d times; expected exactly 1 (the GET), POST must not reach the network", got)
	}
}
