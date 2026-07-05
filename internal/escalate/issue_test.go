package escalate

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

// sampleFinding builds the crashloop primary finding used across the issue tests.
func sampleFinding(sev detect.Severity) detect.Finding {
	return detect.Finding{
		Identity:   "prod|pod.crashloop|pod/team/api",
		Severity:   sev,
		Cluster:    "prod",
		Object:     detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
		Title:      "Pod crashlooping",
		Message:    "Pod team/api has crashlooping container(s): app",
		DetectedAt: ts,
	}
}

// sampleSubject wraps the sample crashloop finding as a singleton incident with a
// single fallback hypothesis. It is the minimal subject the marker / title / label
// tests exercise.
func sampleSubject(sev detect.Severity) Subject {
	return subjectFor(sampleFinding(sev))
}

// hypo is a compact hypothesis constructor for the body-rendering tests.
func hypo(inc correlate.IncidentIdentity, cause diagnose.Cause, conf diagnose.Confidence, title, msg string, evidence ...detect.Finding) diagnose.Hypothesis {
	return diagnose.Hypothesis{
		Identity:   diagnose.HypothesisIdentity("hypothesis|" + string(cause) + "|" + string(inc)),
		Incident:   inc,
		Cluster:    "prod",
		Cause:      cause,
		Confidence: conf,
		Title:      title,
		Message:    msg,
		Evidence:   append([]detect.Finding(nil), evidence...),
		Source:     diagnose.SourceDeterministic,
		DetectedAt: ts,
	}
}

func TestParseIdentityMarker_RoundTrip(t *testing.T) {
	id := correlate.IncidentIdentity("incident|prod|node.notready|node/n1")
	body := "preamble\n" + identityMarker(id) + "\nepilogue"
	got, ok := ParseIdentityMarker(body)
	if !ok || got != id {
		t.Fatalf("round-trip failed: got %q ok=%v, want %q", got, ok, id)
	}
}

func TestParseIdentityMarker_Negative(t *testing.T) {
	cases := map[string]string{
		"no marker":       "just a normal issue body",
		"empty body":      "",
		"unterminated":    "<!-- maklaude:identity=incident|prod|x",
		"empty identity":  "<!-- maklaude:identity= -->",
		"whitespace only": "<!-- maklaude:identity=   -->",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseIdentityMarker(body); ok {
				t.Errorf("expected no marker parsed from %q", body)
			}
		})
	}
}

func TestParseIdentityMarker_FromFullBody(t *testing.T) {
	// The marker must survive being embedded in a real rendered body.
	s := sampleSubject(detect.SeverityCritical)
	body := Body(s)
	got, ok := ParseIdentityMarker(body)
	if !ok || got != s.Identity() {
		t.Fatalf("could not recover identity from full body: got %q ok=%v", got, ok)
	}
}

func TestParseThreadMarker_RoundTrip(t *testing.T) {
	const tts = "1700000000.000123"
	body := "preamble\n" + identityMarker("incident|prod|x|y") + "\n" + threadMarker(tts) + "\nepilogue"
	got, ok := ParseThreadMarker(body)
	if !ok || got != tts {
		t.Fatalf("round-trip failed: got %q ok=%v, want %q", got, ok, tts)
	}
	// The identity marker must still parse alongside it.
	if _, ok := ParseIdentityMarker(body); !ok {
		t.Error("identity marker should coexist with the thread marker")
	}
}

func TestParseThreadMarker_Negative(t *testing.T) {
	cases := map[string]string{
		"no marker":     "just a normal issue body",
		"empty body":    "",
		"unterminated":  "<!-- maklaude:thread=1700.0001",
		"empty ts":      "<!-- maklaude:thread= -->",
		"whitespace ts": "<!-- maklaude:thread=   -->",
		"identity only": Body(sampleSubject(detect.SeverityCritical)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseThreadMarker(body); ok {
				t.Errorf("expected no thread marker parsed from %q", body)
			}
		})
	}
}

