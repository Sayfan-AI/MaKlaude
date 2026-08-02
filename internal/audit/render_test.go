package audit

import (
	"context"
	"strings"
	"testing"
	"time"
)

// executionLifecycle builds the three records a successful execution produces, in
// the order the execution layer appends them, sequenced as a trail would.
func executionLifecycle() []Record {
	approved := fullRecord()
	approved.Seq, approved.Phase = 1, PhaseApproved
	approved.Change, approved.PreState, approved.Outcome = Change{}, PreState{}, Outcome{}
	approved.RecordedAt = time.Date(2026, 8, 1, 11, 0, 5, 0, time.UTC)

	executed := fullRecord()
	executed.Seq, executed.Phase = 2, PhaseExecuted
	executed.Outcome = Outcome{}
	executed.RecordedAt = approved.RecordedAt

	verified := fullRecord()
	verified.Seq, verified.Phase = 3, PhaseVerified
	verified.RecordedAt = approved.RecordedAt

	return []Record{approved, executed, verified}
}

// TestLifecycle_ShowsEveryStageFromProposedToVerified is the "the comms artifact
// shows the lifecycle" criterion, asserted stage by stage.
//
// The proposed stage is checked explicitly because it is the one no record carries:
// it is rendered from [Action.ProposedAt], and a rendering that quietly began at
// "approved" would hide how long an action waited for a human — one of the more
// interesting numbers in an incident review, and one nothing else in the artifact
// answers.
func TestLifecycle_ShowsEveryStageFromProposedToVerified(t *testing.T) {
	got := Lifecycle(executionLifecycle())

	for _, want := range []string{
		"proposed → approved → executed → verified (converged)",
		"**Proposed** 2026-08-01T10:00:00Z",
		"Cordon NotReady node",
		"@the-gigi (human approval)",
		"decision recorded 2026-08-01T11:00:00Z",
		"honored by the gate 2026-08-01T11:00:02Z",
		"approval artifact `42`",
		"| 1 | approved |",
		"| 2 | executed |",
		"| 3 | verified |",
		"PATCH /api/v1/nodes/node-a",
		"resourceVersion `1001`",
		"The attempt ran for 2s, from 2026-08-01T11:00:03Z to 2026-08-01T11:00:05Z.",
		"**State before the action**",
		"| unschedulable | `false` |",
		"**Rollback:** performable",
		"can perform this rollback on request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered lifecycle is missing %q:\n%s", want, got)
		}
	}
}

// TestLifecycle_LinksActionApproverAndOutcomeInOneArtifact is the done criterion
// stated as one assertion: a reader holding only this text can answer what was
// done, to what, on whose authority, and how it turned out.
func TestLifecycle_LinksActionApproverAndOutcomeInOneArtifact(t *testing.T) {
	got := Lifecycle(executionLifecycle())

	for what, want := range map[string]string{
		"the action":   "`cordonnode`",
		"the target":   "`node/node-a`",
		"the cluster":  "cluster `prod`",
		"the approver": "@the-gigi",
		"the outcome":  "converged",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the artifact does not state %s (%q):\n%s", what, want, got)
		}
	}
}

// TestLifecycle_ANonConvergedVerdictIsNotDressedUpAsSuccess pins the distinction
// the [PhaseVerified] doc insists on: verification happened, and it did not find
// what it was looking for. Rendering that as a plain "verified" would tell an
// operator the remediation worked when nobody knows whether it did.
func TestLifecycle_ANonConvergedVerdictIsNotDressedUpAsSuccess(t *testing.T) {
	recs := executionLifecycle()
	recs[2].Outcome = Outcome{
		Convergence: "timed-out",
		Detail:      "deployment shop/web is mid-rollout: 1/3 ready",
		ObservedFor: 90 * time.Second,
		Failure:     "none",
	}

	got := Lifecycle(recs)
	if !strings.Contains(got, "verified (timed-out)") {
		t.Errorf("the lifecycle chain does not carry the verdict:\n%s", got)
	}
	if !strings.Contains(got, "timed-out after watching for 1m30s") {
		t.Errorf("the step does not say how long was watched before the verdict:\n%s", got)
	}
	if !strings.Contains(got, "mid-rollout") {
		t.Errorf("the step does not say what was actually seen:\n%s", got)
	}
}

