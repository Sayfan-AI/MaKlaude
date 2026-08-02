//go:build e2e

// This file is Milestone 5 T1's closing proof, and the one thing no other test in this
// repository does: it drives the SHIPPED BINARY through a real human-gated remediation
// against a real cluster, in the shape production actually runs it — two separate
// processes, with the approval arriving between them.
//
// # What the sibling test already proves, and what it cannot
//
// remediation_test.go wires internal/{health,detect,correlate,diagnose,remediate,
// approve,execute,audit} together in process and carries one approval through to one
// change. That establishes the LAYERS are correct. It cannot establish that `maklaude
// remediate` reaches them, because it never runs the binary — and until PR #148 the
// binary could not reach them at all, which is precisely the gap T1 exists to close.
//
// It also cannot establish the gate's defining property. A gate whose decision lives in
// the process that asked for it is not a gate: real consent is given by somebody else,
// later, and must be found by a run that did not ask. That is two processes and durable
// state, and an in-process approve.MemorySink is neither.
//
// # The shape
//
// Six things, in order, all against one seeded fault (`stuck`, a Deployment wedged on
// an unpullable image — see manifests/stuck-deploy.yaml for why it is a second fault
// rather than a shared one). Three of the five passes are negative, which is the
// deliberate ratio: a test that only proves approved actions execute is the half of the
// test that would still pass with the gate deleted.
//
//	Pass 1 — `maklaude remediate` with the kill switch ARMED (MAKLAUDE_EXECUTE_MODE
//	         =enabled). It must propose, open an artifact, and change NOTHING. An
//	         artifact with no decision on it is never consent.
//	Pass 2 — NEGATIVE CONTROL: self-approval. The approval label is recorded attributed
//	         to MAKLAUDE_GITHUB_SELF_LOGIN — MaKlaude approving itself. The run must
//	         refuse it and change nothing. #124 closed this hole with unit coverage;
//	         nothing until now proved it closed through the binary.
//	Pass 3 — NEGATIVE CONTROL: no decision at all, over an artifact that already exists.
//	         Distinct from pass 1, which had no artifact to recover a decision from.
//	Pass 4 — NEGATIVE CONTROL: an approval whose `labeled` event names no actor. Refused
//	         as unattributable — `isSelfActor("")` is false, so pass 2's check does not
//	         cover this one.
//	Pass 5 — the approval label attributed to a login that is NOT the self login: a
//	         person. Exactly one mutation lands, the cluster converges on an
//	         independent re-scan, and the trail reads proposed → approved → executed →
//	         verified with the approver named.
//	Ledger — the apiserver's own audit log: across everything this suite sent, the only
//	         requests that LANDED are the two approved rollbacks, one per test.
//
// # Why the approval is injected past the HTTP surface
//
// Every request the binary can make is attributed to the self login by the stub, which
// is what makes pass 2 a real control. githubStub.decideAs writes the human's label
// event directly, because that is the one thing MaKlaude has no way to forge — the same
// reason remediation_test.go's simulated approval goes through MemorySink.Decide rather
// than any code path MaKlaude can reach.
//
// # Ordering
//
// Go runs a package's tests in source order across files sorted by name, so this file
// runs last. That matters for exactly one assertion — the audit ledger, which counts
// every landed write in the run and therefore must see the sibling test's. Nothing else
// here depends on it: the fault is this test's own, and the objects the other tests
// assert on are never touched.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/operate"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	// stuckDeploy is this test's own fixable fault. See manifests/stuck-deploy.yaml
	// for why it is not called `wedged-binary`.
	stuckDeploy = "stuck"

	// The stub trail's coordinates. The repo does not exist and cannot: the whole
	// point is that MAKLAUDE_GITHUB_API sends every request to loopback instead.
	binaryTrailOwner = "maklaude-e2e"
	binaryTrailRepo  = "approvals"

	// A token the stub checks for verbatim. It authenticates nothing — it exists so
	// that a binary which forgot to send its credential fails here rather than
	// silently passing against a stub that never looked.
	binaryTrailToken = "stub-trail-token-not-a-credential" //nolint:gosec // not a credential; see above.

	// binarySelfLogin is the account MaKlaude acts as on the trail, and pass 2's
	// forged approver. The stub attributes every label change made THROUGH THE API to
	// it, so it is the identity a self-approval would genuinely arrive under.
	binarySelfLogin = "maklaude-e2e-binary-bot"

	// binaryApprover is the person. It is provably not binarySelfLogin, which is what
	// makes pass 5 an approval and pass 2 a forgery.
	binaryApprover = "maklaude-e2e-binary-operator"

	// binaryCluster is the registry name in the config file handed to the binary.
	binaryCluster = "maklaude-e2e-binary"

	// binaryApprovalCycles bounds how many times pass 5 is re-driven when the target
	// moves underneath it. A wedged Deployment's resourceVersion legitimately advances
	// between the preview a human saw and the moment the write would be sent, and every
	// layer is built to notice that and abandon cleanly; re-proposing against the state
	// that exists now is what production does with such an abort. Only a CLEAN abort
	// continues — see runApprovedPass.
	binaryApprovalCycles = 4

	// binaryPassTimeout bounds one `maklaude remediate` process. It must exceed the
	// execution layer's convergence window (execute.DefaultObserveWindow, 90s) with
	// room for the read pipeline either side, or the harness would kill a run that was
	// working.
	binaryPassTimeout = 6 * time.Minute

	// selfApprovalRefusalMarker is the distinguishing phrase approve.RefusalComment
	// writes for approve.ReasonSelfApproval. It is a substring of that sentence rather
	// than the whole of it so a reworded explanation does not fail the test, but it is
	// specific enough that no other refusal reason produces it.
	selfApprovalRefusalMarker = "applied by MaKlaude's own account"

	// unattributedRefusalMarker is the same idea for approve.ReasonUnattributedApproval.
	// It is what makes pass 4 a control rather than an observation: disqualify() checks
	// attribution BEFORE drift (reconcile.go:209 precedes :218), so an unattributed
	// approval that was refused for drift instead would mean the attribution check no
	// longer runs — and a refusal count alone cannot tell those apart.
	unattributedRefusalMarker = "cannot attribute to a person"
)

