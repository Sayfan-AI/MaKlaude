package diagnose

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// fixedTime is the pinned collection time threaded through every test snapshot so
// DetectedAt is deterministic and assertable.
var fixedTime = time.Date(2026, time.July, 5, 12, 0, 0, 0, time.UTC)

// baseSnapshot returns a reachable, empty snapshot for one cluster. Tests layer
// the specific failing objects onto it, then feed it through the real
// detect→correlate→diagnose pipeline rather than hand-built incidents.
func baseSnapshot() health.Snapshot {
	return health.Snapshot{
		Cluster:      "prod",
		CollectedAt:  fixedTime,
		Reachability: health.Reachability{Reachable: true, ServerVersion: "v1.30.0"},
	}
}

// diagnoseSnapshot runs the deterministic pipeline a real caller would: analyze
// the snapshot into findings, correlate those into incidents, and diagnose all
// of them.
func diagnoseSnapshot(snap health.Snapshot) []Hypothesis {
	incidents := correlate.Correlate(snap, detect.Analyze(snap))
	return Incidents(snap, incidents)
}

// hypothesisByCause returns the single hypothesis with the given cause, failing
// the test if there is not exactly one.
func hypothesisByCause(t *testing.T, hyps []Hypothesis, cause Cause) Hypothesis {
	t.Helper()
	var found []Hypothesis
	for _, h := range hyps {
		if h.Cause == cause {
			found = append(found, h)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one hypothesis with cause %q, got %d (hypotheses: %+v)", cause, len(found), hyps)
	}
	return found[0]
}

// evidenceObjects returns the objects cited as evidence, for order-sensitive
// assertions.
func evidenceObjects(h Hypothesis) []detect.Object {
	out := make([]detect.Object, 0, len(h.Evidence))
	for i := range h.Evidence {
		out = append(out, h.Evidence[i].Object)
	}
	return out
}

// TestDiagnose_BadImageCascade proves the bad-image rule: a Deployment made
// unavailable by a pod whose container is in ImagePullBackOff is diagnosed as
// CauseBadImage at High confidence, citing the deployment/replicaset/pod
// findings.
func TestDiagnose_BadImageCascade(t *testing.T) {
	s := baseSnapshot()
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "shop", Name: "api", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "shop", Name: "api-6f7d", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	owners := []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}}
	s.Pods = []health.PodSignal{
		{Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Pending", Pending: true, Owners: owners,
			Containers: []health.ContainerSignal{
				{Name: "api", WaitingReason: "ImagePullBackOff", WaitingMessage: "Back-off pulling image \"api:nope\""},
			}},
	}

	hyps := diagnoseSnapshot(s)

	h := hypothesisByCause(t, hyps, CauseBadImage)
	if h.Confidence != ConfidenceHigh {
		t.Fatalf("bad-image confidence = %v, want high", h.Confidence)
	}
	if h.Source != SourceDeterministic {
		t.Fatalf("source = %q, want deterministic", h.Source)
	}
	if !h.DetectedAt.Equal(fixedTime) {
		t.Fatalf("DetectedAt = %v, want inherited %v", h.DetectedAt, fixedTime)
	}
	// Evidence cites the whole workload cascade, in the incident's stable order
	// (deployment primary first, then effects sorted by identity).
	want := []detect.Object{
		{Kind: "deployment", Namespace: "shop", Name: "api"},
		{Kind: "pod", Namespace: "shop", Name: "api-6f7d-aaaa"},
		{Kind: "replicaset", Namespace: "shop", Name: "api-6f7d"},
	}
	if got := evidenceObjects(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("bad-image evidence = %+v, want %+v", got, want)
	}
	// It must not also be reported as the generic fallback.
	for _, x := range hyps {
		if x.Cause == CauseUnknown {
			t.Fatalf("unexpected fallback hypothesis alongside a matched rule: %+v", x)
		}
	}
}

