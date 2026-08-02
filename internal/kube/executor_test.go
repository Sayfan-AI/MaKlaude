package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// recordedRequest is what the stub API server saw for one request. Asserting on
// these is how the write-path tests prove what MaKlaude actually put on the wire,
// rather than only what it reported doing.
type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Body        map[string]any

	// RawBody is the body exactly as it arrived. Body is the convenient form for a
	// strategic-merge patch, which is an object; a JSON patch is an ARRAY of operations,
	// which does not survive being decoded into a map at all — and the order of those
	// operations is itself part of what the guard promises.
	RawBody string
}

// stubAPIServer is an httptest server standing in for an API server. It records
// every request and answers with a canned object, so a test can drive the real
// client-go → rest.Config → scoped guard → wire path without a cluster.
type stubAPIServer struct {
	*httptest.Server
	seen   []recordedRequest
	status int
	body   string
}

// newStubAPIServer starts a stub answering every request with 200 and body.
func newStubAPIServer(t *testing.T, body string) *stubAPIServer {
	t.Helper()
	return newStubAPIServerWithStatus(t, http.StatusOK, body)
}

// newStubAPIServerWithStatus starts a stub answering every request with the given
// HTTP status and body.
func newStubAPIServerWithStatus(t *testing.T, status int, body string) *stubAPIServer {
	t.Helper()
	stub := &stubAPIServer{status: status, body: body}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
		}
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err == nil {
			_ = json.Unmarshal(raw, &rec.Body)
		}
		stub.seen = append(stub.seen, rec)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, _ = w.Write([]byte(stub.body))
	}))
	t.Cleanup(stub.Close)
	return stub
}

// only returns the single request the stub saw, failing if it saw any other
// number. Every action in this package is exactly one API call, so "how many
// requests" is itself part of the contract.
func (s *stubAPIServer) only(t *testing.T) recordedRequest {
	t.Helper()
	if len(s.seen) != 1 {
		t.Fatalf("expected exactly 1 request, got %d: %+v", len(s.seen), s.seen)
	}
	return s.seen[0]
}

