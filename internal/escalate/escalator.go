package escalate

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// Escalator is the side-effecting shell around the pure [Reconcile] core. Given
// the current findings, it lists the existing managed issues from its
// [IssueSink], asks Reconcile for the plan, and executes that plan —
// opening, updating, and closing issues — so the external comms trail matches
// reality.
//
// It holds no in-memory issue state of its own between calls: every
// [Escalator.Reconcile] re-lists from the sink, so it is correct across process
// restarts and never relies on remembered mappings. The durable identity marker
// embedded in each issue is what makes that re-listing authoritative.
//
// # Chat continuity (T3)
//
// Alongside the GitHub trail the escalator drives a [notify.Notifier] so the same
// lifecycle is mirrored into chat (Slack) as ONE threaded conversation: opening a
// problem posts a thread root, recurrence replies an update, and clearance replies
// a resolution. Continuity is durable across process restarts because the chat
// thread handle (Slack's "thread_ts") is persisted in the backing issue body (a
// hidden thread marker) when the root is posted, and recovered — via the
// [TrackedIssue.ThreadTS] parsed on every ListOpen — on the next reconcile. The
// escalator passes that recovered handle back to the notifier so the reply lands
// in the original thread, with no duplicate threads even after a restart.
//
// The chat side is strictly best-effort and comms-only: a notifier error is
// recorded but never fails the reconcile or strands the GitHub trail, and the
// notifier has no path to a cluster. When no notifier is configured the escalator
// uses a [notify.NopNotifier], so behavior is byte-for-byte the GitHub + email
// trail of Milestone 1.
type Escalator struct {
	sink     IssueSink
	notifier notify.Notifier
}

// NewEscalator builds an escalator over the given sink with a no-op notifier. A
// nil sink panics: a caller that wants a no-op escalator should pass a [MemorySink]
// (or use a real sink's graceful-degradation path), making the no-op explicit
// rather than hiding it behind a nil. To also mirror the lifecycle into chat, use
// [NewEscalatorWithNotifier].
func NewEscalator(sink IssueSink) *Escalator {
	return NewEscalatorWithNotifier(sink, nil)
}

// NewEscalatorWithNotifier builds an escalator that, in addition to the GitHub
// trail, mirrors the escalation lifecycle into chat via notifier. A nil sink
// panics as in [NewEscalator]; a nil notifier is replaced with a
// [notify.NopNotifier], so callers never have to nil-check and an unconfigured
// chat backend degrades to exactly the Milestone 1 behavior.
func NewEscalatorWithNotifier(sink IssueSink, notifier notify.Notifier) *Escalator {
	if sink == nil {
		panic("escalate: NewEscalator requires a non-nil sink (use NewMemorySink for a no-op)")
	}
	if notifier == nil {
		notifier = notify.NopNotifier{}
	}
	return &Escalator{sink: sink, notifier: notifier}
}

