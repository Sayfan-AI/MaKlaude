package kube

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newScopedRequest builds a request with the given method and raw URL for driving
// the scoped-write guard directly.
func newScopedRequest(t *testing.T, method, rawURL string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, nil) //nolint:noctx // no request is ever sent; the guard rejects or a stub transport answers.
	if err != nil {
		t.Fatalf("building %s %s: %v", method, rawURL, err)
	}
	return req
}

// countingTransport records how many requests reached it, so a test can prove a
// refusal happened BEFORE the network rather than merely that an error came back.
type countingTransport struct {
	calls   int
	lastURL string
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls++
	c.lastURL = req.URL.String()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

const (
	deployPath      = "/apis/apps/v1/namespaces/prod/deployments/web"
	otherDeployPath = "/apis/apps/v1/namespaces/prod/deployments/api"
	podPath         = "/api/v1/namespaces/prod/pods/web-abc"
	podCollection   = "/api/v1/namespaces/prod/pods"
)

// TestScopedWrite_AdmitsExactlyItsScope proves the pinned (method, path) pair is
// admitted and reaches the wire.
func TestScopedWrite_AdmitsExactlyItsScope(t *testing.T) {
	inner := &countingTransport{}
	rt := newScopedWriteTransport(inner, WriteScope{Method: http.MethodPatch, Path: deployPath})

	resp, err := rt.RoundTrip(newScopedRequest(t, http.MethodPatch, "https://api.test"+deployPath))
	if err != nil {
		t.Fatalf("in-scope PATCH refused: %v", err)
	}
	_ = resp.Body.Close()
	if inner.calls != 1 {
		t.Fatalf("expected the in-scope request to reach the inner transport once, got %d calls", inner.calls)
	}
}

// TestScopedWrite_RefusesOutOfScope walks the ways a mutating request can differ
// from its scope. Each case is a distinct way an executor bug or a compromised
// call site could reach an object nobody approved, so each is asserted
// separately rather than folded into one "wrong request" case.
func TestScopedWrite_RefusesOutOfScope(t *testing.T) {
	scope := WriteScope{Method: http.MethodPatch, Path: deployPath}

	cases := []struct {
		name   string
		method string
		path   string
		why    string
	}{
		{
			name:   "different object, same kind",
			method: http.MethodPatch,
			path:   otherDeployPath,
			why:    "authority over one deployment is not authority over its neighbour",
		},
		{
			name:   "different kind entirely",
			method: http.MethodPatch,
			path:   "/api/v1/nodes/node-a",
			why:    "a deployment scope must not reach a node",
		},
		{
			name:   "different verb on the approved object",
			method: http.MethodDelete,
			path:   deployPath,
			why:    "approving a patch is not approving a delete",
		},
		{
			name:   "POST to the approved object",
			method: http.MethodPost,
			path:   deployPath,
			why:    "create is not in the catalog",
		},
		{
			name:   "PUT to the approved object",
			method: http.MethodPut,
			path:   deployPath,
			why:    "a full replace is not the narrow patch that was approved",
		},
		{
			name:   "subresource of the approved object",
			method: http.MethodPatch,
			path:   deployPath + "/scale",
			why:    "exact-match scoping must not admit a subresource riding along on a prefix",
		},
		{
			name:   "collection containing the approved object",
			method: http.MethodDelete,
			path:   podCollection,
			why:    "a collection path is a prefix of every object under it; prefix matching would admit deletecollection",
		},
		{
			name:   "path traversal back to a sibling",
			method: http.MethodPatch,
			path:   deployPath + "/../api",
			why:    "a crafted path must not resolve into a different object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			_, err := rt.RoundTrip(newScopedRequest(t, tc.method, "https://api.test"+tc.path))
			if !errors.Is(err, ErrWriteOutOfScope) {
				t.Fatalf("expected ErrWriteOutOfScope (%s), got: %v", tc.why, err)
			}
			if inner.calls != 0 {
				t.Fatalf("refused request still reached the network (%d calls) — the guard must reject before the wire", inner.calls)
			}
		})
	}
}