const (
	deploymentJSON = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"prod","resourceVersion":"1234"}}`
	nodeJSON       = `{"apiVersion":"v1","kind":"Node","metadata":{"name":"node-a","resourceVersion":"1234"}}`
	successJSON    = `{"apiVersion":"v1","kind":"Status","status":"Success"}`
	conflictJSON   = `{"apiVersion":"v1","kind":"Status","status":"Failure","code":409,"reason":"Conflict","message":"the object has been modified"}`
)

// newExecutorAgainst builds an executor in the given mode pointed at stub.
func newExecutorAgainst(t *testing.T, stub *stubAPIServer, mode ExecuteMode) *Executor {
	t.Helper()
	h := handleFor(t, "prod", writeKubeconfig(t, stub.URL), "maklaude")
	e, err := NewExecutor(h, mode)
	if err != nil {
		t.Fatalf("building executor: %v", err)
	}
	return e
}

// TestNewExecutor_RefusesUnlessExplicitlyEnabled is the kill switch's central
// test. The write path must be unreachable — not merely unused — unless an
// operator opted in, so a disabled mode yields NO executor rather than an inert
// one. The zero-value case matters most: it is what a forgotten field, an unset
// environment variable, or a new call site produces.
func TestNewExecutor_RefusesUnlessExplicitlyEnabled(t *testing.T) {
	h := handleFor(t, "prod", writeKubeconfig(t, "https://127.0.0.1:1"), "maklaude")

	cases := map[string]ExecuteMode{
		"explicitly disabled": ExecuteDisabled,
		"zero value":          ExecuteMode(0),
		"unknown mode":        ExecuteMode(99),
	}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			e, err := NewExecutor(h, mode)
			if !errors.Is(err, ErrExecutorDisabled) {
				t.Fatalf("expected ErrExecutorDisabled, got: %v", err)
			}
			if e != nil {
				t.Fatal("a disabled executor must be nil — there must be no write-capable object to hold")
			}
		})
	}
}

// TestNewExecutor_NilHandle proves a nil handle is a build-config error, not a
// panic.
func TestNewExecutor_NilHandle(t *testing.T) {
	if _, err := NewExecutor(nil, ExecuteEnabled); !errors.Is(err, ErrBuildConfig) {
		t.Fatalf("expected ErrBuildConfig for a nil handle, got: %v", err)
	}
}

// TestNewExecutor_UnknownContextFailsAtConstruction proves an unusable
// kubeconfig/context fails when the executor is built rather than at the first
// action: an executor that exists but can never act is a worse thing to hand an
// approval gate than a construction error.
func TestNewExecutor_UnknownContextFailsAtConstruction(t *testing.T) {
	h := handleFor(t, "prod", writeKubeconfig(t, "https://127.0.0.1:1"), "nonexistent")
	if _, err := NewExecutor(h, ExecuteEnabled); !errors.Is(err, ErrBuildConfig) {
		t.Fatalf("expected ErrBuildConfig for an unknown context, got: %v", err)
	}
}

// TestExecutor_RestartDeploymentRolloutSendsScopedPatch proves the whole write
// path end to end: the request lands on the exact object's path, as a strategic
// merge patch, carrying the restart annotation AND the injected resourceVersion
// precondition — and that a real-execution outcome reports itself as not a dry
// run.
func TestExecutor_RestartDeploymentRolloutSendsScopedPatch(t *testing.T) {
	stub := newStubAPIServer(t, deploymentJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	out, err := e.RestartDeploymentRollout(context.Background(), "prod", "web", "2026-08-01T21:00:00Z", "1234")
	if err != nil {
		t.Fatalf("restart failed: %v", err)
	}

	req := stub.only(t)
	if req.Method != http.MethodPatch {
		t.Fatalf("method %q, want PATCH", req.Method)
	}
	if req.Path != "/apis/apps/v1/namespaces/prod/deployments/web" {
		t.Fatalf("path %q is not the target deployment", req.Path)
	}
	if !strings.Contains(req.ContentType, "strategic-merge-patch") {
		t.Fatalf("content type %q, want a strategic merge patch", req.ContentType)
	}
	if req.Query != "" {
		t.Fatalf("query %q, want none — an enabled executor must not ask for a dry run", req.Query)
	}

	meta, ok := req.Body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("patch body has no metadata object: %+v", req.Body)
	}
	if meta["resourceVersion"] != "1234" {
		t.Fatalf("patch body resourceVersion = %v, want the injected precondition 1234", meta["resourceVersion"])
	}
	if got := restartedAtFromPatch(t, req.Body); got != "2026-08-01T21:00:00Z" {
		t.Fatalf("restartedAt annotation = %q, want the caller-supplied timestamp", got)
	}

	if out.DryRun {
		t.Fatal("an ExecuteEnabled outcome must not report itself as a dry run")
	}
	if out.Cluster != "prod" || out.Target != "deployment/prod/web" || out.ResourceVersion != "1234" {
		t.Fatalf("outcome does not describe the action: %+v", out)
	}
	if out.Scope != "PATCH /apis/apps/v1/namespaces/prod/deployments/web" {
		t.Fatalf("outcome scope %q does not name the admitted request", out.Scope)
	}
}

// restartedAtFromPatch digs the restart annotation out of a recorded patch body.
func restartedAtFromPatch(t *testing.T, body map[string]any) string {
	t.Helper()
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("patch body has no spec: %+v", body)
	}
	template, ok := spec["template"].(map[string]any)
	if !ok {
		t.Fatalf("patch spec has no template: %+v", spec)
	}
	meta, ok := template["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("patch template has no metadata: %+v", template)
	}
	annotations, ok := meta["annotations"].(map[string]any)
	if !ok {
		t.Fatalf("patch template metadata has no annotations: %+v", meta)
	}
	value, _ := annotations[restartedAtAnnotation].(string)
	return value
}

// TestExecutor_DryRunModeAsksTheServerForAPreview proves a dry-run executor sends
// dryRun=All. Combined with the transport's own refusal of a mutating request that
// lacks it (see TestScopedWrite_DryRunScopeRequiresServerDryRun), a preview
// provably cannot mutate: the option is both always sent and structurally
// required.
func TestExecutor_DryRunModeAsksTheServerForAPreview(t *testing.T) {
	actions := map[string]struct {
		run func(*Executor) (*Outcome, error)
		// reply is what the stub answers with.
		reply string
		// inBody says the dry-run marker travels in the request body rather than the
		// query. That is true of DELETE and only DELETE: the apiserver decodes
		// DeleteOptions from the body when one is present and ignores the query, so
		// asserting on the query for a delete would check a parameter the server
		// never reads. Pinning the location per verb here is what keeps that
		// asymmetry from silently regressing into a preview that executes.
		inBody bool
	}{
		"restart deployment": {
			run: func(e *Executor) (*Outcome, error) {
				return e.RestartDeploymentRollout(context.Background(), "prod", "web", "t", "1234")
			},
			reply: deploymentJSON,
		},
		"cordon node": {
			run:   func(e *Executor) (*Outcome, error) { return e.CordonNode(context.Background(), "node-a", "1234") },
			reply: nodeJSON,
		},
		"delete pod": {
			run: func(e *Executor) (*Outcome, error) {
				return e.DeletePod(context.Background(), "prod", "web-abc", "1234")
			},
			reply:  successJSON,
			inBody: true,
		},
	}

	for name, action := range actions {
		t.Run(name, func(t *testing.T) {
			stub := newStubAPIServer(t, action.reply)
			e := newExecutorAgainst(t, stub, ExecuteDryRun)

			out, err := action.run(e)
			if err != nil {
				t.Fatalf("dry-run action failed: %v", err)
			}
			req := stub.only(t)
			if action.inBody {
				if req.Query != "" {
					t.Fatalf("query %q, want none — a delete's dryRun belongs in the body the server reads", req.Query)
				}
				dryRun, ok := req.Body["dryRun"].([]any)
				if !ok || len(dryRun) != 1 || dryRun[0] != metav1.DryRunAll {
					t.Fatalf("request body dryRun = %v, want exactly [All]: %+v", req.Body["dryRun"], req.Body)
				}
			} else if req.Query != "dryRun=All" {
				t.Fatalf("query %q, want exactly dryRun=All", req.Query)
			}
			if !out.DryRun {
				t.Fatal("a dry-run outcome must report itself as a dry run")
			}
			if !strings.HasSuffix(out.Scope, "(dry-run only)") {
				t.Fatalf("outcome scope %q does not record the preview-only posture", out.Scope)
			}
		})
	}
}

// TestExecutor_CordonNodeSendsUnschedulablePatch proves the cordon primitive
// targets the cluster-scoped node path and sets only spec.unschedulable —
// cordoning is not draining, and no eviction or spec change rides along.
func TestExecutor_CordonNodeSendsUnschedulablePatch(t *testing.T) {
	stub := newStubAPIServer(t, nodeJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	if _, err := e.CordonNode(context.Background(), "node-a", "1234"); err != nil {
		t.Fatalf("cordon failed: %v", err)
	}

	req := stub.only(t)
	if req.Method != http.MethodPatch || req.Path != "/api/v1/nodes/node-a" {
		t.Fatalf("cordon sent %s %s, want PATCH /api/v1/nodes/node-a", req.Method, req.Path)
	}
	spec, ok := req.Body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("cordon patch has no spec: %+v", req.Body)
	}
	if spec["unschedulable"] != true {
		t.Fatalf("cordon patch spec = %+v, want unschedulable true", spec)
	}
	if len(spec) != 1 {
		t.Fatalf("cordon patch spec touches more than unschedulable: %+v", spec)
	}
}

// TestExecutor_DeletePodSendsResourceVersionPrecondition proves the delete carries
// the optimistic-concurrency token as a DeleteOptions precondition, and targets
// the single object rather than the collection.
func TestExecutor_DeletePodSendsResourceVersionPrecondition(t *testing.T) {
	stub := newStubAPIServer(t, successJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	if _, err := e.DeletePod(context.Background(), "prod", "web-abc", "1234"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	req := stub.only(t)
	if req.Method != http.MethodDelete {
		t.Fatalf("method %q, want DELETE", req.Method)
	}
	if req.Path != "/api/v1/namespaces/prod/pods/web-abc" {
		t.Fatalf("path %q is not the single target pod", req.Path)
	}
	preconditions, ok := req.Body["preconditions"].(map[string]any)
	if !ok {
		t.Fatalf("delete body carries no preconditions: %+v", req.Body)
	}
	if preconditions["resourceVersion"] != "1234" {
		t.Fatalf("delete precondition resourceVersion = %v, want 1234", preconditions["resourceVersion"])
	}
}

// TestExecutor_RequiresAResourceVersion proves there is no unconditional variant
// of any action to reach for under time pressure: every primitive refuses without
// a precondition, before any request is made.
func TestExecutor_RequiresAResourceVersion(t *testing.T) {
	stub := newStubAPIServer(t, successJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)
	ctx := context.Background()

	actions := map[string]func() (*Outcome, error){
		"restart deployment": func() (*Outcome, error) { return e.RestartDeploymentRollout(ctx, "prod", "web", "t", "") },
		"patch deployment":   func() (*Outcome, error) { return e.PatchDeployment(ctx, "prod", "web", []byte(`{}`), " ") },
		"cordon node":        func() (*Outcome, error) { return e.CordonNode(ctx, "node-a", "") },
		"patch node":         func() (*Outcome, error) { return e.PatchNode(ctx, "node-a", []byte(`{}`), "") },
		"delete pod":         func() (*Outcome, error) { return e.DeletePod(ctx, "prod", "web-abc", "") },
	}
	for name, run := range actions {
		t.Run(name, func(t *testing.T) {
			if _, err := run(); !errors.Is(err, ErrMissingPrecondition) {
				t.Fatalf("expected ErrMissingPrecondition, got: %v", err)
			}
		})
	}
	if len(stub.seen) != 0 {
		t.Fatalf("an action missing its precondition still reached the API server: %+v", stub.seen)
	}
}

// TestExecutor_RejectsMalformedTargets proves target names are validated before a
// path is composed from them. This is a safety check as much as an input check: a
// name containing a slash or a traversal segment would otherwise produce a request
// path that is not the object it claims to be — and because the WriteScope is
// composed from the same values, a crafted name could make an out-of-scope request
// match its own scope.
func TestExecutor_RejectsMalformedTargets(t *testing.T) {
	stub := newStubAPIServer(t, successJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)
	ctx := context.Background()

	actions := map[string]func() (*Outcome, error){
		"empty namespace":        func() (*Outcome, error) { return e.DeletePod(ctx, "", "web-abc", "1234") },
		"empty name":             func() (*Outcome, error) { return e.DeletePod(ctx, "prod", "", "1234") },
		"slash in name":          func() (*Outcome, error) { return e.DeletePod(ctx, "prod", "web/../api", "1234") },
		"traversal in namespace": func() (*Outcome, error) { return e.DeletePod(ctx, "../../kube-system", "web-abc", "1234") },
		"subresource in name":    func() (*Outcome, error) { return e.PatchDeployment(ctx, "prod", "web/scale", []byte(`{}`), "1234") },
		"escaped slash in name":  func() (*Outcome, error) { return e.PatchNode(ctx, "node-a%2Fx", []byte(`{}`), "1234") },
		"whitespace in name":     func() (*Outcome, error) { return e.PatchNode(ctx, "node a", []byte(`{}`), "1234") },
		"uppercase name":         func() (*Outcome, error) { return e.PatchNode(ctx, "Node-A", []byte(`{}`), "1234") },
	}
	for name, run := range actions {
		t.Run(name, func(t *testing.T) {
			if _, err := run(); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("expected ErrInvalidTarget, got: %v", err)
			}
		})
	}
	if len(stub.seen) != 0 {
		t.Fatalf("a malformed target still reached the API server: %+v", stub.seen)
	}
}

// TestExecutor_RejectsRetargetingAndPreconditionRelaxingPatches proves a patch
// body cannot quietly describe a different object than the approved scope admits,
// nor relax the precondition it is conditioned on. Both are refused rather than
// overwritten: silently correcting a body that disagrees with its own target would
// hide a real caller bug.
func TestExecutor_RejectsRetargetingAndPreconditionRelaxingPatches(t *testing.T) {
	stub := newStubAPIServer(t, deploymentJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)
	ctx := context.Background()

	patches := map[string]string{
		"sets metadata.name":         `{"metadata":{"name":"api"}}`,
		"sets metadata.namespace":    `{"metadata":{"namespace":"kube-system"}}`,
		"sets metadata.uid":          `{"metadata":{"uid":"00000000-0000-0000-0000-000000000000"}}`,
		"sets a different rv":        `{"metadata":{"resourceVersion":"9999"}}`,
		"metadata is not an object":  `{"metadata":"nope"}`,
		"body is a JSON array":       `[{"op":"replace"}]`,
		"body is JSON null":          `null`,
		"body is not JSON":           `not json`,
		"body is empty":              ``,
		"body is a bare JSON string": `"spec"`,
	}
	for name, patch := range patches {
		t.Run(name, func(t *testing.T) {
			if _, err := e.PatchDeployment(ctx, "prod", "web", []byte(patch), "1234"); !errors.Is(err, ErrInvalidPatch) {
				t.Fatalf("expected ErrInvalidPatch, got: %v", err)
			}
		})
	}
	if len(stub.seen) != 0 {
		t.Fatalf("a refused patch still reached the API server: %+v", stub.seen)
	}
}

// TestExecutor_AcceptsAMatchingResourceVersionInThePatch proves the refusal above
// is about DISAGREEMENT, not about mentioning the field: a body that states the
// same resourceVersion the executor would inject is fine.
func TestExecutor_AcceptsAMatchingResourceVersionInThePatch(t *testing.T) {
	stub := newStubAPIServer(t, deploymentJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	_, err := e.PatchDeployment(context.Background(), "prod", "web", []byte(`{"metadata":{"resourceVersion":"1234"}}`), "1234")
	if err != nil {
		t.Fatalf("a patch agreeing with its own precondition was refused: %v", err)
	}
	if meta, ok := stub.only(t).Body["metadata"].(map[string]any); !ok || meta["resourceVersion"] != "1234" {
		t.Fatalf("sent patch lost its precondition: %+v", stub.seen)
	}
}

// TestExecutor_StaleProposalSurfacesAsPreconditionConflict proves the API server's
// 409 is classified as its own error class. A stale approval is the expected,
// healthy outcome of a cluster moving underneath a proposal — the caller should
// re-propose, not escalate — so it must be distinguishable from a real failure.
func TestExecutor_StaleProposalSurfacesAsPreconditionConflict(t *testing.T) {
	stub := newStubAPIServerWithStatus(t, http.StatusConflict, conflictJSON)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	_, err := e.RestartDeploymentRollout(context.Background(), "prod", "web", "t", "1234")
	if !errors.Is(err, ErrPreconditionConflict) {
		t.Fatalf("expected ErrPreconditionConflict for a 409, got: %v", err)
	}
	if errors.Is(err, ErrExecute) {
		t.Fatal("a stale precondition must not also read as a generic execution failure")
	}
	if !strings.Contains(err.Error(), "1234") {
		t.Fatalf("conflict error does not name the expected resourceVersion: %v", err)
	}
}

// TestExecutor_APIFailureSurfacesAsExecuteError proves a denial (here an RBAC-shaped
// 403) is reported as an execution failure naming the cluster and target, and is
// not misread as a stale precondition.
func TestExecutor_APIFailureSurfacesAsExecuteError(t *testing.T) {
	forbidden := `{"apiVersion":"v1","kind":"Status","status":"Failure","code":403,"reason":"Forbidden","message":"deployments.apps \"web\" is forbidden"}`
	stub := newStubAPIServerWithStatus(t, http.StatusForbidden, forbidden)
	e := newExecutorAgainst(t, stub, ExecuteEnabled)

	_, err := e.RestartDeploymentRollout(context.Background(), "prod", "web", "t", "1234")
	if !errors.Is(err, ErrExecute) {
		t.Fatalf("expected ErrExecute for a 403, got: %v", err)
	}
	if errors.Is(err, ErrPreconditionConflict) {
		t.Fatal("an RBAC denial must not read as a stale precondition")
	}
	if !strings.Contains(err.Error(), `"prod"`) || !strings.Contains(err.Error(), "deployment/prod/web") {
		t.Fatalf("execute error does not name the cluster and target: %v", err)
	}
}

// TestExecutor_NoCrossClusterLeakage proves two executors built from two handles
// act on their own clusters only. Multi-cluster is a first-class concern and a
// mutating action pointed at the wrong cluster is the worst failure this system
// could have, so it is asserted by observing which server received the request
// rather than inferred from the absence of shared state.
func TestExecutor_NoCrossClusterLeakage(t *testing.T) {
	stubA := newStubAPIServer(t, deploymentJSON)
	stubB := newStubAPIServer(t, deploymentJSON)

	execA := newExecutorAgainst(t, stubA, ExecuteEnabled)
	execB := func() *Executor {
		h := handleFor(t, "staging", writeKubeconfig(t, stubB.URL), "maklaude")
		e, err := NewExecutor(h, ExecuteEnabled)
		if err != nil {
			t.Fatalf("building executor for staging: %v", err)
		}
		return e
	}()

	outA, err := execA.RestartDeploymentRollout(context.Background(), "prod", "web", "t", "1234")
	if err != nil {
		t.Fatalf("cluster A action failed: %v", err)
	}
	if len(stubA.seen) != 1 || len(stubB.seen) != 0 {
		t.Fatalf("cluster A's action hit A=%d B=%d requests; it must reach only A", len(stubA.seen), len(stubB.seen))
	}
	if outA.Cluster != "prod" {
		t.Fatalf("outcome names cluster %q, want prod", outA.Cluster)
	}

	outB, err := execB.RestartDeploymentRollout(context.Background(), "prod", "web", "t", "1234")
	if err != nil {
		t.Fatalf("cluster B action failed: %v", err)
	}
	if len(stubA.seen) != 1 || len(stubB.seen) != 1 {
		t.Fatalf("cluster B's action hit A=%d B=%d requests; it must reach only B", len(stubA.seen), len(stubB.seen))
	}
	if outB.Cluster != "staging" {
		t.Fatalf("outcome names cluster %q, want staging", outB.Cluster)
	}
}

// TestExecutor_ObservationClientStaysReadOnlyAlongsideAnExecutor proves the
// read-only guarantee survives the write path existing. An enabled executor and a
// read-only client are built from the SAME handle, and the client's transport still
// refuses a mutating request — so enabling execution grants authority to the
// executor and to nothing else.
func TestExecutor_ObservationClientStaysReadOnlyAlongsideAnExecutor(t *testing.T) {
	stub := newStubAPIServer(t, deploymentJSON)
	h := handleFor(t, "prod", writeKubeconfig(t, stub.URL), "maklaude")

	if _, err := NewExecutor(h, ExecuteEnabled); err != nil {
		t.Fatalf("building executor: %v", err)
	}

	// A write-capable clientset built from the OBSERVATION config, which is the
	// strongest available probe of that path's guard (see WriteProbeClientForHandle).
	probe, err := WriteProbeClientForHandle(h)
	if err != nil {
		t.Fatalf("building write probe: %v", err)
	}
	err = probe.CoreV1().Pods("prod").Delete(context.Background(), "web-abc", metav1.DeleteOptions{})
	if !errors.Is(err, ErrWriteForbidden) {
		t.Fatalf("the observation path admitted a write while an executor existed: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("the observation path put a mutating request on the wire: %+v", stub.seen)
	}
}

// TestParseExecuteMode covers the vocabulary a later config surface will adopt,
// including that an empty value means disabled ("the operator said nothing" and
// "the operator said off" are the same posture) and that an unrecognised value is
// an error rather than a silent default in either direction.
func TestParseExecuteMode(t *testing.T) {
	valid := map[string]ExecuteMode{
		"":          ExecuteDisabled,
		"disabled":  ExecuteDisabled,
		"dry-run":   ExecuteDryRun,
		"DRY-RUN":   ExecuteDryRun,
		" enabled ": ExecuteEnabled,
	}
	for in, want := range valid {
		got, err := ParseExecuteMode(in)
		if err != nil {
			t.Fatalf("ParseExecuteMode(%q) errored: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseExecuteMode(%q) = %v, want %v", in, got, want)
		}
	}

	for _, in := range []string{"on", "off", "true", "yes", "dryrun", "execute"} {
		got, err := ParseExecuteMode(in)
		if err == nil {
			t.Fatalf("ParseExecuteMode(%q) accepted an unknown value as %v", in, got)
		}
		if got != ExecuteDisabled {
			t.Fatalf("ParseExecuteMode(%q) failed open with %v; an unparseable mode must fall back to disabled", in, got)
		}
	}
}

// TestExecuteMode_String pins the tokens: they appear in escalations, the audit
// trail, and config, so they are part of the contract.
func TestExecuteMode_String(t *testing.T) {
	want := map[ExecuteMode]string{
		ExecuteDisabled: "disabled",
		ExecuteDryRun:   "dry-run",
		ExecuteEnabled:  "enabled",
		ExecuteMode(99): "executemode(99)",
	}
	for mode, token := range want {
		if got := mode.String(); got != token {
			t.Fatalf("ExecuteMode(%d).String() = %q, want %q", int(mode), got, token)
		}
	}
}
