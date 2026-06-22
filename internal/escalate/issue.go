package escalate

import (
	"fmt"
	"strings"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
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

// identityMarker renders the hidden marker line that embeds id in an issue body.
func identityMarker(id detect.Identity) string {
	return identityMarkerPrefix + string(id) + identityMarkerSuffix
}

// threadMarker renders the hidden marker line that embeds a Slack thread_ts in an
// issue body.
func threadMarker(threadTS string) string {
	return threadMarkerPrefix + threadTS + threadMarkerSuffix
}

// ParseIdentityMarker extracts the embedded [detect.Identity] from an issue
// body, returning ok=false if no well-formed marker is present. The escalator
// uses it to recognize its own issues when listing them from the sink. It is
// tolerant of surrounding content and only reads the first marker, so extra body
// text (recurrence comments folded in, human edits) cannot break recognition.
func ParseIdentityMarker(body string) (detect.Identity, bool) {
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
	return detect.Identity(id), true
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

// Title renders a short, human-readable issue title for a finding. It leads with
// the severity and names the cluster explicitly so that, in a multi-cluster
// setup, an operator can tell at a glance which cluster a problem belongs to
// without opening the issue.
func Title(f detect.Finding) string {
	return fmt.Sprintf("[%s][%s] %s", strings.ToUpper(f.Severity.String()), f.Cluster, f.Title)
}

// Body renders the full issue body for a finding: a human-readable summary of
// what was seen (cluster, object, severity, message, time) followed by the
// hidden identity marker the escalator relies on to rediscover the issue. The
// body is regenerated from scratch on every update so it always reflects the
// latest state of an evolving problem.
func Body(f detect.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "MaKlaude detected a problem on cluster **%s**.\n\n", f.Cluster)
	fmt.Fprintf(&b, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Cluster | `%s` |\n", f.Cluster)
	fmt.Fprintf(&b, "| Object | `%s` |\n", f.Object.String())
	fmt.Fprintf(&b, "| Severity | **%s** |\n", f.Severity.String())
	fmt.Fprintf(&b, "| First/last detected | %s |\n", f.DetectedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "\n**%s**\n\n%s\n", f.Title, f.Message)
	if wantsHuman(f) {
		fmt.Fprintf(&b, "\n> This problem warrants a human decision (labelled `%s`). MaKlaude takes no mutating action without approval.\n", NeedsHumanLabel)
	}
	fmt.Fprintf(&b, "\n---\n*Tracked automatically by MaKlaude. This issue updates on recurrence and closes when the problem clears.*\n")
	// The marker MUST be present and parseable; it is the durable identity key.
	fmt.Fprintf(&b, "\n%s\n", identityMarker(f.Identity))
	return b.String()
}

// RecurrenceComment renders the comment added when an existing issue is updated
// because its problem was seen again on a later cycle. It records the latest
// observation so the issue's timeline shows the problem persisting, which is
// exactly what makes recurrence auditable instead of silent.
func RecurrenceComment(f detect.Finding) string {
	return fmt.Sprintf(
		"Still observed at %s — severity **%s**.\n\n%s",
		f.DetectedAt.UTC().Format("2006-01-02 15:04:05 MST"),
		f.Severity.String(),
		f.Message,
	)
}

// ClosingComment renders the comment left when a tracked issue is closed because
// its problem is no longer present in the current findings. Closing with a note
// (rather than silently) keeps the trail self-explanatory for a future reader.
func ClosingComment(id detect.Identity) string {
	return fmt.Sprintf(
		"MaKlaude no longer observes this problem (identity `%s`) — it appears to have cleared. Closing automatically; reopen if it recurs.",
		string(id),
	)
}

// LabelsFor returns the labels an issue for f should carry. Every managed issue
// gets [ManagedLabel]; issues warranting a decision also get [NeedsHumanLabel].
func LabelsFor(f detect.Finding) []string {
	if wantsHuman(f) {
		return []string{ManagedLabel, NeedsHumanLabel}
	}
	return []string{ManagedLabel}
}

// wantsHuman reports whether a finding should be flagged for human attention.
// Warnings and criticals warrant a decision; info-level findings are recorded
// for the audit trail but do not, on their own, demand action — so they do not
// get the needs:human gate.
func wantsHuman(f detect.Finding) bool {
	return f.Severity >= detect.SeverityWarning
}
