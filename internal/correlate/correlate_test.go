package correlate

import (
	"reflect"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// fixedTime is the pinned collection time threaded through every test snapshot so
// DetectedAt is deterministic and assertable.
var fixedTime = time.Date(2026, time.July, 5, 12, 0, 0, 0, time.UTC)

// baseSnapshot returns a reachable, empty snapshot for one cluster. Tests layer
// the specific failing objects onto it, then feed it through [detect.Analyze] and
// [Correlate] together, so the test exercises the real finding→incident pipeline
// rather than hand-built findings.
func baseSnapshot() health.Snapshot {
	return health.Snapshot{
		Cluster:      "prod",
		CollectedAt:  fixedTime,
		Reachability: health.Reachability{Reachable: true, ServerVersion: "v1.30.0"},
	}
}

// correlateSnapshot runs the deterministic pipeline a real caller would: analyze
// the snapshot into findings, then correlate those findings into incidents.
func correlateSnapshot(snap health.Snapshot) []Incident {
	return Correlate(snap, detect.Analyze(snap))
}

// incidentByPrimaryObject finds the single incident whose primary concerns the
// given object, failing the test if there is not exactly one.
func incidentByPrimaryObject(t *testing.T, incidents []Incident, obj detect.Object) Incident {
	t.Helper()
	var found []Incident
	for _, in := range incidents {
		if in.Primary.Object == obj {
			found = append(found, in)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one incident with primary object %s, got %d (incidents: %+v)",
			obj, len(found), incidents)
	}
	return found[0]
}

// effectObjects returns the objects of an incident's effects, for order-sensitive
// assertions.
func effectObjects(in Incident) []detect.Object {
	out := make([]detect.Object, 0, len(in.Effects))
	for i := range in.Effects {
		out = append(out, in.Effects[i].Object)
	}
	return out
}

// TestCorrelate_NodeDownStrandsPods proves the node-placement cascade: a NotReady
// node and the pods stranded on it collapse into a single incident whose primary
// is the node and whose effects are those pods.
func TestCorrelate_NodeDownStrandsPods(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{
		{Name: "node-a", Ready: false}, // the root cause
		{Name: "node-b", Ready: true},  // healthy, hosts nothing failing
	}
	s.Pods = []health.PodSignal{
		// Two pods stranded on the down node — one crashlooping, one failed.
		{Namespace: "app", Name: "web-1", Phase: "Running", Node: "node-a",
			RestartCount: 4, CrashLoopingContainers: []string{"web"}},
		{Namespace: "app", Name: "worker-1", Phase: "Failed", Node: "node-a", Failed: true, Reason: "NodeLost"},
		// A healthy pod on the good node produces no finding at all.
		{Namespace: "app", Name: "cache-1", Phase: "Running", Node: "node-b"},
	}

	incidents := correlateSnapshot(s)

	nodeObj := detect.Object{Kind: "node", Name: "node-a"}
	in := incidentByPrimaryObject(t, incidents, nodeObj)

	if in.Primary.Severity != detect.SeverityCritical {
		t.Fatalf("node incident primary severity = %v, want critical", in.Primary.Severity)
	}
	wantEffects := []detect.Object{
		{Kind: "pod", Namespace: "app", Name: "web-1"},
		{Kind: "pod", Namespace: "app", Name: "worker-1"},
	}
	if got := effectObjects(in); !reflect.DeepEqual(got, wantEffects) {
		t.Fatalf("node incident effects = %+v, want %+v", got, wantEffects)
	}

	// The whole cascade is a single incident: node primary + 2 pod effects, and no
	// other incident exists (the healthy node/pod contributed nothing).
	if len(incidents) != 1 {
		t.Fatalf("expected exactly one incident for the node cascade, got %d: %+v", len(incidents), incidents)
	}
}

// TestCorrelate_BadImageDeploymentCascade proves the owner-reference cascade: a
// bad image reference degrades a Deployment and crashes/stalls the pods its
// ReplicaSet manages; all of it collapses into one incident rooted at the
// Deployment, with the ReplicaSet and pods as effects.
func TestCorrelate_BadImageDeploymentCascade(t *testing.T) {
	s := baseSnapshot()
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "shop", Name: "api", DesiredReplicas: 3, ReadyReplicas: 0, AvailableReplicas: 0},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "shop", Name: "api-6f7d", DesiredReplicas: 3, ReadyReplicas: 0, AvailableReplicas: 0},
	}
	// Pods owned by the ReplicaSet "api-6f7d", which is named after Deployment
	// "api". One is stuck pulling the bad image (crashloop), one is Pending.
	owners := []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}}
	s.Pods = []health.PodSignal{
		{Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Running", Node: "node-a", Owners: owners,
			RestartCount: 5, CrashLoopingContainers: []string{"api"}},
		{Namespace: "shop", Name: "api-6f7d-bbbb", Phase: "Pending", Pending: true, Owners: owners},
	}

	incidents := correlateSnapshot(s)

	// The Deployment is the structural root, so it is the primary.
	depObj := detect.Object{Kind: "deployment", Namespace: "shop", Name: "api"}
	in := incidentByPrimaryObject(t, incidents, depObj)
	if in.Primary.Severity != detect.SeverityCritical {
		t.Fatalf("deployment incident primary severity = %v, want critical", in.Primary.Severity)
	}

	// Effects: the ReplicaSet and both pods, sorted by finding identity.
	wantEffects := []detect.Object{
		{Kind: "pod", Namespace: "shop", Name: "api-6f7d-aaaa"},
		{Kind: "pod", Namespace: "shop", Name: "api-6f7d-bbbb"},
		{Kind: "replicaset", Namespace: "shop", Name: "api-6f7d"},
	}
	if got := effectObjects(in); !reflect.DeepEqual(got, wantEffects) {
		t.Fatalf("deployment incident effects = %+v, want %+v", got, wantEffects)
	}

	// It is one incident: the deployment, its replicaset, and both pods.
	if len(incidents) != 1 {
		t.Fatalf("expected exactly one incident for the deployment cascade, got %d: %+v", len(incidents), incidents)
	}
}