func TestWithThreadMarker(t *testing.T) {
	s := sampleSubject(detect.SeverityCritical)
	base := Body(s)

	// Adding a marker makes it parseable without disturbing the identity marker.
	withTS := withThreadMarker(base, "111.0001")
	if got, ok := ParseThreadMarker(withTS); !ok || got != "111.0001" {
		t.Fatalf("withThreadMarker did not embed ts: got %q ok=%v", got, ok)
	}
	if id, ok := ParseIdentityMarker(withTS); !ok || id != s.Identity() {
		t.Errorf("identity marker disturbed: got %q ok=%v", id, ok)
	}

	// Re-applying replaces, never accumulates.
	replaced := withThreadMarker(withTS, "222.0002")
	if got, _ := ParseThreadMarker(replaced); got != "222.0002" {
		t.Errorf("re-apply should replace ts, got %q", got)
	}
	if n := strings.Count(replaced, threadMarkerPrefix); n != 1 {
		t.Errorf("want exactly one thread marker after re-apply, got %d", n)
	}

	// An empty ts strips any existing marker and leaves a plain body.
	stripped := withThreadMarker(withTS, "")
	if _, ok := ParseThreadMarker(stripped); ok {
		t.Error("empty ts should strip the thread marker")
	}
	if _, ok := ParseIdentityMarker(stripped); !ok {
		t.Error("stripping the thread marker must not remove the identity marker")
	}
}

func TestTitle_NamesClusterAndSeverity(t *testing.T) {
	got := Title(sampleSubject(detect.SeverityCritical))
	for _, want := range []string{"CRITICAL", "prod", "Pod crashlooping"} {
		if !strings.Contains(got, want) {
			t.Errorf("title %q missing %q", got, want)
		}
	}
}

func TestBody_ContainsContextAndGate(t *testing.T) {
	critical := Body(sampleSubject(detect.SeverityCritical))
	for _, want := range []string{"prod", "pod/team/api", "critical", "crashlooping container", NeedsHumanLabel} {
		if !strings.Contains(critical, want) {
			t.Errorf("body missing %q:\n%s", want, critical)
		}
	}
	// Info incidents must not advertise the human gate.
	info := Body(sampleSubject(detect.SeverityInfo))
	if strings.Contains(info, "warrants a human decision") {
		t.Errorf("info body should not mention the human gate:\n%s", info)
	}
}

