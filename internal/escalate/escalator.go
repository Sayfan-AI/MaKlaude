package escalate

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
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
type Escalator struct {
	sink IssueSink
}

// NewEscalator builds an escalator over the given sink. A nil sink panics: a
// caller that wants a no-op escalator should pass a [MemorySink] (or use a
// real sink's graceful-degradation path), making the no-op explicit rather than
// hiding it behind a nil.
func NewEscalator(sink IssueSink) *Escalator {
	if sink == nil {
		panic("escalate: NewEscalator requires a non-nil sink (use NewMemorySink for a no-op)")
	}
	return &Escalator{sink: sink}
}

// Reconcile brings the comms trail in line with findings for one reconciliation
// pass and reports what it did.
//
// findings should be the full current finding set the caller wants reflected in
// the trail (typically one cluster's [detect.Analyze] output, but the function
// is cluster-agnostic — identities already encode their cluster, so a caller may
// pass several clusters' findings at once and isolation still holds). The
// escalator only ever closes issues whose identity is absent from findings, so
// passing a single cluster's findings will NOT close another cluster's issues
// ONLY IF the caller scopes the sink per cluster or passes all clusters'
// findings together; see [Escalator] callers for the recommended pattern.
//
// The pass is best-effort and continues past per-issue errors so one transient
// failure does not strand the rest of the trail; all errors are aggregated and
// returned together, with the [Outcome] still reporting the actions that
// succeeded.
func (e *Escalator) Reconcile(ctx context.Context, findings []detect.Finding) (Outcome, error) {
	tracked, err := e.sink.ListOpen(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("escalate: listing open issues: %w", err)
	}

	plan := Reconcile(findings, tracked)

	var out Outcome
	var errs []error

	for _, a := range plan {
		switch a.Kind {
		case ActionOpen:
			ref, err := e.sink.Create(ctx, Title(a.Finding), Body(a.Finding), LabelsFor(a.Finding))
			if err != nil {
				errs = append(errs, fmt.Errorf("opening issue for %q: %w", a.Identity, err))
				continue
			}
			_ = ref
			out.Opened++

		case ActionUpdate:
			// Refresh the body/labels to the latest state, then record the
			// recurrence in a comment so the timeline shows it persisting.
			if err := e.sink.Update(ctx, a.Ref, Title(a.Finding), Body(a.Finding), LabelsFor(a.Finding)); err != nil {
				errs = append(errs, fmt.Errorf("updating issue %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			if err := e.sink.Comment(ctx, a.Ref, RecurrenceComment(a.Finding)); err != nil {
				errs = append(errs, fmt.Errorf("commenting recurrence on %q for %q: %w", a.Ref, a.Identity, err))
				continue
			}
			out.Updated++

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
