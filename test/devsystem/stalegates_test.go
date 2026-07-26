// Stale-gate escalation tests.
//
// Why these exist: a human-in-the-loop gate that nobody answers is the one
// failure mode with no safety net — every orchestrator run correctly does
// nothing while a `needs:human` gate is open, so a dropped gate produces no
// failing run, no red CI, and no signal at all. Milestone 4's plan gate (#76)
// sat 21 days across ~85 ticks that way. The fix (#84) is deterministic, so its
// two load-bearing properties get deterministic tests: the threshold actually
// filters, and the nudge fires exactly once per gate.
//
// The scripts talk to GitHub only through `gh`, so a stub `gh` on PATH is
// enough to exercise them end to end. The stub keeps its issue list in a state
// file and mutates it on `issue edit --add-label`, so a second run of the
// script sees the label the first run applied — which is precisely what
// idempotency depends on.
package devsystem

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ghStub is a fake `gh` supporting exactly the calls the genesis scripts make:
// `issue list`, `issue view`, `issue comment`, `issue edit`, and `label create`.
// Every invocation is appended to calls.log so a test can assert on the writes.
const ghStub = `#!/usr/bin/env python3
import json, os, sys

state_path = os.environ["STUB_STATE"]
log_path = os.environ["STUB_LOG"]
argv = sys.argv[1:]

with open(log_path, "a") as f:
    f.write(" ".join(argv) + "\n")

def load():
    with open(state_path) as f:
        return json.load(f)

def save(issues):
    with open(state_path, "w") as f:
        json.dump(issues, f)

def flag(name, default=None):
    return argv[argv.index(name) + 1] if name in argv else default

if argv[:2] == ["issue", "list"]:
    label = flag("--label")
    issues = [i for i in load()
              if not label or label in [l["name"] for l in i["labels"]]]
    json.dump(issues, sys.stdout)
elif argv[:2] == ["issue", "view"]:
    num = int(argv[2])
    issue = next(i for i in load() if i["number"] == num)
    json.dump({"labels": issue["labels"]}, sys.stdout)
elif argv[:2] == ["issue", "edit"]:
    num = int(argv[2])
    add = flag("--add-label")
    issues = load()
    for i in issues:
        if i["number"] == num and add:
            i["labels"].append({"name": add})
    save(issues)
elif argv[:2] == ["issue", "comment"]:
    pass
elif argv[:2] == ["label", "create"]:
    pass
else:
    sys.stderr.write("stub gh: unsupported call: %s\n" % argv)
    sys.exit(64)
`

type stubIssue struct {
	Number    int                 `json:"number"`
	Title     string              `json:"title"`
	State     string              `json:"state"`
	URL       string              `json:"url"`
	Labels    []map[string]string `json:"labels"`
	Assignees []map[string]string `json:"assignees"`
	CreatedAt string              `json:"createdAt"`
	UpdatedAt string              `json:"updatedAt"`
}

// gate builds an open needs:human issue created ageDays ago.
func gate(number int, title string, ageDays int, extraLabels ...string) stubIssue {
	ts := time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339)
	labels := []map[string]string{{"name": "needs:human"}}
	for _, l := range extraLabels {
		labels = append(labels, map[string]string{"name": l})
	}
	return stubIssue{
		Number: number, Title: title, State: "OPEN",
		URL:    fmt.Sprintf("https://example.test/issues/%d", number),
		Labels: labels, Assignees: []map[string]string{},
		CreatedAt: ts, UpdatedAt: ts,
	}
}

// gateEnv installs the stub gh and seeds the issue state. It returns the env
// for exec.Cmd plus the paths of the state and call-log files.
func gateEnv(t *testing.T, issues []stubIssue) (env []string, statePath, logPath string) {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(ghStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	statePath = filepath.Join(dir, "state.json")
	b, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal issues: %v", err)
	}
	if err := os.WriteFile(statePath, b, 0o644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	logPath = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_STATE="+statePath,
		"STUB_LOG="+logPath,
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	return env, statePath, logPath
}

// runScript executes a .genesis script with the stubbed environment.
func runScript(t *testing.T, env []string, script string, args ...string) string {
	t.Helper()
	path := filepath.Join("..", "..", ".genesis", "scripts", script)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cannot locate %s: %v", script, err)
	}
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", script, args, err, out)
	}
	return string(out)
}

