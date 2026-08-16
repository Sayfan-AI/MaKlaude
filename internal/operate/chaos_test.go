package operate

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/chaos"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// settle is the recovery allowance every scenario here passes to [Cycle.AdmitChaos]. It
// is short so the window's ceiling is easy to reason about against the pinned clock.
const settle = time.Minute

// chaosProposal is a well-formed pod-kill proposal against the test cluster.
func chaosProposal() chaos.Proposal {
	return chaos.Proposal{
		Cluster: testCluster,
		Experiment: chaos.Experiment{
			Action:    chaos.ActionPodKill,
			Namespace: "maklaude-chaos",
			Mode:      chaos.ModeOne,
			Selector: chaos.Selector{
				Namespaces:     []string{testNamespace},
				LabelSelectors: map[string]string{"app": "web"},
			},
		},
		Rationale:  "prove the crashloop detector fires while a remediation is in flight",
		ProposedAt: fixedTime,
	}
}

// chaosCycle builds a cycle with everything the chaos path needs: a budget, a ruleset, a
// trusting oracle, and a QUARANTINED ledger.
//
// The oracle trusts everything on purpose. Every gate assertion below would also pass
// against an empty ledger, and would then be proving that nothing had earned trust rather
// than that chaos cannot use it.
func chaosCycle(t *testing.T) (*Cycle, *trust.Ledger, *trust.Windows) {
	t.Helper()

	c, _, _ := newCycle(t, kube.ExecuteEnabled)
	c.UseBudget(memoryBudget())
	c.budget.Begin()

	ledger, windows := trust.NewMemory(), trust.NewMemoryWindows()
	q, err := trust.NewQuarantine(ledger, windows)
	if err != nil {
		t.Fatalf("NewQuarantine: %v", err)
	}
	c.rules = chaosRuleset()
	c.oracle = trustEverything{}
	c.ledger = q
	return c, ledger, windows
}

