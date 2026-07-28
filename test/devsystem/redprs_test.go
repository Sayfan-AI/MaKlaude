// Tests for `issues.sh red-prs` — the state-derived recovery for dropped
// CI-failure triage events.
//
// Why this exists: `genesis-ci-failure.yml` can only triage the run named in
// `github.event.workflow_run`, so its work is *event-payload dependent*. GitHub
// keeps one pending run per concurrency group and evicts the older one, and the
// survivor carries a different payload — so an evicted triage event is not
// delayed, it is gone. That happened to the only genuine CI failure in the repo's
// history (run 28736219761, 2026-07-05 09:29:04) and the workflow has 0 successes
// in 157 runs.
//
// Moving the group below the gate stops no-op triggers from causing that. But two
// *real* runs can still collide, so the recovery has to not depend on the event at
// all: "which PRs are red" is re-derived from live repo state and printed by
// `summary` on every scheduled tick. The generalizable rule these tests protect —
// event-payload work is lost under supersession, state-derived work self-heals —
// only holds while red-prs stays honest about what counts as red, hence the
// false-positive cases below.
package devsystem

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// prStub mimics `gh pr list --json ...` for issues.sh. Only the one call shape
// red-prs makes is supported; anything else exits non-zero so a silent change in
// how the script queries PRs shows up as a test failure rather than empty output.
const prGhStub = `#!/usr/bin/env python3
import json, os, sys
argv = sys.argv[1:]
if argv[:2] == ["pr", "list"]:
    with open(os.environ["STUB_PRS"]) as f:
        sys.stdout.write(f.read())
elif argv[:2] == ["issue", "list"]:
    print("[]")
else:
    sys.stderr.write("stub gh: unsupported call: %s\n" % argv)
    sys.exit(64)
`

type stubCheck struct {
	Name       string `json:"name,omitempty"`
	Context    string `json:"context,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	State      string `json:"state,omitempty"`
}

type stubPR struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	HeadRef   string      `json:"headRefName"`
	IsDraft   bool        `json:"isDraft"`
	UpdatedAt string      `json:"updatedAt"`
	Rollup    []stubCheck `json:"statusCheckRollup"`
}

func pr(number int, title string, ageDays int, checks ...stubCheck) stubPR {
	return stubPR{
		Number: number, Title: title,
		HeadRef:   "branch-for-pr",
		UpdatedAt: time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339),
		Rollup:    checks,
	}
}

// runRedPRs runs `issues.sh red-prs` against a stubbed PR list.
func runRedPRs(t *testing.T, prs []stubPR) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(prGhStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	// A nil slice must marshal to `[]`, not `null` — the script's json.load would
	// otherwise get None and the test would pass for the wrong reason.
	if prs == nil {
		prs = []stubPR{}
	}
	b, err := json.Marshal(prs)
	if err != nil {
		t.Fatalf("marshal prs: %v", err)
	}
	prsPath := filepath.Join(dir, "prs.json")
	if err := os.WriteFile(prsPath, b, 0o644); err != nil {
		t.Fatalf("write prs: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "red-prs")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_PRS="+prsPath,
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issues.sh red-prs failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestRedPRsDetectsBothCheckShapes pins the parse. `statusCheckRollup` mixes
// CheckRun (name/conclusion) and StatusContext (context/state) entries; reading
// only one shape would silently under-report, which is the exact failure this
// whole mechanism is the backstop for.
func TestRedPRsDetectsBothCheckShapes(t *testing.T) {
	out := runRedPRs(t, []stubPR{
		pr(11, "check-run failure", 2,
			stubCheck{Name: "CI", Conclusion: "FAILURE"},
			stubCheck{Name: "E2E (kind)", Conclusion: "SUCCESS"}),
		pr(12, "status-context failure", 1,
			stubCheck{Context: "legacy/status", State: "FAILURE"}),
	})

	for _, want := range []string{"#11", "CI", "#12", "legacy/status"} {
		if !strings.Contains(out, want) {
			t.Errorf("red-prs output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "E2E (kind)") {
		t.Errorf("red-prs listed a PASSING check as failing:\n%s", out)
	}
}

// TestRedPRsIgnoresHealthyPRs is the false-positive guard. This report is printed
// on every scheduled tick, so anything it flags spuriously trains the orchestrator
// to discount it — and a backstop nobody trusts is not a backstop. A PR mid-CI in
// particular is not stuck and must not be reported as red.
func TestRedPRsIgnoresHealthyPRs(t *testing.T) {
	cases := []struct {
		name  string
		input stubPR
	}{
		{"all green", pr(21, "green", 0, stubCheck{Name: "CI", Conclusion: "SUCCESS"})},
		{"still running", pr(22, "in progress", 0, stubCheck{Name: "CI"})},
		{"skipped check", pr(23, "skipped", 0, stubCheck{Name: "CI", Conclusion: "SKIPPED"})},
		{"neutral check", pr(24, "neutral", 0, stubCheck{Name: "CI", Conclusion: "NEUTRAL"})},
		{"no checks yet", pr(25, "no checks", 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := runRedPRs(t, []stubPR{c.input}); strings.TrimSpace(out) != "" {
				t.Errorf("%s PR reported as red — empty output means all-clear, so a false positive here is a false alarm every tick:\n%s", c.name, out)
			}
		})
	}
}

// TestRedPRsEmptyWhenNoPRs keeps "no output" meaning "all clear" for callers.
func TestRedPRsEmptyWhenNoPRs(t *testing.T) {
	if out := runRedPRs(t, nil); strings.TrimSpace(out) != "" {
		t.Errorf("expected no output with zero open PRs, got:\n%s", out)
	}
}

// TestRedPRsOrdersStalestFirst — a PR red for days is the one being forgotten, so
// it must lead. Ordering is the whole value of the report once more than one PR is
// red.
func TestRedPRsOrdersStalestFirst(t *testing.T) {
	out := runRedPRs(t, []stubPR{
		pr(31, "fresh", 0, stubCheck{Name: "CI", Conclusion: "FAILURE"}),
		pr(32, "ancient", 9, stubCheck{Name: "CI", Conclusion: "FAILURE"}),
	})
	if i, j := strings.Index(out, "#32"), strings.Index(out, "#31"); i < 0 || j < 0 || i > j {
		t.Errorf("expected #32 (9d red) before #31 (0d red):\n%s", out)
	}
	if !strings.Contains(out, "red 9d") {
		t.Errorf("expected the age to be reported so staleness needs no LLM judgment:\n%s", out)
	}
}

// TestSummaryReportsRedPRs is the membership half: red-prs existing is useless if
// the orchestrator's state assessment does not print it. `summary` is what every
// run reads, so the section must be unconditional there — same reasoning as the
// Human Gates section (#84).
func TestSummaryReportsRedPRs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	summary := src[strings.Index(src, "    summary)"):]
	if end := strings.Index(summary, "\n    close)"); end > 0 {
		summary = summary[:end]
	}
	if !strings.Contains(summary, "Red PRs") || !strings.Contains(summary, "format_red_prs") {
		t.Errorf("issues.sh `summary` does not print red PRs — a dropped ci-failure triage event would then be invisible again:\n%s", summary)
	}
}