// Reconcile brings the comms trail in line with the current incidents for one
// reconciliation pass and reports what it did.
//
// subjects should be the full current set of incidents (each with its ranked
// diagnosis) the caller wants reflected in the trail — typically one cluster's
// [correlate.Correlate] output, each diagnosed via [diagnose.Diagnose]. The
// function is cluster-agnostic: incident identities already encode their cluster,
// so a caller may pass several clusters' subjects at once and isolation still
// holds. The escalator only ever closes issues whose identity is absent from
// subjects, so passing a single cluster's subjects will NOT close another
// cluster's issues ONLY IF the caller scopes the sink per cluster or passes all
// clusters' subjects together; see [Escalator] callers for the recommended
// pattern.
//
// The pass is best-effort and continues past per-issue errors so one transient
// failure does not strand the rest of the trail; all errors are aggregated and
// returned together, with the [Outcome] still reporting the actions that
// succeeded.
func (e *Escalator) Reconcile(ctx context.Context, subjects []Subject) (Outcome, error) {
	tracked, err := e.sink.ListOpen(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("escalate: listing open issues: %w", err)
	}

	plan := Reconcile(subjects, tracked)

	var out Outcome
	var errs []error

	for _, a := range plan {
		// The notifier is keyed on detect.Identity (the M2 chat seam); an incident
		// identity is an equally-opaque, equally-stable key, so it is threaded through
		// unchanged as the notifier's dedup handle. This keeps the chat lifecycle
		// (root/update/resolution) mapped to exactly one thread per incident without
		// widening the notify interface.
		notifyID := detect.Identity(a.Identity)

		switch a.Kind {
		case ActionOpen:
			ref, err := e.sink.Create(ctx, Title(a.Subject), Body(a.Subject), LabelsFor(a.Subject))
			if err != nil {
				errs = append(errs, fmt.Errorf("opening issue for %q: %w", a.Identity, err))
				continue
			}
			out.Opened++

			// Mirror to chat as the thread ROOT, then persist the returned thread
			// handle into the issue body so a future reconcile (even after a restart)
			// can reply into this same thread. Both are best-effort: a chat or
			// persistence hiccup is recorded but never strands the GitHub trail. The
			// root carries the incident summary AND its top-ranked root cause +
			// confidence (see EscalationSummary), so the thread opens with the diagnosis,
			// not just the symptom.
			threadTS, nerr := e.notifier.NotifyEscalation(ctx, notifyID, EscalationSummary(a.Subject), string(ref), wantsHuman(a.Subject))
			if nerr != nil {
				errs = append(errs, fmt.Errorf("notifying escalation for %q: %w", a.Identity, nerr))
				continue
			}
			if threadTS != "" {
				body := withThreadMarker(Body(a.Subject), threadTS)
				if uerr := e.sink.Update(ctx, ref, Title(a.Subject), body, LabelsFor(a.Subject)); uerr != nil {
					errs = append(errs, fmt.Errorf("persisting thread marker on %q for %q: %w", ref, a.Identity, uerr))
				}
			}

		case ActionUpdate:
			// Refresh the body/labels to the latest diagnosis, preserving the durable
			// thread marker so continuity is not lost, then record the recurrence in
			// a comment so the timeline shows it persisting.
			body := withThreadMarker(Body(a.Subject), a.ThreadTS)
			if err := e.sink.Update(ctx, a.Ref, Title(a.Subject), body, LabelsFor(a.Subject)); err != nil {
				errs = append(errs, fmt.Errorf("updating issue %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			if err := e.sink.Comment(ctx, a.Ref, RecurrenceComment(a.Subject)); err != nil {
				errs = append(errs, fmt.Errorf("commenting recurrence on %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			out.Updated++

			// Mirror the recurrence into the original chat thread (recovered ts).
			if nerr := e.notifier.NotifyUpdate(ctx, notifyID, a.ThreadTS, RecurrenceComment(a.Subject)); nerr != nil {
				errs = append(errs, fmt.Errorf("notifying update for %q: %w", a.Identity, nerr))
			}

		case ActionClose:
			// Leave a closing note before closing so the record explains itself.
			if err := e.sink.Comment(ctx, a.Ref, ClosingComment(a.Identity)); err != nil {
				errs = append(errs, fmt.Errorf("commenting closure on %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			if err := e.sink.Close(ctx, a.Ref); err != nil {
				errs = append(errs, fmt.Errorf("closing issue %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			out.Closed++

			// Mirror the resolution into the original chat thread (recovered ts).
			if nerr := e.notifier.NotifyResolution(ctx, notifyID, a.ThreadTS, ClosingComment(a.Identity)); nerr != nil {
				errs = append(errs, fmt.Errorf("notifying resolution for %q: %w", a.Identity, nerr))
			}

		default:
			errs = append(errs, fmt.Errorf("unknown action kind %d for %q", a.Kind, a.Identity))
		}
	}

	return out, errors.Join(errs...)
}

// Outcome summarizes one reconciliation pass: how many issues were opened,
// updated, and closed. It is returned for logging and for the e2e harness to
// assert against; the numbers are independent of any error returned alongside
// (they count only the actions that actually succeeded).
type Outcome struct {
	Opened  int
	Updated int
	Closed  int
}

// String renders a compact, log-friendly summary.
func (o Outcome) String() string {
	return fmt.Sprintf("escalation: opened=%d updated=%d closed=%d", o.Opened, o.Updated, o.Closed)
}
