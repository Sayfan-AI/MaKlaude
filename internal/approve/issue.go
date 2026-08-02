package approve

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Labels applied to, and read from, an approval artifact.
//
// # Why a separate managed label from the escalation trail
//
// [escalate.ManagedLabel] ("maklaude") marks incident issues, and
// [escalate.GitHubSink.ListOpen] fetches every issue carrying it. If approval
// artifacts carried the same label the two trails would list each other's issues
// on every pass — surviving only because each parser skips bodies without its own
// marker, which is a coincidence of implementation rather than a boundary. A
// distinct label makes the two trails disjoint at the QUERY, so neither can act on
// the other's issues even if a marker is ever mis-parsed. It also means an
// operator can subscribe to approval requests without subscribing to every
// diagnosis.
const (
	// ManagedLabel marks an issue as a MaKlaude approval artifact. It is the coarse
	// query filter; the authoritative key is still the embedded proposal marker.
	ManagedLabel = "maklaude-proposal"

	// NeedsHumanLabel marks an artifact as awaiting a decision, matching the label
	// the escalation trail uses so an operator has one thing to watch.
	NeedsHumanLabel = "needs:human"

	// ApprovedLabel is the approval signal. A human adding it authorizes THIS action
	// against the cluster state the artifact displays — nothing broader, and not
	// forever. The label event supplies the approver identity and timestamp the gate
	// records; that attribution is why the signal is a label and not a comment.
	ApprovedLabel = "approved"

	// RejectedLabel is the rejection signal, deliberately as cheap to give as the
	// approval. Declining must not be harder than allowing, or the gate quietly
	// biases toward yes.
	RejectedLabel = "rejected"

	// ExecutedLabel records that the authorized action has run. It is the durable
	// idempotency flag: a label survives a process restart, is returned by the same
	// list call that recovers everything else, and carries its own timestamped event,
	// so a crash between acting and recording cannot yield a second execution.
	ExecutedLabel = "maklaude-executed"
)

// Hidden HTML-comment markers embedded in an artifact body. Rendered markdown
// hides them, so the artifact reads cleanly to a human while carrying the state
// the gate needs to rediscover across process restarts.
//
// The prefixes are deliberately distinct from the escalation trail's
// ("maklaude:identity=", "maklaude:thread="): an approval artifact must never be
// mistaken for an incident issue, in either direction, even if the two ever share
// a label by accident.
const (
	proposalMarkerPrefix = "<!-- maklaude:proposal="
	proposalMarkerSuffix = " -->"

	// The preview marker carries BOTH the displayed resourceVersion and when it was
	// displayed, as "<rv>@<RFC3339>". They travel together because the two drift
	// checks need them together: the version answers "is this still the object the
	// human saw?" and the instant answers "was the approval given after we last
	// changed what they were looking at?". Splitting them into two markers would
	// allow a body carrying one without the other, which has no correct meaning.
	previewMarkerPrefix = "<!-- maklaude:preview="
	previewMarkerSuffix = " -->"

	// The dry-run OUTCOME the body currently shows, kept as its own marker rather
	// than folded into the one above. The two have different absence semantics —
	// a missing preview marker means the gate cannot tell what state was displayed
	// and must refuse a stale approval, while a missing state marker merely means
	// "re-render to be sure" — and a single marker cannot carry both without one of
	// the readings becoming wrong.
	previewStateMarkerPrefix = "<!-- maklaude:preview-state="
	previewStateMarkerSuffix = " -->"

	// The approval posture the body DESCRIBES, kept as its own marker for the same
	// reason the preview-state marker is its own: it changes independently of
	// everything else, and a body that describes the wrong one is wrong in a way a
	// reader cannot detect. See [PendingAction.GateMode].
	gateMarkerPrefix = "<!-- maklaude:gate="
	gateMarkerSuffix = " -->"

	// The chat thread handle, patched in after the chat root is posted, exactly as
	// the escalation trail does — the artifact is the durable store for thread
	// continuity so no second datastore is introduced.
	threadMarkerPrefix = "<!-- maklaude:proposal-thread="
	threadMarkerSuffix = " -->"
)

// Gate-mode tokens recorded in the gate marker. They name the posture the body was
// written under, not the posture in force now — comparing the two is what forces a
// refresh when an operator flips the bypass.
const (
	gateHumanGated = "human-gated"
	gateAutonomous = "autonomous-no-human-review"
)

// gateModeToken renders the posture a body written under this policy would describe.
func gateModeToken(policy Policy) string {
	if policy.AutoApprove {
		return gateAutonomous
	}
	return gateHumanGated
}

