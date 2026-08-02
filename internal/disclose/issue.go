package disclose

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/redact"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file renders everything a person reads on a disclosure artifact.
//
// # The rendering is derived from the AUDIT RECORDS, not from the execution report
//
// The two carry the same facts and only one of them is safe to publish. [audit.Sink]
// redacts on the way in — convergence detail, error text, pre-state values, rollback
// prose — and returns the stored, redacted copy; [execute.Report] is the unredacted
// original and is the value a renderer would reach for first because it is right there
// in the outcome. So the outcome sections here go through [audit.Lifecycle] over
// [Outcome.Records], which is the same rendering the gated path posts and is already
// covered by the redaction tests in [audit].
//
// That also means this file does not reimplement the pre-state table, the rollback
// paragraph, the timing line or the step-by-step. Those are [audit.Lifecycle]'s and
// re-deriving them would give the unattended path a second renderer to keep in step
// with the attended one — which, since the unattended path is the one nobody reviews,
// is the wrong one to let drift.
//
// What this file adds is everything the gated path gets from a person and this one does
// not: which rule permitted the action, what history earned it, which ceiling admitted
// it, and how to take the permission away.

// bannerNoHuman is the first line of every disclosure body and of the chat message.
//
// It is stated in capitals and stated first because of where these are read. An issue
// notification shows a title and an opening line; a chat message is skimmed. If exactly
// one fact survives being skimmed it must be that no person reviewed this, which is the
// same reasoning [approve.Authorization.String] applies to its log line.
const bannerNoHuman = "**NO HUMAN APPROVED THIS ACTION.** MaKlaude applied it under earned autonomy policy."

// Title renders the artifact's subject line.
//
// It leads with the posture rather than the operation. A reader scanning an issue list
// sees a column of titles, and "unattended" in the first characters is what separates
// this trail from the approval requests and the incident escalations they sit next to.
func Title(a Action) string {
	p := a.Proposal
	return fmt.Sprintf("[unattended] %s — %s on cluster %s", p.Title, p.Target.String(), p.Cluster)
}

// LabelsFor returns the labels a new disclosure artifact is opened with.
//
// It deliberately does NOT include [NeedsHumanLabel]. An auto-applied action that
// worked does not need anybody; labelling every one of them for a human would make the
// label meaningless on the ones that do (see [ConsequenceComment], which is where it is
// applied). Nor does it include [AppliedLabel]: at the instant the artifact opens,
// nothing has landed yet, and that label's whole value is that its absence means
// something.
func LabelsFor(Action) []string { return []string{ManagedLabel} }

// Body renders the artifact as it is opened, BEFORE the action runs.
//
// Everything here is known in advance, which is what makes opening first possible: the
// proposal, the rule, the evidence, the admission, and how to revoke. The outcome is
// appended later by [BodyWithOutcome] — see the package doc on why the artifact exists
// before the action does.
func Body(a Action) string {
	var b strings.Builder

	b.WriteString(bannerNoHuman)
	b.WriteString("\n\n")
	b.WriteString(openingPosture(a))
	b.WriteString("\n\n")

	writeAction(&b, a)
	writeAuthority(&b, a)
	writeAdmission(&b, a)
	writeRevocation(&b, a)

	b.WriteString("\n")
	b.WriteString(proposalMarker(a.Proposal.Identity))
	b.WriteString("\n")
	b.WriteString(shapeMarker(a.Shape()))
	b.WriteString("\n")
	return b.String()
}

// BodyWithOutcome renders the artifact once the action has finished: the opening body
// with the outcome sections and the machine-readable lifecycle marker appended.
//
// The opening body is re-rendered rather than the outcome being appended to whatever
// the artifact currently holds, so the result is a pure function of (action, outcome)
// and does not depend on what a person may have edited in the meantime.
//
// A lifecycle that cannot be marked is reported in the body rather than dropped: the
// marker is what a rebuild reads, and an artifact silently missing one is history the
// ledger will not know it lost.
func BodyWithOutcome(a Action, o Outcome) string {
	var b strings.Builder
	b.WriteString(Body(a))

	b.WriteString("\n---\n\n")
	b.WriteString(outcomeHeading(o))
	b.WriteString("\n\n")

	if len(o.Records) > 0 {
		b.WriteString(audit.Lifecycle(o.Records))
		b.WriteString("\n")
	} else {
		b.WriteString("No audit records were produced for this attempt, which should not happen: the execution layer writes one on every path out, including the ones that sent nothing.\n\n")
	}

	writeConsequence(&b, o)

	marker, err := audit.LifecycleMarker(o.Records)
	if err != nil {
		fmt.Fprintf(&b, "\n> **This action's history cannot be rebuilt from this artifact.** The machine-readable lifecycle marker could not be written: %s\n",
			redact.String(err.Error()))
		return b.String()
	}
	b.WriteString("\n")
	b.WriteString(marker)
	b.WriteString("\n")
	return b.String()
}

