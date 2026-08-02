package budget

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statePath returns a path inside a per-test temporary directory.
func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "autonomy", "budget.json")
}

// open is Open with the default limits and a fixed clock, failing the test on the
// errors that mean the arguments were unusable.
func open(t *testing.T, path string, c *clock) *Budget {
	t.Helper()
	b, err := Open(path, DefaultLimits(), c.Now)
	if err != nil {
		t.Fatalf("opening the budget at %s: %v", path, err)
	}
	return b
}

func TestOpen_RejectsAnEmptyPath(t *testing.T) {
	if _, err := Open("  ", DefaultLimits(), nil); err == nil {
		t.Fatal("an empty state path must be refused")
	}
}

func TestOpen_MissingFileIsAFreshInstall(t *testing.T) {
	c := newClock()
	b := open(t, statePath(t), c)

	if b.Sealed() {
		t.Fatal("a missing state file is an empty state, not a corrupt one")
	}
	b.Begin()
	if g := admit(t, b, "prod", "api", c.Now()); !g.Admitted() {
		t.Fatalf("a fresh install must admit within its bounds, got %s", g)
	}
}

func TestOpen_BreakerSurvivesARestart(t *testing.T) {
	path := statePath(t)
	c := newClock()

	first := open(t, path, c)
	first.Begin()
	for range DefaultFailureThreshold {
		first.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, c.Now())
	}

	// The whole reason this file exists: the condition that tripped the breaker
	// outlives the process that observed it.
	second := open(t, path, c)
	second.Begin()
	g := admit(t, second, "prod", "api", c.Now())
	if g.Reason != ReasonBreakerTripped {
		t.Fatalf("the breaker must survive a restart, got %s", g)
	}

	tripped := second.Status().Tripped()
	if len(tripped) != 1 || tripped[0].ConsecutiveFailures != DefaultFailureThreshold {
		t.Fatalf("the failure run must survive too, got %+v", tripped)
	}
}

func TestOpen_CooldownSurvivesARestart(t *testing.T) {
	path := statePath(t)
	c := newClock()

	first := open(t, path, c)
	first.Begin()
	if g := admit(t, first, "prod", "api", c.Now()); !g.Admitted() {
		t.Fatalf("the first admission must succeed, got %s", g)
	}

	// A restart must not be a way to defeat the cooldown — otherwise a crash loop in
	// MaKlaude itself becomes an unbounded action loop against the cluster.
	c.advance(time.Minute)
	second := open(t, path, c)
	second.Begin()
	if g := admit(t, second, "prod", "api", c.Now()); g.Reason != ReasonTargetCoolingDown {
		t.Fatalf("the cooldown must survive a restart, got %s", g)
	}
}

func TestOpen_ClearSurvivesARestartWithItsAttribution(t *testing.T) {
	path := statePath(t)
	c := newClock()

	first := open(t, path, c)
	first.Trip("prod", "test", c.Now())
	if err := first.Clear("prod", "gigi", c.Now()); err != nil {
		t.Fatalf("clearing the breaker: %v", err)
	}

	second := open(t, path, c)
	if len(second.Status().Tripped()) != 0 {
		t.Fatal("a cleared breaker must stay cleared across a restart")
	}

	// "Who re-authorized autonomy on this cluster, and when" is what an incident
	// review asks next, so the clear is kept rather than collapsing to an empty entry.
	var onDisk state
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parsing the state file: %v", err)
	}
	cs, ok := onDisk.Clusters["prod"]
	if !ok {
		t.Fatalf("the cleared cluster must be retained, got %+v", onDisk.Clusters)
	}
	if cs.ClearedBy != "gigi" || cs.ClearedAt.IsZero() {
		t.Errorf("the clear's attribution must persist, got %+v", cs)
	}
}