// TestDiagnose_InsufficientResources proves the resource rule: a Pending pod
// with a FailedScheduling "Insufficient" event is diagnosed as
// CauseInsufficientResources at High confidence, and the message names the short
// resources.
func TestDiagnose_InsufficientResources(t *testing.T) {
	s := baseSnapshot()
	s.Pods = []health.PodSignal{
		{Namespace: "batch", Name: "big-job", Phase: "Pending", Pending: true,
			Requests: health.ResourceList{CPU: "8", Memory: "16Gi"}},
	}
	s.WarningEvents = []health.EventSignal{
		{Namespace: "batch", Name: "big-job.1", Reason: "FailedScheduling", Count: 3,
			InvolvedObject: "Pod/batch/big-job", LastSeen: fixedTime,
			Message: "0/3 nodes are available: 3 Insufficient cpu, 3 Insufficient memory."},
	}

	hyps := diagnoseSnapshot(s)

	h := hypothesisByCause(t, hyps, CauseInsufficientResources)
	if h.Confidence != ConfidenceHigh {
		t.Fatalf("insufficient-resources confidence = %v, want high", h.Confidence)
	}
	if want := "insufficient allocatable cpu and memory"; !strings.Contains(h.Message, want) {
		t.Fatalf("message %q does not mention %q", h.Message, want)
	}
	want := []detect.Object{{Kind: "pod", Namespace: "batch", Name: "big-job"}}
	if got := evidenceObjects(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence = %+v, want %+v", got, want)
	}
}

// TestDiagnose_PendingWithoutInsufficientEvent proves the resource rule does NOT
// fire on a bare Pending pod (no Insufficient scheduling signal); the incident
// falls back to the generic hypothesis instead of over-claiming a resource
// shortage.
func TestDiagnose_PendingWithoutInsufficientEvent(t *testing.T) {
	s := baseSnapshot()
	s.Pods = []health.PodSignal{
		{Namespace: "batch", Name: "waiting", Phase: "Pending", Pending: true},
	}

	hyps := diagnoseSnapshot(s)

	for _, h := range hyps {
		if h.Cause == CauseInsufficientResources {
			t.Fatalf("resource rule fired without an Insufficient event: %+v", h)
		}
	}
	// The single Pending finding must still yield a hypothesis (the fallback).
	fb := hypothesisByCause(t, hyps, CauseUnknown)
	if fb.Confidence != ConfidenceLow {
		t.Fatalf("fallback confidence = %v, want low", fb.Confidence)
	}
}

// TestDiagnose_NodeFailure proves the node rule: a NotReady node stranding pods
// is diagnosed as CauseNodeFailure at High confidence, citing the node and the
// stranded pods.
func TestDiagnose_NodeFailure(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	s.Pods = []health.PodSignal{
		{Namespace: "app", Name: "web-1", Phase: "Running", Node: "node-a",
			RestartCount: 4, CrashLoopingContainers: []string{"web"}},
		{Namespace: "app", Name: "worker-1", Phase: "Failed", Node: "node-a", Failed: true, Reason: "NodeLost"},
	}

	hyps := diagnoseSnapshot(s)

	h := hypothesisByCause(t, hyps, CauseNodeFailure)
	if h.Confidence != ConfidenceHigh {
		t.Fatalf("node-failure confidence = %v, want high", h.Confidence)
	}
	want := []detect.Object{
		{Kind: "node", Name: "node-a"},
		{Kind: "pod", Namespace: "app", Name: "web-1"},
		{Kind: "pod", Namespace: "app", Name: "worker-1"},
	}
	if got := evidenceObjects(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("node-failure evidence = %+v, want %+v", got, want)
	}
	if !strings.Contains(h.Message, "disrupting 2 pod(s)") {
		t.Fatalf("message %q should report the disrupted pod count", h.Message)
	}
}

// TestDiagnose_OOMKill_CurrentVsHistorical proves the OOM rule and its coarse
// confidence: a container presently OOM-terminated is High, while one OOM-killed
// only on a previous instance is Medium.
func TestDiagnose_OOMKill_CurrentVsHistorical(t *testing.T) {
	oom := func(current bool) health.Snapshot {
		s := baseSnapshot()
		c := health.ContainerSignal{Name: "app", RestartCount: 6, CrashLooping: true,
			WaitingReason: "CrashLoopBackOff"}
		term := &health.TerminationSignal{Reason: "OOMKilled", ExitCode: 137}
		if current {
			c.CurrentTermination = term
		} else {
			c.LastTermination = term
		}
		s.Pods = []health.PodSignal{
			{Namespace: "svc", Name: "cache-1", Phase: "Running",
				RestartCount: 6, CrashLoopingContainers: []string{"app"},
				Containers: []health.ContainerSignal{c}},
		}
		return s
	}

	hi := hypothesisByCause(t, diagnoseSnapshot(oom(true)), CauseOOMKill)
	if hi.Confidence != ConfidenceHigh {
		t.Fatalf("current OOM confidence = %v, want high", hi.Confidence)
	}
	lo := hypothesisByCause(t, diagnoseSnapshot(oom(false)), CauseOOMKill)
	if lo.Confidence != ConfidenceMedium {
		t.Fatalf("historical OOM confidence = %v, want medium", lo.Confidence)
	}
	// Same cause + same incident ⇒ same stable identity regardless of confidence.
	if hi.Identity != lo.Identity {
		t.Fatalf("OOM identity changed with confidence: %q vs %q", hi.Identity, lo.Identity)
	}
}

