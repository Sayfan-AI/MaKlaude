package disclose

import (
	"context"
	"sort"
	"sync"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Disclosed is one open disclosure artifact as the cycle reads it back.
//
// It carries the whole body rather than only the parsed markers because the body is
// what a rebuild reads: the lifecycle marker lives there, and fetching it separately
// per artifact would be a second round trip for something the list call already
// returned. Everything else here is derived from the body or the labels.
type Disclosed struct {
	// Ref is the artifact.
	Ref Ref

	// Identity is the proposal the artifact discloses, from the body's marker.
	Identity remediate.ProposalIdentity

	// Shape is the (cluster, operation) pair the action's autonomy was earned at, from
	// the body's marker. It is what [Revoked] revokes.
	Shape autonomy.Shape

	// Revoked reports that a person applied [RevokedLabel]. See the package doc: it is
	// the single revocation signal and it takes effect on the next cycle.
	Revoked bool

	// Applied reports that [AppliedLabel] is present — a real mutation landed. Its
	// ABSENCE on an artifact whose action has finished is the signal worth having: an
	// action that started and never reported back.
	Applied bool

	// Body is the artifact's body, markers included.
	Body string
}

// Sink is the narrow, side-effecting boundary between this package's rendering and
// whatever external system holds the disclosure trail.
//
// It is a SEPARATE interface from [escalate.IssueSink] and [approve.ApprovalSink]
// rather than a reuse of either, for the reason approve's own doc gives about labels:
// three trails that can list each other's artifacts are three trails that can act on
// each other's artifacts, and separate interfaces backed by separate label queries make
// the isolation structural rather than a property of each parser skipping what it does
// not recognize.
//
// There is deliberately no Update taking a label set. A disclosure body is rewritten
// once, when the action finishes, and the artifact may by then be carrying
// [RevokedLabel] applied by a person — a full-label-set Update would silently drop it,
// which is the one label whose loss changes what MaKlaude is allowed to do. [SetBody]
// cannot.
//
// An implementation MUST scope every operation to artifacts carrying [ManagedLabel].
type Sink interface {
	// ListOpen returns every open, MaKlaude-managed disclosure artifact. Artifacts whose
	// proposal marker is missing or unparseable are skipped — they are not this trail's
	// to manage even if they happen to carry the label.
	ListOpen(ctx context.Context) ([]Disclosed, error)

	// Create opens a new artifact and returns a reference to it. The body already
	// contains the markers.
	Create(ctx context.Context, title, body string, labels []string) (Ref, error)

	// SetBody rewrites an artifact's body and touches nothing else — not its title, and
	// above all not its labels. See the interface doc.
	SetBody(ctx context.Context, ref Ref, body string) error

	// Comment adds a comment. It is how the execution layer's notes reach the trail
	// while the action is still running.
	Comment(ctx context.Context, ref Ref, body string) error

	// AddLabel adds a single label without disturbing the others.
	AddLabel(ctx context.Context, ref Ref, label string) error

	// Close closes an artifact. MaKlaude closes one only when it never opened an action
	// behind it; a disclosed action's artifact is closed by a person, as their
	// acknowledgement.
	Close(ctx context.Context, ref Ref) error
}

// memArtifact is one artifact held by a [MemorySink].
type memArtifact struct {
	ref      Ref
	title    string
	body     string
	labels   map[string]bool
	comments []string
	open     bool
}

// MemorySink is an in-memory [Sink] for tests and for the rehearsal path when no real
// trail is configured. It records every operation faithfully — bodies, labels, comments,
// open state — so a test can assert the full effect of a pass without a network.
//
// It is safe for concurrent use so it can stand in for a real sink anywhere.
type MemorySink struct {
	mu        sync.Mutex
	nextID    int
	artifacts map[Ref]*memArtifact
}

// NewMemorySink returns an empty in-memory sink.
func NewMemorySink() *MemorySink {
	return &MemorySink{artifacts: make(map[Ref]*memArtifact)}
}

// ListOpen returns the open, marker-tagged artifacts currently held, in reference order
// so a test sees deterministic output.
func (s *MemorySink) ListOpen(_ context.Context) ([]Disclosed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Disclosed
	for _, a := range s.artifacts {
		if !a.open {
			continue
		}
		id, ok := ParseProposalMarker(a.body)
		if !ok {
			continue
		}
		shape, _ := ParseShapeMarker(a.body)
		out = append(out, Disclosed{
			Ref:      a.ref,
			Identity: id,
			Shape:    shape,
			Revoked:  a.labels[RevokedLabel],
			Applied:  a.labels[AppliedLabel],
			Body:     a.body,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Create records a new open artifact and returns its reference.
func (s *MemorySink) Create(_ context.Context, title, body string, labels []string) (Ref, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	ref := Ref(intToRef(s.nextID))
	a := &memArtifact{ref: ref, title: title, body: body, labels: make(map[string]bool, len(labels)), open: true}
	for _, l := range labels {
		a.labels[l] = true
	}
	s.artifacts[ref] = a
	return ref, nil
}

// SetBody rewrites the stored body, leaving the title, labels and comments alone.
func (s *MemorySink) SetBody(_ context.Context, ref Ref, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.body = body
	return nil
}

// Comment appends a comment to an existing artifact.
func (s *MemorySink) Comment(_ context.Context, ref Ref, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.comments = append(a.comments, body)
	return nil
}

// AddLabel adds one label, leaving the rest alone.
func (s *MemorySink) AddLabel(_ context.Context, ref Ref, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.labels[label] = true
	return nil
}

// Close marks an artifact closed.
func (s *MemorySink) Close(_ context.Context, ref Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	a, ok := s.artifacts[ref]
	if !ok {
		return &NotFoundError{Ref: ref}
	}
	a.open = false
	return nil
}

// Revoke simulates a HUMAN applying [RevokedLabel]. It exists as its own method so a
// test expresses the revocation the way a person performs it, rather than reaching for
// [MemorySink.AddLabel] — which is MaKlaude labelling its own artifact and is never how
// a revocation arrives.
func (s *MemorySink) Revoke(ref Ref) error {
	return s.AddLabel(context.Background(), ref, RevokedLabel)
}

// Snapshot returns a read-only copy of one artifact for test assertions. ok is false if
// no such artifact was ever created.
func (s *MemorySink) Snapshot(ref Ref) (ArtifactView, bool) {
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

// OpenCount returns how many artifacts are currently open.
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
	Ref      Ref
	Title    string
	Body     string
	Labels   []string
	Comments []string
	Open     bool
}

// HasLabel reports whether the view carries a label.
func (v ArtifactView) HasLabel(label string) bool {
	for _, l := range v.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// NotFoundError is returned by a sink when an operation references an artifact it does
// not hold. It is its own type so callers can distinguish "the artifact is gone" from a
// transport error.
type NotFoundError struct {
	Ref Ref
}

func (e *NotFoundError) Error() string {
	return "disclose: disclosure artifact not found: " + string(e.Ref)
}

// intToRef renders an integer id as a reference string, kept tiny and dependency-free
// to mirror the other two trails' helpers.
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
var _ Sink = (*MemorySink)(nil)
