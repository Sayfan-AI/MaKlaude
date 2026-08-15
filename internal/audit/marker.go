package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// This file is what makes an artifact readable by a machine as well as by a person, and
// it exists because of a dependency the trust ledger shipped without.
//
// # The half of T3 that was waiting for this
//
// A trust ledger is documented as a CACHE of the artifacts rather than as the authority
// over them, and it can be replaced wholesale from a supplied entry set. What was missing
// was the reader: a lifecycle projects onto an entry, but nothing could turn an artifact
// back into a lifecycle, because [Lifecycle] renders prose for a person and prose does not
// round-trip.
//
// Parsing the rendered form was the obvious next step and is the wrong one. It would
// couple the ledger's correctness to the wording of a markdown table, so a renderer change
// nobody thought was risky would silently start dropping history — and dropping history is
// only safe in one direction. A lost failure re-grants autonomy; a lost approval merely
// delays it. So the artifact carries the facts separately, in a hidden marker, and the
// rendering stays free to change.
//
// # Why the marker lives HERE and not on either trail
//
// It was written for the disclosure trail, which is where the unattended actions are. That
// placement was wrong the moment the rebuild had to cover the GATED path too, and the T3
// carry-over says why in one sentence: the promotion arithmetic counts HUMAN-APPROVED
// executions, so an artifact format that only marks up autonomous actions makes the
// evidence FOR autonomy the one thing that cannot be rebuilt.
//
// Both trails must therefore write the same marker, and they cannot share one owned by
// either: `disclose` imports `approve` (it records against the same permission slip), so
// `approve` importing `disclose` is a cycle. The marker is a serialization of [Record], the
// package that owns [Record] is imported by both, and so this is the one home that does not
// force one trail to depend on the other.
//
// # It carries a projection, not the records
//
// The marker holds exactly the fields a trust entry is derived from, and the reader
// reconstructs [Record] values carrying those fields and nothing else. That is narrower
// than serializing whole records, deliberately: everything omitted is either free text that
// redaction already touched (convergence detail, error text, pre-state values) or
// navigational (titles, scopes, attempt counts), and none of it can change which entry the
// lifecycle projects to. Keeping the marker to the deciding fields means a world-readable
// artifact carries no cluster-derived free text in its hidden part, and it means the
// ledger's arithmetic stays owned by the ledger rather than being re-derived here.
//
// # Why JSON is safe inside an HTML comment
//
// [encoding/json.Marshal] escapes `<`, `>` and `&` to their \u form by default, so a
// marshalled payload cannot contain a literal `>` and therefore cannot contain the `-->`
// that would truncate the comment. That default is doing real work here rather than being
// incidental, so it is asserted by test with a payload that contains `-->` in a string
// field.

// The hidden HTML-comment marker carrying a finished action's lifecycle. Both the approval
// trail and the disclosure trail embed this exact marker, which is what lets one reader
// walk both and reproduce the ledger.
//
// Its ABSENCE means the action has not finished — it is written once, when the lifecycle is
// complete — so a rebuild must distinguish absence from corruption. See [ErrNoMarker].
const (
	lifecycleMarkerPrefix = "<!-- maklaude:lifecycle="
	lifecycleMarkerSuffix = " -->"
)

// lifecycleVersion is the marker payload's schema version. It is written and checked so
// that a build reading a marker it does not understand REFUSES it rather than parsing the
// fields it recognizes — a rebuild that silently produces a shorter history than the truth
// is the dangerous direction, because a lost failure re-grants autonomy.
const lifecycleVersion = 1

// ErrNoMarker reports that a body carries no lifecycle marker at all. It is separated from
// a parse failure because the two mean different things to a rebuild: absence is the
// ordinary state of an action still in flight, while a malformed marker is history that
// exists and cannot be read, and only the second is a reason to stop.
var ErrNoMarker = errors.New("audit: the body carries no lifecycle marker")