// TestDiagnose_Fallback proves that an incident matching no specialized rule
// still yields exactly one generic hypothesis citing the primary finding.
func TestDiagnose_Fallback(t *testing.T) {
	s := baseSnapshot()
	// A cordoned node: a real finding, but no cascade rule covers it.
	s.Nodes = []health.NodeSignal{{Name: "node-x", Ready: true, Unschedulable: true}}

	hyps := diagnoseSnapshot(s)

	if len(hyps) != 1 {
		t.Fatalf("expected exactly one hypothesis for the lone incident, got %d: %+v", len(hyps), hyps)
	}
	fb := hyps[0]
	if fb.Cause != CauseUnknown || fb.Confidence != ConfidenceLow {
		t.Fatalf("fallback = cause %q conf %v, want unknown/low", fb.Cause, fb.Confidence)
	}
	want := []detect.Object{{Kind: "node", Name: "node-x"}}
	if got := evidenceObjects(fb); !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback evidence = %+v, want the primary %+v", got, want)
	}
}

// TestDiagnose_MultipleHypothesesRanked proves an incident can yield several
// hypotheses and that they come back ranked most-confident-first. A node-down
// incident that also contains an OOM-killed pod yields both a node-failure
// (High) and an OOM (Medium) hypothesis, in that order.
func TestDiagnose_MultipleHypothesesRanked(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	s.Pods = []health.PodSignal{
		{Namespace: "app", Name: "web-1", Phase: "Running", Node: "node-a",
			RestartCount: 6, CrashLoopingContainers: []string{"web"},
			Containers: []health.ContainerSignal{
				{Name: "web", RestartCount: 6, CrashLooping: true, WaitingReason: "CrashLoopBackOff",
					LastTermination: &health.TerminationSignal{Reason: "OOMKilled", ExitCode: 137}},
			}},
	}

	hyps := diagnoseSnapshot(s)

	// Exactly the two specialized causes (no fallback), ranked High before Medium.
	if len(hyps) != 2 {
		t.Fatalf("expected two hypotheses, got %d: %+v", len(hyps), hyps)
	}
	if hyps[0].Cause != CauseNodeFailure || hyps[0].Confidence != ConfidenceHigh {
		t.Fatalf("first hypothesis = %q/%v, want nodefailure/high", hyps[0].Cause, hyps[0].Confidence)
	}
	if hyps[1].Cause != CauseOOMKill || hyps[1].Confidence != ConfidenceMedium {
		t.Fatalf("second hypothesis = %q/%v, want oomkill/medium", hyps[1].Cause, hyps[1].Confidence)
	}
	// Ranking invariant: confidence never increases down the list.
	for i := 1; i < len(hyps); i++ {
		if hyps[i-1].Confidence < hyps[i].Confidence {
			t.Fatalf("hypotheses not sorted by confidence descending at %d", i)
		}
	}
}

// TestDiagnose_StableIdentityAcrossCycles proves the dedup property: the same
// ongoing cause, observed in a later cycle with an evolved confidence and
// timestamp, keeps the SAME hypothesis identity.
func TestDiagnose_StableIdentityAcrossCycles(t *testing.T) {
	makeSnap := func(at time.Time, current bool) health.Snapshot {
		s := baseSnapshot()
		s.CollectedAt = at
		c := health.ContainerSignal{Name: "app", RestartCount: 6, CrashLooping: true, WaitingReason: "CrashLoopBackOff"}
		term := &health.TerminationSignal{Reason: "OOMKilled", ExitCode: 137}
		if current {
			c.CurrentTermination = term
		} else {
			c.LastTermination = term
		}
		s.Pods = []health.PodSignal{
			{Namespace: "svc", Name: "cache-1", Phase: "Running", RestartCount: 6,
				CrashLoopingContainers: []string{"app"}, Containers: []health.ContainerSignal{c}},
		}
		return s
	}

	one := hypothesisByCause(t, diagnoseSnapshot(makeSnap(fixedTime, true)), CauseOOMKill)
	two := hypothesisByCause(t, diagnoseSnapshot(makeSnap(fixedTime.Add(10*time.Minute), false)), CauseOOMKill)

	if one.Identity != two.Identity {
		t.Fatalf("hypothesis identity changed across cycles: %q vs %q", one.Identity, two.Identity)
	}
	if one.Confidence == two.Confidence {
		t.Fatal("expected confidence to evolve between cycles (test fixture error)")
	}
	if !two.DetectedAt.Equal(fixedTime.Add(10 * time.Minute)) {
		t.Fatalf("DetectedAt = %v, want the second snapshot's collection time", two.DetectedAt)
	}
}

