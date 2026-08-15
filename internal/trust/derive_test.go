package trust

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

const (
	testIdentity = remediate.ProposalIdentity("prod/deployment/payments/web/rolloutrestart")
	testRef      = "https://github.com/Sayfan-AI/MaKlaude/issues/4242"
)

var (
	proposedAt = time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	startedAt  = proposedAt.Add(time.Minute)
	attemptEnd = startedAt.Add(30 * time.Second)
)

// lifecycle builds the audit records one execution produces, in the order
// [execute.executionRecords] emits them, and lets a scenario adjust the parts it is
// about.
type lifecycle struct {
	authority   audit.Authority
	ref         string
	dryRun      bool
	sent        bool
	applied     bool
	convergence string
	failure     string
	cleanAbort  bool
	rolledBack  bool
	rollback    bool
}

// converged is the happy path: a human approved it, it ran, it worked.
func converged() lifecycle {
	return lifecycle{
		authority:   audit.AuthorityHuman,
		ref:         testRef,
		sent:        true,
		applied:     true,
		convergence: execute.ConvergenceConverged.String(),
		failure:     execute.FailureNone.String(),
	}
}

// records renders the lifecycle as the audit records a trail would hold.
func (lc lifecycle) records() []audit.Record {
	base := audit.Record{
		Action: audit.Action{
			Identity:      testIdentity,
			Cluster:       shape.Cluster,
			Operation:     shape.Operation,
			Target:        remediate.Target{Cluster: shape.Cluster, Kind: "deployment", Namespace: "payments", Name: "web"},
			Reversibility: remediate.ReversibilityReversible,
			Fingerprint:   fixtureFP,
			ProposedAt:    proposedAt,
		},
		Approver: audit.Approver{Authority: lc.authority, Identity: "the-gigi", Ref: lc.ref},
		Change: audit.Change{
			Sent: lc.sent, Applied: lc.applied, DryRun: lc.dryRun,
			StartedAt: startedAt, FinishedAt: attemptEnd,
		},
		Rollback: audit.Rollback{Attempted: lc.rollback},
	}
	outcome := audit.Outcome{
		Convergence: lc.convergence,
		Failure:     lc.failure,
		CleanAbort:  lc.cleanAbort,
	}

	var recs []audit.Record
	if lc.authority != audit.AuthorityUnattributed {
		approved := base
		approved.Phase = audit.PhaseApproved
		approved.Change = audit.Change{}
		recs = append(recs, approved)
	}
	if lc.sent {
		executed := base
		executed.Phase = audit.PhaseExecuted
		recs = append(recs, executed)
	}
	if lc.convergence != "" && lc.convergence != execute.ConvergenceUnobserved.String() {
		verified := base
		verified.Phase, verified.Outcome = audit.PhaseVerified, outcome
		recs = append(recs, verified)
	}
	if lc.failure != "" && lc.failure != execute.FailureNone.String() {
		failed := base
		failed.Phase, failed.Outcome = audit.PhaseFailed, outcome
		recs = append(recs, failed)
	}
	if lc.rolledBack {
		done := base
		done.Phase, done.Outcome = audit.PhaseRolledBack, outcome
		recs = append(recs, done)
	}
	return recs
}