// TestE2E_BinaryTwoPassGatedRemediation drives `maklaude remediate` through the full
// gated cycle across five separate processes: propose, refuse a self-approval, refuse
// to act on an undecided artifact, refuse an unattributable approval, then execute
// exactly what a person approved.
func TestE2E_BinaryTwoPassGatedRemediation(t *testing.T) {
	bin := buildMaklaudeBinary(t)
	stub := newGitHubStub(t, binaryTrailOwner, binaryTrailRepo, binaryTrailToken, binarySelfLogin)
	cfgPath := writeBinaryRegistryConfig(t)
	passEnv := binaryPassEnv(stub)

	// Reads go through the ordinary read-only client on the executor identity, for the
	// reason remediation_test.go gives: observing the write path with the write path's
	// own client would make every before/after comparison depend on the thing under
	// test.
	reader, err := kube.NewClient(executorHandle(t, buildExecutorRegistry(t)))
	if err != nil {
		t.Fatalf("building the read-only client for the executor identity: %v", err)
	}
	collector := health.NewCollector(reader)

	before := stuckSpecState(t, reader)
	t.Logf("before any pass: %s/%s is at generation %d on image %q",
		e2eNamespace, stuckDeploy, before.generation, before.image)

	// previewed accumulates every target the binary itself said it proposed, across
	// every pass. The audit ledger allows a server-side preview only at an object the
	// binary reported proposing, which keeps the allowlist derived from the run rather
	// than from a hand-maintained list that would rot the first time a rule changes.
	previewed := map[string]bool{}

	// --- Pass 1: propose, and change nothing. ---
	p1 := runRemediatePass(t, bin, cfgPath, passEnv, "pass 1 (propose)")
	assertLiveArmedPass(t, p1, "pass 1")
	collectProposedTargets(p1, previewed)
	identity := assertRollbackProposed(t, p1)
	assertNothingExecuted(t, p1, "pass 1")
	if p1.Clusters[0].Gate.Opened < 1 {
		t.Fatalf("pass 1 opened %d approval artifacts, want at least 1 — MaKlaude proposed a rollback and must have asked",
			p1.Clusters[0].Gate.Opened)
	}
	assertStuckUnchanged(t, reader, before, "pass 1 proposed an action; nothing was approved, so nothing may have run")

	artifact := soleArtifactFor(t, stub, identity)
	t.Logf("pass 1 opened artifact #%d on the durable trail for proposal %s", artifact, identity)

	// --- Pass 2: the negative control. MaKlaude approves itself; the gate refuses. ---
	stub.decideAs(t, artifact, approve.ApprovedLabel, binarySelfLogin)
	p2 := runRemediatePass(t, bin, cfgPath, passEnv, "pass 2 (self-approval)")
	assertLiveArmedPass(t, p2, "pass 2")
	collectProposedTargets(p2, previewed)
	assertNothingExecuted(t, p2, "pass 2")
	if p2.Clusters[0].Gate.Authorized != 0 {
		t.Fatalf("SELF-APPROVAL HOLE: pass 2 authorized %d action(s) from a label MaKlaude applied to its own artifact",
			p2.Clusters[0].Gate.Authorized)
	}
	if p2.Clusters[0].Gate.Refused < 1 {
		t.Errorf("pass 2 recorded %d refusal(s), want at least 1 — a self-applied approval must be refused explicitly, not merely ignored",
			p2.Clusters[0].Gate.Refused)
	}
	assertStuckUnchanged(t, reader, before,
		"pass 2's approval was applied by MaKlaude itself; the self-approval refusal must have stopped it")

	// The refusal is also visible on the artifact: the gate withdraws the label it
	// would not honor, which is what lets a person re-apply one that counts.
	afterP2 := stub.snapshotIssue(t, artifact)
	if afterP2.hasLabel(approve.ApprovedLabel) {
		t.Errorf("after refusing the self-approval the gate left %q on artifact #%d; a refused decision must be withdrawn",
			approve.ApprovedLabel, artifact)
	}

	// And it must have been refused FOR THIS REASON. A count of refusals is not the
	// control: a wedged Deployment's resourceVersion advances on its own, so a pass can
	// legitimately refuse for drift instead — and a test that accepted either would go on
	// passing if the self-approval check were deleted outright. The refusal comment is
	// where the reason is on the record, so that is what is read.
	if !strings.Contains(strings.Join(afterP2.comments, "\n"), selfApprovalRefusalMarker) {
		t.Errorf("SELF-APPROVAL HOLE: pass 2's refusal on artifact #%d never says the approval was MaKlaude's own; "+
			"it was refused for some other reason and the self-approval check is unproven.\n%s",
			artifact, strings.Join(afterP2.comments, "\n---\n"))
	}
	t.Logf("pass 2: the self-applied approval was refused as a self-approval and withdrawn, and the cluster is untouched")

	// --- Pass 3: the artifact exists, is pending, and carries no decision. Nothing runs. ---
	//
	// This is the control that would survive the gate being deleted outright, and it is
	// NOT the same assertion pass 1 makes. Pass 1 had no artifact, so "nothing executed"
	// there is satisfied by the ReasonNewProposal branch, which never consults a label.
	// Here the artifact is on the trail and pass 2 just withdrew its label, so the gate
	// has to re-derive "undecided" from the labels-plus-events it recovers — the same
	// code path that says "approved", reached with the opposite answer. A sink that
	// mistook the existence of a pending artifact for consent would pass pass 1 and
	// fail only here.
	p3 := runRemediatePass(t, bin, cfgPath, passEnv, "pass 3 (pending, undecided)")
	assertLiveArmedPass(t, p3, "pass 3")
	collectProposedTargets(p3, previewed)
	assertNothingExecuted(t, p3, "pass 3")
	if p3.Clusters[0].Gate.Authorized != 0 {
		t.Fatalf("GATE BYPASS: pass 3 authorized %d action(s) on an artifact carrying no decision label at all",
			p3.Clusters[0].Gate.Authorized)
	}
	if afterP3 := stub.snapshotIssue(t, artifact); afterP3.hasLabel(approve.ApprovedLabel) {
		t.Errorf("pass 3 put %q back on artifact #%d; nobody applied it", approve.ApprovedLabel, artifact)
	}
	assertStuckUnchanged(t, reader, before, "pass 3 ran against an undecided artifact; an undecided proposal authorizes nothing")
	t.Logf("pass 3: an existing pending artifact with no decision authorized nothing, and the cluster is untouched")

	// --- Pass 4: an approval nobody can be named for. The gate refuses. ---
	//
	// The label is real and its timestamp is fine; only the identity is gone. That is a
	// state GitHub genuinely serves (see decideAsNobody), and it is the one an
	// attribution check exists for: `isSelfActor("")` is false, so the self-approval
	// check of pass 2 does not catch this one.
	stub.decideAsNobody(t, artifact, approve.ApprovedLabel)
	p4 := runRemediatePass(t, bin, cfgPath, passEnv, "pass 4 (unattributed approval)")
	assertLiveArmedPass(t, p4, "pass 4")
	collectProposedTargets(p4, previewed)
	assertNothingExecuted(t, p4, "pass 4")
	if p4.Clusters[0].Gate.Authorized != 0 {
		t.Fatalf("UNATTRIBUTED APPROVAL HONORED: pass 4 authorized %d action(s) from a label with no identifiable approver",
			p4.Clusters[0].Gate.Authorized)
	}
	if p4.Clusters[0].Gate.Refused < 1 {
		t.Errorf("pass 4 recorded %d refusal(s), want at least 1 — an unattributable approval must be refused explicitly, not merely ignored",
			p4.Clusters[0].Gate.Refused)
	}
	afterP4 := stub.snapshotIssue(t, artifact)
	if afterP4.hasLabel(approve.ApprovedLabel) {
		t.Errorf("after refusing the unattributed approval the gate left %q on artifact #%d; a refused decision must be withdrawn",
			approve.ApprovedLabel, artifact)
	}
	if !strings.Contains(strings.Join(afterP4.comments, "\n"), unattributedRefusalMarker) {
		t.Errorf("UNATTRIBUTED-APPROVAL HOLE: pass 4's refusal on artifact #%d never says the approver could not be named; "+
			"it was refused for some other reason and the attribution check is unproven.\n%s",
			artifact, strings.Join(afterP4.comments, "\n---\n"))
	}
	assertStuckUnchanged(t, reader, before,
		"pass 4's approval named nobody; the attribution refusal must have stopped it")
	t.Logf("pass 4: an approval with no identifiable actor was refused as unattributable and withdrawn, and the cluster is untouched")

	// --- Pass 5: a person approves, and exactly that action runs. ---
	done := runApprovedPass(t, bin, cfgPath, passEnv, stub, identity, artifact, previewed)

	if !done.exec.Executed {
		t.Fatalf("pass 5's execution did not apply: %+v", done.exec)
	}
	if done.exec.Approver != binaryApprover {
		t.Errorf("the executed action names approver %q, want %q — the trail must carry the person who decided",
			done.exec.Approver, binaryApprover)
	}
	if done.exec.Authority != approve.AuthorityHuman.String() {
		t.Errorf("the executed action records authority %q, want %q — nothing here waived the requirement for a person",
			done.exec.Authority, approve.AuthorityHuman.String())
	}
	if done.exec.Attempts != 1 {
		t.Errorf("the executed action produced %d mutating request(s), want exactly 1 — one approval authorizes one action",
			done.exec.Attempts)
	}
	if done.report.Clusters[0].Gate.Authorized != 1 {
		t.Errorf("pass 5 authorized %d action(s), want exactly 1", done.report.Clusters[0].Gate.Authorized)
	}
	// "we looked and it had not happened yet" is a verdict the independent re-scan below
	// settles, and a slow CI runner is entitled to produce it. "we could not look at all"
	// is not a verdict — it means the binary executed a mutation and then lost sight of
	// the cluster, which is the one convergence outcome an operator cannot act on.
	if done.exec.Convergence == execute.ConvergenceUnobservable.String() {
		t.Errorf("the binary could not observe the cluster at all after executing: %+v", done.exec)
	}
	t.Logf("pass 5: %s on %s executed under %s authority from %q — convergence %q",
		done.exec.Operation, done.exec.Target, done.exec.Authority, done.exec.Approver,
		done.exec.Convergence)

	// --- The cluster really converged, checked independently of the runner's verdict. ---
	assertStuckWasRolledBack(t, reader, before)
	assertStuckIsHealthy(t, collector)

	// --- The durable trail reads as the whole lifecycle. ---
	assertTrailShowsLifecycle(t, stub, artifact, done)

	// --- Nothing unapproved ever landed, per the apiserver's own record. ---
	assertOnlyTheTwoApprovedWritesLanded(t, reader, previewed)

	if n := stub.unauthorizedCount(); n != 0 {
		t.Errorf("%d request(s) reached the trail without the expected bearer token; the live GitHub client path was not exercised as configured", n)
	}
}