// TestScopedWrite_ZeroValueScopeRefusesAllMutations proves the zero value is
// inert. A WriteScope that was never populated — by a construction bug, a missed
// branch, a future refactor — must fail closed rather than permit anything.
func TestScopedWrite_ZeroValueScopeRefusesAllMutations(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, WriteScope{})

			_, err := rt.RoundTrip(newScopedRequest(t, method, "https://api.test"+deployPath))
			if !errors.Is(err, ErrWriteOutOfScope) {
				t.Fatalf("zero-value scope admitted %s: %v", method, err)
			}
			if inner.calls != 0 {
				t.Fatalf("zero-value scope let %s reach the network", method)
			}
		})
	}
}

// TestScopedWrite_PartialScopeRefusesAllMutations proves a half-built scope (only
// a method, or only a path) is treated as no scope at all rather than as a
// wildcard over the missing half.
func TestScopedWrite_PartialScopeRefusesAllMutations(t *testing.T) {
	cases := map[string]WriteScope{
		"method without path": {Method: http.MethodPatch},
		"path without method": {Path: deployPath},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			_, err := rt.RoundTrip(newScopedRequest(t, http.MethodPatch, "https://api.test"+deployPath))
			if !errors.Is(err, ErrWriteOutOfScope) {
				t.Fatalf("partial scope admitted a mutation: %v", err)
			}
			if inner.calls != 0 {
				t.Fatalf("partial scope let a mutation reach the network")
			}
		})
	}
}

// TestScopedWrite_AllowsReads proves read verbs pass unconditionally, including
// against objects the scope does not name. An executor legitimately needs to read
// — fetch a Deployment's ReplicaSets to find the previous revision, re-read an
// object to evaluate a precondition — and reads are already bounded by the
// identity's least-privilege RBAC.
func TestScopedWrite_AllowsReads(t *testing.T) {
	scope := WriteScope{Method: http.MethodPatch, Path: deployPath}

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, ""} {
		name := method
		if name == "" {
			name = "empty-method-defaults-to-GET"
		}
		t.Run(name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			req := newScopedRequest(t, http.MethodGet, "https://api.test/apis/apps/v1/namespaces/prod/replicasets")
			req.Method = method

			resp, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("read verb %q refused: %v", method, err)
			}
			_ = resp.Body.Close()
			if inner.calls != 1 {
				t.Fatalf("read verb %q did not reach the inner transport", method)
			}
		})
	}
}

// TestScopedWrite_DryRunScopeRequiresServerDryRun proves a preview-only scope
// refuses a mutating request that lacks dryRun=All. This is what makes "a preview
// cannot mutate" a property of the transport rather than a promise that every call
// site remembered to pass an option.
func TestScopedWrite_DryRunScopeRequiresServerDryRun(t *testing.T) {
	scope := WriteScope{Method: http.MethodPatch, Path: deployPath, RequireDryRun: true}

	// Values Kubernetes does NOT recognise as a dry run. The API server ignores an
	// unrecognised dryRun value and executes for real, so treating any of these as
	// "dry run requested" is precisely the mismatch this guard exists to prevent.
	refused := map[string]string{
		"no dryRun at all":        "",
		"lowercase all":           "?dryRun=all",
		"boolean-ish true":        "?dryRun=true",
		"empty value":             "?dryRun=",
		"unknown strategy":        "?dryRun=Server",
		"duplicated, one correct": "?dryRun=All&dryRun=none",
	}
	for name, query := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			_, err := rt.RoundTrip(newScopedRequest(t, http.MethodPatch, "https://api.test"+deployPath+query))
			if !errors.Is(err, ErrDryRunRequired) {
				t.Fatalf("expected ErrDryRunRequired for %q, got: %v", query, err)
			}
			if inner.calls != 0 {
				t.Fatalf("a non-dry-run mutation reached the network under a dry-run-only scope")
			}
		})
	}

	t.Run("admits dryRun=All", func(t *testing.T) {
		inner := &countingTransport{}
		rt := newScopedWriteTransport(inner, scope)

		resp, err := rt.RoundTrip(newScopedRequest(t, http.MethodPatch, "https://api.test"+deployPath+"?dryRun=All"))
		if err != nil {
			t.Fatalf("dryRun=All refused under a dry-run-only scope: %v", err)
		}
		_ = resp.Body.Close()
		if inner.calls != 1 {
			t.Fatalf("dryRun=All did not reach the inner transport")
		}
	})
}

