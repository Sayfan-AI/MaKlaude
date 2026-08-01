package approve

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The tests in this file cover the part of the gate that decides whether MaKlaude
// may change a production cluster: [Decide], [Reconcile], and the [Authorization]
// they produce. They are separated from approve_test.go, which covers how an
// artifact RENDERS, because rendering being wrong is a legibility bug and this
// being wrong is an unapproved mutation.
//
// Everything here is a pure-value test on purpose — no sink, no clock, no network —
// which is the property [Decide] was written to have. The refusal set can therefore
// be enumerated exhaustively rather than sampled.

// mutate applies f to a copy of the legitimately-approved artifact, so each case
// below states only the ONE thing it changes about an otherwise-authorizable
// decision. A case that has to change two things is a case that is testing two
// things.
func mutate(req Request, f func(*PendingAction)) PendingAction {
	pending := approvedPending(req)
	f(&pending)
	return pending
}

// TestDecideAuthorizesOnlyWhenEveryConditionHolds is the central test of the
// package. Each case takes the one arrangement that SHOULD authorize and breaks
// exactly one condition, asserting both the reason a human will read and — the part
// that actually matters — that no authorization comes out of any branch except the
// single authorize branch.
//
// Asserting the reason and the authorization together is deliberate. A refusal that
// reports the right reason while still handing back a permission slip would satisfy
// a reason-only test and execute anyway.
func TestDecideAuthorizesOnlyWhenEveryConditionHolds(t *testing.T) {
	base := testRequest()

	cases := []struct {
		name       string
		req        Request
		pending    PendingAction
		wantKind   ActionKind
		wantReason Reason
	}{
		{
			name:       "every condition holds",
			req:        base,
			pending:    approvedPending(base),
			wantKind:   ActionAuthorize,
			wantReason: ReasonApprovalValid,
		},
		{
			name: "the object moved since the human was shown it",
			req: func() Request {
				r := testRequest()
				r.Proposal.Target.ResourceVersion = "1001"
				return r
			}(),
			pending:    approvedPending(base),
			wantKind:   ActionRefuse,
			wantReason: ReasonDrift,
		},
		{
			name: "the approval was recorded before the preview it appears to cover",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.DecidedAt = previewAt.Add(-time.Second)
			}),
			wantKind:   ActionRefuse,
			wantReason: ReasonApprovalPredatesPreview,
		},
		{
			name: "the approval is older than the TTL",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				// Both instants move back together, keeping the approval correctly
				// ordered AFTER the preview it covers — otherwise this case would trip
				// the predates-preview check instead and prove nothing about the TTL.
				p.PreviewedAt = passAt.Add(-DefaultApprovalTTL - 2*time.Second)
				p.DecidedAt = passAt.Add(-DefaultApprovalTTL - time.Second)
			}),
			wantKind:   ActionRefuse,
			wantReason: ReasonApprovalExpired,
		},
		{
			name: "nobody can be named for the approval",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.Approver = ""
			}),
			wantKind:   ActionRefuse,
			wantReason: ReasonUnattributedApproval,
		},
		{
			name: "MaKlaude approved its own proposal",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.Approver = "maklaude-bot"
				p.ApproverIsSelf = true
			}),
			wantKind:   ActionRefuse,
			wantReason: ReasonSelfApproval,
		},
		{
			name: "the dry-run came back an error",
			req: func() Request {
				r := testRequest()
				r.Preview.Error = "admission webhook denied the request"
				return r
			}(),
			pending:    approvedPending(base),
			wantKind:   ActionRefuse,
			wantReason: ReasonPreviewFailed,
		},
		{
			name: "the operation has no rollback plan",
			req: func() Request {
				r := testRequest()
				r.Proposal.Operation = remediate.Operation("scale-to-zero-and-pray")
				return r
			}(),
			pending:    approvedPending(base),
			wantKind:   ActionRefuse,
			wantReason: ReasonNoRollbackPlan,
		},
		{
			name: "the action already ran",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.Executed = true
			}),
			wantKind:   ActionHold,
			wantReason: ReasonAlreadyExecuted,
		},
		{
			name: "a human said no",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.State = StateRejected
			}),
			wantKind:   ActionHold,
			wantReason: ReasonRejected,
		},
		{
			name: "nobody has decided yet",
			req:  base,
			pending: mutate(base, func(p *PendingAction) {
				p.State = StatePending
				p.Approver = ""
				p.DecidedAt = time.Time{}
			}),
			wantKind:   ActionHold,
			wantReason: ReasonPreviewCurrent,
		},
		{
			name:       "no artifact exists yet",
			req:        base,
			pending:    PendingAction{},
			wantKind:   ActionOpen,
			wantReason: ReasonNewProposal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.req, tc.pending, DefaultPolicy(), passAt)

			if got.Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s", got.Kind, tc.wantKind)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", got.Reason, tc.wantReason)
			}

			wantAuthorized := tc.wantKind == ActionAuthorize
			if got.Authorization.Valid() != wantAuthorized {
				t.Fatalf("authorization valid = %t, want %t (reason %s) — a non-authorize branch must never produce a permission slip",
					got.Authorization.Valid(), wantAuthorized, got.Reason)
			}
		})
	}
}