// --- Driving the binary. ---

// buildMaklaudeBinary compiles the shipped CLI and returns its path.
//
// It builds the real command with no test-only build tags, so what runs is what ships.
// Building here rather than depending on a prebuilt artifact keeps the test honest
// about which source it exercised: a stale binary on the runner would otherwise let a
// regression in cmd/maklaude pass.
func buildMaklaudeBinary(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "maklaude")
	cmd := exec.Command("go", "build", "-o", out, "github.com/Sayfan-AI/MaKlaude/cmd/maklaude")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the maklaude binary: %v\n%s", err, combined)
	}
	return out
}

// writeBinaryRegistryConfig writes the cluster registry file the binary is pointed at.
//
// It names the EXECUTOR kubeconfig, because one handle has to serve the whole flow: the
// proposal, the permission slip and the write client must all name the same cluster or
// execute.Runner refuses with ErrClusterMismatch, which is the same constraint
// remediation_test.go works under.
func writeBinaryRegistryConfig(t *testing.T) string {
	t.Helper()
	kubeconfig := env(t, "MAKLAUDE_E2E_EXECUTOR_KUBECONFIG")
	contextName := env(t, "MAKLAUDE_E2E_CONTEXT")

	path := filepath.Join(t.TempDir(), "clusters.yaml")
	body := fmt.Sprintf("clusters:\n  - name: %s\n    kubeconfig: %s\n    context: %s\n",
		binaryCluster, kubeconfig, contextName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the registry config for the binary: %v", err)
	}
	return path
}

