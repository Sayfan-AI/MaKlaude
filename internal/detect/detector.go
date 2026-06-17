package detect

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// highRestartThreshold is the per-pod cumulative restart count at or above
// which a pod earns a standalone "high restart count" warning even when it is
// not currently crashlooping. The value is deliberately conservative: a handful
// of restarts is routine during rollouts and node drains, but a pod that has
// restarted this many times has a recurring problem worth surfacing before it
// tips into a crashloop. It is a const, not configurable, to keep [Analyze] a
// pure function with no construction surface; a future milestone can make it
// tunable if real clusters demand it.
const highRestartThreshold int32 = 5

// Analyze applies MaKlaude's deterministic detection rules to a single health
// snapshot and returns the resulting findings, sorted most-urgent-first.
//
// It is a pure function: it reads only the snapshot, calls no clock (every
// finding inherits snap.CollectedAt), performs no I/O, and never mutates a
// cluster. Given the same snapshot it always returns the same findings in the
// same order, so its output is reproducible and directly comparable across
// cycles.
//
// The rules are intentionally conservative and judgment-light — this is the M1
// deterministic layer, not a diagnosis engine. Each rule is documented inline
// with the reasoning behind its severity. When the API server is unreachable
// the only finding produced is the unreachability itself: the snapshot's signal
// slices are empty in that case (there was nothing to read), so there is
// nothing else to interpret and emitting "0 nodes" style noise would be
// misleading.
func Analyze(snap health.Snapshot) []Finding {
	var findings []Finding
	add := func(f Finding) { findings = append(findings, f) }

	analyzeReachability(snap, add)

	// Only interpret the signal slices when the cluster actually answered. An
	// unreachable cluster has empty slices, and treating "we saw nothing"
	// as "everything is fine" (or as a flood of absences) would both be wrong.
	if snap.Reachability.Reachable {
		analyzeNodes(snap, add)
		analyzePods(snap, add)
		analyzeDeployments(snap, add)
		analyzeReplicaSets(snap, add)
		analyzeWarningEvents(snap, findings, add)
	}

	sortFindings(findings)
	return findings
}

// analyzeReachability fires when the control plane did not answer the
// reachability probe. An unreachable API server is the most severe thing
// MaKlaude can observe — nothing else about the cluster can be trusted or even
// read — so it is unconditionally critical.
func analyzeReachability(snap health.Snapshot, add func(Finding)) {
	if snap.Reachability.Reachable {
		return
	}
	obj := Object{Kind: "cluster", Name: snap.Cluster}
	add(Finding{
		Identity:   newIdentity(snap.Cluster, "cluster.unreachable", obj),
		Severity:   SeverityCritical,
		Cluster:    snap.Cluster,
		Object:     obj,
		Title:      "Cluster API server unreachable",
		Message:    fmt.Sprintf("API server for cluster %q did not respond: %s", snap.Cluster, snap.Reachability.Error),
		DetectedAt: snap.CollectedAt,
	})
}

// analyzeNodes emits findings for unhealthy nodes.
//
//   - NotReady is critical: a node that is not Ready cannot run new work and may
//     be losing the pods it already runs, so it demands immediate attention.
//   - Memory / disk / PID pressure is a warning: the node is still serving but
//     the kubelet has signalled resource strain that will lead to evictions if
//     left unaddressed — degraded, not down.
//   - Cordoned (Unschedulable) is a warning: it is often a deliberate operator
//     action (maintenance), so it is not critical, but an unintentionally or
//     forgotten-cordoned node silently erodes capacity, so it is worth
//     surfacing rather than ignoring.
//
// A single node can legitimately produce several of these at once (for example
// NotReady and under memory pressure); each is a distinct problem with its own
// identity.
func analyzeNodes(snap health.Snapshot, add func(Finding)) {
	for i := range snap.Nodes {
		n := snap.Nodes[i]
		obj := Object{Kind: "node", Name: n.Name}

		if !n.Ready {
			add(Finding{
				Identity:   newIdentity(snap.Cluster, "node.notready", obj),
				Severity:   SeverityCritical,
				Cluster:    snap.Cluster,
				Object:     obj,
				Title:      "Node NotReady",
				Message:    fmt.Sprintf("Node %q is not Ready", n.Name),
				DetectedAt: snap.CollectedAt,
			})
		}
		if n.MemoryPressure {
			add(pressureFinding(snap, obj, "memorypressure", "memory", n.Name))
		}
		if n.DiskPressure {
			add(pressureFinding(snap, obj, "diskpressure", "disk", n.Name))
		}
		if n.PIDPressure {
			add(pressureFinding(snap, obj, "pidpressure", "PID", n.Name))
		}
		if n.Unschedulable {
			add(Finding{
				Identity:   newIdentity(snap.Cluster, "node.cordoned", obj),
				Severity:   SeverityWarning,
				Cluster:    snap.Cluster,
				Object:     obj,
				Title:      "Node cordoned",
				Message:    fmt.Sprintf("Node %q is cordoned (unschedulable)", n.Name),
				DetectedAt: snap.CollectedAt,
			})
		}
	}
}

