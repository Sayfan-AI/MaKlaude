//go:build e2e

// This file is Milestone 5's end-to-end proof (T6, #146): MaKlaude changes a real
// cluster with NOBODY WATCHING, and every bound that is supposed to stop it doing so
// actually stops it.
//
// remediation_test.go proves the gated path: a human says yes and exactly one action
// follows. That test's whole safety argument rests on a person being in the loop. This
// one removes the person, which makes the question completely different — not "did the
// approved action run?" but "what runs when nothing approves anything, and what does
// NOT run when a bound says no?" A milestone that ships unattended mutation has to
// answer the second half in a place where a regression fails the build, because the
// failure mode is silent by construction: an autonomy layer that has quietly stopped
// enforcing a bound looks exactly like one that was never asked.
//
// # The five assertions, and the shape of the evidence for each
//
//	(a) a shape with NO trust history GATES — the proposal exists, it is eligible under
//	    the operator's rules, and it still goes to a human because the history that
//	    would earn it does not exist;
//	(b) the SAME shape with a seeded trust history AUTO-APPLIES, the cluster converges,
//	    and the action is disclosed on an artifact that says no human approved it;
//	(c) an off-allowlist proposal NEVER auto-applies even though its shape is trusted,
//	    and a ruleset that tries to configure an irreversible action grants nothing at
//	    all rather than granting the parts of it that parse;
//	(d) a failed auto-apply TRIPS THE BREAKER, re-gates the shape in the ledger, and the
//	    next pass auto-applies nothing on that cluster;
//	(e) no write outside the policy's stated bounds ever reached any cluster.
//
// # Why the phases share one budget and one cluster, in order
//
// The blast-radius state is per-cluster and durable — that is the point of a breaker —
// so the phases run against ONE [budget.Budget] in sequence, and the order is the
// argument. (b) must run while the breaker is closed, (d) trips it, and (e) then
// observes a pass in which nothing can be admitted. Splitting them across independent
// budgets would test each mechanism against a fresh world and would never once show
// that one phase's failure changes the next phase's behaviour, which is the only thing
// a circuit breaker is.
//
// The trust ledger is deliberately NOT shared the same way. Phase (d)'s failure demotes
// the shape, so a phase after it would gate for two reasons at once and could not tell
// which bound held the action back. Each phase therefore gets a ledger seeded to state
// exactly one trust posture, and the breaker phase asserts on the SUPPRESSION REASON so
// "the breaker did it" is a claim about the recorded token rather than about the
// absence of an action.
//
// # A note on the seeded ledger, and what it does not prove
//
// (b) seeds the ledger directly with three human-approved converged executions, which
// is what the task asks for. That is a faithful test of the promotion ARITHMETIC and of
// everything downstream of it; it is not a test that the live gated path ever writes
// such an entry, and at the time of writing it does not — see #166, which is open
// against exactly that wiring gap. Seeding here is therefore honest rather than
// convenient: the test states the history it assumes, and the issue tracks the fact
// that production cannot yet produce it.
//
// # Ordering
//
// Go runs a package's test files in filename order, so this file runs last. That is
// required rather than incidental: remediation_test.go's audit-log ledger asserts that
// exactly ONE mutating request has landed on the cluster, and the actions here land
// more. The corresponding assertion for this phase (assertNoWriteOutsideAutonomyBounds)
// is the order-independent form — every landed write must name an object something
// authorized, whoever authorized it.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/operate"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// The three scenario namespaces and their Deployments. One Deployment per namespace,
// because a namespace list is the only blast-radius bound an autonomy rule has — see
// test/e2e/manifests/autonomy-scenarios.yaml for the full argument.
const (
	autoNamespace      = "maklaude-e2e-auto"
	offLimitsNamespace = "maklaude-e2e-offlimits"
	failNamespace      = "maklaude-e2e-fail"

	earnedDeploy     = "earned"
	offLimitsDeploy  = "offlimits"
	neverReadyDeploy = "neverready"
)

// The rule names. They reach the disclosure artifact and the audit record as the
// authorizing policy, so they are asserted on rather than merely passed in.
const (
	earnedRule     = "e2e-auto-rollback"
	failingRule    = "e2e-fail-rollback"
	suppressedRule = "e2e-breaker-rollback"
	badRule        = "e2e-irreversible-rollback"
)

const (
	// The window for an auto-apply that is EXPECTED to converge. It matches the gated
	// path's for the same reason (a CI runner's disk and a cold kubelet are slower than
	// a laptop), and convergence is watched by the production runner, not by the test.
	unattendedObserveWindow   = 150 * time.Second
	unattendedObserveInterval = 3 * time.Second

	// The window for the auto-apply that CANNOT converge. It is short because the
	// outcome does not depend on its length: `neverready`'s readiness probe fails
	// forever, so the verdict is "timed out" whether the window is 20 seconds or 20
	// minutes, and the only thing a longer one buys is a slower build. Note this is the
	// opposite of using a short window to MANUFACTURE a timeout, which would make the
	// assertion a race — see the manifest header.
	failingObserveWindow   = 20 * time.Second
	failingObserveInterval = 4 * time.Second
)

// TestE2E_UnattendedAutonomy drives the whole unattended decision surface against the
// live kind cluster: gate without trust, auto-apply with it, refuse what no rule
// covers, trip the breaker on a failure, and stay stopped afterwards.
func TestE2E_UnattendedAutonomy(t *testing.T) {
	reg := buildExecutorRegistry(t)
	h := executorHandle(t, reg)

	// Reads go through the ordinary read-only client and its transport guard, as
	// everywhere else in this suite: observing the write path with the write path's own
	// client would make every before/after comparison depend on the thing under test.
	reader, err := kube.NewClient(h)
	if err != nil {
		t.Fatalf("building the read-only client for the executor identity: %v", err)
	}
	collector := health.NewCollector(reader)

	// One budget for the whole test. See the file header: the breaker is the only
	// mechanism here whose meaning is a change of state ACROSS passes.
	//
	// FailureThreshold is 1 rather than the shipped 2 so a single failed auto-apply
	// trips it. That is a bound made STRICTER than the default, which is the only
	// direction a test may move a safety knob: a test that loosened one would prove
	// something about a configuration no operator has.
	bounds := budget.Limits{
		PerClusterPerPass: budget.DefaultPerClusterPerPass,
		Cooldown:          budget.DefaultCooldown,
		FailureThreshold:  1,
	}
	blast := budget.NewMemory(bounds, time.Now)

	// Captured before anything unattended runs, so (e) has a before-state for every
	// object no phase is allowed to touch.
	untouched := captureBeforeAutonomy(t, reader)

	// Every phase that seeds a trust history needs the fingerprint of the fix it is
	// seeding for, and the fingerprints are read off real proposals rather than built
	// here. See [discoverFingerprints]. This runs before (a) because it must observe
	// the cluster in the state the later phases will propose against, and it is safe to
	// run first because an empty ledger trusts nothing.
	fps := discoverFingerprints(t, reg)

	// --- (a) No trust history: the proposal is eligible and still goes to a human. ---
	assertUntrustedShapeGates(t, reg, reader, blast, fps)

	// --- (b) Seeded trust history: the same fix runs unattended and converges. ---
	//         The same pass also carries (c)'s first half: `offlimits` is proposed and
	//         its fix is trusted too, so the rule's namespace bound is the ONLY thing
	//         holding it back. Seeding only `earned` here would make that half vacuous.
	assertEarnedTrustAutoApplies(t, reg, reader, collector, blast, fps)

	// --- (c) A ruleset that tries to permit an irreversible action grants nothing. ---
	assertInvalidRulesetGrantsNothing(t, reg, reader, blast, fps)

	// --- (d) A failed auto-apply trips the breaker and re-gates the shape. ---
	assertFailedAutoApplyTripsBreaker(t, reg, blast, fps)

	// --- (d, continued) The tripped breaker stops the NEXT pass. ---
	assertTrippedBreakerSuppressesTheNextPass(t, reg, blast, fps)

	// --- (e) Nothing outside the stated bounds was ever written. ---
	assertNothingOutOfBoundsChanged(t, reader, untouched)
	assertNoWriteOutsideAutonomyBounds(t)
}

