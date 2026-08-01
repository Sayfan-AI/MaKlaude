package kube

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Sentinel errors produced by the scoped-write transport guard. Every rejection
// wraps one of these so callers, tests, and the audit trail can branch on the
// refusal class with errors.Is.
var (
	// ErrWriteOutOfScope is returned when a mutating request does not match the
	// single (method, path) pair its [WriteScope] pins. It is the executor's
	// structural counterpart to [ErrWriteForbidden]: the read-only guard refuses
	// ALL mutations, this one refuses every mutation except the exact one an
	// approved action needs.
	ErrWriteOutOfScope = errors.New("scoped write client: request outside approved scope")

	// ErrDryRunRequired is returned when a mutating request is attempted under a
	// dry-run-only scope without the server-side dryRun=All parameter. It makes
	// "a preview cannot mutate" a property of the transport rather than a promise
	// that every call site remembered to pass an option.
	ErrDryRunRequired = errors.New("scoped write client: dry-run scope requires dryRun=All")
)

// WriteScope is the complete, single-action authority a write-capable client
// carries: one mutating HTTP method, one exact API path, and whether the request
// must be a server-side dry run.
//
// It is deliberately a whole-request pin rather than a verb allowlist. A client
// scoped to "PATCH /apis/apps/v1/namespaces/prod/deployments/web" cannot patch a
// different deployment, cannot patch a node, cannot delete anything, and cannot
// delete the collection — not because no code path asks it to, but because the
// transport refuses the request before it reaches the network. That is what makes
// "narrowed to the exact target" a structural claim: the authority to act on one
// object is not the authority to act on its neighbours.
//
// The zero value is inert: it permits reads and refuses every mutating request.
// So a WriteScope that was never populated — by a construction bug, a missed
// branch, a future refactor — fails closed.
type WriteScope struct {
	// Method is the single mutating HTTP method permitted (http.MethodPatch,
	// http.MethodPut, http.MethodDelete, http.MethodPost). Empty means no
	// mutating method is permitted at all.
	Method string

	// Path is the exact request path that Method may target, compared verbatim
	// against the outgoing request's URL path. Empty means no mutating request is
	// permitted at all.
	//
	// It is an exact match, not a prefix or pattern, for two reasons. A prefix
	// would let a subresource ride along ("…/deployments/web" would admit
	// "…/deployments/web/scale"), and a collection path is a strict prefix of
	// every object path under it, so prefix matching would silently admit the
	// collection delete this catalog exists to exclude.
	Path string

	// RequireDryRun makes the scope preview-only: a mutating request that matches
	// Method and Path is still refused unless it carries dryRun=All, so the API
	// server validates and admits the change and then discards it.
	RequireDryRun bool
}

// isMutating reports whether the scope permits any mutating request at all. A
// scope missing either half of the (method, path) pin permits none.
func (s WriteScope) isMutating() bool {
	return s.Method != "" && s.Path != ""
}

// String renders the scope for errors, logs, and the audit trail. It is safe to
// log: it carries a method and a path (which name a cluster object the operator
// already approved acting on) and never a query string, which is where the API
// server's field/label selectors and any token-bearing parameters would live.
func (s WriteScope) String() string {
	if !s.isMutating() {
		return "<no mutating scope>"
	}
	if s.RequireDryRun {
		return s.Method + " " + s.Path + " (dry-run only)"
	}
	return s.Method + " " + s.Path
}

// scopedWriteRoundTripper is an http.RoundTripper that enforces a [WriteScope].
//
// It is a deliberate sibling of [readOnlyRoundTripper] rather than a
// generalisation of it. The observation path keeps its own guard, unchanged and
// unparameterised — there is no mode, flag, or scope that turns a read-only
// client into a writing one, because the two guards are different types installed
// by different constructors. Loosening one cannot loosen the other.
//
// Read verbs pass through unconditionally. The scope exists to bound MUTATION;
// reads are already bounded by the least-privilege RBAC the identity carries, and
// an executor legitimately needs to read (fetch a Deployment's ReplicaSets to
// find the previous revision, re-read an object to evaluate a precondition)
// before it acts.
type scopedWriteRoundTripper struct {
	// inner performs the actual HTTP exchange for admitted requests.
	inner http.RoundTripper

	// scope is the single mutating request this transport will pass. It is set at
	// construction and never mutated, so the transport is safe for concurrent use
	// and its authority cannot widen after the fact.
	scope WriteScope
}

