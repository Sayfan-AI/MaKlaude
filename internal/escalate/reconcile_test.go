package escalate

import (
	"reflect"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

var ts = time.Date(2026, time.June, 17, 12, 0, 0, 0, time.UTC)

// finding is a tiny helper that builds a finding with a given cluster/identity
// and severity, enough to seed a subject (whose reconcile keys only on the
// derived incident identity).
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

// incidentID mirrors correlate.newIncidentIdentity so tests can predict the
// incident identity a subject built from a primary finding will carry — the key
// reconcile joins on.
func incidentID(primary detect.Identity) correlate.IncidentIdentity {
	return correlate.IncidentIdentity("incident|" + string(primary))
}

// subjectFor wraps a single finding as a singleton incident with one
// fallback-style hypothesis — exactly the shape correlate+diagnose produce for an
// uncorrelated finding — so reconcile/escalator tests can drive the
// incident-granularity API without building a full snapshot. Extra effect findings
// (correlated symptoms) may be supplied to exercise multi-object incidents.
func subjectFor(primary detect.Finding, effects ...detect.Finding) Subject {
	inc := correlate.Incident{
		Identity:   incidentID(primary.Identity),
		Cluster:    primary.Cluster,
		Primary:    primary,
		Effects:    append([]detect.Finding(nil), effects...),
		DetectedAt: primary.DetectedAt,
	}
	evidence := inc.Findings()
	h := diagnose.Hypothesis{
		Identity:   diagnose.HypothesisIdentity("hypothesis|unknown|" + string(inc.Identity)),
		Incident:   inc.Identity,
		Cluster:    primary.Cluster,
		Cause:      diagnose.CauseUnknown,
		Confidence: diagnose.ConfidenceLow,
		Title:      "Suspected cause: " + primary.Title,
		Message:    "No specialized diagnosis rule matched this incident.",
		Evidence:   evidence,
		Source:     diagnose.SourceDeterministic,
		DetectedAt: primary.DetectedAt,
	}
	return Subject{Incident: inc, Hypotheses: []diagnose.Hypothesis{h}}
}

// subjectsFor builds subjects for a set of primary findings, one singleton
// incident each.
func subjectsFor(findings ...detect.Finding) []Subject {
	out := make([]Subject, 0, len(findings))
	for _, f := range findings {
		out = append(out, subjectFor(f))
	}
	return out
}

// planSummary is the assertable shape of a plan: kind + incident identity + ref.
// The subject payload is exercised by the escalator/formatting tests.
type planSummary struct {
	Kind     ActionKind
	Identity correlate.IncidentIdentity
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
		subjects []Subject
		tracked  []TrackedIssue
		want     []planSummary
	}{
		{
			name:     "nothing detected, nothing tracked -> empty plan",
			subjects: nil,
			tracked:  nil,
			want:     []planSummary{},
		},
		{
			name:     "new incident with no tracked issue -> open",
			subjects: subjectsFor(finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical)),
			tracked:  nil,
			want: []planSummary{
				{Kind: ActionOpen, Identity: incidentID("prod|pod.crashloop|pod/ns/a")},
			},
		},
		{
			name:     "recurring incident with a tracked issue -> update, not a second open",
			subjects: subjectsFor(finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical)),
			tracked:  []TrackedIssue{{Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "7"}},
			want: []planSummary{
				{Kind: ActionUpdate, Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "7"},
			},
		},
		{
			name:     "tracked issue with no current incident -> close (cleared)",
			subjects: nil,
			tracked:  []TrackedIssue{{Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "7"}},
			want: []planSummary{
				{Kind: ActionClose, Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "7"},
			},
		},
		{
			name: "severity change does not change the action (still an update by identity)",
			// Same identity, different severity than whatever the issue was opened
			// at: reconcile keys on incident identity only, so it is an update.
			subjects: subjectsFor(finding("prod", "prod|node.notready|node/n", detect.SeverityWarning)),
			tracked:  []TrackedIssue{{Identity: incidentID("prod|node.notready|node/n"), Ref: "3"}},
			want: []planSummary{
				{Kind: ActionUpdate, Identity: incidentID("prod|node.notready|node/n"), Ref: "3"},
			},
		},
		{
			name: "mixed: one new, one recurring, one cleared",
			subjects: subjectsFor(
				finding("prod", "prod|new|pod/ns/new", detect.SeverityWarning),
				finding("prod", "prod|recurring|pod/ns/rec", detect.SeverityCritical),
			),
			tracked: []TrackedIssue{
				{Identity: incidentID("prod|recurring|pod/ns/rec"), Ref: "10"},
				{Identity: incidentID("prod|cleared|pod/ns/old"), Ref: "11"},
			},
			want: []planSummary{
				// closes first, then opens, then updates; each group id-sorted.
				{Kind: ActionClose, Identity: incidentID("prod|cleared|pod/ns/old"), Ref: "11"},
				{Kind: ActionOpen, Identity: incidentID("prod|new|pod/ns/new")},
				{Kind: ActionUpdate, Identity: incidentID("prod|recurring|pod/ns/rec"), Ref: "10"},
			},
		},
		{
			name: "multi-cluster isolation: same problem on two clusters are distinct issues",
			subjects: subjectsFor(
				finding("prod", "prod|pod.crashloop|pod/ns/a", detect.SeverityCritical),
				finding("staging", "staging|pod.crashloop|pod/ns/a", detect.SeverityCritical),
			),
			tracked: []TrackedIssue{
				// prod already tracked -> update; staging not tracked -> open.
				{Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "1"},
			},
			want: []planSummary{
				{Kind: ActionOpen, Identity: incidentID("staging|pod.crashloop|pod/ns/a")},
				{Kind: ActionUpdate, Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "1"},
			},
		},
		{
			name: "clearing one cluster does not touch the other cluster's issue",
			subjects: subjectsFor(
				// only staging is still active
				finding("staging", "staging|pod.crashloop|pod/ns/a", detect.SeverityCritical),
			),
			tracked: []TrackedIssue{
				{Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "1"},
				{Identity: incidentID("staging|pod.crashloop|pod/ns/a"), Ref: "2"},
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: incidentID("prod|pod.crashloop|pod/ns/a"), Ref: "1"},
				{Kind: ActionUpdate, Identity: incidentID("staging|pod.crashloop|pod/ns/a"), Ref: "2"},
			},
		},
		{
			name: "duplicate subjects for one identity collapse to a single open",
			subjects: []Subject{
				subjectFor(finding("prod", "prod|dup|pod/ns/a", detect.SeverityWarning)),
				subjectFor(finding("prod", "prod|dup|pod/ns/a", detect.SeverityCritical)),
			},
			tracked: nil,
			want: []planSummary{
				{Kind: ActionOpen, Identity: incidentID("prod|dup|pod/ns/a")},
			},
		},
		{
			name:     "duplicate open issues for one active identity self-heal: first updates, rest close",
			subjects: subjectsFor(finding("prod", "prod|dup|pod/ns/a", detect.SeverityWarning)),
			tracked: []TrackedIssue{
				{Identity: incidentID("prod|dup|pod/ns/a"), Ref: "5"},
				{Identity: incidentID("prod|dup|pod/ns/a"), Ref: "6"},
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: incidentID("prod|dup|pod/ns/a"), Ref: "6"},
				{Kind: ActionUpdate, Identity: incidentID("prod|dup|pod/ns/a"), Ref: "5"},
			},
		},
		{
			name: "plan ordering is deterministic across many items",
			subjects: subjectsFor(
				finding("prod", "prod|b|pod/ns/b", detect.SeverityWarning),
				finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical),
			),
			tracked: []TrackedIssue{
				{Identity: incidentID("prod|z|pod/ns/z"), Ref: "9"}, // cleared
				{Identity: incidentID("prod|y|pod/ns/y"), Ref: "8"}, // cleared
			},
			want: []planSummary{
				{Kind: ActionClose, Identity: incidentID("prod|y|pod/ns/y"), Ref: "8"},
				{Kind: ActionClose, Identity: incidentID("prod|z|pod/ns/z"), Ref: "9"},
				{Kind: ActionOpen, Identity: incidentID("prod|a|pod/ns/a")},
				{Kind: ActionOpen, Identity: incidentID("prod|b|pod/ns/b")},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizePlan(Reconcile(tt.subjects, tt.tracked))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plan mismatch:\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestReconcile_Deterministic proves repeated reconciliation of the same input
// is byte-for-byte identical, including ordering, despite internal map use.
func TestReconcile_Deterministic(t *testing.T) {
	subjects := subjectsFor(
		finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical),
		finding("prod", "prod|b|pod/ns/b", detect.SeverityWarning),
		finding("staging", "staging|c|pod/ns/c", detect.SeverityInfo),
	)
	tracked := []TrackedIssue{
		{Identity: incidentID("prod|a|pod/ns/a"), Ref: "1"},
		{Identity: incidentID("prod|gone|pod/ns/x"), Ref: "2"},
	}
	first := Reconcile(subjects, tracked)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, Reconcile(subjects, tracked)) {
			t.Fatalf("reconcile is not deterministic on iteration %d", i)
		}
	}
}

// TestReconcile_PureNoMutation guards that reconcile does not mutate its inputs.
func TestReconcile_PureNoMutation(t *testing.T) {
	subjects := subjectsFor(finding("prod", "prod|a|pod/ns/a", detect.SeverityCritical))
	tracked := []TrackedIssue{{Identity: incidentID("prod|gone|pod/ns/x"), Ref: "2"}}
	subjectsCopy := append([]Subject(nil), subjects...)
	trackedCopy := append([]TrackedIssue(nil), tracked...)

	_ = Reconcile(subjects, tracked)

	if !reflect.DeepEqual(subjects, subjectsCopy) {
		t.Error("Reconcile mutated its subjects slice")
	}
	if !reflect.DeepEqual(tracked, trackedCopy) {
		t.Error("Reconcile mutated its tracked slice")
	}
}