// chaosRuleset is as permissive as a valid ruleset can be about the test cluster: the
// experiment's own namespace, and the reversible operations. It cannot name a chaos
// operation — rule validation refuses that — which is itself part of the point.
func chaosRuleset() autonomy.Ruleset {
	return autonomy.Ruleset{{
		Name:             "restart-default",
		Clusters:         []string{testCluster},
		Namespaces:       []string{testNamespace},
		Operations:       []remediate.Operation{remediate.OpRolloutRestart},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
}

// TestDecideChaos_GatesOnAFullyTrustedCluster is T4's second done criterion at the layer
// that would carry the action out: whatever the trust state, an injection gates.
func TestDecideChaos_GatesOnAFullyTrustedCluster(t *testing.T) {
	c, _, _ := chaosCycle(t)

	d := c.DecideChaos(chaosProposal())

	if !d.Gated() {
		t.Fatalf("an experiment must be put to a human: %+v", d)
	}
	if d.Verdict.AutoApplies() {
		t.Fatal("an experiment was ruled auto-appliable on a fully trusted cluster")
	}
	if d.Decision != "gate" || d.Reason != "chaos-never-promotes" {
		t.Errorf("decision/reason = %s/%s, want gate/chaos-never-promotes", d.Decision, d.Reason)
	}
	if d.Error != "" {
		t.Errorf("a gated experiment is the normal outcome and must carry no error: %s", d.Error)
	}
}

// TestDecideChaos_RendersAsADistinctClass is T4's fourth done criterion: a chaos proposal
// renders as an unmistakably distinct class and never as a remediation.
func TestDecideChaos_RendersAsADistinctClass(t *testing.T) {
	c, _, _ := chaosCycle(t)
	p := chaosProposal()

	d := c.DecideChaos(p)

	if d.Class != "chaos" {
		t.Errorf("Class = %q, want chaos", d.Class)
	}
	if !strings.HasPrefix(d.Operation, chaos.OperationPrefix) {
		t.Errorf("Operation = %q, want the chaos prefix", d.Operation)
	}
	for _, catalog := range []string{"rolloutrestart", "rollbackrevision", "deletepod", "cordonnode"} {
		if d.Operation == catalog {
			t.Fatalf("an experiment rendered as the catalog operation %q", catalog)
		}
	}
	if d.SelfLimit != "instantaneous" {
		t.Errorf("SelfLimit = %q; the fact that makes an injection reviewable must be on the decision", d.SelfLimit)
	}
	if !strings.Contains(d.Description, "chaos experiment") || !strings.Contains(d.Description, "Rationale:") {
		t.Errorf("Description = %q, want the sentence a human approves", d.Description)
	}
	if d.Identity != string(p.Identity()) {
		t.Errorf("Identity = %q, want %q", d.Identity, p.Identity())
	}
}

// TestDecideChaos_RefusesAMalformedProposal: an experiment nobody can describe is not one
// to put to a human.
func TestDecideChaos_RefusesAMalformedProposal(t *testing.T) {
	c, _, _ := chaosCycle(t)
	p := chaosProposal()
	p.Rationale = ""

	d := c.DecideChaos(p)

	if d.Gated() {
		t.Fatal("a proposal with no rationale must not reach a human as a reviewable request")
	}
	if !strings.Contains(d.Error, "rationale is empty") {
		t.Errorf("Error = %q, want it to name the missing rationale", d.Error)
	}
}

// TestDecideChaos_RefusesAClusterMismatch covers the check the shared decision
// function contributes: the worst failure this system could have is an action aimed at
// the wrong cluster, and an experiment is not exempt from that.
func TestDecideChaos_RefusesAClusterMismatch(t *testing.T) {
	c, _, _ := chaosCycle(t)
	p := chaosProposal()
	p.Cluster = ""

	d := c.DecideChaos(p)

	if d.Gated() {
		t.Fatal("an experiment naming no cluster must not be offered to anybody")
	}
}

// TestAdmitChaos_InheritsTheBudget is T4's first done criterion: an approved experiment is
// bounded by the same ceiling an unattended remediation is.
func TestAdmitChaos_InheritsTheBudget(t *testing.T) {
	c, _, windows := chaosCycle(t)

	a := c.AdmitChaos(chaosProposal(), settle)

	if !a.Admitted() {
		t.Fatalf("the first experiment on a clear cluster must be admitted: %+v", a)
	}
	if a.Window.Cluster != testCluster {
		t.Errorf("admission must open a quarantine window on the cluster, got %+v", a.Window)
	}
	if !windows.Quarantined(testCluster, fixedTime) {
		t.Error("the window must be in force at the instant the experiment was admitted")
	}
}

// TestAdmitChaos_IsHeldByTheCooldown proves the per-target cooldown applies to chaos: the
// same blast scope cannot be re-broken immediately.
func TestAdmitChaos_IsHeldByTheCooldown(t *testing.T) {
	c, _, _ := chaosCycle(t)

	if a := c.AdmitChaos(chaosProposal(), settle); !a.Admitted() {
		t.Fatalf("precondition failed, the first admission must succeed: %+v", a)
	}
	second := c.AdmitChaos(chaosProposal(), settle)

	if second.Admitted() {
		t.Fatal("the same blast scope was admitted twice in one instant; the cooldown does not bound chaos")
	}
	if second.Grant.Reason != budget.ReasonTargetCoolingDown {
		t.Errorf("Reason = %s, want the cooldown", second.Grant.Reason)
	}
	if second.Window.Cluster != "" {
		t.Error("a refused admission must not open a quarantine window; the gap would be explained by a fault that never happened")
	}
}

// TestAdmitChaos_IsHeldByTheBreaker is the property that matters most on a bad day: a
// cluster whose breaker a human has not cleared does not get experiments run on it.
func TestAdmitChaos_IsHeldByTheBreaker(t *testing.T) {
	c, _, windows := chaosCycle(t)
	c.budget.Trip(testCluster, "three consecutive auto-apply failures", fixedTime)

	a := c.AdmitChaos(chaosProposal(), settle)

	if a.Admitted() {
		t.Fatal("an experiment ran on a cluster whose breaker is open")
	}
	if a.Grant.Reason != budget.ReasonBreakerTripped {
		t.Errorf("Reason = %s, want the breaker", a.Grant.Reason)
	}
	if windows.Quarantined(testCluster, fixedTime) {
		t.Error("nothing was injected, so nothing may quarantine the trust ledger")
	}
}

// TestAdmitChaos_IsHeldByThePassCap proves the per-pass ceiling counts experiments the
// same way it counts unattended remediations — a basket of approved experiments cannot
// empty a cluster in one pass.
func TestAdmitChaos_IsHeldByThePassCap(t *testing.T) {
	c, _, _ := chaosCycle(t)
	perPass := c.budget.Limits().PerClusterPerPass

	admitted := 0
	for i := 0; i < perPass+2; i++ {
		p := chaosProposal()
		// A distinct blast scope each time, so the cooldown is not what stops this.
		p.Experiment.Selector.LabelSelectors = map[string]string{"app": "web", "shard": string(rune('a' + i))}
		if c.AdmitChaos(p, settle).Admitted() {
			admitted++
		}
	}

	if admitted != perPass {
		t.Errorf("%d experiments were admitted against a per-pass cap of %d", admitted, perPass)
	}
}

// TestAdmitChaos_RefusesWithoutABudget: eligibility with no ceiling is the failure the
// budget package's doc opens with, and an experiment is the last action that should get
// an exemption from it.
func TestAdmitChaos_RefusesWithoutABudget(t *testing.T) {
	c, _, _ := chaosCycle(t)
	c.budget = nil

	a := c.AdmitChaos(chaosProposal(), settle)

	if a.Admitted() {
		t.Fatal("an experiment was admitted with no blast-radius ceiling wired")
	}
	if !strings.Contains(a.Error, "no blast-radius budget") {
		t.Errorf("Error = %q, want it to name the missing ceiling", a.Error)
	}
}

// TestAdmitChaos_RefusesAnUnquarantinedLedger is the one that keeps chaos from silently
// destroying M5's work.
//
// A cycle recording trust through a bare ledger would demote every shape on the cluster
// for failures MaKlaude caused on purpose, and it would do it invisibly — the shapes would
// simply stop auto-applying. So the injection is refused at the point of admission rather
// than discovered later from a history that no longer says what happened.
func TestAdmitChaos_RefusesAnUnquarantinedLedger(t *testing.T) {
	c, ledger, _ := chaosCycle(t)
	c.ledger = ledger // the bare ledger, no quarantine in front of it

	a := c.AdmitChaos(chaosProposal(), settle)

	if a.Admitted() {
		t.Fatal("an experiment was admitted against an unquarantined ledger")
	}
	if !strings.Contains(a.Error, "trust.NewQuarantine") {
		t.Errorf("Error = %q, want it to name the wiring that fixes it", a.Error)
	}
}

// TestAdmitChaos_WindowCeilingCoversTheFaultAndTheSettle pins the arithmetic, because the
// ceiling is what lifts the quarantine when a process dies mid-experiment and a ceiling
// that is too short would admit outcomes from a cluster still under a fault.
func TestAdmitChaos_WindowCeilingCoversTheFaultAndTheSettle(t *testing.T) {
	c, _, _ := chaosCycle(t)
	p := chaosProposal()
	p.Experiment.Action = chaos.ActionPodFailure
	p.Experiment.Duration = 2 * time.Minute

	a := c.AdmitChaos(p, settle)

	if !a.Admitted() {
		t.Fatalf("precondition failed: %+v", a)
	}
	want := fixedTime.Add(2 * time.Minute).Add(settle)
	if !a.Window.Until.Equal(want) {
		t.Errorf("window ceiling = %s, want the fault's duration plus the settle allowance (%s)", a.Window.Until, want)
	}
	if a.Window.Active(want) {
		t.Error("the window must not be active at its own ceiling")
	}
}

// TestChaosWindowKeepsDeliberateFailuresOutOfTheLedger is T4's third done criterion,
// end to end through the cycle: admit, record an outcome that would ordinarily demote,
// close, and check the ledger never saw it — while the window itself is recoverable.
func TestChaosWindowKeepsDeliberateFailuresOutOfTheLedger(t *testing.T) {
	c, ledger, windows := chaosCycle(t)

	a := c.AdmitChaos(chaosProposal(), settle)
	if !a.Admitted() {
		t.Fatalf("precondition failed: %+v", a)
	}

	// The cluster produces a regression while the fault is live: MaKlaude reported this
	// fixed and the fault is back, which is exactly what the experiment caused.
	shape := autonomy.Shape{Cluster: testCluster, Operation: remediate.OpRolloutRestart}
	if err := c.ledger.NoteRecurrence("some-proposal", shape, fixedTime.Add(time.Second)); err != nil {
		t.Fatalf("NoteRecurrence: %v", err)
	}
	if n := ledger.Len(); n != 0 {
		t.Fatalf("the ledger holds %d entries; a fault MaKlaude caused on purpose is not evidence about its fixes", n)
	}

	closed, err := c.CloseChaosWindow(a.Window)
	if err != nil {
		t.Fatalf("CloseChaosWindow: %v", err)
	}
	if closed.End.IsZero() {
		t.Error("a closed window must record when it closed")
	}
	if windows.Quarantined(testCluster, fixedTime.Add(2*time.Second)) {
		t.Error("the quarantine must lift once the window is closed, or the ledger stops learning from real outcomes")
	}

	// And the record answers the question the human asked for: was the ledger
	// quarantined when this happened, and why.
	all := windows.All()
	if len(all) != 1 {
		t.Fatalf("the window log holds %d windows, want the one experiment's", len(all))
	}
	for _, want := range []string{testCluster, "chaos experiment", "pod-kill"} {
		if !strings.Contains(all[0].String(), want) {
			t.Errorf("the recorded window must contain %q, got %q", want, all[0])
		}
	}
}

// TestCloseChaosWindow_WithoutAQuarantineIsAnError: a caller holding a window and a cycle
// with nothing to close it on disagree about what happened, and that must not pass
// silently.
func TestCloseChaosWindow_WithoutAQuarantineIsAnError(t *testing.T) {
	c, ledger, _ := chaosCycle(t)
	a := c.AdmitChaos(chaosProposal(), settle)
	if !a.Admitted() {
		t.Fatalf("precondition failed: %+v", a)
	}
	c.ledger = ledger

	if _, err := c.CloseChaosWindow(a.Window); err == nil {
		t.Error("closing a window on a cycle with no quarantine must be an error")
	}
}

// TestRecordChaosOutcome_FeedsTheSameBreaker: a failed injection is MaKlaude asking a
// cluster to do something and being unable to tell whether it happened, which is the
// condition the breaker exists for regardless of which class produced it.
func TestRecordChaosOutcome_FeedsTheSameBreaker(t *testing.T) {
	c, _, _ := chaosCycle(t)
	p := chaosProposal()
	threshold := c.budget.Limits().FailureThreshold

	var last budget.Consequence
	for i := 0; i < threshold; i++ {
		last = c.RecordChaosOutcome(p, budget.OutcomeFailed)
	}

	if !last.Tripped {
		t.Fatalf("%d failed injections must trip the breaker, got %+v", threshold, last)
	}
	if c.AdmitChaos(p, settle).Admitted() {
		t.Error("a tripped breaker must stop the next experiment as well as the next remediation")
	}
}

// TestReport_QuarantineIsVisibleInTheSummary: a gap in the trust history has to be
// visible in the run that caused it, or the shapes on a cluster under chaos just silently
// stop moving.
func TestReport_QuarantineIsVisibleInTheSummary(t *testing.T) {
	c, _, _ := chaosCycle(t)
	if a := c.AdmitChaos(chaosProposal(), settle); !a.Admitted() {
		t.Fatalf("precondition failed: %+v", a)
	}

	out := renderAutonomy(t, autonomyReport(c.budget, c.posture()))

	if !strings.Contains(out, "trust ledger QUARANTINED") {
		t.Errorf("an active chaos window must be reported:\n%s", out)
	}
	if !strings.Contains(out, "chaos experiment") {
		t.Errorf("the report must say which experiment the quarantine is for:\n%s", out)
	}
}

// TestReport_SaysNothingAboutQuarantineWhenThereIsNone keeps the section from becoming
// noise every deployment that never runs chaos has to read past.
func TestReport_SaysNothingAboutQuarantineWhenThereIsNone(t *testing.T) {
	c, _, _ := chaosCycle(t)

	out := renderAutonomy(t, autonomyReport(c.budget, c.posture()))

	if strings.Contains(out, "QUARANTINED") {
		t.Errorf("no window is in force, so the report must say nothing about one:\n%s", out)
	}
}

// trustEverything is an oracle that trusts every subject, so the gate assertions above
// are about the class rather than about an empty ledger.
type trustEverything struct{}

func (trustEverything) Trust(autonomy.Subject) autonomy.TrustEvidence {
	return autonomy.TrustEvidence{Trusted: true, Citation: "this oracle trusts everything"}
}
