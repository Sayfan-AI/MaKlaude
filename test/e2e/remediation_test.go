//go:build e2e

// This file is Milestone 4's end-to-end proof: MaKlaude diagnoses a real fault on a
// real cluster, asks a human, waits, and — only once someone says yes — changes the
// cluster and watches it come back healthy.
//
// It is the first test in this repository that lets a mutation land. e2e_test.go
// proves the read path writes nothing; executor_test.go proves the write path can
// send a genuine mutating request that the API server admits and discards. Neither
// answers the question this milestone actually rests on: when a human DOES approve,
// does the whole chain — collect, detect, correlate, diagnose, propose, preview,
// gate, execute, verify, audit — carry that one decision through to exactly one
// change and no others?
//
// # Why the fault is a bad rollout
//
// The seeded scenario is a Deployment rolled forward onto an unpullable image
// (wedged-deploy.yaml plus the CI step that wedges it), and the fix is a rollback to
// the revision that was demonstrably running before. It is the one fault in the seed
// set that is genuinely FIXABLE by an action in MaKlaude's catalog: a restart of a
// crashlooping pod would crashloop again, a cordon needs a NotReady node this
// single-node cluster cannot produce on demand, and the badimage Deployment was born
// broken so no previous revision survives to roll back to. A convergence assertion is
// only worth writing against a fault whose remedy actually works.
//
// It is also the first live exercise of kube.Executor.RollbackDeploymentToRevision —
// the JSON-patch `replace` of /spec/template that shipped in PR #129 with unit
// coverage only. A strategic-merge patch cannot express a rollback (it merges
// containers by name, so anything the bad revision ADDED would survive the "restore"),
// and whether the replace behaves against a real Deployment controller is not
// something a stub can answer.
//
// # The four things it asserts, and why the last one is the hard one
//
//	(a) the cluster CONVERGES — both the runner's own bounded-window verdict and an
//	    independent re-scan through detect, so "it worked" is not the executor
//	    marking its own homework;
//	(b) the AUDIT TRAIL is complete and names the approver — proposed → approved →
//	    executed → verified, with human authority, on the record;
//	(c) the COMMS ARTIFACT shows that whole lifecycle, because the in-process trail
//	    dies with the process and the artifact is what an operator actually reads;
//	(d) NO UNAPPROVED WRITE ever reached the cluster.
//
// (d) is where the work is. The M1 assertion — zero mutating verbs attributed to the
// observation ServiceAccount — still holds verbatim and is re-run here AFTER the
// approved mutation lands. What is new is the executor identity, which by design now
// has one real write to its name, so "zero" is the wrong shape for it. Instead every
// mutating request the apiserver audit log attributes to the executor is classified,
// and exactly one is allowed to have landed: assertOnlyTheApprovedWriteLanded carries
// the full reasoning, including the one request whose dry-run marker the audit log
// physically cannot see and what is used to cover it instead.
//
// # Ordering and independence
//
// Go runs a package's tests in source order across files sorted by name, so this file
// runs after e2e_test.go and executor_test.go and before slack_e2e_test.go. Nothing
// here depends on that: the ledger in (d) tolerates the sibling tests' preview
// requests being absent, and the objects those tests assert on are re-read here and
// asserted UNCHANGED rather than assumed untouched.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	// wedgedDeploy is the Deployment the CI job rolls forward onto an unpullable
	// image — the fixable fault this test remediates. See manifests/wedged-deploy.yaml.
	wedgedDeploy = "wedged"

	// e2eApprover is the login the simulated human approval is attributed to. It is a
	// value the gate cannot produce for itself: an approval MaKlaude applied to its own
	// artifact is refused (approve.ReasonSelfApproval), which is why the simulation goes
	// through MemorySink.Decide — the seam that records an actor the way a real GitHub
	// label event does — rather than through any code path MaKlaude can reach.
	e2eApprover = "maklaude-e2e-operator"

	// e2eSelfLogin is the account the gate treats as MaKlaude itself. It is set (rather
	// than left empty) so the self-approval refusal is genuinely armed while this test
	// runs: an approver of e2eApprover is provably not this login.
	e2eSelfLogin = "maklaude-e2e-bot"

	// remediationCycles bounds how many times the whole propose → preview → approve →
	// execute sequence is re-driven when the target moves underneath it. See
	// driveApprovedRollback for why a re-drive is the system working rather than a
	// retry papering over a race.
	remediationCycles = 4

	// The bounded observation window for the approved action. It is longer than the
	// shipped default because a CI runner's disk and a cold kubelet are slower than a
	// developer's laptop, and shorter than the test's own patience; the interval is
	// tightened so a fast convergence is reported promptly rather than on a five-second
	// beat.
	remediationObserveWindow   = 150 * time.Second
	remediationObserveInterval = 3 * time.Second

	// healthyDeadline bounds the independent post-execution health re-scan. Convergence
	// has already been observed by the time it runs, so this is a short confirmation
	// window rather than a wait.
	healthyDeadline = 60 * time.Second
	healthyInterval = 3 * time.Second
)

// The two ServiceAccounts the apiserver audit log may attribute a MaKlaude request
// to. They are spelled out rather than derived because the whole point of reading the
// audit log is to check MaKlaude against an external record, and a username computed
// from the same constants the client authenticates with would check nothing.
const (
	observationUser = "system:serviceaccount:maklaude:maklaude"
	executorUser    = "system:serviceaccount:maklaude:maklaude-executor"
)