// TestScopedWrite_DeleteDryRunIsReadFromTheBody covers the asymmetry that makes
// this guard non-trivial: a DELETE carries DeleteOptions in its BODY, and the
// apiserver decodes options from the body when one is present and only falls back
// to the query when it is absent. So a query dryRun on a DELETE-with-body is
// ignored by the server — and a guard that checked the query would report a
// preview while the object was really deleted.
func TestScopedWrite_DeleteDryRunIsReadFromTheBody(t *testing.T) {
	scope := WriteScope{Method: http.MethodDelete, Path: podPath, RequireDryRun: true}

	cases := []struct {
		name    string
		query   string
		body    string
		admit   bool
		because string
	}{
		{
			name:    "body asks for dryRun All",
			body:    `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"resourceVersion":"1234"},"dryRun":["All"]}`,
			admit:   true,
			because: "this is what a preview delete looks like on the wire",
		},
		{
			name:    "query asks but body does not",
			query:   "?dryRun=All",
			body:    `{"kind":"DeleteOptions","apiVersion":"v1","preconditions":{"resourceVersion":"1234"}}`,
			because: "the server reads the body and ignores the query, so this request would really delete",
		},
		{
			name:    "body omits dryRun",
			body:    `{"kind":"DeleteOptions","apiVersion":"v1"}`,
			because: "a missing marker is not a dry run",
		},
		{
			name:    "body has an unrecognised value",
			body:    `{"kind":"DeleteOptions","apiVersion":"v1","dryRun":["all"]}`,
			because: "Kubernetes ignores an unrecognised dryRun value and executes for real",
		},
		{
			name:    "body has extra strategies alongside All",
			body:    `{"kind":"DeleteOptions","apiVersion":"v1","dryRun":["All","None"]}`,
			because: "the request's meaning would depend on which value the server reads",
		},
		{
			name:    "body is empty",
			body:    ``,
			because: "an unparseable body must fail closed",
		},
		{
			name:    "body is not JSON",
			body:    "\x00\x01binary",
			because: "a body the guard cannot read is a body it must not vouch for",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			req := newScopedRequest(t, http.MethodDelete, "https://api.test"+podPath+tc.query)
			req.Body = io.NopCloser(strings.NewReader(tc.body))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.body)), nil }

			_, err := rt.RoundTrip(req)
			if tc.admit {
				if err != nil {
					t.Fatalf("refused a genuine preview delete (%s): %v", tc.because, err)
				}
				if inner.calls != 1 {
					t.Fatalf("a genuine preview delete did not reach the inner transport")
				}
				return
			}
			if !errors.Is(err, ErrDryRunRequired) {
				t.Fatalf("expected ErrDryRunRequired (%s), got: %v", tc.because, err)
			}
			if inner.calls != 0 {
				t.Fatalf("a non-preview delete reached the network (%s)", tc.because)
			}
		})
	}
}

// TestScopedWrite_DeleteWithoutARetrievableBodyIsRefused proves the body check
// fails closed when the request offers no way to re-read its body. Inspecting a
// request must never consume the body that is about to be sent, so the guard uses
// GetBody — and a request without one gets refused rather than trusted.
func TestScopedWrite_DeleteWithoutARetrievableBodyIsRefused(t *testing.T) {
	scope := WriteScope{Method: http.MethodDelete, Path: podPath, RequireDryRun: true}

	cases := map[string]func(*http.Request){
		"no body at all": func(*http.Request) {},
		"body without GetBody": func(req *http.Request) {
			req.Body = io.NopCloser(strings.NewReader(`{"dryRun":["All"]}`))
			req.GetBody = nil
		},
		"GetBody errors": func(req *http.Request) {
			req.Body = io.NopCloser(strings.NewReader(`{"dryRun":["All"]}`))
			req.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("boom") }
		},
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			inner := &countingTransport{}
			rt := newScopedWriteTransport(inner, scope)

			req := newScopedRequest(t, http.MethodDelete, "https://api.test"+podPath)
			mangle(req)

			if _, err := rt.RoundTrip(req); !errors.Is(err, ErrDryRunRequired) {
				t.Fatalf("expected ErrDryRunRequired, got: %v", err)
			}
			if inner.calls != 0 {
				t.Fatal("an unverifiable delete reached the network")
			}
		})
	}
}

