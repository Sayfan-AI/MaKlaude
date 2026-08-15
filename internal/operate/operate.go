// Package operate is the runnable seam for MaKlaude's GATED REMEDIATION cycle: the
// path that goes observe -> diagnose -> propose -> preview -> ask a human -> execute
// what they approved -> audit it.
//
// It is the sibling of [github.com/Sayfan-AI/MaKlaude/internal/scan], and the split
// between the two is deliberate rather than organizational. scan's package doc makes
// a promise — every step is read-only with respect to the cluster — and that promise
// is worth more than the convenience of one package doing both. So scan keeps its
// guarantee intact and this package carries the write path, with the boundary visible
// in the import graph: nothing in scan can reach a [kube.Executor].
//
// # Why this package exists at all
//
// Milestone 4 built every piece of gated remediation and wired none of them into the
// shipped binary. `maklaude` had two commands (version, scan), `scan.Pipeline` built
// only read-only clients, and [kube.NewExecutor] had no production caller — the whole
// propose -> approve -> execute -> audit chain was reachable only from unit tests and
// the e2e harness. That is a real safety property and it was documented as one, but it
// is also a way of saying the feature had never run outside a test. This package is
// the caller.
//
// # The opt-in, and what happens without it
//
// Everything here is a function of one [kube.ExecuteMode], which the operator sets
// explicitly and which defaults to [kube.ExecuteDisabled]:
//
//   - disabled (the default, and what an unset environment gets) — the cycle observes,
//     diagnoses, and PROPOSES, then stops and prints what it would ask for. It builds
//     no executor, opens no approval artifact, and sends nothing to any cluster. The
//     "builds no executor" is structural rather than conventional: [Cycle.runCluster]
//     returns before the builder is ever called, so there is no write-capable object in
//     the process to misuse. TestRun_DisabledBuildsNoExecutor asserts exactly that.
//   - dry-run — the full sequence runs against real admission controllers with
//     dryRun=All on every request. A human is asked, and if they say yes the action is
//     "executed" as a preview: the cluster does not change. It is the rehearsal mode.
//   - enabled — the full sequence, and the one mode under which a cluster changes.
//
// None of the five gates Milestone 4 built is relaxed by any of this. The separate
// write RBAC bundle, the kill switch, the scope-bound single-use approval, the
// freshly-re-checked preconditions and the resourceVersion are all still in force and
// are all still enforced by the layers that own them; this package only calls them in
// order.
//
// # One cycle is not one action
//
// A pass over a cluster asks about everything it can propose and executes only what
// somebody has already approved. On the first pass over a new problem that is nothing
// — an artifact with no decision on it is never consent — and the authorization
// arrives on a later pass, after a human has labelled it. That two-pass shape is the
// gate's, not this package's: [approve.Gatekeeper.Reconcile] both opens new requests
// and honors decisions found on existing ones, so a caller that runs the cycle on a
// schedule gets "ask, then act when told" for free.
//
// Multi-cluster isolation carries through from the layers underneath. Each cluster is
// observed through its own read-only client and acted on through its own executor,
// each executor is fixed to one cluster at construction, and the runner re-checks that
// the authorization, the proposal, and the write client all name the same cluster
// before anything is sent. A failure against one cluster is recorded against that
// cluster and does not abort the others.
package operate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// clientBuilder constructs the read-only [kube.Client] a cluster is observed through.
// It is the same seam [scan.Pipeline] uses and exists for the same reason: a test can
// hand back a client over a fake clientset instead of dialing an API server.
type clientBuilder func(h *cluster.Handle) (*kube.Client, error)

// mutatorBuilder constructs the write path for one cluster at one mode.
//
// It returns an [execute.Mutator] rather than a *[kube.Executor] on purpose. The
// interface is the enumeration of every mutating request MaKlaude can issue (see its
// doc), so wiring against it means this package cannot reach a write primitive that
// the execution layer has not already accounted for — and a test can substitute a
// recorder that proves an aborted cycle attempted nothing.
//
// It is also what makes the central safety claim testable rather than merely
// documented. "No executor is constructed without the opt-in" is a statement about
// whether this function is CALLED, and a fake can count calls; the same claim against
// a concrete constructor could only be checked by reading the code.
type mutatorBuilder func(h *cluster.Handle, mode kube.ExecuteMode) (execute.Mutator, error)

