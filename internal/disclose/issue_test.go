package disclose

import (
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/budget"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
)

// These tests assert what a PERSON reads, because for an unattended action that is the
// entire oversight surface. Each one is a criterion from the task issue rather than a
// rendering preference, and where the criterion is about what must NOT appear the test
// says so explicitly — an artifact that quietly stops saying "no human approved this" is
// a regression no compile error would catch.

// TestBody_IsUnmistakablyPolicyAuthorizedAndNeverHumanApproved is the central criterion.
func TestBody_IsUnmistakablyPolicyAuthorizedAndNeverHumanApproved(t *testing.T) {
	body := Body(earnedAction())

	if !strings.HasPrefix(body, bannerNoHuman) {
		t.Errorf("the body does not open with the no-human banner:\n%s", firstLines(body, 3))
	}
	for _, want := range []string{
		"NO HUMAN APPROVED THIS ACTION",
		"Nobody. There is no approver on this action and none was asked",
		"`policy` — **not** a human approval",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the body does not contain %q", want)
		}
	}
	// The words that would make this read as a reviewed action. "approved by" is the
	// phrasing the gated path uses and the one a copy-paste would bring across.
	for _, forbidden := range []string{"approved by @", "a human approved", "reviewed by"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the body contains %q, which reads as human review", forbidden)
		}
	}
}

// TestBody_DistinguishesAnEarnedRuleFromTheBlanketBypass is the renderer test #145 asks
// for by name.
//
// It is not enough that the two produce different strings somewhere in the body. The
// distinction has to be legible: the rule is named, the citation is shown, and the body
// says in words what the difference between the two means — because the reader who needs
// it is one who has just been told a machine changed their cluster and is deciding how
// alarmed to be.
func TestBody_DistinguishesAnEarnedRuleFromTheBlanketBypass(t *testing.T) {
	body := Body(earnedAction())

	if !strings.Contains(body, "`policy:"+testRule+"`") {
		t.Errorf("the body does not record the earned rule as policy:%s", testRule)
	}
	if !strings.Contains(body, testCitatio) {
		t.Error("the body does not show the trust evidence that stood in for a review")
	}
	if !strings.Contains(body, approve.AutoApprovePolicy) {
		t.Errorf("the body does not name %q, so a reader cannot tell which of the two authorized this",
			approve.AutoApprovePolicy)
	}
	// And it must say which one it was, not merely mention both.
	if !strings.Contains(body, "This is an *earned* rule, not the blanket auto-approve switch") {
		t.Error("the body mentions both policies without stating which one authorized this action")
	}

	// The blanket bypass never renders as an earned rule, because an action it authorized
	// cannot construct a valid Action at all: it has no rule and no citation.
	bypass := earnedAction()
	bypass.Verdict.Rule = ""
	bypass.Verdict.Evidence = ""
	if bypass.Valid() {
		t.Fatal("an action with no rule and no citation is Valid, so the bypass could be disclosed as earned autonomy")
	}
}

// TestBody_CarriesTheRevocationSignal covers the "single documented signal" criterion.
//
// The three things asserted are the three a person needs in order to act without leaving
// the page: the exact label, the shape it revokes, and when it takes effect.
func TestBody_CarriesTheRevocationSignal(t *testing.T) {
	body := Body(earnedAction())

	if !strings.Contains(body, "## Revoking this") {
		t.Error("the body has no revocation section")
	}
	if !strings.Contains(body, "`"+RevokedLabel+"` label") {
		t.Errorf("the body does not name the %q label as the signal", RevokedLabel)
	}
	if !strings.Contains(body, earnedAction().Shape().String()) {
		t.Error("the body does not say which shape the revocation applies to")
	}
	if !strings.Contains(body, "next cycle") {
		t.Error("the body does not say when the revocation takes effect")
	}
	if !strings.Contains(body, "No configuration change, no restart") {
		t.Error("the body does not state that the label is the whole of the signal")
	}
}