// binaryPassEnv is the environment every pass runs under.
//
// It is built from scratch rather than appended to os.Environ(), and that is a
// correctness requirement rather than hygiene. The CI job deliberately leaves
// MAKLAUDE_GITHUB_* unset so the rest of the suite degrades to an in-memory trail; an
// inherited MAKLAUDE_DANGEROUSLY_AUTO_APPROVE or a stray MAKLAUDE_EXECUTE_MODE would
// change what this test proves without changing what it asserts. Only PATH (the Go
// toolchain and any exec'd helper) and HOME (client-go's cache paths) are carried over.
func binaryPassEnv(stub *githubStub) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),

		// The kill switch, armed. Without this the cycle proposes and stops, and every
		// pass below would pass for the wrong reason.
		operate.ExecuteModeEnv + "=" + kube.ExecuteEnabled.String(),

		// The live trail, pointed at loopback. APIBase is an arbitrary REST base URL —
		// see githubstub_test.go's header for why this is the real client path and not
		// a degraded one.
		"MAKLAUDE_GITHUB_REPO=" + binaryTrailOwner + "/" + binaryTrailRepo,
		"MAKLAUDE_GITHUB_TOKEN=" + binaryTrailToken,
		"MAKLAUDE_GITHUB_API=" + stub.apiBase(),
		approve.SelfLoginEnv + "=" + binarySelfLogin,
	}
}

// runRemediatePass runs one `maklaude remediate` process and decodes its JSON report.
func runRemediatePass(t *testing.T, bin, cfgPath string, passEnv []string, label string) operate.Report {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), binaryPassTimeout)
	defer cancel()

	started := time.Now()
	cmd := exec.CommandContext(ctx, bin, "remediate", "--config", cfgPath, "--json")
	cmd.Env = passEnv
	stdout, err := cmd.Output()
	elapsed := time.Since(started)

	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s: `maklaude remediate` failed after %s: %v\nstdout:\n%s\nstderr:\n%s",
			label, elapsed, err, stdout, stderr)
	}

	var report operate.Report
	if derr := json.Unmarshal(stdout, &report); derr != nil {
		t.Fatalf("%s: decoding the binary's JSON report: %v\nstdout:\n%s", label, derr, stdout)
	}
	if len(report.Clusters) != 1 {
		t.Fatalf("%s: the report covers %d clusters, want exactly 1", label, len(report.Clusters))
	}
	if e := report.Clusters[0].Error; e != "" {
		t.Fatalf("%s: the cycle reported a cluster-level failure: %s", label, e)
	}
	t.Logf("%s completed in %s: %d proposal(s), gate %+v, %d execution(s)",
		label, elapsed, len(report.Clusters[0].Proposals), report.Clusters[0].Gate,
		len(report.Clusters[0].Executions))
	return report
}