// pressureFinding builds the warning for one kind of node resource pressure,
// keeping the three pressure rules (memory/disk/PID) consistent.
func pressureFinding(snap health.Snapshot, obj Object, rule, human, node string) Finding {
	return Finding{
		Identity:   newIdentity(snap.Cluster, "node."+rule, obj),
		Severity:   SeverityWarning,
		Cluster:    snap.Cluster,
		Object:     obj,
		Title:      "Node under " + human + " pressure",
		Message:    fmt.Sprintf("Node %q is reporting %s pressure", node, human),
		DetectedAt: snap.CollectedAt,
	}
}

// analyzePods emits findings for unhealthy pods.
//
//   - Crashlooping is critical: a container stuck in CrashLoopBackOff is failing
//     to start repeatedly — the workload is effectively down for that container —
//     so it warrants immediate attention rather than a soft warning.
//   - Failed phase is a warning: a pod in the Failed phase has terminated
//     unsuccessfully, which is notable, but a controller usually replaces it; it
//     is a degraded signal rather than an outage on its own. (If the failure is
//     widespread it will also surface as a degraded workload, which is handled
//     separately.)
//   - Pending is a warning: a pod that cannot be scheduled is not yet running
//     and may indicate capacity or scheduling-constraint problems. We treat the
//     mere fact of Pending as a warning here; the collector does not expose how
//     long it has been pending, so we cannot gate on a "stuck" duration without
//     a clock — and this layer is deliberately clock-free. A future, snapshot-
//     diffing milestone can refine this to "stuck pending".
//   - High restart count (without an active crashloop) is a warning: a pod that
//     has restarted many times has a recurring fault worth surfacing early. To
//     avoid double-reporting, this rule is suppressed when the pod is already
//     flagged as crashlooping — the crashloop finding is the more specific
//     signal.
func analyzePods(snap health.Snapshot, add func(Finding)) {
	for i := range snap.Pods {
		p := snap.Pods[i]
		obj := Object{Kind: "pod", Namespace: p.Namespace, Name: p.Name}

		crashlooping := len(p.CrashLoopingContainers) > 0
		if crashlooping {
			add(Finding{
				Identity: newIdentity(snap.Cluster, "pod.crashloop", obj),
				Severity: SeverityCritical,
				Cluster:  snap.Cluster,
				Object:   obj,
				Title:    "Pod crashlooping",
				Message: fmt.Sprintf("Pod %s/%s has crashlooping container(s): %s",
					p.Namespace, p.Name, strings.Join(p.CrashLoopingContainers, ", ")),
				DetectedAt: snap.CollectedAt,
			})
		}
		if p.Failed {
			msg := fmt.Sprintf("Pod %s/%s is in the Failed phase", p.Namespace, p.Name)
			if p.Reason != "" {
				msg += fmt.Sprintf(" (reason: %s)", p.Reason)
			}
			add(Finding{
				Identity:   newIdentity(snap.Cluster, "pod.failed", obj),
				Severity:   SeverityWarning,
				Cluster:    snap.Cluster,
				Object:     obj,
				Title:      "Pod Failed",
				Message:    msg,
				DetectedAt: snap.CollectedAt,
			})
		}
		if p.Pending {
			add(Finding{
				Identity:   newIdentity(snap.Cluster, "pod.pending", obj),
				Severity:   SeverityWarning,
				Cluster:    snap.Cluster,
				Object:     obj,
				Title:      "Pod Pending",
				Message:    fmt.Sprintf("Pod %s/%s is Pending and not yet running", p.Namespace, p.Name),
				DetectedAt: snap.CollectedAt,
			})
		}
		// High restart count is only its own finding when the pod is not already
		// crashlooping, so an actively-crashlooping pod is reported once (as the
		// more specific critical), not twice.
		if !crashlooping && p.RestartCount >= highRestartThreshold {
			add(Finding{
				Identity: newIdentity(snap.Cluster, "pod.highrestarts", obj),
				Severity: SeverityWarning,
				Cluster:  snap.Cluster,
				Object:   obj,
				Title:    "Pod high restart count",
				Message: fmt.Sprintf("Pod %s/%s has restarted %d times (threshold %d)",
					p.Namespace, p.Name, p.RestartCount, highRestartThreshold),
				DetectedAt: snap.CollectedAt,
			})
		}
	}
}