// TestCorrelate_SingletonsAndNothingLost proves that uncorrelated problems each
// become their own singleton incident and that correlation partitions the
// findings — every input finding appears in exactly one incident, none dropped or
// duplicated.
func TestCorrelate_SingletonsAndNothingLost(t *testing.T) {
	s := baseSnapshot()
	// Three unrelated problems in different namespaces / objects, none sharing a
	// node or owner, so none should correlate.
	s.Nodes = []health.NodeSignal{{Name: "lonely-node", Ready: false}}
	s.Pods = []health.PodSignal{
		{Namespace: "team-a", Name: "orphan", Phase: "Pending", Pending: true}, // unscheduled, no owner
	}
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "team-b", Name: "detached", DesiredReplicas: 2, ReadyReplicas: 1, AvailableReplicas: 1},
	}

	findings := detect.Analyze(s)
	incidents := Correlate(s, findings)

	if len(incidents) != len(findings) {
		t.Fatalf("expected each unrelated finding to be its own singleton incident: %d incidents for %d findings",
			len(incidents), len(findings))
	}
	for _, in := range incidents {
		if len(in.Effects) != 0 {
			t.Fatalf("singleton incident %q unexpectedly has effects: %+v", in.Identity, in.Effects)
		}
	}

	// Nothing lost: the multiset of finding identities across all incidents equals
	// the input findings exactly.
	seen := make(map[detect.Identity]int)
	total := 0
	for _, in := range incidents {
		for _, f := range in.Findings() {
			seen[f.Identity]++
			total++
		}
	}
	if total != len(findings) {
		t.Fatalf("findings across incidents = %d, want %d (some lost or duplicated)", total, len(findings))
	}
	for i := range findings {
		if seen[findings[i].Identity] != 1 {
			t.Fatalf("finding %q appears %d times across incidents, want exactly 1",
				findings[i].Identity, seen[findings[i].Identity])
		}
	}
}

