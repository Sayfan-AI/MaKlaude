package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// These tests drive the reaper against the same stub API server the injector tests
// use, over the same transport guard, so what they assert is what a sweep actually
// SENDS. Two properties carry the weight, and they pull in opposite directions:
//
//   - a sweep must remove MaKlaude's leaked experiments, or the milestone's worst
//     outcome is permanent rather than temporary;
//   - a sweep must never touch anything else, because a wrong delete on a shared Chaos
//     Mesh installation is worse than the leak it was preventing.
//
// So the negative cases below matter at least as much as the positive one, and each
// ownership signal is defeated on its own rather than all at once — a test that
// scrambles every field would pass even if only one signal were being checked.

// fixedNow is the clock every sweep in this file is measured against. Ages are the
// reaper's whole decision, and a test that reads the real clock asserts something
// slightly different on every run.
var fixedNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func testClock() func() time.Time { return func() time.Time { return fixedNow } }

const chaosNamespace = "maklaude-chaos"

// stubObject describes one PodChaos the stub will return from a LIST.
type stubObject struct {
	name        string
	uid         string
	labels      map[string]string
	annotations map[string]string
	created     time.Time
	// omitCreated writes no creationTimestamp at all, which a real API server never
	// does and which must therefore be treated as an unknown age rather than an old one.
	omitCreated bool
}

// ownedOrphan is a fully MaKlaude-owned PodChaos, old enough to sweep: every
// ownership signal present, and a name the derivation actually produces. Each negative
// test starts from this and breaks exactly one thing.
func ownedOrphan() stubObject {
	return stubObject{
		name:        minimalFor(ActionPodKill).ObjectName(),
		uid:         "uid-orphan",
		labels:      map[string]string{"app.kubernetes.io/managed-by": "maklaude", "app.kubernetes.io/component": "chaos", "app.kubernetes.io/name": "maklaude"},
		annotations: map[string]string{keyPrefix + "cluster": "kind-lab", keyPrefix + "acknowledgement": "…"},
		created:     fixedNow.Add(-1 * time.Hour),
	}
}

// listBody renders objects as the PodChaosList a LIST returns.
func listBody(t *testing.T, objects ...stubObject) string {
	t.Helper()
	items := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		meta := map[string]any{
			"name":            o.name,
			"namespace":       chaosNamespace,
			"uid":             o.uid,
			"resourceVersion": "7",
			"labels":          o.labels,
			"annotations":     o.annotations,
		}
		if !o.omitCreated {
			meta["creationTimestamp"] = o.created.UTC().Format(time.RFC3339)
		}
		items = append(items, map[string]any{
			"apiVersion": APIGroup + "/" + APIVersion,
			"kind":       string(KindPodChaos),
			"metadata":   meta,
		})
	}
	raw, err := json.Marshal(map[string]any{
		"apiVersion": APIGroup + "/" + APIVersion,
		"kind":       string(KindPodChaos) + "List",
		"metadata":   map[string]any{"resourceVersion": "9"},
		"items":      items,
	})
	if err != nil {
		t.Fatalf("marshalling list body: %v", err)
	}
	return string(raw)
}

// newReaperAgainst builds an injector and a reaper over the stub, with the stub set up
// to return objects from a LIST.
func newReaperAgainst(t *testing.T, stub *chaosStub, mode kube.ExecuteMode, objects ...stubObject) *Reaper {
	t.Helper()
	stub.answer(http.MethodGet, http.StatusOK, listBody(t, objects...))
	r, err := NewReaper(newInjectorAgainst(t, stub, mode), DefaultOrphanGrace, testClock())
	if err != nil {
		t.Fatalf("building reaper: %v", err)
	}
	return r
}