// Every outcome the trail can express, mapped to the one the ledger records. This is
// the table the whole derivation is, so it is written as one.
func TestEntryFromClassifiesEveryOutcome(t *testing.T) {
	cases := []struct {
		name string
		lc   lifecycle
		want Outcome
	}{
		{
			name: "converged",
			lc:   converged(),
			want: OutcomeConverged,
		},
		{
			name: "the window timed out",
			lc: func() lifecycle {
				lc := converged()
				lc.convergence = execute.ConvergenceTimedOut.String()
				return lc
			}(),
			want: OutcomeInconclusive,
		},
		{
			name: "the cluster could not be read",
			lc: func() lifecycle {
				lc := converged()
				lc.convergence = execute.ConvergenceUnobservable.String()
				return lc
			}(),
			want: OutcomeInconclusive,
		},
		{
			name: "the API server refused it",
			lc: func() lifecycle {
				lc := converged()
				lc.applied, lc.convergence = false, execute.ConvergenceUnobserved.String()
				lc.failure = execute.FailureExecute.String()
				return lc
			}(),
			want: OutcomeFailed,
		},
		{
			name: "the kill switch stopped it",
			lc: func() lifecycle {
				lc := converged()
				lc.sent, lc.applied, lc.convergence = false, false, execute.ConvergenceUnobserved.String()
				lc.failure = execute.FailureKillSwitch.String()
				return lc
			}(),
			want: OutcomeFailed,
		},
		{
			name: "the target drifted before anything was sent",
			lc: func() lifecycle {
				lc := converged()
				lc.sent, lc.applied, lc.convergence = false, false, execute.ConvergenceUnobserved.String()
				lc.failure, lc.cleanAbort = execute.FailureDrifted.String(), true
				return lc
			}(),
			want: OutcomeDriftAborted,
		},
		{
			name: "the API server rejected it on the resourceVersion",
			lc: func() lifecycle {
				lc := converged()
				lc.applied, lc.convergence = false, execute.ConvergenceUnobserved.String()
				lc.failure, lc.cleanAbort = execute.FailureConflict.String(), true
				return lc
			}(),
			want: OutcomeDriftAborted,
		},
		{
			name: "it was rolled back",
			lc: func() lifecycle {
				lc := converged()
				lc.rolledBack, lc.rollback = true, true
				return lc
			}(),
			want: OutcomeRolledBack,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := EntryFrom(tc.lc.records())
			if err != nil {
				t.Fatalf("deriving: %v", err)
			}
			if e.Outcome != tc.want {
				t.Errorf("Outcome = %s, want %s", e.Outcome, tc.want)
			}
			if e.Shape != shape {
				t.Errorf("Shape = %s, want %s", e.Shape, shape)
			}
			if !e.At.Equal(attemptEnd) {
				t.Errorf("At = %s, want the attempt's finish time %s", e.At, attemptEnd)
			}
		})
	}
}

// The precedence rule, which is the safety argument of the derivation: a lifecycle
// that converged and then went wrong is recorded by what went wrong. Reading the
// happy record and stopping would let a bad execution build trust.
func TestTheWorstOutcomeInALifecycleWins(t *testing.T) {
	cases := []struct {
		name string
		lc   lifecycle
		want Outcome
	}{
		{
			name: "converged, then the execution could not be recorded",
			lc: func() lifecycle {
				lc := converged()
				lc.failure = execute.FailureRecord.String()
				return lc
			}(),
			want: OutcomeFailed,
		},
		{
			name: "converged, then someone rolled it back",
			lc: func() lifecycle {
				lc := converged()
				lc.rolledBack, lc.rollback = true, true
				return lc
			}(),
			want: OutcomeRolledBack,
		},
		{
			name: "a clean abort alongside a real failure is the real failure",
			lc: func() lifecycle {
				lc := converged()
				lc.convergence = execute.ConvergenceUnobserved.String()
				lc.failure = execute.FailureExecute.String()
				return lc
			}(),
			want: OutcomeFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := EntryFrom(tc.lc.records())
			if err != nil {
				t.Fatalf("deriving: %v", err)
			}
			if e.Outcome != tc.want {
				t.Errorf("Outcome = %s, want %s", e.Outcome, tc.want)
			}
		})
	}
}

// A rollback is a second thing that happened to the same proposal, so it needs its
// own key — otherwise the idempotent Record would drop it as a duplicate of the
// execution it undoes, and the demotion would never land.
func TestARollbackGetsItsOwnKey(t *testing.T) {
	lc := converged()
	lc.rolledBack, lc.rollback = true, true

	execution, err := EntryFrom(converged().records())
	if err != nil {
		t.Fatalf("deriving the execution: %v", err)
	}
	rollback, err := EntryFrom(lc.records())
	if err != nil {
		t.Fatalf("deriving the rollback: %v", err)
	}

	if execution.Key == rollback.Key {
		t.Fatalf("the rollback shares the execution's key %q", execution.Key)
	}
	if !strings.HasPrefix(rollback.Key, string(testIdentity)) {
		t.Errorf("the rollback key %q does not name its proposal", rollback.Key)
	}

	// And both land in the ledger, with the rollback demoting.
	l := NewMemory()
	if err := l.RecordLifecycle(converged().records()); err != nil {
		t.Fatalf("recording the execution: %v", err)
	}
	if err := l.RecordLifecycle(lc.records()); err != nil {
		t.Fatalf("recording the rollback: %v", err)
	}
	if got := l.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2: the rollback collapsed into the execution", got)
	}
	if st := l.Standing(subject); !st.Blocked {
		t.Error("the rollback did not block trust")
	}
}

