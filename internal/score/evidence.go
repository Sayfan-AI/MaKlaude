package score

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// This file is the stored form: what a scorecard holds, and what it deliberately does
// not.
//
// # Why the evidence is a projection rather than the records
//
// [Fact] carries the fields the two verdicts are DERIVED FROM and nothing else. An
// [audit.Record] holds more — a pre-state, a rollback plan, free-text detail, the
// phase token — and storing all of it would be the easier choice and the misleading
// one: a reader of a scorecard would reasonably assume the fields in front of them
// were weighed. Every field below is read by [factFaults] or [fixVerdict]. Adding one
// that is not read is how a scorecard starts implying a judgement it never made.
//
// Two consequences worth stating rather than discovering:
//
//   - The projection is lossy on purpose and a bundle is not an audit trail. The audit
//     trail is the record of what happened; this is the record of what a verdict was
//     computed from. Keeping them separate is what lets the trail stay append-only and
//     the scorecard stay recomputable.
//   - The projection step itself is not covered by [Replay], which re-runs the verdict
//     over stored facts. A projection bug is caught by scoring real trails from real
//     runs — which is what `internal/execute`'s scenarios do — not by a round trip.
//
// # Why no free text is carried
//
// [audit.Trail.Append] redacts free-text fields, so a record's detail is already safe
// to publish. It is still left out: none of the bars reads it, and a scorecard that
// carries cluster-derived prose is one more artifact to reason about before publishing.
// Everything here is a stable token, an identifier, a bool, or a timestamp.

// BundleVersion is the stored bundle's schema version. It is written on every bundle
// and checked on every read, so a bundle from a future build is refused rather than
// half-understood — the same fail-loudly posture [trust.Open] takes with a ledger line
// it cannot parse.
const BundleVersion = 1

// ErrBundle wraps every failure to read a stored bundle, so a caller can branch on the
// class without matching prose.
var ErrBundle = errors.New("score: the stored scorecard could not be read")

// ErrReplayMismatch reports that re-scoring a bundle's own evidence did not reproduce
// the bundle's own cards. It means the stored verdict and this build's verdict function
// disagree about the same facts, which is either a regression here or a bundle written
// by a build whose rules differed.
var ErrReplayMismatch = errors.New("score: re-scoring the stored evidence did not reproduce the stored verdict")

// Window is a recorded chaos quarantine period, aliased from the package that owns it.
//
// It is an alias rather than a copy because the semantics that matter here —
// [trust.Window.Active], and the ceiling that bounds a window nothing ever closed — are
// the ones the trust ledger enforces, and a scorer holding a second definition of
// "active" would eventually disagree with the ledger about whether an outcome was
// admissible. One definition, two readers.
type Window = trust.Window