// TestNewReaper_RefusesAGraceThatCouldReachALiveExperiment is the reaper's central
// safety argument, checked at its edge.
//
// There is no exclusion list of live experiment names — deliberately, because that
// would be a convention every call site has to remember. What replaces it is
// arithmetic: no fault this package asks for can outlive [maxDuration], so an owned
// object older than that cannot belong to a live experiment under ANY MaKlaude
// process. A grace below that ceiling voids the argument, so it is refused rather than
// clamped. Zero gets refused by the same comparison, which is the case that matters:
// it is what a forgotten field gets, and "reap everything owned, however young" is
// both plausible-looking and destructive.
func TestNewReaper_RefusesAGraceThatCouldReachALiveExperiment(t *testing.T) {
	stub := newChaosStub(t)
	inj := newInjectorAgainst(t, stub, kube.ExecuteEnabled)

	for name, grace := range map[string]time.Duration{
		"zero (a forgotten field)":         0,
		"negative":                         -time.Minute,
		"a second under the fault ceiling": MaxDuration() - time.Second,
		"a nanosecond under":               MaxDuration() - time.Nanosecond,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := NewReaper(inj, grace, testClock())
			if !errors.Is(err, ErrReaperMisconfigured) {
				t.Fatalf("expected ErrReaperMisconfigured, got %v", err)
			}
			if r != nil {
				t.Fatal("a misconfigured reaper must not exist")
			}
			if !strings.Contains(err.Error(), MaxDuration().String()) {
				t.Errorf("the refusal should name the ceiling it is derived from, got %q", err)
			}
		})
	}

	for name, grace := range map[string]time.Duration{
		"exactly the ceiling": MaxDuration(),
		"the default":         DefaultOrphanGrace,
		"generous":            time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := NewReaper(inj, grace, testClock())
			if err != nil {
				t.Fatalf("expected a reaper, got %v", err)
			}
			if r.Grace() != grace {
				t.Errorf("Grace() = %s, want %s", r.Grace(), grace)
			}
			if r.Cluster() != "kind-lab" {
				t.Errorf("Cluster() = %q, want the injector's cluster", r.Cluster())
			}
		})
	}
}

// TestNewReaper_RefusesNilInjector proves a reaper cannot exist without the injector
// that binds it to one eligible cluster and one kill-switch setting. There is no
// separate chaos target or execute mode to set differently here, which is the point of
// holding the injector rather than copying its fields.
func TestNewReaper_RefusesNilInjector(t *testing.T) {
	r, err := NewReaper(nil, DefaultOrphanGrace, testClock())
	if !errors.Is(err, ErrReaperMisconfigured) {
		t.Fatalf("expected ErrReaperMisconfigured, got %v", err)
	}
	if r != nil {
		t.Fatal("expected no reaper")
	}
}

// TestNewReaper_DefaultsANilClock proves the clock is optional for production callers
// while remaining injectable for tests.
func TestNewReaper_DefaultsANilClock(t *testing.T) {
	stub := newChaosStub(t)
	r, err := NewReaper(newInjectorAgainst(t, stub, kube.ExecuteEnabled), DefaultOrphanGrace, nil)
	if err != nil {
		t.Fatalf("building reaper: %v", err)
	}
	if r.now == nil {
		t.Fatal("a nil clock must default to time.Now, not stay nil")
	}
}

// TestReap_RemovesAnOrphanWithItsUID is the positive case: a leaked experiment that is
// unmistakably MaKlaude's and older than the grace is deleted, conditioned on the UID
// the LIST returned.
//
// The UID precondition is what makes the delete safe against the one race the sweep
// cannot otherwise close: between the list and the delete, the object could be removed
// and its name reused by somebody else. A name-only delete would follow that; this one
// is refused by the API server.
func TestReap_RemovesAnOrphanWithItsUID(t *testing.T) {
	stub := newChaosStub(t)
	orphan := ownedOrphan()
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, orphan)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	stub.assertOnlyChaosPaths(t)

	if sweep.Scanned != 1 || len(sweep.Reaped) != 1 || len(sweep.Skipped) != 0 || len(sweep.Failed) != 0 {
		t.Fatalf("sweep = %+v, want exactly one reaped orphan", sweep)
	}
	if sweep.Reaped[0].Name != orphan.name || sweep.Reaped[0].Kind != KindPodChaos {
		t.Errorf("reaped %s %q, want PodChaos %q", sweep.Reaped[0].Kind, sweep.Reaped[0].Name, orphan.name)
	}
	if sweep.Cluster != "kind-lab" || sweep.Namespace != chaosNamespace || sweep.DryRun {
		t.Errorf("sweep record = %+v", sweep)
	}

	del := stub.only(t, http.MethodDelete)
	if want := "/" + KindPodChaos.Resource() + "/" + orphan.name; !strings.HasSuffix(del.Path, want) {
		t.Errorf("delete path %q does not end in %q", del.Path, want)
	}
	preconditions, ok := del.Body["preconditions"].(map[string]any)
	if !ok {
		t.Fatalf("the delete carried no preconditions: %+v", del.Body)
	}
	if preconditions["uid"] != orphan.uid {
		t.Errorf("uid precondition = %v, want %q", preconditions["uid"], orphan.uid)
	}
}