// TestDiagnose_Deterministic proves diagnosing the same incidents twice, and on
// a shuffled incident order, yields byte-for-byte identical output.
func TestDiagnose_Deterministic(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "shop", Name: "api", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	s.ReplicaSets = []health.ReplicaSetSignal{
		{Namespace: "shop", Name: "api-6f7d", DesiredReplicas: 3, AvailableReplicas: 0},
	}
	owners := []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}}
	s.Pods = []health.PodSignal{
		{Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Pending", Pending: true, Owners: owners,
			Containers: []health.ContainerSignal{{Name: "api", WaitingReason: "ErrImagePull"}}},
		{Namespace: "svc", Name: "cache-1", Phase: "Running", Node: "node-a", RestartCount: 6,
			CrashLoopingContainers: []string{"app"},
			Containers: []health.ContainerSignal{
				{Name: "app", CrashLooping: true, CurrentTermination: &health.TerminationSignal{Reason: "OOMKilled", ExitCode: 137}}}},
		{Namespace: "batch", Name: "big-job", Phase: "Pending", Pending: true},
	}
	s.WarningEvents = []health.EventSignal{
		{Namespace: "batch", Name: "big-job.1", Reason: "FailedScheduling", Count: 2,
			InvolvedObject: "Pod/batch/big-job", LastSeen: fixedTime,
			Message: "0/1 nodes are available: 1 Insufficient memory."},
	}

	incidents := correlate.Correlate(s, detect.Analyze(s))
	first := Incidents(s, incidents)
	second := Incidents(s, incidents)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("diagnosis is not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if len(first) < 3 {
		t.Fatalf("expected several hypotheses across incidents, got %d", len(first))
	}

	// Shuffling the incident order must not change the (sorted) output.
	shuffled := make([]correlate.Incident, len(incidents))
	for i := range incidents {
		shuffled[i] = incidents[len(incidents)-1-i]
	}
	third := Incidents(s, shuffled)
	if !reflect.DeepEqual(first, third) {
		t.Fatalf("diagnosis depends on incident order:\nforward: %+v\nreversed: %+v", first, third)
	}

	// Output is globally ranked most-confident-first.
	for i := 1; i < len(first); i++ {
		if first[i-1].Confidence < first[i].Confidence {
			t.Fatalf("hypotheses not sorted by confidence descending at %d", i)
		}
	}
}

// TestDiagnose_SingleIncidentEntrypoint proves Diagnose (single incident) agrees
// with Incidents for that incident and that a nil Refiner is tolerated.
func TestDiagnose_SingleIncidentEntrypoint(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	s.Pods = []health.PodSignal{
		{Namespace: "app", Name: "web-1", Phase: "Running", Node: "node-a", RestartCount: 4,
			CrashLoopingContainers: []string{"web"}},
	}
	incidents := correlate.Correlate(s, detect.Analyze(s))
	if len(incidents) != 1 {
		t.Fatalf("expected one incident, got %d", len(incidents))
	}

	viaAll := Incidents(s, incidents)
	viaOne := Diagnose(s, incidents[0])
	if !reflect.DeepEqual(viaAll, viaOne) {
		t.Fatalf("Diagnose and Incidents disagree:\nall: %+v\none: %+v", viaAll, viaOne)
	}
	// A nil refiner must behave like the no-op default.
	viaNil := Diagnose(s, incidents[0], nil)
	if !reflect.DeepEqual(viaOne, viaNil) {
		t.Fatalf("nil refiner changed output:\ndefault: %+v\nnil: %+v", viaOne, viaNil)
	}
}

// TestNopRefiner proves the default [NopRefiner] is the identity of the seam:
// passing it explicitly yields the same output as passing no refiner at all.
func TestNopRefiner(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	incidents := correlate.Correlate(s, detect.Analyze(s))

	base := Diagnose(s, incidents[0])
	if got := (NopRefiner{}).Refine(s, incidents[0], base); !reflect.DeepEqual(got, base) {
		t.Fatalf("NopRefiner changed hypotheses: %+v vs %+v", got, base)
	}
	if withNop := Diagnose(s, incidents[0], NopRefiner{}); !reflect.DeepEqual(withNop, base) {
		t.Fatalf("passing NopRefiner differs from the default: %+v vs %+v", withNop, base)
	}
}