func calls(t *testing.T, logPath string) []string {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func countCalls(t *testing.T, logPath, prefix string) int {
	t.Helper()
	n := 0
	for _, c := range calls(t, logPath) {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// TestStaleGatesRespectsThreshold pins the filter: a gate is reported only once
// it is at or past the threshold. Getting this wrong in either direction is bad
// — too eager nags a human who has had no reasonable chance to respond, too lax
// reproduces the 21-day silence #84 was filed for.
func TestStaleGatesRespectsThreshold(t *testing.T) {
	env, _, _ := gateEnv(t, []stubIssue{
		gate(76, "Milestone 4 plan", 21),
		gate(90, "Filed yesterday", 1),
		gate(91, "Exactly at threshold", 3),
	})

	out := runScript(t, env, "issues.sh", "stale-gates", "--stale-days", "3", "--format", "tsv")

	if !strings.Contains(out, "76\t21\t") {
		t.Errorf("21-day-old gate #76 not reported as stale; got:\n%s", out)
	}
	if !strings.Contains(out, "91\t3\t") {
		t.Errorf("gate #91 at exactly the 3-day threshold should be stale (>=, not >); got:\n%s", out)
	}
	if strings.Contains(out, "#90") || strings.Contains(out, "90\t1\t") {
		t.Errorf("1-day-old gate #90 must not be reported at a 3-day threshold; got:\n%s", out)
	}

	// Oldest first: a human scanning the list should see the worst offender at
	// the top.
	if i, j := strings.Index(out, "76\t"), strings.Index(out, "91\t"); i > j {
		t.Errorf("gates should be listed oldest first; got:\n%s", out)
	}
}

// TestGatesReportsAgeForAllGates covers the standing backstop: `gates` (and so
// `summary`) shows every open gate with its age, stale or not, so an unanswered
// gate is never absent from a run's state assessment.
func TestGatesReportsAgeForAllGates(t *testing.T) {
	env, _, _ := gateEnv(t, []stubIssue{
		gate(76, "Milestone 4 plan", 21),
		gate(90, "Filed yesterday", 1),
	})

	out := runScript(t, env, "issues.sh", "gates", "--stale-days", "3")

	for _, want := range []string{"#76", "21d", "STALE", "#90", "1d"} {
		if !strings.Contains(out, want) {
			t.Errorf("`gates` output missing %q; got:\n%s", want, out)
		}
	}
	// The fresh gate is listed but must not be flagged. Check the line, not the
	// whole output, since #76 legitimately carries STALE.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "#90") && strings.Contains(line, "STALE") {
			t.Errorf("1-day-old gate flagged STALE at a 3-day threshold: %q", line)
		}
	}
}

// TestNudgeIsIdempotent is the core guard. The "don't re-notify the user" rule
// makes a duplicate nudge a real defect, not a cosmetic one, and the obvious
// implementation (post whenever the gate is stale) re-nags every 6 hours
// forever. Two runs, one comment.
func TestNudgeIsIdempotent(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{
		gate(76, "Milestone 4 plan", 21),
		gate(90, "Filed yesterday", 1),
	})

	first := runScript(t, env, "nudge-gates.sh")
	if !strings.Contains(first, "#76") {
		t.Fatalf("first run should nudge #76; got:\n%s", first)
	}
	if got := countCalls(t, logPath, "issue comment 76"); got != 1 {
		t.Errorf("first run: want 1 comment on #76, got %d\ncalls:\n%s",
			got, strings.Join(calls(t, logPath), "\n"))
	}
	if got := countCalls(t, logPath, "issue edit 76"); got != 1 {
		t.Errorf("first run: want 1 label edit on #76, got %d", got)
	}
	if got := countCalls(t, logPath, "issue comment 90"); got != 0 {
		t.Errorf("fresh gate #90 must not be nudged, got %d comments", got)
	}

	// Second run — the marker label applied above must suppress the nudge.
	second := runScript(t, env, "nudge-gates.sh")
	if !strings.Contains(second, "already posted") {
		t.Errorf("second run should report #76 as already nudged; got:\n%s", second)
	}
	if got := countCalls(t, logPath, "issue comment 76"); got != 1 {
		t.Errorf("nudge is not idempotent: %d comments on #76 after two runs — "+
			"a stale gate would be re-nagged every scheduled tick", got)
	}
}

// TestNudgeNoopsWhenAllGatesFresh guards the common case: nothing stale means
// nothing written at all. A script that "helpfully" comments anyway would turn
// every 6-hour tick into noise on an open gate.
func TestNudgeNoopsWhenAllGatesFresh(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{gate(90, "Filed yesterday", 1)})

	out := runScript(t, env, "nudge-gates.sh")
	if !strings.Contains(out, "nothing to nudge") {
		t.Errorf("expected an all-clear message; got:\n%s", out)
	}
	for _, c := range calls(t, logPath) {
		if strings.HasPrefix(c, "issue comment") || strings.HasPrefix(c, "issue edit") {
			t.Errorf("no writes expected when every gate is fresh, got: %q", c)
		}
	}
}

// TestScheduledOrchestratorRunsNudge ties the script to its trigger. The check
// is worthless if nothing calls it, and it must run BEFORE the agent step so
// the signal escapes even when that run dies at max-turns — the exact
// circumstance under which a human most needs to hear from the system.
func TestScheduledOrchestratorRunsNudge(t *testing.T) {
	path := filepath.Join(workflowDir(t), "genesis-orchestrator.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := string(b)

	nudgeAt := strings.Index(body, "nudge-gates.sh")
	if nudgeAt < 0 {
		t.Fatalf("genesis-orchestrator.yml does not run nudge-gates.sh — the "+
			"stale-gate check has no trigger (%s)", path)
	}
	agentAt := strings.Index(body, claudeActionMarker)
	if agentAt < 0 {
		t.Fatalf("genesis-orchestrator.yml no longer invokes %s", claudeActionMarker)
	}
	if nudgeAt > agentAt {
		t.Errorf("nudge-gates.sh runs after the agent step; it must run before, " +
			"so a stale-gate nudge still escapes if the agent dies at max-turns")
	}
}