// Cycle runs one pass of MaKlaude's gated remediation over a set of clusters.
//
// The zero value is not usable; construct one with [New] or [NewForTest].
type Cycle struct {
	// mode is the kill switch for this whole cycle, read once at construction and
	// passed to every executor built. [kube.ExecuteDisabled] short-circuits the write
	// path entirely — see the package doc.
	mode kube.ExecuteMode

	newClient  clientBuilder
	newMutator mutatorBuilder

	// gate is the approval gate. It is nil exactly when mode is disabled: with nothing
	// able to execute, asking a human for consent would collect a decision that cannot
	// be acted on now and might be honored later under conditions they never saw.
	gate *approve.Gatekeeper

	// trail is the audit sink every attempt is written to, on every path out —
	// including the ones that sent nothing.
	trail audit.Sink

	// policy is the execution layer's mechanics: observation window, retry budget. It
	// is NOT an authorization policy; see [execute.Policy].
	policy execute.Policy

	// budget is the blast-radius ceiling on unattended actions: caps, cooldowns and the
	// per-cluster circuit breaker. It is nil when autonomy has not been wired, which is
	// the shipped posture, and a nil budget is not a permissive one — with no ceiling
	// there is nothing to auto-apply through, so every proposal takes the human gate.
	//
	// The cycle owns the budget's pass lifecycle ([budget.Budget.Begin] in [Cycle.Run])
	// rather than leaving it to a caller. A per-pass cap depends on somebody declaring
	// where a pass starts, and a caller that forgot would get a cap that never refills
	// or one that refills on every call; neither is a bound.
	budget *budget.Budget

	// rules and oracle are the two halves of "may this run without asking?": the
	// operator's allowlist, and the recorded history that says whether a shape has
	// earned it. Both are nil in the shipped posture and both are required — see
	// [Cycle.autonomyWired] — so nothing is auto-applied until an operator has written
	// rules AND a ledger says the shape earned them.
	//
	// [Cycle.autonomyFromEnv] is what fills them from an operator's environment: a rules
	// file through [rules.Load], and a trust ledger file that serves as both the oracle
	// and the recorder. This file owns what happens once they are here.
	rules  autonomy.Ruleset
	oracle autonomy.TrustOracle

	// disclosure is the trail every unattended action is recorded on. It is nil when
	// autonomy is not wired, and its absence blocks auto-apply outright rather than
	// downgrading it: an unattended mutation with no record is the one outcome this
	// milestone forbids, so "nowhere to disclose" means "nothing to disclose".
	disclosure *disclose.Trail

	// ledger is the trust ledger's write side. Both halves of the cycle write to it:
	// the gated path records the human-approved executions that promote a shape
	// ([Cycle.recordGatedTrust]), and the unattended path records the outcomes that
	// demote one ([Cycle.recordTrust]). What belongs in the evaluation window is the
	// ledger's rule, not either caller's — see [TrustRecorder].
	ledger TrustRecorder

	// autonomyOff is why the unattended half is not wired, in words, empty when it is
	// wired or when nobody said.
	//
	// It is a rendered sentence rather than a code because of who reads it. "Autonomy is
	// off" has half a dozen causes — no rules file, a rules file with the kill switch
	// disabled, a disclosure trail nothing can reach — and they look identical from the
	// outside: a report with no unattended actions in it. Naming the cause is the
	// difference between an operator who knows they have one variable left to set and one
	// who believes autonomy is on and is quietly wrong. See [Cycle.autonomyFromEnv].
	autonomyOff string

	// rulesPath and ledgerPath name the files autonomy was configured from, so the report
	// can point an operator at them instead of making them re-derive their own
	// environment. Both are empty for a cycle built by [NewForTest].
	rulesPath  string
	ledgerPath string

	// live reports whether the approval gate is backed by a real comms system rather
	// than the in-memory dry-run sink. Surfaced in the report because "nobody can
	// approve anything" is a materially different posture from "waiting on a human".
	live bool

	// now stamps the report and the actions; injectable so tests are reproducible.
	now func() time.Time
}

