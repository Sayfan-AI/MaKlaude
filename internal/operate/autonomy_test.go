package operate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The blast-radius sections of the state summary are the whole reason #144 exists as a
// reporting task and not only as a policy one: a tripped breaker and a suppressed
// auto-apply are both states in which MaKlaude correctly does nothing, and a system
// doing nothing correctly is indistinguishable from a system with nothing to do. So
// every case below asserts what an operator SEES, and each condition — not configured,
// all clear, tripped, suppressed, sealed, nearly-tripped — is its own test.

// runText runs one cycle and returns the rendered text summary.
func runText(t *testing.T, c *Cycle) string {
	t.Helper()
	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return buf.String()
}

// budgetTarget names an object in the test cluster.
func budgetTarget(name string) remediate.Target {
	return remediate.Target{
		Cluster: testCluster, Kind: "deployment", Namespace: testNamespace,
		Name: name, ResourceVersion: "100",
	}
}

// memoryBudget builds an in-memory budget on a clock fixed at the cycle's own instant.
func memoryBudget() *budget.Budget {
	return budget.NewMemory(budget.DefaultLimits(), func() time.Time { return fixedTime })
}

func TestReport_AutonomyNotConfiguredSaysSoRatherThanSayingNothing(t *testing.T) {
	// The shipped posture. An absent section would read identically to a configured
	// budget with nothing to report, and those are very different postures.
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)

	out := runText(t, c)
	if !strings.Contains(out, "Autonomy (blast radius): not configured") {
		t.Errorf("the summary must state the unconfigured posture outright:\n%s", out)
	}
	if !strings.Contains(out, "no action can be auto-applied") {
		t.Errorf("the summary must say what unconfigured MEANS:\n%s", out)
	}
}

func TestReport_AutonomyAllClearIsPrintedUnconditionally(t *testing.T) {
	// The load-bearing case: a configured budget with nothing wrong still prints both
	// sections, and empty is stated in words. Empty means all-clear, and the reader is
	// told so rather than left to infer it from a missing heading.
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	c.UseBudget(memoryBudget())

	out := runText(t, c)
	for _, want := range []string{
		"Autonomy (blast radius):",
		"circuit breakers: none tripped",
		"suppressed auto-applies: none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the all-clear summary is missing %q:\n%s", want, out)
		}
	}
}

func TestReport_TrippedBreakerIsPrintedWithItsReason(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	b := memoryBudget()
	c.UseBudget(b)

	for range budget.DefaultFailureThreshold {
		b.RecordOutcome(testCluster, budgetTarget(testDeploy), budget.OutcomeFailed, fixedTime)
	}

	out := runText(t, c)
	for _, want := range []string{
		"circuit breakers TRIPPED (1)",
		"until a human clears them",
		testCluster,
		"consecutive auto-apply failures",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the tripped-breaker section is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "circuit breakers: none tripped") {
		t.Errorf("a tripped breaker must not also render as all-clear:\n%s", out)
	}
}

func TestReport_ClosedBreakerWithFailuresWarnsBeforeItTrips(t *testing.T) {
	// One failure away from tripping is worth seeing before the trip rather than
	// after, so a failure run is reported even while the breaker is still closed.
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	b := memoryBudget()
	c.UseBudget(b)

	b.RecordOutcome(testCluster, budgetTarget(testDeploy), budget.OutcomeFailed, fixedTime)

	out := runText(t, c)
	if !strings.Contains(out, "circuit breakers: none tripped") {
		t.Errorf("a closed breaker is still all-clear for the tripped section:\n%s", out)
	}
	if !strings.Contains(out, "1 consecutive auto-apply failure(s), breaker still closed") {
		t.Errorf("a building failure run must be visible before it trips:\n%s", out)
	}
}

func TestReport_SuppressedAutoApplyIsPrintedWithItsBound(t *testing.T) {
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	b := memoryBudget()
	c.UseBudget(b)

	// Run begins the pass. Then spend the cluster's whole allowance on two distinct
	// targets and ask for a third: only the cap can bound it, since that target has
	// never been touched and so has no cooldown.
	reg := singleClusterRegistry(t)
	report, err := c.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(report.Autonomy.Suppressed) != 0 {
		t.Fatalf("a pass in which nothing was asked has no suppressions, got %+v", report.Autonomy.Suppressed)
	}
	for _, name := range []string{"api", "web", "worker"} {
		b.Admit(testCluster, budgetTarget(name), fixedTime)
	}

	report.Autonomy = autonomyReport(b, posture{})
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"suppressed auto-applies (1)",
		"eligible actions a bound held back",
		"deployment/default/worker",
		"pass-cap-reached",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the suppression section is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "suppressed auto-applies: none") {
		t.Errorf("a suppression must not also render as all-clear:\n%s", out)
	}

	// A suppression describes one pass. The next cycle begins a fresh one, so a stale
	// suppression can never be read as a current one.
	next, err := c.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(next.Autonomy.Suppressed) != 0 {
		t.Errorf("a new pass must clear the previous pass's suppressions, got %+v", next.Autonomy.Suppressed)
	}
}

