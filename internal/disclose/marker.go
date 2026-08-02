package disclose

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file is what makes a disclosure artifact readable by a machine as well as by a
// person, and it exists because of a dependency the trust ledger shipped without.
//
// # The half of T3 that was waiting for this
//
// [trust.Ledger] is documented as a CACHE of the approval artifacts rather than as the
// authority over them, and [trust.Ledger.Rebuild] can replace it wholesale from a
// supplied entry set. What was missing was the reader: [trust.EntryFrom] projects a
// lifecycle onto an entry, but nothing could turn an artifact back into a lifecycle,
// because [audit.Lifecycle] renders prose for a person and prose does not round-trip.
//
// Parsing the rendered form was the obvious next step and is the wrong one. It would
// couple the ledger's correctness to the wording of a markdown table, so a renderer
// change nobody thought was risky would silently start dropping history — and dropping
// history is only safe in one direction. A lost failure re-grants autonomy; a lost
// approval merely delays it. So the artifact carries the facts separately, in a hidden
// marker, and the rendering stays free to change.
//
// # It carries a projection, not the records
//
// The marker holds exactly the fields [trust.EntryFrom] reads, and the reader
// reconstructs [audit.Record] values carrying those fields and nothing else. That is
// narrower than serializing whole records, deliberately: everything omitted is either
// free text that redaction already touched (convergence detail, error text, pre-state
// values) or navigational (titles, scopes, attempt counts), and none of it can change
// which entry the lifecycle projects to. Keeping the marker to the deciding fields
// means a world-readable artifact carries no cluster-derived free text in its hidden
// part, and it means the ledger's arithmetic stays owned by [trust] rather than being
// re-derived here.
//
// # Why JSON is safe inside an HTML comment
//
// [encoding/json.Marshal] escapes `<`, `>` and `&` to their \u form by default, so a
// marshalled payload cannot contain a literal `>` and therefore cannot contain the
// `-->` that would truncate the comment. That default is doing real work here rather
// than being incidental, so it is asserted by test with a payload that contains `-->`
// in a string field.

// Hidden HTML-comment markers embedded in a disclosure body. They are separate markers
// rather than one because they are written at different times and mean different things
// when absent: the proposal and shape markers exist from the instant the artifact is
// opened, and the lifecycle marker only appears once the action has finished. An
// artifact with the first two and not the third is an action that started and never
// reported — see [AppliedLabel].
const (
	proposalMarkerPrefix = "<!-- maklaude:autonomous-proposal="
	proposalMarkerSuffix = " -->"

	shapeMarkerPrefix = "<!-- maklaude:autonomous-shape="
	shapeMarkerSuffix = " -->"

	lifecycleMarkerPrefix = "<!-- maklaude:autonomous-lifecycle="
	lifecycleMarkerSuffix = " -->"
)

// lifecycleVersion is the marker payload's schema version. It is written and checked so
// that a build reading a marker it does not understand REFUSES it rather than parsing
// the fields it recognizes — a rebuild that silently produces a shorter history than
// the truth is the dangerous direction, because a lost failure re-grants autonomy.
const lifecycleVersion = 1

// ErrNoMarker reports that a body carries no lifecycle marker at all. It is separated
// from a parse failure because the two mean different things to a rebuild: absence is
// the ordinary state of an action still in flight, while a malformed marker is history
// that exists and cannot be read, and only the second is a reason to stop.
var ErrNoMarker = errors.New("disclose: the body carries no lifecycle marker")

// wireRecord is one audit record reduced to the fields that decide a trust entry. See
// the file comment on why it is a projection rather than the record.
type wireRecord struct {
	Phase             string    `json:"phase"`
	Authority         string    `json:"authority"`
	Ref               string    `json:"ref,omitempty"`
	Convergence       string    `json:"convergence,omitempty"`
	Failure           string    `json:"failure,omitempty"`
	CleanAbort        bool      `json:"cleanAbort,omitempty"`
	DryRun            bool      `json:"dryRun,omitempty"`
	RollbackAttempted bool      `json:"rollbackAttempted,omitempty"`
	FinishedAt        time.Time `json:"finishedAt,omitzero"`
	RecordedAt        time.Time `json:"recordedAt,omitzero"`
}

