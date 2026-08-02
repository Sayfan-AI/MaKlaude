package audit

import (
	"fmt"
	"strings"
	"time"
)

// Lifecycle renders one action's audit records as the comms artifact a human
// reads: the whole story of what was proposed, who allowed it, what ran, whether it
// worked, and how to reverse it.
//
// # Why the rendering, not just the records
//
// The records are complete but they are a table of structs, and the audience for an
// audit trail is a person reconstructing an incident under time pressure. What they
// need first is the shape — did this get as far as running? did anyone check? — and
// only then the detail. So the output leads with a one-line lifecycle chain,
// follows with the ordered steps, and puts the three things a reader always ends up
// hunting for (who authorized it, what the object looked like before, how to undo
// it) in named sections underneath.
//
// # The proposed stage has no record and is rendered anyway
//
// Nothing appends a [PhaseProposed] record — see the phase's own documentation for
// why stamping one at execution time would assert a false instant. The stage is
// real regardless, and its true time is carried on every record as
// [Action.ProposedAt], so it is rendered from there. A lifecycle that silently began
// at "approved" would leave a reader unable to see how long the action sat waiting
// for a human, which is one of the more interesting numbers in an incident review.
//
// # It redacts, again
//
// Records that came from [Trail.Append] are already sanitized, and this sanitizes
// them a second time. That is not defensive padding: it makes "anything Lifecycle
// emits has been redacted" true of the function rather than true of the way it
// happens to be called today, and redaction is idempotent so the second pass costs
// a regex sweep and changes nothing. The alternative is a rendering path whose
// safety depends on a caller having used the right source, which is precisely the
// kind of invariant that survives review and then quietly stops holding.
//
// Given no records it says so rather than returning an empty string, because an
// empty comms artifact reads as "nothing to report" and the absence of audit records
// for an action that ran is the opposite of nothing to report.
func Lifecycle(recs []Record) string {
	if len(recs) == 0 {
		return "**Audit trail: no records.** MaKlaude has no audit record for this action. " +
			"If an action ran, this is a bug in the execution layer, not evidence that nothing happened."
	}

	safe := make([]Record, 0, len(recs))
	for _, rec := range recs {
		safe = append(safe, rec.redacted())
	}
	head := safe[0]

	var b strings.Builder
	fmt.Fprintf(&b, "## Audit trail — `%s` on `%s`\n\n", head.Action.Operation, head.Action.Target.String())
	fmt.Fprintf(&b, "**Lifecycle:** %s\n\n", chain(safe))
	writeProposed(&b, head)
	writeSteps(&b, safe)
	writeTiming(&b, safe)
	writeAuthority(&b, safe)
	writePreState(&b, safe)
	writeRollback(&b, safe)
	return b.String()
}

// chain renders the one-line stage summary, listing only the stages actually
// reached and annotating the terminal one with its verdict. A reader who reads
// nothing else should still learn whether the cluster changed and whether anyone
// checked that it worked.
func chain(recs []Record) string {
	stages := []string{"proposed"}
	var terminal string

	if has(recs, func(r Record) bool { return r.Phase == PhaseApproved || r.Approver.Attributed() }) {
		stages = append(stages, "approved")
	}
	if rec, ok := find(recs, func(r Record) bool { return r.Phase == PhaseExecuted }); ok {
		if rec.Change.Applied {
			stages = append(stages, "executed")
		} else {
			stages = append(stages, "previewed")
		}
	}
	if rec, ok := find(recs, func(r Record) bool { return r.Phase == PhaseVerified }); ok {
		stages = append(stages, "verified")
		terminal = rec.Outcome.Convergence
	}
	if rec, ok := find(recs, func(r Record) bool { return r.Phase == PhaseFailed }); ok {
		stages = append(stages, "failed")
		terminal = rec.Outcome.Failure
		if rec.Outcome.CleanAbort {
			terminal += " — abandoned cleanly, nothing was applied"
		}
	}
	if rec, ok := find(recs, func(r Record) bool { return r.Phase == PhaseRolledBack }); ok {
		stages = append(stages, "rolled back")
		terminal = "the action's effect was undone"
		if rec.Rollback.AlreadyAtPreState {
			terminal = "already back at its pre-action state; nothing was sent"
		}
	}

	line := strings.Join(stages, " → ")
	if terminal != "" {
		line += " (" + terminal + ")"
	}
	return line
}

// writeProposed renders the stage no record carries, from the proposal's own time.
func writeProposed(b *strings.Builder, head Record) {
	fmt.Fprintf(b, "**Proposed** %s — %s (cluster `%s`, reversibility **%s**).\n\n",
		stamp(head.Action.ProposedAt), titleOf(head.Action), head.Action.Cluster, head.Action.Reversibility)
}

// writeSteps renders the ordered records. Sequence is its own column because it is
// what ordering is defined by; the recorded time is shown alongside it and is
// explicitly labelled as the time of RECORDING, so nobody reads it as the time the
// step happened.
func writeSteps(b *strings.Builder, recs []Record) {
	b.WriteString("| Step | Phase | Recorded (UTC) | What happened |\n|---|---|---|---|\n")
	for _, rec := range recs {
		fmt.Fprintf(b, "| %d | %s | %s | %s |\n", rec.Seq, rec.Phase, stamp(rec.RecordedAt), cell(rec.summary()))
	}
	b.WriteString("\n")
}