// openingPosture states, in one sentence, whether a cluster was actually changed.
//
// A rehearsal and a real mutation produce artifacts that are identical apart from this
// line and the mode field, and reading one as the other is the single most consequential
// mistake a person can make on this page — so the distinction is a sentence at the top
// rather than a value in a table halfway down.
func openingPosture(a Action) string {
	if a.Mode == "dry-run" {
		return "This is a **rehearsal**. The action below was auto-applied through a server-side dry run, so the cluster is unchanged. The whole unattended path ran; only the write was a preview."
	}
	return fmt.Sprintf("MaKlaude changed cluster `%s` without asking anybody. This artifact is the entire oversight record for that change.", a.Proposal.Cluster)
}

// writeAction states what was done, to what, and why MaKlaude wanted to.
func writeAction(b *strings.Builder, a Action) {
	p := a.Proposal
	b.WriteString("## The action\n\n")
	b.WriteString("| Field | Value |\n| --- | --- |\n")
	fmt.Fprintf(b, "| Operation | `%s` |\n", p.Operation)
	fmt.Fprintf(b, "| Target | `%s` |\n", p.Target.String())
	fmt.Fprintf(b, "| Cluster | `%s` |\n", p.Cluster)
	fmt.Fprintf(b, "| resourceVersion | `%s` |\n", cell(p.Target.ResourceVersion))
	fmt.Fprintf(b, "| Reversibility | %s |\n", p.Reversibility)
	fmt.Fprintf(b, "| Execution mode | `%s` |\n", cell(a.Mode))
	fmt.Fprintf(b, "| Applied at | %s |\n", stamp(a.At))
	fmt.Fprintf(b, "| Diagnosis | %s (%s confidence) |\n", p.Cause, p.Confidence)
	b.WriteString("\n")

	if p.Intent != "" {
		fmt.Fprintf(b, "**Why:** %s\n\n", redact.String(p.Intent))
	}
	if p.ExpectedEffect != "" {
		fmt.Fprintf(b, "**Expected effect:** %s\n\n", redact.String(p.ExpectedEffect))
	}
	writePreconditions(b, p)
}

// writePreconditions lists the conditions the action was allowed to assume.
//
// On the gated path a person reads these before consenting. Nobody read them here, so
// they are recorded as what MaKlaude checked on their behalf — the executor re-evaluates
// every one against the live cluster immediately before acting, and an action that ran
// is an action for which all of them held.
func writePreconditions(b *strings.Builder, p remediate.Proposal) {
	if len(p.Preconditions) == 0 {
		return
	}
	b.WriteString("**Conditions re-checked immediately before acting:**\n\n")
	for _, pc := range p.Preconditions {
		fmt.Fprintf(b, "- %s\n", redact.String(pc.Description))
	}
	b.WriteString("\n")
}

// writeAuthority is the section that replaces a person's name.
//
// The rule and the citation are rendered as separate fields because they answer
// different questions and a reviewer needs both: the rule is what an operator WROTE,
// and the citation is what the recorded history said about whether it had been earned.
// A rule with no citation would be the blank cheque the whole milestone exists to avoid,
// which is why [approve.GrantAutonomous] refuses to mint one — this section is where
// that refusal becomes visible.
func writeAuthority(b *strings.Builder, a Action) {
	b.WriteString("## Who permitted it\n\n")
	b.WriteString("Nobody. There is no approver on this action and none was asked. It was authorized by policy:\n\n")
	b.WriteString("| Field | Value |\n| --- | --- |\n")
	fmt.Fprintf(b, "| Authority | `policy` — **not** a human approval |\n")
	fmt.Fprintf(b, "| Rule | `%s` |\n", cell(a.Verdict.Rule))
	fmt.Fprintf(b, "| Recorded as | `%s` |\n", cell(a.Verdict.PolicyIdentity()))
	fmt.Fprintf(b, "| Shape | `%s` |\n", a.Shape().String())
	b.WriteString("\n**Trust evidence — the history that stood in for a review:**\n\n")
	fmt.Fprintf(b, "> %s\n\n", redact.String(a.Verdict.Evidence))
	b.WriteString("This is an *earned* rule, not the blanket auto-approve switch. " +
		"The blanket switch (`policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE`) means a person waived review for everything; " +
		"an earned rule means a person approved this exact shape repeatedly, it converged every time, and the ledger promoted it. " +
		"If this artifact ever names the blanket switch, the trust evidence above is not why this ran.\n\n")
}

