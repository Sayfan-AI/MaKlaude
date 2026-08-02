package disclose

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/execute"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Trail is the disclosure trail: it opens an artifact for an action about to run
// unattended, carries the execution layer's notes onto it while the action runs, and
// records the outcome when it finishes.
//
// It satisfies [execute.Recorder], which is what lets the ordinary execution runner
// drive an unattended action with no change to that package. On the gated path that
// interface is served by [approve.Gatekeeper] and writes to the approval artifact; here
// it writes to the disclosure. Same runner, same records, different trail — and no
// second execution path whose behaviour could diverge from the reviewed one.
//
// The zero value is not usable; construct one with [NewTrail].
type Trail struct {
	sink     Sink
	notifier notify.Notifier
	now      func() time.Time
}

// NewTrail builds a trail over a sink and a chat notifier.
//
// A nil notifier becomes [notify.NopNotifier]: chat is a second copy of a record whose
// durable home is the artifact, and an unconfigured chat must never stop an action being
// disclosed. A nil sink is an error — with nowhere to write, an unattended action would
// run with no record anywhere, which is the state this package exists to make
// impossible.
func NewTrail(sink Sink, notifier notify.Notifier) (*Trail, error) {
	if sink == nil {
		return nil, errors.New("disclose: a trail requires a sink (an unattended action is never taken without a record of it)")
	}
	if notifier == nil {
		notifier = notify.NewNopNotifier()
	}
	return &Trail{sink: sink, notifier: notifier, now: time.Now}, nil
}

// WithClock replaces the trail's clock, for reproducible tests.
func (t *Trail) WithClock(now func() time.Time) *Trail {
	if now != nil {
		t.now = now
	}
	return t
}

// Open creates the disclosure artifact for an action that is ABOUT TO RUN and returns
// its reference, which becomes the authorization's artifact reference.
//
// It refuses an action that does not carry a permitting rule, cited evidence and an
// admitted grant. That is the same set [approve.GrantAutonomous] refuses on, checked
// once more here because the two failures differ: minting a bad slip means the action
// does not run, and opening a bad artifact means a public record asserts a permission
// that was never granted.
//
// The artifact is opened before the action rather than after. See the package doc: an
// action that starts and never reports back has to leave something behind, and the only
// way it can is if the something already exists.
func (t *Trail) Open(ctx context.Context, a Action) (Ref, error) {
	if !a.Valid() {
		return "", fmt.Errorf("disclose: refusing to open a disclosure for an action that was not auto-applicable (%s)", a.Verdict.String())
	}
	if a.At.IsZero() {
		a.At = t.now().UTC()
	}
	ref, err := t.sink.Create(ctx, Title(a), Body(a), LabelsFor(a))
	if err != nil {
		return "", fmt.Errorf("disclose: opening the disclosure for %q: %w", a.Proposal.Identity, err)
	}
	return ref, nil
}

// Abandon closes an artifact opened for an action that never ran — the mint refused, the
// runner could not be built, the context died before the first request.
//
// It is the one path on which MaKlaude closes a disclosure itself. Everywhere else
// closing is a person's acknowledgement, and the difference is whether there is anything
// to acknowledge: an artifact for an action that did not happen is noise, and noise on
// the trail that exists to be noticed is the one cost this package cannot afford.
func (t *Trail) Abandon(ctx context.Context, ref Ref, why string) error {
	if ref == "" {
		return nil
	}
	var errs []error
	if err := t.sink.Comment(ctx, ref, "This action was not taken: "+why+"\n\nNothing was sent to the cluster. Closing.\n"); err != nil {
		errs = append(errs, fmt.Errorf("disclose: noting the abandonment on %q: %w", ref, err))
	}
	if err := t.sink.Close(ctx, ref); err != nil {
		errs = append(errs, fmt.Errorf("disclose: closing %q: %w", ref, err))
	}
	return errors.Join(errs...)
}