func TestReport_SealedBudgetIsLoudInTheSummary(t *testing.T) {
	// A sealed budget denies every auto-apply, which looks exactly like a quiet,
	// healthy system. It is the one state that must never be quiet.
	path := filepath.Join(t.TempDir(), "budget.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt state: %v", err)
	}
	b, err := budget.Open(path, budget.DefaultLimits(), func() time.Time { return fixedTime })
	if err != nil {
		t.Fatalf("opening the budget: %v", err)
	}

	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	c.UseBudget(b)

	out := runText(t, c)
	for _, want := range []string{"STATE UNREADABLE", "every auto-apply is denied", path} {
		if !strings.Contains(out, want) {
			t.Errorf("the sealed posture is missing %q:\n%s", want, out)
		}
	}
}

func TestReport_AutonomySectionSurvivesAnEmptyRegistry(t *testing.T) {
	// The no-clusters branch returns early, and an early return that skips the
	// blast-radius sections is exactly how a tripped breaker becomes invisible.
	b := memoryBudget()
	b.Trip(testCluster, "an anomalous burst", fixedTime)

	report := &Report{GeneratedAt: fixedTime, Mode: kube.ExecuteDisabled.String(), Autonomy: autonomyReport(b, posture{})}
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "No clusters registered.") {
		t.Fatalf("the empty-registry branch did not run:\n%s", out)
	}
	if !strings.Contains(out, "circuit breakers TRIPPED (1)") {
		t.Errorf("the blast-radius sections must survive the early return:\n%s", out)
	}
}

func TestReport_AutonomyJSONIsAlwaysPresentAndNeverNull(t *testing.T) {
	// A consumer of the JSON report must be able to read the two lists without a nil
	// check, so "nothing to report" is an empty array rather than null or an absent key.
	for name, attach := range map[string]bool{"unconfigured": false, "configured": true} {
		t.Run(name, func(t *testing.T) {
			c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
			if attach {
				c.UseBudget(memoryBudget())
			}
			report, err := c.Run(context.Background(), singleClusterRegistry(t))
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			var buf bytes.Buffer
			if err := report.WriteJSON(&buf); err != nil {
				t.Fatalf("WriteJSON: %v", err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
				t.Fatalf("parsing the JSON report: %v", err)
			}
			autonomy, ok := raw["autonomy"]
			if !ok {
				t.Fatalf("the report must always carry an autonomy object:\n%s", buf.String())
			}
			var back AutonomyReport
			if err := json.Unmarshal(autonomy, &back); err != nil {
				t.Fatalf("parsing the autonomy section: %v", err)
			}
			switch {
			case back.Configured != attach:
				t.Errorf("configured = %v, want %v", back.Configured, attach)
			case back.Breakers == nil:
				t.Error("Breakers must serialize as [] rather than null")
			case back.Suppressed == nil:
				t.Error("Suppressed must serialize as [] rather than null")
			}
		})
	}
}

func TestRun_BeginsAPassSoTheCapRefillsEveryCycle(t *testing.T) {
	// The cycle owns the pass lifecycle. Without a Begin per run the cap would either
	// never refill or refill on every call, and neither is a bound.
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	b := memoryBudget()
	c.UseBudget(b)

	reg := singleClusterRegistry(t)
	if _, err := c.Run(context.Background(), reg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, name := range []string{"api", "web"} {
		if g := b.Admit(testCluster, budgetTarget(name), fixedTime); !g.Admitted() {
			t.Fatalf("admission for %s must be within the cap, got %s", name, g)
		}
	}
	// Distinct target, so only the cap can bound it.
	if g := b.Admit(testCluster, budgetTarget("worker"), fixedTime); g.Admitted() {
		t.Fatal("the cap must be spent within one pass")
	}

	// A second cycle is a second pass. The cooldown still holds the two targets already
	// admitted, so the refill is asserted on a fresh one.
	if _, err := c.Run(context.Background(), reg); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if g := b.Admit(testCluster, budgetTarget("worker"), fixedTime); !g.Admitted() {
		t.Fatalf("a new cycle must refill the per-pass cap, got %s", g)
	}
}

func TestRun_WithNoBudgetAutoAppliesNothingAndReportsIt(t *testing.T) {
	// A nil budget is the shipped posture and must not read as a permissive one.
	c, _, _ := newCycle(t, kube.ExecuteDisabled, crashloopingWorkload()...)
	report, err := c.Run(context.Background(), singleClusterRegistry(t))
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if c.Budget() != nil {
		t.Fatal("no budget was attached, so none must be reported")
	}
	if report.Autonomy.Configured {
		t.Error("an unconfigured cycle must not report a configured budget")
	}
}

func TestAutonomyStateEnv_UnsetBuildsNoBudget(t *testing.T) {
	t.Setenv(AutonomyStateEnv, "")
	t.Setenv(ExecuteModeEnv, "")

	c, _, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Budget() != nil {
		t.Error("an unset state path must build no budget — that is the shipped posture")
	}
}

func TestAutonomyStateEnv_SetBuildsADurableBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "budget.json")
	t.Setenv(AutonomyStateEnv, path)
	t.Setenv(ExecuteModeEnv, "")

	c, _, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b := c.Budget()
	if b == nil {
		t.Fatal("a configured state path must build a budget")
	}
	if got := b.Status().Path; got != path {
		t.Errorf("state path = %q, want %q", got, path)
	}
	if b.Sealed() {
		t.Error("a state file that does not exist yet is a fresh install, not a corrupt one")
	}
}
