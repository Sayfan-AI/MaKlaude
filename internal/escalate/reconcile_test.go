package escalate

import (
	"reflect"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

var ts = time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)

// finding is a tiny helper that builds a finding with a given cluster/identity
// and severity, enough to drive reconcile (which only keys on identity).
func finding(cluster string, id detect.Identity, sev detect.Severity) detect.Finding {
	return detect.Finding{
		Identity:   id,
		Severity:   sev,
		Cluster:    cluster,
		Object:     detect.Object{Kind: "pod", Namespace: "ns", Name: string(id)},
		Title:      "Problem " + string(id),
		Message:    "details for " + string(id),
		DetectedAt: ts,
	}
}

// planSummary is the assertable shape of a plan: kind + identity + ref. The
// finding payload is exercised by the escalator/formatting tests.
type planSummary struct {
	Kind     ActionKind
	Identity detect.Identity
	Ref      IssueRef
}

func summarizePlan(plan []Action) []planSummary {
	out := make([]planSummary, 0, len(plan))
	for _, a := range plan {
		out = append(out, planSummary{Kind: a.Kind, Identity: a.Identity, Ref: a.Ref})
	}
	return out
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name     string
		findings []detect.Finding
		tracked  []TrackedIssue
		want     []planSummary
	}{
		{
			name:     "nothing detected, nothing tracked -> empty plan",
			findings: nil,
			tracked:  nil,
			want:     []planSummary{},
		},
		{
			name:     "new finding with no tracked issue -> open",
			findings: []detect.Finding{finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical)},
			tracked:  nil,
			want: []planSummary{
				{Kind: ActionOpen, Identity: "prod|pod.crashloop|pod/ns/a"},
			},
		},
		{
			name:     "recurring finding with a tracked issue -> update, not a second open",
			findings: []detect.Finding{finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical)},
			tracked:  []TrackedIssue{{Identity: "prod|pod.crashloop|pod/ns/a", Ref: "7"}},
			want: []planSummary{
				{Kind: ActionUpdate, Identity: "prod|pod.crashloop|pod/ns/a", Ref: "7"},
			},
		},
		{
			name:     "tracked issue with no current finding -> close (cleared)",
			findings: nil,
			tracked:  []TrackedIssue{{Identity: "prod|pod.crashloop|pod/ns/a", Ref: "7"}},
			want: []planSummary{
				{Kind: ActionClose, Identity: "prod|pod.crashloop|pod/ns/a", Ref: "7"},
			},
		},
		{
			name: "severity change does not change the action (still an update by identity)",
			// Same identity, different severity than whatever the issue was opened
			// at: reconcile keys on identity only, so it is an update.
			findings: []detect.Finding{finding("prod", "prod|node.notready|node/n", detect.SeverityWarning)},
			tracked:  []TrackedIssue{{Identity: "prod|node.notready|node/n", Ref: "3"}},
			want: []planSummary{
				{Kind: ActionUpdate, Identity: "prod|node.notready|node/n", Ref: "3"},
			},
		},
		{
			name: "mixed: one new, one recurring, one cleared",
			findings: []detect.Finding{
				finding("prod", "prod|new|pod/ns/new", detect.SeverityWarning),
				finding("prod", "prod|recurring|pod/ns/rec", detect.SeverityCritical),
			},
			tracked: []TrackedIssue{
				{Identity: "prod|recurring|pod/ns/rec", Ref: "10"},
				{Identity: "prod|cleared|pod/ns/old", Ref: "11"},
			},
			want: []planSummary{
				// closes first, then opens, then updates; each group id-sorted.
				{Kind: ActionClose, Identity: "prod|cleared|pod/ns/old", Ref: "11"},
				{Kind: ActionOpen, Identity: "prod|new|pod/ns/new"},
				{Kind: ActionUpdate, Identity: "prod|recurring|pod/ns/rec", Ref: "10"},
			},
		},
		{
			name: "multi-cluster isolation: same problem on two clusters are distinct issues",
			findings: []detect.Finding{
				finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical),
				finding("staging", "staging|pod.crashloop|pod/ns/a", detect.SeverityCritical),
			},
			tracked: []TrackedIssue{
				// prod already tracked -> update; staging not tracked -> open.
				{Identity: "prod|pod.crashloop|pod/ns/a", Ref: "1"},
			},
			want: []planSummary{
				{Kind: ActionOpen, Identity: "staging|pod.crashloop|pod/ns/a"},
				{Kind: ActionUpdate, Identity: "prod|pod.crashloop|pod/ns/a", Ref: "1"},
			},
		},
		{
			name: "clearing one cluster does not touch the other cluster's issue",
			findings: []detect.Finding{
				// only staging is still active
				finding("staging", "staging|pod.crashloop|pod/ns/a", detect.SeverityCritical),
			},
			tracked: []TrackedIssue{
				{Identity: "prod|pod.crashloop|pod/ns/a", Ref: "1"},
				{Identity: "staging|pod.crashloop|pod/ns/a", Ref: "2"},
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: "prod|pod.crashloop|pod/ns/a", Ref: "1"},
				{Kind: ActionUpdate, Identity: "staging|pod.crashloop|pod/ns/a", Ref: "2"},
			},
		},
		{
			name:     "duplicate findings for one identity collapse to a single open",
			findings: []detect.Finding{finding("prod", "prod|dup|pod/ns/a", detect.SeverityWarning), finding("prod", "prod|dup|pod/ns/a", detect.SeverityCritical)},
			tracked:  nil,
			want: []planSummary{
				{Kind: ActionOpen, Identity: "prod|dup|pod/ns/a"},
			},
		},
		{
			name:     "duplicate open issues for one active identity self-heal: first updates, rest close",
			findings: []detect.Finding{finding("prod", "prod|dup|pod/ns/a", detect.SeverityWarning)},
			tracked: []TrackedIssue{
				{Identity: "prod|dup|pod/ns/a", Ref: "5"},
				{Identity: "prod|dup|pod/ns/a", Ref: "6"},
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: "prod|dup|pod/ns/a", Ref: "6"},
				{Kind: ActionUpdate, Identity: "prod|dup|pod/ns/a", Ref: "5"},
			},
		},
		{
			name: "plan ordering is deterministic across many items",
			findings: []detect.Finding{
				finding("prod", "prod|b|pod/ns/b", detect.SeverityWarning),
				finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical),
			},
			tracked: []TrackedIssue{
				{Identity: "prod|z|pod/ns/z", Ref: "9"}, // cleared
				{Identity: "prod|y|pod/ns/y", Ref: "8"}, // cleared
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: "prod|y|pod/ns/y", Ref: "8"},
				{Kind: ActionClose, Identity: "prod|z|pod/ns/z", Ref: "9"},
				{Kind: ActionOpen, Identity: "prod|a|pod/ns/a"},
				{Kind: ActionOpen, Identity: "prod|b|pod/ns/b"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizePlan(Reconcile(tt.findings, tt.tracked))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plan mismatch:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestReconcile_Deterministic proves repeated reconciliation of the same input
// is byte-for-byte identical, including ordering, despite internal map use.
func TestReconcile_Deterministic(t *testing.T) {
	findings := []detect.Finding{
		finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical),
		finding("prod", "prod|b|pod/ns/b", detect.SeverityWarning),
		finding("staging", "staging|c|pod/ns/c", detect.SeverityInfo),
	}
	tracked := []TrackedIssue{
		{Identity: "prod|a|pod/ns/a", Ref: "1"},
		{Identity: "prod|gone|pod/ns/x", Ref: "2"},
	}
	first := Reconcile(findings, tracked)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, Reconcile(findings, tracked)) {
			t.Fatalf("reconcile is not deterministic on iteration %d", i)
		}
	}
}

// TestReconcile_PureNoMutation guards that reconcile does not mutate its inputs.
func TestReconcile_PureNoMutation(t *testing.T) {
	findings := []detect.Finding{finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical)}
	tracked := []TrackedIssue{{Identity: "prod|gone|pod/ns/x", Ref: "2"}}
	findingsCopy := append([]detect.Finding(nil), findings...)
	trackedCopy := append([]TrackedIssue(nil), tracked...)

	_ = Reconcile(findings, tracked)

	if !reflect.DeepEqual(findings, findingsCopy) {
		t.Error("Reconcile mutated its findings slice")
	}
	if !reflect.DeepEqual(tracked, trackedCopy) {
		t.Error("Reconcile mutated its tracked slice")
	}
}
