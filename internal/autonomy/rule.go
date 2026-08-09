package autonomy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// ErrInvalidRuleset reports a [Ruleset] that will not be honored. Every validation
// failure wraps it, so a caller can branch on the class without matching prose.
//
// The failure is always total: see [Ruleset.Validate] for why one bad rule
// invalidates the whole set rather than being skipped.
var ErrInvalidRuleset = errors.New("autonomy: the ruleset is not valid, so no proposal will be auto-applied")

// Rule is one grant of autonomy: a named operator statement that these operations,
// on these clusters, in these namespaces, up to this reversibility class, may run
// without a human IF the shape has also earned it.
//
// Every selector is required and every selector is an explicit list. There is no
// wildcard spelling, no "all namespaces" value, and no empty-means-everything
// field — an empty list makes the rule invalid, and one invalid rule gates the
// entire ruleset. That is the whole design: the failure modes of a configuration
// language are typos and omissions, and each of them has to land on the safe side
// of a decision that otherwise mutates production with nobody watching.
//
// A rule is necessary and never sufficient. Matching a rule makes a proposal
// ELIGIBLE for autonomy; the trust oracle decides whether it earned it, and the
// blast-radius layer above decides whether it fits in what is left of the budget.
type Rule struct {
	// Name identifies this rule in a verdict, an audit record, and a revocation.
	// It is recorded as the authorizing policy, so it must be unambiguous and stable:
	// unique within the ruleset, and restricted to lowercase alphanumerics and
	// interior hyphens.
	//
	// The charset is not cosmetic. Downstream the name is recorded as
	// "policy:<name>", alongside the blanket bypass's "policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE",
	// in the same field that holds a human's login for a human-approved action.
	// Keeping it to a narrow, predictable shape is what stops a rule name from being
	// read as anything other than a rule name.
	Name string

	// Clusters is the registered cluster names this rule grants autonomy on. It must
	// name at least one, and never grants globally: trust earned on staging says
	// nothing about production, and multi-cluster isolation is a property this
	// system holds everywhere else.
	Clusters []string

	// Namespaces is the namespaces this rule covers. It must name at least one.
	//
	// A consequence worth stating rather than discovering: a cluster-scoped target
	// has no namespace, so it can never match any rule, so a cluster-scoped
	// operation is never auto-applied whatever the configuration says. That is the
	// correct outcome — the namespace list is the blast-radius bound, and an action
	// with cluster-wide effect has none to apply. See [ReasonClusterScopedTarget].
	Namespaces []string

	// Operations is the catalog operations this rule permits. It must name at least
	// one, and every entry must be a real [remediate.Operation].
	//
	// The shipped configuration lists `rolloutrestart` alone. The other three are
	// excluded deliberately and not permanently: `deletepod` destroys the evidence a
	// human would want from a crashed pod, `rollbackrevision` mutates declared
	// intent and can fight a GitOps controller, and `cordonnode` has cluster-wide
	// scheduling effect. An operator who disagrees can list them; what they cannot
	// do is list something outside the catalog.
	Operations []remediate.Operation

	// MaxReversibility is the riskiest reversibility class this rule permits, as a
	// ceiling: a proposal qualifies when its class is this one or safer.
	//
	// The zero value is [remediate.ReversibilityReversible], the strictest setting,
	// so a rule that omits the field permits only fully reversible actions. That
	// alignment is deliberate — of the two ways to be wrong about an unset field,
	// "too strict" costs an operator a config line and "too permissive" costs them a
	// cluster.
	//
	// [remediate.ReversibilityIrreversible] is rejected by validation. It is the one
	// setting no operator may choose, because an irreversible action is refused
	// before rules are even consulted; permitting it to be written would put a
	// setting in a config file that does nothing, which is worse than an error.
	MaxReversibility remediate.Reversibility
}

// Ruleset is the configured autonomy policy: an ordered list of grants.
//
// A nil or empty ruleset is valid and grants nothing — that is the shipped posture
// and what an operator who has never opted in has. Order matters only for which
// rule is NAMED when several would match; the decision itself is the same either
// way, because rules only ever grant.
type Ruleset []Rule