// TestBody_RanksHypothesesWithEvidence proves the diagnostic body presents the
// ranked hypotheses in order, marks the leading one, and groups each hypothesis's
// supporting evidence findings under it.
func TestBody_RanksHypothesesWithEvidence(t *testing.T) {
	primary := detect.Finding{
		Identity: "prod|deploy.unavailable|deployment/team/web", Severity: detect.SeverityCritical, Cluster: "prod",
		Object: detect.Object{Kind: "deployment", Namespace: "team", Name: "web"},
		Title:  "Deployment unavailable", Message: "0/3 replicas available", DetectedAt: ts,
	}
	podEvidence := detect.Finding{
		Identity: "prod|pod.crashloop|pod/team/web-abc", Severity: detect.SeverityCritical, Cluster: "prod",
		Object: detect.Object{Kind: "pod", Namespace: "team", Name: "web-abc"},
		Title:  "Pod crashlooping", Message: "ImagePullBackOff", DetectedAt: ts,
	}
	inc := incidentID(primary.Identity)
	s := Subject{
		Incident: correlate.Incident{
			Identity: inc, Cluster: "prod", Primary: primary,
			Effects: []detect.Finding{podEvidence}, DetectedAt: ts,
		},
		Hypotheses: []diagnose.Hypothesis{
			hypo(inc, diagnose.CauseBadImage, diagnose.ConfidenceHigh,
				"Unpullable or invalid container image", "A container image cannot be pulled.", primary, podEvidence),
			hypo(inc, diagnose.CauseOOMKill, diagnose.ConfidenceMedium,
				"Container OOM-killed (exceeds memory limit)", "A container was OOM-killed on a previous restart.", podEvidence),
		},
	}

	body := Body(s)

	// Both hypotheses appear, ranked, with confidences.
	for _, want := range []string{
		"Ranked root-cause hypotheses",
		"1. Unpullable or invalid container image",
		"confidence: high",
		"leading hypothesis",
		"2. Container OOM-killed",
		"confidence: medium",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// The leading (high-confidence) hypothesis must appear BEFORE the medium one.
	if strings.Index(body, "Unpullable") > strings.Index(body, "OOM-killed") {
		t.Errorf("hypotheses not ranked most-confident-first:\n%s", body)
	}
	// Evidence findings are grouped under the hypotheses.
	if !strings.Contains(body, "Evidence:") {
		t.Errorf("body should group evidence under hypotheses:\n%s", body)
	}
	if !strings.Contains(body, "`pod/team/web-abc`") {
		t.Errorf("evidence should cite the specific finding object:\n%s", body)
	}
	// Affected objects section lists both the primary and the effect.
	if !strings.Contains(body, "Affected objects") ||
		!strings.Contains(body, "`deployment/team/web`") ||
		!strings.Contains(body, "_(primary)_") {
		t.Errorf("body should list affected objects with roles:\n%s", body)
	}
}

// TestBody_LowConfidenceSurfacesCompetingHypotheses proves that when the leading
// hypothesis is uncertain (low confidence), the body honestly flags that and
// invites weighing the alternatives rather than overcommitting.
func TestBody_LowConfidenceSurfacesCompetingHypotheses(t *testing.T) {
	primary := detect.Finding{
		Identity: "prod|pod.crashloop|pod/team/api", Severity: detect.SeverityCritical, Cluster: "prod",
		Object: detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
		Title:  "Pod crashlooping", Message: "restarting repeatedly", DetectedAt: ts,
	}
	inc := incidentID(primary.Identity)
	s := Subject{
		Incident: correlate.Incident{Identity: inc, Cluster: "prod", Primary: primary, DetectedAt: ts},
		Hypotheses: []diagnose.Hypothesis{
			hypo(inc, diagnose.CauseUnknown, diagnose.ConfidenceLow,
				"Suspected cause: Pod crashlooping", "No specialized rule matched.", primary),
		},
	}
	body := Body(s)
	if !strings.Contains(strings.ToLower(body), "low-confidence") {
		t.Errorf("low-confidence leading hypothesis should be flagged as uncertain:\n%s", body)
	}
	if !strings.Contains(strings.ToLower(body), "weigh the alternatives") {
		t.Errorf("uncertain body should invite weighing alternatives:\n%s", body)
	}
}

// TestBody_NextStepsAreDiagnosticOnly proves the suggested next steps are manual,
// read-only investigations — kubectl describe/logs/get/top and the like — and
// NEVER contain a mutating kubectl verb.
func TestBody_NextStepsAreDiagnosticOnly(t *testing.T) {
	primary := detect.Finding{
		Identity: "prod|pod.pending|pod/team/api", Severity: detect.SeverityWarning, Cluster: "prod",
		Object: detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
		Title:  "Pod pending", Message: "unschedulable", DetectedAt: ts,
	}
	inc := incidentID(primary.Identity)
	s := Subject{
		Incident: correlate.Incident{Identity: inc, Cluster: "prod", Primary: primary, DetectedAt: ts},
		Hypotheses: []diagnose.Hypothesis{
			hypo(inc, diagnose.CauseInsufficientResources, diagnose.ConfidenceHigh,
				"Insufficient cluster resources to schedule pod", "The scheduler cannot place the pod.", primary),
		},
	}
	body := Body(s)

	if !strings.Contains(body, "Suggested next steps") {
		t.Fatalf("body should include a next-steps section:\n%s", body)
	}
	// A diagnostic verb must be present.
	if !strings.Contains(body, "kubectl describe") {
		t.Errorf("next steps should include read-only diagnostics like `kubectl describe`:\n%s", body)
	}
	// No mutating kubectl verb may appear ANYWHERE in the body.
	for _, forbidden := range []string{
		"kubectl delete", "kubectl apply", "kubectl scale", "kubectl edit",
		"kubectl patch", "kubectl rollout restart", "kubectl drain", "kubectl cordon",
		"kubectl create", "kubectl replace", "kubectl set ",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("body must not suggest the mutating command %q:\n%s", forbidden, body)
		}
	}
}