// RoundTrip admits read verbs unchanged, admits exactly the one mutating request
// its scope pins, and refuses everything else without making a network call.
func (rt *scopedWriteRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	method := normalizeMethod(req)
	if readOnlyMethods[method] {
		return rt.inner.RoundTrip(req)
	}

	if !rt.scope.isMutating() {
		return nil, fmt.Errorf("%w: refusing %s %s (no mutating scope granted)",
			ErrWriteOutOfScope, method, requestTarget(req))
	}
	if method != rt.scope.Method {
		return nil, fmt.Errorf("%w: refusing %s %s (scope allows %s)",
			ErrWriteOutOfScope, method, requestTarget(req), rt.scope.String())
	}
	if requestTarget(req) != rt.scope.Path {
		return nil, fmt.Errorf("%w: refusing %s %s (scope allows %s)",
			ErrWriteOutOfScope, method, requestTarget(req), rt.scope.String())
	}
	if rt.scope.RequireDryRun && !hasServerDryRun(req) {
		return nil, fmt.Errorf("%w: refusing %s %s", ErrDryRunRequired, method, requestTarget(req))
	}

	return rt.inner.RoundTrip(req)
}

// newScopedWriteTransport wraps inner in a scoped-write guard. If inner is nil it
// falls back to http.DefaultTransport so the guard is always backed by a usable
// transport.
func newScopedWriteTransport(inner http.RoundTripper, scope WriteScope) http.RoundTripper {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &scopedWriteRoundTripper{inner: inner, scope: scope}
}

// normalizeMethod returns the request's HTTP method in the canonical upper-case
// form, treating an empty method as GET per net/http semantics.
func normalizeMethod(req *http.Request) string {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		return http.MethodGet
	}
	return method
}

// hasServerDryRun reports whether the request asks the API server for a dry run,
// checking wherever the Kubernetes API actually reads the marker for that verb.
//
// That location is not uniform, and getting it wrong would make this guard verify
// something the server ignores — a worse outcome than not checking at all, since
// it would report a preview and execute for real:
//
//   - PATCH, PUT and POST carry their options as query parameters, so dryRun is in
//     the query.
//   - DELETE carries DeleteOptions in the request BODY. The apiserver's delete
//     handler decodes options from the body when one is present and only falls
//     back to the query when it is absent (see k8s.io/apiserver
//     pkg/endpoints/handlers/delete.go), so a query dryRun on a DELETE with a body
//     is silently ignored — and MaKlaude's deletes always carry a body, because
//     they always carry a resourceVersion precondition.
//
// Every check fails closed: an unreadable body, an unexpected shape, or a missing
// marker all read as "not a dry run", so the scope refuses rather than assumes.
func hasServerDryRun(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if normalizeMethod(req) == http.MethodDelete {
		return bodyRequestsDryRun(req)
	}
	return queryRequestsDryRun(req)
}

// queryRequestsDryRun reports whether the request's query asks for a dry run,
// i.e. carries exactly one dryRun parameter whose value is "All".
//
// The check is exact and single-valued on purpose. Kubernetes ignores a dryRun
// value it does not recognise, so a typo ("all", "true") would otherwise read as
// "dry run requested" here and execute for real at the API server — the one
// mismatch this guard exists to make impossible. A second dryRun parameter is
// likewise refused rather than merged: the request's meaning would then depend on
// which value the server happens to read.
func queryRequestsDryRun(req *http.Request) bool {
	values, ok := req.URL.Query()["dryRun"]
	if !ok || len(values) != 1 {
		return false
	}
	return values[0] == metav1.DryRunAll
}

// maxDryRunBodyBytes bounds how much of a request body the guard will read while
// looking for the dry-run marker. A DeleteOptions document is a few hundred bytes;
// the bound exists so a malformed or hostile body cannot make the guard itself the
// expensive part of a refusal.
const maxDryRunBodyBytes int64 = 64 << 10

// bodyRequestsDryRun reports whether a DELETE's DeleteOptions body asks for a dry
// run.
//
// It reads a FRESH copy of the body via req.GetBody rather than consuming
// req.Body, so inspecting a request never changes the request that is about to be
// sent. net/http populates GetBody for the readers client-go uses
// (rest.Request hands it a *bytes.Reader), and a nil GetBody fails closed.
//
// Parsing the body requires it to be JSON, which is why the write path pins
// application/json (see [restConfigForScope]) instead of letting client-go
// negotiate protobuf as it does for reads. A protobuf DeleteOptions would leave
// this check with a binary blob it could only guess at, and a guard that guesses
// is not a guard.
func bodyRequestsDryRun(req *http.Request) bool {
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil || body == nil {
		return false
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(body, maxDryRunBodyBytes))
	if err != nil {
		return false
	}

	var opts struct {
		DryRun []string `json:"dryRun"`
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		return false
	}
	return len(opts.DryRun) == 1 && opts.DryRun[0] == metav1.DryRunAll
}
