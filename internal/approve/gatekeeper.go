package approve

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Gatekeeper is the side-effecting shell around the pure [Reconcile] core. Given
// the proposals MaKlaude is currently making, it re-reads the approval trail from
// its [ApprovalSink], asks Reconcile for a plan, executes that plan against the
// trail — and returns the [Authorization]s the plan produced.
//
// # It returns permission; it never uses it
//
// The gatekeeper has no cluster client and no path to one. Its output is a list of
// permission slips its caller may act on, which keeps the "decide" and "do"
// responsibilities in different packages with different dependencies: this one can
// only write to a comms trail, and the executor can only act on something this one
// produced. Neither is capable of the other's mistake.
//
// # State is rediscovered, never remembered
//
// It holds no per-artifact state between passes: every [Gatekeeper.Reconcile]
// re-lists from the sink, so it is correct across process restarts and cannot act
// on a decision it merely remembers. The markers and labels on the artifact are the
// durable truth — including the execution label, which is why a crash between
// acting and recording cannot produce a second execution.
//
// # Chat is best-effort and never load-bearing
//
// Like [escalate.Escalator], the gatekeeper mirrors the lifecycle into chat via a
// [notify.Notifier], persisting the returned thread handle in the artifact body so
// continuity survives restarts. A notifier error is recorded and never fails the
// pass — and, more importantly, a chat message is never the approval signal. A
// human approves by labelling the artifact, so a chat outage can delay the
// conversation but can never lose, forge, or fabricate a decision.
type Gatekeeper struct {
	sink     ApprovalSink
	notifier notify.Notifier
	policy   Policy
	now      func() time.Time
}

// NewGatekeeper builds a gatekeeper over the given sink.
//
// A nil sink panics: a caller wanting a no-op should pass a [MemorySink], making
// the no-op explicit rather than hiding it behind a nil that would silently swallow
// approval requests. A nil notifier becomes a [notify.NopNotifier], so an
// unconfigured chat backend degrades to the issue trail alone. A zero [Policy]
// takes the shipped defaults — see [Policy.ApprovalTTL] for why a forgotten field
// must not be read as "never expires" or "expires instantly".
func NewGatekeeper(sink ApprovalSink, notifier notify.Notifier, policy Policy) *Gatekeeper {
	if sink == nil {
		panic("approve: NewGatekeeper requires a non-nil sink (use NewMemorySink for a no-op)")
	}
	if notifier == nil {
		notifier = notify.NopNotifier{}
	}
	return &Gatekeeper{sink: sink, notifier: notifier, policy: policy.normalized(), now: time.Now}
}

// WithClock replaces the gatekeeper's clock, for tests that need to place an
// approval before or after a preview without sleeping. It returns the receiver so
// it can be chained onto construction.
func (g *Gatekeeper) WithClock(now func() time.Time) *Gatekeeper {
	if now != nil {
		g.now = now
	}
	return g
}

// Policy returns the gate's effective policy, with defaults already applied.
func (g *Gatekeeper) Policy() Policy { return g.policy }

// Result summarizes one reconciliation pass.
//
// The counts are for logs and assertions; Authorized is the part with
// consequences. It carries every action a human has actually allowed to run — and
// nothing else, ever: a pass that produces no authorizations returns an empty slice
// rather than a nil-vs-empty distinction a caller might get wrong.
type Result struct {
	// Opened is how many new approval requests were put to a human.
	Opened int

	// Refreshed is how many pending artifacts were re-rendered with current
	// evidence.
	Refreshed int

	// Held is how many artifacts were deliberately left untouched: rejected, already
	// executed, or still pending against evidence the artifact already displays.
	Held int

	// Refused is how many approvals were present but could not be honored, and were
	// withdrawn with an explanation.
	Refused int

	// Withdrawn is how many artifacts were closed because the proposal was no longer
	// being made — the self-heal path, which runs NOTHING.
	Withdrawn int

	// Authorized carries the permission slips this pass issued, in the plan's
	// deterministic order. Each is valid, attributable, and bound to one object at
	// one resourceVersion.
	Authorized []*Authorization
}

