package approve

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
)

// The cases below are organized around one question: what has to be true before this
// package will manufacture permission that no person gave? Every refusal is its own test
// because each closes a different way for an unattended mutation to happen without the
// record that justifies it — and because a table would let a case be deleted without
// anybody noticing which guarantee went with it.

// earnedVerdict is a policy verdict that auto-applies: a named rule and a real citation.
func earnedVerdict() autonomy.Verdict {
	return autonomy.Verdict{
		Decision: autonomy.DecisionAutoApply,
		Reason:   autonomy.ReasonEarnedTrust,
		Rule:     "prod-rollout-restart",
		Evidence: "3 of the last 3 human-approved rolloutrestarts on prod converged, none rolled back",
	}
}

// admittedGrant is a blast-radius admission for the test proposal's cluster and target.
func admittedGrant() budget.Grant {
	p := testProposal()
	return budget.Grant{Reason: budget.ReasonAdmitted, Cluster: p.Cluster, Target: p.Target.String()}
}

// TestGrantAutonomous_NamesTheRuleAndNotTheBlanketBypass is the criterion from #145 at
// the layer that decides it.
//
// The whole milestone turns on these two being distinguishable: the bypass means a human
// waived review for everything, an earned rule means a human approved this exact shape
// repeatedly and it worked. If the slip recorded the same identity for both, no renderer
// downstream could tell them apart however carefully it was written.
func TestGrantAutonomous_NamesTheRuleAndNotTheBlanketBypass(t *testing.T) {
	auth, err := GrantAutonomous(testRequest(), earnedVerdict(), admittedGrant(), "disclosure-7", passAt)
	if err != nil {
		t.Fatalf("GrantAutonomous: %v", err)
	}

	if !auth.Valid() {
		t.Fatal("the slip is not valid, so no executor would act on it")
	}
	if got, want := auth.Approver(), "policy:prod-rollout-restart"; got != want {
		t.Errorf("Approver() = %q, want %q", got, want)
	}
	if auth.Approver() == AutoApprovePolicy {
		t.Error("an earned rule recorded itself as the blanket auto-approve bypass")
	}
	if got := auth.Authority(); got != AuthorityPolicy {
		t.Errorf("Authority() = %v, want %v", got, AuthorityPolicy)
	}
	if auth.Authority().HumanReviewed() {
		t.Error("HumanReviewed() is true for an action no human reviewed")
	}
	if !auth.ApprovedAt().IsZero() {
		t.Errorf("ApprovedAt() = %v, want the zero time: nothing was decided, so there is no instant", auth.ApprovedAt())
	}
	if got := auth.AuthorizedAt(); !got.Equal(passAt) {
		t.Errorf("AuthorizedAt() = %v, want %v", got, passAt)
	}
	if got := auth.Ref(); got != "disclosure-7" {
		t.Errorf("Ref() = %q, want the disclosure artifact", got)
	}
}

// TestGrantAutonomous_CarriesTheSameActionBindingAsAHumanGrant checks the half that must
// NOT differ. An unattended action gets no weaker binding to its cluster, its object, or
// the conditions to re-check than an approved one does — the executor's safety checks are
// written against these fields and a slip that carried fewer would slip past them.
func TestGrantAutonomous_CarriesTheSameActionBindingAsAHumanGrant(t *testing.T) {
	req := testRequest()
	auth, err := GrantAutonomous(req, earnedVerdict(), admittedGrant(), "disclosure-7", passAt)
	if err != nil {
		t.Fatalf("GrantAutonomous: %v", err)
	}

	if !auth.Matches(req.Proposal) {
		t.Error("the slip does not match the proposal it was minted for")
	}
	if got := auth.Cluster(); got != req.Proposal.Cluster {
		t.Errorf("Cluster() = %q, want %q", got, req.Proposal.Cluster)
	}
	if got := auth.Target(); got != req.Proposal.Target {
		t.Errorf("Target() = %v, want %v", got, req.Proposal.Target)
	}
	if got, want := len(auth.Preconditions()), len(req.Proposal.Preconditions); got != want {
		t.Errorf("Preconditions() carries %d conditions, want %d — an executor re-checks exactly these", got, want)
	}
	if got := auth.Reversibility(); got != req.Proposal.Reversibility {
		t.Errorf("Reversibility() = %v, want %v", got, req.Proposal.Reversibility)
	}
}