// --- (a) ---------------------------------------------------------------------------

// assertUntrustedShapeGates is assertion (a): a fault a rule covers, on a cluster where
// the shape has earned nothing, is proposed and NOT acted on.
//
// The vacuity guard is the important half. "Nothing was auto-applied" is also what a
// pass with no proposals produces, what a pass against an unreachable cluster produces,
// and what a pass with a typo in the namespace produces — so the assertion first
// requires the rollback of `earned` to be present in the proposals, and only then
// requires it not to have run.
func assertUntrustedShapeGates(t *testing.T, reg *cluster.Registry, reader *kube.Client, blast *budget.Budget, fps fingerprints) {
	t.Helper()

	// An empty ledger, wired as the oracle AND as the demoter, is a cluster on its first
	// day: everything gates, by design and not by accident.
	ledger := trust.NewMemory()
	before := generationOf(t, reader, autoNamespace, earnedDeploy)

	p := runUnattendedPass(t, reg, blast, rollbackRule(earnedRule, autoNamespace), ledger, ledger,
		execute.Policy{ObserveWindow: unattendedObserveWindow, ObserveInterval: unattendedObserveInterval})

	requireRollbackProposed(t, p, autoNamespace, earnedDeploy)
	if len(p.cluster.AutoApplied) != 0 {
		t.Fatalf("AUTONOMY WITHOUT EVIDENCE: %d action(s) ran unattended against a cluster with an empty trust ledger: %+v",
			len(p.cluster.AutoApplied), p.cluster.AutoApplied)
	}

	// Not acted on is only half of "gated". The other half is that the proposal reached
	// the human gate, because an eligible proposal that is neither auto-applied nor
	// asked about has been silently dropped — and that reads identically from the
	// cluster's point of view.
	if p.cluster.Gate.Opened == 0 {
		t.Errorf("the untrusted proposal was neither auto-applied nor put to a human: gate report %+v", p.cluster.Gate)
	}
	if got := generationOf(t, reader, autoNamespace, earnedDeploy); got != before {
		t.Errorf("UNAPPROVED WRITE: deployment %s/%s moved from generation %d to %d while its shape was untrusted",
			autoNamespace, earnedDeploy, before, got)
	}

	// The ledger's own account of why, asserted so the test fails differently when the
	// fix gates for some unrelated reason.
	subject := fps.subject(t, target(autoNamespace, earnedDeploy))
	if standing := ledger.Standing(subject); standing.Trusted || standing.Approved != 0 {
		t.Errorf("the empty ledger reports standing %+v for %+v; it must trust nothing", standing, subject)
	}
	t.Logf("untrusted fix %s gated: %s", subject.Fingerprint, ledger.Explain(subject))
}

// --- (b) and the first half of (c) ---------------------------------------------------

