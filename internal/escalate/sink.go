package escalate

import (
	"context"
	"sync"
)

// IssueSink is the narrow, side-effecting boundary between the pure reconcile
// core and whatever external comms system actually holds the trail (GitHub for
// M1). Keeping it small and behind an interface is what lets [Reconcile] and the
// [Escalator] be tested with no network: a fake sink ([MemorySink]) substitutes
// for the real one.
//
// All operations are scoped to MaKlaude-managed issues only. A real
// implementation MUST filter to issues it manages (for example by the
// [ManagedLabel]) so the escalator never touches issues a human opened by hand.
//
// Implementations should be safe to call from a single reconciliation goroutine;
// they are not required to be concurrency-safe across simultaneous reconciles of
// the same repo, since a monitor reconciles one cluster's findings at a time.
type IssueSink interface {
	// ListOpen returns every currently-open, MaKlaude-managed issue, each as a
	// [TrackedIssue] with its identity parsed from the body marker. Issues whose
	// marker is missing or unparseable are skipped — they are not ours to manage.
	ListOpen(ctx context.Context) ([]TrackedIssue, error)

	// Create opens a new issue with the given title, body, and labels, returning
	// a reference to it. The body already contains the identity marker.
	Create(ctx context.Context, title, body string, labels []string) (IssueRef, error)

	// Update rewrites an existing issue's body (and labels) to the latest state.
	// It is used on recurrence so the issue always reflects the current problem.
	Update(ctx context.Context, ref IssueRef, title, body string, labels []string) error

	// Comment adds a comment to an existing issue, used both for recurrence notes
	// and for the closing note that keeps the trail auditable.
	Comment(ctx context.Context, ref IssueRef, body string) error

	// Close closes an existing issue. The escalator always leaves a closing
	// comment first, so Close itself only needs to flip the state.
	Close(ctx context.Context, ref IssueRef) error
}

// memIssue is one issue held by a [MemorySink].
type memIssue struct {
	ref      IssueRef
	title    string
	body     string
	labels   []string
	comments []string
	open     bool
}

// MemorySink is an in-memory [IssueSink] for tests and for the no-op / dry-run
// path when no real comms backend is configured. It records every operation
// faithfully so a test can assert the full effect of a reconcile (which issues
// exist, their bodies and labels, what comments were added, what was closed)
// without any network.
//
// It is safe for concurrent use so it can stand in for a real sink anywhere.
type MemorySink struct {
	mu     sync.Mutex
	nextID int
	issues map[IssueRef]*memIssue
}

// NewMemorySink returns an empty in-memory sink.
func NewMemorySink() *MemorySink {
	return &MemorySink{issues: make(map[IssueRef]*memIssue)}
}

// ListOpen returns the open, identity-tagged issues currently held.
func (s *MemorySink) ListOpen(_ context.Context) ([]TrackedIssue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []TrackedIssue
	for _, iss := range s.issues {
		if !iss.open {
			continue
		}
		id, ok := ParseIdentityMarker(iss.body)
		if !ok {
			continue
		}
		out = append(out, TrackedIssue{Identity: id, Ref: iss.ref})
	}
	return out, nil
}

// Create records a new open issue and returns its reference.
func (s *MemorySink) Create(_ context.Context, title, body string, labels []string) (IssueRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	ref := IssueRef(intToRef(s.nextID))
	s.issues[ref] = &memIssue{
		ref:    ref,
		title:  title,
		body:   body,
		labels: append([]string(nil), labels...),
		open:   true,
	}
	return ref, nil
}

// Update rewrites the stored title/body/labels of an existing issue.
func (s *MemorySink) Update(_ context.Context, ref IssueRef, title, body string, labels []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	iss, ok := s.issues[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	iss.title = title
	iss.body = body
	iss.labels = append([]string(nil), labels...)
	return nil
}

// Comment appends a comment to an existing issue.
func (s *MemorySink) Comment(_ context.Context, ref IssueRef, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	iss, ok := s.issues[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	iss.comments = append(iss.comments, body)
	return nil
}

// Close marks an existing issue closed.
func (s *MemorySink) Close(_ context.Context, ref IssueRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	iss, ok := s.issues[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	iss.open = false
	return nil
}

// Snapshot returns a read-only copy of one issue's recorded state for test
// assertions. ok is false if no such issue was ever created.
func (s *MemorySink) Snapshot(ref IssueRef) (IssueView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	iss, ok := s.issues[ref]
	if !ok {
		return IssueView{}, false
	}
	return IssueView{
		Ref:      iss.ref,
		Title:    iss.title,
		Body:     iss.body,
		Labels:   append([]string(nil), iss.labels...),
		Comments: append([]string(nil), iss.comments...),
		Open:     iss.open,
	}, true
}

// OpenCount returns how many issues are currently open in the sink, a convenient
// assertion for "recurrence did not open a duplicate".
func (s *MemorySink) OpenCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for _, iss := range s.issues {
		if iss.open {
			n++
		}
	}
	return n
}

// IssueView is a read-only copy of a [MemorySink] issue for test assertions.
type IssueView struct {
	Ref      IssueRef
	Title    string
	Body     string
	Labels   []string
	Comments []string
	Open     bool
}

// NotFoundError is returned by a sink when an operation references an issue it
// does not hold. It is its own type so callers can distinguish "the issue is
// gone" (which a reconcile can tolerate) from transport errors.
type NotFoundError struct {
	Ref IssueRef
}

func (e *NotFoundError) Error() string {
	return "escalate: issue not found: " + string(e.Ref)
}

// intToRef renders an integer issue id as a reference string without importing
// strconv at call sites; kept tiny and dependency-free on purpose.
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