// TestScopedWrite_BodyInspectionDoesNotConsumeTheRequest proves the guard's body
// read is side-effect free: the request it admits still carries its full body for
// the transport underneath to send.
func TestScopedWrite_BodyInspectionDoesNotConsumeTheRequest(t *testing.T) {
	const body = `{"kind":"DeleteOptions","apiVersion":"v1","dryRun":["All"]}`

	var served string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		served = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := newScopedWriteTransport(srv.Client().Transport,
		WriteScope{Method: http.MethodDelete, Path: podPath, RequireDryRun: true})
	client := &http.Client{Transport: rt}

	req, err := http.NewRequest(http.MethodDelete, srv.URL+podPath, strings.NewReader(body)) //nolint:noctx // the test drives the transport directly.
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("preview delete refused: %v", err)
	}
	_ = resp.Body.Close()

	if served != body {
		t.Fatalf("the server received %q, want the full original body %q — inspection must not consume it", served, body)
	}
}

// TestScopedWrite_NonDryRunScopeAllowsDryRun proves an enabled scope still admits
// a dry run: previewing under a scope that permits real execution is harmless, so
// the requirement is one-directional.
func TestScopedWrite_NonDryRunScopeAllowsDryRun(t *testing.T) {
	inner := &countingTransport{}
	rt := newScopedWriteTransport(inner, WriteScope{Method: http.MethodDelete, Path: podPath})

	resp, err := rt.RoundTrip(newScopedRequest(t, http.MethodDelete, "https://api.test"+podPath+"?dryRun=All"))
	if err != nil {
		t.Fatalf("dry run refused under an execute-enabled scope: %v", err)
	}
	_ = resp.Body.Close()
	if inner.calls != 1 {
		t.Fatalf("dry run did not reach the inner transport")
	}
}

// TestScopedWrite_RefusalIsBeforeTheNetworkEndToEnd drives the guard over a real
// httptest.Server transport, proving an out-of-scope mutation never produces a
// request the server sees — the same end-to-end shape the read-only guard's own
// test uses.
func TestScopedWrite_RefusalIsBeforeTheNetworkEndToEnd(t *testing.T) {
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := newScopedWriteTransport(srv.Client().Transport, WriteScope{Method: http.MethodPatch, Path: deployPath})
	client := &http.Client{Transport: rt}

	req := newScopedRequest(t, http.MethodDelete, srv.URL+podPath)
	_, err := client.Do(req) //nolint:bodyclose // the guard refuses before a response exists.
	if err == nil {
		t.Fatal("expected the out-of-scope DELETE to fail")
	}
	if !strings.Contains(err.Error(), ErrWriteOutOfScope.Error()) {
		t.Fatalf("expected an out-of-scope refusal, got: %v", err)
	}
	if served != 0 {
		t.Fatalf("the server saw %d requests; an out-of-scope mutation must never be sent", served)
	}
}

// TestWriteScope_StringIsLogSafeAndDescribesTheScope proves the rendered scope
// names the method, path, and preview posture (it lands in escalations and the
// audit trail) and never carries a query string, which is where selectors and any
// token-bearing parameters would live.
func TestWriteScope_StringIsLogSafeAndDescribesTheScope(t *testing.T) {
	cases := []struct {
		name  string
		scope WriteScope
		want  string
	}{
		{"no scope", WriteScope{}, "<no mutating scope>"},
		{"execute", WriteScope{Method: http.MethodPatch, Path: deployPath}, "PATCH " + deployPath},
		{"dry run", WriteScope{Method: http.MethodDelete, Path: podPath, RequireDryRun: true}, "DELETE " + podPath + " (dry-run only)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.scope.String(); got != tc.want {
				t.Fatalf("scope rendered %q, want %q", got, tc.want)
			}
		})
	}
}
