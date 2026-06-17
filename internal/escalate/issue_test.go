package escalate

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

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

func TestParseIdentityMarker_RoundTrip(t *testing.T) {
	id := detect.Identity("prod|node.notready|node/n1")
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
		"unterminated":    "<!-- maklaude:identity=prod|x",
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
	body := Body(sampleFinding(detect.SeverityCritical))
	got, ok := ParseIdentityMarker(body)
	if !ok || got != sampleFinding(detect.SeverityCritical).Identity {
		t.Fatalf("could not recover identity from full body: got %q ok=%v", got, ok)
	}
}

func TestTitle_NamesClusterAndSeverity(t *testing.T) {
	got := Title(sampleFinding(detect.SeverityCritical))
	for _, want := range []string{"CRITICAL", "prod", "Pod crashlooping"} {
		if !strings.Contains(got, want) {
			t.Errorf("title %q missing %q", got, want)
		}
	}
}

func TestBody_ContainsContextAndGate(t *testing.T) {
	critical := Body(sampleFinding(detect.SeverityCritical))
	for _, want := range []string{"prod", "pod/team/api", "critical", "crashlooping container", NeedsHumanLabel} {
		if !strings.Contains(critical, want) {
			t.Errorf("body missing %q:\n%s", want, critical)
		}
	}
	// Info findings must not advertise the human gate.
	info := Body(sampleFinding(detect.SeverityInfo))
	if strings.Contains(info, "warrants a human decision") {
		t.Errorf("info body should not mention the human gate:\n%s", info)
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
		labels := LabelsFor(sampleFinding(c.sev))
		if !hasLabel(labels, ManagedLabel) {
			t.Errorf("sev %s: missing managed label", c.sev)
		}
		if got := hasLabel(labels, NeedsHumanLabel); got != c.wantHuman {
			t.Errorf("sev %s: needs:human = %v, want %v", c.sev, got, c.wantHuman)
		}
	}
}

func TestRecurrenceAndClosingComments(t *testing.T) {
	rec := RecurrenceComment(sampleFinding(detect.SeverityWarning))
	if !strings.Contains(rec, "Still observed") || !strings.Contains(rec, "crashlooping container") {
		t.Errorf("recurrence comment not well-formed: %q", rec)
	}
	cl := ClosingComment("prod|x|y")
	if !strings.Contains(cl, "cleared") || !strings.Contains(cl, "prod|x|y") {
		t.Errorf("closing comment not well-formed: %q", cl)
	}
}