// assertEarnedTrustAutoApplies is assertion (b), and it carries the strongest half of
// (c) in the same pass.
//
// One pass, two objects with the IDENTICAL trusted shape: `earned`, inside the rule's
// namespace, and `offlimits`, outside it. Running them together is what makes the
// second one evidence. Asserting "the off-allowlist proposal did not run" in a pass
// where nothing ran would prove nothing; asserting it in a pass where the very same
// operation, on the very same trust, DID run against the very same cluster isolates the
// rule as the reason.
func assertEarnedTrustAutoApplies(t *testing.T, reg *cluster.Registry, reader *kube.Client,
	collector *health.Collector, blast *budget.Budget, fps fingerprints) {
	t.Helper()

	// Both fixes are trusted. `offlimits` is held back by the rule's namespace bound and
	// by nothing else, which is the half of (c) this pass carries — seeding only `earned`
	// would let it pass for the wrong reason.
	ledger := seededLedger(t, fps,
		target(autoNamespace, earnedDeploy),
		target(offLimitsNamespace, offLimitsDeploy))
	earnedBefore := generationOf(t, reader, autoNamespace, earnedDeploy)
	offLimitsBefore := generationOf(t, reader, offLimitsNamespace, offLimitsDeploy)

	p := runUnattendedPass(t, reg, blast, rollbackRule(earnedRule, autoNamespace), ledger, ledger,
		execute.Policy{ObserveWindow: unattendedObserveWindow, ObserveInterval: unattendedObserveInterval})

	requireRollbackProposed(t, p, autoNamespace, earnedDeploy)
	requireRollbackProposed(t, p, offLimitsNamespace, offLimitsDeploy)

	if len(p.cluster.AutoApplied) != 1 {
		t.Fatalf("the pass auto-applied %d action(s), want exactly 1 (the rollback of %s/%s): %+v",
			len(p.cluster.AutoApplied), autoNamespace, earnedDeploy, p.cluster.AutoApplied)
	}
	applied := p.cluster.AutoApplied[0]

	// What permitted it, what earned it, and where it is disclosed. Each of the three is
	// separately load-bearing: without the rule name nobody can revoke it, without the
	// citation the action has no oversight artifact at all (there is no approver to
	// name), and without the disclosure reference there is nothing for a person to read.
	if want := "deployment/" + autoNamespace + "/" + earnedDeploy; applied.Target != want {
		t.Fatalf("the unattended action targeted %s, want %s", applied.Target, want)
	}
	if applied.Rule != earnedRule {
		t.Errorf("the action records rule %q, want %q", applied.Rule, earnedRule)
	}
	if applied.Reason != "earned-trust" {
		t.Errorf("the action records reason %q, want \"earned-trust\"", applied.Reason)
	}
	if applied.Admission != "admitted" {
		t.Errorf("the action records admission %q, want \"admitted\"", applied.Admission)
	}
	if !strings.Contains(applied.Evidence, "human-approved executions of this exact fix") {
		t.Errorf("the action's trust citation does not state the evidence it rests on: %q", applied.Evidence)
	}
	if applied.Disclosure == "" {
		t.Fatal("the action carries no disclosure reference, so an unattended mutation has no artifact a person could read")
	}
	if applied.Error != "" {
		t.Errorf("the unattended machinery reported an error: %s", applied.Error)
	}

	// What it did. Authority is asserted explicitly because "policy" and "human" land in
	// the same audit field, and an unattended action recorded as human-approved would be
	// the worst possible entry in this trail.
	exec := applied.Execution
	switch {
	case !exec.Executed:
		t.Fatalf("the unattended action did not execute: failure=%q error=%q", exec.Failure, exec.Error)
	case exec.Previewed:
		t.Fatal("the unattended action reports itself a preview; the cycle was not in enabled mode")
	case exec.Authority != audit.AuthorityPolicy.String():
		t.Errorf("the unattended action was recorded under authority %q, want %q", exec.Authority, audit.AuthorityPolicy)
	case exec.Attempts != 1:
		t.Errorf("the unattended action produced %d mutating requests, want exactly 1", exec.Attempts)
	case exec.Convergence != execute.ConvergenceConverged.String():
		t.Fatalf("the unattended action reports convergence %q: %s", exec.Convergence, exec.Error)
	}
	if applied.Tripped || applied.Demoted || applied.RolledBack {
		t.Errorf("a converged action reported a failure consequence: tripped=%t demoted=%t rolledBack=%t",
			applied.Tripped, applied.Demoted, applied.RolledBack)
	}

	// The cluster's own account, read back through the ordinary read path rather than
	// from the runner's verdict — the same second-opinion argument assertWedgedIsHealthy
	// makes for the gated path.
	if got := generationOf(t, reader, autoNamespace, earnedDeploy); got <= earnedBefore {
		t.Errorf("deployment %s/%s is still at generation %d (was %d); the unattended patch did not change the spec",
			autoNamespace, earnedDeploy, got, earnedBefore)
	}
	assertRolledBackToAGoodImage(t, reader, autoNamespace, earnedDeploy)
	assertDeploymentIsHealthy(t, collector, autoNamespace, earnedDeploy)

	// The durable record. An unattended action's artifact is the ONLY thing standing in
	// for an approver, so it must say outright that nobody approved it and must carry
	// the citation that did.
	assertDisclosureIsHonest(t, p.disclosed, disclose.Ref(applied.Disclosure))

	// (c), first half: same shape, same trust, same cluster, different namespace.
	if got := generationOf(t, reader, offLimitsNamespace, offLimitsDeploy); got != offLimitsBefore {
		t.Errorf("OUT-OF-SCOPE WRITE: deployment %s/%s moved from generation %d to %d; no rule names its namespace",
			offLimitsNamespace, offLimitsDeploy, offLimitsBefore, got)
	}
	t.Logf("auto-applied %s on %s under rule %q, disclosed on %s; %s/%s was left alone",
		applied.Operation, applied.Target, applied.Rule, applied.Disclosure, offLimitsNamespace, offLimitsDeploy)
}

// --- (c), second half ----------------------------------------------------------------

// assertInvalidRulesetGrantsNothing is the "even if explicitly configured" half of (c).
//
// [remediate.ReversibilityIrreversible] is the one setting an operator may not choose:
// an irreversible action is refused before any rule is read, so permitting it in a rule
// would be a knob that does nothing. [autonomy.Ruleset.Validate] therefore rejects it —
// and rejects the WHOLE ruleset, including the rules around it that are perfectly
// well-formed.
//
// That total failure is the property worth an e2e rather than a unit test. The
// alternative behaviour (honor the valid rules, skip the bad one) is strictly narrower
// than what the operator wrote and therefore looks safe, and it is the one that hides
// the mistake: three rules' worth of autonomy and no signal. Here the operator's file
// contains a rule that WOULD have auto-applied `earned`'s sibling fault, the shape is
// trusted, and the answer is still nothing.
func assertInvalidRulesetGrantsNothing(t *testing.T, reg *cluster.Registry, reader *kube.Client, blast *budget.Budget, fps fingerprints) {
	t.Helper()

	rules := append(rollbackRule(suppressedRule, offLimitsNamespace), autonomy.Rule{
		Name:       badRule,
		Clusters:   []string{executorClusterName},
		Namespaces: []string{offLimitsNamespace},
		Operations: []remediate.Operation{remediate.OpRollbackRevision},
		// The line an operator must not be able to write.
		MaxReversibility: remediate.ReversibilityIrreversible,
	})
	if err := rules.Validate(); err == nil {
		t.Fatal("a ruleset permitting irreversible actions validated; the configuration surface no longer refuses the one setting it must")
	}

	ledger := seededLedger(t, fps, target(offLimitsNamespace, offLimitsDeploy))
	before := generationOf(t, reader, offLimitsNamespace, offLimitsDeploy)

	p := runUnattendedPass(t, reg, blast, rules, ledger, ledger,
		execute.Policy{ObserveWindow: unattendedObserveWindow, ObserveInterval: unattendedObserveInterval})

	// Vacuity guard: the FIRST rule in that set covers this proposal, and the shape is
	// trusted, so an implementation that honored the valid rules would auto-apply here.
	requireRollbackProposed(t, p, offLimitsNamespace, offLimitsDeploy)

	if len(p.cluster.AutoApplied) != 0 {
		t.Fatalf("MALFORMED CONFIGURATION GRANTED AUTONOMY: %d action(s) ran under a ruleset that does not validate: %+v",
			len(p.cluster.AutoApplied), p.cluster.AutoApplied)
	}
	if got := generationOf(t, reader, offLimitsNamespace, offLimitsDeploy); got != before {
		t.Errorf("UNAPPROVED WRITE: deployment %s/%s moved from generation %d to %d under an invalid ruleset",
			offLimitsNamespace, offLimitsDeploy, before, got)
	}
	if p.cluster.Gate.Opened == 0 {
		t.Errorf("an invalid ruleset must gate proposals, not drop them: gate report %+v", p.cluster.Gate)
	}
}

// --- (d) ------------------------------------------------------------------------------