// approvedPass is the pass that actually executed, plus the report it came from.
type approvedPass struct {
	report operate.Report
	exec   operate.ExecutionReport
}

// runApprovedPass re-applies a human approval and runs the binary until the approved
// action executes, or until binaryApprovalCycles is exhausted.
//
// The loop is the recovery path, not a flake suppressor. A wedged Deployment's
// resourceVersion advances on its own as the kubelet reports the failing pull, so the
// preview a human's approval was given against can legitimately be stale by the time
// the next pass runs — the gate calls that drift, refuses, withdraws the label, and
// re-asks against the state that exists now. Re-approving and re-running is exactly
// what a person watching the artifact would do. Only that outcome continues: a pass
// that executed something and failed, or authorized without executing, fails the test
// immediately, because a loop that swallowed the difference between "stale" and
// "refused" would hide what this test exists to catch.
func runApprovedPass(t *testing.T, bin, cfgPath string, passEnv []string, stub *githubStub,
	identity string, artifact int, previewed map[string]bool) approvedPass {
	t.Helper()

	// lastExplanation is the gate's OWN account of why the most recent cycle did not
	// authorize. It is carried out of the loop because the report says only that a
	// refusal happened, while the reason is posted on the artifact — so a loop that
	// recovers without reading it can do nothing but guess, and the guess is what gets
	// printed when the loop finally gives up.
	lastExplanation := "no explanation was recorded on the artifact"

	for cycle := 1; cycle <= binaryApprovalCycles; cycle++ {
		stub.decideAs(t, artifact, approve.ApprovedLabel, binaryApprover)

		label := fmt.Sprintf("pass 5 (human approval, cycle %d/%d)", cycle, binaryApprovalCycles)
		report := runRemediatePass(t, bin, cfgPath, passEnv, label)
		assertLiveArmedPass(t, report, label)
		collectProposedTargets(report, previewed)
		cr := report.Clusters[0]

		var mine []operate.ExecutionReport
		for _, e := range cr.Executions {
			if e.Identity == identity {
				mine = append(mine, e)
			}
		}
		for _, e := range cr.Executions {
			if e.Identity != identity {
				t.Fatalf("%s executed something nobody in this test approved: %+v", label, e)
			}
		}

		switch {
		case len(mine) == 1 && mine[0].Executed:
			return approvedPass{report: report, exec: mine[0]}

		case len(mine) == 1 && mine[0].CleanAbort:
			// The target moved between the preview and the send. Re-propose and re-approve.
			lastExplanation = "the executor aborted cleanly: " + mine[0].Failure
			t.Logf("%s aborted cleanly (%s); the target moved, so re-approving against the state that exists now",
				label, mine[0].Failure)

		case len(mine) == 1:
			t.Fatalf("%s: the approved action neither executed nor aborted cleanly: %+v", label, mine[0])

		case len(mine) > 1:
			t.Fatalf("APPROVAL-GATE VIOLATION: %s produced %d executions for one approval: %+v", label, len(mine), mine)

		case cr.Gate.Authorized > 0:
			t.Fatalf("%s authorized %d action(s) and executed none; an authorization that is not acted on is a permission left open",
				label, cr.Gate.Authorized)

		default:
			// Refused or refreshed without authorizing: something was caught one layer
			// earlier, at the gate rather than at the executor. Same recovery — but the
			// reason is READ off the artifact rather than assumed. Drift is only one of the
			// things this branch can mean, and a refusal the harness itself causes (a stale
			// simulated approval, say) looks identical from the report alone.
			lastExplanation = latestComment(t, stub, artifact)
			t.Logf("%s did not authorize (gate %+v), so re-approving. The gate's own explanation:\n%s",
				label, cr.Gate, lastExplanation)
		}

		// The refusal withdrew the label; the next cycle re-applies it. Re-confirm the
		// artifact is still the one being managed, so a withdrawn-and-reopened artifact
		// does not leave this loop labelling a closed issue forever.
		if snap := stub.snapshotIssue(t, artifact); snap.state != "open" {
			artifact = soleArtifactFor(t, stub, identity)
			t.Logf("the previous artifact was closed; the live one for %s is now #%d", identity, artifact)
		}
	}

	t.Fatalf("the approved rollback of %s/%s did not execute within %d cycles. The last cycle's reason was:\n%s",
		e2eNamespace, stuckDeploy, binaryApprovalCycles, lastExplanation)
	return approvedPass{}
}

// latestComment returns the most recent comment on an artifact, which after a
// non-authorizing pass is the refusal the gate just posted.
func latestComment(t *testing.T, stub *githubStub, artifact int) string {
	t.Helper()
	snap := stub.snapshotIssue(t, artifact)
	if len(snap.comments) == 0 {
		return "(the artifact carries no comments)"
	}
	return snap.comments[len(snap.comments)-1]
}

// --- Assertions on a pass. ---