// TestE2E_GatedRemediation drives one approved remediation end to end against the live
// kind cluster and asserts (a) convergence, (b) a complete audit trail naming the
// approver, (c) a comms artifact carrying the whole lifecycle, and (d) that the single
// approved write is the only one that ever landed.
func TestE2E_GatedRemediation(t *testing.T) {
	h := executorHandle(t, buildExecutorRegistry(t))

	// Reads go through the ordinary read-only client and its transport guard, even
	// though the credential behind it can write. Observing the write path with the
	// write path's own client would make every before/after comparison depend on the
	// thing under test.
	reader, err := kube.NewClient(h)
	if err != nil {
		t.Fatalf("building the read-only client for the executor identity: %v", err)
	}
	collector := health.NewCollector(reader)

	// One cluster handle serves the whole flow, so the proposal, the permission slip,
	// and the write client all name the same cluster — the three-way check
	// execute.Runner performs before it sends anything. Splitting observation onto the
	// read-only kubeconfig would name a DIFFERENT registered cluster and the runner
	// would (correctly) refuse with ErrClusterMismatch.
	executor, err := kube.NewExecutor(h, kube.ExecuteEnabled)
	if err != nil {
		t.Fatalf("building the write-enabled executor: %v", err)
	}
	previewer, err := kube.NewExecutor(h, kube.ExecuteDryRun)
	if err != nil {
		t.Fatalf("building the preview-only executor: %v", err)
	}

	sink := approve.NewMemorySink()
	sink.SelfLogin = e2eSelfLogin
	gate := approve.NewGatekeeper(sink, notify.NewNopNotifier(), approve.DefaultPolicy())

	trail := audit.NewTrail()
	runner, err := execute.New(executor, collector, gate, trail, execute.Policy{
		ObserveWindow:   remediationObserveWindow,
		ObserveInterval: remediationObserveInterval,
	})
	if err != nil {
		t.Fatalf("building the execution runner: %v", err)
	}

	// What the approved action must NOT touch, captured before anything is sent.
	untouched := captureUntouched(t, reader)

	// --- The gated cycle: propose, preview, approve, execute. ---
	done := driveApprovedRollback(t, collector, previewer, gate, sink, runner)
	t.Logf("approved remediation: rolled %s back to revision %d on artifact %q — %s",
		done.proposal.Target.String(), done.revision, done.ref, done.report)

	// --- (a) The cluster converged. ---
	assertConverged(t, done.report)
	assertWedgedIsHealthy(t, collector)

	// --- (b) The audit trail is complete and names the approver. ---
	assertAuditTrailComplete(t, trail, done)

	// --- (c) The comms artifact shows the full lifecycle. ---
	assertArtifactShowsLifecycle(t, sink, done)

	// --- (d) Only the approved write ever landed. ---
	assertUntouched(t, reader, untouched)
	assertTargetWasMutated(t, reader, untouched)
	// The M1 assertion, re-run AFTER a real mutation: the observation identity's
	// zero-writes guarantee is not weakened by the executor identity gaining one.
	assertNoMutatingAudit(t)
	assertOnlyTheApprovedWriteLanded(t, "deployments/"+e2eNamespace+"/"+wedgedDeploy)
}

// remediation is everything one completed gated cycle produced, carried together so
// the assertions can cross-check the proposal, the permission slip, the artifact, and
// the report against each other rather than each against itself.
type remediation struct {
	proposal remediate.Proposal
	auth     *approve.Authorization
	ref      approve.ActionRef
	revision int64
	report   execute.Report
}