// TestDecideNeverAuthorizesAnUnapprovedArtifact closes the loop the table above
// leaves open: it varies the decision state across the whole enum while leaving
// every OTHER condition satisfied, so the only thing standing between the proposal
// and execution is the human's label.
func TestDecideNeverAuthorizesAnUnapprovedArtifact(t *testing.T) {
	req := testRequest()

	for _, state := range []State{StatePending, StateRejected, State(99)} {
		t.Run(state.String(), func(t *testing.T) {
			pending := mutate(req, func(p *PendingAction) { p.State = state })

			got := Decide(req, pending, DefaultPolicy(), passAt)
			if got.Authorization.Valid() {
				t.Fatalf("state %s produced a valid authorization (kind %s, reason %s)", state, got.Kind, got.Reason)
			}
			if got.Kind == ActionAuthorize {
				t.Fatalf("state %s reached the authorize branch", state)
			}
		})
	}
}

// TestDecideReportsTheMostUsefulReasonWhenSeveralApply pins the check ORDER that
// [Decide] documents as part of its contract. Ordering is normally a cosmetic
// concern; here it decides what an operator reads first on an artifact that is
// wrong in more than one way, and each of these pairs was ordered for a stated
// reason that a refactor could silently invert.
func TestDecideReportsTheMostUsefulReasonWhenSeveralApply(t *testing.T) {
	req := testRequest()
	drifted := testRequest()
	drifted.Proposal.Target.ResourceVersion = "1001"

	failedAndDrifted := drifted
	failedAndDrifted.Preview.Error = "the server rejected the patch"

	cases := []struct {
		name string
		req  Request
		f    func(*PendingAction)
		want Reason
	}{
		{
			name: "already executed outranks an expired approval",
			req:  req,
			f: func(p *PendingAction) {
				p.Executed = true
				p.DecidedAt = passAt.Add(-DefaultApprovalTTL - time.Hour)
			},
			want: ReasonAlreadyExecuted,
		},
		{
			name: "already executed outranks a rejection",
			req:  req,
			f: func(p *PendingAction) {
				p.Executed = true
				p.State = StateRejected
			},
			want: ReasonAlreadyExecuted,
		},
		{
			name: "a failed dry-run outranks drift",
			req:  failedAndDrifted,
			f:    func(*PendingAction) {},
			want: ReasonPreviewFailed,
		},
		{
			name: "self-approval outranks the missing-identity check",
			req:  req,
			f: func(p *PendingAction) {
				p.Approver = ""
				p.ApproverIsSelf = true
			},
			want: ReasonSelfApproval,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.req, mutate(req, tc.f), DefaultPolicy(), passAt)
			if got.Reason != tc.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tc.want)
			}
			if got.Authorization.Valid() {
				t.Fatal("a disqualified decision produced an authorization")
			}
		})
	}
}