// TestCorrelate_StableIdentityAcrossCycles proves the core dedup property: the
// same ongoing cascade, observed in two cycles with a later timestamp and evolved
// severities/messages, yields an incident with the SAME identity.
func TestCorrelate_StableIdentityAcrossCycles(t *testing.T) {
	makeSnapshot := func(collectedAt time.Time, degradedNotDown bool) health.Snapshot {
		s := baseSnapshot()
		s.CollectedAt = collectedAt
		// In cycle two the deployment is merely degraded (some replicas back), so
		// its severity changes from critical to warning and its message changes —
		// but it is the same deployment, so the same incident.
		avail := int32(0)
		if degradedNotDown {
			avail = 2
		}
		s.Deployments = []health.DeploymentSignal{
			{Namespace: "shop", Name: "api", DesiredReplicas: 3, ReadyReplicas: avail, AvailableReplicas: avail},
		}
		owners := []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}}
		s.Pods = []health.PodSignal{
			{Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Running", Owners: owners,
				RestartCount: 5, CrashLoopingContainers: []string{"api"}},
		}
		return s
	}

	one := correlateSnapshot(makeSnapshot(fixedTime, false))
	two := correlateSnapshot(makeSnapshot(fixedTime.Add(10*time.Minute), true))

	depObj := detect.Object{Kind: "deployment", Namespace: "shop", Name: "api"}
	inOne := incidentByPrimaryObject(t, one, depObj)
	inTwo := incidentByPrimaryObject(t, two, depObj)

	if inOne.Identity != inTwo.Identity {
		t.Fatalf("incident identity changed across cycles: %q vs %q", inOne.Identity, inTwo.Identity)
	}
	// The fixture must actually have evolved severity, or the test proves nothing.
	if inOne.Severity() == inTwo.Severity() {
		t.Fatal("expected primary severity to evolve between cycles (test fixture error)")
	}
	// And DetectedAt tracks the snapshot's clock, not the incident's own.
	if !inTwo.DetectedAt.Equal(fixedTime.Add(10 * time.Minute)) {
		t.Fatalf("incident DetectedAt = %v, want the second snapshot's collection time", inTwo.DetectedAt)
	}
}

// TestCorrelate_Deterministic proves correlating the same snapshot twice yields
// byte-for-byte identical incidents, including all ordering.
func TestCorrelate_Deterministic(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false, MemoryPressure: true}}
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "shop", Name: "api", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "shop", Name: "api-6f7d", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	owners := []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}}
	s.Pods = []health.PodSignal{
		{Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Running", Node: "node-a", Owners: owners,
			CrashLoopingContainers: []string{"api"}},
		{Namespace: "shop", Name: "api-6f7d-bbbb", Phase: "Pending", Pending: true, Owners: owners},
		{Namespace: "other", Name: "solo", Phase: "Failed", Failed: true},
	}

	findings := detect.Analyze(s)
	first := Correlate(s, findings)
	second := Correlate(s, findings)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("correlation is not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if len(first) == 0 {
		t.Fatal("expected incidents for a degraded snapshot")
	}

	// Incidents must be ordered most-urgent-first by primary severity.
	for i := 1; i < len(first); i++ {
		if first[i-1].Severity() < first[i].Severity() {
			t.Fatalf("incidents not sorted by severity descending at %d: %v then %v",
				i, first[i-1].Severity(), first[i].Severity())
		}
	}
}

// TestCorrelate_Empty proves that no findings yields no incidents (and does not
// panic), and that an unreachable cluster's single finding is a lone incident.
func TestCorrelate_Empty(t *testing.T) {
	if got := Correlate(baseSnapshot(), nil); got != nil {
		t.Fatalf("Correlate with no findings = %+v, want nil", got)
	}

	s := baseSnapshot()
	s.Reachability = health.Reachability{Reachable: false, Error: "connection refused"}
	incidents := correlateSnapshot(s)
	if len(incidents) != 1 || len(incidents[0].Effects) != 0 {
		t.Fatalf("unreachable cluster should be one singleton incident, got %+v", incidents)
	}
	if incidents[0].Primary.Object.Kind != "cluster" {
		t.Fatalf("unreachable incident primary = %v, want cluster", incidents[0].Primary.Object)
	}
}