// assertLiveArmedPass checks the two postures every pass must run under. Both are
// configuration this test sets, and both are worth re-reading from the report: a pass
// that quietly ran disabled, or against an in-memory trail, would satisfy most of the
// assertions below for entirely the wrong reason.
func assertLiveArmedPass(t *testing.T, report operate.Report, label string) {
	t.Helper()
	if report.Mode != kube.ExecuteEnabled.String() {
		t.Fatalf("%s ran under mode %q, want %q — the kill switch was not armed, so nothing below proves anything",
			label, report.Mode, kube.ExecuteEnabled)
	}
	if !report.Live {
		t.Fatalf("%s reports a non-live approval trail; MAKLAUDE_GITHUB_* did not reach a real GitHubSink, so the two-pass gate was never exercised",
			label)
	}
}

// assertRollbackProposed checks the pipeline arrived at a rollback of this test's fault
// and returns its identity — the key every later assertion matches on.
func assertRollbackProposed(t *testing.T, report operate.Report) string {
	t.Helper()
	want := "deployment/" + e2eNamespace + "/" + stuckDeploy
	for _, p := range report.Clusters[0].Proposals {
		if p.Operation == string(remediate.OpRollbackRevision) && p.Target == want {
			return p.Identity
		}
	}
	t.Fatalf("the binary proposed no %s for %s. It proposed: %s. "+
		"Either the CI job did not wedge the Deployment onto an unpullable image, or revision 1's ReplicaSet is gone.",
		remediate.OpRollbackRevision, want, renderProposalReports(report.Clusters[0].Proposals))
	return ""
}

func renderProposalReports(proposals []operate.ProposalReport) string {
	if len(proposals) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(proposals))
	for _, p := range proposals {
		parts = append(parts, p.Operation+" on "+p.Target)
	}
	return strings.Join(parts, "; ")
}

// assertNothingExecuted requires a pass to have applied nothing at all.
func assertNothingExecuted(t *testing.T, report operate.Report, label string) {
	t.Helper()
	for _, e := range report.Clusters[0].Executions {
		if e.Executed {
			t.Fatalf("APPROVAL-GATE VIOLATION: %s executed an action nobody approved: %+v", label, e)
		}
	}
}

// collectProposedTargets records every target the binary said it proposed. Each one is
// an object it will have sent a server-side preview at, which is what the audit ledger
// needs to be able to explain them.
func collectProposedTargets(report operate.Report, into map[string]bool) {
	for _, p := range report.Clusters[0].Proposals {
		into[auditTargetFor(p.Target)] = true
	}
}

// auditTargetFor converts a remediate.Target string into the resource/namespace/name
// shape mutatingAuditEvent.target() renders, so the two can be compared. The apiserver
// names resources in the plural.
//
// The cluster-scoped case is the one worth spelling out: remediate.Target.String()
// omits the namespace segment entirely for a Node ("node/ip-10-0-0-1"), while the audit
// rendering keeps the empty slot ("nodes//ip-10-0-0-1"). Collapsing them would silently
// drop a cordon from the preview allowlist and report it as an unapproved write.
func auditTargetFor(target string) string {
	switch parts := strings.Split(target, "/"); len(parts) {
	case 2:
		return parts[0] + "s//" + parts[1]
	case 3:
		return parts[0] + "s/" + parts[1] + "/" + parts[2]
	default:
		return target
	}
}

// soleArtifactFor finds the one open artifact carrying a proposal's identity marker.
//
// It matches on the identity rather than on the target's name because a single object
// can attract more than one proposal, and two artifacts about the same Deployment would
// make "the artifact" ambiguous in exactly the situation where being wrong is worst.
func soleArtifactFor(t *testing.T, stub *githubStub, identity string) int {
	t.Helper()
	// The closing delimiter is part of the needle so one identity cannot match another's
	// marker by being a prefix of it.
	found := stub.openIssuesMentioning("maklaude:proposal=" + identity + " -->")
	if len(found) != 1 {
		t.Fatalf("the trail holds %d open artifacts for proposal %s, want exactly 1: %v", len(found), identity, found)
	}
	return found[0]
}

// --- Assertions on the cluster. ---

// stuckSpec is the Deployment state a write would necessarily change. generation is the
// sharp signal: the apiserver bumps it on any SPEC change and never on a status update,
// so it distinguishes "MaKlaude patched this" from "the kubelet reported another failed
// pull" — which a resourceVersion comparison cannot.
type stuckSpec struct {
	generation int64
	image      string
}

func stuckSpecState(t *testing.T, reader *kube.Client) stuckSpec {
	t.Helper()
	dep := readDeployment(t, reader, stuckDeploy)
	return stuckSpec{generation: dep.Generation, image: containerImage(t, dep)}
}

func containerImage(t *testing.T, dep *appsv1.Deployment) string {
	t.Helper()
	if len(dep.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("deployment %s/%s has %d containers, want 1 (the seed defines one)",
			e2eNamespace, dep.Name, len(dep.Spec.Template.Spec.Containers))
	}
	return dep.Spec.Template.Spec.Containers[0].Image
}