// TestLifecycle_CleanAbortSaysNothingWasApplied covers the outcome an operator is
// most likely to misread. A drifted precondition is a healthy refusal, and an
// artifact that renders it as a bare "failed" invites someone to go and fix
// something that is not broken.
func TestLifecycle_CleanAbortSaysNothingWasApplied(t *testing.T) {
	approved := fullRecord()
	approved.Seq, approved.Phase = 1, PhaseApproved
	approved.Change, approved.PreState, approved.Outcome = Change{}, PreState{}, Outcome{}

	failed := fullRecord()
	failed.Seq, failed.Phase = 2, PhaseFailed
	failed.Change = Change{Mode: "enabled"}
	failed.PreState = PreState{}
	failed.Outcome = Outcome{
		Convergence: "unobserved",
		Failure:     "drifted",
		CleanAbort:  true,
		Error:       "execute: a precondition no longer holds: nodenotready: node \"node-a\" has recovered",
	}

	got := Lifecycle([]Record{approved, failed})

	for _, want := range []string{
		"failed (drifted — abandoned cleanly, nothing was applied)",
		"the action was abandoned cleanly (drifted); nothing was applied",
		"has recovered",
		// The rollback section must not offer to reverse a change that was never made.
		"Nothing was applied, so there is nothing to undo.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered abort is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "can perform this rollback on request") {
		t.Errorf("an aborted action offers a rollback for a change it never made:\n%s", got)
	}
	if strings.Contains(got, "**State before the action**") {
		t.Errorf("an aborted action rendered a pre-state it never captured:\n%s", got)
	}
}

// TestLifecycle_PolicyWaivedApprovalIsCalledOut is the artifact-level half of the
// guarantee issue #124 needs. Rendering the approver honestly in [Approver.String]
// is not enough on its own: the artifact must make it hard to skim past, because the
// reader scanning it is looking for a name and will find one either way.
func TestLifecycle_PolicyWaivedApprovalIsCalledOut(t *testing.T) {
	recs := executionLifecycle()
	for i := range recs {
		recs[i].Approver = Approver{
			Authority:    AuthorityPolicy,
			Identity:     "MAKLAUDE_DANGEROUSLY_AUTO_APPROVE",
			AuthorizedAt: time.Date(2026, 8, 1, 11, 0, 2, 0, time.UTC),
			Ref:          "42",
		}
	}

	got := Lifecycle(recs)
	if !strings.Contains(got, "> No human reviewed this action.") {
		t.Errorf("a policy-waived action is not called out in the artifact:\n%s", got)
	}
	if strings.Contains(got, "human approval") {
		t.Errorf("a policy-waived action claims human approval:\n%s", got)
	}
	// The chain still reads "approved", because something did authorize it; the
	// authority section is what says who.
	if !strings.Contains(got, "proposed → approved →") {
		t.Errorf("a policy-waived action lost its approved stage:\n%s", got)
	}
}

// TestLifecycle_UnauthorizedAttemptNamesNobody covers the record written when there
// was no valid permission slip at all. Saying "nobody" in so many words is the
// point: a missing approver section would read as an omission, and this is the one
// case where the absence of a name is the finding.
func TestLifecycle_UnauthorizedAttemptNamesNobody(t *testing.T) {
	rec := fullRecord()
	rec.Seq, rec.Phase = 1, PhaseFailed
	rec.Approver = Approver{}
	rec.Change, rec.PreState = Change{}, PreState{}
	rec.Outcome = Outcome{Failure: "not-authorized", Error: "execute: no valid authorization for this action"}

	got := Lifecycle([]Record{rec})
	if !strings.Contains(got, "**Authorized by:** nobody.") {
		t.Errorf("an unauthorized attempt does not say nobody authorized it:\n%s", got)
	}
	if strings.Contains(got, "proposed → approved") {
		t.Errorf("an unauthorized attempt rendered an approved stage:\n%s", got)
	}
}

// TestLifecycle_RolledBackClosesTheStory checks both endings a rollback has: one
// where MaKlaude sent the inverse, and one where it found the work already done.
// The second is a success with nothing sent, and an artifact that rendered it as a
// failure would push an operator to re-run a rollback that is not needed.
func TestLifecycle_RolledBackClosesTheStory(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Record)
		want   []string
		absent []string
	}{
		"the inverse was performed": {
			mutate: func(r *Record) {
				r.Rollback.Performed = true
				r.Rollback.Description = "uncordon the node"
			},
			want: []string{
				"rolled back (the action's effect was undone)",
				"inverse action performed (uncordon the node)",
				"MaKlaude has already performed this rollback",
			},
		},
		"someone had already restored it": {
			mutate: func(r *Record) {
				r.Rollback.AlreadyAtPreState = true
				r.Change = Change{}
			},
			want: []string{
				"already back at its pre-action state; nothing was sent",
				"A rollback was requested and nothing was sent",
			},
			absent: []string{"failed"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := fullRecord()
			rec.Seq, rec.Phase = 4, PhaseRolledBack
			rec.Rollback.Attempted = true
			tc.mutate(&rec)

			got := Lifecycle([]Record{rec})
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the rendered rollback is missing %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("the rendered rollback should not mention %q:\n%s", bad, got)
				}
			}
		})
	}
}