// String renders a compact, log-friendly summary.
func (r Result) String() string {
	return fmt.Sprintf("approval: opened=%d refreshed=%d held=%d refused=%d withdrawn=%d authorized=%d",
		r.Opened, r.Refreshed, r.Held, r.Refused, r.Withdrawn, len(r.Authorized))
}

// Reconcile brings the approval trail in line with the current proposals for one
// pass and returns what it did, including any authorizations issued.
//
// reqs should be the full current set of proposals the caller wants reflected. The
// full set matters: an artifact whose proposal is ABSENT from reqs is withdrawn, so
// passing a partial set would close approval requests that are still valid. A
// caller that scans clusters independently must therefore either pass all clusters'
// proposals together or scope the sink per cluster — the same contract
// [escalate.Escalator.Reconcile] carries, for the same reason.
//
// The pass is best-effort and continues past per-artifact errors so one transient
// failure does not strand the rest of the trail. Errors are aggregated and returned
// together, with the [Result] still reporting what succeeded. One asymmetry is
// deliberate: an error while REFUSING an approval suppresses nothing, because the
// refusal path never authorizes anything, whereas an error anywhere in the
// authorize path drops that authorization rather than returning a slip whose
// artifact was not updated.
func (g *Gatekeeper) Reconcile(ctx context.Context, reqs []Request) (Result, error) {
	tracked, err := g.sink.ListOpen(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("approve: listing approval artifacts: %w", err)
	}

	now := g.now().UTC()
	plan := Reconcile(reqs, tracked, g.policy, now)
	trackedByID := indexTracked(tracked)

	res := Result{Authorized: []*Authorization{}}
	var errs []error

	for _, a := range plan {
		switch a.Kind {
		case ActionOpen:
			if err := g.open(ctx, a, now); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Opened++

		case ActionRefresh:
			if err := g.refresh(ctx, a, trackedByID[a.Identity], now); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Refreshed++

		case ActionHold:
			// Deliberately no sink call. A rejected or already-executed artifact is
			// frozen at what was actually decided or done, and an unchanged pending one
			// must not have its preview instant re-stamped — see [ReasonPreviewCurrent].
			res.Held++

		case ActionRefuse:
			if err := g.refuse(ctx, a, trackedByID[a.Identity], now); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Refused++

		case ActionWithdraw:
			if err := g.withdraw(ctx, a); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Withdrawn++

		case ActionAuthorize:
			// The artifact is annotated BEFORE the authorization is handed back, so a
			// failure here means the caller never receives a slip whose trail does not
			// record it. The audit record is not allowed to lag the permission.
			if err := g.recordAuthorization(ctx, a); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Authorized = append(res.Authorized, a.Authorization)

		default:
			errs = append(errs, fmt.Errorf("approve: unknown action kind %d for %q", a.Kind, a.Identity))
		}
	}

	return res, errors.Join(errs...)
}

// open creates a new artifact, mirrors it into chat as a thread root, and patches
// the returned thread handle back into the body so the conversation is
// rediscoverable after a restart.
func (g *Gatekeeper) open(ctx context.Context, a Action, now time.Time) error {
	pending := PendingAction{State: StatePending}
	title := Title(a.Request)
	body := Body(a.Request, now)

	ref, err := g.sink.Create(ctx, title, body, LabelsFor(pending))
	if err != nil {
		return fmt.Errorf("opening approval request for %q: %w", a.Identity, err)
	}

	threadTS, nerr := g.notifier.NotifyEscalation(ctx, g.notifyID(a.Identity), ApprovalSummary(a.Request), string(ref), true)
	if nerr != nil {
		return fmt.Errorf("announcing approval request for %q: %w", a.Identity, nerr)
	}
	if threadTS == "" {
		return nil
	}
	if uerr := g.sink.Update(ctx, ref, title, withThreadMarker(body, threadTS), LabelsFor(pending)); uerr != nil {
		return fmt.Errorf("persisting thread marker on %q for %q: %w", ref, a.Identity, uerr)
	}
	return nil
}

