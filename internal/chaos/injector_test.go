package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// These tests drive the real path — Experiment → kube.ChaosRestConfig → the reused
// WriteScope guard → client-go's dynamic client → the wire — against a stub API
// server, so what they assert is what MaKlaude actually SENDS rather than what it
// reports having sent. The scope's refusals are proven where the scope lives
// (internal/kube/chaosscope_test.go); what is proven here is that the injector
// builds the right scope and puts the right request in it.

// recordedRequest is what the stub saw for one request.
type recordedRequest struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Body        map[string]any
}

// stubReply is a canned response for one HTTP method.
type stubReply struct {
	status int
	body   string
}

// chaosStub is an httptest server standing in for an API server hosting the Chaos
// Mesh CRDs. It records every request and answers per-method, so a test can set up
// "the object is absent, the create succeeds" or "the create loses a race" without
// a cluster.
type chaosStub struct {
	*httptest.Server
	seen  []recordedRequest
	reply map[string]stubReply
}

const (
	statusNotFound      = `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404,"reason":"NotFound","message":"podchaos not found"}`
	statusAlreadyExists = `{"kind":"Status","apiVersion":"v1","status":"Failure","code":409,"reason":"AlreadyExists","message":"already exists"}`
	statusConflict      = `{"kind":"Status","apiVersion":"v1","status":"Failure","code":409,"reason":"Conflict","message":"uid mismatch"}`
	statusSuccess       = `{"kind":"Status","apiVersion":"v1","status":"Success"}`
	createdPodChaos     = `{"apiVersion":"chaos-mesh.org/v1alpha1","kind":"PodChaos","metadata":{"name":"maklaude-podchaos-x","namespace":"maklaude-chaos","uid":"uid-1","resourceVersion":"42"}}`
	livePodChaos        = `{"apiVersion":"chaos-mesh.org/v1alpha1","kind":"PodChaos","metadata":{"name":"maklaude-podchaos-x","namespace":"maklaude-chaos","uid":"uid-live","resourceVersion":"7"}}`
)

// newChaosStub starts a stub whose default posture is "nothing is there, and a
// create or delete succeeds".
func newChaosStub(t *testing.T) *chaosStub {
	t.Helper()
	stub := &chaosStub{reply: map[string]stubReply{
		http.MethodGet:    {http.StatusNotFound, statusNotFound},
		http.MethodPost:   {http.StatusCreated, createdPodChaos},
		http.MethodDelete: {http.StatusOK, statusSuccess},
	}}
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

		reply := stub.reply[r.Method]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(stub.Close)
	return stub
}

// answer overrides the canned reply for one method.
func (s *chaosStub) answer(method string, status int, body string) {
	s.reply[method] = stubReply{status: status, body: body}
}