// driveApprovedRollback runs the full gated sequence and returns the cycle that
// actually executed.
//
// It re-drives the whole sequence — re-observe, re-propose, re-preview, re-approve —
// when the target moves underneath it, and that is the system working rather than a
// retry hiding a race. The seeded Deployment is unhealthy on purpose, so its
// resourceVersion can legitimately advance between the snapshot a proposal was
// computed from and the moment the request would be sent; every layer here is built to
// notice that and abandon cleanly (approve.ReasonDrift, execute.FailureDrifted,
// kube.ErrPreconditionConflict), and re-proposing against the state that exists now is
// precisely what the production flow does with such an abort. So the loop exercises the
// real recovery path instead of suppressing a flake — and note the asymmetry: only a
// CLEAN abort continues. Any other failure fails the test immediately, because a retry
// loop that swallowed the difference between "stale" and "refused" would hide the
// failures this test exists to catch.
func driveApprovedRollback(t *testing.T, collector *health.Collector, previewer *kube.Executor,
	gate *approve.Gatekeeper, sink *approve.MemorySink, runner *execute.Runner) remediation {
	t.Helper()
	ctx := context.Background()

	var lastAbort string
	for cycle := 1; cycle <= remediationCycles; cycle++ {
		proposal := rollbackProposal(t, collector)
		revision := approvedRevision(t, proposal)

		// The dry run a human is shown BEFORE deciding: the identical request, sent for
		// real, admitted by real admission controllers, and discarded by the API server.
		preview, ok := previewRollback(t, previewer, proposal, revision)
		if !ok {
			lastAbort = fmt.Sprintf("cycle %d: the preview hit a stale resourceVersion", cycle)
			t.Log(lastAbort + "; re-observing")
			continue
		}
		req := approve.Request{Proposal: proposal, Preview: preview}

		// Pass one: MaKlaude asks. On the first cycle nothing may be authorized — an
		// artifact with no decision on it is never consent — and that is asserted rather
		// than assumed, because "the gate opened a request and authorized it in the same
		// breath" is the failure that would make every other assertion here meaningless.
		//
		// The assertion is scoped to the first cycle because a later one legitimately
		// starts from an artifact a previous cycle already carried a decision on. What the
		// gate does with a stale decision is its own business (refuse, refresh, or honor if
		// nothing moved), and re-asserting "nobody is authorized yet" against that state
		// would be asserting something untrue.
		asked, err := gate.Reconcile(ctx, []approve.Request{req})
		if err != nil {
			t.Fatalf("cycle %d: opening the approval request: %v", cycle, err)
		}
		if cycle == 1 && len(asked.Authorized) != 0 {
			t.Fatalf("the gate issued %d authorization(s) on the pass that merely ASKED; nothing may be authorized before a human acts",
				len(asked.Authorized))
		}
		ref := soleOpenArtifact(t, sink)

		// The simulated human. MemorySink.Decide records an actor and an instant exactly
		// as a GitHub label event does, and the instant is placed a second in the future
		// so it is unambiguously AFTER the preview the body displays — the ordering the
		// gate checks (approve.ReasonApprovalPredatesPreview) and the one a wall clock
		// truncated to RFC3339 seconds could otherwise make a coin flip.
		if err := sink.Decide(ref, approve.ApprovedLabel, e2eApprover, time.Now().UTC().Add(time.Second)); err != nil {
			t.Fatalf("cycle %d: recording the simulated human approval on %q: %v", cycle, ref, err)
		}

		// Pass two: the gate honors the decision, or refuses it because the world moved.
		honored, err := gate.Reconcile(ctx, []approve.Request{req})
		if err != nil {
			t.Fatalf("cycle %d: honoring the approval: %v", cycle, err)
		}
		if len(honored.Authorized) == 0 {
			lastAbort = fmt.Sprintf("cycle %d: the gate refused the approval (%s)", cycle, honored)
			t.Log(lastAbort + "; re-observing")
			continue
		}
		if len(honored.Authorized) != 1 {
			t.Fatalf("cycle %d: the gate issued %d authorizations for one proposal, want 1", cycle, len(honored.Authorized))
		}
		auth := honored.Authorized[0]
		assertHumanAuthority(t, auth, proposal)

		report, err := runner.Execute(ctx, auth, proposal)
		if report.CleanAbort() {
			lastAbort = fmt.Sprintf("cycle %d: the runner abandoned the action cleanly (%s): %s", cycle, report.Failure, report.Error)
			t.Log(lastAbort + "; re-observing")
			continue
		}
		if err != nil {
			t.Fatalf("cycle %d: executing the approved rollback: %v (report: %s)", cycle, err, report)
		}
		return remediation{proposal: proposal, auth: auth, ref: ref, revision: revision, report: report}
	}

	t.Fatalf("no gated remediation completed within %d cycles; last abort: %s", remediationCycles, lastAbort)
	return remediation{}
}

// rollbackProposal drives the real read-only pipeline over the live cluster and
// returns the rollback it proposes for the wedged Deployment.
//
// Every stage is the production one — health.Collector, detect.Analyze,
// correlate.Correlate, diagnose.Diagnose, remediate.Hypotheses — because the claim
// being tested is that a proposal ARRIVES from a diagnosis, not that a proposal can be
// constructed. A hand-built remediate.Proposal would authorize and execute exactly the
// same way while proving nothing about the four layers underneath it.
func rollbackProposal(t *testing.T, collector *health.Collector) remediate.Proposal {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	snap, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("collecting cluster health: %v", err)
	}
	if !snap.Reachability.Reachable {
		t.Fatalf("cluster unreachable during collection: %s", snap.Reachability.Error)
	}

	findings := detect.Analyze(snap)
	var hypotheses []diagnose.Hypothesis
	for _, incident := range correlate.Correlate(snap, findings) {
		hypotheses = append(hypotheses, diagnose.Diagnose(snap, incident)...)
	}

	proposals := remediate.Hypotheses(snap, hypotheses)
	for _, p := range proposals {
		if p.Operation == remediate.OpRollbackRevision &&
			p.Target.Kind == "deployment" && p.Target.Namespace == e2eNamespace && p.Target.Name == wedgedDeploy {
			return p
		}
	}
	t.Fatalf("the pipeline proposed no %s for deployment %s/%s. It proposed: %s. "+
		"Either the CI job did not wedge the Deployment onto an unpullable image, or revision 1's ReplicaSet is gone.",
		remediate.OpRollbackRevision, e2eNamespace, wedgedDeploy, renderProposals(proposals))
	return remediate.Proposal{}
}

// renderProposals lists what WAS proposed, for the failure message above. A bare
// "no rollback proposed" would leave a reader unable to tell an unwedged Deployment
// from a diagnosis that landed on a different cause.
func renderProposals(proposals []remediate.Proposal) string {
	if len(proposals) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(proposals))
	for _, p := range proposals {
		parts = append(parts, string(p.Operation)+" on "+p.Target.String())
	}
	return strings.Join(parts, "; ")
}

// approvedRevision reads the revision the proposal (and therefore the artifact a human
// reads) names, from the same precondition the execution layer resolves it from. The
// test derives it the same way production does rather than re-deriving "the previous
// revision" from a live read, which would let this test approve one revision and
// execute another without noticing.
func approvedRevision(t *testing.T, p remediate.Proposal) int64 {
	t.Helper()
	for _, pc := range p.Preconditions {
		if pc.Kind != remediate.PreconditionRevisionExists {
			continue
		}
		revision, err := strconv.ParseInt(strings.TrimSpace(pc.Expect), 10, 64)
		if err != nil || revision <= 0 {
			t.Fatalf("the proposal's %s precondition names %q, which is not a revision", pc.Kind, pc.Expect)
		}
		return revision
	}
	t.Fatalf("the proposal for %s carries no %s precondition, so nothing records which revision a human would be approving",
		p.Target.String(), remediate.PreconditionRevisionExists)
	return 0
}

