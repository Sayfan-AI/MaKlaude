package approve

import (
	"context"
	"sort"
	"sync"
	"time"
)

// ApprovalSink is the narrow, side-effecting boundary between the pure decision
// core and whatever external system holds the approval trail (GitHub here).
// Keeping it small and behind an interface is what lets [Decide], [Reconcile], and
// the [Gatekeeper] be exercised with no network.
//
// It is deliberately a SEPARATE interface from [escalate.IssueSink] rather than an
// extension of it, for two reasons that both matter more than the code it
// duplicates. First, this sink must read state the escalation trail never needed:
// which decision labels are present, WHO applied them, and WHEN — the attribution
// the whole gate rests on. Second, the two trails must not be able to act on each
// other's issues; separate interfaces backed by separate label queries make that a
// structural property rather than a convention.
//
// All operations are scoped to MaKlaude-managed approval artifacts only. An
// implementation MUST filter to [ManagedLabel] so the gate can never touch an issue
// a human opened by hand.
type ApprovalSink interface {
	// ListOpen returns every open, MaKlaude-managed approval artifact as a
	// [PendingAction], with its identity and preview recovered from the body markers
	// and its decision recovered from labels plus their events. Artifacts whose
	// proposal marker is missing or unparseable are skipped — they are not the gate's
	// to manage.
	ListOpen(ctx context.Context) ([]PendingAction, error)

	// Create opens a new artifact and returns a reference to it. The body already
	// contains the markers.
	Create(ctx context.Context, title, body string, labels []string) (ActionRef, error)

	// Update rewrites an existing artifact's title, body, and labels. Callers must
	// pass the full intended label set; see [LabelsFor] for why decision labels are
	// preserved explicitly rather than regenerated.
	Update(ctx context.Context, ref ActionRef, title, body string, labels []string) error

	// Comment adds a comment, used for refusals, executions, and closing notes.
	Comment(ctx context.Context, ref ActionRef, body string) error

	// AddLabel adds a single label without disturbing the others. It is how an
	// execution is recorded durably.
	AddLabel(ctx context.Context, ref ActionRef, label string) error

	// RemoveLabel removes a single label without disturbing the others. It is how an
	// approval that cannot be honored is withdrawn, leaving the artifact and its
	// history intact.
	RemoveLabel(ctx context.Context, ref ActionRef, label string) error

	// Close closes an artifact. The gatekeeper always leaves a closing comment
	// first, so Close only needs to flip the state.
	Close(ctx context.Context, ref ActionRef) error
}

// memArtifact is one artifact held by a [MemorySink].
type memArtifact struct {
	ref      ActionRef
	title    string
	body     string
	labels   map[string]bool
	events   map[string]labelEvent
	comments []string
	open     bool
}

// labelEvent records who applied a label, when, and whether that actor was
// MaKlaude itself — the in-memory stand-in for GitHub's issue-events endpoint. It
// exists so the attribution path is exercised by unit tests rather than only by the
// live sink: "the approver identity is captured" is the property most worth testing
// and the one hardest to test against a network.
//
// isSelf is resolved where the actor is recorded rather than where the decision is
// read, because only the sink knows which account MaKlaude runs as. See
// [PendingAction.ApproverIsSelf].
type labelEvent struct {
	actor  string
	at     time.Time
	isSelf bool
}

// decision is what a label set plus its events say a human decided about an
// artifact. It is a struct rather than four return values so a caller cannot
// silently drop isSelf — the one field whose absence turns the gate off completely
// (a sink that forgets it lets MaKlaude approve its own proposals).
type decision struct {
	state     State
	approver  string
	decidedAt time.Time
	isSelf    bool
}

// MemorySink is an in-memory [ApprovalSink] for tests and for the dry-run path
// when no real trail is configured. It records every operation faithfully — bodies,
// labels, label attribution, comments, open state — so a test can assert the full
// effect of a reconciliation pass without a network.
//
// It is safe for concurrent use so it can stand in for a real sink anywhere.
type MemorySink struct {
	// SelfLogin is the account the sink treats as MaKlaude itself, so a decision
	// applied by that account is reported as [PendingAction.ApproverIsSelf] and
	// refused by the gate. It is exported (rather than a constructor argument)
	// because the common case — a test that does not care — should not have to
	// mention it, while the test that DOES care is the one exercising the most
	// important refusal in the package.
	SelfLogin string

	mu        sync.Mutex
	nextID    int
	artifacts map[ActionRef]*memArtifact
}

// NewMemorySink returns an empty in-memory sink.
func NewMemorySink() *MemorySink {
	return &MemorySink{artifacts: make(map[ActionRef]*memArtifact)}
}

