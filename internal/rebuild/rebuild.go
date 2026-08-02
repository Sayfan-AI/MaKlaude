// Package rebuild reconstructs the trust ledger from the comms artifacts, which is what
// makes the ledger a cache rather than the authority over MaKlaude's own autonomy.
//
// # What this closes
//
// The ledger was always documented as rebuildable, and half of that shipped: it can be
// replaced wholesale from a supplied entry set, and a lifecycle projects onto an entry.
// What was missing was the reader — nothing could turn an artifact back into a lifecycle,
// and nothing could enumerate the artifacts in the first place, because both trails list
// only OPEN artifacts and a finished action's artifact is closed.
//
// Both halves are now here: [audit.ParseLifecycleMarker] reads a body, and each trail's
// concrete sink can enumerate its full history. This package is the only thing that reads
// BOTH trails, which is the whole reason it is its own package — the two are deliberately
// disjoint at the query so neither can act on the other's artifacts, and a rebuild is the
// one operation that legitimately spans them because trust is earned on one and spent on
// the other.
//
// # It must never produce a SHORTER history than the truth
//
// The asymmetry that governs every decision in this file: a lost failure re-grants
// autonomy, a lost approval merely delays it. So an artifact whose marker cannot be read
// is a hard error, never a skipped entry — the caller is told the history is incomplete
// and the ledger is left exactly as it was, rather than being replaced by a version that
// is missing the failures that would have re-gated a shape.
//
// The one thing that is skipped is an artifact carrying NO marker, which is an action
// still in flight. That is not history loss: an action that has not finished has not
// produced evidence yet, and it will carry a marker when it does.
package rebuild

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sayfan-AI/MaKlaude/internal/approve"
	"github.com/Sayfan-AI/MaKlaude/internal/audit"
	"github.com/Sayfan-AI/MaKlaude/internal/disclose"
	"github.com/Sayfan-AI/MaKlaude/internal/trust"
)

// Artifact is one comms artifact reduced to what a rebuild reads: which trail it came
// from and which artifact it is (both only so a failure can name it), and the body.
type Artifact struct {
	Trail string
	Ref   string
	Body  string
}

// Archive is a trail that can enumerate its whole history, closed artifacts included.
//
// It is satisfied by adapters over the concrete sinks rather than by the sinks directly,
// because neither trail's Sink interface carries an enumerate-everything method — see
// internal/approve/history.go for why that capability is kept off the gate's surface.
type Archive interface {
	// Trail names the archive in errors and in the report. It is a label for a human,
	// not a key.
	Trail() string

	// ListAll returns every artifact this trail has ever opened.
	ListAll(ctx context.Context) ([]Artifact, error)
}

// ApprovalHistory is the read capability [ApprovalArchive] needs, satisfied by
// *[approve.GitHubSink] and *[approve.MemorySink].
type ApprovalHistory interface {
	ListAll(ctx context.Context) ([]approve.ArchivedArtifact, error)
}

// DisclosureHistory is the read capability [DisclosureArchive] needs, satisfied by
// *[disclose.GitHubSink] and *[disclose.MemorySink].
type DisclosureHistory interface {
	ListAll(ctx context.Context) ([]disclose.ArchivedArtifact, error)
}

// Trail names for the two archives. They appear in errors and in [Report], so a person
// told that history could not be read also learns which trail it was on.
const (
	TrailApproval   = "approval"
	TrailDisclosure = "disclosure"
)

// ApprovalArchive adapts the approval trail to [Archive].
func ApprovalArchive(src ApprovalHistory) Archive { return approvalArchive{src: src} }

type approvalArchive struct{ src ApprovalHistory }

func (a approvalArchive) Trail() string { return TrailApproval }

func (a approvalArchive) ListAll(ctx context.Context) ([]Artifact, error) {
	raw, err := a.src.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0, len(raw))
	for _, r := range raw {
		out = append(out, Artifact{Trail: TrailApproval, Ref: string(r.Ref), Body: r.Body})
	}
	return out, nil
}

// DisclosureArchive adapts the disclosure trail to [Archive].
func DisclosureArchive(src DisclosureHistory) Archive { return disclosureArchive{src: src} }

type disclosureArchive struct{ src DisclosureHistory }

