package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// The fixture cluster for a rollback: deployment prod/web at revision 2, with revision
// 1's ReplicaSet still in its history.
//
// The two templates differ in the way that matters. Revision 2 ADDED a sidecar container
// and an environment variable, which is precisely what a strategic-merge patch cannot
// take back — it merges containers and env vars by name, so both would survive a
// "rollback" and the Deployment would end up on a template that is neither revision.
// Every assertion below about the patch body is really an assertion about that.
const (
	rollbackDeploymentUID = "11111111-2222-3333-4444-555555555555"

	rollbackDeploymentJSON = `{
		"apiVersion":"apps/v1","kind":"Deployment",
		"metadata":{"name":"web","namespace":"prod","uid":"` + rollbackDeploymentUID + `","resourceVersion":"2002"},
		"spec":{
			"selector":{"matchLabels":{"app":"web"}},
			"template":{
				"metadata":{"labels":{"app":"web","pod-template-hash":"bbbb"}},
				"spec":{"containers":[
					{"name":"app","image":"web:2","env":[{"name":"FEATURE_X","value":"on"}]},
					{"name":"sidecar","image":"proxy:1"}
				]}
			}
		}
	}`

	// revision 1: one container, no sidecar, no FEATURE_X.
	replicaSetListJSON = `{
		"apiVersion":"apps/v1","kind":"ReplicaSetList","metadata":{"resourceVersion":"2100"},
		"items":[
			{
				"metadata":{
					"name":"web-aaaa","namespace":"prod","resourceVersion":"2101",
					"annotations":{"deployment.kubernetes.io/revision":"1"},
					"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"web","uid":"` + rollbackDeploymentUID + `","controller":true}]
				},
				"spec":{"template":{
					"metadata":{"labels":{"app":"web","pod-template-hash":"aaaa"}},
					"spec":{"containers":[{"name":"app","image":"web:1"}]}
				}}
			},
			{
				"metadata":{
					"name":"web-bbbb","namespace":"prod","resourceVersion":"2102",
					"annotations":{"deployment.kubernetes.io/revision":"2"},
					"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"web","uid":"` + rollbackDeploymentUID + `","controller":true}]
				},
				"spec":{"template":{
					"metadata":{"labels":{"app":"web","pod-template-hash":"bbbb"}},
					"spec":{"containers":[
						{"name":"app","image":"web:2","env":[{"name":"FEATURE_X","value":"on"}]},
						{"name":"sidecar","image":"proxy:1"}
					]}
				}}
			},
			{
				"metadata":{
					"name":"impostor","namespace":"prod","resourceVersion":"2103",
					"annotations":{"deployment.kubernetes.io/revision":"9"},
					"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"web","uid":"99999999-9999-9999-9999-999999999999","controller":true}]
				},
				"spec":{"template":{
					"metadata":{"labels":{"app":"web"}},
					"spec":{"containers":[{"name":"app","image":"attacker:latest"}]}
				}}
			}
		]
	}`
)

// rollbackStub is an API server that answers each leg of a rollback differently: the
// Deployment GET, the ReplicaSet LIST, and the PATCH. The shared stubAPIServer answers
// every request with one canned body, which cannot express a read-then-write action.
type rollbackStub struct {
	*httptest.Server
	seen []recordedRequest
}

func newRollbackStub(t *testing.T) *rollbackStub {
	t.Helper()
	stub := &rollbackStub{}
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
			rec.RawBody = string(raw)
		}
		stub.seen = append(stub.seen, rec)

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deployments/web"):
			_, _ = w.Write([]byte(rollbackDeploymentJSON))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/replicasets"):
			_, _ = w.Write([]byte(replicaSetListJSON))
		default:
			_, _ = w.Write([]byte(rollbackDeploymentJSON))
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

// requests returns the requests the stub saw, by method and path, for assertions about
// what an action did and did NOT do.
func (s *rollbackStub) requests() []recordedRequest { return s.seen }