// assertStuckUnchanged is the load-bearing negative assertion of passes 1 and 2: the
// binary looked, proposed, asked, and left the cluster exactly as it found it.
func assertStuckUnchanged(t *testing.T, reader *kube.Client, before stuckSpec, why string) {
	t.Helper()
	now := stuckSpecState(t, reader)
	if now.generation != before.generation || now.image != before.image {
		t.Fatalf("UNAPPROVED WRITE: %s/%s moved from generation %d image %q to generation %d image %q — %s",
			e2eNamespace, stuckDeploy, before.generation, before.image, now.generation, now.image, why)
	}
	if !strings.Contains(now.image, wedgeImageMarker) {
		t.Fatalf("%s/%s is no longer on the wedged image (%q); the seeded fault is gone before anything approved it",
			e2eNamespace, stuckDeploy, now.image)
	}
}

// assertStuckWasRolledBack checks the approved action did what it said: the spec moved,
// and it moved back to an image that is not the wedge.
func assertStuckWasRolledBack(t *testing.T, reader *kube.Client, before stuckSpec) {
	t.Helper()
	now := stuckSpecState(t, reader)
	if now.generation <= before.generation {
		t.Errorf("%s/%s is still at generation %d after the approved rollback; a rollback patches the spec",
			e2eNamespace, stuckDeploy, now.generation)
	}
	if strings.Contains(now.image, wedgeImageMarker) {
		t.Fatalf("%s/%s is still on the wedged image %q after the approved rollback",
			e2eNamespace, stuckDeploy, now.image)
	}
	t.Logf("%s/%s rolled from generation %d image %q to generation %d image %q",
		e2eNamespace, stuckDeploy, before.generation, before.image, now.generation, now.image)
}

// assertStuckIsHealthy re-scans the cluster through the full read pipeline and requires
// the Deployment to be genuinely healthy with no actionable findings.
//
// It is the INDEPENDENT half of the convergence proof. The runner's own verdict comes
// from the observation window it opened around its own write, so taking it as the
// answer would be the executor marking its own homework; this goes back through
// health.Collect and detect.Analyze, the same instruments that reported the fault.
func assertStuckIsHealthy(t *testing.T, collector *health.Collector) {
	t.Helper()

	deadline := time.Now().Add(healthyDeadline)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		snap, err := collector.Collect(ctx)
		cancel()
		if err != nil {
			t.Fatalf("re-collecting cluster health after the binary's remediation: %v", err)
		}

		last := findingsAbout(detect.Analyze(snap), stuckDeploy)
		dep, found := deploymentSignal(snap, e2eNamespace, stuckDeploy)
		healthy := found && dep.AvailableReplicas == dep.DesiredReplicas && dep.ReadyReplicas == dep.DesiredReplicas
		if healthy && len(last) == 0 {
			t.Logf("deployment %s/%s is healthy after the binary's approved rollback: %d/%d ready and available, no findings",
				e2eNamespace, stuckDeploy, dep.ReadyReplicas, dep.DesiredReplicas)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s/%s did not read as healthy within %s of the approved rollback (found=%t, signal=%+v, findings=%+v)",
				e2eNamespace, stuckDeploy, healthyDeadline, found, dep, last)
		}
		time.Sleep(healthyInterval)
	}
}

// --- Assertions on the durable trail. ---

// assertTrailShowsLifecycle checks the artifact a person actually reads carries the
// whole story.
//
// This is the one record that outlives the run — the in-process audit.Trail dies with
// each pass, and there were three of them — so "the lifecycle is on the record" has to
// be true here or it is true nowhere.
func assertTrailShowsLifecycle(t *testing.T, stub *githubStub, artifact int, done approvedPass) {
	t.Helper()
	snap := stub.snapshotIssue(t, artifact)

	if !snap.hasLabel(approve.ExecutedLabel) {
		t.Errorf("artifact #%d does not carry %q after the action ran; a later pass would re-ask about work already done",
			artifact, approve.ExecutedLabel)
	}

	joined := strings.Join(snap.comments, "\n---\n")
	for _, want := range []string{
		binaryApprover,        // who decided
		done.exec.Target,      // what was done, to what
		done.exec.Convergence, // and what watching it afterwards concluded
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("artifact #%d's trail never mentions %q; the durable record is incomplete.\n%s", artifact, want, joined)
		}
	}

	// The approval that counted must be attributed to the person, not to MaKlaude. The
	// events endpoint is where that attribution lives, and it is the single fact the
	// whole gate rests on.
	var approvedBy []string
	for _, ev := range snap.events {
		if ev.kind == "labeled" && ev.label == approve.ApprovedLabel {
			approvedBy = append(approvedBy, ev.actor)
		}
	}
	if len(approvedBy) == 0 || approvedBy[len(approvedBy)-1] != binaryApprover {
		t.Errorf("the standing approval on artifact #%d is attributed to %v, want the last one to be %q",
			artifact, approvedBy, binaryApprover)
	}
	t.Logf("artifact #%d carries %d comment(s) and %d label event(s), approved by %v",
		artifact, len(snap.comments), len(snap.events), approvedBy)
}

// --- The apiserver's own ledger. ---