// previewRollback sends the approved action as a server-side dry run and renders the
// result as the evidence the approval artifact displays.
//
// ok is false for the two responses that mean "the world moved, ask again" — a stale
// resourceVersion, or a revision Kubernetes pruned while we were looking — and the
// caller re-observes. Any other error fails the test: a preview the API server rejects
// on its merits is a real problem with the action, and the gate would (correctly)
// refuse to authorize it, so continuing would only turn a clear failure into an
// exhausted retry budget.
func previewRollback(t *testing.T, previewer *kube.Executor, p remediate.Proposal, revision int64) (approve.Preview, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	out, err := previewer.RollbackDeploymentToRevision(ctx, p.Target.Namespace, p.Target.Name, revision, p.Target.ResourceVersion)
	switch {
	case errors.Is(err, kube.ErrPreconditionConflict), errors.Is(err, kube.ErrRevisionNotFound):
		return approve.Preview{}, false
	case err != nil:
		t.Fatalf("the dry-run preview of %s to revision %d was rejected: %v", p.Target.String(), revision, err)
	case out == nil:
		t.Fatalf("the preview returned neither an outcome nor an error")
	case !out.DryRun:
		t.Fatalf("PREVIEW VIOLATION: the preview-only executor reported a REAL mutation of %s: %+v", p.Target.String(), out)
	}

	return approve.Preview{
		Performed: true,
		Summary: fmt.Sprintf("The API server accepted a dryRun=All rollback of %s to revision %d under scope `%s`, conditioned on resourceVersion %s. Nothing was applied.",
			p.Target.String(), revision, out.Scope, out.ResourceVersion),
	}, true
}

// soleOpenArtifact returns the reference of the one open approval artifact, failing if
// there is any other number of them. More than one would mean one action had collected
// two chances to be approved, which is the duplicate the gate's own reconciliation is
// built to collapse.
func soleOpenArtifact(t *testing.T, sink *approve.MemorySink) approve.ActionRef {
	t.Helper()
	open, err := sink.ListOpen(context.Background())
	if err != nil {
		t.Fatalf("listing open approval artifacts: %v", err)
	}
	if len(open) != 1 {
		t.Fatalf("expected exactly 1 open approval artifact, got %d: %+v", len(open), open)
	}
	return open[0].Ref
}

// assertHumanAuthority checks the permission slip before anything is executed on it.
//
// The authority field is the assertion that matters. approve.AuthorityPolicy is a valid
// slip too — the autonomous-mode bypass issues one — and an authority mix-up would let
// this test claim it proved a human-gated remediation while actually proving the
// bypass. The gate cannot be handed a forged slip (Authorization has no exported
// constructor), so what is checked here is which KIND of authority it issued.
func assertHumanAuthority(t *testing.T, auth *approve.Authorization, p remediate.Proposal) {
	t.Helper()
	if !auth.Valid() {
		t.Fatalf("the gate returned an invalid authorization")
	}
	if auth.Authority() != approve.AuthorityHuman {
		t.Fatalf("authorization authority = %s, want %s — this test must prove a HUMAN-gated remediation, not a policy-waived one",
			auth.Authority(), approve.AuthorityHuman)
	}
	if auth.Approver() != e2eApprover {
		t.Errorf("authorization approver = %q, want %q", auth.Approver(), e2eApprover)
	}
	if !auth.Matches(p) {
		t.Fatalf("the permission slip covers %s on %s, not the proposal %s on %s",
			auth.Operation(), auth.Target().String(), p.Operation, p.Target.String())
	}
	if auth.ApprovedAt().IsZero() {
		t.Errorf("a human-authority slip carries no approval instant")
	}
}

// assertConverged checks the runner's own account of the action: a real mutation, sent
// once, recorded, and observed to take effect.
func assertConverged(t *testing.T, rep execute.Report) {
	t.Helper()
	if !rep.Executed {
		t.Fatalf("the approved action did not execute: %s (%s)", rep.Failure, rep.Error)
	}
	if rep.DryRun {
		t.Fatalf("the approved action reported itself a preview; the write path was not in enabled mode")
	}
	if rep.Failure != execute.FailureNone {
		t.Fatalf("the approved action terminated with %s: %s", rep.Failure, rep.Error)
	}
	if rep.Attempts != 1 {
		t.Errorf("the action produced %d mutating requests, want exactly 1 — an approval authorizes one action", rep.Attempts)
	}
	if !rep.Recorded {
		t.Errorf("the execution was not recorded on the approval trail; nothing durably prevents a second one")
	}
	if rep.Convergence != execute.ConvergenceConverged {
		t.Fatalf("convergence = %s after %s: %s", rep.Convergence, rep.ObservedFor.Round(time.Second), rep.ConvergenceDetail)
	}
	if !rep.PreState.Captured {
		t.Errorf("no pre-state was captured for the mutated object")
	}
	t.Logf("converged in %s: %s", rep.ObservedFor.Round(time.Second), rep.ConvergenceDetail)
}