// TestBody_StatesTheChangeThePreStateAndTheRollbackPlan walks the remaining content
// criteria. Pre-state and rollback come from [audit.Lifecycle], so this asserts they
// reach the artifact rather than re-testing their formatting.
func TestBody_StatesTheChangeThePreStateAndTheRollbackPlan(t *testing.T) {
	body := BodyWithOutcome(earnedAction(), convergedOutcome())

	for _, want := range []string{
		"`rolloutrestart`",               // the operation
		"deployment/shop/web",            // the target
		"`1000`",                         // the resourceVersion it was conditioned on
		"the deployment has not changed", // the conditions re-checked before acting
		"## Outcome: the action landed and the cluster reached the expected state",
		"## What followed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the completed body does not contain %q", want)
		}
	}
	// The lifecycle rendering carries the change, the pre-state and the rollback story.
	if !strings.Contains(body, audit.Lifecycle(convergedLifecycle())) {
		t.Error("the completed body does not embed the audit lifecycle")
	}
}

// TestBodyWithOutcome_RendersTheRedactedRecordsRatherThanTheRawReport is the secrets
// criterion, and it is asserted through the path a leak would actually take.
//
// The execution report carries the UNREDACTED convergence detail and error text; the
// audit sink redacts on the way in and hands back the stored copy. A renderer that
// reached for the report — which is right there in the outcome and is the more obvious
// field — would publish the original. So the test puts a secret in the report only, and
// the body must not contain it.
func TestBodyWithOutcome_RendersTheRedactedRecordsRatherThanTheRawReport(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLEKEY123456"

	o := convergedOutcome()
	o.Report.ConvergenceDetail = "the pod logged " + secret
	o.Report.Error = "connection refused using token " + secret
	o.Report.PreState.Fields = []execute.PreStateField{{Name: "annotation", Value: secret}}

	body := BodyWithOutcome(earnedAction(), o)
	if strings.Contains(body, secret) {
		t.Fatalf("the body published a value that only exists on the unredacted execution report:\n%s", body)
	}
}

// TestBody_RedactsTheProposalProseAndTheCitation covers the free-text fields this package
// renders itself, which [audit] never sees.
func TestBody_RedactsTheProposalProseAndTheCitation(t *testing.T) {
	const secret = "ghp1234567890abcdefghijklmnopqrstuvwx"

	a := earnedAction()
	a.Proposal.Intent = "the pod cannot pull using " + secret
	a.Proposal.ExpectedEffect = "it will pull with " + secret
	a.Proposal.Preconditions[0].Description = "the secret " + secret + " is unchanged"
	a.Verdict.Evidence = "3 converged runs, last one used " + secret

	body := Body(a)
	if strings.Contains(body, secret) {
		t.Fatalf("the body published a high-entropy value from the proposal or the citation:\n%s", body)
	}
}

// TestBody_KeepsTheStructuredIdentifiersLegible is the other half of redaction, and it
// matters as much: a sweep aggressive enough to blank the target produces an artifact
// that cannot say what was changed, which is not an audit record.
func TestBody_KeepsTheStructuredIdentifiersLegible(t *testing.T) {
	body := Body(earnedAction())
	for _, want := range []string{"deployment/shop/web", testCluster, "rolloutrestart", testRule} {
		if !strings.Contains(body, want) {
			t.Errorf("redaction removed the structured identifier %q", want)
		}
	}
}

// TestBody_RehearsalSaysTheClusterIsUnchangedInTheFirstSentence pins the distinction that
// is most consequential to misread. A dry-run artifact and a real one differ by one line
// and one table cell, and the line is where a reader looks.
func TestBody_RehearsalSaysTheClusterIsUnchangedInTheFirstSentence(t *testing.T) {
	a := earnedAction()
	a.Mode = "dry-run"
	body := Body(a)

	if !strings.Contains(body, "This is a **rehearsal**") {
		t.Error("a dry-run disclosure does not announce itself as a rehearsal")
	}
	if !strings.Contains(body, "the cluster is unchanged") {
		t.Error("a dry-run disclosure does not say the cluster is unchanged")
	}
	if strings.Contains(body, "MaKlaude changed cluster") {
		t.Error("a dry-run disclosure claims the cluster changed")
	}
}