// TestLifecycle_AFailedRollbackIsNotAFailedExecution guards the one ambiguity in the
// phase enum. Both are [PhaseFailed], and an operator reading "failed" needs to know
// which of the two things failed before anything else about the record matters —
// especially since the cluster is in opposite states in the two cases.
func TestLifecycle_AFailedRollbackIsNotAFailedExecution(t *testing.T) {
	rec := fullRecord()
	rec.Seq, rec.Phase = 4, PhaseFailed
	rec.Rollback.Attempted = true
	rec.Outcome = Outcome{Failure: "precondition-conflict", Error: "kube: target changed since the proposal was made"}

	got := Lifecycle([]Record{rec})
	if !strings.Contains(got, "the rollback failed (precondition-conflict)") {
		t.Errorf("a failed rollback does not identify itself as a rollback:\n%s", got)
	}
	if strings.Contains(got, "the action failed") {
		t.Errorf("a failed rollback reads as a failed execution:\n%s", got)
	}
}

// TestLifecycle_RedactsRecordsItWasHandedDirectly is the belt to Append's braces.
//
// Records that came from the trail are already sanitized, so this can only fail for
// a caller that renders records it built itself — which is exactly the mistake worth
// being immune to, since the output goes into a world-readable issue.
func TestLifecycle_RedactsRecordsItWasHandedDirectly(t *testing.T) {
	rec := fullRecord()
	rec.Seq, rec.Phase = 1, PhaseFailed
	rec.Outcome = Outcome{Failure: "execute-failed", Error: "unauthorized: token " + seededSecret}
	rec.Rollback.Note = "retry with " + seededSecret

	got := Lifecycle([]Record{rec})
	if strings.Contains(got, seededSecret) {
		t.Fatalf("Lifecycle rendered an unredacted record:\n%s", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("Lifecycle removed the secret without saying so:\n%s", got)
	}
}

// TestLifecycle_KeepsTheTableIntact covers the mundane failure that would make the
// artifact unreadable in practice: API server errors contain pipes and newlines, and
// either one turns a markdown table into garbage.
func TestLifecycle_KeepsTheTableIntact(t *testing.T) {
	rec := fullRecord()
	rec.Seq, rec.Phase = 1, PhaseFailed
	rec.Outcome = Outcome{
		Failure: "execute-failed",
		Error:   "admission webhook denied:\nfield | value\nreplicas | 0",
	}

	got := Lifecycle([]Record{rec})
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "| 1 |") {
			continue
		}
		if columns := strings.Count(line, "|") - strings.Count(line, `\|`); columns != 5 {
			t.Fatalf("the step row has %d unescaped pipes, want 5 (4 columns): %q", columns, line)
		}
		return
	}
	t.Fatalf("no step row was rendered:\n%s", got)
}

// TestLifecycle_WithNoRecordsSaysSo follows the project's rule that absence is
// reported rather than omitted. An empty string here would be posted as an empty
// comment and read as "nothing to report", when the absence of audit records for an
// action that ran is the opposite of nothing to report.
func TestLifecycle_WithNoRecordsSaysSo(t *testing.T) {
	got := Lifecycle(nil)
	if !strings.Contains(got, "no audit record") {
		t.Fatalf("an empty trail rendered %q, want an explicit statement that there is no record", got)
	}
}

// TestLifecycle_RendersATrailsOwnRecords is the end-to-end shape: append through the
// sink, render what came back, and get an artifact carrying real sequence numbers.
// The unit tests above build records by hand, which is precisely the arrangement
// that would not notice if Append and Lifecycle disagreed.
func TestLifecycle_RendersATrailsOwnRecords(t *testing.T) {
	trail := NewTrail().WithClock(func() time.Time { return time.Date(2026, 8, 1, 11, 0, 5, 0, time.UTC) })
	ctx := context.Background()

	var stored []Record
	for _, rec := range executionLifecycle() {
		// Deliberately drop the hand-assigned sequence: the trail assigns it.
		rec.Seq = 0
		s, err := trail.Append(ctx, rec)
		if err != nil {
			t.Fatalf("appending: %v", err)
		}
		stored = append(stored, s)
	}

	got := Lifecycle(stored)
	for _, want := range []string{"| 1 | approved | 2026-08-01T11:00:05Z", "| 2 | executed |", "| 3 | verified |"} {
		if !strings.Contains(got, want) {
			t.Errorf("the artifact rendered from the trail is missing %q:\n%s", want, got)
		}
	}
	if fromTrail := Lifecycle(trail.For(stored[0].Action.Identity)); fromTrail != got {
		t.Error("rendering the trail's own selection differs from rendering the appended records")
	}
}