// TestDiagnose_Empty proves no incidents yields no hypotheses (and does not
// panic).
func TestDiagnose_Empty(t *testing.T) {
	if got := Incidents(baseSnapshot(), nil); got != nil {
		t.Fatalf("Incidents with no incidents = %+v, want nil", got)
	}
}

// TestConfidenceString proves the confidence tokens are the stable strings the
// package contract promises, including the out-of-range fallback.
func TestConfidenceString(t *testing.T) {
	cases := map[Confidence]string{
		ConfidenceLow:    "low",
		ConfidenceMedium: "medium",
		ConfidenceHigh:   "high",
		Confidence(42):   "confidence(42)",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Fatalf("Confidence(%d).String() = %q, want %q", int(c), got, want)
		}
	}
}

// TestDiagnose_NodeFailureWithoutPods proves the node rule still fires (High) for
// a NotReady node with no failing pods attributed to it, wording the message
// accordingly.
func TestDiagnose_NodeFailureWithoutPods(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}

	h := hypothesisByCause(t, diagnoseSnapshot(s), CauseNodeFailure)
	if h.Confidence != ConfidenceHigh {
		t.Fatalf("confidence = %v, want high", h.Confidence)
	}
	if !strings.Contains(h.Message, "no failing pods") {
		t.Fatalf("message %q should note the absence of attributed pods", h.Message)
	}
	if got := evidenceObjects(h); !reflect.DeepEqual(got, []detect.Object{{Kind: "node", Name: "node-a"}}) {
		t.Fatalf("evidence = %+v, want just the node", got)
	}
}

// TestDiagnose_InsufficientGenericResource proves the resource rule handles an
// "Insufficient" message that names neither cpu nor memory (for example an
// extended resource), falling back to the generic phrasing.
func TestDiagnose_InsufficientGenericResource(t *testing.T) {
	s := baseSnapshot()
	s.Pods = []health.PodSignal{{Namespace: "ml", Name: "trainer", Phase: "Pending", Pending: true}}
	s.WarningEvents = []health.EventSignal{
		{Namespace: "ml", Name: "trainer.1", Reason: "FailedScheduling", Count: 1,
			InvolvedObject: "Pod/ml/trainer", LastSeen: fixedTime,
			Message: "0/2 nodes are available: 2 Insufficient nvidia.com/gpu."},
	}

	h := hypothesisByCause(t, diagnoseSnapshot(s), CauseInsufficientResources)
	if !strings.Contains(h.Message, "insufficient allocatable resources") {
		t.Fatalf("message %q should use the generic insufficient phrasing", h.Message)
	}
}

// reRanker is a test Refiner that flips the base hypotheses to Refined-source and
// injects an extra hypothesis, proving the seam lets the fuzzy layer add and
// re-mark hypotheses while the core re-sorts the result.
type reRanker struct{}

func (reRanker) Refine(_ health.Snapshot, in correlate.Incident, base []Hypothesis) []Hypothesis {
	out := make([]Hypothesis, 0, len(base)+1)
	for _, h := range base {
		h.Source = SourceRefined
		out = append(out, h)
	}
	out = append(out, Hypothesis{
		Identity:   newHypothesisIdentity("llm.novel", in.Identity),
		Incident:   in.Identity,
		Cluster:    in.Cluster,
		Cause:      "llm.novel",
		Confidence: ConfidenceHigh,
		Title:      "Refined cause",
		Source:     SourceRefined,
		DetectedAt: in.DetectedAt,
	})
	return out
}

// TestDiagnose_RefinerSeam proves the Refiner seam: a refiner can rewrite and add
// hypotheses, the core re-sorts them, and the deterministic entrypoints are
// unaffected (they use the no-op refiner).
func TestDiagnose_RefinerSeam(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
	incidents := correlate.Correlate(s, detect.Analyze(s))

	base := Diagnose(s, incidents[0])
	refined := Diagnose(s, incidents[0], reRanker{})

	if len(refined) != len(base)+1 {
		t.Fatalf("refiner should have added one hypothesis: base %d, refined %d", len(base), len(refined))
	}
	for _, h := range refined {
		if h.Source != SourceRefined {
			t.Fatalf("refined hypothesis %q not marked SourceRefined", h.Identity)
		}
	}
	// The deterministic entrypoint is untouched by the refiner's existence.
	for _, h := range base {
		if h.Source != SourceDeterministic {
			t.Fatalf("deterministic hypothesis %q should be SourceDeterministic", h.Identity)
		}
	}
	// Re-sorted most-confident-first.
	for i := 1; i < len(refined); i++ {
		if refined[i-1].Confidence < refined[i].Confidence {
			t.Fatalf("refined output not re-sorted by confidence at %d", i)
		}
	}
}