// mutating returns the single mutating request the stub saw, failing when there was any
// other number — "how many writes did this produce?" is part of the contract.
func (s *rollbackStub) mutating(t *testing.T) recordedRequest {
	t.Helper()
	var found []recordedRequest
	for _, req := range s.seen {
		if req.Method != http.MethodGet {
			found = append(found, req)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 mutating request, got %d: %+v", len(found), s.seen)
	}
	return found[0]
}

func newExecutorAgainstRollbackStub(t *testing.T, stub *rollbackStub, mode ExecuteMode) *Executor {
	t.Helper()
	h := handleFor(t, "prod", writeKubeconfig(t, stub.URL), "maklaude")
	e, err := NewExecutor(h, mode)
	if err != nil {
		t.Fatalf("building executor: %v", err)
	}
	return e
}

// TestExecutor_RollbackSendsTheTargetRevisionsTemplate is the primitive's central test.
// It asserts on what went on the WIRE, because everything this operation gets wrong is
// invisible in its return value: a rollback that sent the current template, or the old
// one merged into the new, reports success just the same.
func TestExecutor_RollbackSendsTheTargetRevisionsTemplate(t *testing.T) {
	stub := newRollbackStub(t)
	e := newExecutorAgainstRollbackStub(t, stub, ExecuteEnabled)

	out, err := e.RollbackDeploymentToRevision(context.Background(), "prod", "web", 1, "2002")
	if err != nil {
		t.Fatalf("rollback failed: %v", err)
	}

	req := stub.mutating(t)
	if req.Method != http.MethodPatch || req.Path != "/apis/apps/v1/namespaces/prod/deployments/web" {
		t.Fatalf("the mutating request was %s %s, want a PATCH of the target deployment", req.Method, req.Path)
	}
	if !strings.Contains(req.ContentType, "json-patch") {
		t.Fatalf("content type %q, want an RFC 6902 JSON patch — a strategic merge cannot remove fields", req.ContentType)
	}

	ops := decodeOps(t, req.RawBody)
	if len(ops) != 2 {
		t.Fatalf("the patch has %d operations, want the precondition plus one template replace: %s", len(ops), req.RawBody)
	}
	if ops[0].Op != "replace" || ops[0].Path != resourceVersionPointer || string(ops[0].Value) != `"2002"` {
		t.Fatalf("the first operation is %+v, want the injected resourceVersion precondition", ops[0])
	}
	if ops[1].Op != "replace" || ops[1].Path != podTemplatePointer {
		t.Fatalf("the second operation is %s %s, want a replace of %s", ops[1].Op, ops[1].Path, podTemplatePointer)
	}

	template := string(ops[1].Value)
	if !strings.Contains(template, `"image":"web:1"`) {
		t.Fatalf("the restored template is not revision 1's: %s", template)
	}
	// The two things a strategic-merge patch would have left behind.
	if strings.Contains(template, "sidecar") {
		t.Fatalf("the restored template still carries the container revision 2 added: %s", template)
	}
	if strings.Contains(template, "FEATURE_X") {
		t.Fatalf("the restored template still carries the env var revision 2 added: %s", template)
	}
	// pod-template-hash is derived from the template, so writing one revision's hash into
	// the Deployment's own template would pin it to that revision forever.
	if strings.Contains(template, podTemplateHashLabel) {
		t.Fatalf("the restored template carries %s, which belongs to the ReplicaSet and not to the Deployment: %s",
			podTemplateHashLabel, template)
	}

	if out.DryRun {
		t.Fatal("an ExecuteEnabled outcome must not report itself as a dry run")
	}
	if out.Target != "deployment/prod/web" || out.ResourceVersion != "2002" {
		t.Fatalf("outcome does not describe the action: %+v", out)
	}
}

// TestExecutor_RollbackRefusesARevisionThatIsGone covers the pruned-history case, which
// is the expected outcome of an approval that waited: Kubernetes deletes old ReplicaSets
// past a Deployment's revisionHistoryLimit.
//
// It must fail with [ErrRevisionNotFound] and SEND NOTHING, which is why the assertion is
// on the stub rather than on the error alone: the whole reason the revision is resolved
// by a read first is so a missing one costs no mutating request.
func TestExecutor_RollbackRefusesARevisionThatIsGone(t *testing.T) {
	stub := newRollbackStub(t)
	e := newExecutorAgainstRollbackStub(t, stub, ExecuteEnabled)

	_, err := e.RollbackDeploymentToRevision(context.Background(), "prod", "web", 7, "2002")
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("expected ErrRevisionNotFound, got: %v", err)
	}
	// The surviving revisions belong in the error: they are what tells a human whether the
	// history was pruned or the revision never existed.
	if !strings.Contains(err.Error(), "surviving revisions: 1, 2") {
		t.Fatalf("the error does not say what survived: %v", err)
	}
	for _, req := range stub.requests() {
		if req.Method != http.MethodGet {
			t.Fatalf("a missing revision still produced %s %s", req.Method, req.Path)
		}
	}
}