// TestOutcomeHeading_NamesEveryEndingDistinctly stops the four outcomes collapsing into
// two. "Landed and worked" and "landed and did not work" are the pair most worth keeping
// apart, because only one of them is a reason to look at the cluster.
func TestOutcomeHeading_NamesEveryEndingDistinctly(t *testing.T) {
	converged := Outcome{Report: convergedReport()}

	dryRun := convergedReport()
	dryRun.Executed, dryRun.DryRun, dryRun.Convergence = false, true, execute.ConvergenceUnobserved

	aborted := convergedReport()
	aborted.Executed, aborted.Failure, aborted.Convergence = false, execute.FailureDrifted, execute.ConvergenceUnobserved

	notRun := execute.Report{Failure: execute.FailureKillSwitch}

	headings := map[string]string{
		"converged": outcomeHeading(converged),
		"dry-run":   outcomeHeading(Outcome{Report: dryRun}),
		"aborted":   outcomeHeading(Outcome{Report: aborted}),
		"timed-out": outcomeHeading(Outcome{Report: failedReport()}),
		"not-run":   outcomeHeading(Outcome{Report: notRun}),
	}
	seen := map[string]string{}
	for name, h := range headings {
		if other, dup := seen[h]; dup {
			t.Errorf("%q and %q render the same heading %q", name, other, h)
		}
		seen[h] = name
	}
	if !strings.Contains(headings["timed-out"], "did NOT reach the expected state") {
		t.Errorf("a non-converged execution does not say so: %q", headings["timed-out"])
	}
}

// TestWriteConsequence_StatesNothingFollowedRatherThanOmittingTheSection is the
// empty-means-all-clear rule this repository keeps relearning: a missing section and a
// section that says "none" are indistinguishable to a reader who does not know the
// renderer.
func TestWriteConsequence_StatesNothingFollowedRatherThanOmittingTheSection(t *testing.T) {
	body := BodyWithOutcome(earnedAction(), convergedOutcome())
	if !strings.Contains(body, "## What followed") {
		t.Fatal("the consequences section is missing from a successful disclosure")
	}
	if !strings.Contains(body, "Nothing. The action succeeded") {
		t.Error("a successful disclosure does not say that nothing followed")
	}
}

// TestWriteConsequence_ReportsAFailedDemotionLoudly covers the one follow-up whose silent
// failure would leave a shape trusted after an unattended failure.
func TestWriteConsequence_ReportsAFailedDemotionLoudly(t *testing.T) {
	o := Outcome{
		Report:      failedReport(),
		Records:     lifecycle("timed-out", "", false, false),
		Consequence: budget.Consequence{Demote: true, Escalate: true, ConsecutiveFailures: 1},
		DemotionErr: "the ledger file is read-only",
	}
	body := BodyWithOutcome(earnedAction(), o)

	if !strings.Contains(body, "The shape could not be demoted") {
		t.Error("a failed demotion is not reported")
	}
	if !strings.Contains(body, "revoke it with the label above") {
		t.Error("a failed demotion does not tell the reader what to do instead")
	}
}

// TestWriteConsequence_AnnouncesATrippedBreaker checks the transition is stated, since
// it is the moment a whole cluster stops being operated unattended.
func TestWriteConsequence_AnnouncesATrippedBreaker(t *testing.T) {
	o := Outcome{
		Report:      failedReport(),
		Records:     lifecycle("timed-out", "", false, false),
		Consequence: budget.Consequence{Demote: true, Escalate: true, Tripped: true, ConsecutiveFailures: 2},
	}
	body := BodyWithOutcome(earnedAction(), o)

	if !strings.Contains(body, "circuit breaker has TRIPPED") {
		t.Error("a tripped breaker is not announced on the artifact")
	}
	if !strings.Contains(body, "not** retried") {
		t.Error("the artifact does not say the failed action is not retried")
	}
}

// TestTitle_LeadsWithThePosture keeps the trail legible in a list, which is where the
// difference between an approval request and an unattended action has to be visible.
func TestTitle_LeadsWithThePosture(t *testing.T) {
	title := Title(earnedAction())
	if !strings.HasPrefix(title, "[unattended] ") {
		t.Errorf("Title = %q, want it to lead with the posture", title)
	}
	if !strings.Contains(title, testCluster) {
		t.Errorf("Title = %q, does not name the cluster", title)
	}
}