// gateMarker renders the hidden marker embedding the described approval posture.
func gateMarker(policy Policy) string {
	return gateMarkerPrefix + gateModeToken(policy) + gateMarkerSuffix
}

// ParseGateMarker extracts the approval posture the body describes, returning an empty
// string when no marker is present. Absence is reported as unknown rather than
// defaulted to the human-gated token, so a body predating the marker re-renders
// instead of being assumed to describe the posture currently in force.
func ParseGateMarker(body string) string {
	v, ok := betweenMarkers(body, gateMarkerPrefix, gateMarkerSuffix)
	if !ok {
		return ""
	}
	return v
}

// proposalMarker renders the hidden marker embedding a proposal identity.
func proposalMarker(id remediate.ProposalIdentity) string {
	return proposalMarkerPrefix + string(id) + proposalMarkerSuffix
}

// previewMarker renders the hidden marker embedding the displayed resourceVersion
// and the instant it was displayed.
func previewMarker(resourceVersion string, at time.Time) string {
	return previewMarkerPrefix + resourceVersion + "@" + at.UTC().Format(time.RFC3339) + previewMarkerSuffix
}

// threadMarker renders the hidden marker embedding a chat thread handle.
func threadMarker(threadTS string) string {
	return threadMarkerPrefix + threadTS + threadMarkerSuffix
}

// Preview state tokens recorded in the preview-state marker. They describe the
// SHAPE of the dry-run section a body is showing, not its wording: a summary or
// diff that changes without the target's resourceVersion moving is the same object
// described slightly differently, whereas a dry-run that flips between accepted,
// rejected, and not-attempted changes what the reader is being asked to weigh.
const (
	previewStateOK     = "ok"
	previewStateFailed = "failed"
	previewStateNone   = "none"
)

// previewStateToken renders which of the three dry-run sections [writePreview]
// would produce for this preview.
func previewStateToken(p Preview) string {
	switch {
	case p.Failed():
		return previewStateFailed
	case !p.Performed:
		return previewStateNone
	default:
		return previewStateOK
	}
}

// previewStateMarker renders the hidden marker embedding the displayed dry-run
// outcome.
func previewStateMarker(p Preview) string {
	return previewStateMarkerPrefix + previewStateToken(p) + previewStateMarkerSuffix
}

// ParsePreviewStateMarker extracts the displayed dry-run outcome, returning an empty
// string when no marker is present. Absence is reported as unknown rather than
// defaulted to a token, so a body predating the marker re-renders instead of being
// assumed current.
func ParsePreviewStateMarker(body string) string {
	v, ok := betweenMarkers(body, previewStateMarkerPrefix, previewStateMarkerSuffix)
	if !ok {
		return ""
	}
	return v
}

// ParseProposalMarker extracts the embedded [remediate.ProposalIdentity] from an
// artifact body, returning ok=false when no well-formed marker is present. A sink
// uses it to recognize its own artifacts; a body without it is not MaKlaude's to
// manage even if it carries the label.
func ParseProposalMarker(body string) (remediate.ProposalIdentity, bool) {
	raw, ok := betweenMarkers(body, proposalMarkerPrefix, proposalMarkerSuffix)
	if !ok {
		return "", false
	}
	return remediate.ProposalIdentity(raw), true
}

// ParsePreviewMarker extracts the displayed resourceVersion and the instant it was
// displayed. ok is false when the marker is missing or malformed.
//
// A malformed marker is reported as ABSENT rather than partially trusted. The
// fields it carries are both used to refuse an approval, so a half-parsed value
// would relax a safety check on the strength of a corrupt body — the failure
// direction has to be "we do not know, so do not honor a stale approval", which is
// what an absent preview produces (drift comparison against an empty string fails
// for any real resourceVersion).
func ParsePreviewMarker(body string) (resourceVersion string, at time.Time, ok bool) {
	raw, found := betweenMarkers(body, previewMarkerPrefix, previewMarkerSuffix)
	if !found {
		return "", time.Time{}, false
	}
	// Split on the LAST "@": a resourceVersion is opaque to us and could in
	// principle contain one, while the RFC3339 suffix never does.
	i := strings.LastIndex(raw, "@")
	if i < 0 {
		return "", time.Time{}, false
	}
	rv := strings.TrimSpace(raw[:i])
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(raw[i+1:]))
	if err != nil || rv == "" {
		return "", time.Time{}, false
	}
	return rv, ts.UTC(), true
}