// assertFailedAutoApplyTripsBreaker is assertion (d): an unattended action that lands
// and does not work takes the cluster out of autonomy.
//
// The failure is structural rather than timed — `neverready`'s readiness probe fails
// forever, so the rollback the pipeline proposes cannot converge however long anyone
// watches. See the manifest header for why a short window would have been the wrong way
// to produce this.
//
// Three consequences are asserted, and they are three because they are carried out by
// three different layers on purpose: the breaker (this cluster stops), the demotion
// (this SHAPE stops, on every cluster the ledger covers), and the rollback (the change
// itself is undone). A regression that dropped any one of them would leave the other
// two looking like a working failure path.
func assertFailedAutoApplyTripsBreaker(t *testing.T, reg *cluster.Registry, blast *budget.Budget, fps fingerprints) {
	t.Helper()

	doomed := target(failNamespace, neverReadyDeploy)
	ledger := seededLedger(t, fps, doomed)
	subject := fps.subject(t, doomed)
	if !ledger.Trust(subject).Trusted {
		t.Fatalf("the seeded ledger does not trust %s, so this phase would prove nothing: %s", doomed, ledger.Explain(subject))
	}

	p := runUnattendedPass(t, reg, blast, rollbackRule(failingRule, failNamespace), ledger, ledger,
		execute.Policy{ObserveWindow: failingObserveWindow, ObserveInterval: failingObserveInterval})

	requireRollbackProposed(t, p, failNamespace, neverReadyDeploy)
	if len(p.cluster.AutoApplied) != 1 {
		t.Fatalf("the pass auto-applied %d action(s), want exactly 1 (the doomed rollback of %s/%s): %+v",
			len(p.cluster.AutoApplied), failNamespace, neverReadyDeploy, p.cluster.AutoApplied)
	}
	applied := p.cluster.AutoApplied[0]

	if want := "deployment/" + failNamespace + "/" + neverReadyDeploy; applied.Target != want {
		t.Fatalf("the unattended action targeted %s, want %s", applied.Target, want)
	}
	// The action must genuinely have RUN. A failure that never sent anything would trip
	// the breaker just as well and would prove something much weaker.
	if !applied.Execution.Executed {
		t.Fatalf("the doomed action never executed (%s: %s), so its failure says nothing about a change that landed",
			applied.Execution.Failure, applied.Execution.Error)
	}
	if applied.Execution.Convergence != execute.ConvergenceTimedOut.String() {
		t.Fatalf("the doomed action reports convergence %q, want %q — the fault it targets cannot be fixed by a rollback",
			applied.Execution.Convergence, execute.ConvergenceTimedOut)
	}
	if !applied.Tripped {
		t.Errorf("a failed unattended action did not trip the cluster's breaker: %+v", applied)
	}
	if !applied.Demoted {
		t.Errorf("a failed unattended action did not record a demotion against its shape: %+v", applied)
	}
	if !applied.RolledBack {
		t.Errorf("a failed unattended action was not rolled back, though a revision rollback is reversible: %+v", applied)
	}

	// The shape is re-gated in the ledger. This is the demotion as STATE rather than as
	// a reported flag: the flag says the write was attempted, the standing says it took
	// effect, and only the second one changes what the next pass may do.
	standing := ledger.Standing(subject)
	if standing.Trusted || !standing.Blocked {
		t.Errorf("the failed action left %s trusted (standing %+v); a failure must re-gate the shape", doomed, standing)
	}
	// Demotion is scoped to the SHAPE, not to the fix that failed — so the fingerprint
	// of a DIFFERENT rollback on this cluster is blocked too. That asymmetry is the
	// safety argument behind issue #167: if demotion were fingerprint-scoped, changing
	// the fix would mint a clean record and launder the failure.
	if sibling := ledger.Standing(fps.subject(t, target(offLimitsNamespace, offLimitsDeploy))); !sibling.Blocked {
		t.Errorf("a failure on %s left a sibling fix of the same shape unblocked (standing %+v); demotion must be shape-scoped",
			doomed, sibling)
	}

	// The breaker as durable state, and as something an operator can SEE. A tripped
	// breaker nobody is told about is the invisible-nothing-happened failure this
	// repository keeps paying for.
	tripped := blast.Status().Tripped()
	if len(tripped) != 1 || tripped[0].Cluster != executorClusterName {
		t.Fatalf("the budget reports tripped breakers %+v, want exactly one for %s", tripped, executorClusterName)
	}
	for _, want := range []string{"circuit breakers TRIPPED", executorClusterName, "until a human clears them"} {
		if !strings.Contains(p.text, want) {
			t.Errorf("the state summary does not report the tripped breaker (%q missing):\n%s", want, p.text)
		}
	}
	t.Logf("the doomed rollback failed as designed and tripped the breaker: %s", tripped[0].Detail)
}

// assertTrippedBreakerSuppressesTheNextPass is the second half of (d): once the breaker
// is open, the next pass auto-applies nothing on that cluster.
//
// It runs with a FRESHLY seeded ledger, which is the whole design of this phase. The
// previous phase demoted the shape, so reusing its ledger would gate this pass for two
// independent reasons and the test could not tell which one did it. A trusted shape,
// a rule that covers the target, and a tripped breaker isolates the breaker as the only
// bound in play — and the assertion is on the recorded suppression reason rather than
// on the absence of an action, because absence is what every other failure also looks
// like.
func assertTrippedBreakerSuppressesTheNextPass(t *testing.T, reg *cluster.Registry, blast *budget.Budget, fps fingerprints) {
	t.Helper()

	ledger := seededLedger(t, fps, target(offLimitsNamespace, offLimitsDeploy))
	rules := append(rollbackRule(suppressedRule, offLimitsNamespace), rollbackRule(failingRule, failNamespace)...)

	p := runUnattendedPass(t, reg, blast, rules, ledger, ledger,
		execute.Policy{ObserveWindow: failingObserveWindow, ObserveInterval: failingObserveInterval})

	// Vacuity guard: at least one proposal this pass was covered by a rule and carried a
	// trusted shape, so the only thing standing between it and the cluster is the breaker.
	requireRollbackProposed(t, p, offLimitsNamespace, offLimitsDeploy)

	if len(p.cluster.AutoApplied) != 0 {
		t.Fatalf("BREAKER IGNORED: %d action(s) ran unattended on a cluster whose breaker is open: %+v",
			len(p.cluster.AutoApplied), p.cluster.AutoApplied)
	}
	if len(p.report.Autonomy.Suppressed) == 0 {
		t.Fatal("the pass suppressed nothing; an eligible, trusted proposal on a tripped cluster must be RECORDED as held back, not silently skipped")
	}
	for _, s := range p.report.Autonomy.Suppressed {
		if s.Reason != budget.ReasonBreakerTripped.String() {
			t.Errorf("suppression of %s records reason %q, want %q", s.Target, s.Reason, budget.ReasonBreakerTripped)
		}
	}
	for _, want := range []string{"suppressed auto-applies", budget.ReasonBreakerTripped.String()} {
		if !strings.Contains(p.text, want) {
			t.Errorf("the state summary does not report the suppression (%q missing):\n%s", want, p.text)
		}
	}
	// Suppressed is not the same as dropped: the proposal still goes to a human, which
	// is the posture "autonomy is off on this cluster" is supposed to mean.
	if p.cluster.Gate.Opened == 0 {
		t.Errorf("a suppressed auto-apply must still reach the human gate: gate report %+v", p.cluster.Gate)
	}
}

// --- (e) ------------------------------------------------------------------------------