// TestUndecidedArtifactRefreshesOnlyWhenItsEvidenceMoved covers the pending branch,
// where the failure mode is the opposite of the approved branch's: refreshing too
// EAGERLY re-stamps PreviewedAt every pass, which is the clock PendingTTL measures
// against, so an unconditional refresh makes that knob unreachable.
func TestUndecidedArtifactRefreshesOnlyWhenItsEvidenceMoved(t *testing.T) {
	req := testRequest()
	undecided := func(f func(*PendingAction)) PendingAction {
		return mutate(req, func(p *PendingAction) {
			p.State = StatePending
			p.Approver = ""
			p.DecidedAt = time.Time{}
			f(p)
		})
	}

	cases := []struct {
		name    string
		req     Request
		pending PendingAction
		want    Reason
	}{
		{
			name:    "the body already shows what is true now",
			req:     req,
			pending: undecided(func(*PendingAction) {}),
			want:    ReasonPreviewCurrent,
		},
		{
			name: "the target moved",
			req:  req,
			pending: undecided(func(p *PendingAction) {
				p.PreviewedResourceVersion = "999"
			}),
			want: ReasonPreviewChanged,
		},
		{
			name: "the dry-run outcome changed while the target did not",
			req: func() Request {
				r := testRequest()
				r.Preview.Error = "the server now rejects this patch"
				return r
			}(),
			pending: undecided(func(*PendingAction) {}),
			want:    ReasonPreviewChanged,
		},
		{
			name: "the preview marker is unrecoverable, so it fails closed and re-renders",
			req:  req,
			pending: undecided(func(p *PendingAction) {
				p.PreviewedState = ""
			}),
			want: ReasonPreviewChanged,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.req, tc.pending, DefaultPolicy(), passAt)
			if got.Reason != tc.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tc.want)
			}
			if got.Authorization.Valid() {
				t.Fatal("an undecided artifact produced an authorization")
			}
		})
	}
}

// TestPendingExpiryIsOffUnlessConfigured guards the nullable-duration decision:
// PendingTTL is a pointer precisely so a forgotten zero cannot be read as a real
// value and withdraw every artifact on the first pass, including ones a human is
// part-way through reading.
func TestPendingExpiryIsOffUnlessConfigured(t *testing.T) {
	req := testRequest()
	stale := mutate(req, func(p *PendingAction) {
		p.State = StatePending
		p.Approver = ""
		p.DecidedAt = time.Time{}
		p.PreviewedAt = passAt.Add(-72 * time.Hour)
	})

	zero := time.Duration(0)
	short := time.Hour

	cases := []struct {
		name   string
		policy Policy
		want   Reason
	}{
		{
			name:   "the shipped default lets an undecided question wait indefinitely",
			policy: DefaultPolicy(),
			want:   ReasonPreviewCurrent,
		},
		{
			name:   "an explicit zero is off, not instant expiry",
			policy: Policy{ApprovalTTL: DefaultApprovalTTL, PendingTTL: &zero},
			want:   ReasonPreviewCurrent,
		},
		{
			name:   "a configured TTL withdraws the stale question",
			policy: Policy{ApprovalTTL: DefaultApprovalTTL, PendingTTL: &short},
			want:   ReasonPendingExpired,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(req, stale, tc.policy, passAt)
			if got.Reason != tc.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tc.want)
			}
		})
	}
}