// TestLabelsFor_DoesNotAskForAHumanOnASuccess. Labelling every unattended action
// needs:human would make the label meaningless on the ones that do need somebody.
func TestLabelsFor_DoesNotAskForAHumanOnASuccess(t *testing.T) {
	labels := LabelsFor(earnedAction())
	for _, l := range labels {
		if l == NeedsHumanLabel {
			t.Error("a new disclosure is opened needs:human, which makes the label meaningless on failures")
		}
		if l == AppliedLabel {
			t.Error("a new disclosure is opened applied, so its absence would no longer mean the action never landed")
		}
	}
	if len(labels) != 1 || labels[0] != ManagedLabel {
		t.Errorf("LabelsFor = %v, want exactly [%s]", labels, ManagedLabel)
	}
}

// TestManagedLabel_IsDisjointFromTheOtherTrails is the query-isolation property. Three
// trails sharing one label would list each other's artifacts, surviving only because each
// parser skips bodies without its own marker — which is a coincidence, not a boundary.
func TestManagedLabel_IsDisjointFromTheOtherTrails(t *testing.T) {
	if ManagedLabel == approve.ManagedLabel {
		t.Errorf("the disclosure trail shares %q with the approval trail", ManagedLabel)
	}
	if ManagedLabel == "maklaude" {
		t.Errorf("the disclosure trail shares %q with the escalation trail", ManagedLabel)
	}
}

// TestChatSummary_SaysNoHumanApprovedItBeforeAnythingElse. A chat message is skimmed; if
// one fact survives that it must be this one.
func TestChatSummary_SaysNoHumanApprovedItBeforeAnythingElse(t *testing.T) {
	summary := ChatSummary(earnedAction(), convergedOutcome())
	if !strings.HasPrefix(summary, bannerNoHuman) {
		t.Errorf("the chat summary does not lead with the banner: %q", summary)
	}
	if !strings.Contains(summary, "no approver") {
		t.Errorf("the chat summary does not state that there is no approver: %q", summary)
	}
	if !strings.Contains(summary, testRule) {
		t.Errorf("the chat summary does not name the rule: %q", summary)
	}
}

// TestEscalationComment_LeadsWithNobodyWatching, which is the fact that makes a failed
// unattended action different from a failed approved one.
func TestEscalationComment_LeadsWithNobodyWatching(t *testing.T) {
	o := Outcome{Report: failedReport(), Consequence: budget.Consequence{Escalate: true, Tripped: true}}
	comment := EscalationComment(earnedAction(), o)

	if !strings.Contains(comment, "Nobody was watching when it ran") {
		t.Error("the escalation does not say nobody was watching")
	}
	if !strings.Contains(comment, "tripped cluster") {
		t.Error("the escalation does not mention the tripped breaker")
	}
	if !strings.Contains(comment, RevokedLabel) {
		t.Error("the escalation does not tell the reader how to revoke the rule that fired")
	}
}

// TestAction_ValidRefusesEveryHalfConfiguredShape. Each field is the thing whose absence
// would make the artifact assert a permission it cannot evidence.
func TestAction_ValidRefusesEveryHalfConfiguredShape(t *testing.T) {
	cases := map[string]func(*Action){
		"no identity":  func(a *Action) { a.Proposal.Identity = "" },
		"gated":        func(a *Action) { a.Verdict = autonomy.Verdict{} },
		"no rule":      func(a *Action) { a.Verdict.Rule = "" },
		"no citation":  func(a *Action) { a.Verdict.Evidence = "  " },
		"not admitted": func(a *Action) { a.Grant.Reason = budget.ReasonPassCapReached },
		"zero grant":   func(a *Action) { a.Grant = budget.Grant{} },
	}
	for name, mutate := range cases {
		a := earnedAction()
		mutate(&a)
		if a.Valid() {
			t.Errorf("%s: Action.Valid() is true", name)
		}
	}
	if !earnedAction().Valid() {
		t.Fatal("the fully-populated fixture is not Valid, so every case above proves nothing")
	}
}

// firstLines returns the first n lines of s, for readable failure output.
func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
