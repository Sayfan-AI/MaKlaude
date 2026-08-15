// Package rules is the configuration surface for earned autonomy: it turns a file an
// operator writes into the [autonomy.Ruleset] the decision layer consumes.
//
// It exists as its own package because [autonomy] deliberately refuses to be one.
// That package is a pure decision function with no filesystem, no environment and no
// clock, and its doc places rule loading "with the documentation describing it" —
// this is that half. The split also means the format can change without touching the
// decision, and the decision can be unit-tested without a file.
//
// # The file is the most consequential thing an operator writes here
//
// Every other configuration surface in MaKlaude describes what to look at. This one
// grants a machine permission to change a production cluster with nobody watching, so
// it is read strictly:
//
//   - UNKNOWN FIELDS ARE ERRORS. A rule whose `namespace:` was meant to be
//     `namespaces:` would otherwise validate as a rule with no namespaces, and the
//     operator would be left wondering why their rule never fired.
//   - EVERY SELECTOR IS REQUIRED and there is no wildcard spelling. That is
//     [autonomy.Rule]'s design, enforced here by handing the parsed ruleset to
//     [autonomy.Ruleset.Validate] before returning it.
//   - AN UNREADABLE OR INVALID FILE IS AN ERROR, not an empty ruleset. An empty
//     ruleset grants nothing, so the mistake would be perfectly safe and completely
//     silent — indistinguishable from a deployment that never enabled autonomy. The
//     caller ([operate.New]) turns this into a refusal to start.
//
// Note that a refusal to start is a SECOND guard rather than a replacement for
// [autonomy.ReasonRulesetInvalid]. That reason still exists and still gates everything
// if an invalid ruleset ever reaches [autonomy.Decide] by another route; this package
// is what stops the production path being that route.
//
// The environment variable that points at the file is NOT declared here — it is
// [operate.AutonomyRulesEnv], because assembling a cycle from the environment is that
// package's job and splitting the two halves of one variable across two packages is
// how they drift.
//
// # What this file cannot do
//
// It cannot grant trust. There is no `trusted:` key, no seed and no bootstrap list,
// because trust is derived from a recorded history of human-approved executions and
// never declared — see [trust]. A rule makes a shape ELIGIBLE; the ledger decides
// whether it earned it. It also cannot widen the blast-radius bounds: the per-pass
// cap, the cooldown and the breaker threshold are [budget.DefaultLimits] and are not
// configurable, so a rules file cannot raise the ceiling it operates under.
package rules

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Version is the only format version this build understands.
//
// It is REQUIRED in the file rather than defaulted, for the reason every other
// omitted field in this milestone is an error: a version that defaults is a version
// nobody has stated, and the first time the format changes there would be no way to
// tell a file written for the old shape from one written for the new.
const Version = 1

// File is the on-disk shape of the autonomy rules file.
//
// Example:
//
//	version: 1
//	rules:
//	  - name: staging-web-restart
//	    clusters: [staging]
//	    namespaces: [web, api]
//	    operations: [rolloutrestart]
//	    maxReversibility: reversible
type File struct {
	// Version must equal [Version]. See there for why it is required.
	Version int `yaml:"version"`

	// Rules is the list of grants. It must name at least one — see [Parse].
	Rules []Rule `yaml:"rules"`
}

// Rule is one grant of autonomy as written in the file. It mirrors [autonomy.Rule]
// with the enums carried as their stable string tokens, so an operator writes
// `rolloutrestart` and `reversible` rather than integers whose meaning depends on
// declaration order.
type Rule struct {
	// Name identifies this rule wherever an unattended action is recorded. Lowercase
	// alphanumerics and interior hyphens only; see [autonomy.Rule.Name].
	Name string `yaml:"name"`

	// Clusters names the registered clusters this rule grants autonomy on. Required.
	Clusters []string `yaml:"clusters"`

	// Namespaces names the namespaces it covers. Required — and note the consequence
	// stated in [autonomy.Rule.Namespaces]: a cluster-scoped target has no namespace,
	// so no rule can ever cover one.
	Namespaces []string `yaml:"namespaces"`

	// Operations names the catalog operations it permits, as their catalog tokens:
	// rolloutrestart, rollbackrevision, deletepod, cordonnode. Required.
	Operations []string `yaml:"operations"`

	// MaxReversibility is the riskiest reversibility class this rule permits, as a
	// ceiling: "reversible" (the default when omitted, and the strictest) or
	// "recreated-by-controller". "irreversible" is rejected — see
	// [parseReversibility].
	MaxReversibility string `yaml:"maxReversibility"`
}

// Load reads and validates the rules file at path.
//
// A missing file is an error rather than an empty ruleset. An operator who pointed
// MaKlaude at a path meant for it to be read, and a typo'd path that silently granted
// nothing would present exactly as a working deployment where no shape had earned
// trust yet.
func Load(path string) (autonomy.Ruleset, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied config, not user input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("autonomy rules: file %q does not exist", path)
		}
		return nil, fmt.Errorf("autonomy rules: cannot open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	rs, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("autonomy rules %q: %w", path, err)
	}
	return rs, nil
}

