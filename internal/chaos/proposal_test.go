package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const proposalCluster = "prod"

var proposedAt = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// killProposal is a well-formed proposal for the fixture pod-kill experiment.
func killProposal() Proposal {
	return Proposal{
		Cluster:    proposalCluster,
		Experiment: podKill(),
		Rationale:  "verify the crashloop detector fires while a remediation is in flight",
		ProposedAt: proposedAt,
	}
}

// TestProposal_OperationIsNamespacedAndOffCatalog is the property the whole
// never-promotes argument leans on twice: the token is prefixed, so it is unmistakable in
// a record, and it is outside the remediation catalog, so no operator's allowlist can
// name it.
func TestProposal_OperationIsNamespacedAndOffCatalog(t *testing.T) {
	op := killProposal().Operation()

	if !strings.HasPrefix(string(op), OperationPrefix) {
		t.Errorf("Operation() = %q, want the %q prefix that keeps it out of the remediation catalog", op, OperationPrefix)
	}
	for _, catalog := range []remediate.Operation{
		remediate.OpRolloutRestart, remediate.OpRollbackRevision, remediate.OpDeletePod, remediate.OpCordonNode,
	} {
		if op == catalog {
			t.Fatalf("a chaos operation rendered as the catalog operation %q", catalog)
		}
	}

	// The same guard from the other side: a rule naming it does not validate, so a
	// configuration file cannot grant autonomy for a fault.
	rs := autonomy.Ruleset{{
		Name:             "break-demo",
		Clusters:         []string{proposalCluster},
		Namespaces:       []string{"demo"},
		Operations:       []remediate.Operation{op},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
	if err := rs.Validate(); err == nil {
		t.Error("an autonomy rule naming a chaos operation must not validate")
	}
}

// TestProposal_OperationIsEmptyForAnUnknownAction keeps a malformed proposal from
// travelling onward as "chaos:" plus whatever string a caller had in the field.
func TestProposal_OperationIsEmptyForAnUnknownAction(t *testing.T) {
	p := killProposal()
	p.Experiment.Action = "pod-obliterate"
	if got := p.Operation(); got != "" {
		t.Errorf("Operation() = %q for an action outside the catalog, want empty", got)
	}
}

// TestProposal_RequestIsAlwaysChaos pins the class at its source. A caller cannot choose
// it, which is what keeps the never-promotes property off the list of things every call
// site has to get right.
func TestProposal_RequestIsAlwaysChaos(t *testing.T) {
	req := killProposal().Request()

	if req.Class != autonomy.ClassChaos {
		t.Errorf("Request().Class = %s, want %s", req.Class, autonomy.ClassChaos)
	}
	if req.Class.MayAutoApply() {
		t.Error("the class a chaos proposal projects to must never be eligible for autonomy")
	}
	if req.Cluster != proposalCluster || req.ProposalCluster != proposalCluster || req.Target.Cluster != proposalCluster {
		t.Errorf("all three clusters must be the proposal's: %+v", req)
	}
}

// TestProposal_GatesThroughTheRealDecisionFunction is the end-to-end of T4's first two
// done criteria at this layer: the projection reaches [autonomy.DecideRequest], and what
// comes back is a gate — with a ruleset and an oracle as permissive as this package can
// construct.
func TestProposal_GatesThroughTheRealDecisionFunction(t *testing.T) {
	rs := autonomy.Ruleset{{
		Name:             "restart-demo",
		Clusters:         []string{proposalCluster},
		Namespaces:       []string{"demo"},
		Operations:       []remediate.Operation{remediate.OpRolloutRestart},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
	v := autonomy.DecideRequest(killProposal().Request(), rs, trustEverything{})

	if v.AutoApplies() {
		t.Fatalf("an experiment was ruled auto-appliable: %s", v)
	}
	if v.Decision != autonomy.DecisionGate {
		t.Errorf("Decision = %s, want gate: an experiment is put to a human, not refused outright", v.Decision)
	}
	if v.Reason != autonomy.ReasonChaosNeverPromotes {
		t.Errorf("Reason = %s, want chaos-never-promotes", v.Reason)
	}
}

// TestProposal_BlastTargetDescribesTheFaultNotTheObject is the design choice
// [Proposal.BlastTarget] documents, executed. The cooldown the budget keys on this target
// only bounds anything if two experiments with the same reach render the same, and
// different reaches render differently.
func TestProposal_BlastTargetDescribesTheFaultNotTheObject(t *testing.T) {
	base := killProposal()

	longer := base
	longer.Experiment = podFailure()
	longerAgain := longer
	longerAgain.Experiment.Duration = 5 * time.Minute

	if longer.Experiment.ObjectName() == longerAgain.Experiment.ObjectName() {
		t.Fatal("precondition failed: two durations must produce different CR names, or this test proves nothing")
	}
	if longer.BlastTarget() != longerAgain.BlastTarget() {
		t.Error("two experiments differing only in duration have the same reach and must share a cooldown; " +
			"keying on the CR would give each one its own and the cooldown would never bite")
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Proposal)
	}{
		{"a different namespace", func(p *Proposal) { p.Experiment.Selector.Namespaces = []string{"other"} }},
		{"a different label selector", func(p *Proposal) { p.Experiment.Selector.LabelSelectors = map[string]string{"app": "api"} }},
		{"a different size", func(p *Proposal) { p.Experiment.Mode, p.Experiment.ModeValue = ModeFixed, "3" }},
		{"a different action", func(p *Proposal) { p.Experiment = podFailure() }},
		{"a different cluster", func(p *Proposal) { p.Cluster = "staging" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := killProposal()
			tc.mutate(&other)
			if other.BlastTarget() == base.BlastTarget() {
				t.Errorf("%s must be a different blast target, both rendered %s", tc.name, base.BlastTarget())
			}
		})
	}
}

// TestProposal_BlastTargetIsStableAcrossSelectorOrder: a target that depended on the
// order a caller happened to build a slice or a map in would give one experiment two
// cooldowns.
func TestProposal_BlastTargetIsStableAcrossSelectorOrder(t *testing.T) {
	a, b := killProposal(), killProposal()
	a.Experiment.Selector.Namespaces = []string{"demo", "payments"}
	b.Experiment.Selector.Namespaces = []string{"payments", "demo"}
	a.Experiment.Selector.LabelSelectors = map[string]string{"app": "web", "tier": "front"}
	b.Experiment.Selector.LabelSelectors = map[string]string{"tier": "front", "app": "web"}

	if a.BlastTarget() != b.BlastTarget() {
		t.Errorf("the same scope rendered two ways:\n  %s\n  %s", a.BlastTarget(), b.BlastTarget())
	}
	// And the caller's slice is not reordered underneath them.
	if a.Experiment.Selector.Namespaces[0] != "demo" {
		t.Error("rendering the target mutated the caller's selector")
	}
}

// TestProposal_IdentityIsStableAndDistinct: the identity is what an audit lifecycle and a
// disclosure are looked up by, so the same experiment proposed twice must be one identity
// and two different experiments must never collide.
func TestProposal_IdentityIsStableAndDistinct(t *testing.T) {
	first, second := killProposal(), killProposal()
	if first.Identity() != second.Identity() {
		t.Error("the same experiment must have one identity across two proposals of it")
	}
	elsewhere := killProposal()
	elsewhere.Cluster = "staging"
	if elsewhere.Identity() == first.Identity() {
		t.Error("the same experiment on two clusters must be two identities; multi-cluster isolation is not optional")
	}
	other := killProposal()
	other.Experiment = podFailure()
	if other.Identity() == first.Identity() {
		t.Error("two different experiments must not share an identity")
	}
	if !strings.HasPrefix(string(first.Identity()), OperationPrefix) {
		t.Errorf("an identity must be recognizable as chaos on sight, got %q", first.Identity())
	}
}

// TestProposal_ValidateReportsEveryProblemAtOnce covers the two problems a proposal adds
// on top of the experiment's own, and that they are reported together rather than one per
// round trip.
func TestProposal_ValidateReportsEveryProblemAtOnce(t *testing.T) {
	if err := killProposal().Validate(); err != nil {
		t.Fatalf("the fixture proposal must be valid: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Proposal)
		want   string
	}{
		{"no cluster", func(p *Proposal) { p.Cluster = " " }, "cluster is empty"},
		{"no rationale", func(p *Proposal) { p.Rationale = "" }, "rationale is empty"},
		{"a duration on a one-shot action", func(p *Proposal) { p.Experiment.Duration = time.Minute }, "duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := killProposal()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must mention %q", err, tc.want)
			}
		})
	}

	broken := killProposal()
	broken.Cluster, broken.Rationale = "", ""
	err := broken.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "cluster is empty") || !strings.Contains(err.Error(), "rationale is empty") {
		t.Errorf("both problems must be reported at once, got %q", err)
	}
}