// wireLifecycle is one action's whole lifecycle as carried by the marker. The action's
// identity, cluster and operation are hoisted out of the records because the audit
// package duplicates them onto every record and repeating them here would let a
// malformed payload disagree with itself about which action it describes.
type wireLifecycle struct {
	Version   int          `json:"v"`
	Identity  string       `json:"identity"`
	Cluster   string       `json:"cluster"`
	Operation string       `json:"operation"`
	Records   []wireRecord `json:"records"`
}

// proposalMarker renders the hidden marker embedding a proposal identity.
func proposalMarker(id remediate.ProposalIdentity) string {
	return proposalMarkerPrefix + string(id) + proposalMarkerSuffix
}

// ParseProposalMarker extracts the embedded proposal identity from a disclosure body,
// ok=false when no well-formed marker is present. A sink uses it to tell its own
// artifacts from anything else that happens to carry [ManagedLabel].
func ParseProposalMarker(body string) (remediate.ProposalIdentity, bool) {
	raw, ok := betweenMarkers(body, proposalMarkerPrefix, proposalMarkerSuffix)
	if !ok || raw == "" {
		return "", false
	}
	return remediate.ProposalIdentity(raw), true
}

// shapeMarker renders the hidden marker embedding the shape whose autonomy this action
// ran under — the granularity [RevokedLabel] revokes at.
func shapeMarker(s autonomy.Shape) string {
	return shapeMarkerPrefix + s.String() + shapeMarkerSuffix
}

// ParseShapeMarker extracts the embedded [autonomy.Shape], ok=false when absent or
// malformed.
//
// It splits on the LAST separator rather than the first. [autonomy.Shape.String]
// renders "cluster/operation", the operation is a catalog token that never contains a
// separator, and a registered cluster name is not constrained not to — so splitting
// from the right is the reading that cannot mis-attribute an action to a cluster whose
// name happens to contain a slash.
func ParseShapeMarker(body string) (autonomy.Shape, bool) {
	raw, ok := betweenMarkers(body, shapeMarkerPrefix, shapeMarkerSuffix)
	if !ok {
		return autonomy.Shape{}, false
	}
	cut := strings.LastIndex(raw, "/")
	if cut <= 0 || cut == len(raw)-1 {
		return autonomy.Shape{}, false
	}
	return autonomy.Shape{
		Cluster:   raw[:cut],
		Operation: remediate.Operation(raw[cut+1:]),
	}, true
}

// LifecycleMarker renders the hidden marker carrying the lifecycle a rebuild reads.
//
// It refuses a lifecycle whose records disagree about which action they describe. That
// is a programming error rather than a data condition — the audit package copies the
// action onto every record precisely so a disagreement is visible — and writing it out
// anyway would put an artifact on a public trail attributing one cluster's mutation to
// another's history.
func LifecycleMarker(recs []audit.Record) (string, error) {
	if len(recs) == 0 {
		return "", errors.New("disclose: refusing to mark an empty lifecycle")
	}
	head := recs[0].Action
	if head.Identity == "" || head.Cluster == "" || head.Operation == "" {
		return "", errors.New("disclose: the lifecycle's first record names no identified action")
	}

	wire := wireLifecycle{
		Version:   lifecycleVersion,
		Identity:  string(head.Identity),
		Cluster:   head.Cluster,
		Operation: string(head.Operation),
		Records:   make([]wireRecord, 0, len(recs)),
	}
	for i, rec := range recs {
		if rec.Action.Identity != head.Identity || rec.Action.Cluster != head.Cluster || rec.Action.Operation != head.Operation {
			return "", fmt.Errorf("disclose: record %d describes %s/%s on %q, the lifecycle describes %s/%s on %q",
				i, rec.Action.Cluster, rec.Action.Operation, rec.Action.Identity,
				head.Cluster, head.Operation, head.Identity)
		}
		wire.Records = append(wire.Records, wireRecord{
			Phase:             rec.Phase.String(),
			Authority:         rec.Approver.Authority.String(),
			Ref:               rec.Approver.Ref,
			Convergence:       rec.Outcome.Convergence,
			Failure:           rec.Outcome.Failure,
			CleanAbort:        rec.Outcome.CleanAbort,
			DryRun:            rec.Change.DryRun,
			RollbackAttempted: rec.Rollback.Attempted,
			FinishedAt:        rec.Change.FinishedAt,
			RecordedAt:        rec.RecordedAt,
		})
	}

	payload, err := json.Marshal(wire)
	if err != nil {
		return "", fmt.Errorf("disclose: encoding the lifecycle marker: %w", err)
	}
	return lifecycleMarkerPrefix + string(payload) + lifecycleMarkerSuffix, nil
}