// Fact is one audit record projected onto the fields a verdict is derived from.
//
// Every field is read by the scorer. See the file doc for why nothing else is here.
type Fact struct {
	// Seq is the record's position in the trail. It orders the facts for one action, and
	// ordering is what identifies the terminal record the fix verdict is read from.
	Seq int `json:"seq"`

	// Identity, Cluster and Operation name the action.
	Identity  remediate.ProposalIdentity `json:"identity"`
	Cluster   string                     `json:"cluster"`
	Operation remediate.Operation        `json:"operation"`

	// TargetCluster is the cluster the target object lives in. It is carried separately
	// from Cluster precisely so a disagreement between them is visible — that is what
	// the audit layer duplicates it for.
	TargetCluster string `json:"targetCluster"`

	// TargetNamespace is the target's namespace, empty for a cluster-scoped object. The
	// empty case is load-bearing: it is what makes a target unbounded by the namespace
	// dimension autonomy is granted along.
	TargetNamespace string `json:"targetNamespace"`

	// Reversibility is the recorded reversibility token.
	Reversibility string `json:"reversibility"`

	// Authority is the recorded authority token — see [audit.Authority]. It is stored as
	// the token rather than the enum so a bundle is readable by a person and stays
	// meaningful if the enum's numbering ever changes.
	Authority string `json:"authority"`

	// Approver is who or what authorized the action: a login, or a policy identity under
	// [autonomy.PolicyPrefix].
	Approver string `json:"approver,omitempty"`

	// Ref is the artifact the authorization points at — an approval issue for a human
	// grant, a disclosure issue for an unattended one.
	Ref string `json:"ref,omitempty"`

	// Sent, Applied and DryRun are what the request actually was: whether it left the
	// process, whether the cluster changed, and whether it was a server-side preview.
	Sent    bool `json:"sent"`
	Applied bool `json:"applied"`
	DryRun  bool `json:"dryRun"`

	// CleanAbort reports that the terminating failure was the expected answer to a stale
	// approval. It is carried as the recorded fact rather than re-derived from a failure
	// token, so this package does not need a second opinion on which classes qualify.
	CleanAbort bool `json:"cleanAbort"`

	// RollbackAttempted reports that this record describes a rollback rather than the
	// original action, which is what keeps "the undo worked" from reading as "the fix
	// worked".
	RollbackAttempted bool `json:"rollbackAttempted"`

	// Convergence is the recorded observation verdict token.
	Convergence string `json:"convergence,omitempty"`

	// StartedAt and FinishedAt bound the attempt including its observation window. They
	// are what a recorded chaos window is compared against.
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// CitesNothing reports that this record's authorization points at nothing a reader could
// follow: no authorizing identity, an identity that is the bare policy prefix with no
// rule after it, or no artifact.
//
// It is only ever asked about an unattended action, where it is decisive. Nobody was
// asked, so the citation IS the oversight record, and [approve.GrantAutonomous] refuses
// to mint a slip without one — a record that has one anyway did not come from there.
func (f Fact) CitesNothing() bool {
	identity := strings.TrimSpace(f.Approver)
	return identity == "" || identity == autonomy.PolicyPrefix || strings.TrimSpace(f.Ref) == ""
}

// FactFrom projects one audit record.
func FactFrom(rec audit.Record) Fact {
	return Fact{
		Seq:               rec.Seq,
		Identity:          rec.Action.Identity,
		Cluster:           rec.Action.Cluster,
		Operation:         rec.Action.Operation,
		TargetCluster:     rec.Action.Target.Cluster,
		TargetNamespace:   rec.Action.Target.Namespace,
		Reversibility:     rec.Action.Reversibility.String(),
		Authority:         rec.Approver.Authority.String(),
		Approver:          rec.Approver.Identity,
		Ref:               rec.Approver.Ref,
		Sent:              rec.Change.Sent,
		Applied:           rec.Change.Applied,
		DryRun:            rec.Change.DryRun,
		CleanAbort:        rec.Outcome.CleanAbort,
		RollbackAttempted: rec.Rollback.Attempted,
		Convergence:       rec.Outcome.Convergence,
		StartedAt:         rec.Change.StartedAt,
		FinishedAt:        rec.Change.FinishedAt,
	}
}

// FactsFrom projects a trail's records in the order given.
func FactsFrom(recs []audit.Record) []Fact {
	facts := make([]Fact, 0, len(recs))
	for _, rec := range recs {
		facts = append(facts, FactFrom(rec))
	}
	return facts
}

// Evidence is everything a verdict may be derived from: the projected records, and the
// recorded quarantine windows they are read against.
//
// The windows are part of the evidence rather than a parameter to scoring because they
// are part of the record. "Was the ledger quarantined when this happened?" has to be
// answerable from the stored artifact alone by someone with no memory of the run, which
// is exactly the cost the milestone's quarantine decision accepted as binding.
type Evidence struct {
	Facts   []Fact   `json:"facts"`
	Windows []Window `json:"windows,omitempty"`
}

// EvidenceFrom assembles evidence from a trail's records and a window log's windows.
func EvidenceFrom(recs []audit.Record, windows []Window) Evidence {
	return Evidence{Facts: FactsFrom(recs), Windows: append([]Window(nil), windows...)}
}

// Bundle is a scorecard as stored: the evidence, and the verdicts derived from it.
//
// Both halves are kept, and that is the whole point of the type. Storing only the cards
// would leave a verdict nobody could check; storing only the evidence would leave every
// reader to re-derive it and hope their build agrees. Holding both makes the disagreement
// detectable — see [Replay].
type Bundle struct {
	// Version is [BundleVersion] as of the build that wrote this bundle.
	Version int `json:"version"`

	// Evidence is what the verdicts were derived from.
	Evidence Evidence `json:"evidence"`

	// Cards are the verdicts, one per action, in the order [Cards] produced them.
	Cards []Card `json:"cards"`
}

// NewBundle scores the evidence and returns the bundle holding both.
func NewBundle(ev Evidence) Bundle {
	return Bundle{Version: BundleVersion, Evidence: ev, Cards: Cards(ev)}
}

// Replay re-derives the verdicts from the bundle's own stored evidence and checks them
// against the bundle's own stored cards.
//
// This is task T6's third criterion in one call: a verdict that can be recomputed from
// stored records, after the fact, without re-running the scenario. It returns the
// re-derived cards so a caller can use them, and [ErrReplayMismatch] naming the first
// disagreement if this build reaches a different verdict than the one stored.
//
// A mismatch is not necessarily a bug in either side — a build that adds a bar will
// legitimately grade an old bundle worse than it was graded at the time. It is always
// something a person has to look at, which is why it is an error rather than a
// silently-updated card.
func Replay(b Bundle) ([]Card, error) {
	if b.Version != BundleVersion {
		return nil, fmt.Errorf("%w: bundle version %d, this build writes %d", ErrBundle, b.Version, BundleVersion)
	}
	rescored := Cards(b.Evidence)
	if len(rescored) != len(b.Cards) {
		return rescored, fmt.Errorf("%w: the stored bundle holds %d card(s) and its evidence produces %d",
			ErrReplayMismatch, len(b.Cards), len(rescored))
	}
	for i, want := range b.Cards {
		if !rescored[i].Equal(want) {
			return rescored, fmt.Errorf("%w: card %d stored as %q, re-derived as %q",
				ErrReplayMismatch, i, want.String(), rescored[i].String())
		}
	}
	return rescored, nil
}

// Write encodes a bundle as indented JSON with a trailing newline.
//
// Indented rather than compact: the artifact's whole purpose is to be read later by a
// person reconstructing an incident, and a scorecard nobody can read in a terminal is
// one they will not consult.
func Write(w io.Writer, b Bundle) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(b); err != nil {
		return fmt.Errorf("score: writing the scorecard: %w", err)
	}
	return nil
}

