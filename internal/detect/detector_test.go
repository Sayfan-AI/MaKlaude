package detect

import (
	"reflect"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// fixedTime is the pinned collection time threaded through every test snapshot
// so DetectedAt is deterministic and assertable.
var fixedTime = time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)

// baseSnapshot returns a reachable, empty snapshot for one cluster. Tests layer
// the specific failing signal onto it so each case exercises one rule in
// isolation.
func baseSnapshot() health.Snapshot {
	return health.Snapshot{
		Cluster:      "prod",
		CollectedAt:  fixedTime,
		Reachability: health.Reachability{Reachable: true, ServerVersion: "v1.30.0"},
	}
}

// findingSummary is the subset of a Finding the table-driven tests assert on:
// identity, severity, and object. Title/Message are human-facing prose, exercised
// separately, and DetectedAt is asserted once globally.
type findingSummary struct {
	Identity Identity
	Severity Severity
	Object   Object
}

func summarize(findings []Finding) []findingSummary {
	out := make([]findingSummary, 0, len(findings))
	for i := range findings {
		out = append(out, findingSummary{
			Identity: findings[i].Identity,
			Severity: findings[i].Severity,
			Object:   findings[i].Object,
		})
	}
	return out
}

func TestAnalyze_Rules(t *testing.T) {
	tests := []struct {
		name string
		snap health.Snapshot
		want []findingSummary
	}{
		{
			name: "clean snapshot produces no findings",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: true}}
				s.Pods = []health.PodSignal{{Namespace: "default", Name: "ok", Phase: "Running"}}
				s.Deployments = []health.DeploymentSignal{
					{Namespace: "default", Name: "web", DesiredReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
				}
				return s
			}(),
			want: []findingSummary{},
		},
		{
			name: "unreachable cluster is the only finding",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Reachability = health.Reachability{Reachable: false, Error: "dial tcp: connection refused"}
				// Signal slices would be empty in reality; even if not, an
				// unreachable cluster must yield only the unreachability finding.
				s.Nodes = []health.NodeSignal{{Name: "ghost", Ready: false}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "cluster.unreachable", Object{Kind: "cluster", Name: "prod"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "cluster", Name: "prod"},
				},
			},
		},
		{
			name: "not-ready node is critical",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "node.notready", Object{Kind: "node", Name: "node-a"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "node", Name: "node-a"},
				},
			},
		},
		{
			name: "node pressure and cordon are warnings",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Nodes = []health.NodeSignal{{
					Name: "node-a", Ready: true,
					MemoryPressure: true, DiskPressure: true, PIDPressure: true,
					Unschedulable: true,
				}}
				return s
			}(),
			want: []findingSummary{
				// All warning severity, so ordered by identity ascending.
				{
					Identity: newIdentity("prod", "node.cordoned", Object{Kind: "node", Name: "node-a"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "node", Name: "node-a"},
				},
				{
					Identity: newIdentity("prod", "node.diskpressure", Object{Kind: "node", Name: "node-a"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "node", Name: "node-a"},
				},
				{
					Identity: newIdentity("prod", "node.memorypressure", Object{Kind: "node", Name: "node-a"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "node", Name: "node-a"},
				},
				{
					Identity: newIdentity("prod", "node.pidpressure", Object{Kind: "node", Name: "node-a"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "node", Name: "node-a"},
				},
			},
		},
		{
			name: "crashlooping pod is critical and suppresses high-restart warning",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{
					Namespace: "team", Name: "crash", Phase: "Running",
					RestartCount: 12, CrashLoopingContainers: []string{"app"},
				}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "pod.crashloop", Object{Kind: "pod", Namespace: "team", Name: "crash"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "crash"},
				},
			},
		},
		{
			name: "high restart count without crashloop is a warning",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{
					Namespace: "team", Name: "flappy", Phase: "Running", RestartCount: 6,
				}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "pod.highrestarts", Object{Kind: "pod", Namespace: "team", Name: "flappy"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "flappy"},
				},
			},
		},
		{
			name: "restart count just below threshold is silent",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{
					Namespace: "team", Name: "fine", Phase: "Running", RestartCount: highRestartThreshold - 1,
				}}
				return s
			}(),
			want: []findingSummary{},
		},
		{
			name: "pending pod is a warning",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "pod.pending", Object{Kind: "pod", Namespace: "team", Name: "pending"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "pending"},
				},
			},
		},
		{
			name: "failed pod is a warning",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{Namespace: "team", Name: "dead", Phase: "Failed", Reason: "Evicted", Failed: true}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "pod.failed", Object{Kind: "pod", Namespace: "team", Name: "dead"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "dead"},
				},
			},
		},
		{
			name: "degraded deployment is a warning, fully-down is critical",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Deployments = []health.DeploymentSignal{
					{Namespace: "default", Name: "degraded", DesiredReplicas: 3, ReadyReplicas: 2, AvailableReplicas: 2},
					{Namespace: "default", Name: "down", DesiredReplicas: 2, ReadyReplicas: 0, AvailableReplicas: 0},
					// Scaled to zero on purpose: never a finding.
					{Namespace: "default", Name: "scaled-zero", DesiredReplicas: 0},
				}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "deployment.underreplicated", Object{Kind: "deployment", Namespace: "default", Name: "down"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "deployment", Namespace: "default", Name: "down"},
				},
				{
					Identity: newIdentity("prod", "deployment.underreplicated", Object{Kind: "deployment", Namespace: "default", Name: "degraded"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "deployment", Namespace: "default", Name: "degraded"},
				},
			},
		},
		{
			name: "degraded replicaset mirrors deployment logic",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.ReplicaSets = []health.ReplicaSetSignal{
					{Namespace: "default", Name: "rs-down", DesiredReplicas: 2, ReadyReplicas: 0, AvailableReplicas: 0},
				}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "replicaset.underreplicated", Object{Kind: "replicaset", Namespace: "default", Name: "rs-down"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "replicaset", Namespace: "default", Name: "rs-down"},
				},
			},
		},
		{
			name: "warning event on uncovered object surfaces as info",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.WarningEvents = []health.EventSignal{{
					Namespace: "team", Name: "evt", Reason: "Unhealthy", Count: 3,
					InvolvedObject: "Pod/team/probe", LastSeen: fixedTime.Add(-time.Minute),
				}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "event.warning", Object{Kind: "pod", Namespace: "team", Name: "probe"}),
					Severity: SeverityInfo,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "probe"},
				},
			},
		},
		{
			name: "warning event is suppressed when its object already has a finding",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Pods = []health.PodSignal{{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true}}
				// FailedScheduling event about the same pod must NOT add a second finding.
				s.WarningEvents = []health.EventSignal{{
					Namespace: "team", Name: "evt", Reason: "FailedScheduling", Count: 1,
					InvolvedObject: "Pod/team/pending", LastSeen: fixedTime.Add(-time.Minute),
				}}
				return s
			}(),
			want: []findingSummary{
				{
					Identity: newIdentity("prod", "pod.pending", Object{Kind: "pod", Namespace: "team", Name: "pending"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "pending"},
				},
			},
		},
		{
			name: "findings are ordered critical-first then by identity",
			snap: func() health.Snapshot {
				s := baseSnapshot()
				s.Nodes = []health.NodeSignal{{Name: "node-z", Ready: false}}
				s.Pods = []health.PodSignal{{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true}}
				s.WarningEvents = []health.EventSignal{{
					Namespace: "team", Name: "evt", Reason: "Unhealthy", Count: 1,
					InvolvedObject: "Pod/team/other", LastSeen: fixedTime.Add(-time.Minute),
				}}
				return s
			}(),
			want: []findingSummary{
				// critical first
				{
					Identity: newIdentity("prod", "node.notready", Object{Kind: "node", Name: "node-z"}),
					Severity: SeverityCritical,
					Object:   Object{Kind: "node", Name: "node-z"},
				},
				// then warning
				{
					Identity: newIdentity("prod", "pod.pending", Object{Kind: "pod", Namespace: "team", Name: "pending"}),
					Severity: SeverityWarning,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "pending"},
				},
				// then info
				{
					Identity: newIdentity("prod", "event.warning", Object{Kind: "pod", Namespace: "team", Name: "other"}),
					Severity: SeverityInfo,
					Object:   Object{Kind: "pod", Namespace: "team", Name: "other"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Analyze(tt.snap)

			// Every finding must carry the snapshot's collection time, never a
			// clock of its own.
			for i := range got {
				if !got[i].DetectedAt.Equal(fixedTime) {
					t.Errorf("finding %d DetectedAt = %v, want %v", i, got[i].DetectedAt, fixedTime)
				}
				if got[i].Cluster != tt.snap.Cluster {
					t.Errorf("finding %d Cluster = %q, want %q", i, got[i].Cluster, tt.snap.Cluster)
				}
				if got[i].Title == "" || got[i].Message == "" {
					t.Errorf("finding %d has empty title/message: %+v", i, got[i])
				}
			}

			if diff := summarize(got); !reflect.DeepEqual(diff, tt.want) {
				t.Errorf("findings mismatch:\n got %+v\nwant %+v", diff, tt.want)
			}
		})
	}
}

