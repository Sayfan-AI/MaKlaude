package escalate

import (
	"fmt"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

// ManagedLabel is applied to every issue MaKlaude opens. It is a coarse filter
// the escalator (and a human) can use to find MaKlaude-managed issues at a
// glance; the authoritative per-problem key is still the embedded identity
// marker, since a label cannot encode an arbitrary identity safely.
const ManagedLabel = "maklaude"

// NeedsHumanLabel marks an issue MaKlaude wants a human to act on. It is applied
// when a finding's severity warrants a decision (see [wantsHuman]); GitHub's own
// label-based filtering and notifications then surface it to operators. This is
// the "human-in-the-loop" gate expressed in the comms layer.
const NeedsHumanLabel = "needs:human"

// identityMarkerPrefix and identityMarkerSuffix bracket the hidden HTML comment
// that embeds a finding's [detect.Identity] in an issue body. Rendered GitHub
// markdown hides HTML comments, so the marker is invisible to humans but lets
// the escalator rediscover, across process restarts, exactly which open issue
// represents which problem. The marker is the durable source of truth for that
// mapping — the in-memory cache is only an optimization on top of it.
const (
	identityMarkerPrefix = "<!-- maklaude:identity="
	identityMarkerSuffix = " -->"
)

// threadMarkerPrefix and threadMarkerSuffix bracket a second hidden HTML comment
// that embeds the Slack thread timestamp ("thread_ts") of the chat thread opened
// for a problem. It is the T3 counterpart to the identity marker: where identity
// makes the issue rediscoverable as "which problem", the thread marker makes the
// chat thread rediscoverable as "which conversation", so that — even after the
// monitor process restarts and any in-memory map is gone — a recurrence or a
// clearance replies into the ORIGINAL Slack thread rather than spawning a
// duplicate top-level message. The backing GitHub issue is therefore the durable
// store for both keys; no new datastore is introduced.
//
// Like the identity marker it lives in a rendered-invisible HTML comment, so the
// issue still reads cleanly to a human. It is intentionally a separate marker (not
// folded into the identity one) so the two can be written and parsed independently:
// the thread_ts is unknown when the issue is first created and is patched in only
// after the Slack root has been posted.
const (
	threadMarkerPrefix = "<!-- maklaude:thread="
	threadMarkerSuffix = " -->"
)

// identityMarker renders the hidden marker line that embeds an incident identity
// in an issue body. Since T4 the trail is keyed on the correlated incident, so
// the marker carries the [correlate.IncidentIdentity] — the durable key by which
// the escalator rediscovers which open issue represents which incident.
func identityMarker(id correlate.IncidentIdentity) string {
	return identityMarkerPrefix + string(id) + identityMarkerSuffix
}

// threadMarker renders the hidden marker line that embeds a Slack thread_ts in an
// issue body.
func threadMarker(threadTS string) string {
	return threadMarkerPrefix + threadTS + threadMarkerSuffix
}

// ParseIdentityMarker extracts the embedded [correlate.IncidentIdentity] from an
// issue body, returning ok=false if no well-formed marker is present. The
// escalator uses it to recognize its own issues when listing them from the sink.
// It is tolerant of surrounding content and only reads the first marker, so extra
// body text (recurrence comments folded in, human edits) cannot break
// recognition.
func ParseIdentityMarker(body string) (correlate.IncidentIdentity, bool) {
	start := strings.Index(body, identityMarkerPrefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(identityMarkerPrefix):]
	end := strings.Index(rest, identityMarkerSuffix)
	if end < 0 {
		return "", false
	}
	id := strings.TrimSpace(rest[:end])
	if id == "" {
		return "", false
	}
	return correlate.IncidentIdentity(id), true
}

// ParseThreadMarker extracts the embedded Slack thread_ts from an issue body,
// returning ok=false if no well-formed thread marker is present (the normal case
// for issues opened before any Slack root was posted, or when Slack is
// unconfigured). The escalator uses it to recover, across process restarts, which
// chat thread an open issue belongs to, so updates and the resolution reply into
// the original thread instead of fragmenting. Like [ParseIdentityMarker] it reads
// only the first marker and tolerates surrounding content, so recurrence comments
// folded into the body or human edits cannot break recovery.
func ParseThreadMarker(body string) (string, bool) {
	start := strings.Index(body, threadMarkerPrefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(threadMarkerPrefix):]
	end := strings.Index(rest, threadMarkerSuffix)
	if end < 0 {
		return "", false
	}
	ts := strings.TrimSpace(rest[:end])
	if ts == "" {
		return "", false
	}
	return ts, true
}

// withThreadMarker returns body with a Slack thread marker embedded for threadTS,
// replacing any thread marker already present so a body regenerated on recurrence
// never accumulates stale markers or loses the durable thread_ts. An empty
// threadTS leaves the body unchanged (and strips any existing marker), which is
// the correct behavior when Slack is unconfigured or no root has been posted yet.
//
// It is the write-side counterpart to [ParseThreadMarker]: the escalator calls it
// to patch the freshly-rendered [Body] before handing it to the sink, so the
// identity marker (written by Body) and the thread marker (written here) coexist
// in one body without either clobbering the other.
func withThreadMarker(body, threadTS string) string {
	body = stripThreadMarker(body)
	if strings.TrimSpace(threadTS) == "" {
		return body
	}
	// Append on its own line, mirroring how Body appends the identity marker.
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + threadMarker(threadTS) + "\n"
}

// stripThreadMarker removes the first thread marker (and its trailing newline, if
// any) from body, leaving everything else intact. It is used by [withThreadMarker]
// so re-rendering a body never duplicates the marker.
func stripThreadMarker(body string) string {
	start := strings.Index(body, threadMarkerPrefix)
	if start < 0 {
		return body
	}
	rest := body[start+len(threadMarkerPrefix):]
	end := strings.Index(rest, threadMarkerSuffix)
	if end < 0 {
		return body
	}
	markerEnd := start + len(threadMarkerPrefix) + end + len(threadMarkerSuffix)
	// Also consume a single trailing newline so removal is clean.
	if markerEnd < len(body) && body[markerEnd] == '\n' {
		markerEnd++
	}
	return body[:start] + body[markerEnd:]
}

// Title renders a short, human-readable issue title for an incident. It leads
// with the incident severity and names the cluster explicitly so that, in a
// multi-cluster setup, an operator can tell at a glance which cluster a problem
// belongs to without opening the issue, and it uses the primary finding's title
// as the human label for the problem.
func Title(s Subject) string {
	p := s.Incident.Primary
	return fmt.Sprintf("[%s][%s] %s", strings.ToUpper(s.Severity().String()), s.Cluster(), p.Title)
}

// Body renders the full diagnostic issue body for an incident: the incident
// summary (cluster, severity, primary object, time), the ranked root-cause
// hypotheses — each with its confidence, explanation, and the specific evidence
// findings grouped under it — the affected objects, and DIAGNOSTIC-ONLY suggested
// next steps, followed by the hidden identity marker the escalator relies on to
// rediscover the issue. The body is regenerated from scratch on every update so
// it always reflects the latest diagnosis of an evolving incident.
//
// # The read-only boundary is expressed here, not just enforced elsewhere
//
// Milestone 3 is strictly read-only: MaKlaude diagnoses, it does not remediate.
// The body therefore NEVER suggests an action MaKlaude will take or auto-execute
// — every "next step" is a manual, read-only investigation for a human to run
// (kubectl describe/logs/get/top, inspecting an image, checking quotas), phrased
// as guidance for the operator. See [nextSteps]. When the top hypothesis is
// uncertain (low confidence), the body honestly surfaces the competing
// hypotheses rather than overcommitting to one cause.
func Body(s Subject) string {
	var b strings.Builder
	p := s.Incident.Primary

	fmt.Fprintf(&b, "MaKlaude diagnosed an incident on cluster **%s**.\n\n", s.Cluster())
	fmt.Fprintf(&b, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Cluster | `%s` |\n", s.Cluster())
	fmt.Fprintf(&b, "| Primary object | `%s` |\n", p.Object.String())
	fmt.Fprintf(&b, "| Severity | **%s** |\n", s.Severity().String())
	fmt.Fprintf(&b, "| Affected objects | %d |\n", len(s.Incident.Findings()))
	fmt.Fprintf(&b, "| First/last detected | %s |\n", s.Incident.DetectedAt.UTC().Format("2006-01-02 15:04:05 MST"))

	fmt.Fprintf(&b, "\n**%s**\n\n%s\n", p.Title, p.Message)

	writeHypotheses(&b, s)
	writeAffectedObjects(&b, s)
	writeNextSteps(&b, s)

	if wantsHuman(s) {
		fmt.Fprintf(&b, "\n> This incident warrants a human decision (labelled `%s`). MaKlaude only observes and diagnoses; it takes no mutating or remediating action, and never will without explicit human approval.\n", NeedsHumanLabel)
	}
	fmt.Fprintf(&b, "\n---\n*Diagnosed automatically by MaKlaude (read-only). This issue updates on recurrence and closes when the incident clears. The steps above are for a human to investigate — MaKlaude does not run them.*\n")
	// The marker MUST be present and parseable; it is the durable incident key.
	fmt.Fprintf(&b, "\n%s\n", identityMarker(s.Identity()))
	return b.String()
}

// writeHypotheses renders the ranked root-cause hypotheses section: each
// hypothesis with its confidence, explanation, and the exact evidence findings
// that support it. The leading (most-confident) hypothesis is marked as such; when
// it is only low-confidence, a note invites the reader to weigh the competing
// hypotheses that follow rather than trusting the top one — honest uncertainty
// instead of overcommitment.
func writeHypotheses(b *strings.Builder, s Subject) {
	b.WriteString("\n## Ranked root-cause hypotheses\n")
	if len(s.Hypotheses) == 0 {
		// A real incident always yields at least one hypothesis; guard anyway so the
		// body is well-formed for the degenerate case.
		b.WriteString("\n_No hypotheses were produced for this incident._\n")
		return
	}

	top := s.Hypotheses[0]
	if top.Confidence <= diagnose.ConfidenceLow {
		fmt.Fprintf(b, "\n> The leading hypothesis is **low-confidence**: no specialized rule matched decisively. Weigh the alternatives below rather than treating the first as settled.\n")
	}

	for i, h := range s.Hypotheses {
		lead := ""
		if i == 0 {
			lead = " _(leading hypothesis)_"
		}
		fmt.Fprintf(b, "\n### %d. %s — confidence: %s%s\n\n", i+1, h.Title, h.Confidence.String(), lead)
		fmt.Fprintf(b, "%s\n", h.Message)
		writeEvidence(b, h)
	}
}

// writeEvidence lists, under one hypothesis, the specific findings that support
// it — the observations that make the diagnosis auditable. Each is rendered with
// its severity and object so a reader can trace the hypothesis back to the exact
// facts MaKlaude saw.
func writeEvidence(b *strings.Builder, h diagnose.Hypothesis) {
	if len(h.Evidence) == 0 {
		return
	}
	b.WriteString("\nEvidence:\n")
	for i := range h.Evidence {
		e := h.Evidence[i]
		fmt.Fprintf(b, "- `%s` [%s] %s — %s\n", e.Object.String(), e.Severity.String(), e.Title, e.Message)
	}
}

// writeAffectedObjects lists every object the incident touches (its primary plus
// the correlated effects), most-structural first, so an operator sees the full
// blast radius of the one incident rather than a scatter of separate findings.
func writeAffectedObjects(b *strings.Builder, s Subject) {
	findings := s.Incident.Findings()
	fmt.Fprintf(b, "\n## Affected objects (%d)\n\n", len(findings))
	for i := range findings {
		f := findings[i]
		role := "effect"
		if i == 0 {
			role = "primary"
		}
		fmt.Fprintf(b, "- `%s` [%s] — %s _(%s)_\n", f.Object.String(), f.Severity.String(), f.Title, role)
	}
}

// writeNextSteps renders the DIAGNOSTIC-ONLY suggested next steps for the
// incident. Every step is a manual, read-only investigation a human runs; none is
// an action MaKlaude will take. The steps are derived from the causes present in
// the ranked hypotheses (deduped, in ranked order) so the guidance is specific to
// what MaKlaude suspects, and a generic fallback always applies.
func writeNextSteps(b *strings.Builder, s Subject) {
	b.WriteString("\n## Suggested next steps (manual, read-only)\n\n")
	b.WriteString("_These are investigative steps for a human operator. MaKlaude will not run, apply, or execute any of them; it neither remediates nor mutates the cluster._\n\n")

	seen := make(map[diagnose.Cause]bool, len(s.Hypotheses))
	wrote := false
	for _, h := range s.Hypotheses {
		if seen[h.Cause] {
			continue
		}
		seen[h.Cause] = true
		for _, step := range nextSteps(h.Cause) {
			fmt.Fprintf(b, "- %s\n", step)
			wrote = true
		}
	}
	if !wrote {
		for _, step := range nextSteps(diagnose.CauseUnknown) {
			fmt.Fprintf(b, "- %s\n", step)
		}
	}
}

// nextSteps returns the manual, read-only investigation steps appropriate to a
// diagnosed cause. Every returned step is diagnostic: it inspects, describes, or
// reads state (kubectl describe/logs/get/top, checking an image or a quota) and
// NEVER mutates the cluster. This is the concrete expression of the M3 read-only
// boundary in the comms trail — the suggestions can only ever tell a human what to
// look at, never announce something MaKlaude will do.
func nextSteps(cause diagnose.Cause) []string {
	switch cause {
	case diagnose.CauseBadImage:
		return []string{
			"Confirm the image reference is correct and the tag exists: `kubectl describe pod <pod> -n <namespace>` and inspect the container `image:` field.",
			"Check registry reachability and that the pull secret is valid (`kubectl get secret`, `kubectl describe serviceaccount`); no changes — just verify.",
		}
	case diagnose.CauseInsufficientResources:
		return []string{
			"Read the pod's scheduling events: `kubectl describe pod <pod> -n <namespace>` (look for the `FailedScheduling` event).",
			"Compare the pod's requests against node capacity: `kubectl get nodes -o wide`, `kubectl describe node <node>`, and review any `ResourceQuota`/`LimitRange` in the namespace.",
		}
	case diagnose.CauseNodeFailure:
		return []string{
			"Inspect the node's conditions: `kubectl describe node <node>` and `kubectl get node <node> -o yaml`.",
			"Check the kubelet and node connectivity (kubelet logs, `kubectl get events`); investigate only — do not cordon or drain until a human decides.",
		}
	case diagnose.CauseOOMKill:
		return []string{
			"Confirm the OOM kill and review restarts: `kubectl describe pod <pod> -n <namespace>` and `kubectl logs --previous <pod> -c <container> -n <namespace>`.",
			"Compare the container's memory limit against observed usage: `kubectl top pod <pod> -n <namespace>` and inspect the container's `resources.limits.memory`.",
		}
	default: // CauseUnknown and any future cause without specific guidance.
		return []string{
			"Gather more detail on the primary object: `kubectl describe <kind>/<name> -n <namespace>`.",
			"Read recent logs and events: `kubectl logs <pod> -n <namespace>` and `kubectl get events -n <namespace> --sort-by=.lastTimestamp`.",
		}
	}
}

// RecurrenceComment renders the comment added when an existing issue is updated
// because its incident was seen again on a later cycle. It records the latest
// observation — the leading hypothesis and its confidence — so the issue's
// timeline shows the incident persisting, which is exactly what makes recurrence
// auditable instead of silent.
func RecurrenceComment(s Subject) string {
	lead := "no hypothesis"
	if top, ok := s.TopHypothesis(); ok {
		lead = fmt.Sprintf("%s (confidence %s)", top.Title, top.Confidence.String())
	}
	return fmt.Sprintf(
		"Still observed at %s — severity **%s**. Leading hypothesis: %s.\n\n%s",
		s.Incident.DetectedAt.UTC().Format("2006-01-02 15:04:05 MST"),
		s.Severity().String(),
		lead,
		s.Incident.Primary.Message,
	)
}

// ClosingComment renders the comment left when a tracked issue is closed because
// its incident is no longer present in the current diagnosis. Closing with a note
// (rather than silently) keeps the trail self-explanatory for a future reader.
func ClosingComment(id correlate.IncidentIdentity) string {
	return fmt.Sprintf(
		"MaKlaude no longer observes this incident (identity `%s`) — it appears to have cleared. Closing automatically; reopen if it recurs.",
		string(id),
	)
}

// LabelsFor returns the labels an issue for s should carry. Every managed issue
// gets [ManagedLabel]; incidents warranting a decision also get [NeedsHumanLabel].
func LabelsFor(s Subject) []string {
	if wantsHuman(s) {
		return []string{ManagedLabel, NeedsHumanLabel}
	}
	return []string{ManagedLabel}
}

// wantsHuman reports whether an incident should be flagged for human attention.
// Warnings and criticals warrant a decision; info-level incidents are recorded
// for the audit trail but do not, on their own, demand action — so they do not
// get the needs:human gate.
func wantsHuman(s Subject) bool {
	return s.Severity() >= detect.SeverityWarning
}