// assertWedgedIsHealthy re-runs the read-only pipeline and requires that the remediated
// Deployment produces no finding at all.
//
// It is deliberately a SECOND opinion rather than a restatement of the runner's
// verdict. execute's convergence check asks a narrow question it wrote itself ("is
// there a newer revision, and do the replica counts agree?"); detect.Analyze asks the
// question the rest of MaKlaude asks ("is anything wrong here?"). A rollback that
// satisfied the first and left the workload in some other broken state would pass the
// runner's check and fail this one, which is the whole reason to look twice.
//
// It polls, because convergence and a fresh list are two different reads and the pod
// the old revision left behind can take a moment to disappear from the second.
func assertWedgedIsHealthy(t *testing.T, collector *health.Collector) {
	t.Helper()

	deadline := time.Now().Add(healthyDeadline)
	var last []detect.Finding
	for {
		ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
		snap, err := collector.Collect(ctx)
		cancel()
		if err != nil {
			t.Fatalf("re-collecting cluster health after the remediation: %v", err)
		}

		last = findingsAbout(detect.Analyze(snap), wedgedDeploy)
		dep, found := deploymentSignal(snap, e2eNamespace, wedgedDeploy)
		healthy := found && dep.AvailableReplicas == dep.DesiredReplicas && dep.ReadyReplicas == dep.DesiredReplicas
		if healthy && len(last) == 0 {
			t.Logf("deployment %s/%s is healthy after the approved rollback: %d/%d ready and available, no findings",
				e2eNamespace, wedgedDeploy, dep.ReadyReplicas, dep.DesiredReplicas)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s/%s did not read as healthy within %s of converging (found=%t, signal=%+v, findings=%+v)",
				e2eNamespace, wedgedDeploy, healthyDeadline, found, dep, last)
		}
		time.Sleep(healthyInterval)
	}
}

// findingsAbout returns the actionable findings naming an object that is, or is
// derived from, the remediated Deployment.
//
// The prefix match is what catches the Deployment's ReplicaSets and pods, whose names
// carry a pod-template hash that is not known ahead of time — and those are exactly the
// objects a half-finished rollback would leave broken.
//
// Info-severity findings are deliberately excluded, and the exclusion is about what
// this assertion means rather than about making it pass. detect surfaces recent warning
// EVENTS as info findings, and Kubernetes keeps an event for an hour after the object
// it concerns is gone — so the failed revision's "Failed to pull image" events outlive
// the pods they were about and would be reported for the rest of the run. That is the
// cluster narrating what already happened, not a workload that is still broken, and
// detect classifies it as info for exactly that reason. Anything at warning or above is
// a current problem and fails this check.
func findingsAbout(findings []detect.Finding, name string) []detect.Finding {
	var out []detect.Finding
	for _, f := range findings {
		if f.Object.Namespace != e2eNamespace || f.Severity < detect.SeverityWarning {
			continue
		}
		if f.Object.Name == name || strings.HasPrefix(f.Object.Name, name+"-") {
			out = append(out, f)
		}
	}
	return out
}

// deploymentSignal picks one Deployment out of a snapshot.
func deploymentSignal(snap health.Snapshot, namespace, name string) (health.DeploymentSignal, bool) {
	for i := range snap.Deployments {
		if snap.Deployments[i].Namespace == namespace && snap.Deployments[i].Name == name {
			return snap.Deployments[i], true
		}
	}
	return health.DeploymentSignal{}, false
}

// assertAuditTrailComplete checks that the in-process audit trail tells the whole story
// of the action and attributes it to the person who approved it.
//
// "Complete" is asserted as an exact phase SEQUENCE rather than as a set of presences.
// The phases each mean something a reader relies on — approved says permission existed,
// executed says a request was sent, verified says somebody checked afterwards — and a
// trail that recorded two of the three, or recorded them out of order, would read as a
// coherent but different story.
//
// The sequence is checked against the END of the trail because an earlier cycle may
// have abandoned cleanly and left records of its own (see driveApprovedRollback). Those
// records are not noise to be skipped past: they are checked too, and each must be a
// clean abort. A trail is allowed to contain "MaKlaude was authorized and then declined
// to act because the world had moved"; it is not allowed to contain a second execution,
// which is what the applied-change count below is really asserting.
func assertAuditTrailComplete(t *testing.T, trail *audit.Trail, done remediation) {
	t.Helper()

	records := trail.For(done.proposal.Identity)
	if len(records) < 3 {
		t.Fatalf("the audit trail holds %d record(s) for %s; a completed execution produces at least 3 (approved, executed, verified)",
			len(records), done.proposal.Identity)
	}

	applied, previous := 0, 0
	for i, rec := range records {
		if rec.Seq <= previous {
			t.Errorf("record %d carries Seq %d after %d; the trail's ordering is not a total order", i, rec.Seq, previous)
		}
		previous = rec.Seq
		if rec.Action.Cluster != done.proposal.Cluster || rec.Action.Target != done.proposal.Target {
			t.Errorf("record %d describes %s on %s, not the action that ran (%s on %s)",
				i, rec.Action.Cluster, rec.Action.Target.String(), done.proposal.Cluster, done.proposal.Target.String())
		}
		if rec.Change.Applied {
			applied++
		}
		if rec.Phase == audit.PhaseFailed && !rec.Outcome.CleanAbort {
			t.Errorf("record %d records a non-clean failure (%s): %s", i, rec.Outcome.Failure, rec.Outcome.Error)
		}
	}
	if applied != 1 {
		t.Errorf("the audit trail records %d applied change(s) for %s, want exactly 1 — one approval authorizes one action",
			applied, done.proposal.Identity)
	}

	records = records[len(records)-3:]
	phases := []string{records[0].Phase.String(), records[1].Phase.String(), records[2].Phase.String()}
	want := []string{
		audit.PhaseApproved.String(),
		audit.PhaseExecuted.String(),
		audit.PhaseVerified.String(),
	}
	if strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Fatalf("the audit trail for %s ends with phases %v, want %v", done.proposal.Identity, phases, want)
	}

	// The approval record: a named human, on the artifact that carried the decision.
	approved := records[0]
	if approved.Approver.Authority != audit.AuthorityHuman {
		t.Errorf("the audit trail records authority %s; a policy-waived action must never be recorded as a human approval",
			approved.Approver.Authority)
	}
	if approved.Approver.Identity != e2eApprover {
		t.Errorf("the audit trail names approver %q, want %q", approved.Approver.Identity, e2eApprover)
	}
	if approved.Approver.Ref != string(done.ref) {
		t.Errorf("the audit trail points at approval artifact %q, want %q", approved.Approver.Ref, done.ref)
	}
	if approved.Approver.ApprovedAt.IsZero() || approved.Approver.AuthorizedAt.IsZero() {
		t.Errorf("the approval record is missing a decision or authorization instant: %+v", approved.Approver)
	}

	// The execution record: a real change, sent once, on the record.
	executed := records[1]
	switch {
	case !executed.Change.Sent:
		t.Errorf("the executed record says nothing was sent")
	case !executed.Change.Applied:
		t.Errorf("the executed record says nothing was applied")
	case executed.Change.DryRun:
		t.Errorf("the executed record calls the action a preview")
	case executed.Change.Mode != kube.ExecuteEnabled.String():
		t.Errorf("the executed record ran under mode %q, want %q", executed.Change.Mode, kube.ExecuteEnabled)
	case !executed.Change.RecordedOnTrail:
		t.Errorf("the executed record says the execution never reached the approval trail")
	}
	if !strings.Contains(executed.Change.Scope, "/namespaces/"+e2eNamespace+"/deployments/"+wedgedDeploy) {
		t.Errorf("the executed record's scope %q does not name the approved object", executed.Change.Scope)
	}
	if !executed.PreState.Captured || executed.PreState.Kind != "deployment" {
		t.Errorf("the executed record carries no deployment pre-state: %+v", executed.PreState)
	}

	// The verification record: somebody looked, and said what they saw.
	verified := records[2]
	if verified.Outcome.Convergence != execute.ConvergenceConverged.String() {
		t.Errorf("the verified record reports convergence %q, want %q", verified.Outcome.Convergence, execute.ConvergenceConverged)
	}
	if verified.Outcome.Failed() {
		t.Errorf("the verified record carries failure %q", verified.Outcome.Failure)
	}
}

