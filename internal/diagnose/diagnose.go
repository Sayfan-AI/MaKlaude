package diagnose

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// imagePullReasons is the set of container waiting reasons that unambiguously
// mean "this image cannot be pulled or is invalid". They are the kubelet's own
// reason strings; matching on them (rather than on free-text messages) keeps the
// bad-image rule deterministic and robust to wording changes.
var imagePullReasons = map[string]struct{}{
	"ImagePullBackOff":    {},
	"ErrImagePull":        {},
	"InvalidImageName":    {},
	"ImageInspectError":   {},
	"ErrImageNeverPull":   {},
	"RegistryUnavailable": {},
}

// Diagnose turns one incident into its ranked root-cause hypotheses.
//
// The deterministic core is a pure function of (snap, incident): it reads only
// those, calls no clock (every hypothesis inherits the incident's DetectedAt),
// performs no I/O, contacts no cluster, and invokes no LLM. Given the same
// inputs — and a deterministic refiner, or none — it always returns the same
// hypotheses in the same order.
//
// Every incident yields at least one hypothesis: when no specialized rule
// matches, a generic [CauseUnknown] hypothesis naming the primary finding is
// produced, so an incident is never silently dropped. The returned hypotheses
// are sorted most-confident-first by [Confidence], then by [HypothesisIdentity]
// ascending as a fully decisive tiebreak.
//
// The optional refiner is the [Refiner] seam for the fuzzy/LLM layer (T5): pass
// at most one to have it improve the deterministic hypotheses before the final
// sort. Passing none (or a nil one) runs the deterministic core alone, which is
// the byte-stable, fully-tested default.
func Diagnose(snap health.Snapshot, incident correlate.Incident, refiner ...Refiner) []Hypothesis {
	idx := newSnapshotIndex(snap)
	hyps := refine(snap, incident, deterministicHypotheses(incident, idx), refiner)
	sortHypotheses(hyps)
	return hyps
}

// Incidents diagnoses every incident and returns one flat list of hypotheses
// across all of them, sorted most-confident-first by [Confidence] then by
// [HypothesisIdentity].
//
// Because every hypothesis identity is unique (it embeds both its cause and its
// incident), the final sort is a total order, so the output is byte-stable for a
// fixed set of incidents REGARDLESS of the order they are supplied in. Each
// hypothesis carries its incident's identity, so a consumer can regroup the flat
// list by incident if it wants to.
//
// The optional refiner behaves exactly as in [Diagnose]; it is applied per
// incident.
func Incidents(snap health.Snapshot, incidents []correlate.Incident, refiner ...Refiner) []Hypothesis {
	if len(incidents) == 0 {
		return nil
	}
	idx := newSnapshotIndex(snap)
	var out []Hypothesis
	for i := range incidents {
		hyps := refine(snap, incidents[i], deterministicHypotheses(incidents[i], idx), refiner)
		out = append(out, hyps...)
	}
	sortHypotheses(out)
	return out
}

// refine applies the optional refiner (at most one; a nil or absent one is a
// no-op) to a single incident's deterministic hypotheses. Centralising the
// variadic-and-nil handling keeps [Diagnose] and [Incidents] identical in how
// they treat the seam.
func refine(snap health.Snapshot, incident correlate.Incident, base []Hypothesis, refiner []Refiner) []Hypothesis {
	if len(refiner) > 0 && refiner[0] != nil {
		return refiner[0].Refine(snap, incident, base)
	}
	return base
}