// beforeAutonomy is the state of everything the unattended phases must not change,
// captured before any of them runs.
//
// Pods are compared by UID and Deployments by generation, for the reasons
// untouchedState gives: the seeded pods are unhealthy on purpose so their status — and
// their resourceVersion — moves throughout the run, while a UID moves only on a
// replacement and a generation only on a spec change.
type beforeAutonomy struct {
	podUIDs           map[string]string
	deployGenerations map[string]int64
}

// captureBeforeAutonomy records the pre-state of every object no unattended action is
// permitted to touch.
func captureBeforeAutonomy(t *testing.T, reader *kube.Client) beforeAutonomy {
	t.Helper()
	state := beforeAutonomy{
		podUIDs:           map[string]string{},
		deployGenerations: map[string]int64{},
	}
	for _, name := range []string{crashloopPod, pendingPod} {
		state.podUIDs[e2eNamespace+"/"+name] = string(readPod(t, reader, name).UID)
	}
	for _, ref := range []struct{ namespace, name string }{
		{e2eNamespace, badImageDeploy},
		{offLimitsNamespace, offLimitsDeploy},
	} {
		state.deployGenerations[ref.namespace+"/"+ref.name] = generationOf(t, reader, ref.namespace, ref.name)
	}
	return state
}

// assertNothingOutOfBoundsChanged is the in-process half of (e), and the half that does
// not depend on the apiserver audit log being readable.
func assertNothingOutOfBoundsChanged(t *testing.T, reader *kube.Client, before beforeAutonomy) {
	t.Helper()

	for key, uid := range before.podUIDs {
		name := strings.TrimPrefix(key, e2eNamespace+"/")
		pod := readPod(t, reader, name)
		if string(pod.UID) != uid {
			t.Errorf("OUT-OF-BOUNDS WRITE: pod %s was replaced (UID %s -> %s) while MaKlaude was acting unattended", key, uid, pod.UID)
		}
		if pod.DeletionTimestamp != nil {
			t.Errorf("OUT-OF-BOUNDS WRITE: pod %s is being deleted (deletionTimestamp %s)", key, pod.DeletionTimestamp)
		}
	}
	for key, generation := range before.deployGenerations {
		parts := strings.SplitN(key, "/", 2)
		if got := generationOf(t, reader, parts[0], parts[1]); got != generation {
			t.Errorf("OUT-OF-BOUNDS WRITE: deployment %s had its spec changed (generation %d -> %d); no rule covers it",
				key, generation, got)
		}
	}
}

// assertNoWriteOutsideAutonomyBounds is the apiserver's own account of (e): across
// every mutating request the audit log attributes to a MaKlaude identity, each one that
// LANDED names an object something in this suite authorized.
//
// It is the order-independent restatement of assertOnlyTheApprovedWriteLanded, and it
// has to be restated rather than reused because that assertion's shape — "exactly one
// landed write" — is true only at the point in the suite where it runs. By the time
// this file executes, four objects have legitimately been written: `wedged` (a human
// approved it in remediation_test.go), `stuck` (a human approved it through the shipped
// binary), `earned` (an earned rule auto-applied it here), and `neverready` (an earned
// rule auto-applied it here, and the failure consequence then rolled it back, so it
// carries TWO landed writes rather than one).
//
// What stays exactly as strict is the complement. `offlimits`, the crashloop pod, the
// pending pod and `badimage` appear in this suite's proposals and are covered by no
// authorization of any kind, so a landed write naming one of them is the failure this
// whole milestone is built to make impossible — and the observation identity must still
// have landed nothing at all, whatever it attempted.
//
// It degrades gracefully when the audit log is unset or unreadable (the apiserver writes
// it as root, and a CI runner may not be able to open it), because the object-state
// proofs above hold on their own. That is the posture the sibling assertions take, for
// the reason recorded in CLAUDE.md's testing learning: optional corroboration must warn
// and skip, never hard-fail, when the primary proof already holds.
func assertNoWriteOutsideAutonomyBounds(t *testing.T) {
	t.Helper()

	events, ok := readMutatingAudit(t)
	if !ok {
		return
	}

	// Every object a write may legitimately have landed on, and what authorized it.
	authorized := map[string]string{
		"deployments/" + e2eNamespace + "/" + wedgedDeploy:      "approved by a human in TestE2E_GatedRemediation",
		"deployments/" + e2eNamespace + "/" + stuckDeploy:       "approved by a human in TestE2E_BinaryTwoPassGatedRemediation",
		"deployments/" + autoNamespace + "/" + earnedDeploy:     "auto-applied under an earned rule in TestE2E_UnattendedAutonomy",
		"deployments/" + failNamespace + "/" + neverReadyDeploy: "auto-applied and then rolled back in TestE2E_UnattendedAutonomy",
	}
	// The objects that must never carry a landed write, stated positively so a typo in
	// the map above cannot quietly turn one of them into an allowed target.
	forbidden := map[string]bool{
		"deployments/" + offLimitsNamespace + "/" + offLimitsDeploy: true,
		"deployments/" + e2eNamespace + "/" + badImageDeploy:        true,
		"pods/" + e2eNamespace + "/" + crashloopPod:                 true,
		"pods/" + e2eNamespace + "/" + pendingPod:                   true,
	}
	// The single request whose dry-run marker rides in its body, which this cluster's
	// Metadata-level audit policy cannot see. It is covered by the pod's own UID being
	// unchanged above; see assertOnlyTheApprovedWriteLanded for the full argument.
	bodyPreviewedDelete := "pods/" + e2eNamespace + "/" + pendingPod

	landed := map[string]int{}
	for _, ev := range events {
		switch {
		case ev.user == observationUser:
			// Whatever it attempted, nothing it sent may have been accepted. The suite
			// deliberately provokes one refusal (TestE2E_ObservationIdentityCannotExecute),
			// so the property asserted here is acceptance, not attempts.
			if ev.accepted() {
				t.Errorf("ZERO-WRITES VIOLATION: the observation identity landed a mutating request: %s", ev)
			}

		case ev.previewed():
			// A dry run reaches admission control and changes nothing, so previews are
			// unrestricted here — every proposal this cycle put to a human was previewed,
			// including the ones aimed at objects no rule covers, and that is the gated
			// path working rather than a leak.

		case ev.verb == "delete" && ev.target() == bodyPreviewedDelete:
			// The body-marked dry-run delete. Covered by the pod read above.

		case !ev.accepted():
			// Reached the API server and was refused, so it changed nothing. It is still
			// worth failing on when it names an object nothing authorized: something tried
			// to write there and only the API server stopped it.
			if forbidden[ev.target()] {
				t.Errorf("OUT-OF-BOUNDS WRITE ATTEMPT: a mutating request was sent at an object no rule covers, and the API server refused it: %s", ev)
			}

		default:
			landed[ev.target()]++
			if forbidden[ev.target()] {
				t.Errorf("OUT-OF-BOUNDS WRITE: a mutating request LANDED on an object nothing authorized: %s", ev)
			}
			if _, allowed := authorized[ev.target()]; !allowed {
				t.Errorf("UNACCOUNTED WRITE: a mutating request landed on an object this suite never authorizes: %s", ev)
			}
			if ev.user != executorUser {
				t.Errorf("a landed write was made by %q, not the executor identity %q: %s", ev.user, executorUser, ev)
			}
		}
	}

	// The positive half. Two of the authorized objects were written by THIS test, and a
	// run in which they were not is a run where the auto-apply assertions passed against
	// a cluster nothing reached — the failure mode a ledger of absences cannot catch.
	for _, target := range []string{
		"deployments/" + autoNamespace + "/" + earnedDeploy,
		"deployments/" + failNamespace + "/" + neverReadyDeploy,
	} {
		if landed[target] == 0 {
			t.Errorf("no mutating request landed on %s, but the report says one was auto-applied there", target)
		}
	}
	t.Logf("audit log: landed writes by target %v — each one authorized, and nothing outside the bounds", landed)
}

