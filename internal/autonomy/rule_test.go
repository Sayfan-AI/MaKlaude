package autonomy

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// validRule is the smallest rule that validates. Each rejection case below takes
// this and breaks exactly one field, so a failing test names one cause.
func validRule() Rule {
	return Rule{
		Name:       "restart-payments",
		Clusters:   []string{"prod"},
		Namespaces: []string{"payments"},
		Operations: []remediate.Operation{remediate.OpRolloutRestart},
	}
}

// TestRuleset_ValidAndEmpty pins the two shapes that must pass. The empty ruleset
// matters as much as the populated one: it is the shipped posture, and a validator
// that rejected it would make "autonomy off" an error state.
func TestRuleset_ValidAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules Ruleset
	}{
		{"nil", nil},
		{"empty", Ruleset{}},
		{"one rule", Ruleset{validRule()}},
		{"two distinctly named rules", Ruleset{validRule(), func() Rule { r := validRule(); r.Name = "restart-web"; return r }()}},
		{"every catalog operation", func() Ruleset {
			r := validRule()
			r.Operations = []remediate.Operation{remediate.OpRolloutRestart, remediate.OpRollbackRevision, remediate.OpDeletePod, remediate.OpCordonNode}
			r.MaxReversibility = remediate.ReversibilityRecreatedByController
			return Ruleset{r}
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.rules.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestRuleset_Rejects walks every way a rule fails to validate. The empty-selector
// cases are the ones that matter most: each is a half-written rule that a
// permissive validator would read as a wildcard, which is the single mistake that
// turns a narrow grant into a broad one without anybody typing anything broad.
func TestRuleset_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Rule)
		wantMsg string
	}{
		{"no name", func(r *Rule) { r.Name = "" }, "name is empty"},
		{"uppercase name", func(r *Rule) { r.Name = "Restart" }, "not a valid rule name"},
		{"name with a space", func(r *Rule) { r.Name = "restart payments" }, "not a valid rule name"},
		{"name with a colon", func(r *Rule) { r.Name = "policy:restart" }, "not a valid rule name"},
		{"name with a leading hyphen", func(r *Rule) { r.Name = "-restart" }, "not a valid rule name"},
		{"name with a trailing hyphen", func(r *Rule) { r.Name = "restart-" }, "not a valid rule name"},

		{"no clusters", func(r *Rule) { r.Clusters = nil }, "clusters is empty"},
		{"empty cluster entry", func(r *Rule) { r.Clusters = []string{""} }, "clusters contains"},
		{"padded cluster entry", func(r *Rule) { r.Clusters = []string{" prod"} }, "clusters contains"},
		{"duplicate cluster", func(r *Rule) { r.Clusters = []string{"prod", "prod"} }, `clusters lists "prod" twice`},

		{"no namespaces", func(r *Rule) { r.Namespaces = nil }, "namespaces is empty"},
		{"empty namespace entry", func(r *Rule) { r.Namespaces = []string{"payments", ""} }, "namespaces contains"},
		{"duplicate namespace", func(r *Rule) { r.Namespaces = []string{"payments", "payments"} }, "twice"},

		{"no operations", func(r *Rule) { r.Operations = nil }, "operations is empty"},
		{"off-catalog operation", func(r *Rule) {
			r.Operations = []remediate.Operation{remediate.Operation("deletenamespace")}
		}, "not in the remediation catalog"},
		{"empty operation", func(r *Rule) { r.Operations = []remediate.Operation{""} }, "not in the remediation catalog"},
		{"duplicate operation", func(r *Rule) {
			r.Operations = []remediate.Operation{remediate.OpRolloutRestart, remediate.OpRolloutRestart}
		}, "listed twice"},

		{"irreversible ceiling", func(r *Rule) { r.MaxReversibility = remediate.ReversibilityIrreversible }, "may not be configured"},
		{"ceiling above the range", func(r *Rule) { r.MaxReversibility = remediate.ReversibilityIrreversible + 3 }, "may not be configured"},
		{"ceiling below the range", func(r *Rule) { r.MaxReversibility = -1 }, "below the defined range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := validRule()
			tc.mutate(&r)

			err := Ruleset{r}.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error for %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidRuleset) {
				t.Errorf("error does not wrap ErrInvalidRuleset: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestRuleset_RejectsDuplicateNames covers the one property that is about the set
// rather than a member. Names identify a rule in the audit trail and in a
// revocation, so two rules sharing one would make a revocation ambiguous at exactly
// the moment an operator needs it to be precise.
func TestRuleset_RejectsDuplicateNames(t *testing.T) {
	first := validRule()
	second := validRule()
	second.Namespaces = []string{"web"}

	err := Ruleset{first, second}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error for two rules sharing a name")
	}
	if !errors.Is(err, ErrInvalidRuleset) || !strings.Contains(err.Error(), "duplicate rule name") {
		t.Errorf("error = %v, want a wrapped duplicate-name error", err)
	}
}

// TestRuleset_ErrorNamesTheFirstBadRule keeps the diagnostic useful: an operator
// reading the message should be pointed at the rule they need to open, and the
// index is the only stable handle a nameless or duplicated rule has.
func TestRuleset_ErrorNamesTheFirstBadRule(t *testing.T) {
	good := validRule()
	bad := validRule()
	bad.Name, bad.Clusters = "broken", nil

	err := Ruleset{good, bad}.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "rule 1") {
		t.Errorf("error = %q, want it to name the index of the offending rule", err)
	}
}

// TestCatalogOperation_IsAnExplicitSwitch guards the property the switch exists
// for: this package's notion of the catalog is written down here, so a new
// operation added to remediate is off-catalog — and therefore refused — until
// somebody decides whether it may ever run unattended.
//
// If remediate gains an operation, this test fails and the failure is the prompt to
// make that decision. Do not fix it by widening the switch reflexively.
func TestCatalogOperation_IsAnExplicitSwitch(t *testing.T) {
	known := []remediate.Operation{
		remediate.OpRolloutRestart, remediate.OpRollbackRevision,
		remediate.OpDeletePod, remediate.OpCordonNode,
	}
	for _, op := range known {
		if !catalogOperation(op) {
			t.Errorf("catalogOperation(%q) = false, want true", op)
		}
	}
	for _, op := range []remediate.Operation{"", " ", "rolloutrestart ", "RolloutRestart", "drainnode", "scalezero"} {
		if catalogOperation(op) {
			t.Errorf("catalogOperation(%q) = true, want false", op)
		}
	}
}

// TestStaticTrust reports the oracle's three answers, including that a nil map is a
// usable oracle that trusts nothing. A ledger that has not been populated yet must
// not be a nil-pointer panic in the middle of a remediation cycle.
func TestStaticTrust(t *testing.T) {
	subject := Subject{
		Shape:       Shape{Cluster: "prod", Operation: remediate.OpRolloutRestart},
		Fingerprint: "fp1:abc",
	}

	if got := StaticTrust(nil).Trust(subject); got.Trusted || got.Citation != "" {
		t.Errorf("nil StaticTrust returned %#v, want the untrusted zero value", got)
	}
	if got := (StaticTrust{}).Trust(subject); got.Trusted {
		t.Errorf("empty StaticTrust trusted %s", subject)
	}
	got := StaticTrust{subject: "3 converged"}.Trust(subject)
	if !got.Trusted || got.Citation != "3 converged" {
		t.Errorf("Trust(%s) = %#v, want trusted with the citation carried through", subject, got)
	}

	// The fingerprint is part of the key, not decoration on it. A seeded subject must
	// not answer for the same shape carrying a different fix — that is the whole of
	// issue #167 expressed at the smallest oracle there is.
	other := subject
	other.Fingerprint = "fp1:def"
	if got := (StaticTrust{subject: "3 converged"}).Trust(other); got.Trusted {
		t.Errorf("Trust(%s) = %#v on an oracle seeded only for %s; trust must not carry across fingerprints",
			other, got, subject)
	}
}

// TestShape_String pins the rendering, which ends up in rule names, log lines and
// test failures.
func TestShape_String(t *testing.T) {
	got := Shape{Cluster: "prod", Operation: remediate.OpRolloutRestart}.String()
	if got != "prod/rolloutrestart" {
		t.Errorf("String() = %q, want %q", got, "prod/rolloutrestart")
	}
}