// deterministicHypotheses runs the fixed-order rule layer over one incident and
// returns the hypotheses that matched, falling back to the generic
// primary-as-cause hypothesis when none did. It never returns an empty slice for
// a real incident, so nothing is silently dropped.
func deterministicHypotheses(in correlate.Incident, idx *snapshotIndex) []Hypothesis {
	var out []Hypothesis
	for _, rule := range rules {
		if h, ok := rule(in, idx); ok {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		out = append(out, fallbackHypothesis(in))
	}
	return out
}

// rule is one deterministic diagnosis rule: it inspects an incident (against the
// snapshot index) and either proposes a hypothesis or declines. Rules are pure
// and independent; several may fire for one incident, each contributing a
// hypothesis that ranking then orders.
type rule func(in correlate.Incident, idx *snapshotIndex) (Hypothesis, bool)

// rules is the fixed, ordered set of deterministic diagnosis rules — one per
// well-understood cascade. The order here does not affect the final output
// (ranking re-sorts), but it is fixed so intermediate behaviour is reproducible.
var rules = []rule{
	badImageRule,
	insufficientResourcesRule,
	nodeFailureRule,
	oomKillRule,
}

// badImageRule fires when a container in the incident is stuck on an
// image-pull failure. That is the classic "bad image ⇒ workload unavailable"
// cascade: the container cannot start, so its ReplicaSet never becomes available
// and its Deployment goes unavailable. It is a strong, current signal, so the
// confidence is High.
func badImageRule(in correlate.Incident, idx *snapshotIndex) (Hypothesis, bool) {
	var where string
	for _, f := range in.Findings() {
		if f.Object.Kind != "pod" {
			continue
		}
		pod, ok := idx.podByKey[podKey(f.Object.Namespace, f.Object.Name)]
		if !ok {
			continue
		}
		for i := range pod.Containers {
			c := pod.Containers[i]
			if _, bad := imagePullReasons[c.WaitingReason]; bad {
				where = fmt.Sprintf("pod %s/%s container %q: %s", pod.Namespace, pod.Name, c.Name, c.WaitingReason)
				break
			}
		}
		if where != "" {
			break
		}
	}
	if where == "" {
		return Hypothesis{}, false
	}
	return newHypothesis(in, CauseBadImage, ConfidenceHigh,
		"Unpullable or invalid container image",
		fmt.Sprintf("A container image cannot be pulled or is invalid (%s), so the container never starts and the workload is left unavailable.", where),
		findingsOfKinds(in, "pod", "replicaset", "deployment", "statefulset")), true
}

// insufficientResourcesRule fires when a Pending pod in the incident has a
// FailedScheduling event reporting that the cluster lacks the cpu/memory it
// requests. That is the "insufficient resources ⇒ FailedScheduling/Pending"
// cascade. The scheduling event is an explicit, current signal, so confidence is
// High.
func insufficientResourcesRule(in correlate.Incident, idx *snapshotIndex) (Hypothesis, bool) {
	var detail, where string
	for _, f := range in.Findings() {
		if f.Object.Kind != "pod" {
			continue
		}
		pod, ok := idx.podByKey[podKey(f.Object.Namespace, f.Object.Name)]
		if !ok || !pod.Pending {
			continue
		}
		for _, e := range idx.eventsByObject[eventKey("pod", pod.Namespace, pod.Name)] {
			if e.Reason == "FailedScheduling" && strings.Contains(e.Message, "Insufficient") {
				detail = insufficientDetail(e.Message)
				where = fmt.Sprintf("pod %s/%s", pod.Namespace, pod.Name)
				break
			}
		}
		if where != "" {
			break
		}
	}
	if where == "" {
		return Hypothesis{}, false
	}
	return newHypothesis(in, CauseInsufficientResources, ConfidenceHigh,
		"Insufficient cluster resources to schedule pod",
		fmt.Sprintf("The scheduler cannot place %s because the cluster has %s; it stays Pending until capacity frees up or is added.", where, detail),
		findingsOfKinds(in, "pod", "replicaset", "deployment", "statefulset")), true
}

// nodeFailureRule fires when the incident contains a NotReady node. A NotReady
// node cannot run new work and may be losing the pods it already runs, so it is
// the cause and any pods in the incident are its victims. The node's readiness
// is a strong, current signal, so confidence is High.
func nodeFailureRule(in correlate.Incident, idx *snapshotIndex) (Hypothesis, bool) {
	var downNode string
	for _, f := range in.Findings() {
		if f.Object.Kind != "node" {
			continue
		}
		if n, ok := idx.nodeByName[f.Object.Name]; ok && !n.Ready {
			downNode = f.Object.Name
			break
		}
	}
	if downNode == "" {
		return Hypothesis{}, false
	}
	pods := findingsOfKinds(in, "pod")
	msg := fmt.Sprintf("Node %q is NotReady", downNode)
	if len(pods) > 0 {
		msg += fmt.Sprintf(" and is disrupting %d pod(s) scheduled on it.", len(pods))
	} else {
		msg += "; no failing pods are currently attributed to it."
	}
	return newHypothesis(in, CauseNodeFailure, ConfidenceHigh,
		"Node failure disrupting its pods", msg,
		findingsOfKinds(in, "node", "pod")), true
}

// oomKillRule fires when a container in the incident was terminated by the
// out-of-memory killer. A container OOM-killed by the kernel is restarted (and
// often crashloops) because it exceeds its memory limit. Confidence is High when
// a container is presently OOM-terminated, and Medium when the OOM kill is only
// visible on the previous instance (already restarted, recurring but not
// current) — a coarse, deterministic distinction, never computed arithmetic.
func oomKillRule(in correlate.Incident, idx *snapshotIndex) (Hypothesis, bool) {
	const oom = "OOMKilled"
	var whereCurrent, whereLast string
	for _, f := range in.Findings() {
		if f.Object.Kind != "pod" {
			continue
		}
		pod, ok := idx.podByKey[podKey(f.Object.Namespace, f.Object.Name)]
		if !ok {
			continue
		}
		for i := range pod.Containers {
			c := pod.Containers[i]
			if c.CurrentTermination != nil && c.CurrentTermination.Reason == oom && whereCurrent == "" {
				whereCurrent = fmt.Sprintf("pod %s/%s container %q", pod.Namespace, pod.Name, c.Name)
			}
			if c.LastTermination != nil && c.LastTermination.Reason == oom && whereLast == "" {
				whereLast = fmt.Sprintf("pod %s/%s container %q", pod.Namespace, pod.Name, c.Name)
			}
		}
	}
	if whereCurrent == "" && whereLast == "" {
		return Hypothesis{}, false
	}
	conf := ConfidenceMedium
	where := whereLast
	tense := "was OOM-killed on a previous restart"
	if whereCurrent != "" {
		conf = ConfidenceHigh
		where = whereCurrent
		tense = "is being OOM-killed"
	}
	return newHypothesis(in, CauseOOMKill, conf,
		"Container OOM-killed (exceeds memory limit)",
		fmt.Sprintf("A container %s (%s), so it is repeatedly restarted and may be crashlooping; its memory limit is likely too low for its workload.", tense, where),
		findingsOfKinds(in, "pod")), true
}

// insufficientDetail turns a scheduler's FailedScheduling message into a stable,
// human phrase naming which resources are short, by matching the kubelet's
// canonical "Insufficient <resource>" tokens. It reports cpu and/or memory in a
// fixed order (never depending on their order in the message), and falls back to
// a generic phrase when the message mentions "Insufficient" but neither cpu nor
// memory — so the result is deterministic for a fixed message.
func insufficientDetail(msg string) string {
	var parts []string
	if strings.Contains(msg, "Insufficient cpu") {
		parts = append(parts, "cpu")
	}
	if strings.Contains(msg, "Insufficient memory") {
		parts = append(parts, "memory")
	}
	if len(parts) == 0 {
		return "insufficient allocatable resources"
	}
	return "insufficient allocatable " + strings.Join(parts, " and ")
}

// fallbackHypothesis is the guarantee that no incident is ever silently dropped:
// when no specialized rule matched, the incident's primary finding is offered as
// the suspected cause, at the lowest confidence. It cites the primary as its
// evidence.
func fallbackHypothesis(in correlate.Incident) Hypothesis {
	p := in.Primary
	return newHypothesis(in, CauseUnknown, ConfidenceLow,
		"Suspected cause: "+p.Title,
		fmt.Sprintf("No specialized diagnosis rule matched this incident; the primary finding %q on %s is the suspected cause.",
			p.Title, p.Object.String()),
		[]detect.Finding{p})
}

// newHypothesis assembles a hypothesis from a matched rule, filling in the
// stable identity (from cause + incident), the incident's cluster and detection
// time, and the deterministic source marker. Centralising construction keeps the
// identity/inheritance rules in one place so every rule obeys them.
func newHypothesis(in correlate.Incident, cause Cause, conf Confidence, title, message string, evidence []detect.Finding) Hypothesis {
	return Hypothesis{
		Identity:   newHypothesisIdentity(cause, in.Identity),
		Incident:   in.Identity,
		Cluster:    in.Cluster,
		Cause:      cause,
		Confidence: conf,
		Title:      title,
		Message:    message,
		Evidence:   evidence,
		Source:     SourceDeterministic,
		DetectedAt: in.DetectedAt,
	}
}

// findingsOfKinds returns, as a fresh slice, the incident's findings whose
// object is one of the given kinds, preserving the incident's own stable finding
// order (primary first, then effects sorted by identity). It is how each rule
// cites exactly the evidence relevant to its cause.
func findingsOfKinds(in correlate.Incident, kinds ...string) []detect.Finding {
	want := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	var out []detect.Finding
	for _, f := range in.Findings() {
		if _, ok := want[f.Object.Kind]; ok {
			out = append(out, f)
		}
	}
	return out
}

// sortHypotheses orders hypotheses most-confident-first: by confidence
// descending, then by identity ascending as a fully decisive tiebreak. Because
// identity is itself fully deterministic and unique per (cause, incident), the
// resulting order is reproducible for any given input and independent of the
// order the hypotheses were produced in.
func sortHypotheses(hyps []Hypothesis) {
	sort.Slice(hyps, func(i, j int) bool {
		if hyps[i].Confidence != hyps[j].Confidence {
			return hyps[i].Confidence > hyps[j].Confidence
		}
		return hyps[i].Identity < hyps[j].Identity
	})
}

// snapshotIndex holds the lookups the rules need to resolve an incident's
// findings back to the structural facts the collector captured — a pod's
// containers, a node's readiness, a pod's scheduling events — without rescanning
// the snapshot per rule. It is built once per Diagnose call and read-only
// thereafter.
type snapshotIndex struct {
	podByKey       map[string]health.PodSignal
	nodeByName     map[string]health.NodeSignal
	eventsByObject map[string][]health.EventSignal
}

// newSnapshotIndex builds the read-only index over a snapshot. Because the
// snapshot's slices are already deterministically sorted, and this only groups
// them into maps for lookup (never iterating a map to produce output), it
// introduces no nondeterminism.
func newSnapshotIndex(snap health.Snapshot) *snapshotIndex {
	idx := &snapshotIndex{
		podByKey:       make(map[string]health.PodSignal, len(snap.Pods)),
		nodeByName:     make(map[string]health.NodeSignal, len(snap.Nodes)),
		eventsByObject: make(map[string][]health.EventSignal, len(snap.WarningEvents)),
	}
	for i := range snap.Pods {
		idx.podByKey[podKey(snap.Pods[i].Namespace, snap.Pods[i].Name)] = snap.Pods[i]
	}
	for i := range snap.Nodes {
		idx.nodeByName[snap.Nodes[i].Name] = snap.Nodes[i]
	}
	for i := range snap.WarningEvents {
		e := snap.WarningEvents[i]
		key := strings.ToLower(e.InvolvedObject)
		idx.eventsByObject[key] = append(idx.eventsByObject[key], e)
	}
	return idx
}

// podKey is the namespace/name key used to index pods, matching the form the
// correlate package uses.
func podKey(namespace, name string) string {
	return namespace + "/" + name
}

// eventKey builds the lowercase "kind/namespace/name" key used to look up a
// pod's events, matching the lowercased form newSnapshotIndex stores events
// under (the health package emits involved-object strings with a capitalised
// kind such as "Pod/...").
func eventKey(kind, namespace, name string) string {
	return strings.ToLower(kind + "/" + namespace + "/" + name)
}