// TestReap_ListsThroughAClientThatCannotWrite proves the read that decides what to
// delete carries none of the authority to delete.
//
// The LIST client is built with the zero-value [kube.WriteScope], which the transport
// admits for reads and refuses for every mutation — so the enumeration cannot mutate
// anything even if a future change asked it to. The label selector is asserted too,
// but as an optimisation rather than as the ownership test: the point of
// [Reaper.ownershipProblem] re-checking labels locally is that this filter being
// ignored must not lead to a delete.
func TestReap_ListsThroughAClientThatCannotWrite(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled)

	if _, err := r.Reap(context.Background(), chaosNamespace); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	stub.assertOnlyChaosPaths(t)

	list := stub.only(t, http.MethodGet)
	wantPath := kube.ChaosAPIPathPrefix + APIVersion + "/namespaces/" + chaosNamespace + "/" + KindPodChaos.Resource()
	if list.Path != wantPath {
		t.Errorf("list path = %q, want the collection %q", list.Path, wantPath)
	}
	query, err := url.ParseQuery(list.Query)
	if err != nil {
		t.Fatalf("parsing list query %q: %v", list.Query, err)
	}
	for key, value := range ownershipLabels {
		if !strings.Contains(query.Get("labelSelector"), key+"="+value) {
			t.Errorf("labelSelector %q does not filter on %s=%s", query.Get("labelSelector"), key, value)
		}
	}
	if len(stub.requestsFor(http.MethodDelete)) != 0 {
		t.Errorf("an empty cluster must produce no deletes, got %+v", stub.requestsFor(http.MethodDelete))
	}
	// A read scope forbids a mutation outright, so the sweep provably cannot have
	// widened its own authority to look around.
	if _, err := kube.ChaosRestConfig(eligibleTarget(t, stub.URL), kube.WriteScope{
		Method: http.MethodDelete, Path: "/api/v1/namespaces/default/pods/x",
	}); !errors.Is(err, kube.ErrNotChaosScope) {
		t.Errorf("the chaos door must refuse a non-chaos mutation, got %v", err)
	}
}

// TestReap_LeavesForeignExperimentsAlone is the test this whole design exists for.
//
// Chaos Mesh is shared: a human's own experiment sits in the same namespace and looks
// broadly the same. Each case below defeats exactly ONE ownership signal and leaves
// the rest intact, because a case that scrambled everything would pass even if only
// one signal were checked. Every one of them must produce zero deletes.
func TestReap_LeavesForeignExperimentsAlone(t *testing.T) {
	cases := map[string]struct {
		mutate func(*stubObject)
		want   string
	}{
		"not managed by maklaude": {
			mutate: func(o *stubObject) { o.labels["app.kubernetes.io/managed-by"] = "somebody-else" },
			want:   "app.kubernetes.io/managed-by",
		},
		"managed-by label absent entirely": {
			mutate: func(o *stubObject) { delete(o.labels, "app.kubernetes.io/managed-by") },
			want:   "app.kubernetes.io/managed-by",
		},
		"a different maklaude component": {
			mutate: func(o *stubObject) { o.labels["app.kubernetes.io/component"] = "remediation" },
			want:   "app.kubernetes.io/component",
		},
		"no labels at all (the server ignored the selector)": {
			mutate: func(o *stubObject) { o.labels = nil },
			want:   "app.kubernetes.io/component",
		},
		"authorised for a different cluster": {
			mutate: func(o *stubObject) { o.annotations[keyPrefix+"cluster"] = "prod-eu" },
			want:   "not this cluster",
		},
		"no cluster annotation": {
			mutate: func(o *stubObject) { o.annotations = nil },
			want:   "not this cluster",
		},
		"a hand-written name": {
			mutate: func(o *stubObject) { o.name = "my-own-experiment" },
			want:   "not a MaKlaude-derived experiment name",
		},
		"MaKlaude's prefix with a generated suffix": {
			mutate: func(o *stubObject) { o.name = "maklaude-podchaos-abc123" },
			want:   "not a MaKlaude-derived experiment name",
		},
		"a derived name for a different kind": {
			mutate: func(o *stubObject) { o.name = "maklaude-networkchaos-0123456789ab" },
			want:   "does not carry the kind",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newChaosStub(t)
			obj := ownedOrphan()
			tc.mutate(&obj)
			r := newReaperAgainst(t, stub, kube.ExecuteEnabled, obj)

			sweep, err := r.Reap(context.Background(), chaosNamespace)
			if err != nil {
				t.Fatalf("leaving an object alone is not a failure, got: %v", err)
			}
			if got := stub.requestsFor(http.MethodDelete); len(got) != 0 {
				t.Fatalf("a foreign experiment must never be deleted, got %+v", got)
			}
			if len(sweep.Reaped) != 0 || len(sweep.Skipped) != 1 {
				t.Fatalf("sweep = %+v, want exactly one skip and no reaps", sweep)
			}
			if !strings.Contains(sweep.Skipped[0].Reason, tc.want) {
				t.Errorf("skip reason %q does not name the signal %q", sweep.Skipped[0].Reason, tc.want)
			}
		})
	}
}