// analyzeDeployments emits a finding for any deployment running below its
// desired replica count.
//
//   - Zero available while some are desired is critical: the deployment is
//     effectively down — no replica is serving — so it warrants immediate
//     attention.
//   - Otherwise (some but not all available) is a warning: the deployment is
//     degraded but still serving from its remaining replicas.
//
// A desired count of zero is intentionally never a finding: scaling a workload
// to zero is a normal, deliberate state, not a problem.
func analyzeDeployments(snap health.Snapshot, add func(Finding)) {
	for i := range snap.Deployments {
		d := snap.Deployments[i]
		if d.DesiredReplicas <= 0 || d.AvailableReplicas >= d.DesiredReplicas {
			continue
		}
		obj := Object{Kind: "deployment", Namespace: d.Namespace, Name: d.Name}
		sev := SeverityWarning
		title := "Deployment degraded"
		if d.AvailableReplicas == 0 {
			sev = SeverityCritical
			title = "Deployment unavailable"
		}
		add(Finding{
			Identity: newIdentity(snap.Cluster, "deployment.underreplicated", obj),
			Severity: sev,
			Cluster:  snap.Cluster,
			Object:   obj,
			Title:    title,
			Message: fmt.Sprintf("Deployment %s/%s has %d/%d replicas available",
				d.Namespace, d.Name, d.AvailableReplicas, d.DesiredReplicas),
			DetectedAt: snap.CollectedAt,
		})
	}
}

// analyzeReplicaSets emits a finding for any replica set running below its
// desired replica count, with the same severity logic as deployments. Bare
// replica sets are reported in their own right because not every replica set is
// owned by a deployment (some are created directly), and the snapshot does not
// carry ownership; the stable per-object identity keeps a deployment-owned
// replica set's finding distinct from its deployment's anyway.
func analyzeReplicaSets(snap health.Snapshot, add func(Finding)) {
	for i := range snap.ReplicaSets {
		r := snap.ReplicaSets[i]
		if r.DesiredReplicas <= 0 || r.AvailableReplicas >= r.DesiredReplicas {
			continue
		}
		obj := Object{Kind: "replicaset", Namespace: r.Namespace, Name: r.Name}
		sev := SeverityWarning
		title := "ReplicaSet degraded"
		if r.AvailableReplicas == 0 {
			sev = SeverityCritical
			title = "ReplicaSet unavailable"
		}
		add(Finding{
			Identity: newIdentity(snap.Cluster, "replicaset.underreplicated", obj),
			Severity: sev,
			Cluster:  snap.Cluster,
			Object:   obj,
			Title:    title,
			Message: fmt.Sprintf("ReplicaSet %s/%s has %d/%d replicas available",
				r.Namespace, r.Name, r.AvailableReplicas, r.DesiredReplicas),
			DetectedAt: snap.CollectedAt,
		})
	}
}

// analyzeWarningEvents surfaces recent warning events as low-priority findings,
// but only when the involved object is not ALREADY covered by a more specific
// workload/node finding. The snapshot's warning events are noisy and often just
// the cluster narrating a problem this package already detected structurally
// (a FailedScheduling event for a pod we already flagged as Pending, say).
// Re-emitting those would double-report the same problem, so we dedup by the
// involved object: if any earlier finding concerns the same object, the event
// is dropped.
//
// Surviving events are emitted at info severity — they are context for the
// audit trail, not independently actionable — with an identity keyed on the
// event's own object so the same recurring event coalesces to one finding.
func analyzeWarningEvents(snap health.Snapshot, existing []Finding, add func(Finding)) {
	// Index the objects already covered by structural findings so event dedup is
	// a cheap lookup rather than a scan per event.
	covered := make(map[string]struct{}, len(existing))
	for i := range existing {
		covered[existing[i].Object.String()] = struct{}{}
	}

	for i := range snap.WarningEvents {
		e := snap.WarningEvents[i]
		obj := eventObject(e)
		if _, ok := covered[obj.String()]; ok {
			// A more specific finding already concerns this object; skip the
			// event to avoid double-reporting the same underlying problem.
			continue
		}
		add(Finding{
			Identity: newIdentity(snap.Cluster, "event.warning", obj),
			Severity: SeverityInfo,
			Cluster:  snap.Cluster,
			Object:   obj,
			Title:    "Warning event: " + e.Reason,
			Message: fmt.Sprintf("Warning event %q on %s (x%d): %s",
				e.Reason, e.InvolvedObject, e.Count, e.Message),
			DetectedAt: snap.CollectedAt,
		})
	}
}

// eventObject parses the health package's "Kind/namespace/name" (or
// "Kind/name") involved-object string back into a typed [Object], lowercasing
// the kind so it matches the kinds the structural rules use for dedup. If the
// reference is malformed it falls back to the event's own namespace/name so the
// finding still has a sensible, stable identity.
func eventObject(e health.EventSignal) Object {
	parts := strings.Split(e.InvolvedObject, "/")
	switch len(parts) {
	case 3:
		return Object{Kind: strings.ToLower(parts[0]), Namespace: parts[1], Name: parts[2]}
	case 2:
		return Object{Kind: strings.ToLower(parts[0]), Name: parts[1]}
	default:
		return Object{Kind: "event", Namespace: e.Namespace, Name: e.Name}
	}
}

// sortFindings orders findings most-urgent-first: by severity descending, then
// by identity ascending as a stable tiebreak. Because identity is itself fully
// deterministic, the resulting order is reproducible for any given snapshot.
func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		return findings[i].Identity < findings[j].Identity
	})
}