// --- harness --------------------------------------------------------------------------

// unattendedPass is one completed [operate.Cycle] pass and everything the assertions
// read from it.
type unattendedPass struct {
	report    *operate.Report
	cluster   operate.ClusterReport
	disclosed *disclose.MemorySink
	text      string
}

// runUnattendedPass wires a production cycle with autonomy enabled and runs exactly one
// pass over the live cluster.
//
// Everything here is the shipped machinery. The cycle is [operate.NewForTest] rather
// than [operate.New] for one reason, and it is the SINKS rather than the autonomy: New
// wires autonomy from the environment (T7) but requires a LIVE disclosure trail to do
// it, because an unattended action whose only record dies with the process is what the
// milestone forbids — and this test needs an in-memory disclosure sink it can read back,
// plus an approval sink nobody can approve on. The client builder, the mutator builder,
// the gate, the runner, the disclosure trail and the budget are all the real ones, and
// [operate.New]'s own path over the same seam is covered by
// internal/operate/wire_test.go.
//
// The approval gate is real and NOBODY can approve on it: an in-memory sink with no
// decisions. That is what makes "the proposal went to a human" a checkable outcome
// rather than an accident of nothing being wired.
func runUnattendedPass(t *testing.T, reg *cluster.Registry, blast *budget.Budget, rules autonomy.Ruleset,
	oracle autonomy.TrustOracle, ledger operate.TrustRecorder, policy execute.Policy) unattendedPass {
	t.Helper()

	approvals := approve.NewMemorySink()
	approvals.SelfLogin = e2eSelfLogin
	gate := approve.NewGatekeeper(approvals, notify.NewNopNotifier(), approve.DefaultPolicy())

	cycle, err := operate.NewForTest(kube.ExecuteEnabled, kube.NewClient, newE2EMutator, gate,
		audit.NewTrail(), policy, false, time.Now)
	if err != nil {
		t.Fatalf("building the unattended cycle: %v", err)
	}
	cycle.UseBudget(blast)

	disclosed := disclose.NewMemorySink()
	trail, err := disclose.NewTrail(disclosed, notify.NewNopNotifier())
	if err != nil {
		t.Fatalf("building the disclosure trail: %v", err)
	}
	cycle.UseAutonomy(rules, oracle, trail, ledger)

	// The four-way wiring check, asserted rather than assumed. A cycle missing any one
	// of rules, oracle, budget or disclosure trail auto-applies nothing and says nothing
	// about why — so every assertion below it would pass for the wrong reason.
	if !cycle.Autonomous() {
		t.Fatal("the cycle was given rules, a trust oracle, a budget and a disclosure trail and still reports autonomy off")
	}

	report, err := cycle.Run(context.Background(), reg)
	if err != nil {
		t.Fatalf("running the unattended cycle: %v", err)
	}
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("rendering the state summary: %v", err)
	}

	p := unattendedPass{report: report, disclosed: disclosed, text: buf.String()}
	for _, cr := range report.Clusters {
		if cr.Cluster == executorClusterName {
			p.cluster = cr
		}
	}
	if p.cluster.Cluster == "" {
		t.Fatalf("the report has no section for cluster %q: %+v", executorClusterName, report.Clusters)
	}
	if p.cluster.Error != "" {
		t.Fatalf("the pass failed against cluster %q: %s", executorClusterName, p.cluster.Error)
	}
	t.Logf("pass over %s: %d proposal(s), %d auto-applied, %d opened at the gate",
		p.cluster.Cluster, len(p.cluster.Proposals), len(p.cluster.AutoApplied), p.cluster.Gate.Opened)
	return p
}

// newE2EMutator is the production write-client builder, spelled out here because
// operate's own is unexported. It is the same [kube.NewExecutor] call, so nothing about
// the write path is substituted for the test.
func newE2EMutator(h *cluster.Handle, mode kube.ExecuteMode) (execute.Mutator, error) {
	return kube.NewExecutor(h, mode)
}

// rollbackRule is the operator configuration under test: rollbacks, on this cluster, in
// these namespaces, reversible actions only.
//
// MaxReversibility is left at [remediate.ReversibilityReversible] — the zero value and
// the strictest setting — rather than widened to cover the operation under test. A
// revision rollback IS reversible, so the ceiling is not what permits it; writing a
// wider ceiling than the rule needs would quietly make this test unable to notice if
// the operation's classification ever changed.
func rollbackRule(name string, namespaces ...string) autonomy.Ruleset {
	return autonomy.Ruleset{{
		Name:             name,
		Clusters:         []string{executorClusterName},
		Namespaces:       namespaces,
		Operations:       []remediate.Operation{remediate.OpRollbackRevision},
		MaxReversibility: remediate.ReversibilityReversible,
	}}
}