// TestReap_LeavesAYoungExperimentAlone proves the grace is applied, which is what
// protects a fault that is still running — including one belonging to a concurrently
// running MaKlaude this process knows nothing about.
//
// The edge is checked at one SECOND rather than one nanosecond, because
// metav1.Time serialises as RFC3339 to second precision: a sub-second offset written
// into a creationTimestamp is truncated on the way through the API, so a
// nanosecond-under case would assert something the wire cannot express. The
// exactly-at-the-grace case — which must be reaped — is asserted in
// [TestReap_RemovesAnOrphanAtExactlyTheGrace].
func TestReap_LeavesAYoungExperimentAlone(t *testing.T) {
	for name, age := range map[string]time.Duration{
		"created this instant":             0,
		"a minute old":                     time.Minute,
		"a second under the grace":         DefaultOrphanGrace - time.Second,
		"mid-fault at the fault's ceiling": MaxDuration(),
	} {
		t.Run(name, func(t *testing.T) {
			stub := newChaosStub(t)
			obj := ownedOrphan()
			obj.created = fixedNow.Add(-age)
			r := newReaperAgainst(t, stub, kube.ExecuteEnabled, obj)

			sweep, err := r.Reap(context.Background(), chaosNamespace)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if got := stub.requestsFor(http.MethodDelete); len(got) != 0 {
				t.Fatalf("an experiment under the grace must not be deleted, got %+v", got)
			}
			if len(sweep.Skipped) != 1 || !strings.Contains(sweep.Skipped[0].Reason, "orphan grace") {
				t.Fatalf("sweep = %+v, want one skip citing the grace", sweep)
			}
		})
	}
}

// TestReap_RemovesAnOrphanAtExactlyTheGrace pins the inclusive side of the comparison.
// The skip is `age < grace`, so an object that has just reached the grace is swept —
// asserted separately from the under-grace cases because an off-by-one here is the
// difference between a leak that clears on the next cycle and one that never does.
func TestReap_RemovesAnOrphanAtExactlyTheGrace(t *testing.T) {
	stub := newChaosStub(t)
	obj := ownedOrphan()
	obj.created = fixedNow.Add(-DefaultOrphanGrace)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, obj)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(sweep.Reaped) != 1 || len(sweep.Skipped) != 0 {
		t.Fatalf("sweep = %+v, want the orphan removed at exactly the grace", sweep)
	}
}

