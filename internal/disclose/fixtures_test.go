package disclose

import (
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Fixed instants. Every time-dependent property in this package is about the ORDER of
// two instants, so the tests name them rather than computing offsets at the call site.
var (
	proposedAt = time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	appliedAt  = time.Date(2026, 8, 2, 9, 0, 30, 0, time.UTC)
	finishedAt = time.Date(2026, 8, 2, 9, 1, 15, 0, time.UTC)
)

const (
	testCluster = "prod"
	testRule    = "prod-rollout-restart"
	testCitatio = "3 of the last 3 human-approved rolloutrestarts on prod converged, none rolled back"
)

// testProposal is the safest operation in the catalog with a defined rollback plan, so a
// test that wants a refusal has to introduce it rather than inherit one from the fixture.
func testProposal() remediate.Proposal {
	return remediate.Proposal{
		Identity:   remediate.ProposalIdentity("rolloutrestart|prod|deployment|shop|web"),
		Hypothesis: "hyp-badimage-1",
		Incident:   "inc-shop-web",
		Cause:      diagnose.CauseBadImage,
		Confidence: diagnose.ConfidenceHigh,
		Cluster:    testCluster,
		Operation:  remediate.OpRolloutRestart,
		Target: remediate.Target{
			Cluster: testCluster, Kind: "deployment", Namespace: "shop", Name: "web", ResourceVersion: "1000",
		},
		Reversibility:  remediate.ReversibilityReversible,
		Title:          "Restart the rollout of deployment shop/web",
		Intent:         "the running pods are stuck on an image that never pulls",
		ExpectedEffect: "pods are replaced gradually by a fresh rollout",
		Preconditions: []remediate.Precondition{{
			Kind:        remediate.PreconditionUnchanged,
			Expect:      "1000",
			Description: "the deployment has not changed since the snapshot",
		}},
		ProposedAt: proposedAt,
	}
}

// earnedAction is one action about to be auto-applied under an earned rule.
func earnedAction() Action {
	p := testProposal()
	return Action{
		Proposal: p,
		Verdict: autonomy.Verdict{
			Decision: autonomy.DecisionAutoApply,
			Reason:   autonomy.ReasonEarnedTrust,
			Rule:     testRule,
			Evidence: testCitatio,
		},
		Grant: budget.Grant{Reason: budget.ReasonAdmitted, Cluster: p.Cluster, Target: p.Target.String()},
		Mode:  "enabled",
		At:    appliedAt,
	}
}

// convergedReport is the execution layer's account of an action that landed and worked.
func convergedReport() execute.Report {
	p := testProposal()
	return execute.Report{
		Identity: p.Identity, Cluster: p.Cluster, Operation: p.Operation, Target: p.Target,
		Reversibility: p.Reversibility, ProposedAt: p.ProposedAt,
		Approver: "policy:" + testRule, ApprovalRef: "1",
		Mode: kube.ExecuteEnabled, StartedAt: appliedAt, FinishedAt: finishedAt,
		PreState:    execute.PreState{Captured: true, Kind: "deployment", ResourceVersion: "1000"},
		Rollback:    execute.RollbackPlan{Kind: execute.RollbackNotRequired, Note: "a rollout restart leaves nothing to undo"},
		Attempts:    1,
		Executed:    true,
		Recorded:    true,
		Convergence: execute.ConvergenceConverged,
	}
}

// failedReport is an action that landed and did not do what it was supposed to — the
// case the circuit breaker exists for.
func failedReport() execute.Report {
	rep := convergedReport()
	rep.Convergence = execute.ConvergenceTimedOut
	rep.ConvergenceDetail = "the deployment still reports 1 unavailable replica"
	return rep
}

// lifecycle builds the audit records one execution produces, in trail order, in the shape
// [execute] emits them. Written out rather than driven through the execution layer so the
// marker tests pin the FORMAT rather than re-testing the producer.
func lifecycle(convergence, failure string, cleanAbort, dryRun bool) []audit.Record {
	p := testProposal()
	action := audit.Action{
		Identity: p.Identity, Cluster: p.Cluster, Operation: p.Operation, Target: p.Target,
		Reversibility: p.Reversibility, Title: p.Title, ProposedAt: p.ProposedAt,
	}
	approver := audit.Approver{
		Authority: audit.AuthorityPolicy, Identity: "policy:" + testRule, AuthorizedAt: appliedAt, Ref: "1",
	}
	change := audit.Change{
		Sent: true, Applied: !dryRun, DryRun: dryRun, Mode: "enabled",
		Scope: "PATCH /apis/apps/v1/namespaces/shop/deployments/web", ResourceVersion: "1000",
		Attempts: 1, RecordedOnTrail: true, StartedAt: appliedAt, FinishedAt: finishedAt,
	}

	recs := []audit.Record{
		{RecordedAt: finishedAt, Phase: audit.PhaseApproved, Action: action, Approver: approver},
		{RecordedAt: finishedAt, Phase: audit.PhaseExecuted, Action: action, Approver: approver, Change: change},
	}
	if convergence != "" {
		recs = append(recs, audit.Record{
			RecordedAt: finishedAt, Phase: audit.PhaseVerified, Action: action, Approver: approver, Change: change,
			Outcome: audit.Outcome{Convergence: convergence, Failure: "none"},
		})
	}
	if failure != "" {
		recs = append(recs, audit.Record{
			RecordedAt: finishedAt, Phase: audit.PhaseFailed, Action: action, Approver: approver, Change: change,
			Outcome: audit.Outcome{Convergence: convergence, Failure: failure, CleanAbort: cleanAbort},
		})
	}
	for i := range recs {
		recs[i].Seq = i + 1
	}
	return recs
}

// convergedLifecycle is the happy path's records.
func convergedLifecycle() []audit.Record { return lifecycle("converged", "", false, false) }

// convergedOutcome pairs the happy report with its records.
func convergedOutcome() Outcome {
	return Outcome{Report: convergedReport(), Records: convergedLifecycle(), At: finishedAt}
}