// TestApprovalFreshnessUsesTheConfiguredTTL checks that the perishability of
// consent is actually driven by the policy rather than hard-coded, including the
// forgotten-field case: a zero ApprovalTTL must behave like the default and not
// like "expires instantly" (a gate that refuses everything) or "never expires" (a
// safety property silently switched off).
func TestApprovalFreshnessUsesTheConfiguredTTL(t *testing.T) {
	req := testRequest()
	pending := approvedPending(req)

	cases := []struct {
		name   string
		policy Policy
		at     time.Time
		want   Reason
	}{
		{
			name:   "a fresh approval under the default",
			policy: Policy{},
			at:     decidedAt.Add(DefaultApprovalTTL - time.Minute),
			want:   ReasonApprovalValid,
		},
		{
			name:   "a zero TTL falls back to the default rather than expiring instantly",
			policy: Policy{},
			at:     decidedAt.Add(time.Minute),
			want:   ReasonApprovalValid,
		},
		{
			name:   "a zero TTL does not disable expiry either",
			policy: Policy{},
			at:     decidedAt.Add(DefaultApprovalTTL + time.Minute),
			want:   ReasonApprovalExpired,
		},
		{
			name:   "a shorter configured TTL is honored",
			policy: Policy{ApprovalTTL: 5 * time.Minute},
			at:     decidedAt.Add(6 * time.Minute),
			want:   ReasonApprovalExpired,
		},
		{
			name:   "a longer configured TTL is honored",
			policy: Policy{ApprovalTTL: 48 * time.Hour},
			at:     decidedAt.Add(24 * time.Hour),
			want:   ReasonApprovalValid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(req, pending, tc.policy, tc.at)
			if got.Reason != tc.want {
				t.Fatalf("reason = %s, want %s", got.Reason, tc.want)
			}
		})
	}
}

// TestAuthorizationCannotBeForgedOutsideThePackage is the type-level half of the
// safety argument. The claim in the [Authorization] doc — that another package can
// write the struct literal but cannot make it report Valid — is only true while
// `granted` stays unexported and [grant] stays the sole writer, and both are the
// kind of thing a well-meaning refactor exports.
func TestAuthorizationCannotBeForgedOutsideThePackage(t *testing.T) {
	forged := &Authorization{
		identity: "rolloutrestart|prod|deployment|shop|web",
		cluster:  "prod",
		approver: "definitely-a-human",
	}

	if forged.Valid() {
		t.Fatal("a struct literal reported Valid — the unforgeability guarantee is gone")
	}
	if forged.Matches(testProposal()) {
		t.Error("an ungranted authorization matched a proposal")
	}

	// Every accessor must read as empty rather than leaking the populated fields,
	// so an executor that skips Valid() still cannot act on a forgery.
	if got := forged.Identity(); got != "" {
		t.Errorf("Identity() = %q on an ungranted authorization, want empty", got)
	}
	if got := forged.Cluster(); got != "" {
		t.Errorf("Cluster() = %q on an ungranted authorization, want empty", got)
	}
	if got := forged.Approver(); got != "" {
		t.Errorf("Approver() = %q on an ungranted authorization, want empty", got)
	}
	if !strings.Contains(forged.String(), "INVALID") {
		t.Errorf("String() = %q, want it to say INVALID so a log never suggests a grant that does not exist", forged.String())
	}

	// A nil receiver is the other shape an executor's guard has to survive without
	// a nil-check in front of it.
	var nilAuth *Authorization
	if nilAuth.Valid() {
		t.Error("a nil authorization reported Valid")
	}
	if !strings.Contains(nilAuth.String(), "INVALID") {
		t.Error("a nil authorization did not render as invalid")
	}
}