// assertArtifactShowsLifecycle checks the DURABLE half of the audit story.
//
// The in-process trail asserted above dies with the process; the approval artifact is
// what an operator opens six months later, and it is where the labels that prevent a
// second execution live. So the same lifecycle is checked again in the place it has to
// survive: the executed label applied, the human gate cleared, and the rendered
// lifecycle chain present verbatim in the conversation.
func assertArtifactShowsLifecycle(t *testing.T, sink *approve.MemorySink, done remediation) {
	t.Helper()

	// The permission slip and the artifact must be the same conversation. They are
	// carried separately — the gate hands back a slip, the sink holds the artifact —
	// and an executor that recorded its outcome against a DIFFERENT artifact than the
	// one a human decided on would leave both readable and neither true.
	if done.auth.Ref() != done.ref {
		t.Fatalf("the permission slip points at artifact %q, but the decision was recorded on %q",
			done.auth.Ref(), done.ref)
	}

	view, ok := sink.Snapshot(done.ref)
	if !ok {
		t.Fatalf("no approval artifact %q on the trail", done.ref)
	}
	if !view.HasLabel(approve.ExecutedLabel) {
		t.Errorf("the artifact is missing %q, so nothing durably stops a second execution: %v", approve.ExecutedLabel, view.Labels)
	}
	if !view.HasLabel(approve.ApprovedLabel) {
		t.Errorf("the artifact no longer carries %q; the decision it acted on is not visible on it: %v", approve.ApprovedLabel, view.Labels)
	}
	if view.HasLabel(approve.NeedsHumanLabel) {
		t.Errorf("the artifact still asks for a human decision after executing: %v", view.Labels)
	}

	conversation := strings.Join(view.Comments, "\n\n---\n\n")
	// The whole lifecycle, in the one line audit.Lifecycle renders it as. Asserting the
	// chain verbatim is what makes this a lifecycle check rather than four independent
	// keyword checks that would also pass on a trail describing a different action.
	const chain = "proposed → approved → executed → verified (converged)"
	for _, want := range []string{
		chain,
		"Approval honored.",
		"**Executed.**",
		"approved by @" + e2eApprover,
		done.proposal.Target.String(),
		"**Rollback:**",
	} {
		if !strings.Contains(conversation, want) {
			t.Errorf("the approval artifact's trail does not contain %q; it reads:\n%s", want, conversation)
		}
	}
	if strings.Contains(conversation, "NO HUMAN REVIEWED THIS") {
		t.Errorf("the artifact describes the action as unreviewed, but a named human approved it:\n%s", conversation)
	}
}

// untouchedState is the state of everything the approved action must not change, plus
// the approved target's own, captured before the action runs.
//
// The fields differ by kind because the fields that CAN move differ by kind. The seeded
// pods are deliberately unhealthy, so their status — and therefore their
// resourceVersion — is rewritten by the kubelet throughout the run; comparing that
// would flake for reasons unrelated to any write. A pod's UID and deletionTimestamp
// move only when the pod is actually replaced or deleted, and a Deployment's generation
// moves only on a SPEC change, which is exactly what a mutating action performs.
type untouchedState struct {
	podUIDs           map[string]string
	deployGenerations map[string]int64
}

// captureUntouched records the pre-action state of the other seeded objects and of the
// target itself.
func captureUntouched(t *testing.T, reader *kube.Client) untouchedState {
	t.Helper()
	state := untouchedState{
		podUIDs:           map[string]string{},
		deployGenerations: map[string]int64{},
	}
	for _, name := range []string{crashloopPod, pendingPod} {
		state.podUIDs[name] = string(readPod(t, reader, name).UID)
	}
	for _, name := range []string{badImageDeploy, wedgedDeploy} {
		state.deployGenerations[name] = readDeployment(t, reader, name).Generation
	}
	return state
}