// A server-side preview is a real request that changed nothing. Counting it would
// let a shape earn autonomy out of rehearsals.
func TestAPreviewIsNotAnExecution(t *testing.T) {
	lc := converged()
	lc.dryRun, lc.applied = true, false
	lc.convergence = execute.ConvergenceUnobserved.String()

	_, err := EntryFrom(lc.records())
	if !errors.Is(err, ErrNotAnExecution) {
		t.Fatalf("a preview derived an entry: err = %v", err)
	}

	l := NewMemory()
	if err := l.RecordLifecycle(lc.records()); err != nil {
		t.Fatalf("RecordLifecycle surfaced the sentinel as a failure: %v", err)
	}
	if l.Len() != 0 {
		t.Errorf("Len = %d, want 0: a preview was recorded", l.Len())
	}
}

// A verified record whose change was a dry run must not be read as evidence about
// the cluster even if one somehow appears, since the executor's contract is that it
// returns before observing a preview.
func TestAVerifiedPreviewIsStillNotEvidence(t *testing.T) {
	lc := converged()
	lc.dryRun, lc.applied = true, false

	_, err := EntryFrom(lc.records())
	if !errors.Is(err, ErrNotAnExecution) {
		t.Fatalf("a previewed convergence derived an entry: err = %v", err)
	}
}

// An approval nobody acted on yet is not an execution.
func TestAnApprovalWithNoAttemptIsNotAnExecution(t *testing.T) {
	lc := lifecycle{authority: audit.AuthorityHuman, ref: testRef}

	if _, err := EntryFrom(lc.records()); !errors.Is(err, ErrNotAnExecution) {
		t.Fatalf("an unacted approval derived an entry: err = %v", err)
	}
	if _, err := EntryFrom(nil); !errors.Is(err, ErrNotAnExecution) {
		t.Fatalf("an empty lifecycle derived an entry: err = %v", err)
	}
}

// The authority travels from the trail unchanged, which is what keeps the "autonomy
// does not compound" rule honest end to end: an auto-applied execution derives an
// entry that cannot promote.
func TestAuthorityTravelsFromTheTrail(t *testing.T) {
	lc := converged()
	lc.authority, lc.ref = audit.AuthorityPolicy, ""

	e, err := EntryFrom(lc.records())
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	if e.Authority != audit.AuthorityPolicy {
		t.Errorf("Authority = %s, want policy", e.Authority)
	}
	if e.Promotes() {
		t.Error("an auto-applied converged execution promoted")
	}
	if e.Outcome != OutcomeConverged {
		t.Errorf("Outcome = %s, want converged: the outcome is a fact about the cluster", e.Outcome)
	}
}

// A human-approved lifecycle whose records name no artifact cannot become an entry:
// validate rejects it, and the derivation must not paper over that by inventing a
// reference or by downgrading the authority.
func TestAHumanApprovedLifecycleWithNoArtifactIsRejected(t *testing.T) {
	lc := converged()
	lc.ref = ""

	_, err := EntryFrom(lc.records())
	if err == nil {
		t.Fatal("a human-approved lifecycle with no artifact derived an entry")
	}
	if errors.Is(err, ErrNotAnExecution) {
		t.Fatalf("a malformed lifecycle was reported as having nothing to learn from: %v", err)
	}
	if !strings.Contains(err.Error(), "approval artifact") {
		t.Errorf("error does not name the missing artifact: %v", err)
	}
}