// TestAuthorizationRecordsWhoApprovedWhatAndWhen covers the audit half of T3's done
// criteria: approver identity and time captured, bound to one cluster, one object,
// and the resourceVersion the decision was made against.
func TestAuthorizationRecordsWhoApprovedWhatAndWhen(t *testing.T) {
	req := testRequest()
	auth := Decide(req, approvedPending(req), DefaultPolicy(), passAt).Authorization

	if !auth.Valid() {
		t.Fatal("the legitimately-approved arrangement did not authorize")
	}

	if got := auth.Approver(); got != "the-gigi" {
		t.Errorf("Approver() = %q, want the login from the label event", got)
	}
	if got := auth.ApprovedAt(); !got.Equal(decidedAt) {
		t.Errorf("ApprovedAt() = %s, want the artifact's decision time %s", got, decidedAt)
	}
	if got := auth.AuthorizedAt(); !got.Equal(passAt) {
		t.Errorf("AuthorizedAt() = %s, want the pass time %s", got, passAt)
	}
	if auth.ApprovedAt().After(auth.AuthorizedAt()) {
		t.Error("consent was recorded after it was acted on")
	}

	if got := auth.Cluster(); got != "prod" {
		t.Errorf("Cluster() = %q, want prod — an authorization is scoped to one cluster", got)
	}
	if got := auth.Operation(); got != remediate.OpRolloutRestart {
		t.Errorf("Operation() = %q, want the proposed operation", got)
	}
	if got := auth.Target().ResourceVersion; got != "1000" {
		t.Errorf("Target().ResourceVersion = %q, want the version the decision was bound to", got)
	}
	if got := auth.Ref(); got != ActionRef("7") {
		t.Errorf("Ref() = %q, want the artifact the outcome is recorded back onto", got)
	}

	// The audit line must name the approver and the object, since it is what lands
	// in logs where nobody has the struct to inspect.
	line := auth.String()
	for _, want := range []string{"the-gigi", "prod", "rv=1000"} {
		if !strings.Contains(line, want) {
			t.Errorf("String() = %q, missing %q", line, want)
		}
	}
}

// TestAuthorizationCoversOnlyItsOwnAction guards against positional
// correspondence: an executor holding several authorizations, or re-reading a
// proposal between authorization and execution, must be told no.
func TestAuthorizationCoversOnlyItsOwnAction(t *testing.T) {
	req := testRequest()
	auth := Decide(req, approvedPending(req), DefaultPolicy(), passAt).Authorization

	if !auth.Matches(req.Proposal) {
		t.Fatal("an authorization did not match the proposal it was granted for")
	}

	cases := []struct {
		name  string
		alter func(*remediate.Proposal)
	}{
		{"a different proposal identity", func(p *remediate.Proposal) { p.Identity = "something-else" }},
		{"a different operation", func(p *remediate.Proposal) { p.Operation = remediate.OpDeletePod }},
		{"a different namespace", func(p *remediate.Proposal) { p.Target.Namespace = "kube-system" }},
		{"a different object name", func(p *remediate.Proposal) { p.Target.Name = "api" }},
		{"a different cluster on the target", func(p *remediate.Proposal) { p.Target.Cluster = "staging" }},
		{"a resourceVersion that moved after the grant", func(p *remediate.Proposal) { p.Target.ResourceVersion = "1001" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := testProposal()
			tc.alter(&other)
			if auth.Matches(other) {
				t.Fatalf("authorization matched a proposal differing by %s", tc.name)
			}
		})
	}
}

// TestAuthorizationPreconditionsCannotBeMutatedByTheHolder checks the defensive
// copy. An executor that could edit the conditions it was allowed to assume could
// widen its own permission after the fact.
func TestAuthorizationPreconditionsCannotBeMutatedByTheHolder(t *testing.T) {
	req := testRequest()
	auth := Decide(req, approvedPending(req), DefaultPolicy(), passAt).Authorization

	got := auth.Preconditions()
	if len(got) != 1 {
		t.Fatalf("Preconditions() returned %d, want the 1 the proposal carried", len(got))
	}
	got[0].Expect = "anything-goes"

	if again := auth.Preconditions(); again[0].Expect != "1000" {
		t.Fatalf("mutating the returned slice changed the authorization: Expect = %q", again[0].Expect)
	}

	// Mutating the ORIGINATING proposal after the grant must not reach in either.
	req.Proposal.Preconditions[0].Expect = "tampered"
	if again := auth.Preconditions(); again[0].Expect != "1000" {
		t.Fatalf("mutating the source proposal changed the authorization: Expect = %q", again[0].Expect)
	}
}

