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
// Every earlier fix in this class raised only the ONE runner that happened to
// fail, leaving the class floor where it was — so the same failure recurred on
// whichever runner was next-tightest. #89 moved the floor for the class instead
// of the instance, but set it to 40, a budget that had *itself* already been
// observed to fail. 45 then failed too (#96). The floor must sit strictly above
// every budget ever observed to die at error_max_turns for this shape;
// observedFailedBudgets below makes that mechanical rather than a comment.
//
// See #89, #96. Raising an individual budget above the floor is always fine.
const orchestratorClassFloor = 60

// observedFailedBudgets are the orchestrator-class `--max-turns` values that
// have actually died at error_max_turns in this repo, with the escalation issue
// that recorded each:
//
//	20 — evolver, daily Jun 22–24 (#38)
//	20 — events runner, mid-task on #78/PR #79 (#80, #81)
//	30 — evolver, no artifact at all (#85)
//	40 — scheduled orchestrator, died at turn 41 just after opening PR #86 (#87)
//	45 — evolver, died at turn 46 just after PR #95 merged (#96)
//
// The list is append-only history, not configuration: a budget that has been
// seen to be insufficient never becomes sufficient again.
var observedFailedBudgets = []int{20, 30, 40, 45}

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
	"genesis-orchestrator.yml": orchestratorClassFloor, // scheduled; assess + one unit of work (#87)
	"genesis-events.yml":       orchestratorClassFloor, // same agent, event-triggered (#81)
	"genesis-evolver.yml":      orchestratorClassFloor, // self-improvement + subagents (#38, #85, #96)
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

// TestOrchestratorFloorExceedsObservedFailures encodes the class rule itself:
// the floor must be strictly greater than every budget already seen to die at
// error_max_turns. Six times running, a fix set the floor to a value at or
// below an observed failure and the class recurred on the next-tightest runner
// (#38, #80/#81, #85, #87, #96). Without this, "raise the floor above what has
// failed" stays a comment someone has to remember; with it, appending the next
// observed failure to observedFailedBudgets fails the build until the floor
// moves.
func TestOrchestratorFloorExceedsObservedFailures(t *testing.T) {
	for _, failed := range observedFailedBudgets {
		if orchestratorClassFloor <= failed {
			t.Errorf("orchestratorClassFloor is %d but %d has already been observed to die at error_max_turns — the floor must exceed every observed failure, or the class recurs on whichever runner is next-tightest", orchestratorClassFloor, failed)
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