// TestReap_LeavesAnObjectWithNoCreationTimestampAlone: an unknown age is not an old
// age. A real API server always stamps this, so reaching the case means something
// between MaKlaude and the object dropped it — and inferring "old enough" from a
// missing field is how a live experiment gets deleted. The object is still there next
// sweep, so skipping costs nothing.
func TestReap_LeavesAnObjectWithNoCreationTimestampAlone(t *testing.T) {
	stub := newChaosStub(t)
	obj := ownedOrphan()
	obj.omitCreated = true
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, obj)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := stub.requestsFor(http.MethodDelete); len(got) != 0 {
		t.Fatalf("an object of unknown age must not be deleted, got %+v", got)
	}
	if len(sweep.Skipped) != 1 || !strings.Contains(sweep.Skipped[0].Reason, "age cannot be established") {
		t.Fatalf("sweep = %+v, want one skip citing the unknown age", sweep)
	}
}

// TestReap_DryRunPreviews proves the sweep rides the same kill switch as everything
// else on the write path: under dry-run the deletes are server-side previews, so every
// orphan is still on the cluster and the record says so.
func TestReap_DryRunPreviews(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteDryRun, ownedOrphan())

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !sweep.DryRun || len(sweep.Reaped) != 1 || !sweep.Reaped[0].DryRun {
		t.Fatalf("sweep = %+v, want a previewed removal", sweep)
	}

	del := stub.only(t, http.MethodDelete)
	dryRun, ok := del.Body["dryRun"].([]any)
	if !ok || len(dryRun) != 1 || dryRun[0] != "All" {
		t.Errorf("the delete must carry dryRun: [All], got %v", del.Body["dryRun"])
	}
}

// TestReap_ContinuesAfterAFailedDelete: five leaked experiments where the first delete
// is denied is exactly the situation where the other four matter most, so a failure
// must not abort the sweep. Both orphans are attempted, both failures are on the
// record, and the error says the sweep was incomplete rather than that it succeeded.
func TestReap_ContinuesAfterAFailedDelete(t *testing.T) {
	stub := newChaosStub(t)
	first := ownedOrphan()
	second := ownedOrphan()
	second.name = minimalFor(ActionPodFailure).ObjectName()
	second.uid = "uid-orphan-2"
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, first, second)
	stub.answer(http.MethodDelete, http.StatusForbidden,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","code":403,"reason":"Forbidden","message":"denied"}`)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if !errors.Is(err, ErrReapIncomplete) {
		t.Fatalf("expected ErrReapIncomplete, got %v", err)
	}
	if sweep == nil {
		t.Fatal("the sweep record must survive a partial failure — it is what says which objects are still out there")
	}
	if len(stub.requestsFor(http.MethodDelete)) != 2 {
		t.Errorf("one failed delete must not abort the sweep: attempted %d of 2",
			len(stub.requestsFor(http.MethodDelete)))
	}
	if len(sweep.Failed) != 2 || len(sweep.Reaped) != 0 {
		t.Fatalf("sweep = %+v, want two failures", sweep)
	}
	for _, f := range sweep.Failed {
		if !errors.Is(f.Err, ErrInject) {
			t.Errorf("failure for %q should wrap the delete's own error, got %v", f.Name, f.Err)
		}
	}
}

// TestReap_ReportsARecycledNameAsAConflictRatherThanAsSuccess proves the UID
// precondition's refusal survives into the record. If the object under that name is no
// longer the one the LIST saw, MaKlaude's own experiment is already gone and somebody
// else's is in its place — a fact worth keeping, and emphatically not a delete to
// retry without the precondition.
func TestReap_ReportsARecycledNameAsAConflictRatherThanAsSuccess(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, ownedOrphan())
	stub.answer(http.MethodDelete, http.StatusConflict, statusConflict)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if !errors.Is(err, ErrReapIncomplete) {
		t.Fatalf("expected ErrReapIncomplete, got %v", err)
	}
	if len(sweep.Failed) != 1 || !errors.Is(sweep.Failed[0].Err, kube.ErrPreconditionConflict) {
		t.Fatalf("sweep = %+v, want one precondition conflict", sweep)
	}
}

// TestReap_TreatsAnAlreadyAbsentOrphanAsRemoved: two sweeps can overlap, and a
// concurrent MaKlaude may have torn the same object down. The goal is that no
// experiment outlives its run, and an object that is already gone satisfies it.
func TestReap_TreatsAnAlreadyAbsentOrphanAsRemoved(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, ownedOrphan())
	stub.answer(http.MethodDelete, http.StatusNotFound, statusNotFound)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("an already-absent orphan is a success, got: %v", err)
	}
	if len(sweep.Reaped) != 1 || !sweep.Reaped[0].AlreadyAbsent {
		t.Fatalf("sweep = %+v, want one removal flagged AlreadyAbsent", sweep)
	}
}