// assertUntouched re-reads everything the approved action was not authorized to touch
// and requires it to be exactly where it was.
//
// This is the in-process half of assertion (d), and it is the half that does not depend
// on the apiserver audit log being readable. It also covers the one request the audit
// log cannot classify on its own — executor_test.go's dry-run pod DELETE — by proving
// from the object itself that no deletion took effect.
func assertUntouched(t *testing.T, reader *kube.Client, before untouchedState) {
	t.Helper()

	for name, uid := range before.podUIDs {
		pod := readPod(t, reader, name)
		if string(pod.UID) != uid {
			t.Errorf("UNAPPROVED WRITE: pod %s/%s was replaced (UID %s -> %s)", e2eNamespace, name, uid, pod.UID)
		}
		if pod.DeletionTimestamp != nil {
			t.Errorf("UNAPPROVED WRITE: pod %s/%s is being deleted (deletionTimestamp %s)", e2eNamespace, name, pod.DeletionTimestamp)
		}
	}

	dep := readDeployment(t, reader, badImageDeploy)
	if got := before.deployGenerations[badImageDeploy]; dep.Generation != got {
		t.Errorf("UNAPPROVED WRITE: deployment %s/%s had its spec changed (generation %d -> %d)",
			e2eNamespace, badImageDeploy, got, dep.Generation)
	}
	if _, stamped := dep.Spec.Template.Annotations[restartedAtKey]; stamped {
		t.Errorf("UNAPPROVED WRITE: deployment %s/%s carries a restart annotation; something restarted it",
			e2eNamespace, badImageDeploy)
	}
}

// assertTargetWasMutated is the positive counterpart of assertUntouched: the one object
// a human approved MUST have changed.
//
// Without it the whole suite would pass if the approved action had quietly done
// nothing, since every other assertion here is about absence. A Deployment's generation
// moves only on a spec change, and replacing /spec/template is a spec change, so a
// bumped generation is the API server's own evidence that the approved patch landed.
func assertTargetWasMutated(t *testing.T, reader *kube.Client, before untouchedState) {
	t.Helper()
	dep := readDeployment(t, reader, wedgedDeploy)
	was := before.deployGenerations[wedgedDeploy]
	if dep.Generation <= was {
		t.Errorf("the approved rollback left deployment %s/%s at generation %d (was %d); the patch did not change the spec",
			e2eNamespace, wedgedDeploy, dep.Generation, was)
	}
	assertRestoredImage(t, dep)
}

// assertRestoredImage checks the thing a rollback is actually FOR: the workload is back
// on an image that pulls.
//
// It asserts on the absence of the wedge marker rather than on the presence of a
// literal tag, so the manifest can change its good image without this test having to be
// edited in lockstep — while still failing loudly if the "rollback" left the unpullable
// image in place, which is the outcome a merge patch (rather than a JSON-patch replace)
// would silently produce.
func assertRestoredImage(t *testing.T, dep *appsv1.Deployment) {
	t.Helper()
	for _, c := range dep.Spec.Template.Spec.Containers {
		if strings.Contains(c.Image, wedgeImageMarker) {
			t.Errorf("deployment %s/%s container %q is still on the wedged image %q; the rollback did not restore the previous pod template",
				e2eNamespace, wedgedDeploy, c.Name, c.Image)
		}
	}
}

// wedgeImageMarker is the distinctive fragment of the unpullable tag the CI job rolls
// the Deployment forward onto. It is duplicated from the workflow rather than shared,
// deliberately: a test that asserted the code agrees with itself would prove nothing,
// whereas this fails the build if the two ever drift apart.
const wedgeImageMarker = "maklaude-e2e-wedged-rollout"

// mutatingAuditEvent is one mutating request the apiserver attributed to a MaKlaude
// identity, reduced to the fields the ledger below reasons about.
type mutatingAuditEvent struct {
	verb       string
	user       string
	resource   string
	namespace  string
	name       string
	requestURI string
	code       int
}

// target renders the object the request named, in the audit log's own plural-resource
// form.
func (e mutatingAuditEvent) target() string {
	return e.resource + "/" + e.namespace + "/" + e.name
}

// previewed reports whether the request carried the server-side dry-run marker in its
// query, which is where the API server reads it for every verb except DELETE.
func (e mutatingAuditEvent) previewed() bool {
	return strings.Contains(e.requestURI, "dryRun=All")
}

// accepted reports whether the API server actually performed the request. A rejected
// one — a 409 from a stale precondition, a 403 from RBAC — reached the server and
// changed nothing, so it belongs in the ledger but not in the count of writes that
// landed.
func (e mutatingAuditEvent) accepted() bool { return e.code >= 200 && e.code < 300 }

func (e mutatingAuditEvent) String() string {
	return fmt.Sprintf("%s %s by %s (uri=%s status=%d)", e.verb, e.target(), e.user, e.requestURI, e.code)
}