// TestAnalyze_StableIdentityAcrossCycles proves the core dedup property: the
// same ongoing problem, observed in two different collection cycles (different
// timestamps, evolved message), yields findings with the SAME identity, while a
// genuinely different problem yields a different one.
func TestAnalyze_StableIdentityAcrossCycles(t *testing.T) {
	cycleOne := baseSnapshot()
	cycleOne.Pods = []health.PodSignal{{
		Namespace: "team", Name: "crash", Phase: "Running",
		RestartCount: 4, CrashLoopingContainers: []string{"app"},
	}}

	cycleTwo := baseSnapshot()
	cycleTwo.CollectedAt = fixedTime.Add(5 * time.Minute) // later cycle
	cycleTwo.Pods = []health.PodSignal{{
		Namespace: "team", Name: "crash", Phase: "Running",
		RestartCount: 99, CrashLoopingContainers: []string{"app", "sidecar"}, // worsened
	}}

	one := Analyze(cycleOne)
	two := Analyze(cycleTwo)
	if len(one) != 1 || len(two) != 1 {
		t.Fatalf("expected one finding per cycle, got %d and %d", len(one), len(two))
	}
	if one[0].Identity != two[0].Identity {
		t.Fatalf("identity changed across cycles for the same problem: %q vs %q",
			one[0].Identity, two[0].Identity)
	}
	// The evolving message and timestamp must NOT be folded into identity.
	if one[0].Message == two[0].Message {
		t.Fatal("expected the message to evolve between cycles (test fixture error)")
	}

	// A different pod is a different problem with a different identity.
	other := baseSnapshot()
	other.Pods = []health.PodSignal{{
		Namespace: "team", Name: "different", Phase: "Running", CrashLoopingContainers: []string{"app"},
	}}
	otherFindings := Analyze(other)
	if otherFindings[0].Identity == one[0].Identity {
		t.Fatal("expected distinct objects to produce distinct identities")
	}
}

