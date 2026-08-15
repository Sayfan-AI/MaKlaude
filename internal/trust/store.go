package trust

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/autonomy"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The on-disk format is newline-delimited JSON, one [Entry] per line, appended and
// never rewritten in place.
//
// Three properties drove the choice, in this order.
//
// APPEND-ONLY MATCHES WHAT THE DATA IS. Every entry describes something that already
// happened; nothing in the promotion arithmetic ever edits history, and a format
// with no update operation cannot be used to edit it by accident. It is the same
// discipline [audit.Trail] applies in memory, for the same reason.
//
// A PARTIAL LINE IS DETECTABLE. A crash mid-append leaves a truncated final line
// rather than a structurally broken file, and JSON parsing rejects it — see [load],
// which fails rather than skipping. A format where a torn write silently yields a
// plausible record would be the worst outcome here: the file's whole job is to be
// the evidence behind an unattended cluster mutation.
//
// AN OPERATOR CAN READ IT. This file is what a human opens when they want to know
// why MaKlaude thinks it may act on its own. That has to be answerable with `cat`.
//
// The wire form uses the enums' stable string tokens rather than their integer
// values, so a ledger written by one build and read by another cannot be
// reinterpreted by an `iota` that shifted when a constant was inserted.

// wireEntry is the on-disk shape of an [Entry]. It exists so the file format is a
// deliberate, reviewable thing rather than whatever the struct fields happen to be
// called this month, and so the enums travel as tokens.
//
// The fingerprint is `omitempty` and its absence is meaningful rather than merely
// tolerated: a line written before issue #167 has no such key, decodes to the empty
// fingerprint, and thereby promotes nothing while still counting toward its shape's
// failures. That is the intended reading of a history whose fixes are no longer
// identifiable, so the format needs no version bump and no migration — see
// [Entry.Fingerprint].
type wireEntry struct {
	Key         string    `json:"key"`
	Identity    string    `json:"identity,omitempty"`
	Cluster     string    `json:"cluster"`
	Operation   string    `json:"operation"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Authority   string    `json:"authority"`
	Outcome     string    `json:"outcome"`
	At          time.Time `json:"at"`
	Ref         string    `json:"ref,omitempty"`
}

// marshal renders one entry as a single JSON line, newline included.
func marshal(e Entry) ([]byte, error) {
	line, err := json.Marshal(wireEntry{
		Key:         e.Key,
		Identity:    string(e.Identity),
		Cluster:     e.Shape.Cluster,
		Operation:   string(e.Shape.Operation),
		Fingerprint: string(e.Fingerprint),
		Authority:   e.Authority.String(),
		Outcome:     e.Outcome.String(),
		At:          e.At.UTC(),
		Ref:         e.Ref,
	})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// unmarshal parses one JSON line back into an [Entry].
//
// An unrecognized authority or outcome token is an error, not a fallback to the
// zero value. Both zero values are safe in the arithmetic — unattributed cannot
// promote, unrecorded demotes — but silently coercing an unreadable file into safe
// values would mean a ledger written by a newer build reads here as a history of
// failures, and the operator would see a shape mysteriously demoted rather than a
// message saying this binary cannot read the file.
func unmarshal(line []byte) (Entry, error) {
	var w wireEntry
	if err := json.Unmarshal(line, &w); err != nil {
		return Entry{}, err
	}

	authority, ok := parseAuthority(w.Authority)
	if !ok {
		return Entry{}, fmt.Errorf("unknown authority %q", w.Authority)
	}
	outcome, ok := parseOutcome(w.Outcome)
	if !ok {
		return Entry{}, fmt.Errorf("unknown outcome %q", w.Outcome)
	}

	return Entry{
		Key:         w.Key,
		Identity:    remediate.ProposalIdentity(w.Identity),
		Shape:       autonomy.Shape{Cluster: w.Cluster, Operation: remediate.Operation(w.Operation)},
		Fingerprint: remediate.Fingerprint(w.Fingerprint),
		Authority:   authority,
		Outcome:     outcome,
		At:          w.At,
		Ref:         w.Ref,
	}, nil
}

// store is the durable backing for a [Ledger]: a file it appends lines to and can
// atomically replace.
//
// It holds no open handle between calls. Each append opens, writes, syncs and
// closes, which costs a syscall or three per recorded execution — a rate bounded by
// how often MaKlaude mutates a cluster, so a handful per hour at most — and buys
// back the case that actually matters: a long-lived process holding a dirty buffer
// when the node it runs on goes away. The evidence behind an unattended mutation
// should be on disk before the mutation's own record is, not whenever a buffer
// happened to flush.
type store struct{ path string }

// Open loads the ledger at path, creating it if it does not exist, and returns a
// [Ledger] backed by it.
//
// A file that cannot be read or parsed is an ERROR rather than an empty ledger. An
// empty ledger would be the safe-looking failure — nothing trusted, everything gates
// — and it is the wrong one, because it is indistinguishable from a fresh install
// and would quietly mean "MaKlaude's history is being silently discarded on every
// start" for as long as nobody noticed. Failing loudly is what makes the difference
// visible; the caller can still choose to proceed with [NewMemory], but it has to
// choose.
func Open(path string) (*Ledger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("trust: no ledger path was given")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("trust: preparing ledger directory: %w", err)
	}

	entries, err := load(path)
	if err != nil {
		return nil, err
	}

	l := NewMemory()
	l.store = &store{path: path}
	for _, e := range entries {
		if err := validate(e); err != nil {
			return nil, fmt.Errorf("trust: ledger %s: %w", path, err)
		}
		if _, dup := l.keys[e.Key]; dup {
			// A duplicate key on disk is history recorded twice, not two executions. It
			// collapses rather than double-counting toward promotion, which is the same
			// resolution Record and Rebuild reach for the same reason.
			continue
		}
		l.insert(e)
	}
	return l, nil
}

// Path reports the ledger file backing this ledger, empty for an in-memory one. It
// exists so an escalation or a status report can tell an operator which file to
// look at, rather than making them infer it from configuration.
func (l *Ledger) Path() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.store == nil {
		return ""
	}
	return l.store.path
}

// load reads every entry from the file. A missing file is an empty history, which is
// the correct reading of "this cluster has never been remediated".
func load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: opening ledger %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	// A ledger line is a handful of short fields; the default 64KiB token limit is
	// already generous, and raising it would only make a corrupt file take longer to
	// reject.
	for line := 1; scanner.Scan(); line++ {
		raw := scanner.Bytes()
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		e, err := unmarshal(raw)
		if err != nil {
			return nil, fmt.Errorf("trust: ledger %s line %d: %w", path, line, err)
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("trust: reading ledger %s: %w", path, err)
	}
	return entries, nil
}

// append writes one entry to the end of the file and flushes it to stable storage.
func (s *store) append(e Entry) error {
	line, err := marshal(e)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// replace atomically rewrites the file to hold exactly these entries.
//
// It writes a sibling temporary file and renames it over the target, so a crash
// leaves either the old ledger or the new one and never a half-rebuilt mixture. A
// rebuild is the recovery path — the thing an operator reaches for when the ledger
// is already suspect — and a recovery that can itself produce a corrupt file is not
// one.
func (s *store) replace(entries []Entry) error {
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".rebuild-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup for every path that does not reach the rename. Removing a
	// name that rename already consumed fails harmlessly.
	defer func() { _ = os.Remove(tmpName) }()

	w := bufio.NewWriter(tmp)
	for _, e := range entries {
		line, err := marshal(e)
		if err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := w.Write(line); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