// writeAdmission states which ceiling let the action through.
//
// It is here because "a rule permitted it" is only half the authorization: eligibility
// with no ceiling is how one bad diagnosis becomes fifty restarts, and the blast-radius
// layer is the half that bounds it. An artifact that named the rule and not the
// admission would describe a permission that does not exist in this system.
func writeAdmission(b *strings.Builder, a Action) {
	b.WriteString("## The ceiling it ran under\n\n")
	fmt.Fprintf(b, "The blast-radius budget admitted this action (`%s`). Admission is a reservation: it consumed one of this pass's "+
		"auto-applies for cluster `%s` and started the cooldown on `%s`, so the same object will not be acted on unattended again until it expires.\n\n",
		cell(a.Grant.Reason.String()), a.Proposal.Cluster, a.Proposal.Target.String())
}

// writeRevocation is the one-signal kill switch, and it is a section of its own on
// every artifact rather than a line in a footer.
//
// The person who needs it is already reading this page at the moment they decide they
// want it. A revocation that sends them to a config file, a CLI, or another repository
// is one they perform later or not at all, and "later" is measured in unattended actions.
func writeRevocation(b *strings.Builder, a Action) {
	b.WriteString("## Revoking this\n\n")
	fmt.Fprintf(b, "**Add the `%s` label to this issue.** That revokes autonomy for the shape `%s` — every `%s` on cluster `%s` — "+
		"and it takes effect on MaKlaude's next cycle, which re-reads the open disclosures before it decides anything. "+
		"No configuration change, no restart, and nothing else to remember.\n\n",
		RevokedLabel, a.Shape().String(), a.Proposal.Operation, a.Proposal.Cluster)
	b.WriteString("Removing the label (or closing this issue) lifts the revocation. " +
		"It is an override rather than a demotion: it does not rewrite the trust ledger, because a person deciding to stop trusting a shape " +
		"is not something that happened to a cluster.\n\n")
	fmt.Fprintf(b, "To stop **all** unattended action on cluster `%s` instead of just this shape, trip its circuit breaker — see the autonomy section of MaKlaude's state summary.\n\n",
		a.Proposal.Cluster)
}

// outcomeHeading states the result in the heading, so the verdict is legible from a
// collapsed diff or a notification digest without opening the lifecycle table.
func outcomeHeading(o Outcome) string {
	switch {
	case o.Converged():
		return "## Outcome: the action landed and the cluster reached the expected state"
	case o.Report.DryRun:
		return "## Outcome: rehearsal complete — nothing was applied"
	case o.Report.CleanAbort():
		return "## Outcome: abandoned cleanly — the target moved and nothing was sent"
	case o.Report.Executed:
		return "## Outcome: the action landed and did NOT reach the expected state"
	default:
		return "## Outcome: the action did not run"
	}
}

