// Package devsystem holds regression tests for the *dev system* itself — the
// genesis GitHub Actions workflows that drive autonomous development — rather
// than for the MaKlaude product code.
//
// Why a test and not a review checklist: turn budgets drift silently. A
// Claude-invoking workflow whose `--max-turns` is too low does not fail loudly;
// it burns an API call, dies at `error_max_turns`, and produces no progress and
// no diagnosis (see #38 for the evolver, #81/#80 for the events runner). The
// only visible symptom is a safety-net escalation issue, days later. Encoding
// the agreed floors here turns that class of bug into a build error.
package devsystem

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// claudeActionMarker identifies a workflow step that spends an agent turn
// budget. Any workflow containing it MUST appear in minTurns below.
const claudeActionMarker = "anthropics/claude-code-action"

// orchestratorClassFloor is the minimum budget for any workflow running an
// open-ended agent definition: assess state, read issues/PRs/diffs, dispatch
// subagents, and complete a unit of work.
//
// It is 40 because every earlier fix in this class raised only the ONE runner
// that happened to fail, leaving the class floor where it was — so the same
// failure recurred on whichever runner was next-tightest: #38 (evolver at 20),
// #80/#81 (events at 20), #85 (evolver at 30), #87 (scheduled orchestrator at
// 40). 40 is the lowest budget not yet observed to fail for this shape. A
// runner should not have to die at max-turns to earn a workable budget, so this
// floor moves for the class and individual budgets sit at or above it.
//
// See #89. Raising an individual budget above the floor is always fine.
const orchestratorClassFloor = 40

// narrowScopeFloor is for a single well-specified job with a fixed procedure
// (check green PRs, squash merge, close the issue). Small on purpose — if such a
// job needs more turns, something is wrong and failing fast is the right
// outcome, so this floor deliberately does NOT track the class above.
const narrowScopeFloor = 15

// minTurns is the agreed minimum `--max-turns` per Claude-invoking workflow.
//
// Lowering a budget below its floor, or adding a Claude-invoking workflow
// without classifying it here, fails the test.
var minTurns = map[string]int{
	"genesis-orchestrator.yml": orchestratorClassFloor, // scheduled; currently 60 — assess + one unit of work (#87)
	"genesis-events.yml":       orchestratorClassFloor, // same agent, event-triggered; currently 40 (#81)
	"genesis-evolver.yml":      orchestratorClassFloor, // self-improvement + subagents; currently 45 (#38, #85)
	"genesis-ci-failure.yml":   orchestratorClassFloor, // CI triage: read logs, classify, fix or escalate
	"genesis-merge.yml":        narrowScopeFloor,       // narrow scope by design — see floor note above
}

var maxTurnsRE = regexp.MustCompile(`--max-turns\s+(\d+)`)

// workflowDir resolves .github/workflows relative to this test's package dir,
// so the test does not depend on the working directory `go test` chose.
func workflowDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", ".github", "workflows")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cannot locate .github/workflows from test dir: %v", err)
	}
	return dir
}

func claudeWorkflows(t *testing.T) map[string]string {
	t.Helper()
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	found := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if body := string(b); strings.Contains(body, claudeActionMarker) {
			found[name] = body
		}
	}
	if len(found) == 0 {
		t.Fatalf("no workflow references %s — the marker or the layout changed, so this test is no longer guarding anything", claudeActionMarker)
	}
	return found
}

// TestClaudeWorkflowsMeetTurnFloor is the actual guard: every Claude-invoking
// workflow declares a turn budget, and that budget is at or above its floor.
func TestClaudeWorkflowsMeetTurnFloor(t *testing.T) {
	for name, body := range claudeWorkflows(t) {
		floor, classified := minTurns[name]
		if !classified {
			t.Errorf("%s invokes %s but has no entry in minTurns — classify it (orchestratorClassFloor or narrowScopeFloor) so its budget is guarded", name, claudeActionMarker)
			continue
		}

		m := maxTurnsRE.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s invokes %s without an explicit --max-turns; an implicit budget is exactly the silent-drift case this test exists to catch", name, claudeActionMarker)
			continue
		}
		turns, err := strconv.Atoi(m[1])
		if err != nil {
			t.Errorf("%s: cannot parse --max-turns %q: %v", name, m[1], err)
			continue
		}
		if turns < floor {
			t.Errorf("%s: --max-turns %d is below the agreed floor of %d — a run this tight dies at error_max_turns with no progress and no diagnosis (see #81)", name, turns, floor)
		}
	}
}

// TestTurnFloorTableHasNoStaleEntries keeps the table honest in the other
// direction: a renamed or deleted workflow must not leave a floor behind that
// silently guards nothing.
func TestTurnFloorTableHasNoStaleEntries(t *testing.T) {
	found := claudeWorkflows(t)
	for name := range minTurns {
		if _, ok := found[name]; !ok {
			t.Errorf("minTurns lists %q but no such workflow invokes %s — remove or rename the entry", name, claudeActionMarker)
		}
	}
}