// Read decodes a bundle and refuses one this build does not understand.
//
// Unknown fields are rejected. A bundle written by a build that recorded a field this
// one drops is a bundle whose verdict was derived from evidence this build cannot see,
// and scoring it anyway would produce a confident answer from partial input.
func Read(r io.Reader) (Bundle, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	if b.Version != BundleVersion {
		return Bundle{}, fmt.Errorf("%w: bundle version %d, this build reads %d", ErrBundle, b.Version, BundleVersion)
	}
	return b, nil
}

// WriteFile stores a bundle at path, creating parent directories as needed.
//
// The file is written 0o600 and the directories 0o755, matching the durable stores in
// [trust] and [budget]. A scorecard holds no credential — nothing free-text reaches it —
// but it is an operational record of what MaKlaude was permitted to do, and matching the
// neighbours means one answer to "who can read MaKlaude's state" rather than three.
func WriteFile(path string, b Bundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("score: creating the scorecard directory: %w", err)
	}
	var buf strings.Builder
	if err := Write(&buf, b); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		return fmt.Errorf("score: writing the scorecard: %w", err)
	}
	return nil
}

// ReadFile loads a bundle from path.
func ReadFile(path string) (Bundle, error) {
	f, err := os.Open(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	defer f.Close()
	return Read(f)
}

// The three enums below travel as their stable tokens rather than as integers.
//
// A stored verdict outlives the build that wrote it, and an integer in a file is a
// promise that nobody ever reorders a const block — the kind of promise this repo has
// learned not to make. The token form is also the readable one, so `"grade":
// "over-permitted"` needs no decoder ring.
//
// Each unmarshaller refuses a token it does not recognize rather than falling back to a
// zero value, for the reason [audit.ParsePhase] gives: the zero values here are safe to
// render and unsafe to substitute silently. A bundle whose grade could not be read must
// say so, not read as unassessable.

// MarshalJSON writes the fix verdict as its token.
func (f Fix) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

// UnmarshalJSON reads a fix verdict token, refusing an unrecognized one.
func (f *Fix) UnmarshalJSON(data []byte) error {
	return unmarshalToken(data, "fix verdict", ParseFix, f)
}

// MarshalJSON writes the fault as its token.
func (f Fault) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

// UnmarshalJSON reads a fault token, refusing an unrecognized one.
func (f *Fault) UnmarshalJSON(data []byte) error {
	return unmarshalToken(data, "fault", ParseFault, f)
}

// MarshalJSON writes the grade as its token.
func (g Grade) MarshalJSON() ([]byte, error) { return json.Marshal(g.String()) }

// UnmarshalJSON reads a grade token, refusing an unrecognized one.
func (g *Grade) UnmarshalJSON(data []byte) error {
	return unmarshalToken(data, "grade", ParseGrade, g)
}

// unmarshalToken decodes a JSON string and runs it through the enum's own parser, so
// there is one parser per enum rather than one per direction.
func unmarshalToken[T any](data []byte, what string, parse func(string) (T, bool), out *T) error {
	var token string
	if err := json.Unmarshal(data, &token); err != nil {
		return fmt.Errorf("%w: reading a %s: %w", ErrBundle, what, err)
	}
	parsed, ok := parse(token)
	if !ok {
		return fmt.Errorf("%w: %s %q is not one this build recognizes", ErrBundle, what, token)
	}
	*out = parsed
	return nil
}