// TestBody_NeverClaimsMaKlaudeWillAct is the read-only safety assertion from #63:
// the body must never claim MaKlaude will run/apply/delete/scale/remediate
// anything. M3 is strictly read-only, and the human-facing trail must say so.
func TestBody_NeverClaimsMaKlaudeWillAct(t *testing.T) {
	// Exercise every cause so any cause-specific next-steps wording is covered.
	for _, cause := range []diagnose.Cause{
		diagnose.CauseBadImage, diagnose.CauseInsufficientResources,
		diagnose.CauseNodeFailure, diagnose.CauseOOMKill, diagnose.CauseUnknown,
	} {
		primary := detect.Finding{
			Identity: detect.Identity("prod|x|pod/team/api-" + string(cause)), Severity: detect.SeverityCritical,
			Cluster: "prod", Object: detect.Object{Kind: "pod", Namespace: "team", Name: "api"},
			Title: "Problem", Message: "detail", DetectedAt: ts,
		}
		inc := incidentID(primary.Identity)
		s := Subject{
			Incident:   correlate.Incident{Identity: inc, Cluster: "prod", Primary: primary, DetectedAt: ts},
			Hypotheses: []diagnose.Hypothesis{hypo(inc, cause, diagnose.ConfidenceHigh, "T", "m", primary)},
		}
		lower := strings.ToLower(Body(s))
		for _, forbidden := range []string{
			"maklaude will run", "maklaude will apply", "maklaude will delete",
			"maklaude will scale", "maklaude will restart", "maklaude will remediate",
			"maklaude will fix", "maklaude will drain", "maklaude will cordon",
			"maklaude has applied", "maklaude has deleted", "auto-remediate",
			"automatically remediate", "will automatically fix", "maklaude will execute",
		} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("cause %q: body claims a mutating action (%q) — M3 is read-only:\n%s", cause, forbidden, Body(s))
			}
		}
		// It must positively state MaKlaude does not mutate/remediate.
		if !strings.Contains(lower, "read-only") {
			t.Errorf("cause %q: body should assert the read-only boundary:\n%s", cause, Body(s))
		}
	}
}

func TestLabelsFor(t *testing.T) {
	cases := []struct {
		sev       detect.Severity
		wantHuman bool
	}{
		{detect.SeverityInfo, false},
		{detect.SeverityWarning, true},
		{detect.SeverityCritical, true},
	}
	for _, c := range cases {
		labels := LabelsFor(sampleSubject(c.sev))
		if !hasLabel(labels, ManagedLabel) {
			t.Errorf("sev %s: missing managed label", c.sev)
		}
		if got := hasLabel(labels, NeedsHumanLabel); got != c.wantHuman {
			t.Errorf("sev %s: needs:human = %v, want %v", c.sev, got, c.wantHuman)
		}
	}
}

func TestRecurrenceAndClosingComments(t *testing.T) {
	rec := RecurrenceComment(sampleSubject(detect.SeverityWarning))
	if !strings.Contains(rec, "Still observed") || !strings.Contains(rec, "crashlooping container") {
		t.Errorf("recurrence comment not well-formed: %q", rec)
	}
	cl := ClosingComment("incident|prod|x|y")
	if !strings.Contains(cl, "cleared") || !strings.Contains(cl, "incident|prod|x|y") {
		t.Errorf("closing comment not well-formed: %q", cl)
	}
}