// seededLedger returns a trust ledger holding exactly the history that earns autonomy
// for the rollback shape: [trust.PromotionThreshold] human-approved, converged
// executions and nothing else.
//
// Seeding is what the task asks for, and the entries are built through
// [trust.Ledger.Record] rather than by writing a file, so every one of them passes the
// ledger's own validation — including the rule that a human-approved entry MUST name
// the approval artifact behind it, which is the check that stops a hand-edited ledger
// from being a blank cheque.
//
// The live gated path writes exactly this kind of entry since issue #166's fix —
// operate.Cycle.recordGatedTrust projects every finished gated execution onto the
// ledger — and driving a shape from untrusted to trusted through real approvals, end
// to end in the live wiring, is proven by
// internal/operate's TestRun_ApprovalsAloneDriveAShapeFromUntrustedToTrusted. Seeding
// here is therefore a fixture convenience, not a workaround: this scenario is about
// what a trusted shape may DO unattended, and three real approval round-trips through
// the kind cluster would buy it nothing but wall-clock time.
func seededLedger(t *testing.T, fps fingerprints, targets ...string) *trust.Ledger {
	t.Helper()

	if len(targets) == 0 {
		t.Fatal("seededLedger was given no targets, so it would earn trust for nothing")
	}
	ledger := trust.NewMemory()
	base := time.Now().UTC().Add(-time.Hour)
	for n, target := range targets {
		subject := fps.subject(t, target)
		for i := range trust.PromotionThreshold {
			entry := trust.Entry{
				Key:         fmt.Sprintf("e2e-seeded-approval-%d-%d", n, i),
				Shape:       subject.Shape,
				Fingerprint: subject.Fingerprint,
				Authority:   audit.AuthorityHuman,
				Outcome:     trust.OutcomeConverged,
				At:          base.Add(time.Duration(n*trust.PromotionThreshold+i) * time.Minute),
				// Kept SHORT on purpose. The disclosure renders the trust citation
				// through [redact.String], whose last rule blanks any unbroken run of
				// 24+ characters from [A-Za-z0-9+/_-] as a high-entropy blob — and a
				// hyphenated ref is exactly that shape. At 25 characters
				// "e2e-approval-artifact-0-2" is redacted out of the artifact entirely,
				// which reads as "the citation lost its evidence" rather than as "the
				// fixture outgrew a redaction threshold". Keep this under 24.
				Ref: fmt.Sprintf("e2e-approval-%d-%d", n, i),
			}
			if err := ledger.Record(entry); err != nil {
				t.Fatalf("seeding trust entry %d for %s: %v", i, target, err)
			}
		}
		if !ledger.Trust(subject).Trusted {
			t.Fatalf("%d seeded human approvals did not earn trust for %s (%s): %s",
				trust.PromotionThreshold, target, subject.Fingerprint, ledger.Explain(subject))
		}
	}
	return ledger
}

// fingerprints maps a proposal target — "deployment/<namespace>/<name>" — to the
// [remediate.Fingerprint] the pipeline actually computed for the rollback of it.
//
// Since issue #167 trust is keyed on the fix rather than on the (cluster, operation)
// shape, so a phase that seeds a history has to seed it for a fingerprint the cycle
// will recompute and recognize. One seeded fingerprint no longer covers two objects,
// which is the whole point of the change and the reason the phases below name their
// targets explicitly.
type fingerprints map[string]remediate.Fingerprint

// subject builds the [autonomy.Subject] the trust ledger answers about for one target.
func (f fingerprints) subject(t *testing.T, target string) autonomy.Subject {
	t.Helper()

	fp, ok := f[target]
	if !ok {
		t.Fatalf("no rollback proposal was ever observed for %s, so there is no fingerprint to seed trust for; observed: %v",
			target, f)
	}
	return autonomy.Subject{
		Shape:       autonomy.Shape{Cluster: executorClusterName, Operation: remediate.OpRollbackRevision},
		Fingerprint: fp,
	}
}

// target renders the proposal target key for a Deployment.
func target(namespace, name string) string { return "deployment/" + namespace + "/" + name }

// discoverFingerprints runs one pass purely to read the fingerprints off the rollback
// proposals the pipeline actually makes.
//
// They are READ rather than reconstructed. Building a [remediate.Proposal] by hand here
// and hashing it would produce a fingerprint that differs from the real one the moment
// any fingerprinted field drifts — a changed precondition set, a different cause, a
// bumped [remediate.PlannerVersion] — and the failure would not look like a stale test.
// It would look like a policy bug: trust seeded, rule matching, and nothing auto-applied.
//
// The pass is safe to run first because it trusts nothing. Its ledger is empty, so every
// proposal gates and no write reaches any cluster, and it is given its own budget so it
// spends none of the shared blast allowance the breaker phases depend on.
func discoverFingerprints(t *testing.T, reg *cluster.Registry) fingerprints {
	t.Helper()

	scratch := budget.NewMemory(budget.Limits{
		PerClusterPerPass: budget.DefaultPerClusterPerPass,
		Cooldown:          budget.DefaultCooldown,
		FailureThreshold:  1,
	}, time.Now)
	ledger := trust.NewMemory()

	p := runUnattendedPass(t, reg, scratch, rollbackRule(earnedRule, autoNamespace), ledger, ledger,
		execute.Policy{ObserveWindow: unattendedObserveWindow, ObserveInterval: unattendedObserveInterval})

	if len(p.cluster.AutoApplied) != 0 {
		t.Fatalf("the discovery pass auto-applied %d action(s) against an empty ledger: %+v",
			len(p.cluster.AutoApplied), p.cluster.AutoApplied)
	}

	found := fingerprints{}
	for _, proposal := range p.cluster.Proposals {
		if proposal.Operation != remediate.OpRollbackRevision.String() {
			continue
		}
		if proposal.Fingerprint == "" {
			t.Fatalf("the pipeline proposed %s with an empty fingerprint; an empty fingerprint can never be trusted, so every seeded phase would gate",
				proposal.Target)
		}
		found[proposal.Target] = remediate.Fingerprint(proposal.Fingerprint)
	}
	t.Logf("discovered %d rollback fingerprint(s): %v", len(found), found)
	return found
}

// requireRollbackProposed fails the test unless the read-only pipeline actually proposed
// a rollback for the named Deployment on this pass.
//
// Every "nothing was auto-applied" assertion in this file depends on it. Without the
// guard, a seed that never wedged, a diagnosis that landed elsewhere, or a namespace
// typo would produce a pass with no proposals — and every safety assertion would pass,
// unanimously, against a cluster where MaKlaude had nothing to do.
func requireRollbackProposed(t *testing.T, p unattendedPass, namespace, name string) {
	t.Helper()

	want := "deployment/" + namespace + "/" + name
	for _, proposal := range p.cluster.Proposals {
		if proposal.Operation == string(remediate.OpRollbackRevision) && proposal.Target == want {
			return
		}
	}
	t.Fatalf("the pipeline proposed no %s for %s, so this phase would assert nothing. It proposed: %s. "+
		"Either the CI job did not wedge that Deployment onto an unpullable image, or its first revision is gone.",
		remediate.OpRollbackRevision, want, renderProposalReports(p.cluster.Proposals))
}