// Complete records the finished attempt: it escalates if the blast-radius layer asked
// for one, rewrites the body with the outcome and the machine-readable lifecycle
// marker, posts the outcome as a comment, and announces it in chat.
//
// The escalation happens FIRST so that its own failure is a fact the body can state.
// A demotion or an escalation that failed silently would leave a shape trusted (or a
// person uninformed) after an unattended failure, and the artifact is the only place
// that could have said so.
//
// Every step is attempted even when an earlier one failed, and the errors are joined.
// A chat outage must not cost the durable record, and a body rewrite that failed must
// not cost the comment that notifies somebody.
func (t *Trail) Complete(ctx context.Context, ref Ref, a Action, o Outcome) error {
	if ref == "" {
		return errors.New("disclose: refusing to record an outcome against no artifact")
	}
	if o.At.IsZero() {
		o.At = t.now().UTC()
	}

	var errs []error
	if o.Consequence.Escalate {
		if err := t.escalate(ctx, ref, a, o); err != nil {
			o.EscalationErr = err.Error()
			errs = append(errs, err)
		}
	}

	if err := t.sink.SetBody(ctx, ref, BodyWithOutcome(a, o)); err != nil {
		errs = append(errs, fmt.Errorf("disclose: recording the outcome on %q: %w", ref, err))
	}

	detail := "No audit records were produced for this attempt."
	if len(o.Records) > 0 {
		detail = audit.Lifecycle(o.Records)
	}
	if err := t.sink.Comment(ctx, ref, OutcomeComment(o, detail)); err != nil {
		errs = append(errs, fmt.Errorf("disclose: commenting the outcome on %q: %w", ref, err))
	}

	// Chat is last and its failure is reported but never allowed to look like the record
	// failed. The artifact is the durable trail; this is the copy that reaches a person
	// who is not reading issues.
	if _, err := t.notifier.NotifyEscalation(ctx, notifyID(a.Proposal.Identity),
		ChatSummary(a, o), string(ref), o.Consequence.Escalate); err != nil {
		errs = append(errs, fmt.Errorf("disclose: announcing %q: %w", ref, err))
	}
	return errors.Join(errs...)
}

// escalate pushes a failed unattended action to a person: the label the other two trails
// use, plus a comment saying what happened and how to revoke the rule that permitted it.
func (t *Trail) escalate(ctx context.Context, ref Ref, a Action, o Outcome) error {
	var errs []error
	if err := t.sink.AddLabel(ctx, ref, NeedsHumanLabel); err != nil {
		errs = append(errs, fmt.Errorf("disclose: marking %q for a human: %w", ref, err))
	}
	if err := t.sink.Comment(ctx, ref, EscalationComment(a, o)); err != nil {
		errs = append(errs, fmt.Errorf("disclose: escalating on %q: %w", ref, err))
	}
	return errors.Join(errs...)
}

// Revocations returns the shapes a person has revoked, keyed by shape, with the artifact
// that carries the revocation.
//
// It reads the OPEN artifacts, which is what makes removing the label — or closing the
// issue — lift the revocation. Both are one action for a person, and neither requires
// them to remember a second mechanism for undoing what the first one did.
//
// An artifact whose shape marker is missing or unreadable is skipped, and skipping is
// the safe direction here: a revocation that cannot be attributed to a shape cannot be
// applied to one, and the alternative — revoking everything on an unreadable marker —
// would let one malformed body silently disable autonomy the operator had earned.
// Callers that need to know report [Disclosed] separately; the count is in the report.
func (t *Trail) Revocations(ctx context.Context) (map[autonomy.Shape]Ref, error) {
	open, err := t.sink.ListOpen(ctx)
	if err != nil {
		return nil, fmt.Errorf("disclose: reading the disclosure trail: %w", err)
	}
	revoked := make(map[autonomy.Shape]Ref)
	for _, d := range open {
		if !d.Revoked || d.Shape.Cluster == "" || d.Shape.Operation == "" {
			continue
		}
		if _, seen := revoked[d.Shape]; !seen {
			revoked[d.Shape] = d.Ref
		}
	}
	return revoked, nil
}