// Run executes one gated remediation pass over every cluster in the registry and
// returns a combined [Report].
//
// The returned error is non-nil only when the registry itself is unusable. Per-cluster
// failures are recorded inside the report so one unreachable cluster does not hide
// what MaKlaude found on the others.
func (c *Cycle) Run(ctx context.Context, reg *cluster.Registry) (*Report, error) {
	if reg == nil {
		return nil, fmt.Errorf("operate: nil registry")
	}

	// One Run is one pass, which is the unit the per-cluster auto-apply cap is measured
	// over. Beginning it here — before any cluster is touched, on the only path that
	// runs a cycle — is what stops the cap depending on a caller remembering to say so.
	if c.budget != nil {
		c.budget.Begin()
	}

	report := &Report{
		GeneratedAt: c.now().UTC(),
		Mode:        c.mode.String(),
		Live:        c.live,
	}

	// Read once, before any cluster is touched, so a person's revocation cannot race the
	// pass that would act on it. A read FAILURE disqualifies the unattended half of the
	// whole pass rather than being tolerated: acting unattended because the list of
	// things a person forbade could not be fetched would turn a network blip into a grant
	// of authority. The gated half is unaffected — every proposal simply takes the human
	// gate, which is the posture an operator who never enabled autonomy already has.
	revoked := c.revocations(ctx)
	for _, h := range reg.Handles() {
		report.Clusters = append(report.Clusters, c.runCluster(ctx, h, revoked))
	}
	// Taken after the clusters have run, so the suppressions reported are this pass's.
	// The revocation failure is stamped on afterwards rather than up front, because this
	// line replaces the whole struct — an assignment before it would be silently lost,
	// which is the failure mode of reporting a failure.
	report.Autonomy = autonomyReport(c.budget, c.posture())
	report.Autonomy.RevocationError = revoked.err
	report.finalize()
	return report, nil
}

// runCluster runs the whole cycle for one cluster, always returning a populated
// [ClusterReport] — including on failure, where Error explains what went wrong.
func (c *Cycle) runCluster(ctx context.Context, h *cluster.Handle, revoked revocationView) ClusterReport {
	cr := ClusterReport{Cluster: h.Name()}

	proposals, err := c.propose(ctx, h)
	if err != nil {
		cr.Error = err.Error()
		return cr
	}
	cr.Proposals = toProposalReports(proposals)

	// THE OPT-IN CHECK, and the reason it is here rather than further down: every
	// return below this point has already built a write-capable object. Under the
	// default posture the function ends here, so no executor exists at all — not a
	// disabled one, not an unused one. See the package doc.
	if c.mode == kube.ExecuteDisabled {
		return cr
	}
	if len(proposals) == 0 {
		// Nothing to ask about. Building an executor to do nothing with would hold
		// write authority open for no reason.
		return cr
	}

	// Recurrences are noted BEFORE anything is decided, so a fix that has just been
	// shown not to hold cannot authorize itself on the same pass that proves it. This
	// is the check that lets the counting window go — see the [trust] package doc — and
	// its placement is the whole of its value: a demotion recorded after the decision
	// would be perfectly correct and one pass too late.
	cr.Regressions = c.noteRecurrences(proposals)

	// The unattended half runs BEFORE the gate, and what it hands back is everything a
	// person still has to decide. Running it first is what stops an action being both
	// auto-applied and put to a human on the same pass; handing the remainder to the
	// unchanged gate is what keeps "not auto-applied" meaning "gated as before" rather
	// than "dropped".
	pass := c.autoApply(ctx, h, proposals, revoked)
	cr.AutoApplied = pass.Applied
	cr.RefusedByPolicy = pass.Refused
	cr.RevokedByHuman = pass.Revoked
	proposals = pass.Deferred
	if len(proposals) == 0 {
		return cr
	}

	requests, previewErrs := c.preview(ctx, h, proposals)
	cr.PreviewErrors = previewErrs
	if len(requests) == 0 {
		cr.Error = "no proposal could be previewed, so none was put to a human"
		return cr
	}

	result, err := c.gate.Reconcile(ctx, requests)
	if err != nil {
		cr.Error = fmt.Sprintf("reconciling the approval gate: %v", err)
		return cr
	}
	cr.Gate = GateReport{
		Opened:     result.Opened,
		Refreshed:  result.Refreshed,
		Refused:    result.Refused,
		Withdrawn:  result.Withdrawn,
		Authorized: len(result.Authorized),
	}
	if len(result.Authorized) == 0 {
		// The common and correct outcome of a first pass: MaKlaude asked, and nobody
		// has answered yet.
		return cr
	}

	cr.Executions = c.executeAuthorized(ctx, h, result.Authorized, proposals)
	return cr
}

