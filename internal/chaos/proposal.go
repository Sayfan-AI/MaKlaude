package chaos

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file is how an experiment asks permission.
//
// [Experiment] is a requested fault and [Injector] is what sends it. Neither of them
// decides whether the fault MAY be sent, and that is deliberate: the whole safety
// argument of Milestone 5 is that one function decides, one layer bounds, and one
// trail records — so a second write path with its own private judgement would be the
// most dangerous thing this milestone could add.
//
// So a chaos experiment becomes a Proposal, and a Proposal answers exactly the
// questions [autonomy.DecideRequest] and [budget.Budget.Admit] ask. It is its OWN type
// rather than a [remediate.Proposal] carrying a chaos-shaped operation, and that is
// the fourth of task T4's done criteria rather than a stylistic preference:
//
//   - A remediation proposal is an ANSWER to a diagnosis. It carries a hypothesis, an
//     incident, a cause, a confidence and the findings that evidence it, and every one
//     of those fields would be empty or invented here. An experiment is not caused by
//     anything MaKlaude observed; it is the cause.
//   - Renderers, ledgers and artifacts branch on types and tokens. A chaos experiment
//     wearing a remediation's struct would eventually be counted, cited or promoted as
//     one by some consumer that reasonably assumed the type meant what it says.
//
// What the two DO share is the vocabulary the policy and the ceiling are written in —
// [remediate.Operation], [remediate.Target], [autonomy.Request] — because sharing
// those is exactly what makes one decision function and one budget possible.

// OperationPrefix namespaces a chaos action where it is recorded as an operation,
// alongside remediation's catalog operations in the same field.
//
// The prefix is what makes the two impossible to confuse in a record, and it is load
// bearing in one direction that is easy to miss: [autonomy.Rule] validation refuses
// any operation outside the remediation catalog, so a prefixed chaos operation can
// never appear in an operator's allowlist. An operator cannot grant chaos autonomy by
// writing a rule, however hard they try — which is a structural bar independent of
// [autonomy.Class.MayAutoApply], and the reason two bars exist is that this is the one
// place in the system where MaKlaude breaks things on purpose.
const OperationPrefix = "chaos:"

// TargetKind is the [remediate.Target] kind every chaos proposal carries.
//
// It is a fixed token rather than the Chaos Mesh kind, and the difference matters for
// the blast-radius ceiling: the budget keys a cooldown on the target's rendered form,
// so the target has to describe the FAULT'S SCOPE — where it lands and how many
// objects it may touch — rather than the custom resource that requests it. Two
// experiments differing only in their CR name are the same blast radius and must share
// a cooldown; see [Proposal.BlastTarget].
const TargetKind = "chaosexperiment"

// Proposal is a requested fault, put to the system that decides whether it may run.
//
// It is a plain value: no clock, no client, no cluster access. Everything it needs to
// be decided, bounded, rendered and recorded is in it, which is what lets the whole
// chaos decision path be tested without Chaos Mesh, without a cluster, and without a
// credential.
type Proposal struct {
	// Cluster is the registered name of the cluster to break. It is here — and not on
	// [Experiment] — because an experiment deliberately cannot name its own target
	// (see the [Experiment] doc), and a PROPOSAL must, since the thing being decided is
	// "may this fault run on THIS cluster". The two are reconciled at injection time:
	// the injector holds a [cluster.ChaosTarget] and the proposal names a cluster, and
	// a disagreement between them is the check [autonomy.DecideRequest] runs first.
	Cluster string

	// Experiment is the fault itself.
	Experiment Experiment

	// Rationale is why breaking this is worth doing, in the words a human will read
	// when they approve or decline it.
	//
	// It is required — see [Proposal.Validate] — because this is the only class of
	// action in the system whose whole purpose is to cause harm, and "MaKlaude wants to
	// kill a pod in payments" with no stated reason is not a reviewable request. A
	// remediation gets its justification for free from the diagnosis that produced it;
	// an experiment has no diagnosis, so somebody has to say it.
	Rationale string

	// ProposedAt is when this proposal was made (UTC). It is carried rather than read
	// from a clock so the value stays pure and a test can pin it.
	ProposedAt time.Time
}

