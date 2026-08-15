package rules

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The file this package parses is the one that grants a machine permission to change a
// production cluster unattended, so the tests are weighted toward what it must REFUSE.
// Every rejection case below is a file an operator could plausibly write and would
// otherwise be left wondering about: a rule that never fires, or worse, one that fires
// wider than they meant.

const minimalFile = `version: 1
rules:
  - name: staging-web-restart
    clusters: [staging]
    namespaces: [web]
    operations: [rolloutrestart]
`

func TestParse_MinimalFile(t *testing.T) {
	rs, err := Parse(strings.NewReader(minimalFile))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := autonomy.Ruleset{{
		Name:       "staging-web-restart",
		Clusters:   []string{"staging"},
		Namespaces: []string{"web"},
		Operations: []remediate.Operation{remediate.OpRolloutRestart},
		// Omitted in the file. The zero value is the strictest class, which is the
		// alignment [autonomy.Rule.MaxReversibility] argues for, and asserting it here is
		// what stops a future default from being loosened silently.
		MaxReversibility: remediate.ReversibilityReversible,
	}}
	if len(rs) != len(want) {
		t.Fatalf("got %d rule(s), want %d: %+v", len(rs), len(want), rs)
	}
	if got := rs[0]; !sameRule(got, want[0]) {
		t.Fatalf("parsed rule\n got %+v\nwant %+v", got, want[0])
	}
}

func TestParse_EveryFieldAndBothReversibilityClasses(t *testing.T) {
	const file = `version: 1
rules:
  - name: staging-broad
    clusters: [staging, staging-eu]
    namespaces: [web, api]
    operations: [rolloutrestart, deletepod]
    maxReversibility: recreated-by-controller
  - name: prod-restart-only
    clusters: [prod-us-east]
    namespaces: [web]
    operations: [rolloutrestart]
    maxReversibility: reversible
`
	rs, err := Parse(strings.NewReader(file))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d rule(s), want 2: %+v", len(rs), rs)
	}
	if rs[0].MaxReversibility != remediate.ReversibilityRecreatedByController {
		t.Fatalf("rule 0 reversibility: got %v, want recreated-by-controller", rs[0].MaxReversibility)
	}
	if rs[1].MaxReversibility != remediate.ReversibilityReversible {
		t.Fatalf("rule 1 reversibility: got %v, want reversible", rs[1].MaxReversibility)
	}
	if len(rs[0].Operations) != 2 || rs[0].Operations[1] != remediate.OpDeletePod {
		t.Fatalf("rule 0 operations: got %v, want [rolloutrestart deletepod]", rs[0].Operations)
	}
	// Order is preserved: it decides which rule is NAMED when several would match, and a
	// verdict that named a different rule than the operator expects is a bug report.
	if rs[0].Name != "staging-broad" || rs[1].Name != "prod-restart-only" {
		t.Fatalf("declaration order was not preserved: %q, %q", rs[0].Name, rs[1].Name)
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name string
		file string
		// want is a fragment the error must contain. Asserting on the message rather than
		// only on non-nil is deliberate: an operator's next action comes from the message,
		// so a rejection that does not say which field is barely better than a silent one.
		want string
	}{{
		name: "empty file",
		file: "",
		want: "the file is empty",
	}, {
		name: "no version",
		file: "rules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n    operations: [rolloutrestart]\n",
		want: "version is missing",
	}, {
		name: "future version",
		file: "version: 2\nrules: []\n",
		want: "version 2 is not supported",
	}, {
		name: "no rules",
		file: "version: 1\nrules: []\n",
		want: "rules is empty",
	}, {
		name: "unknown top-level field",
		file: "version: 1\ntrusted: [staging/rolloutrestart]\nrules: []\n",
		want: "field trusted not found",
	}, {
		// The typo this whole KnownFields(true) decision exists for: singular
		// "namespace" would otherwise parse as a rule with no namespaces at all.
		name: "singular namespace typo",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespace: n\n    operations: [rolloutrestart]\n",
		want: "field namespace not found",
	}, {
		name: "operation outside the catalog",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n    operations: [deletenamespace]\n",
		want: `operation "deletenamespace" is not one MaKlaude can automate`,
	}, {
		name: "empty operation entry",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n    operations: [\"\"]\n",
		want: "operations contains an empty entry",
	}, {
		name: "irreversible ceiling",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n    operations: [rolloutrestart]\n    maxReversibility: irreversible\n",
		want: "may not be configured",
	}, {
		name: "unknown reversibility class",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n    operations: [rolloutrestart]\n    maxReversibility: mostly\n",
		want: "is not a reversibility class",
	}, {
		// The four selector cases below are [autonomy.Ruleset.Validate]'s, asserted from
		// here because a loader that skipped validation would be the failure: an empty
		// selector must never read as a wildcard, and the only proof is that the file is
		// refused.
		name: "no clusters",
		file: "version: 1\nrules:\n  - name: r\n    namespaces: [n]\n    operations: [rolloutrestart]\n",
		want: "clusters is empty",
	}, {
		name: "no namespaces",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    operations: [rolloutrestart]\n",
		want: "namespaces is empty",
	}, {
		name: "no operations",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [c]\n    namespaces: [n]\n",
		want: "operations is empty",
	}, {
		name: "no name",
		file: "version: 1\nrules:\n  - clusters: [c]\n    namespaces: [n]\n    operations: [rolloutrestart]\n",
		want: "name is empty",
	}, {
		name: "upper-case name",
		file: "version: 1\nrules:\n  - name: Staging\n    clusters: [c]\n    namespaces: [n]\n    operations: [rolloutrestart]\n",
		want: "is not a valid rule name",
	}, {
		name: "duplicate rule names",
		file: "version: 1\nrules:\n  - name: r\n    clusters: [a]\n    namespaces: [n]\n    operations: [rolloutrestart]\n  - name: r\n    clusters: [b]\n    namespaces: [n]\n    operations: [rolloutrestart]\n",
		want: "duplicate rule name",
	}, {
		name: "malformed YAML",
		file: "version: 1\nrules: [oh dear\n",
		want: "invalid YAML",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs, err := Parse(strings.NewReader(tc.file))
			if err == nil {
				t.Fatalf("this file must be refused, got ruleset %+v", rs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the problem\n got %v\nwant a message containing %q", err, tc.want)
			}
			if rs != nil {
				t.Fatalf("a refused file must yield no ruleset, got %+v", rs)
			}
		})
	}
}