// propose drives the read-only pipeline over one cluster and returns the remediations
// it suggests. Every stage is the production one, so a proposal ARRIVES from a
// diagnosis rather than being constructed.
func (c *Cycle) propose(ctx context.Context, h *cluster.Handle) ([]remediate.Proposal, error) {
	client, err := c.newClient(h)
	if err != nil {
		return nil, fmt.Errorf("building the read-only client: %w", err)
	}
	snap, err := health.NewCollector(client).Collect(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting health: %w", err)
	}
	if !snap.Reachability.Reachable {
		return nil, fmt.Errorf("cluster unreachable: %s", snap.Reachability.Error)
	}

	findings := detect.Analyze(snap)
	var hypotheses []diagnose.Hypothesis
	for _, incident := range correlate.Correlate(snap, findings) {
		hypotheses = append(hypotheses, diagnose.Diagnose(snap, incident)...)
	}
	return remediate.Hypotheses(snap, hypotheses), nil
}

// preview sends every proposal as a server-side dry run and pairs each with the
// evidence a human will read before deciding.
//
// The preview client is ALWAYS [kube.ExecuteDryRun], never c.mode — even when the
// cycle is execution-enabled. The preview's whole purpose is to be the request that
// did not happen, and sending it through a client that can really write would make the
// thing shown to a human the thing already done to the cluster.
//
// A proposal whose dry run fails is still put to the human. The gate refuses to
// authorize it ([approve.ReasonPreviewFailed]) but opens the artifact anyway, because
// "MaKlaude wanted to do this and the API server said no" is exactly what an operator
// should be able to see. The one class that is dropped instead is drift — a stale
// resourceVersion or a pruned revision — where the right answer is to re-propose
// against the state that exists now, on the next pass.
func (c *Cycle) preview(ctx context.Context, h *cluster.Handle, proposals []remediate.Proposal) ([]approve.Request, []string) {
	previewer, err := c.newMutator(h, kube.ExecuteDryRun)
	if err != nil {
		return nil, []string{fmt.Sprintf("building the preview-only client: %v", err)}
	}

	at := c.now().UTC()
	requests := make([]approve.Request, 0, len(proposals))
	var problems []string
	for _, p := range proposals {
		out, err := execute.Preview(ctx, previewer, p, at)
		switch {
		case errors.Is(err, kube.ErrPreconditionConflict), errors.Is(err, kube.ErrRevisionNotFound):
			// The world moved between the snapshot and the dry run. Not a failure, and
			// not something to ask a human about: the next pass re-proposes against
			// what exists then.
			problems = append(problems, fmt.Sprintf("%s on %s: the target moved while previewing; will re-propose", p.Operation, p.Target.String()))
		case errors.Is(err, execute.ErrNotPreviewOnly):
			// A preview client that can really write is a wiring bug, and continuing
			// would put the rest of the proposals through the same client.
			problems = append(problems, fmt.Sprintf("refusing to preview through a write-capable client: %v", err))
			return nil, problems
		case err != nil:
			// Everything else — an RBAC denial, an admission rejection, an unsupported
			// operation — is disqualifying evidence rather than a reason to stay quiet.
			requests = append(requests, approve.Request{
				Proposal: p,
				Preview:  approve.Preview{Performed: true, Error: err.Error()},
			})
		default:
			requests = append(requests, approve.Request{
				Proposal: p,
				Preview: approve.Preview{
					Performed: true,
					Summary: fmt.Sprintf("The API server accepted a dryRun=All %s of %s under scope `%s`, conditioned on resourceVersion %s. Nothing was applied.",
						p.Operation, p.Target.String(), out.Scope, out.ResourceVersion),
				},
			})
		}
	}
	return requests, problems
}