// assertOnlyTheTwoApprovedWritesLanded is this suite's final accounting: across
// everything the apiserver recorded, the only MaKlaude requests that CHANGED anything
// are the two approved rollbacks — remediation_test.go's `wedged` and this test's
// `stuck`.
//
// It is the same instrument assertOnlyTheApprovedWriteLanded uses and the same
// classification, widened for the fact that a second test now legitimately writes. Read
// that function's doc for the full reasoning behind each class; the differences here
// are two:
//
//   - The preview allowlist is DERIVED from what the binary reported proposing, rather
//     than enumerated. The binary previews everything it proposes across the whole
//     cluster, which is more objects than the in-process test touches and is a set that
//     changes whenever a proposal rule does. A hand-written list would either rot or be
//     widened until it stopped excluding anything.
//   - Pod DELETEs are excepted by the same argument as before — a DELETE's dry-run
//     marker rides in its body, which this cluster's audit policy deliberately does not
//     record — and covered by the same instrument: every excepted pod is read back
//     here, and a real deletion survives none of it.
//
// It degrades gracefully when the audit log is unset or unreadable, because the
// object-state proofs above hold on their own.
func assertOnlyTheTwoApprovedWritesLanded(t *testing.T, reader *kube.Client, previewed map[string]bool) {
	t.Helper()

	events, ok := readMutatingAudit(t)
	if !ok {
		return
	}

	// Every object this suite deliberately aims a server-side PREVIEW at: the two the
	// in-process tests use, plus everything the binary reported proposing.
	previewTargets := map[string]bool{
		"deployments/" + e2eNamespace + "/" + badImageDeploy: true, // executor_test.go's dry-run patch
		"deployments/" + e2eNamespace + "/" + wedgedDeploy:   true, // remediation_test.go's preview
	}
	for target := range previewed {
		previewTargets[target] = true
	}

	// The requests whose dry-run marker the audit log physically cannot see: pod
	// DELETEs. executor_test.go previews one at `pending`; the binary previews one at
	// every pod it proposes deleting. Each is covered by the pod still being there.
	bodyPreviewedDeletes := map[string]bool{"pods/" + e2eNamespace + "/" + pendingPod: true}
	for target := range previewed {
		if strings.HasPrefix(target, "pods/") {
			bodyPreviewedDeletes[target] = true
		}
	}
	// The other half of that exception, and the reason it is an exception rather than a
	// hole: each excepted pod must still be there. readPod fails the test outright if it
	// is not, which is the whole assertion.
	for target := range bodyPreviewedDeletes {
		parts := strings.Split(target, "/")
		if len(parts) != 3 || parts[1] != e2eNamespace {
			t.Fatalf("a pod DELETE was excepted for %q, which is not a pod in %s; the exception must never widen beyond this suite's own seeds",
				target, e2eNamespace)
		}
		pod := readPod(t, reader, parts[2])
		if pod.DeletionTimestamp != nil {
			t.Errorf("pod %s is terminating; a previewed DELETE must leave it untouched", target)
		}
	}

	// The one mutating request the observation identity is expected to have ATTEMPTED:
	// TestE2E_ObservationIdentityCannotExecute's probe, which exists to be refused.
	rbacProbeTarget := "deployments/" + e2eNamespace + "/" + badImageDeploy

	approvedTargets := map[string]bool{
		"deployments/" + e2eNamespace + "/" + wedgedDeploy: true, // remediation_test.go's approved rollback
		"deployments/" + e2eNamespace + "/" + stuckDeploy:  true, // this test's
	}

	var landed []mutatingAuditEvent
	for _, ev := range events {
		switch {
		case ev.user == observationUser:
			if ev.code != http.StatusForbidden || ev.target() != rbacProbeTarget {
				t.Errorf("ZERO-WRITES VIOLATION: the observation identity issued a mutating request: %s", ev)
			}

		case ev.previewed():
			if !previewTargets[ev.target()] {
				t.Errorf("UNAPPROVED WRITE: the executor previewed an object nothing in this suite aims at: %s", ev)
			}

		case ev.verb == "delete" && bodyPreviewedDeletes[ev.target()]:
			// Covered by the pod being read back below.

		case !ev.accepted():
			if !previewTargets[ev.target()] && !approvedTargets[ev.target()] {
				t.Errorf("UNAPPROVED WRITE: the executor sent a mutating request at an object nothing approved, and the API server rejected it: %s", ev)
			}

		default:
			landed = append(landed, ev)
		}
	}

	got := map[string]bool{}
	for _, ev := range landed {
		if ev.user != executorUser {
			t.Errorf("a landed write was made by %q, not the executor identity %q: %s", ev.user, executorUser, ev)
		}
		if ev.verb != "patch" {
			t.Errorf("a landed write is a %s, and both approved actions are rollback patches: %s", ev.verb, ev)
		}
		if !approvedTargets[ev.target()] {
			t.Errorf("APPROVAL-GATE VIOLATION: a mutating request landed on %s, which nobody approved: %s", ev.target(), ev)
		}
		if got[ev.target()] {
			t.Errorf("APPROVAL-GATE VIOLATION: more than one mutating request landed on %s: %s", ev.target(), ev)
		}
		got[ev.target()] = true
	}
	if len(landed) != len(approvedTargets) {
		t.Fatalf("APPROVAL-GATE VIOLATION: %d mutating requests landed on the cluster, want exactly %d (one per approved rollback): %v",
			len(landed), len(approvedTargets), landed)
	}
	t.Logf("audit log: exactly %d mutating requests landed, and they are the %s — %v",
		len(landed), "two approved rollbacks", sortedTargets(got))
}

func sortedTargets(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
