package budget

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The on-disk format is a single JSON object, rewritten atomically on every change.
//
// It is deliberately NOT the append-only line format [trust] uses, and the difference
// is what the data is. A trust ledger is a HISTORY: every line describes something that
// happened, nothing is ever edited, and the arithmetic reads a window over the whole
// file. This is CURRENT STATE: a breaker is open or closed, a target was last acted on
// at one instant, and the only question ever asked of the file is "what is true now".
// Modelling mutable state as an append-only log would mean the answer depends on
// replaying every line in order, which is a lot of machinery to reconstruct three
// fields — and a truncated tail would silently produce a stale-but-plausible posture
// rather than an unreadable file.
//
// Rewriting atomically is what makes the failure mode detectable instead. A crash
// mid-save leaves either the whole previous state or the whole new one, and a file that
// is neither — hand-edited, truncated by a full disk, written by a build with a
// different schema — fails to parse and SEALS the budget. See the package doc: a state
// file that cannot be read must gate everything, so "unparseable" has to be a state the
// code can reach and report, never one it papers over.

// stateVersion is the schema version written into every file.
//
// An unrecognized version seals rather than being ignored, in either direction. A newer
// MaKlaude may record a bound this build knows nothing about, and silently honoring
// only the fields this build understands would apply a ceiling weaker than the one the
// operator configured — which is precisely the unbounded failure this file exists to
// prevent.
const stateVersion = 1

// state is the persisted posture: one entry per cluster that has any.
type state struct {
	Version  int                      `json:"version"`
	Clusters map[string]*clusterState `json:"clusters"`
}

// clusterState is one cluster's breaker and cooldowns.
type clusterState struct {
	// Tripped, TrippedAt and TrippedDetail are the breaker. It stays open across
	// process restarts, which is the whole reason this file exists: the condition that
	// tripped it is a cluster MaKlaude is wrong about, and that outlives a process.
	Tripped       bool      `json:"tripped"`
	TrippedAt     time.Time `json:"trippedAt,omitzero"`
	TrippedDetail string    `json:"trippedDetail,omitempty"`

	// ConsecutiveFailures is the current run of auto-apply failures. Reset by a
	// success and by [Budget.Clear].
	ConsecutiveFailures int `json:"consecutiveFailures"`

	// ClearedAt and ClearedBy record the last human who closed the breaker. They are
	// kept after the clear rather than discarded, because "who re-authorized autonomy
	// on this cluster, and when" is exactly what an incident review asks next.
	ClearedAt time.Time `json:"clearedAt,omitzero"`
	ClearedBy string    `json:"clearedBy,omitempty"`

	// LastAdmitted maps a target's compact key to the instant an auto-apply was last
	// admitted for it. Entries older than the cooldown are pruned on save — see
	// [state.prune] — so the file stays proportional to recent activity rather than to
	// the lifetime of the deployment.
	LastAdmitted map[string]time.Time `json:"lastAdmitted,omitempty"`
}

// newState builds an empty state.
func newState() state { return state{Version: stateVersion, Clusters: map[string]*clusterState{}} }

// cluster returns the named cluster's state, creating it on first use. The caller must
// hold the budget's mutex.
func (s *state) cluster(name string) *clusterState {
	if s.Clusters == nil {
		s.Clusters = map[string]*clusterState{}
	}
	cs, ok := s.Clusters[name]
	if !ok {
		cs = &clusterState{LastAdmitted: map[string]time.Time{}}
		s.Clusters[name] = cs
	}
	if cs.LastAdmitted == nil {
		cs.LastAdmitted = map[string]time.Time{}
	}
	return cs
}

// prune drops cooldown entries that have already expired, and cluster entries that
// carry nothing worth keeping.
//
// An expired cooldown admits anyway, so dropping it changes no decision — it only stops
// the file growing one entry per object ever touched. A cluster is kept as long as its
// breaker is open, its failure count is non-zero, it has a live cooldown, or a human has
// cleared it: the last of those is history rather than state, and losing it would erase
// the attribution [Budget.Clear] exists to record.
func (s *state) prune(now time.Time, cooldown time.Duration) {
	if cooldown <= 0 {
		return
	}
	for name, cs := range s.Clusters {
		for key, at := range cs.LastAdmitted {
			if now.Sub(at.UTC()) >= cooldown {
				delete(cs.LastAdmitted, key)
			}
		}
		if !cs.Tripped && cs.ConsecutiveFailures == 0 && len(cs.LastAdmitted) == 0 && cs.ClearedBy == "" {
			delete(s.Clusters, name)
		}
	}
}

// store is the durable backing for a [Budget].
//
// Like [trust]'s store it holds no open handle between calls. The write rate is bounded
// by how often MaKlaude acts on a cluster — a handful per hour at most — so paying a
// create-write-sync-rename per change costs nothing measurable and buys the property
// that matters: the ceiling is on disk before the action it bounds runs, not whenever a
// buffer happened to flush.
type store struct{ path string }

// newStore prepares the directory for the state file. It does not create the file: a
// budget that has recorded nothing has nothing to say, and a missing file is correctly
// read as an empty state.
func newStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("budget: preparing the state directory: %w", err)
	}
	return &store{path: path}, nil
}

// load reads the state file.
//
// A missing file is an empty state and not an error — that is a fresh install, where
// no breaker has tripped and no target has been acted on. Everything else is an error,
// and [Open] turns it into a seal.
func (s *store) load() (state, error) {
	f, err := os.Open(s.path)
	if os.IsNotExist(err) {
		return newState(), nil
	}
	if err != nil {
		return newState(), fmt.Errorf("the state file %s could not be opened: %w", s.path, err)
	}
	defer func() { _ = f.Close() }()

	var loaded state
	dec := json.NewDecoder(bufio.NewReader(f))
	// An unknown field means the file was written by a build that records something
	// this one does not model. Rejecting it is the fail-closed reading; see
	// [stateVersion].
	dec.DisallowUnknownFields()
	if err := dec.Decode(&loaded); err != nil {
		return newState(), fmt.Errorf("the state file %s could not be parsed: %w", s.path, err)
	}
	if loaded.Version != stateVersion {
		return newState(), fmt.Errorf("the state file %s is version %d and this build reads version %d",
			s.path, loaded.Version, stateVersion)
	}
	if loaded.Clusters == nil {
		loaded.Clusters = map[string]*clusterState{}
	}
	for _, cs := range loaded.Clusters {
		if cs == nil {
			return newState(), fmt.Errorf("the state file %s contains a null cluster entry", s.path)
		}
		if cs.LastAdmitted == nil {
			cs.LastAdmitted = map[string]time.Time{}
		}
	}
	return loaded, nil
}

// save rewrites the file to hold exactly this state.
//
// It writes a sibling temporary file and renames it over the target, so a crash leaves
// either the old state or the new one. The mode is 0600: the file records which
// clusters MaKlaude is allowed to act on unattended, which is not a secret but is not
// something to leave world-readable either.
func (s *store) save(st state) error {
	st.Version = stateVersion

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".save-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup for every path that does not reach the rename. Removing a
	// name rename already consumed fails harmlessly.
	defer func() { _ = os.Remove(tmpName) }()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
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