// TestProposal_DescribeStatesTheSelfLimit is what makes an injection reviewable: a person
// deciding whether to allow a fault has to be told how it ends without being expected to
// know the action catalog.
func TestProposal_DescribeStatesTheSelfLimit(t *testing.T) {
	kill := killProposal().Describe()
	if !strings.Contains(kill, "nothing to revert") {
		t.Errorf("a one-shot fault must say it is a single event, got %q", kill)
	}

	fail := killProposal()
	fail.Experiment = podFailure()
	desc := fail.Describe()
	for _, want := range []string{"30s", "Chaos Mesh reverts it", "whether or not MaKlaude is still running"} {
		if !strings.Contains(desc, want) {
			t.Errorf("a duration-bounded fault's description must contain %q, got %q", want, desc)
		}
	}

	for _, want := range []string{"chaos experiment", proposalCluster, "app=web", "demo", "Rationale:"} {
		if !strings.Contains(kill, want) {
			t.Errorf("the description must contain %q so a reviewer sees what would break, got %q", want, kill)
		}
	}
}

// TestProposal_DescribeNamesAnUndeclaredSelfLimit: an action outside the catalog cannot be
// injected, and the sentence a human reads should say so rather than quietly omitting the
// clause that makes the request reviewable.
func TestProposal_DescribeNamesAnUndeclaredSelfLimit(t *testing.T) {
	p := killProposal()
	p.Experiment.Action = "pod-obliterate"
	if got := p.Describe(); !strings.Contains(got, "declares no self-limit") {
		t.Errorf("an uncatalogued action must be described as uninjectable, got %q", got)
	}
}

// trustEverything is an oracle that trusts every subject, used to prove the chaos verdict
// does not depend on the ledger being empty.
type trustEverything struct{}

func (trustEverything) Trust(autonomy.Subject) autonomy.TrustEvidence {
	return autonomy.TrustEvidence{Trusted: true, Citation: "this oracle trusts everything"}
}