// withIdentity returns the standard request re-keyed to a distinct identity and
// object, so the multi-proposal Reconcile tests below read as separate actions
// rather than as one action repeated.
func withIdentity(id string, name string) Request {
	r := testRequest()
	r.Proposal.Identity = remediate.ProposalIdentity(id)
	r.Proposal.Target.Name = name
	return r
}

// TestReconcileWithdrawsAProposalThatIsNoLongerBeingMade is the self-heal
// guarantee, and it is the reason a pending approval is not a queued job: when the
// reason to act disappears, the authority to act goes with it — even when a human
// already said yes.
func TestReconcileWithdrawsAProposalThatIsNoLongerBeingMade(t *testing.T) {
	req := testRequest()

	cases := []struct {
		name     string
		pending  PendingAction
		wantKind ActionKind
		want     Reason
	}{
		{
			name:     "an approved-but-unrun proposal that healed is withdrawn, not executed",
			pending:  approvedPending(req),
			wantKind: ActionWithdraw,
			want:     ReasonSelfHealed,
		},
		{
			name:     "an undecided proposal that healed is withdrawn",
			pending:  mutate(req, func(p *PendingAction) { p.State = StatePending; p.Approver = "" }),
			wantKind: ActionWithdraw,
			want:     ReasonSelfHealed,
		},
		{
			name:     "one that already ran closes as completed",
			pending:  mutate(req, func(p *PendingAction) { p.Executed = true }),
			wantKind: ActionWithdraw,
			want:     ReasonCompleted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No requests: the proposal is not being made this pass.
			plan := Reconcile(nil, []PendingAction{tc.pending}, DefaultPolicy(), passAt)

			if len(plan) != 1 {
				t.Fatalf("plan has %d actions, want 1: %v", len(plan), plan)
			}
			if plan[0].Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s", plan[0].Kind, tc.wantKind)
			}
			if plan[0].Reason != tc.want {
				t.Errorf("reason = %s, want %s", plan[0].Reason, tc.want)
			}
			if plan[0].Authorization.Valid() {
				t.Fatal("a withdrawal produced an authorization — a healed proposal must never execute")
			}
			if plan[0].Ref != tc.pending.Ref {
				t.Errorf("ref = %q, want the artifact being withdrawn %q", plan[0].Ref, tc.pending.Ref)
			}
		})
	}
}

// TestReconcileCollapsesDuplicatesSoOneActionCollectsOneApproval covers the two
// duplication shapes the external trail can produce. It matters more here than on
// the escalation trail: two artifacts for one action are two chances to approve it,
// and therefore two executions of something a human meant to allow once.
func TestReconcileCollapsesDuplicatesSoOneActionCollectsOneApproval(t *testing.T) {
	req := testRequest()

	t.Run("two artifacts claiming one identity keep the first and withdraw the rest", func(t *testing.T) {
		first := approvedPending(req)
		second := approvedPending(req)
		second.Ref = ActionRef("8")

		plan := Reconcile([]Request{req}, []PendingAction{first, second}, DefaultPolicy(), passAt)

		var authorized, withdrawn []ActionRef
		for _, a := range plan {
			switch a.Kind {
			case ActionAuthorize:
				authorized = append(authorized, a.Ref)
			case ActionWithdraw:
				withdrawn = append(withdrawn, a.Ref)
			}
		}

		if len(authorized) != 1 {
			t.Fatalf("got %d authorizations for one identity, want exactly 1: %v", len(authorized), authorized)
		}
		if authorized[0] != first.Ref {
			t.Errorf("authorized %q, want the first artifact %q", authorized[0], first.Ref)
		}
		if len(withdrawn) != 1 || withdrawn[0] != second.Ref {
			t.Errorf("withdrawn = %v, want just the duplicate %q", withdrawn, second.Ref)
		}
	})

	t.Run("a duplicated request opens only one artifact", func(t *testing.T) {
		plan := Reconcile([]Request{req, req}, nil, DefaultPolicy(), passAt)

		if len(plan) != 1 {
			t.Fatalf("plan has %d actions for a duplicated request, want 1: %v", len(plan), plan)
		}
		if plan[0].Kind != ActionOpen {
			t.Errorf("kind = %s, want %s", plan[0].Kind, ActionOpen)
		}
	})
}