// ParseThreadMarker extracts the embedded chat thread handle, ok=false when absent
// (the normal case before a chat root is posted, or when chat is unconfigured).
func ParseThreadMarker(body string) (string, bool) {
	return betweenMarkers(body, threadMarkerPrefix, threadMarkerSuffix)
}

// betweenMarkers returns the trimmed content of the first prefix/suffix pair in
// body. It tolerates surrounding content and reads only the first occurrence, so
// human edits or quoted text later in the body cannot break recovery.
func betweenMarkers(body, prefix, suffix string) (string, bool) {
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return "", false
	}
	v := strings.TrimSpace(rest[:end])
	if v == "" {
		return "", false
	}
	return v, true
}

// withThreadMarker returns body with the chat thread marker set to threadTS,
// replacing any marker already present so a regenerated body never accumulates
// stale handles or loses the durable one. An empty threadTS strips the marker,
// which is correct when chat is unconfigured.
func withThreadMarker(body, threadTS string) string {
	body = stripMarker(body, threadMarkerPrefix, threadMarkerSuffix)
	if strings.TrimSpace(threadTS) == "" {
		return body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + threadMarker(threadTS) + "\n"
}

// stripMarker removes the first prefix/suffix marker (and one trailing newline) so
// re-rendering never duplicates it.
func stripMarker(body, prefix, suffix string) string {
	start := strings.Index(body, prefix)
	if start < 0 {
		return body
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return body
	}
	markerEnd := start + len(prefix) + end + len(suffix)
	if markerEnd < len(body) && body[markerEnd] == '\n' {
		markerEnd++
	}
	return body[:start] + body[markerEnd:]
}

// Title renders the artifact's title: the approval marker, the cluster, the action,
// and the object. An operator scanning a list of issues can tell what would happen
// to which object on which cluster without opening any of them — which matters
// most in exactly the situation this gate exists for, several clusters and several
// pending decisions at once.
func Title(req Request) string {
	p := req.Proposal
	return fmt.Sprintf("[APPROVAL][%s] %s — %s", p.Cluster, p.Title, p.Target.String())
}

// ApprovalSummary renders the text posted as the ROOT of the chat thread when an
// approval is first requested. It leads with the action and its reversibility
// class, because in a chat channel the question an operator answers first is "how
// much attention does this need?", and points at the artifact for the full preview
// rather than trying to fit one into a chat message.
//
// It is content-only, like [escalate.EscalationSummary]: the notify layer adds the
// banner, the link, and any mention.
//
// The last line changes with the policy, and that is the whole reason this takes one.
// "Nothing runs until a human adds the `approved` label" is the single most important
// sentence in the message and it is FALSE under the bypass — a chat notice that keeps
// saying it while MaKlaude acts unattended is worse than no notice, because a reader
// who has seen it a hundred times will not re-read it on the hundred-and-first.
func ApprovalSummary(req Request, policy Policy) string {
	p := req.Proposal
	closing := fmt.Sprintf("Nothing runs until a human adds the `%s` label to the linked issue.", ApprovedLabel)
	if policy.AutoApprove {
		closing = fmt.Sprintf(
			"AUTONOMOUS MODE: MaKlaude will run this itself, with NO human review, on its next pass. Add the `%s` label to the linked issue to stop it.",
			RejectedLabel)
	}
	return fmt.Sprintf("%s\nAction: %s on `%s` (cluster `%s`)\nReversibility: %s\n%s",
		p.Title, p.Operation, p.Target.String(), p.Cluster, p.Reversibility, closing)
}

// Body renders the full approval artifact: exactly what will run, on which
// cluster, the dry-run evidence, the reversibility class and rollback plan, the
// diagnosis it addresses, the preconditions that will be rechecked, and how to
// decide — followed by the hidden markers the gate rediscovers its own state from.
//
// # The body is the preview, so what it omits is a safety question
//
// A human approving this is authorizing a mutation of a production cluster on the
// strength of this text alone. So every section exists because its absence would
// let someone approve something they did not understand: the operation and target
// (what), the dry-run (does the server even accept it), the reversibility and
// rollback plan (what it costs to be wrong), the diagnosis and evidence (why anyone
// thinks this is the fix), and the preconditions (what will be rechecked before it
// actually runs). Where a section has nothing to show — no dry-run was performed —
// it SAYS SO in those words rather than being omitted, because an absent section
// reads as "nothing to worry about" and a missing dry-run is precisely something to
// worry about.
//
// # Under the bypass the body is a notice, not a question, and says so first
//
// The opening line is a promise ("nothing runs until a human adds a label") that
// [Policy.AutoApprove] breaks. A body that keeps making it while MaKlaude acts
// unattended is not a stale detail — it is the artifact actively misinforming the one
// person who could still stop the action, at the moment their attention is worth most.
// So the policy is threaded in and the lead is replaced, and the posture is recorded in
// a hidden marker so flipping the mode forces every open artifact to re-render before
// anything is authorized under it (see [PendingAction.GateMode]).
//
// previewedAt is stamped into the hidden preview marker and shown in the body, so
// the instant the approval is judged against is the same instant the human can see.
func Body(req Request, previewedAt time.Time, policy Policy) string {
	p := req.Proposal
	var b strings.Builder

	if policy.AutoApprove {
		fmt.Fprintf(&b, "MaKlaude is about to run **one mutating action** on cluster **%s**, and it is **not waiting for you**.\n\n", p.Cluster)
		fmt.Fprintf(&b, "> **AUTONOMOUS MODE IS ENABLED** (`%s`). MaKlaude will authorize this action itself on its next reconciliation pass — "+
			"**no human will review it**, and the audit trail will record it as waived by policy rather than approved by a person. "+
			"This issue is a notice, not a question.\n>\n"+
			"> Add the `%s` label to stop it: a human's refusal still overrides autonomous mode. The `%s` label is on this issue so you SEE it, "+
			"not because MaKlaude is waiting for you.\n\n",
			AutoApproveEnv, RejectedLabel, NeedsHumanLabel)
	} else {
		fmt.Fprintf(&b, "MaKlaude is requesting approval to run **one mutating action** on cluster **%s**.\n\n", p.Cluster)
		fmt.Fprintf(&b, "> Nothing runs until a human adds the `%s` label to this issue. Adding `%s` declines it. "+
			"An approval authorizes **this action, on this object, at the cluster state shown below, once** — it is never a standing grant.\n\n",
			ApprovedLabel, RejectedLabel)
	}

	writeActionTable(&b, req, previewedAt)
	writeWhatWillRun(&b, req)
	writePreview(&b, req)
	writeRollback(&b, req)
	writeDiagnosis(&b, req)
	writePreconditions(&b, req)
	writeHowToDecide(&b, req, policy)

	fmt.Fprintf(&b, "\n---\n*Requested automatically by MaKlaude. This issue is refreshed while the proposal stands, "+
		"and is **withdrawn without running anything** if the problem resolves on its own.*\n")

	// The markers MUST be present and parseable: they are the durable record of
	// which proposal this is, which cluster state it was previewed against, and which
	// approval posture the text above describes.
	fmt.Fprintf(&b, "\n%s\n", proposalMarker(p.Identity))
	fmt.Fprintf(&b, "%s\n", previewMarker(p.Target.ResourceVersion, previewedAt))
	fmt.Fprintf(&b, "%s\n", previewStateMarker(req.Preview))
	fmt.Fprintf(&b, "%s\n", gateMarker(policy))
	return b.String()
}

// writeActionTable renders the at-a-glance facts. Reversibility sits high in the
// table on purpose: it is the field that should set the reader's level of scrutiny
// before they read anything else.
func writeActionTable(b *strings.Builder, req Request, previewedAt time.Time) {
	p := req.Proposal
	fmt.Fprintf(b, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(b, "| Cluster | `%s` |\n", p.Cluster)
	fmt.Fprintf(b, "| Operation | `%s` |\n", p.Operation)
	fmt.Fprintf(b, "| Target | `%s` |\n", p.Target.String())
	fmt.Fprintf(b, "| Reversibility | **%s** |\n", p.Reversibility)
	fmt.Fprintf(b, "| Target resourceVersion | `%s` |\n", p.Target.ResourceVersion)
	fmt.Fprintf(b, "| Diagnosed cause | `%s` (confidence %s) |\n", p.Cause, p.Confidence)
	fmt.Fprintf(b, "| Proposed at | %s |\n", p.ProposedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(b, "| Previewed at | %s |\n", previewedAt.UTC().Format("2006-01-02 15:04:05 MST"))
}

// writeWhatWillRun states the action in plain language: MaKlaude's intent and the
// effect it expects, both carried from the proposal rather than re-worded here.
func writeWhatWillRun(b *strings.Builder, req Request) {
	p := req.Proposal
	b.WriteString("\n## Exactly what will run\n\n")
	fmt.Fprintf(b, "**%s** — `%s` on `%s` in cluster `%s`.\n\n", p.Title, p.Operation, p.Target.String(), p.Cluster)
	fmt.Fprintf(b, "- **Why:** %s\n", p.Intent)
	fmt.Fprintf(b, "- **Expected effect:** %s\n", p.ExpectedEffect)
}

// writePreview renders the dry-run evidence, and states plainly when there is
// none. A failed dry-run is rendered as a prominent refusal notice, not as a
// footnote: the artifact stays open so an operator can see what MaKlaude wanted to
// do and why the server said no, but the gate will not authorize it however it is
// labelled.
func writePreview(b *strings.Builder, req Request) {
	b.WriteString("\n## Dry-run preview\n\n")
	switch {
	case req.Preview.Failed():
		fmt.Fprintf(b, "> **The dry-run FAILED.** The API server rejected this action as a preview, so MaKlaude will not execute it even if this issue is approved.\n\n")
		fmt.Fprintf(b, "```\n%s\n```\n", strings.TrimSpace(req.Preview.Error))
	case !req.Preview.Performed:
		fmt.Fprintf(b, "_No dry-run was performed for this action._ The effect described above is MaKlaude's deterministic expectation, **not** something the API server has validated. Weigh it accordingly.\n")
	default:
		if s := strings.TrimSpace(req.Preview.Summary); s != "" {
			fmt.Fprintf(b, "%s\n", s)
		}
		if d := strings.TrimSpace(req.Preview.Diff); d != "" {
			fmt.Fprintf(b, "\n```diff\n%s\n```\n", d)
		}
	}
}

// writeRollback renders the reversibility class and the concrete plan for undoing
// the action. An operation with no defined plan renders an explicit warning AND is
// refused by [Decide] — the text and the gate agree, so a reader is never told
// something the code will not honor.
func writeRollback(b *strings.Builder, req Request) {
	p := req.Proposal
	b.WriteString("\n## Reversibility and rollback\n\n")
	fmt.Fprintf(b, "**Class: %s.** %s\n\n", p.Reversibility, reversibilityMeaning(p.Reversibility))
	plan, ok := rollbackPlan(p.Operation)
	if !ok {
		fmt.Fprintf(b, "> **No rollback plan is defined for operation `%s`.** MaKlaude will refuse to execute this action regardless of approval. This is a bug in the operation catalog, not a decision for you to make.\n", p.Operation)
		return
	}
	fmt.Fprintf(b, "**Rollback plan:** %s\n", plan)
}

// writeDiagnosis renders why the action is being proposed at all: the cause, the
// confidence, the identities that link back through the hypothesis to its incident,
// and the exact findings that bear on this action. Carrying the identities makes
// the chain auditable without re-deriving anything.
func writeDiagnosis(b *strings.Builder, req Request) {
	p := req.Proposal
	b.WriteString("\n## Diagnosis this addresses\n\n")
	fmt.Fprintf(b, "- **Cause:** `%s` (confidence **%s**)\n", p.Cause, p.Confidence)
	fmt.Fprintf(b, "- **Hypothesis:** `%s`\n", p.Hypothesis)
	fmt.Fprintf(b, "- **Incident:** `%s`\n", p.Incident)
	if len(p.Evidence) == 0 {
		return
	}
	b.WriteString("\nEvidence:\n")
	for i := range p.Evidence {
		e := p.Evidence[i]
		fmt.Fprintf(b, "- `%s` [%s] %s — %s\n", e.Object.String(), e.Severity, e.Title, e.Message)
	}
}

// writePreconditions lists what will be rechecked immediately before the action
// runs. It is shown to the approver, not just held for the executor, because it is
// the honest answer to "what if things change between my clicking approve and this
// running?" — the answer is that these are verified again and the action is
// abandoned if any fails.
func writePreconditions(b *strings.Builder, req Request) {
	p := req.Proposal
	b.WriteString("\n## Rechecked immediately before running\n\n")
	if len(p.Preconditions) == 0 {
		b.WriteString("_This action carries no preconditions._\n")
		return
	}
	for i := range p.Preconditions {
		pc := p.Preconditions[i]
		fmt.Fprintf(b, "- `%s` — %s\n", pc.Kind, pc.Description)
	}
	fmt.Fprintf(b, "\nIf any of these no longer holds, the action is abandoned rather than run.\n")
}

// writeHowToDecide spells out the mechanism and the scope of the decision. It
// states the scope limits as consequences ("if the object changes, your approval
// stops applying") rather than as policy, because that is how an approver needs to
// understand them: not as rules MaKlaude follows but as the boundaries of what
// they are agreeing to.
//
// Under the bypass it becomes "how to stop this", which is the only decision left to a
// reader. It still lists what remains in force, because the useful thing to tell
// someone who has just learned that nobody reviewed an action is precisely which
// protections did not go away with the review.
func writeHowToDecide(b *strings.Builder, req Request, policy Policy) {
	if policy.AutoApprove {
		writeHowToStop(b, req)
		return
	}
	b.WriteString("\n## How to decide\n\n")
	fmt.Fprintf(b, "- **Approve:** add the `%s` label. MaKlaude records who added it and when.\n", ApprovedLabel)
	fmt.Fprintf(b, "- **Decline:** add the `%s` label. The action is not run and is not re-proposed while this issue stays open.\n", RejectedLabel)
	b.WriteString("\nYour approval covers this action only, and stops applying if:\n\n")
	fmt.Fprintf(b, "- the target object changes (its resourceVersion moves away from `%s`),\n", req.Proposal.Target.ResourceVersion)
	b.WriteString("- this issue is refreshed with a newer preview after you approved,\n")
	b.WriteString("- too much time passes before the next reconciliation runs it, or\n")
	b.WriteString("- the problem resolves on its own, in which case this is withdrawn without running anything.\n")
	b.WriteString("\nIn any of those cases MaKlaude asks again with fresh evidence rather than acting on a stale decision.\n")
}

// writeHowToStop is the bypass's replacement for the decide section.
func writeHowToStop(b *strings.Builder, req Request) {
	b.WriteString("\n## How to stop this\n\n")
	fmt.Fprintf(b, "- **Stop it:** add the `%s` label. A human's refusal overrides autonomous mode, and this action is not run "+
		"and not re-proposed while this issue stays open.\n", RejectedLabel)
	fmt.Fprintf(b, "- **Stop everything:** unset `%s` on the MaKlaude process. Every open request then goes back to waiting for a human.\n", AutoApproveEnv)
	fmt.Fprintf(b, "- **Approving is not required.** Adding `%s` from your own account does not change whether this runs — it changes what the audit "+
		"trail records, from a policy waiver to a named human approval. Worth doing only if you actually reviewed it.\n", ApprovedLabel)

	b.WriteString("\nAutonomous mode waives the requirement for consent and **nothing else**. This action is still abandoned without running if:\n\n")
	fmt.Fprintf(b, "- the target object changes (its resourceVersion moves away from `%s`),\n", req.Proposal.Target.ResourceVersion)
	b.WriteString("- the dry-run above starts failing,\n")
	b.WriteString("- any precondition listed above no longer holds when it is re-checked immediately before the action, or\n")
	b.WriteString("- the problem resolves on its own, in which case this is withdrawn without running anything.\n")
	b.WriteString("\nWhat is missing, and cannot be added afterwards, is a person having looked at it.\n")
}

// reversibilityMeaning renders, in a sentence, what a reversibility class actually
// costs an operator if the action turns out to be wrong. The enum's token alone
// ("recreated-by-controller") does not tell a reader whether their data is coming
// back.
func reversibilityMeaning(r remediate.Reversibility) string {
	switch r {
	case remediate.ReversibilityReversible:
		return "A single opposite action restores the prior state. Nothing is destroyed."
	case remediate.ReversibilityRecreatedByController:
		return "The object itself is destroyed permanently — its name, its identity, its logs are gone — but a controller rebuilds a replacement automatically, so the workload's function returns without anyone acting."
	case remediate.ReversibilityIrreversible:
		return "The effect cannot be undone and nothing will repair it on its own."
	default:
		return "Unclassified. Treat as irreversible."
	}
}

// rollbackPlan returns the concrete plan for undoing an operation, and ok=false for
// an operation that has none.
//
// The ok=false case is a real safety mechanism rather than defensive padding. The
// operation catalog is deliberately small today, but it is designed to grow, and
// the failure mode when it grows is that someone adds an operation and nobody
// writes down how to undo it — at which point an approver would be shown a
// reversibility class with no accompanying plan and would reasonably assume one
// exists. Refusing to authorize such an operation ([ReasonNoRollbackPlan]) makes
// the omission fail loudly at the gate instead of quietly widening what a human can
// approve without understanding.
func rollbackPlan(op remediate.Operation) (string, bool) {
	switch op {
	case remediate.OpRolloutRestart:
		return "No rollback is required: the Deployment's spec is unchanged apart from the restart annotation, and its own rolling-update strategy replaces pods gradually rather than taking the workload down. If the fresh pods are unhealthy for a reason the restart did not fix, roll the Deployment back to its previous revision.", true
	case remediate.OpRollbackRevision:
		return "Roll forward again to the revision that was current before this action (a second `rollout undo`), which restores the prior state exactly. Both revisions remain in the Deployment's history.", true
	case remediate.OpDeletePod:
		return "The pod itself cannot be restored — its name, its identity, and its logs are gone permanently. Its controller recreates a replacement automatically, so no rollback action is needed to restore function, but anything you still need from that pod must be captured BEFORE approving.", true
	case remediate.OpCordonNode:
		return "Uncordon the node to make it schedulable again. Pods already running on it were never touched, so uncordoning restores the prior state exactly.", true
	default:
		return "", false
	}
}

// LabelsFor returns the labels an artifact should carry. Every artifact gets
// [ManagedLabel]; a still-undecided one also gets [NeedsHumanLabel], which is what
// puts it in front of an operator.
//
// The decision labels are NOT returned here, and that is deliberate: they are
// applied by humans and by [Gatekeeper.RecordExecution], never rewritten by a body
// refresh. A refresh that re-sent the full label set would silently re-apply — or
// erase — a decision, which is the one thing this package must never do to a human's
// input. The gatekeeper preserves them explicitly instead.
func LabelsFor(pending PendingAction) []string {
	labels := []string{ManagedLabel}
	if pending.State == StatePending && !pending.Executed {
		labels = append(labels, NeedsHumanLabel)
	}
	if pending.State == StateApproved {
		labels = append(labels, ApprovedLabel)
	}
	if pending.State == StateRejected {
		labels = append(labels, RejectedLabel)
	}
	if pending.Executed {
		labels = append(labels, ExecutedLabel)
	}
	return labels
}

// RefusalComment renders the note posted when an approval is present but cannot be
// honored. It names the reason in the operator's terms, states what MaKlaude did
// about it (withdrew the approval, refreshed the evidence), and says what happens
// next — so a human who comes back to a de-approved issue is not left guessing
// whether the system malfunctioned or protected them.
func RefusalComment(req Request, pending PendingAction, reason Reason, policy Policy) string {
	policy = policy.normalized()
	var detail string
	switch reason {
	case ReasonDrift:
		detail = fmt.Sprintf(
			"The target object changed after this action was previewed: it was approved against resourceVersion `%s`, and `%s` is now at `%s`. The approved action and the action now possible are not the same action.",
			pending.PreviewedResourceVersion, req.Proposal.Target.String(), req.Proposal.Target.ResourceVersion)
	case ReasonApprovalPredatesPreview:
		detail = "The approval was recorded before this issue last refreshed its preview, so it is consent to a cluster state that has since been replaced."
	case ReasonApprovalExpired:
		detail = fmt.Sprintf(
			"The approval was recorded at %s, which is older than the %s approval lifetime. Consent to change a live cluster is deliberately perishable.",
			pending.DecidedAt.UTC().Format(time.RFC3339), policy.ApprovalTTL)
	case ReasonUnattributedApproval:
		detail = "The approval label carries no identifiable approver. MaKlaude will not act on an approval it cannot attribute to a person."
	case ReasonSelfApproval:
		detail = "The approval label was applied by MaKlaude's own account, not by a person. A system that can approve its own proposals is not gated, so this approval is void regardless of anything else about it."
	case ReasonPreviewFailed:
		detail = "The API server rejected this action as a dry-run, so executing it would fail. The preview error is in the issue body."
	case ReasonNoRollbackPlan:
		detail = fmt.Sprintf("Operation `%s` has no defined rollback plan, so the issue could not honestly describe the cost of being wrong.", req.Proposal.Operation)
	default:
		detail = "The approval could not be honored."
	}
	next := fmt.Sprintf("The issue has been refreshed with current evidence. Re-add the `%s` label if you still want this action against the state shown now.", ApprovedLabel)
	if policy.AutoApprove {
		// Saying "re-add the label" and stopping would imply MaKlaude is now waiting,
		// which under the bypass it is not — it will re-decide on its own next pass. A
		// reader who acts on the wrong one of those two beliefs loses the only window
		// they had to intervene.
		next = fmt.Sprintf(
			"The issue has been refreshed with current evidence. **Autonomous mode is enabled** (`%s`), so MaKlaude will re-decide this itself on its next pass "+
				"against the refreshed evidence — it is not waiting for you. Add the `%s` label to stop it.",
			AutoApproveEnv, RejectedLabel)
	}
	return fmt.Sprintf(
		"**Approval withdrawn — the action was NOT run.** (`%s`)\n\n%s\n\n%s",
		reason, detail, next)
}

// AuthorizationComment renders the note the gate posts on an artifact at the moment it
// honors a decision, before the permission slip leaves the package.
//
// The two authorities get genuinely different text rather than one sentence with a
// substituted name. A human-approved action's note is a receipt — here is who allowed
// it and when — and a policy-waived one's is a warning, because it is the record that
// an action ran against a live cluster with nobody having looked at it. Rendering the
// second as the first with `@policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` in the name slot
// would be technically accurate and would read, to anyone skimming, exactly like an
// approval.
func AuthorizationComment(auth *Authorization) string {
	if !auth.Valid() {
		return "An authorization was recorded that the gate did not issue. This should never happen; treat it as a bug."
	}
	tail := "\n\nThe action has **not** run yet; its outcome will be recorded here."

	if !auth.Authority().HumanReviewed() {
		return fmt.Sprintf(
			"**Auto-approved — NO HUMAN REVIEWED THIS.** MaKlaude is authorized to run `%s` on `%s` (cluster `%s`) at resourceVersion `%s` because "+
				"`%s` is enabled on the process that made this decision. Nobody approved it: the requirement for approval was waived by configuration, "+
				"and the audit trail records it as `%s` rather than naming a person.%s",
			auth.Operation(), auth.Target().String(), auth.Cluster(), auth.Target().ResourceVersion,
			AutoApproveEnv, AutoApprovePolicy, tail)
	}
	return fmt.Sprintf(
		"**Approval honored.** MaKlaude is authorized to run `%s` on `%s` (cluster `%s`) at resourceVersion `%s`, "+
			"on the approval recorded by @%s at %s.%s",
		auth.Operation(), auth.Target().String(), auth.Cluster(), auth.Target().ResourceVersion,
		auth.Approver(), auth.ApprovedAt().UTC().Format(time.RFC3339), tail)
}

// WithdrawalComment renders the note left when an artifact is closed. Closing with
// a note rather than silently is what keeps the trail self-explanatory: the most
// important thing a future reader can learn from a withdrawn approval request is
// that it was withdrawn WITHOUT running, and the wording says so explicitly for
// every reason that did not execute.
func WithdrawalComment(id remediate.ProposalIdentity, reason Reason) string {
	switch reason {
	case ReasonCompleted:
		return fmt.Sprintf(
			"MaKlaude no longer proposes this action (`%s`) — it ran, and the condition that justified it has cleared. Closing; the execution record above is the audit trail.",
			id)
	case ReasonPendingExpired:
		return fmt.Sprintf(
			"No decision was recorded on this approval request within its lifetime, so it has been withdrawn **without running anything** (`%s`). If MaKlaude still proposes this action, it will open a fresh request with current evidence.",
			id)
	default:
		return fmt.Sprintf(
			"MaKlaude no longer proposes this action (`%s`) — the problem it addressed is no longer observed. Closing **without running anything**: a pending approval is not a queued job, so the authority to act expires with the reason to act.",
			id)
	}
}

// ExecutionComment renders the note recording that an authorized action ran. It
// restates on whose authority it ran and against which resourceVersion, so the audit
// trail answers "who allowed this, and what exactly did it act on" from the artifact
// alone.
//
// It is the LAST place in this package where an authority could be misrendered, and
// the highest-stakes one: everything before it describes something that might happen,
// and this describes a cluster that has already changed. So it asks the authority for
// the same reason [AuthorizationComment] does — a note reading "approved by
// @policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE" would be the trail's permanent record of
// an unreviewed mutation, phrased as a review.
func ExecutionComment(auth *Authorization, detail string) string {
	if !auth.Valid() {
		return "An execution was recorded against an invalid authorization. This should never happen; treat it as a bug."
	}
	var body string
	if auth.Authority().HumanReviewed() {
		body = fmt.Sprintf(
			"**Executed.** `%s` on `%s` (cluster `%s`, resourceVersion `%s`), approved by @%s at %s.",
			auth.Operation(), auth.Target().String(), auth.Cluster(), auth.Target().ResourceVersion,
			auth.Approver(), auth.ApprovedAt().UTC().Format(time.RFC3339))
	} else {
		body = fmt.Sprintf(
			"**Executed — NO HUMAN REVIEWED THIS.** `%s` on `%s` (cluster `%s`, resourceVersion `%s`), run under `%s` with the approval requirement waived by policy (`%s`).",
			auth.Operation(), auth.Target().String(), auth.Cluster(), auth.Target().ResourceVersion,
			AutoApproveEnv, AutoApprovePolicy)
	}
	if d := strings.TrimSpace(detail); d != "" {
		body += "\n\n" + d
	}
	return body + fmt.Sprintf("\n\nThis issue is labelled `%s` and will not be authorized again.", ExecutedLabel)
}