// refresh re-renders a still-pending artifact against current evidence, stamping a
// new preview instant. Stamping it is what makes an approval given against the
// PREVIOUS body detectably stale ([ReasonApprovalPredatesPreview]) — the refresh is
// not merely cosmetic, it invalidates consent to what is no longer displayed.
//
// No comment is posted. A pending artifact is refreshed on every pass, and a
// comment per pass would bury the human decision this issue exists to collect under
// a stream of machine chatter.
func (g *Gatekeeper) refresh(ctx context.Context, a Action, pending PendingAction, now time.Time) error {
	body := withThreadMarker(Body(a.Request, now), a.ThreadTS)
	if err := g.sink.Update(ctx, a.Ref, Title(a.Request), body, LabelsFor(pending)); err != nil {
		return fmt.Errorf("refreshing approval request %q for %q: %w", a.Ref, a.Identity, err)
	}
	return nil
}

// refuse explains why an approval cannot be honored, withdraws the approval label,
// and re-renders the artifact with current evidence so the human can decide again
// against what is actually true.
//
// The order is chosen so no window exists in which the artifact looks approved to a
// later pass but the human has not been told why it was not acted on: the comment
// lands first, the label comes off second, and only then is the body replaced.
// Removing the label is what actually revokes the authority — the comment is for
// the person, the label is for the gate.
func (g *Gatekeeper) refuse(ctx context.Context, a Action, pending PendingAction, now time.Time) error {
	if err := g.sink.Comment(ctx, a.Ref, RefusalComment(a.Request, pending, a.Reason, g.policy)); err != nil {
		return fmt.Errorf("explaining refusal on %q for %q: %w", a.Ref, a.Identity, err)
	}
	if err := g.sink.RemoveLabel(ctx, a.Ref, ApprovedLabel); err != nil {
		return fmt.Errorf("withdrawing approval on %q for %q: %w", a.Ref, a.Identity, err)
	}

	// The artifact is pending again from here, so it is re-rendered and re-labelled
	// as such.
	reopened := PendingAction{State: StatePending}
	body := withThreadMarker(Body(a.Request, now), a.ThreadTS)
	if err := g.sink.Update(ctx, a.Ref, Title(a.Request), body, LabelsFor(reopened)); err != nil {
		return fmt.Errorf("refreshing refused request %q for %q: %w", a.Ref, a.Identity, err)
	}

	if nerr := g.notifier.NotifyUpdate(ctx, g.notifyID(a.Identity), a.ThreadTS,
		RefusalComment(a.Request, pending, a.Reason, g.policy)); nerr != nil {
		return fmt.Errorf("announcing refusal for %q: %w", a.Identity, nerr)
	}
	return nil
}

// withdraw closes an artifact whose proposal is no longer being made, leaving a
// note that says explicitly that nothing was run.
func (g *Gatekeeper) withdraw(ctx context.Context, a Action) error {
	note := WithdrawalComment(a.Identity, a.Reason)
	if err := g.sink.Comment(ctx, a.Ref, note); err != nil {
		return fmt.Errorf("noting withdrawal on %q for %q: %w", a.Ref, a.Identity, err)
	}
	if err := g.sink.Close(ctx, a.Ref); err != nil {
		return fmt.Errorf("closing approval request %q for %q: %w", a.Ref, a.Identity, err)
	}
	if nerr := g.notifier.NotifyResolution(ctx, g.notifyID(a.Identity), a.ThreadTS, note); nerr != nil {
		return fmt.Errorf("announcing withdrawal for %q: %w", a.Identity, nerr)
	}
	return nil
}