// requestsFor returns every recorded request with the given method.
func (s *chaosStub) requestsFor(method string) []recordedRequest {
	var out []recordedRequest
	for _, r := range s.seen {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

// only returns the single recorded request with the given method.
func (s *chaosStub) only(t *testing.T, method string) recordedRequest {
	t.Helper()
	got := s.requestsFor(method)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 %s, got %d (all: %+v)", method, len(got), s.seen)
	}
	return got[0]
}

// assertOnlyChaosPaths fails if the injector touched anything outside the chaos API
// group. It runs in every wire test: the narrowed guarantee is about what MaKlaude
// sends, and a test that only checks the request it expected would not notice an
// extra one.
func (s *chaosStub) assertOnlyChaosPaths(t *testing.T) {
	t.Helper()
	for _, r := range s.seen {
		if !strings.HasPrefix(r.Path, kube.ChaosAPIPathPrefix) {
			t.Errorf("request outside the chaos API group: %s %s", r.Method, r.Path)
		}
	}
}

// writeKubeconfig writes a kubeconfig pointing at serverURL, with a second context
// aimed nowhere so a test proves the handle's selected context is the one used.
func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	contents := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: other
clusters:
  - name: chaos-test
    cluster:
      server: %s
      insecure-skip-tls-verify: true
  - name: elsewhere
    cluster:
      server: https://127.0.0.1:1
      insecure-skip-tls-verify: true
contexts:
  - name: maklaude
    context:
      cluster: chaos-test
      user: tester
  - name: other
    context:
      cluster: elsewhere
      user: tester
users:
  - name: tester
    user:
      token: test-token
`, serverURL)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// eligibleTarget mints a chaos capability token for a cluster whose config carries
// the human's acknowledgement. There is no other way to get one: the interface is
// sealed by an unexported method, so a test cannot stub past eligibility any more
// than production code can.
func eligibleTarget(t *testing.T, serverURL string) cluster.ChaosTarget {
	t.Helper()
	const name = "kind-lab"
	reg, err := cluster.NewRegistry(&cluster.Config{
		Clusters: []cluster.Spec{{
			Name:       name,
			Kubeconfig: writeKubeconfig(t, serverURL),
			Context:    "maklaude",
			Chaos: &cluster.ChaosEligibility{
				Cluster:         name,
				Acknowledgement: cluster.ChaosAcknowledgementFor(name),
			},
		}},
	})
	if err != nil {
		t.Fatalf("building registry: %v", err)
	}
	target, err := reg.ChaosTarget(name)
	if err != nil {
		t.Fatalf("minting chaos target: %v", err)
	}
	return target
}

// ineligibleTarget is deliberately absent: there is no such thing. A cluster with
// no acknowledgement yields no token, so the ineligible case cannot reach an
// Injector at all — it is a missing argument, not a runtime branch. The registry
// side of that is proven in internal/cluster/chaos_test.go.

func newInjectorAgainst(t *testing.T, stub *chaosStub, mode kube.ExecuteMode) *Injector {
	t.Helper()
	i, err := NewInjector(eligibleTarget(t, stub.URL), mode)
	if err != nil {
		t.Fatalf("building injector: %v", err)
	}
	return i
}

// TestNewInjector_RefusesUnlessExplicitlyEnabled proves chaos rides the SAME kill
// switch as remediation. There is no separate chaos on/off knob to leave in the
// wrong position, and the zero value — a forgotten field, an unset variable, a new
// call site — yields no injector at all.
func TestNewInjector_RefusesUnlessExplicitlyEnabled(t *testing.T) {
	stub := newChaosStub(t)
	target := eligibleTarget(t, stub.URL)

	for name, mode := range map[string]kube.ExecuteMode{
		"explicitly disabled": kube.ExecuteDisabled,
		"zero value":          kube.ExecuteMode(0),
		"unknown mode":        kube.ExecuteMode(99),
	} {
		t.Run(name, func(t *testing.T) {
			i, err := NewInjector(target, mode)
			if !errors.Is(err, kube.ErrExecutorDisabled) {
				t.Fatalf("expected kube.ErrExecutorDisabled, got: %v", err)
			}
			if i != nil {
				t.Fatal("a disabled mode must yield no injector, not an inert one")
			}
		})
	}
}

// TestNewInjector_RefusesNilTarget proves the capability is required at
// construction. A nil target is what a caller who skipped eligibility holds.
func TestNewInjector_RefusesNilTarget(t *testing.T) {
	i, err := NewInjector(nil, kube.ExecuteEnabled)
	if !errors.Is(err, cluster.ErrChaosIneligible) {
		t.Fatalf("expected cluster.ErrChaosIneligible, got: %v", err)
	}
	if i != nil {
		t.Fatal("expected no injector alongside the error")
	}
}

// TestInject_SendsOneScopedCreate is the central wire test: an absence check, then
// exactly one POST, to the collection path the scope pins, carrying the derived
// name and the validated spec.
func TestInject_SendsOneScopedCreate(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)
	e := podKill()

	got, err := i.Inject(context.Background(), e)
	if err != nil {
		t.Fatalf("injecting: %v", err)
	}
	stub.assertOnlyChaosPaths(t)

	// The absence check reads the object path; the create posts to the collection.
	read := stub.only(t, http.MethodGet)
	if read.Path != e.objectPath() {
		t.Errorf("absence check read %q, want %q", read.Path, e.objectPath())
	}

	create := stub.only(t, http.MethodPost)
	if create.Path != e.collectionPath() {
		t.Errorf("create posted to %q, want the collection %q", create.Path, e.collectionPath())
	}
	if create.Query != "" {
		t.Errorf("a real injection must carry no dryRun, got query %q", create.Query)
	}
	if !strings.HasPrefix(create.ContentType, "application/json") {
		t.Errorf("content type = %q, want JSON", create.ContentType)
	}

	meta := create.Body["metadata"].(map[string]any)
	if meta["name"] != e.ObjectName() {
		t.Errorf("created name = %v, want the derived %q", meta["name"], e.ObjectName())
	}
	if _, ok := meta["generateName"]; ok {
		t.Error("the create must not use generateName: every retry would succeed with a fresh name")
	}
	if create.Body["kind"] != "PodChaos" {
		t.Errorf("created kind = %v", create.Body["kind"])
	}

	if got.UID != "uid-1" || got.ResourceVersion != "42" {
		t.Errorf("record should carry the created identity, got uid %q rv %q", got.UID, got.ResourceVersion)
	}
	if got.Cluster != "kind-lab" || got.Name != e.ObjectName() || got.Kind != KindPodChaos {
		t.Errorf("record = %+v", got)
	}
	if got.DryRun {
		t.Error("an enabled injection is not a preview")
	}
	if want := "POST " + e.collectionPath(); got.Scope != want {
		t.Errorf("recorded scope = %q, want %q", got.Scope, want)
	}
	if !strings.Contains(got.Acknowledgement, "deliberately break the cluster named kind-lab") {
		t.Errorf("record should quote the human's consent, got %q", got.Acknowledgement)
	}
}

// TestInject_ReplayCollidesBeforeSending proves the create-shaped precondition's
// first half: an identical experiment already live is refused, and NOTHING is sent.
// This is the case a retry hits, and injecting a second copy of the same fault is
// the outcome the derived name exists to prevent.
func TestInject_ReplayCollidesBeforeSending(t *testing.T) {
	stub := newChaosStub(t)
	stub.answer(http.MethodGet, http.StatusOK, livePodChaos)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Inject(context.Background(), podKill())
	if !errors.Is(err, ErrExperimentExists) {
		t.Fatalf("expected ErrExperimentExists, got: %v", err)
	}
	if !strings.Contains(err.Error(), "uid-live") {
		t.Errorf("the refusal should name the object in the way, got: %v", err)
	}
	if posts := stub.requestsFor(http.MethodPost); len(posts) != 0 {
		t.Fatalf("nothing must be sent when the precondition fails, got %+v", posts)
	}
}

// TestInject_ReplayCollidesAtTheAPIServer proves the second half. The absence check
// is a time-of-check/time-of-use race by construction, so it cannot be the
// guarantee — the API server's uniqueness check is, and its 409 must map to the same
// sentinel so a caller sees one outcome however the race went.
func TestInject_ReplayCollidesAtTheAPIServer(t *testing.T) {
	stub := newChaosStub(t)
	stub.answer(http.MethodPost, http.StatusConflict, statusAlreadyExists)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Inject(context.Background(), podKill())
	if !errors.Is(err, ErrExperimentExists) {
		t.Fatalf("expected ErrExperimentExists, got: %v", err)
	}
}

// TestInject_SurfacesAFailedAbsenceCheck proves a read that fails is not read as
// absence. A create attempted because the check errored is a create with no
// precondition at all.
func TestInject_SurfacesAFailedAbsenceCheck(t *testing.T) {
	stub := newChaosStub(t)
	stub.answer(http.MethodGet, http.StatusInternalServerError,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","code":500,"reason":"InternalError"}`)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Inject(context.Background(), podKill())
	if !errors.Is(err, ErrInject) {
		t.Fatalf("expected ErrInject, got: %v", err)
	}
	if posts := stub.requestsFor(http.MethodPost); len(posts) != 0 {
		t.Fatalf("nothing must be sent when the check fails, got %+v", posts)
	}
}