// Parse decodes a ruleset from r, converts the tokens to their enums, and validates
// the result.
//
// It rejects unknown fields, and it returns a ruleset only when
// [autonomy.Ruleset.Validate] accepts it — so no caller can act on a half-written
// grant. See the package doc for why each of those is an error rather than a
// narrowing.
func Parse(r io.Reader) (autonomy.Ruleset, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}

	switch {
	case f.Version == 0:
		return nil, fmt.Errorf("version is missing; this build reads version %d, and a rules file that does not state its format version is one nobody can tell apart from a file written for a different one", Version)
	case f.Version != Version:
		return nil, fmt.Errorf("version %d is not supported; this build reads version %d", f.Version, Version)
	case len(f.Rules) == 0:
		return nil, fmt.Errorf("rules is empty; a rules file that grants nothing is a half-written configuration rather than a posture — leave the rules variable unset to run fully gated")
	}

	rs := make(autonomy.Ruleset, 0, len(f.Rules))
	for i, r := range f.Rules {
		converted, err := r.convert()
		if err != nil {
			return nil, fmt.Errorf("rule %d (%s): %w", i, describeName(r.Name), err)
		}
		rs = append(rs, converted)
	}

	// The decision layer's own validation, run here so an invalid grant never leaves
	// this package. Its message already explains each failure in the operator's terms.
	if err := rs.Validate(); err != nil {
		return nil, err
	}
	return rs, nil
}

// convert turns one file rule into the decision layer's form. It resolves the two
// token fields and copies the rest verbatim; the selectors are validated by
// [autonomy.Ruleset.Validate] rather than duplicated here, so there is exactly one
// definition of what a well-formed rule is.
func (r Rule) convert() (autonomy.Rule, error) {
	ops := make([]remediate.Operation, 0, len(r.Operations))
	for _, token := range r.Operations {
		op, err := parseOperation(token)
		if err != nil {
			return autonomy.Rule{}, err
		}
		ops = append(ops, op)
	}
	rev, err := parseReversibility(r.MaxReversibility)
	if err != nil {
		return autonomy.Rule{}, err
	}
	return autonomy.Rule{
		Name:             r.Name,
		Clusters:         r.Clusters,
		Namespaces:       r.Namespaces,
		Operations:       ops,
		MaxReversibility: rev,
	}, nil
}

// catalog is every operation a rule may name, in the order they are listed back to an
// operator who named something else.
//
// It is written out rather than derived from [remediate], for the reason
// [autonomy]'s own catalog switch is: adding an operation to the catalog must not
// silently make it configurable for unattended use. A new operation is unknown to
// this loader until somebody adds it here, which forces the question "should this ever
// run without a human?" at the moment the catalog grows.
var catalog = []remediate.Operation{
	remediate.OpRolloutRestart,
	remediate.OpRollbackRevision,
	remediate.OpDeletePod,
	remediate.OpCordonNode,
}

// parseOperation resolves a catalog operation token.
func parseOperation(token string) (remediate.Operation, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("operations contains an empty entry")
	}
	for _, op := range catalog {
		if remediate.Operation(token) == op {
			return op, nil
		}
	}
	names := make([]string, 0, len(catalog))
	for _, op := range catalog {
		names = append(names, string(op))
	}
	return "", fmt.Errorf("operation %q is not one MaKlaude can automate; the catalog is %s", token, strings.Join(names, ", "))
}

// parseReversibility resolves the reversibility ceiling token.
//
// An omitted value is [remediate.ReversibilityReversible], which is both the zero
// value and the strictest setting — see [autonomy.Rule.MaxReversibility] for why the
// two are aligned. "irreversible" is refused with its own message rather than falling
// into the unrecognized-token case, because an operator who wrote it has a specific
// misunderstanding worth correcting: an irreversible action is refused before any rule
// is consulted, so permitting it here would be a setting that does nothing.
func parseReversibility(token string) (remediate.Reversibility, error) {
	switch strings.TrimSpace(token) {
	case "":
		return remediate.ReversibilityReversible, nil
	case remediate.ReversibilityReversible.String():
		return remediate.ReversibilityReversible, nil
	case remediate.ReversibilityRecreatedByController.String():
		return remediate.ReversibilityRecreatedByController, nil
	case remediate.ReversibilityIrreversible.String():
		return 0, fmt.Errorf("maxReversibility %q may not be configured; an irreversible action is refused before any rule is read, so allowing it here would be a setting that does nothing", token)
	default:
		return 0, fmt.Errorf("maxReversibility %q is not a reversibility class; use %q or %q",
			token, remediate.ReversibilityReversible, remediate.ReversibilityRecreatedByController)
	}
}

// describeName renders a rule's name for an error message, naming the position when
// the rule has no name to name.
func describeName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "unnamed"
	}
	return name
}