// Operation renders the experiment's action as a namespaced operation token, or the
// empty string for an action outside the catalog.
//
// The empty answer for an unknown action is deliberate: it makes an unvalidated
// proposal fail the decision's cluster-and-class ladder as a malformed request rather
// than travelling onward as "chaos:" plus whatever string a caller had in the field.
func (p Proposal) Operation() remediate.Operation {
	if p.Experiment.Kind() == "" {
		return ""
	}
	return remediate.Operation(OperationPrefix + string(p.Experiment.Action))
}

// Shape is the (cluster, operation) pair this proposal would be recorded under.
//
// A chaos shape can never accumulate trust — nothing promotes it and no rule can name
// its operation — so this exists for the opposite purpose: the blast-radius breaker
// and the trail are both shape-keyed, and an experiment has to be nameable there in
// the same vocabulary as everything else on the cluster.
func (p Proposal) Shape() autonomy.Shape {
	return autonomy.Shape{Cluster: p.Cluster, Operation: p.Operation()}
}

// BlastTarget renders the fault's scope as the target the blast-radius layer bounds.
//
// This is the one projection with a real design choice in it, so the choice is stated
// here. The budget starts a cooldown keyed on the target's rendered form, which means
// the target must answer "have we recently broken THIS" — and there are three
// candidates for what "this" is:
//
//   - The custom resource being created. Rejected: its name is a digest of the whole
//     experiment ([Experiment.ObjectName]), so two experiments that differ by one
//     second of duration are different targets and neither would ever cool down. A
//     cooldown that never bites is not a bound.
//   - The objects the selector matches. Rejected as impossible here: which pods match
//     is a fact about the cluster at injection time, this value is computed without
//     touching one, and a target that changes between the decision and the action
//     cannot key a cooldown at all.
//   - The scope the fault may reach: the namespaces, the label selector, and the size.
//     This is what is used. It is knowable without a cluster, it is stable across
//     replays, and it is the thing an operator means when they say "we already ran
//     chaos there".
//
// Duration is deliberately NOT in it. A 30-second pod-failure and a 5-minute one in
// the same namespace are the same blast radius arriving for different lengths of time,
// and letting them cool down independently would be a way to re-break a namespace
// immediately by changing one field.
func (p Proposal) BlastTarget() remediate.Target {
	return remediate.Target{
		Cluster:   p.Cluster,
		Kind:      TargetKind,
		Namespace: strings.Join(sortedCopy(p.Experiment.Selector.Namespaces), ","),
		Name:      p.scopeName(),
	}
}

// scopeName renders the size and selector half of the blast scope, in a stable order.
func (p Proposal) scopeName() string {
	parts := []string{string(p.Experiment.Action), string(p.Experiment.Mode)}
	if p.Experiment.ModeValue != "" {
		parts = append(parts, p.Experiment.ModeValue)
	}
	keys := make([]string, 0, len(p.Experiment.Selector.LabelSelectors))
	for k := range p.Experiment.Selector.LabelSelectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+p.Experiment.Selector.LabelSelectors[k])
	}
	return strings.Join(parts, ";")
}

// Identity is the stable key for one experiment on one cluster, in the same shape
// remediation proposals use so one audit trail can hold both.
//
// It is derived from the cluster and the experiment's own derived object name, so the
// same experiment proposed twice has one identity — which is the property the audit
// trail's lifecycle lookup and the disclosure's idempotency both need.
func (p Proposal) Identity() remediate.ProposalIdentity {
	return remediate.ProposalIdentity(string(p.Operation()) + ":" + p.Cluster + "/" + p.Experiment.ObjectName())
}