// TestInject_DryRunPreviews proves a preview is a real, server-validated request
// that changes nothing: the POST carries dryRun=All as a query parameter, which is
// where the API server reads it for a create, and the scope would refuse the request
// without it.
func TestInject_DryRunPreviews(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteDryRun)

	got, err := i.Inject(context.Background(), podFailure())
	if err != nil {
		t.Fatalf("previewing: %v", err)
	}
	create := stub.only(t, http.MethodPost)
	if create.Query != "dryRun=All" {
		t.Errorf("preview query = %q, want dryRun=All", create.Query)
	}
	if !got.DryRun {
		t.Error("record must say the cluster is unchanged")
	}
	if !strings.HasSuffix(got.Scope, "(dry-run only)") {
		t.Errorf("recorded scope should say it was preview-only, got %q", got.Scope)
	}
}

// TestInject_RefusesAnInvalidExperimentWithoutSending proves validation happens
// before the network, so a malformed experiment costs no request at all.
func TestInject_RefusesAnInvalidExperimentWithoutSending(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	e := podKill()
	e.Selector.Namespaces = nil

	if _, err := i.Inject(context.Background(), e); !errors.Is(err, ErrInvalidExperiment) {
		t.Fatalf("expected ErrInvalidExperiment, got: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("an invalid experiment must reach no network at all, got %+v", stub.seen)
	}
}

// TestRemove_DeletesWithAUIDPrecondition proves teardown is conditioned on object
// IDENTITY. A resourceVersion would fail precisely because a live experiment's
// status keeps changing; a UID answers the question teardown actually has — is this
// the object I created?
func TestRemove_DeletesWithAUIDPrecondition(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	injected, err := i.Inject(context.Background(), podKill())
	if err != nil {
		t.Fatalf("injecting: %v", err)
	}

	removal, err := i.Remove(context.Background(), *injected)
	if err != nil {
		t.Fatalf("removing: %v", err)
	}
	stub.assertOnlyChaosPaths(t)

	del := stub.only(t, http.MethodDelete)
	if want := objectPathFor(injected.Namespace, injected.Kind, injected.Name); del.Path != want {
		t.Errorf("delete path = %q, want %q", del.Path, want)
	}
	preconditions, ok := del.Body["preconditions"].(map[string]any)
	if !ok {
		t.Fatalf("delete body carries no preconditions: %+v", del.Body)
	}
	if preconditions["uid"] != "uid-1" {
		t.Errorf("precondition uid = %v, want uid-1", preconditions["uid"])
	}
	if _, ok := preconditions["resourceVersion"]; ok {
		t.Error("teardown must not condition on resourceVersion: a live experiment's status changes constantly")
	}
	if removal.AlreadyAbsent {
		t.Error("the object was there; AlreadyAbsent must be false")
	}
}

// TestRemove_RequiresAUID proves there is no unconditional teardown to reach for.
func TestRemove_RequiresAUID(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Remove(context.Background(), Injected{
		Kind:      KindPodChaos,
		Namespace: "maklaude-chaos",
		Name:      "maklaude-podchaos-abc",
	})
	if !errors.Is(err, ErrMissingUID) {
		t.Fatalf("expected ErrMissingUID, got: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("nothing must be sent, got %+v", stub.seen)
	}
}

// TestRemove_AlreadyAbsentIsSuccess proves teardown's asymmetry. Its goal is that no
// MaKlaude experiment outlives its run, so an object already gone satisfies it — but
// the fact is REPORTED rather than smoothed into an ordinary success, because "torn
// down" and "was never there" are different facts about a cluster.
func TestRemove_AlreadyAbsentIsSuccess(t *testing.T) {
	stub := newChaosStub(t)
	stub.answer(http.MethodDelete, http.StatusNotFound, statusNotFound)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	removal, err := i.Remove(context.Background(), Injected{
		Kind:      KindPodChaos,
		Namespace: "maklaude-chaos",
		Name:      "maklaude-podchaos-abc",
		UID:       "uid-1",
	})
	if err != nil {
		t.Fatalf("an already-absent experiment is torn down: %v", err)
	}
	if !removal.AlreadyAbsent {
		t.Error("AlreadyAbsent must be set so the record does not claim a deletion that did not happen")
	}
}

// TestRemove_RefusesToDeleteARecycledName proves the UID precondition's point: if
// the name now belongs to a different object, MaKlaude's experiment is already gone
// and deleting the stranger would be a write nobody authorised.
func TestRemove_RefusesToDeleteARecycledName(t *testing.T) {
	stub := newChaosStub(t)
	stub.answer(http.MethodDelete, http.StatusConflict, statusConflict)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Remove(context.Background(), Injected{
		Kind:      KindPodChaos,
		Namespace: "maklaude-chaos",
		Name:      "maklaude-podchaos-abc",
		UID:       "uid-1",
	})
	if !errors.Is(err, kube.ErrPreconditionConflict) {
		t.Fatalf("expected kube.ErrPreconditionConflict, got: %v", err)
	}
}

// TestRemove_DryRunPreviewsInTheBody is the delete-shaped half of the dry-run
// check, and the reason it is its own test: the API server reads DeleteOptions from
// the request BODY when one is present and ignores a query dryRun, so a delete whose
// marker lands in the query would report a preview and delete for real. The scope
// guard reads the body for exactly this reason.
func TestRemove_DryRunPreviewsInTheBody(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteDryRun)

	removal, err := i.Remove(context.Background(), Injected{
		Kind:      KindPodChaos,
		Namespace: "maklaude-chaos",
		Name:      "maklaude-podchaos-abc",
		UID:       "uid-1",
	})
	if err != nil {
		t.Fatalf("previewing a teardown: %v", err)
	}
	del := stub.only(t, http.MethodDelete)
	dryRun, ok := del.Body["dryRun"].([]any)
	if !ok || len(dryRun) != 1 || dryRun[0] != "All" {
		t.Fatalf("delete body dryRun = %v, want [All] in the BODY", del.Body["dryRun"])
	}
	if !removal.DryRun {
		t.Error("record must say the experiment is still live")
	}
}

// TestRemove_RefusesAnUnknownKind proves a record with a kind this build does not
// support cannot compose a path with an empty resource segment.
func TestRemove_RefusesAnUnknownKind(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	_, err := i.Remove(context.Background(), Injected{
		Kind:      Kind("NetworkChaos"),
		Namespace: "maklaude-chaos",
		Name:      "maklaude-networkchaos-abc",
		UID:       "uid-1",
	})
	if !errors.Is(err, ErrInvalidExperiment) {
		t.Fatalf("expected ErrInvalidExperiment, got: %v", err)
	}
	if len(stub.seen) != 0 {
		t.Fatalf("nothing must be sent, got %+v", stub.seen)
	}
}

// TestCollectionPath_MatchesTheDynamicClient closes the one gap a composed path
// could hide: this package builds the scope path from kube.ChaosAPIPathPrefix while
// client-go's dynamic client derives the request path from a GroupVersionResource.
// If those ever disagree the guard refuses every experiment, so the agreement is
// load-bearing — and the create test above passing IS the proof, since the request
// reached the stub. This states it directly so a future reader does not have to
// infer it from a passing test elsewhere.
func TestCollectionPath_MatchesTheDynamicClient(t *testing.T) {
	stub := newChaosStub(t)
	i := newInjectorAgainst(t, stub, kube.ExecuteEnabled)
	e := podKill()

	if _, err := i.Inject(context.Background(), e); err != nil {
		t.Fatalf("injecting: %v", err)
	}
	wire := stub.only(t, http.MethodPost).Path
	if wire != e.collectionPath() {
		t.Fatalf("the dynamic client sent %q but the scope pinned %q", wire, e.collectionPath())
	}
	gvr := e.Kind().gvr()
	if want := kube.ChaosAPIPathPrefix + gvr.Version + "/namespaces/" + e.Namespace + "/" + gvr.Resource; wire != want {
		t.Fatalf("wire path %q does not match the GVR-derived %q", wire, want)
	}
}
