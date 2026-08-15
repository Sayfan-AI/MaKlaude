package cluster

import (
	"errors"
	"fmt"
	"strings"
)

// Chaos eligibility — the door M6 opens, and the lock on it.
//
// Milestone 6 gives MaKlaude the ability to break a cluster on purpose, so that
// its own behaviour under fault can be measured rather than assumed. That
// ability must be reachable on exactly the clusters a human has explicitly
// offered up for it, and on no others.
//
// Two design decisions carry that guarantee, and both are deliberate:
//
//  1. **Eligibility is a type, not a boolean.** There is no
//     `Handle.ChaosEligible() bool` in this package, because a boolean is a
//     thing a future caller forgets to check. Instead, the chaos write path
//     (T2) takes a [ChaosTarget] — a token that only this package can mint, and
//     only for a cluster whose eligibility verified. A caller holding no token
//     cannot construct a chaos client, so "did you check?" is not a question
//     anyone has to remember to ask.
//
//  2. **The marker names the cluster it applies to, twice.** The realistic
//     accident is not a typo, it is a copy-paste: an operator marks a scratch
//     cluster eligible, then copies that config block when adding a production
//     cluster. A bare `chaos: true` survives that copy silently. An
//     acknowledgement that spells out the cluster's name does not — pasted
//     under a different cluster it names the wrong one, and eligibility fails
//     closed with an error that says exactly that.
//
// Absence is the default: a config with no `chaos` block is ineligible, which
// is every config written before this milestone.

// ErrChaosIneligible is the sentinel returned by every path that could hand out
// a chaos capability and declines to. Use errors.Is to detect it.
//
// Every failure mode wraps this one sentinel — unknown cluster, absent marker,
// malformed marker, marker naming a different cluster — because they all mean
// the same thing to a caller: no chaos here. There is deliberately no way to
// distinguish "ineligible" from "not a cluster I know about" by error type; the
// safe read is identical.
var ErrChaosIneligible = errors.New("cluster is not chaos-eligible")

// ChaosEligibility is a human's explicit, per-cluster acknowledgement that
// MaKlaude may deliberately break that cluster.
//
// Both fields are required and both must name the cluster the block is declared
// under: Cluster repeats the name, and Acknowledgement is the exact sentence
// returned by [ChaosAcknowledgementFor] for that name. Neither field carries
// information the rest of the config lacks — that redundancy *is* the
// mechanism. A block copied from one cluster to another names the cluster it
// was written for, so it grants nothing where it was pasted.
//
// Example YAML:
//
//	clusters:
//	  - name: kind-maklaude
//	    kubeconfig: ~/.kube/config
//	    context: kind-maklaude
//	    chaos:
//	      cluster: kind-maklaude
//	      acknowledgement: >-
//	        I accept that MaKlaude may deliberately break the cluster named
//	        kind-maklaude
type ChaosEligibility struct {
	// Cluster must equal the name of the cluster this block is declared under.
	Cluster string `yaml:"cluster"`
	// Acknowledgement must equal [ChaosAcknowledgementFor] for that name.
	// Surrounding and internal whitespace is normalised before comparison, so a
	// YAML folded scalar (`>-`) may wrap the sentence across lines; wording and
	// letter case are compared exactly.
	Acknowledgement string `yaml:"acknowledgement"`
}

// ChaosAcknowledgementFor returns the exact sentence a human must write to make
// the named cluster chaos-eligible. It is exported so the error messages, the
// docs, and the tests all quote one string rather than three copies of it.
func ChaosAcknowledgementFor(cluster string) string {
	return fmt.Sprintf("I accept that MaKlaude may deliberately break the cluster named %s", strings.TrimSpace(cluster))
}

// problems reports every reason e fails to make the cluster named name
// eligible. An empty result means the block verifies.
//
// A nil receiver returns one problem rather than none: callers that mean
// "absent, therefore ineligible" must not route through here, because a
// verifier that answers "no problems" for a missing marker is one refactor away
// from granting eligibility to every cluster in the config.
func (e *ChaosEligibility) problems(name string) []string {
	if e == nil {
		return []string{"chaos: no eligibility block"}
	}

	name = strings.TrimSpace(name)
	if name == "" {
		return []string{"chaos: eligibility cannot be verified for a cluster with no name"}
	}

	var problems []string

	declared := strings.TrimSpace(e.Cluster)
	switch {
	case declared == "":
		problems = append(problems, "chaos: missing required field 'cluster' (it must repeat this cluster's name)")
	case declared != name:
		problems = append(problems, fmt.Sprintf(
			"chaos: eligibility names cluster %q but is declared under cluster %q "+
				"(an eligibility block does not carry over when copied; write %q here or remove the block)",
			declared, name, name))
	}

	want := ChaosAcknowledgementFor(name)
	got := normaliseAcknowledgement(e.Acknowledgement)
	switch {
	case got == "":
		problems = append(problems, fmt.Sprintf("chaos: missing required field 'acknowledgement' (it must read exactly: %q)", want))
	case got != want:
		problems = append(problems, fmt.Sprintf("chaos: acknowledgement reads %q but must read exactly: %q", got, want))
	}

	return problems
}