// TestParse_InvalidRulesetIsRefusedAsSuchPins the error class, so a caller can branch on
// "the operator's grant is not honorable" without matching prose.
func TestParse_InvalidRulesetIsRefusedAsSuch(t *testing.T) {
	_, err := Parse(strings.NewReader("version: 1\nrules:\n  - name: r\n    clusters: []\n    namespaces: [n]\n    operations: [rolloutrestart]\n"))
	if !errors.Is(err, autonomy.ErrInvalidRuleset) {
		t.Fatalf("a semantically invalid ruleset must wrap autonomy.ErrInvalidRuleset, got %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "autonomy.yaml")
	if err := os.WriteFile(path, []byte(minimalFile), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	rs, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs) != 1 || rs[0].Name != "staging-web-restart" {
		t.Fatalf("Load returned %+v", rs)
	}
}

// TestLoad_MissingFileIsAnErrorNotAnEmptyRuleset is the case that would otherwise be
// invisible: a typo'd path granting nothing looks exactly like a working deployment where
// no shape has earned trust yet.
func TestLoad_MissingFileIsAnErrorNotAnEmptyRuleset(t *testing.T) {
	rs, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatalf("a missing rules file must be an error, got ruleset %+v", rs)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("the error must say the file is missing, got %v", err)
	}
}

// TestLoad_NamesTheFile keeps a multi-file deployment diagnosable: the message has to say
// which file was wrong, not just what was wrong with it.
func TestLoad_NamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nrules: []\n"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("the error must name the file %q, got %v", path, err)
	}
}

// TestCatalogMatchesWhatAutonomyWillAccept pins the loader's catalog against the decision
// layer's. They are deliberately separate switches — see [catalog] — but a token this
// loader accepts and [autonomy] then rejects would be a rule that validates on load and
// gates forever, which is the silent-misconfiguration shape both lists exist to avoid.
func TestCatalogMatchesWhatAutonomyWillAccept(t *testing.T) {
	for _, op := range catalog {
		rs := autonomy.Ruleset{{
			Name:       "probe",
			Clusters:   []string{"c"},
			Namespaces: []string{"n"},
			Operations: []remediate.Operation{op},
		}}
		if err := rs.Validate(); err != nil {
			t.Fatalf("this loader accepts operation %q, which autonomy refuses: %v", op, err)
		}
	}
}

func sameRule(a, b autonomy.Rule) bool {
	if a.Name != b.Name || a.MaxReversibility != b.MaxReversibility {
		return false
	}
	if !sameStrings(a.Clusters, b.Clusters) || !sameStrings(a.Namespaces, b.Namespaces) {
		return false
	}
	if len(a.Operations) != len(b.Operations) {
		return false
	}
	for i := range a.Operations {
		if a.Operations[i] != b.Operations[i] {
			return false
		}
	}
	return true
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