// The instant is event time. If the attempt's own bounds are missing — a lifecycle
// reconstructed from a rendered artifact — the recording time is the fallback, and
// an entry with neither is refused rather than being ordered arbitrarily.
func TestTheInstantPrefersEventTimeAndFallsBackToRecordingTime(t *testing.T) {
	recordedAt := attemptEnd.Add(time.Hour)

	withoutBounds := converged().records()
	for i := range withoutBounds {
		withoutBounds[i].Change.StartedAt = time.Time{}
		withoutBounds[i].Change.FinishedAt = time.Time{}
		withoutBounds[i].RecordedAt = recordedAt
	}
	e, err := EntryFrom(withoutBounds)
	if err != nil {
		t.Fatalf("deriving: %v", err)
	}
	if !e.At.Equal(recordedAt) {
		t.Errorf("At = %s, want the recording time %s", e.At, recordedAt)
	}

	withNeither := converged().records()
	for i := range withNeither {
		withNeither[i].Change.StartedAt = time.Time{}
		withNeither[i].Change.FinishedAt = time.Time{}
	}
	if _, err := EntryFrom(withNeither); err == nil {
		t.Fatal("a lifecycle with no usable instant derived an entry")
	}
}

// This package branches on the audit trail's string tokens rather than importing the
// execution layer's enums, because a consumer of a stored record must be able to
// read it without linking the package that wrote it. That indirection is only safe
// while the tokens agree, so the agreement is asserted rather than assumed.
func TestTrailTokensMatchTheExecutionEnums(t *testing.T) {
	pins := []struct {
		token string
		want  string
		what  string
	}{
		{tokenConverged, execute.ConvergenceConverged.String(), "converged"},
		{tokenTimedOut, execute.ConvergenceTimedOut.String(), "timed out"},
		{tokenUnobservable, execute.ConvergenceUnobservable.String(), "unobservable"},
		{tokenUnobserved, execute.ConvergenceUnobserved.String(), "unobserved"},
		{tokenNoFailure, execute.FailureNone.String(), "no failure"},
	}

	for _, pin := range pins {
		if pin.token != pin.want {
			t.Errorf("the %s token is %q here and %q in execute", pin.what, pin.token, pin.want)
		}
	}
}

// Every convergence value the execution layer can produce has to be classified. A
// new one that fell through would be silently inconclusive-or-worse, and the failure
// would show up as a shape that never earns trust for no visible reason.
func TestEveryConvergenceValueIsClassified(t *testing.T) {
	for _, c := range []execute.Convergence{
		execute.ConvergenceUnobserved,
		execute.ConvergenceConverged,
		execute.ConvergenceTimedOut,
		execute.ConvergenceUnobservable,
	} {
		lc := converged()
		lc.convergence = c.String()

		e, err := EntryFrom(lc.records())
		switch {
		case c == execute.ConvergenceUnobserved:
			if !errors.Is(err, ErrNotAnExecution) {
				t.Errorf("%s derived an entry: err = %v", c, err)
			}
		case err != nil:
			t.Errorf("%s failed to derive: %v", c, err)
		case e.Outcome == OutcomeUnrecorded:
			t.Errorf("%s classified as %s", c, e.Outcome)
		}
	}
}

// The derivation is the ledger's only writer in production, so the end-to-end shape
// worth asserting is a synthetic history of audit lifecycles earning autonomy.
func TestASyntheticTrailHistoryEarnsAutonomy(t *testing.T) {
	l := NewMemory()

	for i := 0; i < PromotionThreshold; i++ {
		recs := converged().records()
		for j := range recs {
			// Each execution is a distinct proposal against a bumped resourceVersion,
			// which is what makes them separate entries.
			recs[j].Action.Identity = remediate.ProposalIdentity(string(testIdentity) + "-" + string(rune('a'+i)))
			recs[j].Change.FinishedAt = attemptEnd.Add(time.Duration(i) * time.Hour)
		}
		if err := l.RecordLifecycle(recs); err != nil {
			t.Fatalf("recording execution %d: %v", i, err)
		}
	}

	if ev := l.Trust(subject); !ev.Trusted {
		t.Fatalf("%d converged human-approved lifecycles did not earn autonomy: %s",
			PromotionThreshold, l.Explain(subject))
	}
	if !strings.Contains(l.Trust(subject).Citation, testRef) {
		t.Errorf("the citation does not point at the approval artifact: %s", l.Trust(subject).Citation)
	}
}