// assertOnlyTheApprovedWriteLanded is assertion (d): across everything the apiserver
// recorded, exactly one MaKlaude request actually changed the cluster, and it is the
// one a human approved.
//
// # Why this is not simply "zero mutating verbs" any more
//
// e2e_test.go's assertNoMutatingAudit still holds verbatim for the OBSERVATION identity
// and is re-run by this test after the mutation lands. The executor identity is
// different by design: it now has one real write to its name, plus the deliberate
// server-side previews this suite sends. So its events are classified rather than
// counted, and every class must be explained:
//
//   - a request carrying dryRun=All in its query is a preview, allowed only against an
//     object this suite deliberately previews;
//   - a request the API server did not accept reached it and changed nothing, allowed
//     only against a deliberate object;
//   - everything else LANDED, and there must be exactly one of those.
//
// # The one request the audit log cannot classify, and what covers it instead
//
// executor_test.go previews a pod DELETE. A DELETE's dry-run marker rides in its
// DeleteOptions BODY — the apiserver ignores a query marker whenever a body is present,
// which is why kube.hasServerDryRun reads the body for that verb — and this cluster's
// audit policy records Metadata only, deliberately, so bodies never reach a
// world-readable log. That request is therefore indistinguishable here from a real
// deletion, and no amount of parsing changes that.
//
// It is excepted by name, as narrowly as possible (that verb, that one pod), and the
// property it would otherwise prove is proven by a different instrument: the pod is
// re-read here, and a real deletion survives none of it — captureUntouched fails
// outright if the pod is gone, and assertUntouched fails if its UID moved or a
// deletionTimestamp appeared. Raising the audit policy to Request level would make the marker
// visible and is not worth it — it would put request bodies in a log this public repo's
// CI can surface, to re-prove something an object read already establishes.
//
// The check degrades gracefully when the audit log is unset or unreadable (the
// apiserver writes it as root, so a CI runner may not be able to open it), because the
// in-process proofs above hold on their own — the same posture assertNoMutatingAudit
// takes, for the same reason.
func assertOnlyTheApprovedWriteLanded(t *testing.T, approvedTarget string) {
	t.Helper()

	events, ok := readMutatingAudit(t)
	if !ok {
		return
	}

	// The objects this suite deliberately aims a server-side PREVIEW at.
	previewTargets := map[string]bool{
		"deployments/" + e2eNamespace + "/" + badImageDeploy: true, // executor_test.go's dry-run patch
		"deployments/" + e2eNamespace + "/" + wedgedDeploy:   true, // this test's preview of the rollback
	}
	// The single request whose dry-run marker the audit log cannot see. See above.
	bodyPreviewedDelete := "pods/" + e2eNamespace + "/" + pendingPod

	var landed []mutatingAuditEvent
	for _, ev := range events {
		switch {
		case ev.user == observationUser:
			t.Errorf("ZERO-WRITES VIOLATION: the observation identity issued a mutating request: %s", ev)

		case ev.previewed():
			if !previewTargets[ev.target()] {
				t.Errorf("UNAPPROVED WRITE: the executor previewed an object nothing in this suite aims at: %s", ev)
			}

		case ev.verb == "delete" && ev.target() == bodyPreviewedDelete:
			// The body-marked dry-run delete. Covered by assertUntouched's read of the pod.

		case !ev.accepted():
			if !previewTargets[ev.target()] && ev.target() != approvedTarget {
				t.Errorf("UNAPPROVED WRITE: the executor sent a mutating request at an object nothing approved, and the API server rejected it: %s", ev)
			}

		default:
			landed = append(landed, ev)
		}
	}

	if len(landed) != 1 {
		t.Fatalf("APPROVAL-GATE VIOLATION: %d mutating requests landed on the cluster, want exactly 1 (the approved rollback): %v",
			len(landed), landed)
	}
	only := landed[0]
	if only.user != executorUser {
		t.Errorf("the one landed write was made by %q, not the executor identity %q", only.user, executorUser)
	}
	if only.verb != "patch" || only.target() != approvedTarget {
		t.Fatalf("APPROVAL-GATE VIOLATION: the one landed write is %s, want a patch of %s", only, approvedTarget)
	}
	t.Logf("audit log: exactly one mutating request landed, and it is the approved action — %s", only)
}

// readMutatingAudit parses the apiserver audit log into the mutating requests it
// attributes to a MaKlaude identity, reporting ok=false when the log is unset or
// unreadable.
func readMutatingAudit(t *testing.T) ([]mutatingAuditEvent, bool) {
	t.Helper()

	path := strings.TrimSpace(os.Getenv("MAKLAUDE_E2E_AUDIT_LOG"))
	if path == "" {
		t.Log("MAKLAUDE_E2E_AUDIT_LOG unset; skipping the audit-log ledger (the object-state proofs above still hold)")
		return nil, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is supplied by the CI harness, not user input.
	if err != nil {
		t.Logf("audit log %q unreadable (%v); skipping the audit-log ledger (the object-state proofs above still hold)", path, err)
		return nil, false
	}

	mutating := map[string]bool{
		"create": true, "update": true, "patch": true,
		"delete": true, "deletecollection": true,
	}

	var out []mutatingAuditEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		// Cheap pre-filter before the JSON decode: every MaKlaude identity shares this
		// prefix, and the log is dominated by system components.
		if line == "" || !strings.Contains(line, "system:serviceaccount:maklaude:") {
			continue
		}
		var ev struct {
			Verb       string `json:"verb"`
			RequestURI string `json:"requestURI"`
			User       struct {
				Username string `json:"username"`
			} `json:"user"`
			ObjectRef struct {
				Resource  string `json:"resource"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"objectRef"`
			ResponseStatus struct {
				Code int `json:"code"`
			} `json:"responseStatus"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // not a JSON audit event line
		}
		if ev.User.Username != observationUser && ev.User.Username != executorUser {
			continue
		}
		if !mutating[strings.ToLower(ev.Verb)] {
			continue
		}
		out = append(out, mutatingAuditEvent{
			verb:       strings.ToLower(ev.Verb),
			user:       ev.User.Username,
			resource:   ev.ObjectRef.Resource,
			namespace:  ev.ObjectRef.Namespace,
			name:       ev.ObjectRef.Name,
			requestURI: ev.RequestURI,
			code:       ev.ResponseStatus.Code,
		})
	}
	t.Logf("audit log: %d mutating request(s) attributed to a MaKlaude identity", len(out))
	return out, true
}