// normaliseAcknowledgement collapses runs of whitespace to single spaces and
// trims the ends, so a sentence wrapped by a YAML folded scalar compares equal
// to the one-line form. Wording and case are left alone: the point of the
// sentence is that a human typed it deliberately.
func normaliseAcknowledgement(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// resolveChaosEligibility returns the verified acknowledgement for a cluster, or
// the empty string if the cluster is not eligible.
//
// This runs at handle-resolution time and re-verifies from scratch, even though
// [Config.Validate] has already rejected a malformed block. That is not
// redundant work: it means eligibility is granted because the marker *matches*,
// never because validation happened to run first. Reordering or bypassing
// validation can therefore make a config load fail loudly, but it cannot make
// an ineligible cluster eligible.
func resolveChaosEligibility(name string, e *ChaosEligibility) string {
	if len(e.problems(name)) > 0 {
		return ""
	}
	return ChaosAcknowledgementFor(name)
}

// ChaosTarget is proof, carried in the type system, that a specific cluster was
// explicitly marked chaos-eligible by a human.
//
// It is the argument the chaos write path takes instead of a cluster name or a
// [Handle], so the compiler — not a caller's diligence — is what stops an
// experiment from reaching an ineligible cluster. Obtain one from
// [Handle.ChaosTarget] or [Registry.ChaosTarget]; both return
// [ErrChaosIneligible] rather than a token when eligibility does not verify.
//
// The interface has an unexported method on purpose. No type outside this
// package can satisfy ChaosTarget, so a token cannot be forged, subclassed, or
// stubbed into existence by a caller who would rather not deal with the error —
// the only way to hold one is for a human to have written the acknowledgement.
type ChaosTarget interface {
	// Handle returns the underlying cluster handle.
	Handle() *Handle
	// Acknowledgement returns the verified acknowledgement sentence, so an
	// audit record can quote the human's own words rather than assert consent.
	Acknowledgement() string
	// chaosEligible is unexported and seals the interface. Do not remove it:
	// without it, eligibility becomes forgeable from any package.
	chaosEligible()
}

// chaosTarget is the only implementation of [ChaosTarget].
type chaosTarget struct {
	handle          *Handle
	acknowledgement string
}

func (t chaosTarget) Handle() *Handle         { return t.handle }
func (t chaosTarget) Acknowledgement() string { return t.acknowledgement }
func (t chaosTarget) chaosEligible()          {}

// ChaosTarget returns a chaos capability token for this cluster, or an error
// wrapping [ErrChaosIneligible] if the cluster carries no verified eligibility.
func (h *Handle) ChaosTarget() (ChaosTarget, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: nil cluster handle", ErrChaosIneligible)
	}
	if h.chaosAcknowledgement == "" {
		return nil, fmt.Errorf(
			"%w: cluster %q has no verified 'chaos' eligibility block in the config "+
				"(to make it eligible, add one whose 'cluster' is %q and whose 'acknowledgement' reads exactly: %q)",
			ErrChaosIneligible, h.name, h.name, ChaosAcknowledgementFor(h.name))
	}
	return chaosTarget{handle: h, acknowledgement: h.chaosAcknowledgement}, nil
}

// ChaosTarget returns a chaos capability token for the named cluster.
//
// An unknown name and an ineligible cluster both wrap [ErrChaosIneligible]:
// they differ in message only, because they do not differ in consequence.
func (r *Registry) ChaosTarget(name string) (ChaosTarget, error) {
	h, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: no cluster named %q is registered", ErrChaosIneligible, name)
	}
	return h.ChaosTarget()
}

// ChaosTargets returns a token for every chaos-eligible cluster, in declaration
// order. It is empty for every config that does not opt in, which is the
// default and every config written before Milestone 6.
//
// The chaos reaper (T3) uses this to find the clusters it is allowed to sweep
// for orphaned experiments, and a startup summary uses it to say out loud which
// clusters are eligible.
func (r *Registry) ChaosTargets() []ChaosTarget {
	var out []ChaosTarget
	for _, h := range r.Handles() {
		t, err := h.ChaosTarget()
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}