// writeConsequence renders what the blast-radius layer decided must follow, and what
// was actually done about it.
//
// A zero consequence is stated rather than omitted. "Nothing follows from this" is a
// real result and the reader has to be able to tell it apart from a renderer that
// forgot the section — the same reason [budget.Status]'s lists are printed empty.
func writeConsequence(b *strings.Builder, o Outcome) {
	b.WriteString("## What followed\n\n")
	c := o.Consequence

	if !c.Acted() {
		b.WriteString("Nothing. The action succeeded, so the shape keeps its standing and the cluster's breaker stays closed.\n\n")
		return
	}

	fmt.Fprintf(b, "The blast-radius layer recorded a failure for this cluster (%d consecutive).\n\n", c.ConsecutiveFailures)
	if c.Tripped {
		b.WriteString("- **The cluster's circuit breaker has TRIPPED.** No further action will be auto-applied on it, by any rule, until a person clears it.\n")
	}
	switch {
	case o.RolledBack:
		fmt.Fprintf(b, "- Rollback: %s\n", redact.String(o.Rollback.String()))
	case c.RollBack && o.RollbackSkipped != "":
		fmt.Fprintf(b, "- Rollback was called for and not attempted: %s\n", redact.String(o.RollbackSkipped))
	case c.RollBack:
		b.WriteString("- Rollback was called for and no account of it was recorded.\n")
	}
	if c.Demote {
		if o.DemotionErr != "" {
			fmt.Fprintf(b, "- **The shape could not be demoted: %s.** It may still be trusted, so revoke it with the label above.\n", redact.String(o.DemotionErr))
		} else {
			b.WriteString("- The shape was demoted in the trust ledger, which re-gates it until its history recovers.\n")
		}
	}
	if c.Escalate {
		if o.EscalationErr != "" {
			fmt.Fprintf(b, "- **This could not be escalated: %s.**\n", redact.String(o.EscalationErr))
		} else {
			fmt.Fprintf(b, "- Escalated: this artifact carries `%s`.\n", NeedsHumanLabel)
		}
	}
	b.WriteString("\nThe failed action is **not** retried. Nobody was watching when it ran, so a retry would compound an outcome no person has yet seen.\n\n")
}

// ExecutionComment is the note posted the instant a mutation lands, before the
// observation window opens.
//
// It exists as a comment rather than only as a body rewrite because a comment sends a
// notification and a body edit does not. The window is up to a minute and a half; a
// person subscribed to this trail should learn that their cluster changed at the moment
// it changed, not when the verdict is in.
func ExecutionComment(detail string) string {
	return bannerNoHuman + "\n\n" + redact.String(detail)
}

// OutcomeComment is the note posted once the attempt is finished. detail is the
// already-redacted lifecycle rendering.
func OutcomeComment(o Outcome, detail string) string {
	var b strings.Builder
	b.WriteString(strings.TrimPrefix(outcomeHeading(o), "## "))
	b.WriteString("\n\n")
	b.WriteString(detail)
	return b.String()
}

// EscalationComment is what a failed unattended action says to the person it is being
// pushed to. It leads with the fact that nobody saw it happen, because that is what
// makes this different from a failed action somebody approved.
func EscalationComment(a Action, o Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Needs a person\n\nAn unattended `%s` on `%s` (cluster `%s`) failed. Nobody was watching when it ran, so nobody learns about it unless it is pushed here.\n\n",
		a.Proposal.Operation, a.Proposal.Target.String(), a.Proposal.Cluster)
	if o.Consequence.Tripped {
		fmt.Fprintf(&b, "This failure **tripped cluster `%s`'s circuit breaker**: nothing further will be auto-applied on it, by any rule, until a person clears it.\n\n",
			a.Proposal.Cluster)
	}
	fmt.Fprintf(&b, "It was authorized by rule `%s` under this trust evidence:\n\n> %s\n\n",
		cell(a.Verdict.Rule), redact.String(a.Verdict.Evidence))
	fmt.Fprintf(&b, "If that rule should not have fired, add `%s` to this issue and the shape `%s` stops being auto-applied on the next cycle.\n",
		RevokedLabel, a.Shape().String())
	return b.String()
}

// ChatSummary is the one-line chat form. It carries the banner, the action, the rule,
// and the outcome, and nothing else: a chat message that has to be read to the end to
// learn a cluster changed is one that gets read to the end once.
func ChatSummary(a Action, o Outcome) string {
	verdict := "did not run"
	switch {
	case o.Converged():
		verdict = "converged"
	case o.Report.DryRun:
		verdict = "rehearsed only, cluster unchanged"
	case o.Report.CleanAbort():
		verdict = "abandoned cleanly, nothing sent"
	case o.Report.Executed:
		verdict = "applied but did NOT converge"
	}
	return fmt.Sprintf("%s `%s` on `%s` (cluster `%s`) — %s. Authorized by rule `%s`; no approver.",
		bannerNoHuman, a.Proposal.Operation, a.Proposal.Target.String(), a.Proposal.Cluster, verdict, cell(a.Verdict.Rule))
}

// cell renders a value for a markdown table cell, substituting a visible placeholder
// for an empty one so a row never reads as a missing column.
func cell(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not recorded"
	}
	return s
}

// stamp renders a timestamp in the trail's fixed UTC form, or says plainly that it was
// never set.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "not recorded"
	}
	return t.UTC().Format(time.RFC3339)
}