// assertDisclosureIsHonest checks the durable record of an action nobody approved.
//
// The artifact is the ENTIRE oversight surface for an unattended mutation — there is no
// approver to name and no decision to point at — so three things have to be true of it
// and each is a separate failure: it must say outright that no human reviewed it, it
// must carry the trust citation that stood in for one, and it must be labelled as
// applied so a reader can tell an action that ran from one that was only opened.
//
// The citation is checked in FRAGMENTS rather than verbatim against
// [operate.AutoApplyReport.Evidence], and the reason is worth recording so nobody
// tightens it back: the artifact passes through the redactor on its way out, whose
// high-entropy sweep rewrites any unbroken 24+ character run of [A-Za-z0-9+/_-] as
// `[REDACTED]`. Three tokens in the citation are that shape — the `cluster/operation`
// shape, the fingerprint's hex digest, and a long enough approval reference — so the
// rendered evidence reads "3 human-approved executions of this exact fix on [REDACTED]
// converged (3 required) ... fingerprint fp1:[REDACTED] ... (ref e2e-approval-0-2)".
// That is the same collapse #132 reported against Change.Scope.
//
// What survives is what makes the citation evidence rather than a claim: the counts,
// the threshold, the "this exact fix" scoping, and the artifact reference — the last
// only because [seededLedger] deliberately keeps its refs under the 24-character
// threshold. The shape itself is stated un-redacted in the artifact's own "Shape" row
// two lines above.
//
// The redacted FINGERPRINT is a real limitation rather than a property worth keeping:
// it is a hash precisely so it can be published, and blanking it costs an operator the
// one thing it is for — comparing two artifacts to see whether the fix changed. Filed
// separately; the fix belongs in the redactor, not in a test.
func assertDisclosureIsHonest(t *testing.T, sink *disclose.MemorySink, ref disclose.Ref) {
	t.Helper()

	view, ok := sink.Snapshot(ref)
	if !ok {
		t.Fatalf("no disclosure artifact %q on the trail", ref)
	}
	body := view.Body + "\n" + strings.Join(view.Comments, "\n")

	if !strings.Contains(body, "NO HUMAN APPROVED THIS ACTION") {
		t.Errorf("the disclosure for an unattended action does not say a human did not approve it:\n%s", body)
	}
	for _, want := range []string{
		// The evidence that stood in for an approver, and the numbers behind it.
		"Trust evidence",
		// Since issue #167 the citation has to say the approvals were of THIS fix, not
		// merely of this shape — that distinction is the authorization, so an artifact
		// that stated the weaker claim would be overstating what a human sanctioned.
		"human-approved executions of this exact fix",
		fmt.Sprintf("(%d required)", trust.PromotionThreshold),
		// The most recent approval artifact the ledger cited. It is one of the references
		// seededLedger wrote, so this also proves the citation was computed from the
		// history under test rather than rendered from a template.
		"e2e-approval-",
		// The distinction the docs task (#147) exists to keep straight, stated on the
		// artifact itself: an earned rule is not the blanket bypass.
		"MAKLAUDE_DANGEROUSLY_AUTO_APPROVE",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the disclosure is missing %q, so it does not carry the evidence that authorized the action:\n%s", want, body)
		}
	}
	if !view.HasLabel(disclose.AppliedLabel) {
		t.Errorf("the disclosure is missing %q, so a reader cannot tell that a real mutation landed: %v",
			disclose.AppliedLabel, view.Labels)
	}
	if !view.HasLabel(disclose.ManagedLabel) {
		t.Errorf("the disclosure is missing %q, so the revocation query would never find it: %v",
			disclose.ManagedLabel, view.Labels)
	}
}

// assertRolledBackToAGoodImage checks the thing a rollback is actually FOR: the workload
// is off the image that could not be pulled.
//
// It asserts the absence of the wedge marker rather than the presence of a literal tag,
// so the manifest can change its good image without this test being edited in lockstep —
// while still failing loudly if the "rollback" left the unpullable image in place, which
// is what a strategic-merge patch would silently produce.
func assertRolledBackToAGoodImage(t *testing.T, reader *kube.Client, namespace, name string) {
	t.Helper()
	dep := readDeploymentIn(t, reader, namespace, name)
	for _, c := range dep.Spec.Template.Spec.Containers {
		if strings.Contains(c.Image, wedgeImageMarker) {
			t.Errorf("deployment %s/%s container %q is still on the wedged image %q; the unattended rollback did not restore the previous pod template",
				namespace, name, c.Name, c.Image)
		}
	}
}

// assertDeploymentIsHealthy re-runs the read-only pipeline and requires the remediated
// Deployment to produce no actionable finding at all.
//
// It is the namespace-aware sibling of assertWedgedIsHealthy and exists for the same
// reason: execute's convergence check asks a narrow question it wrote itself, and
// detect.Analyze asks the question the rest of MaKlaude asks. An unattended action that
// satisfied the first and left the workload broken in some other way would pass the
// runner's check and fail this one — and with nobody watching, that gap is the whole
// risk.
func assertDeploymentIsHealthy(t *testing.T, collector *health.Collector, namespace, name string) {
	t.Helper()

	deadline := time.Now().Add(healthyDeadline)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		snap, err := collector.Collect(ctx)
		cancel()
		if err != nil {
			t.Fatalf("re-collecting cluster health after the unattended action: %v", err)
		}

		findings := actionableFindingsAbout(detect.Analyze(snap), namespace, name)
		dep, found := deploymentSignal(snap, namespace, name)
		healthy := found && dep.AvailableReplicas == dep.DesiredReplicas && dep.ReadyReplicas == dep.DesiredReplicas
		if healthy && len(findings) == 0 {
			t.Logf("deployment %s/%s is healthy after the unattended rollback: %d/%d ready and available, no findings",
				namespace, name, dep.ReadyReplicas, dep.DesiredReplicas)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s/%s did not read as healthy within %s of converging (found=%t, signal=%+v, findings=%+v)",
				namespace, name, healthyDeadline, found, dep, findings)
		}
		time.Sleep(healthyInterval)
	}
}

// actionableFindingsAbout returns the warning-or-worse findings naming an object that
// is, or is derived from, the named Deployment.
//
// It is findingsAbout with the namespace as a parameter. Info-severity findings are
// excluded for the reason that function records: Kubernetes keeps an event for an hour
// after the object it concerns is gone, so the failed revision's "Failed to pull image"
// events outlive the pods they were about. That is the cluster narrating what already
// happened, not a workload that is still broken.
func actionableFindingsAbout(findings []detect.Finding, namespace, name string) []detect.Finding {
	var out []detect.Finding
	for _, f := range findings {
		if f.Object.Namespace != namespace || f.Severity < detect.SeverityWarning {
			continue
		}
		if f.Object.Name == name || strings.HasPrefix(f.Object.Name, name+"-") {
			out = append(out, f)
		}
	}
	return out
}

// readDeploymentIn fetches one Deployment from any namespace through a read-only client.
// It is readDeployment with the namespace as a parameter; the Milestone 5 scenarios each
// live in their own.
func readDeploymentIn(t *testing.T, c *kube.Client, namespace, name string) *appsv1.Deployment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	deploys, err := c.ListDeployments(ctx, namespace)
	if err != nil {
		t.Fatalf("listing deployments in %s: %v", namespace, err)
	}
	for i := range deploys {
		if deploys[i].Name == name {
			return &deploys[i]
		}
	}
	t.Fatalf("seeded deployment %s/%s not found (was test/e2e/manifests/autonomy-scenarios.yaml applied?)", namespace, name)
	return nil
}

// generationOf reads a Deployment's generation, which moves only on a SPEC change and is
// therefore the API server's own evidence that something wrote to the object.
func generationOf(t *testing.T, c *kube.Client, namespace, name string) int64 {
	t.Helper()
	return readDeploymentIn(t, c, namespace, name).Generation
}