// TestExecutor_RollbackIgnoresAReplicaSetItDoesNotOwn is the reason the ReplicaSet is
// matched by controller UID rather than by name or labels.
//
// The fixture's "impostor" carries the Deployment's labels and a revision annotation, so
// a selector-only match would accept it — and whatever pod template it carries becomes
// the Deployment's spec. Matching the owner UID is what makes that impossible.
func TestExecutor_RollbackIgnoresAReplicaSetItDoesNotOwn(t *testing.T) {
	stub := newRollbackStub(t)
	e := newExecutorAgainstRollbackStub(t, stub, ExecuteEnabled)

	_, err := e.RollbackDeploymentToRevision(context.Background(), "prod", "web", 9, "2002")
	if !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("expected the impostor ReplicaSet to be invisible (ErrRevisionNotFound), got: %v", err)
	}
	for _, req := range stub.requests() {
		if req.Method != http.MethodGet {
			t.Fatalf("a ReplicaSet the deployment does not own produced %s %s", req.Method, req.Path)
		}
	}
}

// TestExecutor_RollbackRefusesBeforeReadingAnything proves the argument checks happen
// before any network call. A rollback that cannot be conditioned, or that names no real
// revision, must not so much as read the cluster.
func TestExecutor_RollbackRefusesBeforeReadingAnything(t *testing.T) {
	cases := map[string]struct {
		namespace, name string
		revision        int64
		resourceVersion string
		wantErr         error
	}{
		"no resourceVersion": {"prod", "web", 1, "", ErrMissingPrecondition},
		"revision zero":      {"prod", "web", 0, "2002", ErrInvalidTarget},
		"negative revision":  {"prod", "web", -3, "2002", ErrInvalidTarget},
		"empty namespace":    {"", "web", 1, "2002", ErrInvalidTarget},
		"path-crafted name":  {"prod", "web/../nodes", 1, "2002", ErrInvalidTarget},
		"empty deploy name":  {"prod", "", 1, "2002", ErrInvalidTarget},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newRollbackStub(t)
			e := newExecutorAgainstRollbackStub(t, stub, ExecuteEnabled)

			_, err := e.RollbackDeploymentToRevision(context.Background(), tc.namespace, tc.name, tc.revision, tc.resourceVersion)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got: %v", tc.wantErr, err)
			}
			if len(stub.requests()) != 0 {
				t.Fatalf("a refused rollback still made %d request(s): %+v", len(stub.requests()), stub.requests())
			}
		})
	}
}