// TestReap_SurfacesAFailedList is the failure a caller must never read as "nothing
// leaked". A sweep that cannot enumerate the cluster knows nothing about what is on it,
// and reporting that as a clean sweep is how a leak becomes invisible.
func TestReap_SurfacesAFailedList(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, ownedOrphan())
	stub.answer(http.MethodGet, http.StatusInternalServerError,
		`{"kind":"Status","apiVersion":"v1","status":"Failure","code":500,"reason":"InternalError","message":"boom"}`)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if !errors.Is(err, ErrReapFailed) {
		t.Fatalf("expected ErrReapFailed, got %v", err)
	}
	if sweep.Scanned != 0 || len(sweep.Reaped) != 0 {
		t.Errorf("sweep = %+v, want nothing scanned and nothing removed", sweep)
	}
	if got := stub.requestsFor(http.MethodDelete); len(got) != 0 {
		t.Errorf("a sweep that could not list must delete nothing, got %+v", got)
	}
}

// TestReap_RefusesABadNamespaceWithoutSending keeps the sweep's path composition under
// the same constraint as every other request here: a namespace is a path segment, and
// a value carrying a slash or a traversal would address something other than what it
// claims to.
func TestReap_RefusesABadNamespaceWithoutSending(t *testing.T) {
	for _, ns := range []string{"", "   ", "Bad_Namespace", "../../secrets", "a/b"} {
		stub := newChaosStub(t)
		r := newReaperAgainst(t, stub, kube.ExecuteEnabled, ownedOrphan())

		_, err := r.Reap(context.Background(), ns)
		if !errors.Is(err, ErrReapFailed) || !errors.Is(err, ErrInvalidExperiment) {
			t.Errorf("namespace %q: expected a refusal wrapping both sentinels, got %v", ns, err)
		}
		if got := stub.recorded(); len(got) != 0 {
			t.Errorf("namespace %q: nothing must be sent, got %+v", ns, got)
		}
	}
}

// TestReap_VisitsEveryKindInTheCatalog guards the addition case: the catalog has one
// kind today, and a kind added later must be swept without anybody remembering to add
// it here. The reaper iterates [kindResource], so this asserts the sweep issued one
// LIST per kind rather than one LIST for the kind that happened to be first.
func TestReap_VisitsEveryKindInTheCatalog(t *testing.T) {
	stub := newChaosStub(t)
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled)

	if _, err := r.Reap(context.Background(), chaosNamespace); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	lists := stub.requestsFor(http.MethodGet)
	if len(lists) != len(kindResource) {
		t.Fatalf("issued %d LISTs for %d kinds in the catalog", len(lists), len(kindResource))
	}
	for kind, resource := range kindResource {
		found := false
		for _, l := range lists {
			if strings.HasSuffix(l.Path, "/"+resource) {
				found = true
			}
		}
		if !found {
			t.Errorf("no LIST covered kind %s (resource %q); paths: %+v", kind, resource, lists)
		}
	}
}

// TestSweepReasons_QuoteNothingButIdentifiers is a leak check on the record.
//
// A sweep is meant to be logged and quoted in an escalation, and it is assembled from
// fields of objects MaKlaude did not create. The reasons may name labels, annotation
// values and object names — all operator-configured Kubernetes identifiers — and must
// not carry object contents. This asserts the shape by planting a marker in a field
// the reasons do not read.
func TestSweepReasons_QuoteNothingButIdentifiers(t *testing.T) {
	const marker = "SENSITIVE-MARKER"
	stub := newChaosStub(t)
	obj := ownedOrphan()
	obj.labels["app.kubernetes.io/managed-by"] = "somebody-else"
	obj.annotations[keyPrefix+"acknowledgement"] = marker
	r := newReaperAgainst(t, stub, kube.ExecuteEnabled, obj)

	sweep, err := r.Reap(context.Background(), chaosNamespace)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	rendered := fmt.Sprintf("%+v", sweep)
	if strings.Contains(rendered, marker) {
		t.Errorf("the sweep record echoed an unrelated annotation value: %s", rendered)
	}
}
