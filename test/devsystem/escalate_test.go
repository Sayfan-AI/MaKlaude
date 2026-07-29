package devsystem

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Why these tests: a run that dies at max-turns has usually already produced its
// deliverable and lost only the wrap-up (#87 died seconds after opening PR #86;
// #96 seconds after merging PR #95). The escalation is the ONLY thing a human
// sees in that case, so two properties have to hold mechanically rather than by
// review: every agent-running workflow must have a failure path at all, and that
// path must say what landed. Both are the "test the membership, not the member"
// shape that the turn-budget floors and the concurrency group each had to learn
// the hard way — an opt-in invariant is not an invariant (#97).

const escalateScript = ".genesis/scripts/escalate.sh"

// escalatePathFor resolves .genesis/scripts/escalate.sh relative to this test's
// package dir, matching workflowDir's approach in workflows_test.go.
func escalatePathFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", escalateScript)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return p
}

var (
	ifFailureRE = regexp.MustCompile(`if:\s*failure\(\)`)
	escalateRE  = regexp.MustCompile(`escalate\.sh`)
)

// escalateWithinLines is how far after an `if: failure()` the escalate.sh call
// may appear and still count as guarded by it. A step is `if:` + `name:` +
// `run:` + an `env:` block; 15 lines covers that with room to spare while still
// rejecting an escalate.sh that lives in an unrelated, unguarded step.
const escalateWithinLines = 15

// TestEveryClaudeWorkflowEscalatesOnFailure asserts the membership property: a
// workflow that spends an agent turn budget can die at max-turns, so it MUST
// have a deterministic failure path that tells a human. Adding a new
// Claude-invoking workflow without one fails the build instead of silently
// creating a runner whose deaths are invisible.
func TestEveryClaudeWorkflowEscalatesOnFailure(t *testing.T) {
	dir := workflowDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(b), claudeActionMarker) {
			continue
		}
		checked++

		lines := strings.Split(string(b), "\n")
		guarded := false
		for i, line := range lines {
			if !ifFailureRE.MatchString(line) {
				continue
			}
			end := min(i+1+escalateWithinLines, len(lines))
			for _, follow := range lines[i+1 : end] {
				if escalateRE.MatchString(follow) {
					guarded = true
					break
				}
			}
			if guarded {
				break
			}
		}
		if !guarded {
			t.Errorf("%s runs %s but has no `if: failure()` step invoking %s — a max-turns death in this workflow would be silent, and the escalation is the only thing a human sees (#97)",
				name, claudeActionMarker, escalateScript)
		}
	}

	if checked == 0 {
		t.Fatalf("no workflow under %s runs %s — layout changed, so this test guards nothing", dir, claudeActionMarker)
	}
}

// TestEscalationReportsLandedArtifacts pins the content of that failure path.
// An escalation saying only "run failed" costs a human a manual hunt for
// whether a PR or comment already landed — #87's fix was auto-merged while its
// escalation sat open. The report must also not be built on the search API,
// which is index-lagged: the artifacts worth reporting were created seconds
// before the run died, so search would routinely miss exactly those.
func TestEscalationReportsLandedArtifacts(t *testing.T) {
	b, err := os.ReadFile(escalatePathFor(t))
	if err != nil {
		t.Fatalf("read %s: %v", escalateScript, err)
	}
	src := string(b)

	// Discovery is a `since`-filtered read of the issues endpoint, which returns
	// PRs too, so one call covers both.
	for _, want := range []string{"gh api", "/issues", "since="} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer contains %q — artifact discovery is what turns \"run failed\" into \"run failed, and here is the PR it opened first\" (#97)",
				escalateScript, want)
		}
	}

	// The rendered body must actually surface it.
	if !strings.Contains(src, "may already have landed") {
		t.Errorf("%s builds an escalation body without the landed-artifact section — a human triaging it is back to guessing whether the run achieved anything (#97)", escalateScript)
	}

	// Search API is index-lagged; `--search` here would silently under-report.
	// Comment lines are exempt on purpose — the script explains *why* it avoids
	// search, and that rationale is the most valuable line in the file to keep.
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(line, "--search") {
			t.Errorf("%s:%d uses `--search` for artifact discovery (%q) — the search index lags by up to minutes and these artifacts are seconds old, so it would miss precisely the ones worth reporting; use the REST issues endpoint with `since` (#97)",
				escalateScript, i+1, strings.TrimSpace(line))
		}
	}
}

// TestEscalationClassifiesTheDeathClass pins the *conclusion* the escalation
// draws, not just the facts it lists.
//
// Artifact discovery (#97) gave a human the facts; it left the inference to
// them, and the inference has been drawn wrong six times running — every one of
// 15→30→40→45→60 was read as "the budget is too low" and raised. Three of those
// deaths (#87, #96, #101) happened *seconds after the deliverable landed*, where
// no budget is the fix. The escalation is where that call gets made, so both
// branches must name their class and point at the list the budget belongs in.
//
// Guarding the wording is deliberate: this is the sentence that stops raise #7,
// and it is worth more than the artifact list it sits under.
func TestEscalationClassifiesTheDeathClass(t *testing.T) {
	b, err := os.ReadFile(escalatePathFor(t))
	if err != nil {
		t.Fatalf("read %s: %v", escalateScript, err)
	}
	src := string(b)

	// The deliverable-landed branch must say plainly not to raise the budget,
	// and route the value to the list that does NOT move the floor.
	for _, want := range []string{"do NOT raise", "budgetsFailedDuringWrapUp"} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer contains %q — without it the escalation reports a wrap-up truncation as a bare failure and the reflex fix is budget raise #7 (#97, #101)",
				escalateScript, want)
		}
	}

	// The nothing-landed branch is the one case where the budget genuinely is
	// too small, and it must route to the list that DOES move the floor —
	// otherwise the split silently disables the guard it replaced.
	if !strings.Contains(src, "budgetsFailedBeforeDelivering") {
		t.Errorf("%s never names budgetsFailedBeforeDelivering — a run that died with nothing landed (#85) is the only evidence that raises orchestratorClassFloor, so the escalation must say where to record it", escalateScript)
	}

	// Both branches must carry a note; a classification that only fires on one
	// side leaves the other reading as "cause unknown".
	for _, want := range []string{"WRAPUP_NOTE", "NO_ARTIFACT_NOTE"} {
		if strings.Count(src, want) < 2 {
			t.Errorf("%s defines or uses %s fewer than twice — each note must be both defined and rendered into the body, or the classification never reaches the issue", escalateScript, want)
		}
	}
}