// TestExecutor_RollbackInDryRunModeAsksTheServerForAPreview proves the kill switch's
// preview mode reaches the new primitive too. The scope's own RequireDryRun refusal
// (TestScopedWrite_DryRunScopeRequiresServerDryRun) is the other half: the marker is both
// always sent and structurally required, so a preview provably cannot mutate.
func TestExecutor_RollbackInDryRunModeAsksTheServerForAPreview(t *testing.T) {
	stub := newRollbackStub(t)
	e := newExecutorAgainstRollbackStub(t, stub, ExecuteDryRun)

	out, err := e.RollbackDeploymentToRevision(context.Background(), "prod", "web", 1, "2002")
	if err != nil {
		t.Fatalf("dry-run rollback failed: %v", err)
	}
	req := stub.mutating(t)
	if !strings.Contains(req.Query, "dryRun=All") {
		t.Fatalf("the patch query is %q, want dryRun=All", req.Query)
	}
	if !out.DryRun || !strings.Contains(out.Scope, "dry-run") {
		t.Fatalf("outcome does not record the action as preview-only: %+v", out)
	}
}

// TestRollbackPatchRemovesWhatAStrategicMergeWouldKeep is the claim the whole operation
// rests on, checked against the API server's OWN patch implementations rather than
// against a description of them.
//
// Both libraries here are the ones Kubernetes applies server-side: strategicpatch is what
// handles StrategicMergePatchType, and the RFC 6902 implementation is what handles
// JSONPatchType. Applying the same restored template through each shows the divergence
// directly — the merge keeps the container and the env var revision 2 added, because
// strategic merge merges lists by key and has no way to say "remove", while the JSON
// patch replaces the subtree and they are gone.
//
// This is a unit test of a live-cluster property, and it is worth having as one: it fails
// on a laptop in milliseconds if anyone ever "simplifies" the rollback back onto the merge
// primitive, without waiting for a kind cluster to disagree.
func TestRollbackPatchRemovesWhatAStrategicMergeWouldKeep(t *testing.T) {
	current := []byte(`{
		"apiVersion":"apps/v1","kind":"Deployment",
		"metadata":{"name":"web","namespace":"prod","resourceVersion":"2002"},
		"spec":{"template":{
			"metadata":{"labels":{"app":"web"}},
			"spec":{"containers":[
				{"name":"app","image":"web:2","env":[{"name":"FEATURE_X","value":"on"}]},
				{"name":"sidecar","image":"proxy:1"}
			]}
		}}
	}`)
	restored := `{
		"metadata":{"labels":{"app":"web"}},
		"spec":{"containers":[{"name":"app","image":"web:1"}]}
	}`

	// What MaKlaude sends.
	patch, err := jsonpatch.DecodePatch([]byte(`[{"op":"replace","path":"` + podTemplatePointer + `","value":` + restored + `}]`))
	if err != nil {
		t.Fatalf("decoding the rollback patch: %v", err)
	}
	afterJSONPatch, err := patch.Apply(current)
	if err != nil {
		t.Fatalf("applying the rollback patch: %v", err)
	}

	// What sending the same template through the merge primitive would have done.
	afterMerge, err := strategicpatch.StrategicMergePatch(current,
		[]byte(`{"spec":{"template":`+restored+`}}`), appsv1.Deployment{})
	if err != nil {
		t.Fatalf("applying the strategic merge patch: %v", err)
	}

	for _, leftover := range []string{"sidecar", "FEATURE_X"} {
		if !strings.Contains(string(afterMerge), leftover) {
			t.Fatalf("the strategic merge dropped %q, so this test no longer demonstrates why the JSON patch is needed: %s",
				leftover, afterMerge)
		}
		if strings.Contains(string(afterJSONPatch), leftover) {
			t.Fatalf("the JSON patch left %q behind; the rollback is not restoring the revision: %s", leftover, afterJSONPatch)
		}
	}
	if !strings.Contains(string(afterJSONPatch), `"image":"web:1"`) {
		t.Fatalf("the JSON patch did not restore revision 1's image: %s", afterJSONPatch)
	}
}

// decodeOps parses a recorded JSON-patch body into its operations.
func decodeOps(t *testing.T, body string) []struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
} {
	t.Helper()
	var ops []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(body), &ops); err != nil {
		t.Fatalf("the recorded patch body is not a JSON array of operations: %v (%s)", err, body)
	}
	return ops
}