// TestGrantAutonomous_StringSaysNoHumanReviewedIt pins the log line, which is where this
// fact reaches somebody who is not reading an artifact.
func TestGrantAutonomous_StringSaysNoHumanReviewedIt(t *testing.T) {
	auth, err := GrantAutonomous(testRequest(), earnedVerdict(), admittedGrant(), "disclosure-7", passAt)
	if err != nil {
		t.Fatalf("GrantAutonomous: %v", err)
	}
	line := auth.String()
	if !strings.Contains(line, "NO HUMAN REVIEWED THIS") {
		t.Errorf("the log line does not say no human reviewed it:\n%s", line)
	}
	if !strings.Contains(line, "policy:prod-rollout-restart") {
		t.Errorf("the log line does not name the rule that permitted it:\n%s", line)
	}
	if strings.Contains(line, "approved by") && !strings.Contains(line, "AUTO-APPROVED by") {
		t.Errorf("the log line reads as a human approval:\n%s", line)
	}
}

// TestGrantAutonomous_RefusesAVerdictThatDoesNotAutoApply is the base case: policy said
// gate, and a slip minted anyway would be permission the policy layer never gave.
func TestGrantAutonomous_RefusesAVerdictThatDoesNotAutoApply(t *testing.T) {
	for _, v := range []autonomy.Verdict{
		{},
		{Decision: autonomy.DecisionGate, Reason: autonomy.ReasonUntrustedShape, Rule: "prod-rollout-restart"},
		{Decision: autonomy.DecisionRefuse, Reason: autonomy.ReasonIrreversible},
		// A verdict whose decision says auto-apply while its reason does not is exactly
		// what [autonomy.verdict] makes unconstructible; a caller assembling one by hand
		// must not get further than a caller who read the package.
		{Decision: autonomy.DecisionGate, Reason: autonomy.ReasonEarnedTrust, Rule: "r", Evidence: "e"},
	} {
		if _, err := GrantAutonomous(testRequest(), v, admittedGrant(), "disclosure-7", passAt); !errors.Is(err, ErrNotAutoApplicable) {
			t.Errorf("GrantAutonomous(%v) returned %v, want ErrNotAutoApplicable", v, err)
		}
	}
}

// TestGrantAutonomous_RefusesTrustWithNoCitation is the guarantee that keeps "earned"
// meaning earned.
//
// Nobody approved the action, so the citation is the entire oversight artifact an
// incident review works from. A rule that fires with nothing to point at is the blank
// cheque this repository already ships under an honest name, wearing a better word.
func TestGrantAutonomous_RefusesTrustWithNoCitation(t *testing.T) {
	for _, evidence := range []string{"", "   ", "\n\t "} {
		v := earnedVerdict()
		v.Evidence = evidence
		_, err := GrantAutonomous(testRequest(), v, admittedGrant(), "disclosure-7", passAt)
		if !errors.Is(err, ErrNotAutoApplicable) {
			t.Fatalf("evidence %q was accepted: %v", evidence, err)
		}
		if !strings.Contains(err.Error(), "oversight") {
			t.Errorf("the refusal does not say why the citation matters: %v", err)
		}
	}
}

// TestGrantAutonomous_RefusesWithoutAnAdmittedGrant is the eligible-is-not-go rule, made
// structural.
//
// [autonomy] says auto-apply means ELIGIBLE and [budget] is the ceiling that turns
// eligibility into permission. Written as prose in two package docs that rule is
// remembered; here it is enforced.
func TestGrantAutonomous_RefusesWithoutAnAdmittedGrant(t *testing.T) {
	for _, reason := range []budget.Reason{
		budget.ReasonStateUnreadable,
		budget.ReasonBreakerTripped,
		budget.ReasonPassCapReached,
		budget.ReasonTargetCoolingDown,
		budget.ReasonNoPass,
		budget.ReasonLimitsInvalid,
	} {
		g := admittedGrant()
		g.Reason = reason
		if _, err := GrantAutonomous(testRequest(), earnedVerdict(), g, "disclosure-7", passAt); !errors.Is(err, ErrNotAutoApplicable) {
			t.Errorf("a %s grant minted a permission slip: %v", reason, err)
		}
	}
	// The zero Grant is the case a refactor produces, and it must not read as permission.
	if _, err := GrantAutonomous(testRequest(), earnedVerdict(), budget.Grant{}, "disclosure-7", passAt); !errors.Is(err, ErrNotAutoApplicable) {
		t.Errorf("a zero Grant minted a permission slip: %v", err)
	}
}

