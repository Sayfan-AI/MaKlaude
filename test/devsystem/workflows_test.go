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
// observed to fail. 45 then failed too (#96), then 60 (#101).
//
// The floor is deliberately NOT raised past 60. Six raises in a row did not
// converge because one number was being fed by two unrelated failure classes;
// budgetsFailedBeforeDelivering and budgetsFailedDuringWrapUp below split them,
// and only the first class is something a floor can fix. See #97 and
// .genesis/design/agent-turn-budgets.md.
//
// See #89, #96, #101. Raising an individual budget above the floor is always fine.
const orchestratorClassFloor = 60

// budgetsFailedBeforeDelivering are orchestrator-class `--max-turns` values that
// died at error_max_turns *without the run's deliverable landing* — the budget
// ran out mid-task, so the work simply did not happen:
//
//	20 — evolver, daily Jun 22–24, no progress (#38)
//	20 — events runner, died mid-task on #78/PR #79 (#80, #81)
//	30 — evolver, died at turn 31 having produced NO artifact at all (#85)
//
// THIS is the list the floor answers to. Append-only history, not
// configuration: a budget seen to be too small to reach a deliverable never
// becomes sufficient again.
var budgetsFailedBeforeDelivering = []int{20, 30}

// budgetsFailedDuringWrapUp are orchestrator-class `--max-turns` values that
// died at error_max_turns *seconds after the deliverable already landed*:
//
//	40 — scheduled orchestrator, died at turn 41 just after opening PR #86 (#87)
//	45 — evolver, died at turn 46 just after PR #95 merged (#96)
//	60 — evolver, died at turn 61 after PR #100 landed (#101)
//
// These are recorded but deliberately do NOT drive the floor, and that
// distinction is the whole point of this file's split.
//
// Conflating them with the list above is why six consecutive raises failed to
// converge. Implementation cost is unbounded — it scales with whatever task the
// run picks up, chosen *during* the run — so for any floor N there is a task
// that overruns it, and a death after the deliverable landed is evidence about
// the *task*, not about N. Reading these three as "the floor is still too low"
// produces raise #7 and then raise #8.
//
// What actually addressed this class was placement, not size: move every output
// a human must receive outside the agent's budget — nudge-gates.sh runs before
// the agent step, escalate.sh after it, and checkpoint.sh is the agent's first
// write. See #97, #103 and .genesis/design/agent-turn-budgets.md.
//
// Triage rule for the next death, so it lands in the right list: read the
// escalation's "What this run may already have landed" section. Deliverable
// present → wrap-up truncation, append here, do not touch the floor. Nothing
// landed → append above, and the floor moves.
var budgetsFailedDuringWrapUp = []int{40, 45, 60}

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
// the floor must be strictly greater than every budget seen to die at
// error_max_turns *before its deliverable landed*. Without this, "raise the
// floor above what has failed" stays a comment someone has to remember; with
// it, appending the next such failure fails the build until the floor moves.
//
// Scoped to budgetsFailedBeforeDelivering on purpose. The earlier version read
// every observed failure, which made it assert something that only stayed true
// while the list was stale: 60 died at error_max_turns on 2026-07-28 (#101) and
// 60 *is* the floor, so the test was green solely because nobody had appended
// it. Widening it back to include wrap-up truncations does not make it stricter
// — it makes it unsatisfiable, because that class has no smallest sufficient
// value to clear.
func TestOrchestratorFloorExceedsObservedFailures(t *testing.T) {
	for _, failed := range budgetsFailedBeforeDelivering {
		if orchestratorClassFloor <= failed {
			t.Errorf("orchestratorClassFloor is %d but %d has already been observed to die at error_max_turns before delivering — the floor must exceed every such failure, or the class recurs on whichever runner is next-tightest", orchestratorClassFloor, failed)
		}
	}
}

// TestBudgetFailureClassesAreDisjoint keeps the split from quietly collapsing.
//
// The two lists exist to answer different questions, so one budget cannot be in
// both: a given value either was or was not enough to reach a deliverable. A
// duplicate means a death was filed twice under conflicting readings, and since
// only one list drives the floor, the disagreement would be invisible — exactly
// the "green because the data is stale" failure this split was made to fix.
//
// It also catches the tempting shortcut of appending the next wrap-up
// truncation to BOTH lists "to be safe": that quietly forces raise #7.
func TestBudgetFailureClassesAreDisjoint(t *testing.T) {
	beforeDelivering := make(map[int]bool, len(budgetsFailedBeforeDelivering))
	for _, b := range budgetsFailedBeforeDelivering {
		beforeDelivering[b] = true
	}
	for _, b := range budgetsFailedDuringWrapUp {
		if beforeDelivering[b] {
			t.Errorf("budget %d is listed as both a before-delivering failure and a wrap-up truncation — a budget either did or did not reach a deliverable; pick one, because only the first list moves orchestratorClassFloor", b)
		}
	}
}

// TestWrapUpTruncationsAreRecordedNotForgotten guards the direction the split
// opens up. Wrap-up truncations no longer move the floor, which removes the
// build error that used to force someone to acknowledge them — so the new risk
// is that they stop being recorded at all and the loop forgets this class of
// death exists.
//
// The three known ones (#87, #96, #101) are what makes the "placement, not
// size" argument above evidence rather than an opinion. Asserting they are
// still present costs nothing and means deleting the history to make a floor
// raise look justified fails the build.
func TestWrapUpTruncationsAreRecordedNotForgotten(t *testing.T) {
	for _, want := range []int{40, 45, 60} {
		found := false
		for _, got := range budgetsFailedDuringWrapUp {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("budget %d died at error_max_turns after its deliverable landed (#87/#96/#101) but is no longer in budgetsFailedDuringWrapUp — this history is the evidence that raising the floor does not fix this class", want)
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