// ListOpen returns the open, marker-tagged artifacts currently held, in reference
// order so a test sees deterministic output.
func (s *MemorySink) ListOpen(_ context.Context) ([]PendingAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []PendingAction
	for _, a := range s.artifacts {
		if !a.open {
			continue
		}
		id, ok := ParseProposalMarker(a.body)
		if !ok {
			continue
		}
		pa := PendingAction{Identity: id, Ref: a.ref, Executed: a.labels[ExecutedLabel]}
		pa.ThreadTS, _ = ParseThreadMarker(a.body)
		pa.PreviewedResourceVersion, pa.PreviewedAt, _ = ParsePreviewMarker(a.body)
		pa.PreviewedState = ParsePreviewStateMarker(a.body)
		d := decisionFrom(a.labels, a.events)
		pa.State, pa.Approver, pa.DecidedAt, pa.ApproverIsSelf = d.state, d.approver, d.decidedAt, d.isSelf
		out = append(out, pa)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// decisionFrom maps a label set plus its events onto a decision. Rejection wins a
// contradictory pair: if both labels are somehow present, the safe reading is the
// one that does not authorize a mutation. This is shared by the memory sink and the
// GitHub sink so the two cannot disagree about what a given label set means.
func decisionFrom(labels map[string]bool, events map[string]labelEvent) decision {
	if labels[RejectedLabel] {
		ev := events[RejectedLabel]
		return decision{state: StateRejected, approver: ev.actor, decidedAt: ev.at, isSelf: ev.isSelf}
	}
	if labels[ApprovedLabel] {
		ev := events[ApprovedLabel]
		return decision{state: StateApproved, approver: ev.actor, decidedAt: ev.at, isSelf: ev.isSelf}
	}
	return decision{state: StatePending}
}

// Create records a new open artifact and returns its reference.
func (s *MemorySink) Create(_ context.Context, title, body string, labels []string) (ActionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	ref := ActionRef(intToRef(s.nextID))
	a := &memArtifact{
		ref:    ref,
		title:  title,
		body:   body,
		labels: make(map[string]bool, len(labels)),
		events: make(map[string]labelEvent),
		open:   true,
	}
	for _, l := range labels {
		a.labels[l] = true
	}
	s.artifacts[ref] = a
	return ref, nil
}

// Update rewrites the stored title, body, and labels of an existing artifact.
func (s *MemorySink) Update(_ context.Context, ref ActionRef, title, body string, labels []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.title = title
	a.body = body
	a.labels = make(map[string]bool, len(labels))
	for _, l := range labels {
		a.labels[l] = true
	}
	return nil
}

// Comment appends a comment to an existing artifact.
func (s *MemorySink) Comment(_ context.Context, ref ActionRef, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.comments = append(a.comments, body)
	return nil
}

// AddLabel adds one label, attributing it to MaKlaude itself. A human decision is
// recorded through [MemorySink.Decide] instead, which is what makes the two
// distinguishable in a test: an approval MaKlaude applied to itself would be
// exactly the forgery this gate exists to prevent.
//
// The event is marked isSelf unconditionally, not only when [MemorySink.SelfLogin]
// happens to be set. Anything reaching this method is MaKlaude acting, whatever
// account it runs under, so a sink with no configured login must not degrade into
// one that reports its own labels as human decisions.
func (s *MemorySink) AddLabel(_ context.Context, ref ActionRef, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.labels[label] = true
	a.events[label] = labelEvent{actor: s.SelfLogin, at: time.Now().UTC(), isSelf: true}
	return nil
}

// RemoveLabel removes one label, leaving the rest and the artifact's history
// intact. The label's event is dropped with it, so a re-applied label carries a new
// attribution rather than inheriting the old one.
func (s *MemorySink) RemoveLabel(_ context.Context, ref ActionRef, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	delete(a.labels, label)
	delete(a.events, label)
	return nil
}

// Close marks an artifact closed.
func (s *MemorySink) Close(_ context.Context, ref ActionRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.open = false
	return nil
}

// Decide simulates a HUMAN applying a decision label, recording the actor and the
// instant exactly as a real label event would. It is the test seam for the one
// input this system cannot generate for itself, and it takes an explicit actor and
// time so a test can express "approved by someone, before the preview was
// refreshed" — the ordering case that a wall clock could not reproduce.
func (s *MemorySink) Decide(ref ActionRef, label, actor string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.labels[label] = true
	a.events[label] = labelEvent{
		actor:  actor,
		at:     at.UTC(),
		isSelf: s.SelfLogin != "" && actor == s.SelfLogin,
	}
	return nil
}

// Snapshot returns a read-only copy of one artifact's recorded state for test
// assertions. ok is false if no such artifact was ever created.
func (s *MemorySink) Snapshot(ref ActionRef) (ArtifactView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return ArtifactView{}, false
	}
	labels := make([]string, 0, len(a.labels))
	for l := range a.labels {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return ArtifactView{
		Ref:      a.ref,
		Title:    a.title,
		Body:     a.body,
		Labels:   labels,
		Comments: append([]string(nil), a.comments...),
		Open:     a.open,
	}, true
}

// OpenCount returns how many artifacts are currently open — the direct assertion
// for "a recurring proposal did not open a duplicate".
func (s *MemorySink) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, a := range s.artifacts {
		if a.open {
			n++
		}
	}
	return n
}

// ArtifactView is a read-only copy of a [MemorySink] artifact for test assertions.
// Labels are sorted so comparisons are stable.
type ArtifactView struct {
	Ref      ActionRef
	Title    string
	Body     string
	Labels   []string
	Comments []string
	Open     bool
}

// HasLabel reports whether the view carries a label, a convenience for the many
// assertions that turn on exactly that.
func (v ArtifactView) HasLabel(label string) bool {
	for _, l := range v.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// NotFoundError is returned by a sink when an operation references an artifact it
// does not hold. It is its own type so callers can distinguish "the artifact is
// gone" (which a pass can tolerate) from transport errors.
type NotFoundError struct {
	Ref ActionRef
}

func (e *NotFoundError) Error() string {
	return "approve: approval artifact not found: " + string(e.Ref)
}

// intToRef renders an integer id as a reference string, kept tiny and
// dependency-free to mirror the escalation sink's helper.
func intToRef(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Ensure the in-memory sink satisfies the interface at compile time.
var _ ApprovalSink = (*MemorySink)(nil)