// RecordExecution posts the note the execution layer writes the instant a mutation
// lands, and applies [AppliedLabel].
//
// It implements [execute.Recorder]. On the gated path the same call applies the executed
// label to an approval artifact, where that label is what durably prevents a SECOND
// execution; here it is not doing that job — there is no approval to re-honor — and it
// is doing a different one. Its absence on a finished artifact is the only evidence that
// an action started and the process died before it could report.
//
// It refuses an authorization that is not policy-authorized. A human-approved action
// recorded on this trail would be a public artifact headed "no human approved this"
// describing an action a person did approve, which is the mirror image of the lie the
// authority field exists to prevent. The blanket bypass is refused for the same reason
// with the sign flipped: it belongs to the approval trail, whose artifact it was granted
// against, and disclosing it here would attribute it to an earned rule.
func (t *Trail) RecordExecution(ctx context.Context, auth *approve.Authorization, detail string) error {
	ref, err := t.refFor(auth)
	if err != nil {
		return err
	}
	var errs []error
	if err := t.sink.Comment(ctx, ref, ExecutionComment(detail)); err != nil {
		errs = append(errs, fmt.Errorf("disclose: recording the execution on %q: %w", ref, err))
	}
	// The label is applied even if the comment failed: the comment is for a person, the
	// label is the machine-readable fact that a mutation landed.
	if err := t.sink.AddLabel(ctx, ref, AppliedLabel); err != nil {
		errs = append(errs, fmt.Errorf("disclose: marking %q applied: %w", ref, err))
	}
	return errors.Join(errs...)
}

// RecordOutcome posts a note without touching any label. It implements
// [execute.Recorder] and is how the audit lifecycle reaches the artifact on every path
// out of the runner, including the ones that sent nothing.
func (t *Trail) RecordOutcome(ctx context.Context, auth *approve.Authorization, note string) error {
	ref, err := t.refFor(auth)
	if err != nil {
		return err
	}
	if note == "" {
		return errors.New("disclose: refusing to record an empty outcome note")
	}
	if err := t.sink.Comment(ctx, ref, note); err != nil {
		return fmt.Errorf("disclose: recording an outcome on %q: %w", ref, err)
	}
	return nil
}

// refFor recovers the disclosure this authorization was minted against, refusing every
// slip that does not belong on this trail. See [Trail.RecordExecution] for why each
// refusal is a refusal rather than a best-effort write.
func (t *Trail) refFor(auth *approve.Authorization) (Ref, error) {
	switch {
	case !auth.Valid():
		return "", errors.New("disclose: refusing to record against an authorization the gate did not issue")
	case auth.Authority() != approve.AuthorityPolicy:
		return "", fmt.Errorf("disclose: refusing to disclose an action authorized by %s — this trail is for unattended actions only",
			auth.Authority())
	case auth.Approver() == approve.AutoApprovePolicy:
		return "", errors.New("disclose: refusing to disclose a blanket auto-approve bypass as earned autonomy; it belongs on the approval trail it was granted against")
	case auth.Ref() == "":
		return "", errors.New("disclose: the authorization carries no artifact reference to record against")
	}
	return Ref(auth.Ref()), nil
}

// notifyID adapts a proposal identity to the notifier's key type, exactly as the
// approval gate does: both are opaque, stable dedup handles, so one threads through as
// the other and every message about one action lands in one chat thread.
func notifyID(id remediate.ProposalIdentity) detect.Identity { return detect.Identity(id) }

// The trail must be usable as the execution layer's recorder. If [execute.Recorder]
// moves, the build fails here rather than at the wiring site.
var _ execute.Recorder = (*Trail)(nil)