func TestOpen_CorruptFileSealsRatherThanGoingUnbounded(t *testing.T) {
	for name, contents := range map[string]string{
		"truncated json": `{"version":1,"clusters":{"prod":`,
		"not json":       "breaker: tripped\n",
		"empty file":     "",
		"null cluster":   `{"version":1,"clusters":{"prod":null}}`,
		"future version": `{"version":2,"clusters":{}}`,
		"unknown field":  `{"version":1,"clusters":{},"maxSpendPerDay":100}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := statePath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatalf("preparing the directory: %v", err)
			}
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("writing the corrupt state: %v", err)
			}

			c := newClock()
			b, err := Open(path, DefaultLimits(), c.Now)
			// The error must NOT be how this is reported: a caller writing the ordinary
			// `if err != nil { return }` would drop the budget and thereby delete the
			// ceiling. The seal is a state of the object instead.
			if err != nil {
				t.Fatalf("a corrupt file must not be a returned error: %v", err)
			}
			if b == nil {
				t.Fatal("a corrupt file must still yield a budget, or the caller has no ceiling at all")
			}
			if !b.Sealed() {
				t.Fatal("a corrupt file must seal the budget")
			}

			b.Begin()
			g := admit(t, b, "prod", "api", c.Now())
			if g.Admitted() {
				t.Fatalf("a sealed budget must admit nothing, got %s", g)
			}
			if g.Reason != ReasonStateUnreadable {
				t.Errorf("reason = %s, want %s", g.Reason, ReasonStateUnreadable)
			}

			// And the operator has to be able to see it: a sealed budget blocks all
			// autonomy, which looks exactly like a quiet, healthy system.
			s := b.Status()
			if !s.Sealed || s.SealDetail == "" {
				t.Errorf("the seal must be visible in the status, got %+v", s)
			}
			if !strings.Contains(s.SealDetail, path) {
				t.Errorf("the seal detail must name the file, got %q", s.SealDetail)
			}
		})
	}
}

func TestClear_IsRefusedOnASealedBudget(t *testing.T) {
	path := statePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt state: %v", err)
	}

	b, err := Open(path, DefaultLimits(), newClock().Now)
	if err != nil {
		t.Fatalf("opening the budget: %v", err)
	}
	if err := b.Clear("prod", "gigi", base); err == nil {
		t.Fatal("clearing a breaker whose state cannot be read must be refused")
	}
}

func TestRecordOutcome_StillAsksForARollbackWhenSealed(t *testing.T) {
	path := statePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing the directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatalf("writing the corrupt state: %v", err)
	}
	b, err := Open(path, DefaultLimits(), newClock().Now)
	if err != nil {
		t.Fatalf("opening the budget: %v", err)
	}

	// Being unable to read the ceiling is a reason to trust it less, never a reason
	// to skip the rollback and the escalation an action that already ran needs.
	c := b.RecordOutcome("prod", target("prod", "api"), OutcomeFailed, base)
	if !c.RollBack || !c.Demote || !c.Escalate {
		t.Fatalf("a sealed budget must still ask for the failure response, got %+v", c)
	}
}

func TestPersist_WriteFailureSealsTheBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "budget.json")
	c := newClock()
	b := open(t, path, c)
	b.Begin()

	// Make the directory unwritable so the atomic rewrite cannot create its temporary
	// file. State that cannot be written is state the next process will not see, so
	// the breaker and the cooldowns have stopped being durable.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("making the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the directory mode")
	}

	if g := admit(t, b, "prod", "api", c.Now()); !g.Admitted() {
		t.Fatalf("the admission itself is within bounds, got %s", g)
	}
	if !b.Sealed() {
		t.Fatal("a state write that failed must seal the budget")
	}
	b.Begin()
	if g := admit(t, b, "prod", "web", c.Now()); g.Reason != ReasonStateUnreadable {
		t.Fatalf("a sealed budget must admit nothing further, got %s", g)
	}
}

func TestSave_WritesAPrivateFile(t *testing.T) {
	path := statePath(t)
	c := newClock()
	b := open(t, path, c)
	b.Trip("prod", "test", c.Now())

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat-ing the state file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %v, want 0600", perm)
	}
}

func TestSave_PrunesExpiredCooldownsAndEmptyClusters(t *testing.T) {
	path := statePath(t)
	c := newClock()
	b := open(t, path, c)

	b.Begin()
	admit(t, b, "prod", "api", c.Now())

	// Past the cooldown the entry decides nothing, so keeping it would only grow the
	// file by one line per object ever touched.
	c.advance(DefaultCooldown + time.Minute)
	b.Begin()
	admit(t, b, "prod", "web", c.Now())

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the state file: %v", err)
	}
	var onDisk state
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("parsing the state file: %v", err)
	}
	cs, ok := onDisk.Clusters["prod"]
	if !ok {
		t.Fatalf("the live cooldown must keep its cluster, got %+v", onDisk.Clusters)
	}
	if _, stale := cs.LastAdmitted["deployment/default/api"]; stale {
		t.Errorf("an expired cooldown must be pruned, got %+v", cs.LastAdmitted)
	}
	if _, live := cs.LastAdmitted["deployment/default/web"]; !live {
		t.Errorf("a live cooldown must be kept, got %+v", cs.LastAdmitted)
	}

	// With every cooldown expired and no breaker, the cluster carries nothing.
	c.advance(DefaultCooldown + time.Minute)
	b.Begin()
	admit(t, b, "staging", "api", c.Now())
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading the state file: %v", err)
	}
	onDisk = state{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("re-parsing the state file: %v", err)
	}
	if _, kept := onDisk.Clusters["prod"]; kept {
		t.Errorf("a cluster with nothing to say must be pruned, got %+v", onDisk.Clusters)
	}
}

func TestSave_PruningNeverDropsATrippedBreaker(t *testing.T) {
	path := statePath(t)
	c := newClock()
	b := open(t, path, c)

	b.Begin()
	admit(t, b, "prod", "api", c.Now())
	b.Trip("prod", "test", c.Now())

	// Long past every cooldown, so pruning has a reason to look at this cluster.
	c.advance(DefaultCooldown * 100)
	b.Begin()
	admit(t, b, "staging", "api", c.Now())

	reopened := open(t, path, c)
	if len(reopened.Status().Tripped()) != 1 {
		t.Fatalf("pruning must never close a breaker, got %+v", reopened.Status().Breakers)
	}
}

func TestStatus_NamesTheFileAnOperatorMustLookAt(t *testing.T) {
	path := statePath(t)
	b := open(t, path, newClock())
	if got := b.Status().Path; got != path {
		t.Errorf("status path = %q, want %q", got, path)
	}
	if got := NewMemory(DefaultLimits(), nil).Status().Path; got != "" {
		t.Errorf("an in-memory budget has no path, got %q", got)
	}
}