// executeAuthorized runs every permission slip the gate issued this pass.
//
// The write client is built ONCE here rather than per action, and only on the path
// where at least one authorization exists — an executor that is constructed and then
// used for nothing still holds write authority open for the life of the process.
// Within it, [kube.Executor] already builds a fresh, single-scope clientset per
// action, so nothing is shared between two approved actions beyond the cluster handle.
func (c *Cycle) executeAuthorized(ctx context.Context, h *cluster.Handle,
	auths []*approve.Authorization, proposals []remediate.Proposal) []ExecutionReport {

	mutator, err := c.newMutator(h, c.mode)
	if err != nil {
		return []ExecutionReport{{Error: fmt.Sprintf("building the write client: %v", err)}}
	}
	client, err := c.newClient(h)
	if err != nil {
		return []ExecutionReport{{Error: fmt.Sprintf("building the read-only client for convergence checks: %v", err)}}
	}
	runner, err := execute.New(mutator, health.NewCollector(client), c.gate, c.trail, c.policy)
	if err != nil {
		return []ExecutionReport{{Error: fmt.Sprintf("building the execution runner: %v", err)}}
	}

	out := make([]ExecutionReport, 0, len(auths))
	for _, auth := range auths {
		p, ok := proposalFor(auth, proposals)
		if !ok {
			// The gate issued a slip for something this pass did not propose. The
			// runner would refuse it anyway (the authorization must match the
			// proposal), but there is no proposal to hand it, so say so plainly
			// instead of fabricating one.
			out = append(out, ExecutionReport{
				Error: fmt.Sprintf("the gate authorized %s on %s, which this pass did not propose; skipping",
					auth.Operation(), auth.Target().String()),
			})
			continue
		}
		rep, err := runner.Execute(ctx, auth, p)
		er := toExecutionReport(rep, err)
		// The authority comes from the slip rather than the report: [execute.Report]
		// records WHO approved but not on what basis, and "a human said yes" versus
		// "a policy waived the requirement" is the distinction an operator reading
		// this is most likely to care about.
		er.Authority = auth.Authority().String()
		recs := c.lifecycleFor(p.Identity)
		c.markLifecycle(ctx, auth, recs, &er)
		c.recordGatedTrust(recs, &er)
		out = append(out, er)
	}
	return out
}

// markLifecycle attaches the machine-readable lifecycle marker to the approval artifact
// once a gated action has finished, so the execution can be read back off the trail and
// re-projected onto a trust entry.
//
// It runs on the GATED path deliberately, and the reason is specific to this path rather
// than symmetry with the unattended one: the trust ledger's promotion arithmetic counts
// human-approved executions, so a marker written only onto disclosures would make the
// evidence FOR autonomy the one thing a rebuild cannot reconstruct. See
// [approve.Gatekeeper.RecordLifecycle].
//
// A failure to mark is reported on the execution report and does not fail the action.
// The action has already run; the marker describes something finished, and the audit
// trail still holds the records whether or not the artifact carries them. The report is
// what keeps the loss visible — an artifact silently missing its marker is history a
// rebuild will not know it lost, which is the one failure mode this whole format exists
// to prevent.
func (c *Cycle) markLifecycle(ctx context.Context, auth *approve.Authorization,
	recs []audit.Record, er *ExecutionReport) {

	if c.gate == nil {
		return
	}
	if len(recs) == 0 {
		// No records to mark. The audit sink could not be read back, which the
		// execution report already reflects; writing an empty marker would put an
		// unreadable history on the trail in place of an absent one.
		return
	}
	if err := c.gate.RecordLifecycle(ctx, auth, recs); err != nil {
		er.Error = appendError(er.Error,
			fmt.Sprintf("the action ran and its approval artifact carries no rebuildable lifecycle: %v", err))
	}
}

// recordGatedTrust projects a finished gated execution onto the trust ledger, on the
// live path — a human-approved converged execution is the only entry that can promote
// a shape, and until this call existed none ever arrived (#166). The marker written by
// [Cycle.markLifecycle] made that evidence recoverable by a rebuild; this makes the
// ledger what its doc says it is, a cache of the artifacts rather than a file that
// happens to be reconstructible.
//
// A nil ledger is the shipped posture, not an error: the artifact carries the
// lifecycle marker, so the evidence is durable and a ledger wired later recovers it
// through [internal/rebuild]. A recording failure is reported on the execution report
// and does not fail the action, for the same reason a marking failure does not — the
// action has already run.
func (c *Cycle) recordGatedTrust(recs []audit.Record, er *ExecutionReport) {
	if c.ledger == nil || len(recs) == 0 {
		return
	}
	if err := c.ledger.RecordLifecycle(recs); err != nil {
		er.Error = appendError(er.Error,
			fmt.Sprintf("the action ran and its outcome could not be recorded in the trust ledger: %v", err))
	}
}

// appendError joins a new problem onto an existing error string without losing either.
func appendError(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// proposalFor finds the proposal a permission slip covers.
//
// It matches on [approve.Authorization.Matches] rather than on identity equality
// because Matches is the execution layer's own definition of "the slip covers this
// action", and using a second definition here would let the two disagree about which
// proposal is being executed.
func proposalFor(auth *approve.Authorization, proposals []remediate.Proposal) (remediate.Proposal, bool) {
	for _, p := range proposals {
		if auth.Matches(p) {
			return p, true
		}
	}
	return remediate.Proposal{}, false
}