// TestGrantAutonomous_RefusesWithNoDisclosureToPointAt closes the path that would let an
// unattended mutation run with no durable record anywhere.
//
// It is the ordering guarantee as well as a validity one: the artifact has to exist
// before the slip does, because the slip carries its reference, and enforcing it here is
// what stops the sequence being a thing a caller remembers.
func TestGrantAutonomous_RefusesWithNoDisclosureToPointAt(t *testing.T) {
	_, err := GrantAutonomous(testRequest(), earnedVerdict(), admittedGrant(), "", passAt)
	if !errors.Is(err, ErrNotAutoApplicable) {
		t.Fatalf("a slip was minted with no disclosure artifact: %v", err)
	}
	if !strings.Contains(err.Error(), "unrecorded") {
		t.Errorf("the refusal does not say what would go unrecorded: %v", err)
	}
}

// TestGrantAutonomous_RefusesACrossClusterAdmission is multi-cluster isolation at the one
// point where a policy decision, a ceiling and a proposal meet. Every layer underneath
// re-checks its own pair; this is the pair no other layer sees.
func TestGrantAutonomous_RefusesACrossClusterAdmission(t *testing.T) {
	g := admittedGrant()
	g.Cluster = "staging"
	_, err := GrantAutonomous(testRequest(), earnedVerdict(), g, "disclosure-7", passAt)
	if !errors.Is(err, ErrNotAutoApplicable) {
		t.Fatalf("an admission for another cluster minted a slip for this one: %v", err)
	}
}

// TestGrantAutonomous_RefusesARuleThatRendersAsTheBypass is the guard against the
// conflation the milestone exists to prevent, in the case where it would arrive by a
// spelling change rather than by a bug.
func TestGrantAutonomous_RefusesARuleThatRendersAsTheBypass(t *testing.T) {
	v := earnedVerdict()
	v.Rule = AutoApproveEnv
	_, err := GrantAutonomous(testRequest(), v, admittedGrant(), "disclosure-7", passAt)
	if !errors.Is(err, ErrNotAutoApplicable) {
		t.Fatalf("a rule named after the bypass minted a slip indistinguishable from it: %v", err)
	}
}

// TestGrantAutonomous_RefusalsMintNothing checks that every refusal returns a nil
// authorization rather than an invalid one. A caller that checks the error is fine either
// way; one that checks Valid() on a returned value is only safe if there is nothing to
// check.
func TestGrantAutonomous_RefusalsMintNothing(t *testing.T) {
	auth, err := GrantAutonomous(testRequest(), autonomy.Verdict{}, budget.Grant{}, "", time.Time{})
	if err == nil {
		t.Fatal("a fully-zero call was accepted")
	}
	if auth != nil {
		t.Fatalf("a refusal returned a non-nil authorization (Valid=%v)", auth.Valid())
	}
}

// TestGrant_BlanketBypassStillRecordsTheBypass is the other side of the distinction,
// asserted here so the two renderings are pinned in one file.
//
// [Decide] runs over artifacts on the approval trail and an earned rule never opens one,
// so the bypass is the only policy that reaches [grant] — and it must keep naming itself
// rather than acquiring a rule name it does not have.
func TestGrant_BlanketBypassStillRecordsTheBypass(t *testing.T) {
	auth := grant(testRequest(), PendingAction{Approver: "someone", DecidedAt: decidedAt, Ref: "artifact-3"}, AuthorityPolicy, passAt)

	if got := auth.Approver(); got != AutoApprovePolicy {
		t.Errorf("Approver() = %q, want %q", got, AutoApprovePolicy)
	}
	if strings.Contains(auth.Approver(), "someone") {
		t.Error("a policy-waived grant carried forward a person's login")
	}
	if !auth.ApprovedAt().IsZero() {
		t.Error("a policy-waived grant recorded a decision instant")
	}
	// And the two policy identities are not the same string, which is the property every
	// renderer downstream depends on.
	earned, err := GrantAutonomous(testRequest(), earnedVerdict(), admittedGrant(), "disclosure-7", passAt)
	if err != nil {
		t.Fatalf("GrantAutonomous: %v", err)
	}
	if earned.Approver() == auth.Approver() {
		t.Fatalf("an earned rule and the blanket bypass both record %q", auth.Approver())
	}
}