// writeTiming renders how long the attempt actually took, from the first check to
// the last observation.
//
// It is worth a line of its own because the numbers around it are all bounds rather
// than measurements — the observation window is a maximum, the attempt cap is a
// maximum — and a reader comparing "watched for 90s" against a policy that permits
// 90s cannot tell whether the window elapsed or the action simply finished. The
// elapsed wall-clock time is the one figure in the artifact that is neither
// configured nor capped.
func writeTiming(b *strings.Builder, recs []Record) {
	rec, ok := find(recs, func(r Record) bool { return r.Change.Duration() > 0 })
	if !ok {
		return
	}
	fmt.Fprintf(b, "The attempt ran for %s, from %s to %s.\n\n",
		rec.Change.Duration().Round(time.Millisecond), stamp(rec.Change.StartedAt), stamp(rec.Change.FinishedAt))
}

// writeAuthority renders who authorized the action, stating the kind of authority in
// so many words. It is its own section rather than a table cell because "on whose
// authority" is half of what an audit trail is for, and because a policy-waived
// action must be impossible to skim past as though a person had signed it.
func writeAuthority(b *strings.Builder, recs []Record) {
	rec, ok := find(recs, func(r Record) bool { return r.Approver.Attributed() })
	if !ok {
		b.WriteString("**Authorized by:** nobody. No valid authorization covered this action, " +
			"so MaKlaude refused it — this record exists to say the attempt was made.\n\n")
		return
	}

	ap := rec.Approver
	detail := joinNonEmpty(", ",
		stampedPhrase("decision recorded", ap.ApprovedAt),
		stampedPhrase("honored by the gate", ap.AuthorizedAt),
		refDetail(ap.Ref))
	fmt.Fprintf(b, "%s.\n\n", joinNonEmpty(" — ", "**Authorized by:** "+ap.String(), detail))

	if !ap.Authority.HumanReviewed() {
		b.WriteString("> No human reviewed this action. It ran on configured policy alone.\n\n")
	}
}

// writePreState renders what the object looked like before MaKlaude touched it —
// the record a rollback is computed against, and the answer to "what exactly did
// this change?".
func writePreState(b *strings.Builder, recs []Record) {
	rec, ok := find(recs, func(r Record) bool { return r.PreState.Captured })
	if !ok {
		return
	}
	pre := rec.PreState
	fmt.Fprintf(b, "**State before the action** — %s at resourceVersion `%s`, read %s:\n\n",
		pre.Kind, pre.ResourceVersion, stamp(pre.ObservedAt))
	b.WriteString("| Field | Value before |\n|---|---|\n")
	for _, f := range pre.Fields {
		fmt.Fprintf(b, "| %s | `%s` |\n", f.Name, f.Value)
	}
	b.WriteString("\n")
}

// writeRollback renders how to reverse the action, and whether MaKlaude can do it.
//
// The "can" is stated separately from the "how" because they answer different
// questions: an operator wants to know what reversal would take even when nothing
// here can perform it. And the case checked first is the one that reads worst if it
// is got wrong — an attempt that applied nothing has nothing to undo, and offering a
// rollback for it would invite an operator to reverse a change that was never made.
func writeRollback(b *strings.Builder, recs []Record) {
	rec, ok := find(recs, func(r Record) bool { return r.Rollback.Kind != "" })
	if !ok {
		return
	}
	rb := rec.Rollback
	applied := has(recs, func(r Record) bool { return r.Change.Applied })

	fmt.Fprintf(b, "**Rollback:** %s", rb.Kind)
	if rb.Note != "" {
		fmt.Fprintf(b, " — %s", rb.Note)
	}
	b.WriteString("\n\n")

	switch {
	case rb.Performed:
		b.WriteString("MaKlaude has already performed this rollback; the original action remains recorded above.\n")
	case rb.AlreadyAtPreState:
		b.WriteString("A rollback was requested and nothing was sent: the target was already back at its pre-action state.\n")
	case !applied:
		b.WriteString("Nothing was applied, so there is nothing to undo.\n")
	case rb.Available:
		b.WriteString("MaKlaude captured the pre-state and can perform this rollback on request. It will not do so on its own.\n")
	default:
		b.WriteString("MaKlaude cannot perform this rollback itself.\n")
	}
}

// titleOf returns the action's title, falling back to the operation when a proposal
// carried none, so the rendered line never reads "**Proposed** … —  (cluster …)".
func titleOf(a Action) string {
	if strings.TrimSpace(a.Title) != "" {
		return a.Title
	}
	return string(a.Operation)
}

// refDetail renders the approval artifact reference, or nothing when there is none.
func refDetail(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return ""
	}
	return "approval artifact `" + ref + "`"
}

// cell makes a string safe to put in a markdown table cell: pipes would split it
// into extra columns and newlines would end the row, and the audit trail carries
// API server errors, which contain both.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// find returns the first record matching pred.
func find(recs []Record, pred func(Record) bool) (Record, bool) {
	for _, rec := range recs {
		if pred(rec) {
			return rec, true
		}
	}
	return Record{}, false
}

// has reports whether any record matches pred.
func has(recs []Record, pred func(Record) bool) bool {
	_, ok := find(recs, pred)
	return ok
}