// Validate reports whether this ruleset will be honored, returning an error
// wrapping [ErrInvalidRuleset] if not.
//
// # Why one bad rule invalidates all of them
//
// The alternative — skip the invalid rule, honor the rest — is superficially
// attractive because it is strictly narrower than what the operator wrote, and
// narrower is safe. It is still the wrong choice. An operator who writes four rules
// and typos one gets three rules' worth of autonomy and no signal, and the missing
// fourth shows up as "why didn't it restart that deployment?" weeks later, if at
// all. Gating everything turns the same mistake into an immediate, visible,
// zero-blast-radius symptom on the very first proposal: nothing is automated, and
// the reason token says the ruleset is invalid. A configuration error should cost
// an operator a minute, not an audit.
//
// The checks are ordered so the error names the first problem in file order, which
// is the one an operator will look at first.
func (rs Ruleset) Validate() error {
	seen := make(map[string]bool, len(rs))
	for i, r := range rs {
		if err := r.validate(); err != nil {
			return fmt.Errorf("%w: rule %d: %w", ErrInvalidRuleset, i, err)
		}
		if seen[r.Name] {
			return fmt.Errorf("%w: rule %d: duplicate rule name %q; names identify a rule in the audit trail and in a revocation, so they must be unique", ErrInvalidRuleset, i, r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// validate checks one rule in isolation. It is deliberately strict about empties:
// see the [Rule] doc for why an omitted selector is an error rather than a wildcard.
func (r Rule) validate() error {
	if err := validateName(r.Name); err != nil {
		return err
	}
	if err := validateSelector("clusters", r.Clusters); err != nil {
		return err
	}
	if err := validateSelector("namespaces", r.Namespaces); err != nil {
		return err
	}
	if len(r.Operations) == 0 {
		return errors.New("operations is empty; a rule must name at least one operation, because there is no wildcard and an empty list is a half-written rule rather than a permissive one")
	}
	ops := make(map[remediate.Operation]bool, len(r.Operations))
	for _, op := range r.Operations {
		if !catalogOperation(op) {
			return fmt.Errorf("operation %q is not in the remediation catalog; the catalog is what rollback plans and preconditions are written against, so a rule cannot name anything outside it", op)
		}
		if ops[op] {
			return fmt.Errorf("operation %q is listed twice", op)
		}
		ops[op] = true
	}
	switch {
	case r.MaxReversibility < remediate.ReversibilityReversible:
		return fmt.Errorf("maxReversibility %d is below the defined range", int(r.MaxReversibility))
	// BREAK-VERIFICATION (issue #146, assertion (c)) — DO NOT MERGE: the bound below
	// was >=, which is what refuses ReversibilityIrreversible; relaxing it to > lets
	// an operator configure the one setting no operator may choose.
	// assertInvalidRulesetGrantsNothing must fail the e2e on this branch ("a ruleset
	// permitting irreversible actions validated"); a green run means it lacks teeth.
	case r.MaxReversibility > remediate.ReversibilityIrreversible:
		return fmt.Errorf("maxReversibility %q may not be configured; an irreversible action is refused before any rule is read, so allowing it here would be a setting that does nothing", r.MaxReversibility)
	}
	return nil
}

// validateSelector checks one required list-of-names selector.
func validateSelector(field string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s is empty; every rule must name the %s it covers, because an unset selector is a half-written rule and must not read as a wildcard", field, field)
	}
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		if strings.TrimSpace(v) != v || v == "" {
			return fmt.Errorf("%s contains %q; entries must be exact names with no surrounding whitespace, so a rule cannot fail to match for a reason nobody can see", field, v)
		}
		if seen[v] {
			return fmt.Errorf("%s lists %q twice", field, v)
		}
		seen[v] = true
	}
	return nil
}

// validateName enforces the rule-name charset. The restriction is argued in the
// [Rule.Name] doc; the implementation is hand-rolled rather than a regexp so the
// accepted grammar is readable at the point of enforcement.
func validateName(name string) error {
	if name == "" {
		return errors.New("name is empty; a rule that permits an unattended mutation must be nameable in the record of it")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(name)-1:
		default:
			return fmt.Errorf("name %q is not a valid rule name; use lowercase alphanumerics and interior hyphens only, so the name reads unambiguously where it is recorded as the authorizing policy", name)
		}
	}
	return nil
}

// catalogOperation reports whether an operation is one [remediate] actually
// defines.
//
// It is an explicit switch rather than a package-level set for one reason that
// matters more than the ergonomics: adding a catalog operation to [remediate] does
// NOT silently make it configurable here. A new operation is off-catalog to this
// package — and therefore refused — until somebody adds it to this switch, which
// forces a decision about whether it should ever run unattended at the moment the
// catalog grows rather than after.
func catalogOperation(op remediate.Operation) bool {
	switch op {
	case remediate.OpRolloutRestart, remediate.OpRollbackRevision, remediate.OpDeletePod, remediate.OpCordonNode:
		return true
	default:
		return false
	}
}

// covers reports whether this rule names the given cluster.
func (r Rule) coversCluster(cluster string) bool { return contains(r.Clusters, cluster) }

// coversNamespace reports whether this rule names the given namespace. An empty
// namespace never matches: a cluster-scoped target has no blast-radius bound to
// check against, and the caller reports that as its own reason.
func (r Rule) coversNamespace(ns string) bool { return ns != "" && contains(r.Namespaces, ns) }

// coversOperation reports whether this rule permits the given operation.
func (r Rule) coversOperation(op remediate.Operation) bool {
	for _, candidate := range r.Operations {
		if candidate == op {
			return true
		}
	}
	return false
}

// permitsReversibility reports whether the class is at or below this rule's ceiling.
func (r Rule) permitsReversibility(class remediate.Reversibility) bool {
	return class <= r.MaxReversibility
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