func (a disclosureArchive) Trail() string { return TrailDisclosure }

func (a disclosureArchive) ListAll(ctx context.Context) ([]Artifact, error) {
	raw, err := a.src.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0, len(raw))
	for _, r := range raw {
		out = append(out, Artifact{Trail: TrailDisclosure, Ref: string(r.Ref), Body: r.Body})
	}
	return out, nil
}

// Report is what a rebuild read, per trail and in total. It exists because "the ledger now
// has 12 entries" is not enough to tell a person whether the rebuild worked: 12 entries
// from 12 artifacts is a healthy trail, and 12 from 400 is a trail whose actions mostly
// never finished.
type Report struct {
	// Artifacts is how many artifacts were enumerated across every archive.
	Artifacts int

	// Finished is how many carried a readable lifecycle marker and became entries.
	Finished int

	// InFlight is how many carried no marker — actions that have not finished. It is
	// reported rather than ignored because a large number here on a quiet trail means
	// actions are starting and never reporting back, which is the one thing the
	// disclosure trail's applied label exists to make visible.
	InFlight int

	// ByTrail counts the artifacts enumerated per trail, keyed by [Archive.Trail].
	ByTrail map[string]int
}

// UnreadableError reports an artifact whose lifecycle marker exists and cannot be parsed.
//
// It is its own type, and it aborts the rebuild, because of the asymmetry in the package
// doc: skipping it would produce a ledger shorter than the truth, and a ledger shorter
// than the truth can re-grant autonomy a failure had taken away. The artifact is named so
// a person can go and look at it.
type UnreadableError struct {
	Trail string
	Ref   string
	Err   error
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("rebuild: %s artifact %s carries a lifecycle marker that cannot be read "+
		"(refusing to rebuild a history shorter than the truth): %v", e.Trail, e.Ref, e.Err)
}

func (e *UnreadableError) Unwrap() error { return e.Err }

// Entries reads every archive and projects each finished action onto a trust entry.
//
// It returns entries in the order the archives returned them, which is not the ledger's
// order and does not need to be: entries are ordered by their recorded instants when they
// are inserted, precisely so a rebuild does not depend on what order an API happened to
// page them in.
func Entries(ctx context.Context, archives ...Archive) ([]trust.Entry, Report, error) {
	report := Report{ByTrail: map[string]int{}}
	var entries []trust.Entry

	for _, archive := range archives {
		if archive == nil {
			continue
		}
		artifacts, err := archive.ListAll(ctx)
		if err != nil {
			return nil, report, fmt.Errorf("rebuild: enumerating the %s trail: %w", archive.Trail(), err)
		}
		report.ByTrail[archive.Trail()] += len(artifacts)
		report.Artifacts += len(artifacts)

		for _, a := range artifacts {
			recs, err := audit.ParseLifecycleMarker(a.Body)
			if errors.Is(err, audit.ErrNoMarker) {
				report.InFlight++
				continue
			}
			if err != nil {
				return nil, report, &UnreadableError{Trail: a.Trail, Ref: a.Ref, Err: err}
			}
			entry, err := trust.EntryFrom(recs)
			if err != nil {
				return nil, report, &UnreadableError{Trail: a.Trail, Ref: a.Ref, Err: err}
			}
			report.Finished++
			entries = append(entries, entry)
		}
	}
	return entries, report, nil
}

// Ledger replaces a ledger's entire history with what the archives say, atomically.
//
// The read happens first and the write only if every artifact was readable, so a trail
// with one corrupt body leaves the ledger untouched rather than half-replaced. That is the
// same all-or-nothing shape [trust.Ledger.Rebuild] already has on disk, extended across
// the read: a partial rebuild is exactly the shorter-than-the-truth history this package
// refuses to produce.
func Ledger(ctx context.Context, l *trust.Ledger, archives ...Archive) (Report, error) {
	if l == nil {
		return Report{}, errors.New("rebuild: no ledger to rebuild")
	}
	entries, report, err := Entries(ctx, archives...)
	if err != nil {
		return report, err
	}
	if err := l.Rebuild(entries); err != nil {
		return report, fmt.Errorf("rebuild: replacing the ledger: %w", err)
	}
	return report, nil
}