// TestReconcileNeverReauthorizesAnExecutedAction is the durable idempotency
// property. The flag is recovered from the artifact rather than from memory
// precisely so a restart between "executed" and "recorded" cannot produce a second
// execution, so the test asserts it across the pass that would re-authorize.
func TestReconcileNeverReauthorizesAnExecutedAction(t *testing.T) {
	req := testRequest()
	executed := mutate(req, func(p *PendingAction) { p.Executed = true })

	// Two passes over the same state, as a restarted monitor would do.
	for pass := 1; pass <= 2; pass++ {
		plan := Reconcile([]Request{req}, []PendingAction{executed}, DefaultPolicy(), passAt.Add(time.Duration(pass)*time.Minute))

		if len(plan) != 1 {
			t.Fatalf("pass %d: plan has %d actions, want 1: %v", pass, len(plan), plan)
		}
		if plan[0].Kind != ActionHold || plan[0].Reason != ReasonAlreadyExecuted {
			t.Fatalf("pass %d: got %s/%s, want %s/%s", pass, plan[0].Kind, plan[0].Reason, ActionHold, ReasonAlreadyExecuted)
		}
		if plan[0].Authorization.Valid() {
			t.Fatalf("pass %d: an executed action was authorized again", pass)
		}
	}
}

// TestReconcileAccountsForEveryTrackedArtifact checks totality. ActionHold exists
// as an explicit action rather than as a gap in the plan so that "MaKlaude decided
// to leave this alone" and "MaKlaude never looked at this" stay distinguishable —
// which is only true if no path silently drops an artifact.
func TestReconcileAccountsForEveryTrackedArtifact(t *testing.T) {
	held := withIdentity("a-rejected", "web")
	authorizable := withIdentity("b-approved", "api")
	refusable := withIdentity("c-drifted", "cache")
	refreshable := withIdentity("d-pending", "queue")
	unproposed := withIdentity("e-healed", "gone")
	fresh := withIdentity("f-new", "new")

	// The drifted one is proposed at a version the artifact never displayed.
	drifted := refusable
	drifted.Proposal.Target.ResourceVersion = "2000"

	tracked := []PendingAction{
		mutate(held, func(p *PendingAction) { p.Identity = held.Identity(); p.State = StateRejected; p.Ref = "1" }),
		mutate(authorizable, func(p *PendingAction) { p.Identity = authorizable.Identity(); p.Ref = "2" }),
		mutate(refusable, func(p *PendingAction) { p.Identity = refusable.Identity(); p.Ref = "3" }),
		mutate(refreshable, func(p *PendingAction) {
			p.Identity = refreshable.Identity()
			p.State = StatePending
			p.Approver = ""
			p.PreviewedResourceVersion = "1"
			p.Ref = "4"
		}),
		mutate(unproposed, func(p *PendingAction) { p.Identity = unproposed.Identity(); p.Ref = "5" }),
	}
	reqs := []Request{held, authorizable, drifted, refreshable, fresh}

	plan := Reconcile(reqs, tracked, DefaultPolicy(), passAt)

	seen := make(map[remediate.ProposalIdentity]ActionKind, len(plan))
	for _, a := range plan {
		if prev, dup := seen[a.Identity]; dup {
			t.Fatalf("identity %q appears twice in the plan (%s then %s)", a.Identity, prev, a.Kind)
		}
		seen[a.Identity] = a.Kind
	}

	want := map[remediate.ProposalIdentity]ActionKind{
		held.Identity():         ActionHold,
		authorizable.Identity(): ActionAuthorize,
		refusable.Identity():    ActionRefuse,
		refreshable.Identity():  ActionRefresh,
		unproposed.Identity():   ActionWithdraw,
		fresh.Identity():        ActionOpen,
	}
	for id, kind := range want {
		got, ok := seen[id]
		if !ok {
			t.Errorf("identity %q is missing from the plan", id)
			continue
		}
		if got != kind {
			t.Errorf("identity %q got %s, want %s", id, got, kind)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("plan covers %d identities, want %d", len(seen), len(want))
	}
}

// TestReconcileSettlesTheTrailBeforeAuthorizing pins the plan ORDER, which the
// package documents as a safety property rather than a cosmetic one: a caller that
// starts acting on the plan as it walks it must have dropped every authority that
// should no longer exist before it is handed one that should.
func TestReconcileSettlesTheTrailBeforeAuthorizing(t *testing.T) {
	authorizable := withIdentity("z-approved", "api")
	drifted := withIdentity("y-drifted", "cache")
	proposedDrifted := drifted
	proposedDrifted.Proposal.Target.ResourceVersion = "2000"
	healed := withIdentity("x-healed", "gone")
	fresh := withIdentity("w-new", "new")

	tracked := []PendingAction{
		mutate(authorizable, func(p *PendingAction) { p.Identity = authorizable.Identity(); p.Ref = "1" }),
		mutate(drifted, func(p *PendingAction) { p.Identity = drifted.Identity(); p.Ref = "2" }),
		mutate(healed, func(p *PendingAction) { p.Identity = healed.Identity(); p.Ref = "3" }),
	}
	plan := Reconcile([]Request{authorizable, proposedDrifted, fresh}, tracked, DefaultPolicy(), passAt)

	// Rank encodes the documented group order; every action's rank must be
	// non-decreasing across the plan.
	rank := map[ActionKind]int{
		ActionWithdraw:  0,
		ActionRefuse:    1,
		ActionOpen:      2,
		ActionRefresh:   3,
		ActionHold:      3,
		ActionAuthorize: 4,
	}
	last := -1
	for i, a := range plan {
		r, ok := rank[a.Kind]
		if !ok {
			t.Fatalf("plan[%d] has unranked kind %s", i, a.Kind)
		}
		if r < last {
			t.Fatalf("plan[%d] is %s after a later-ranked action — authorizations must come last: %v", i, a.Kind, plan)
		}
		last = r
	}
	if len(plan) == 0 || plan[len(plan)-1].Kind != ActionAuthorize {
		t.Fatalf("the last action is not an authorization: %v", plan)
	}
}

// TestReconcileIsDeterministic guards the reproducibility the plan's sort exists
// for: the same inputs in a different order must produce the same plan, so a
// reviewer diffing two passes sees only real change.
func TestReconcileIsDeterministic(t *testing.T) {
	a := withIdentity("aaa", "one")
	b := withIdentity("bbb", "two")
	c := withIdentity("ccc", "three")

	tracked := []PendingAction{
		mutate(a, func(p *PendingAction) { p.Identity = a.Identity(); p.Ref = "1" }),
		mutate(b, func(p *PendingAction) { p.Identity = b.Identity(); p.Ref = "2" }),
		mutate(c, func(p *PendingAction) { p.Identity = c.Identity(); p.Ref = "3" }),
	}
	reversedTracked := []PendingAction{tracked[2], tracked[1], tracked[0]}

	forward := Reconcile([]Request{a, b, c}, tracked, DefaultPolicy(), passAt)
	backward := Reconcile([]Request{c, b, a}, reversedTracked, DefaultPolicy(), passAt)

	if len(forward) != len(backward) {
		t.Fatalf("plan lengths differ: %d vs %d", len(forward), len(backward))
	}
	for i := range forward {
		if forward[i].Identity != backward[i].Identity || forward[i].Kind != backward[i].Kind {
			t.Fatalf("plan[%d] differs by input order: %s/%s vs %s/%s",
				i, forward[i].Identity, forward[i].Kind, backward[i].Identity, backward[i].Kind)
		}
	}
}
