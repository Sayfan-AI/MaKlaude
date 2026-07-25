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

// minTurns is the agreed minimum `--max-turns` per Claude-invoking workflow.
//
// Two tiers, deliberately:
//
//   - Orchestrator-class (30+): runs an open-ended agent definition that
//     assesses state, reads issues/PRs/diffs and dispatches subagents — each
//     subagent consuming turns from the same budget. The observed floor for
//     this shape is ~30; below it, runs die mid-task.
//   - Narrow-scope (15): a single well-specified job with a fixed procedure
//     (check green PRs, squash merge, close the issue). Its budget is small on
//     purpose — if it needs more turns, something is wrong and failing fast is
//     the correct outcome.
//
// Raising a budget above its minimum is always fine. Lowering one below it, or
// adding a Claude-invoking workflow without classifying it here, fails the test.
var minTurns = map[string]int{
	"genesis-orchestrator.yml": 30, // scheduled orchestrator; currently 40
	"genesis-events.yml":       30, // same agent as above, event-triggered; currently 40 (#81)
	"genesis-evolver.yml":      30, // self-improvement agent + subagents (#38)
	"genesis-ci-failure.yml":   30, // CI triage: read logs, classify, fix or escalate
	"genesis-merge.yml":        15, // narrow scope by design — see tier note above
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
			t.Errorf("%s invokes %s but has no entry in minTurns — classify it (orchestrator-class 30+, or narrow-scope) so its budget is guarded", name, claudeActionMarker)
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