// Request projects this proposal onto the decision function's input, tagged
// [autonomy.ClassChaos].
//
// The class is set here, once, by the type that must never promote — not by the caller
// asking for the decision. A caller that could choose the class could choose
// [autonomy.ClassRemediation] for an injection, and the entire never-promotes property
// would rest on every call site getting one field right.
//
// Reversibility and Fingerprint are left at their zero values on purpose. Neither is
// read for this class — the decision gates before the reversibility ladder and never
// reaches the trust question — and filling them in would invite a reader to believe
// that a fault has a rollback plan or that an experiment can be trusted per-fix.
func (p Proposal) Request() autonomy.Request {
	return autonomy.Request{
		Class:           autonomy.ClassChaos,
		Cluster:         p.Cluster,
		ProposalCluster: p.Cluster,
		Operation:       p.Operation(),
		Target:          p.BlastTarget(),
	}
}

// Validate reports whether this proposal is well-formed, wrapping
// [ErrInvalidExperiment] with every problem found — the experiment's own problems plus
// the two a proposal adds.
//
// It reports all problems at once, for the same reason [Experiment.Validate] does: a
// person composing an experiment should not need one round trip per field.
func (p Proposal) Validate() error {
	var problems []string
	if strings.TrimSpace(p.Cluster) == "" {
		problems = append(problems, "cluster is empty; a proposal must name the cluster it would break, "+
			"because the first thing the decision checks is whether the caller and the proposal agree on it")
	}
	if strings.TrimSpace(p.Rationale) == "" {
		problems = append(problems, "rationale is empty; a deliberate fault is the one action in this system "+
			"with no diagnosis behind it to justify it, so the reason has to be stated for a human to review")
	}
	if err := p.Experiment.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidExperiment, strings.Join(problems, "; "))
}

// Describe renders the proposal as the sentence a human approves or declines.
//
// It states the self-limit explicitly, because that is the fact that makes an
// injection reviewable at all: "MaKlaude will make these pods unavailable, and Chaos
// Mesh will put them back in 2m whether or not MaKlaude is alive" is a different
// request from the same words without the second clause, and a reviewer should not
// have to know the action catalog to tell them apart.
func (p Proposal) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "chaos experiment: %s on cluster %s, affecting %s of the objects matching %s",
		p.Experiment.Action, p.Cluster, p.Experiment.modeDescription(), p.selectorDescription())
	switch p.Experiment.SelfLimit() {
	case SelfLimitServerDuration:
		fmt.Fprintf(&b, ". The fault persists for %s and Chaos Mesh reverts it on expiry, "+
			"whether or not MaKlaude is still running", p.Experiment.Duration)
	case SelfLimitInstant:
		b.WriteString(". The fault is a single event with nothing to revert; the objects' controllers recreate them")
	default:
		b.WriteString(". This action declares no self-limit, which is why it cannot be injected")
	}
	fmt.Fprintf(&b, ". Rationale: %s", strings.TrimSpace(p.Rationale))
	return b.String()
}

// selectorDescription renders the selector for [Proposal.Describe].
func (p Proposal) selectorDescription() string {
	ns := strings.Join(sortedCopy(p.Experiment.Selector.Namespaces), ", ")
	if len(p.Experiment.Selector.LabelSelectors) == 0 {
		return fmt.Sprintf("every pod in namespace(s) %s", ns)
	}
	keys := make([]string, 0, len(p.Experiment.Selector.LabelSelectors))
	for k := range p.Experiment.Selector.LabelSelectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	labels := make([]string, 0, len(keys))
	for _, k := range keys {
		labels = append(labels, k+"="+p.Experiment.Selector.LabelSelectors[k])
	}
	return fmt.Sprintf("%s in namespace(s) %s", strings.Join(labels, ","), ns)
}

// modeDescription renders how many matched objects the experiment affects.
func (e Experiment) modeDescription() string {
	if modeNeedsValue[e.Mode] {
		return string(e.Mode) + " " + e.ModeValue
	}
	return string(e.Mode)
}

// sortedCopy returns a sorted copy, so a rendering never depends on the order a
// caller happened to build a slice in and never mutates the caller's slice.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