// ParseLifecycleMarker reconstructs an action's audit lifecycle from a disclosure body,
// in the order it was written, ready for [trust.EntryFrom].
//
// It returns [ErrNoMarker] when the body carries none, and a real error when it carries
// one that cannot be read. A caller rebuilding a ledger must treat those differently:
// the first is an action still in flight, the second is history it is about to lose.
// Failing loudly on the unreadable case is what [trust.Open] already does with a corrupt
// ledger line, and for the same reason.
func ParseLifecycleMarker(body string) ([]audit.Record, error) {
	raw, ok := betweenMarkers(body, lifecycleMarkerPrefix, lifecycleMarkerSuffix)
	if !ok {
		return nil, ErrNoMarker
	}

	var wire wireLifecycle
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, fmt.Errorf("disclose: decoding the lifecycle marker: %w", err)
	}
	switch {
	case wire.Version != lifecycleVersion:
		return nil, fmt.Errorf("disclose: the lifecycle marker is version %d and this build reads version %d",
			wire.Version, lifecycleVersion)
	case wire.Identity == "" || wire.Cluster == "" || wire.Operation == "":
		return nil, errors.New("disclose: the lifecycle marker names no identified action")
	case len(wire.Records) == 0:
		return nil, errors.New("disclose: the lifecycle marker carries no records")
	}

	action := audit.Action{
		Identity:  remediate.ProposalIdentity(wire.Identity),
		Cluster:   wire.Cluster,
		Operation: remediate.Operation(wire.Operation),
	}
	recs := make([]audit.Record, 0, len(wire.Records))
	for i, w := range wire.Records {
		phase, ok := audit.ParsePhase(w.Phase)
		if !ok {
			return nil, fmt.Errorf("disclose: record %d carries an unreadable phase %q", i, w.Phase)
		}
		authority, ok := audit.ParseAuthority(w.Authority)
		if !ok {
			return nil, fmt.Errorf("disclose: record %d carries an unreadable authority %q", i, w.Authority)
		}
		recs = append(recs, audit.Record{
			Seq:        i + 1,
			RecordedAt: w.RecordedAt,
			Phase:      phase,
			Action:     action,
			Approver:   audit.Approver{Authority: authority, Ref: w.Ref},
			Change:     audit.Change{DryRun: w.DryRun, FinishedAt: w.FinishedAt},
			Outcome: audit.Outcome{
				Convergence: w.Convergence,
				Failure:     w.Failure,
				CleanAbort:  w.CleanAbort,
			},
			Rollback: audit.Rollback{Attempted: w.RollbackAttempted},
		})
	}
	return recs, nil
}

// betweenMarkers extracts the text between a marker's prefix and the first suffix that
// follows it. ok is false when the prefix is absent or the marker is unterminated.
func betweenMarkers(body, prefix, suffix string) (string, bool) {
	start := strings.Index(body, prefix)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
