package correlate

import (
	"sort"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// causePrecedence ranks how "root-like" a finding is by the kind of object it
// concerns, from most-likely-cause (lowest) to most-likely-effect (highest). It
// is the backbone of deterministic primary selection: when a group of correlated
// findings must nominate one as its suspected cause, the finding about the most
// structural object wins.
//
// The ordering encodes a simple, defensible causal story. An unreachable cluster
// dwarfs everything; a NotReady or pressured node is infrastructure beneath the
// workloads it hosts; a Deployment (or StatefulSet) is the intent above the
// ReplicaSet it manages, which is above the pods it schedules; a bare event is
// mere narration. So a node strands its pods (node is cause), and a bad
// Deployment radiates into its ReplicaSet and pods (deployment is cause) — in
// both cases the structural object outranks its symptoms.
//
// Precedence is intentionally derived from object kind alone, not from severity
// or message, so it is stable as an incident evolves. Ties within a kind are
// broken elsewhere (by severity, then identity) — see [Correlate].
func causePrecedence(f detect.Finding) int {
	switch f.Object.Kind {
	case "cluster":
		return 0
	case "node":
		return 1
	case "deployment", "statefulset":
		return 2
	case "replicaset":
		return 3
	case "pod":
		return 4
	case "event":
		return 5
	default:
		return 6
	}
}

// Correlate groups a snapshot's findings into incidents, deterministically and
// using structural signals only.
//
// It is a pure function: it reads only the snapshot and the findings it is given
// (which must be the [detect.Analyze] output for that same snapshot), calls no
// clock (every incident inherits snap.CollectedAt via its findings), performs no
// I/O, contacts no cluster, and uses no LLM. Given the same inputs it always
// returns the same incidents in the same order, so its output is reproducible and
// directly comparable across cycles.
//
// The algorithm is a union-find over the findings:
//
//  1. Findings about the same object are unioned first, so a node's several
//     findings (NotReady and under pressure), or a pod's several findings
//     (crashloop and high restarts), never fragment across incidents.
//  2. Structural edges then union findings across objects: a pod's finding is
//     unioned with its node's finding (node placement) and with its
//     ReplicaSet's and Deployment's findings (owner references, the ReplicaSet
//     matched to its Deployment by the "<deployment>-<hash>" naming convention),
//     and a ReplicaSet's finding is unioned with its Deployment's. Edges are
//     scoped to a single namespace; namespace and co-occurrence are guards, never
//     standalone merges.
//  3. Each resulting connected component becomes one incident. Its primary is the
//     most-likely-cause finding — lowest [causePrecedence], then highest severity,
//     then smallest identity as a fully deterministic final tiebreak. The rest
//     become effects, sorted by identity.
//
// A finding that no edge touches forms a singleton incident (itself as primary,
// no effects). Every input finding lands in exactly one incident — Correlate
// partitions the findings and never drops one. The returned incidents are sorted
// most-urgent-first by primary severity, then by incident identity.
func Correlate(snap health.Snapshot, findings []detect.Finding) []Incident {
	if len(findings) == 0 {
		return nil
	}

	// Index every finding's object so structural edges can resolve an object
	// reference (a pod's node, a ReplicaSet's deployment) to the finding about it,
	// if any. The first finding wins for a given object; because all findings
	// about one object are unioned regardless, which one we record does not affect
	// grouping.
	findingByObject := make(map[string]int, len(findings))
	for i := range findings {
		key := findings[i].Object.String()
		if _, seen := findingByObject[key]; !seen {
			findingByObject[key] = i
		}
	}

	// Collect the deployment findings per namespace so a ReplicaSet can be matched
	// to its owning Deployment by name prefix ("<deployment>-<hash>").
	deployNamesByNS := make(map[string][]string)
	for i := range findings {
		if findings[i].Object.Kind == "deployment" {
			ns := findings[i].Object.Namespace
			deployNamesByNS[ns] = append(deployNamesByNS[ns], findings[i].Object.Name)
		}
	}

	// Index the snapshot's pods so a pod finding (or a warning event about a pod)
	// can be resolved to the structural facts — node and owners — the health
	// collector captured for it.
	podByKey := make(map[string]health.PodSignal, len(snap.Pods))
	for i := range snap.Pods {
		podByKey[snap.Pods[i].Namespace+"/"+snap.Pods[i].Name] = snap.Pods[i]
	}

	uf := newUnionFind(len(findings))

	// Step 1: union findings that concern the very same object.
	firstByObject := make(map[string]int, len(findings))
	for i := range findings {
		key := findings[i].Object.String()
		if first, seen := firstByObject[key]; seen {
			uf.union(first, i)
		} else {
			firstByObject[key] = i
		}
	}

	// Step 2: union across structural edges. For each finding, compute the objects
	// that are its likely causes and, when a finding exists for such an object,
	// union the two.
	for i := range findings {
		for _, target := range causeObjects(findings[i], podByKey, deployNamesByNS) {
			if j, ok := findingByObject[target.String()]; ok {
				uf.union(i, j)
			}
		}
	}

	// Step 3: gather connected components, then build one incident per component.
	components := make(map[int][]int)
	for i := range findings {
		root := uf.find(i)
		components[root] = append(components[root], i)
	}

	incidents := make([]Incident, 0, len(components))
	for _, members := range components {
		incidents = append(incidents, buildIncident(findings, members))
	}

	sortIncidents(incidents)
	return incidents
}

// causeObjects returns the objects that finding f structurally points to as its
// likely causes. Resolving those objects to findings (and unioning) is the
// caller's job; this function only knows the structural relationships.
//
//   - A pod finding points to the node it is scheduled on and to its owning
//     ReplicaSet / Deployment / StatefulSet / Job.
//   - A ReplicaSet finding points to its owning Deployment (matched by name
//     prefix within the namespace).
//
// Everything else points nowhere (nodes, deployments, the cluster, and standalone
// warning events are causes or context, not effects to be reparented). The
// returned objects may not correspond to any finding; the caller filters those
// out.
func causeObjects(f detect.Finding, podByKey map[string]health.PodSignal, deployNamesByNS map[string][]string) []detect.Object {
	switch f.Object.Kind {
	case "pod":
		pod, ok := podByKey[f.Object.Namespace+"/"+f.Object.Name]
		if !ok {
			return nil
		}
		return podCauseObjects(pod, deployNamesByNS)
	case "replicaset":
		if dep, ok := matchDeployment(deployNamesByNS[f.Object.Namespace], f.Object.Name); ok {
			return []detect.Object{{Kind: "deployment", Namespace: f.Object.Namespace, Name: dep}}
		}
		return nil
	default:
		return nil
	}
}

// podCauseObjects returns the structural causes for a single pod: the node it is
// bound to (if any) and, for each owner reference, the owning workload — a
// ReplicaSet resolves both to itself and, by naming convention, to its
// Deployment; a Deployment / StatefulSet / Job resolves directly.
func podCauseObjects(pod health.PodSignal, deployNamesByNS map[string][]string) []detect.Object {
	var out []detect.Object

	if pod.Node != "" {
		out = append(out, detect.Object{Kind: "node", Name: pod.Node})
	}

	for i := range pod.Owners {
		o := pod.Owners[i]
		switch o.Kind {
		case "ReplicaSet":
			out = append(out, detect.Object{Kind: "replicaset", Namespace: pod.Namespace, Name: o.Name})
			if dep, ok := matchDeployment(deployNamesByNS[pod.Namespace], o.Name); ok {
				out = append(out, detect.Object{Kind: "deployment", Namespace: pod.Namespace, Name: dep})
			}
		case "Deployment", "StatefulSet", "Job":
			out = append(out, detect.Object{Kind: strings.ToLower(o.Kind), Namespace: pod.Namespace, Name: o.Name})
		}
	}

	return out
}

// matchDeployment finds the Deployment that owns a ReplicaSet by Kubernetes'
// naming convention: a Deployment-managed ReplicaSet is named
// "<deployment>-<pod-template-hash>". It returns the longest deployment name that
// is a prefix of rsName followed by a "-", so that with both "web" and "web-api"
// present, "web-api-5f9" matches "web-api" rather than "web". The longest-match
// rule and the deterministic input order make the result stable.
func matchDeployment(deployNames []string, rsName string) (string, bool) {
	best := ""
	for _, d := range deployNames {
		if len(d) > len(best) && strings.HasPrefix(rsName, d+"-") {
			best = d
		}
	}
	return best, best != ""
}

// buildIncident assembles one incident from the finding indices in a connected
// component. It selects the primary deterministically (most-likely-cause first),
// collects the remaining findings as effects sorted by identity, and derives the
// stable incident identity from the primary.
func buildIncident(findings []detect.Finding, members []int) Incident {
	primary := members[0]
	for _, idx := range members[1:] {
		if morePrimary(findings[idx], findings[primary]) {
			primary = idx
		}
	}

	effects := make([]detect.Finding, 0, len(members)-1)
	for _, idx := range members {
		if idx != primary {
			effects = append(effects, findings[idx])
		}
	}
	sort.Slice(effects, func(i, j int) bool {
		return effects[i].Identity < effects[j].Identity
	})

	p := findings[primary]
	return Incident{
		Identity:   newIncidentIdentity(p.Identity),
		Cluster:    p.Cluster,
		Primary:    p,
		Effects:    effects,
		DetectedAt: p.DetectedAt,
	}
}

// morePrimary reports whether finding a is a better primary (more likely the root
// cause) than finding b. The comparison is total and deterministic: most
// structural first (lowest [causePrecedence]), then most severe, then smallest
// identity as a final, always-decisive tiebreak. The identity tiebreak is what
// keeps primary selection — and therefore incident identity — stable across
// cycles even when two candidates are otherwise indistinguishable.
func morePrimary(a, b detect.Finding) bool {
	if pa, pb := causePrecedence(a), causePrecedence(b); pa != pb {
		return pa < pb
	}
	if a.Severity != b.Severity {
		return a.Severity > b.Severity
	}
	return a.Identity < b.Identity
}

// sortIncidents orders incidents most-urgent-first: by primary severity
// descending, then by incident identity ascending as a stable tiebreak. Because
// incident identity is itself fully deterministic, the resulting order is
// reproducible for any given input.
func sortIncidents(incidents []Incident) {
	sort.Slice(incidents, func(i, j int) bool {
		if incidents[i].Severity() != incidents[j].Severity() {
			return incidents[i].Severity() > incidents[j].Severity()
		}
		return incidents[i].Identity < incidents[j].Identity
	})
}

// unionFind is a minimal disjoint-set structure with path compression and
// union-by-size, used to group correlated findings into connected components. It
// operates purely on finding indices, so it never mutates the findings.
type unionFind struct {
	parent []int
	size   []int
}

// newUnionFind creates a union-find over n elements, each initially its own
// singleton set.
func newUnionFind(n int) *unionFind {
	uf := &unionFind{parent: make([]int, n), size: make([]int, n)}
	for i := range uf.parent {
		uf.parent[i] = i
		uf.size[i] = 1
	}
	return uf
}

// find returns the representative of i's set, compressing the path as it goes.
func (uf *unionFind) find(i int) int {
	for uf.parent[i] != i {
		uf.parent[i] = uf.parent[uf.parent[i]]
		i = uf.parent[i]
	}
	return i
}

// union merges the sets containing i and j. To keep the structure independent of
// the order in which unions arrive, the smaller tree is attached under the
// larger, and ties are broken toward the smaller root index — both deterministic.
func (uf *unionFind) union(i, j int) {
	ri, rj := uf.find(i), uf.find(j)
	if ri == rj {
		return
	}
	if uf.size[ri] < uf.size[rj] || (uf.size[ri] == uf.size[rj] && ri > rj) {
		ri, rj = rj, ri
	}
	uf.parent[rj] = ri
	uf.size[ri] += uf.size[rj]
}