// wireRecord is one audit record reduced to the fields that decide a trust entry. See the
// file comment on why it is a projection rather than the record.
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
// identity, cluster and operation are hoisted out of the records because this package
// duplicates them onto every record and repeating them here would let a malformed payload
// disagree with itself about which action it describes.
//
// The fingerprint is hoisted for the same reason and carried with `omitempty`, because
// a marker written before it existed simply has no such key. Absent decodes to the
// empty fingerprint, which is exactly the right reading — see
// [trust.Entry.Fingerprint] — so an older artifact rebuilds into a history that still
// counts its failures and can promote nothing.
type wireLifecycle struct {
	Version     int          `json:"v"`
	Identity    string       `json:"identity"`
	Cluster     string       `json:"cluster"`
	Operation   string       `json:"operation"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	Records     []wireRecord `json:"records"`
}

// LifecycleMarker renders the hidden marker carrying the lifecycle a rebuild reads.
//
// It refuses a lifecycle whose records disagree about which action they describe. That is a
// programming error rather than a data condition — this package copies the action onto
// every record precisely so a disagreement is visible — and writing it out anyway would put
// an artifact on a public trail attributing one cluster's mutation to another's history.
func LifecycleMarker(recs []Record) (string, error) {
	if len(recs) == 0 {
		return "", errors.New("audit: refusing to mark an empty lifecycle")
	}
	head := recs[0].Action
	if head.Identity == "" || head.Cluster == "" || head.Operation == "" {
		return "", errors.New("audit: the lifecycle's first record names no identified action")
	}

	wire := wireLifecycle{
		Version:     lifecycleVersion,
		Identity:    string(head.Identity),
		Cluster:     head.Cluster,
		Operation:   string(head.Operation),
		Fingerprint: string(head.Fingerprint),
		Records:     make([]wireRecord, 0, len(recs)),
	}
	for i, rec := range recs {
		if rec.Action.Identity != head.Identity || rec.Action.Cluster != head.Cluster || rec.Action.Operation != head.Operation {
			return "", fmt.Errorf("audit: record %d describes %s/%s on %q, the lifecycle describes %s/%s on %q",
				i, rec.Action.Cluster, rec.Action.Operation, rec.Action.Identity,
				head.Cluster, head.Operation, head.Identity)
		}
		// Checked separately from the three above so the message can say what actually
		// went wrong. Two records of one lifecycle disagreeing about the fingerprint means
		// the action was re-derived mid-flight from a proposal that had changed, and
		// hoisting either answer would file the whole lifecycle under a fix that only half
		// of it was.
		if rec.Action.Fingerprint != head.Fingerprint {
			return "", fmt.Errorf("audit: record %d carries fingerprint %q, the lifecycle carries %q",
				i, rec.Action.Fingerprint, head.Fingerprint)
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
		return "", fmt.Errorf("audit: encoding the lifecycle marker: %w", err)
	}
	return lifecycleMarkerPrefix + string(payload) + lifecycleMarkerSuffix, nil
}

// ParseLifecycleMarker reconstructs an action's audit lifecycle from an artifact body, in
// the order it was written, ready to be projected onto a trust entry.
//
// It returns [ErrNoMarker] when the body carries none, and a real error when it carries one
// that cannot be read. A caller rebuilding a ledger must treat those differently: the first
// is an action still in flight, the second is history it is about to lose. Failing loudly
// on the unreadable case is what a ledger already does with a corrupt line, and for the
// same reason.
func ParseLifecycleMarker(body string) ([]Record, error) {
	raw, ok := betweenMarkers(body, lifecycleMarkerPrefix, lifecycleMarkerSuffix)
	if !ok {
		return nil, ErrNoMarker
	}

	var wire wireLifecycle
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, fmt.Errorf("audit: decoding the lifecycle marker: %w", err)
	}
	switch {
	case wire.Version != lifecycleVersion:
		return nil, fmt.Errorf("audit: the lifecycle marker is version %d and this build reads version %d",
			wire.Version, lifecycleVersion)
	case wire.Identity == "" || wire.Cluster == "" || wire.Operation == "":
		return nil, errors.New("audit: the lifecycle marker names no identified action")
	case len(wire.Records) == 0:
		return nil, errors.New("audit: the lifecycle marker carries no records")
	}

	action := Action{
		Identity:    remediate.ProposalIdentity(wire.Identity),
		Cluster:     wire.Cluster,
		Operation:   remediate.Operation(wire.Operation),
		Fingerprint: remediate.Fingerprint(wire.Fingerprint),
	}
	recs := make([]Record, 0, len(wire.Records))
	for i, w := range wire.Records {
		phase, ok := ParsePhase(w.Phase)
		if !ok {
			return nil, fmt.Errorf("audit: record %d carries an unreadable phase %q", i, w.Phase)
		}
		authority, ok := ParseAuthority(w.Authority)
		if !ok {
			return nil, fmt.Errorf("audit: record %d carries an unreadable authority %q", i, w.Authority)
		}
		recs = append(recs, Record{
			Seq:        i + 1,
			RecordedAt: w.RecordedAt,
			Phase:      phase,
			Action:     action,
			Approver:   Approver{Authority: authority, Ref: w.Ref},
			Change:     Change{DryRun: w.DryRun, FinishedAt: w.FinishedAt},
			Outcome: Outcome{
				Convergence: w.Convergence,
				Failure:     w.Failure,
				CleanAbort:  w.CleanAbort,
			},
			Rollback: Rollback{Attempted: w.RollbackAttempted},
		})
	}
	return recs, nil
}

// WithLifecycleMarker returns body carrying exactly one lifecycle marker: any marker
// already present is removed first.
//
// Replacing rather than appending is the only safe rule. A body is the artifact's single
// source of truth for the rebuild, and two markers in one body would make the reconstructed
// history depend on which one [betweenMarkers] happened to reach first — the trail would
// have two answers and no way to say which is current. The gated path in particular writes
// its marker onto a body that has been re-rendered several times, so "this can only happen
// once" is not a property the caller can promise.
func WithLifecycleMarker(body, marker string) string {
	body = stripMarker(body, lifecycleMarkerPrefix, lifecycleMarkerSuffix)
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + marker + "\n"
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

// stripMarker removes the first prefix/suffix marker, and one trailing newline with it, so
// a rewritten body never accumulates markers or gains a blank line per rewrite.
func stripMarker(body, prefix, suffix string) string {
	start := strings.Index(body, prefix)
	if start < 0 {
		return body
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return body
	}
	markerEnd := start + len(prefix) + end + len(suffix)
	if markerEnd < len(body) && body[markerEnd] == '\n' {
		markerEnd++
	}
	return body[:start] + body[markerEnd:]
}