// TestAnalyze_Deterministic proves repeated analysis of the same snapshot is
// byte-for-byte identical, including ordering.
func TestAnalyze_Deterministic(t *testing.T) {
	s := baseSnapshot()
	s.Nodes = []health.NodeSignal{{Name: "node-a", Ready: false, MemoryPressure: true}}
	s.Pods = []health.PodSignal{
		{Namespace: "team", Name: "crash", Phase: "Running", CrashLoopingContainers: []string{"app"}},
		{Namespace: "team", Name: "pending", Phase: "Pending", Pending: true},
	}
	s.Deployments = []health.DeploymentSignal{
		{Namespace: "default", Name: "web", DesiredReplicas: 3, AvailableReplicas: 1},
	}

	first := Analyze(s)
	second := Analyze(s)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("analysis is not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}
	if len(first) == 0 {
		t.Fatal("expected findings for a degraded snapshot")
	}
}

// TestSeverityString guards the stable lowercase tokens the rest of the package
// (and downstream consumers) depend on.
func TestSeverityString(t *testing.T) {
	cases := map[Severity]string{
		SeverityInfo:     "info",
		SeverityWarning:  "warning",
		SeverityCritical: "critical",
	}
	for sev, want := range cases {
		if got := sev.String(); got != want {
			t.Errorf("Severity(%d).String() = %q, want %q", int(sev), got, want)
		}
	}
	// Ordering must rank critical above warning above info for the sort.
	if !(SeverityCritical > SeverityWarning && SeverityWarning > SeverityInfo) {
		t.Fatal("severity ordering is not critical > warning > info")
	}
}