// recordAuthorization notes on the trail that the gate honored an approval, before
// the permission slip leaves this package. It does NOT mark the artifact executed —
// the action has not run yet, and claiming it did would break idempotency in the
// dangerous direction if execution then failed and the proposal recurred.
func (g *Gatekeeper) recordAuthorization(ctx context.Context, a Action) error {
	auth := a.Authorization
	note := fmt.Sprintf(
		"**Approval honored.** MaKlaude is authorized to run `%s` on `%s` (cluster `%s`) at resourceVersion `%s`, "+
			"on the approval recorded by @%s at %s.\n\nThe action has **not** run yet; its outcome will be recorded here.",
		auth.Operation(), auth.Target().String(), auth.Cluster(), auth.Target().ResourceVersion,
		auth.Approver(), auth.ApprovedAt().UTC().Format(time.RFC3339))

	if err := g.sink.Comment(ctx, a.Ref, note); err != nil {
		return fmt.Errorf("recording authorization on %q for %q: %w", a.Ref, a.Identity, err)
	}
	if nerr := g.notifier.NotifyUpdate(ctx, g.notifyID(a.Identity), a.ThreadTS, note); nerr != nil {
		return fmt.Errorf("announcing authorization for %q: %w", a.Identity, nerr)
	}
	return nil
}

// RecordExecution closes the loop after a caller has run an authorized action: it
// posts the outcome and applies [ExecutedLabel] to the artifact.
//
// The label is the point. It is what makes single-execution durable rather than a
// property of one process's memory: the next pass reads it back through
// [ApprovalSink.ListOpen], sees [PendingAction.Executed], and holds the artifact
// instead of authorizing it again — even if the process that ran the action died
// immediately afterwards, and even if the same proposal is still being made because
// the cluster has not caught up yet.
//
// It is called AFTER execution rather than before, which is the correct direction
// for the two ways this can go wrong. Labelling first and crashing before acting
// would leave an action approved, unexecuted, and permanently un-authorizable —
// silent inaction. Acting first and crashing before labelling leaves a proposal
// that may be re-proposed and re-approved by a human, who sees the trail and
// decides — a visible re-ask rather than an invisible stall. Between a silent
// no-op and a re-ask a person is asked to confirm, the re-ask is the safer failure.
//
// It refuses an invalid authorization outright: recording an execution the gate
// never authorized would put a false entry in the audit trail, which is worse than
// no entry.
func (g *Gatekeeper) RecordExecution(ctx context.Context, auth *Authorization, detail string) error {
	if !auth.Valid() {
		return errors.New("approve: refusing to record an execution against an authorization the gate did not issue")
	}
	ref := auth.Ref()
	if ref == "" {
		return errors.New("approve: authorization carries no artifact reference to record against")
	}

	var errs []error
	if err := g.sink.Comment(ctx, ref, ExecutionComment(auth, detail)); err != nil {
		errs = append(errs, fmt.Errorf("recording execution on %q: %w", ref, err))
	}
	// The label is applied even if the comment failed: the comment is for a human,
	// the label is what stops a second execution.
	if err := g.sink.AddLabel(ctx, ref, ExecutedLabel); err != nil {
		errs = append(errs, fmt.Errorf("marking %q executed: %w", ref, err))
	}
	if err := g.sink.RemoveLabel(ctx, ref, NeedsHumanLabel); err != nil {
		errs = append(errs, fmt.Errorf("clearing the human gate on %q: %w", ref, err))
	}
	return errors.Join(errs...)
}

// notifyID adapts a proposal identity to the notifier's key type. The notifier is
// keyed on [detect.Identity] — an opaque, stable dedup handle — and a proposal
// identity is equally opaque and equally stable, so it threads through unchanged.
// This keeps one chat thread per proposed action without widening the notify
// interface, exactly as the escalation trail does with incident identities.
func (g *Gatekeeper) notifyID(id remediate.ProposalIdentity) detect.Identity {
	return detect.Identity(id)
}

// indexTracked keys the tracked artifacts by identity so the gatekeeper can recover
// the full [PendingAction] behind an action the plan describes. [Action] carries
// only what the pure layer needed to decide; the shell needs the rest — the
// approver, the previewed resourceVersion, the decision time — to render a refusal
// that tells a human exactly what changed under their approval. Keeping those
// fields out of Action is what stops the pure layer's contract from growing every
// time the wording of a comment does.
func indexTracked(tracked []PendingAction) map[remediate.ProposalIdentity]PendingAction {
	byID := make(map[remediate.ProposalIdentity]PendingAction, len(tracked))
	for i := range tracked {
		if _, seen := byID[tracked[i].Identity]; !seen {
			byID[tracked[i].Identity] = tracked[i]
		}
	}
	return byID
}
