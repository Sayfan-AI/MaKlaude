// Tests for `issues.sh failure-streaks` and the streak line escalate.sh prepends
// to a repeat-failure comment — the outage-visibility half of #151.
//
// Why this exists: escalate.sh dedups repeat failures per workflow, so failures
// 2..N of one cause become comments on one issue instead of new issues. That is
// correct — 14 issues for one dead credential would be worse — but its side
// effect is that a 3.5-day total loop outage (2026-07-30, #150) presented, to
// anyone scanning the issue list or `summary`, as two issues titled "a run
// failed". The recurrence count lived only inside a comment thread nobody
// re-reads. A single failure is noise; fourteen in a row is an outage, and the
// count is the only visible difference between them.
//
// Same family as stale-gates, red-prs, ready-prs and unanswered-comments:
// derived from repo state, printed unconditionally by `summary`, empty means
// all-clear. And the same false-positive discipline: this prints every tick, so
// a comment that is not a recorded failure (human triage, a bot diagnosis) must
// not inflate the streak.
package devsystem

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// failureGhStub mimics `gh issue list --json ...` for issues.sh. Only the one
// call shape failure-streaks makes is supported; anything else exits non-zero so
// a silent change in how the script queries issues shows up as a test failure
// rather than empty output.
const failureGhStub = `#!/usr/bin/env python3
import json, os, sys
argv = sys.argv[1:]
if argv[:2] == ["issue", "list"]:
    with open(os.environ["STUB_ISSUES"]) as f:
        sys.stdout.write(f.read())
else:
    sys.stderr.write("stub gh: unsupported call: %s\n" % argv)
    sys.exit(64)
`

type stubFailureComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type stubFailureIssue struct {
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	CreatedAt string        `json:"createdAt"`
	Comments  []stubFailureComment `json:"comments"`
}

// wfMarker matches the dedup marker escalate.sh embeds in every escalation body
// and repeat comment.
func wfMarker(wf string) string {
	return "<!-- genesis-failure-wf: " + wf + " -->"
}

func runFailureStreaks(t *testing.T, issues []stubFailureIssue) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(failureGhStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	// A nil slice must marshal to `[]`, not `null` — the script's json.load
	// would otherwise get None and the test would pass for the wrong reason.
	if issues == nil {
		issues = []stubFailureIssue{}
	}
	b, err := json.Marshal(issues)
	if err != nil {
		t.Fatalf("marshal issues: %v", err)
	}
	issuesPath := filepath.Join(dir, "issues.json")
	if err := os.WriteFile(issuesPath, b, 0o644); err != nil {
		t.Fatalf("write issues: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "failure-streaks")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_ISSUES="+issuesPath,
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issues.sh failure-streaks failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestFailureStreaksCountsMarkerCommentsOnly pins the counting rule: one failure
// for the issue body plus one per comment carrying the workflow's dedup marker.
// Human triage comments and bot diagnoses carry no marker and must not inflate
// the count — an overstated streak is the false alarm that teaches the
// orchestrator to skip the section.
func TestFailureStreaksCountsMarkerCommentsOnly(t *testing.T) {
	wf := "Genesis Orchestrator (Scheduled)"
	out := runFailureStreaks(t, []stubFailureIssue{{
		Number: 108, Title: "Autonomous system needs help: " + wf + " run failed",
		Body:      "A workflow run failed.\n\n" + wfMarker(wf),
		CreatedAt: "2026-07-30T01:50:43Z",
		Comments: []stubFailureComment{
			{Body: "repeat failure\n" + wfMarker(wf), CreatedAt: "2026-07-31T02:00:00Z"},
			{Body: "human triage: looks like auth", CreatedAt: "2026-07-31T03:00:00Z"},
			{Body: "repeat failure\n" + wfMarker(wf), CreatedAt: "2026-08-01T19:59:05Z"},
		},
	}})

	for _, want := range []string{"#108", "3 consecutive failures", "since 2026-07-30", "newest 2026-08-01", wf} {
		if !strings.Contains(out, want) {
			t.Errorf("failure-streaks output missing %q — the streak count and its endpoints are what separate an outage from a one-off (#151):\n%s", want, out)
		}
	}
	if strings.Contains(out, "4 ") {
		t.Errorf("a marker-less comment inflated the streak — human triage on the thread must not count as a failure:\n%s", out)
	}
}

// TestFailureStreaksSingleFailureIsNotAStreak — one failure renders as one
// failure, without the word "consecutive" doing outage work a single data point
// cannot support.
func TestFailureStreaksSingleFailureIsNotAStreak(t *testing.T) {
	wf := "Genesis Evolver"
	out := runFailureStreaks(t, []stubFailureIssue{{
		Number: 109, Title: "Autonomous system needs help: " + wf + " run failed",
		Body:      "A workflow run failed.\n\n" + wfMarker(wf),
		CreatedAt: "2026-07-30T11:07:22Z",
	}})
	if !strings.Contains(out, "1 failure") || strings.Contains(out, "consecutive") {
		t.Errorf("a single failure should read as exactly that:\n%s", out)
	}
}

// TestFailureStreaksOrdersLongestFirst — the workflow that has failed the most
// times running is the outage; it must lead regardless of which failed last.
func TestFailureStreaksOrdersLongestFirst(t *testing.T) {
	orch, evol := "Genesis Orchestrator (Scheduled)", "Genesis Evolver"
	out := runFailureStreaks(t, []stubFailureIssue{
		{
			Number: 109, Title: evol, Body: wfMarker(evol),
			CreatedAt: "2026-07-30T11:07:22Z",
		},
		{
			Number: 108, Title: orch, Body: wfMarker(orch),
			CreatedAt: "2026-07-30T01:50:43Z",
			Comments: []stubFailureComment{
				{Body: wfMarker(orch), CreatedAt: "2026-07-30T08:00:00Z"},
				{Body: wfMarker(orch), CreatedAt: "2026-07-30T14:00:00Z"},
			},
		},
	})
	if i, j := strings.Index(out, "#108"), strings.Index(out, "#109"); i < 0 || j < 0 || i > j {
		t.Errorf("expected #108 (3 failures) before #109 (1 failure):\n%s", out)
	}
}

// TestFailureStreaksEmptyWhenNoFailures keeps "no output" meaning "all clear"
// for callers, matching every other section of `summary`.
func TestFailureStreaksEmptyWhenNoFailures(t *testing.T) {
	if out := runFailureStreaks(t, nil); strings.TrimSpace(out) != "" {
		t.Errorf("expected no output with zero open failure escalations, got:\n%s", out)
	}
}

// TestFailureStreaksFallsBackToTitle — an escalation whose body lost its marker
// (hand-written, or an older format) must still be listed, not silently dropped:
// under-reporting here recreates the invisible outage this section exists to end.
func TestFailureStreaksFallsBackToTitle(t *testing.T) {
	out := runFailureStreaks(t, []stubFailureIssue{{
		Number: 38, Title: "Autonomous system needs help: a workflow run failed",
		Body:      "no marker in this one",
		CreatedAt: "2026-06-22T11:42:56Z",
	}})
	if !strings.Contains(out, "#38") || !strings.Contains(out, "a workflow run failed") {
		t.Errorf("a marker-less escalation was dropped instead of listed by title:\n%s", out)
	}
}

// TestSummaryReportsFailureStreaks is the membership half: the section is useless
// if the state assessment every run reads does not print it. Same reasoning as
// Human Gates (#84), Red PRs (#100), Ready to Merge (#112) and Unanswered Human
// Comments (#141) — unconditional, empty means all-clear.
func TestSummaryReportsFailureStreaks(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	summary := src[strings.Index(src, "    summary)"):]
	if end := strings.Index(summary, "\n    close)"); end > 0 {
		summary = summary[:end]
	}
	if !strings.Contains(summary, "Automation Failure Streaks") || !strings.Contains(summary, "format_failure_streaks") {
		t.Errorf("issues.sh `summary` does not print failure streaks — an outage then presents as one issue titled \"a run failed\", which is how #150 hid for 3.5 days (#151):\n%s", summary)
	}
}

// TestEscalationStatesTheStreak pins the writer's half: the repeat-failure
// comment itself must say "this is failure N since T", because the person who
// opens the issue reads the newest comment, not the whole thread, and one run
// described in isolation is how an outage reads as an incident (#151). Source
// inspection rather than execution — the streak read is a live `gh issue view`
// on the dedup'd issue, which has no seam to stub without faking the whole
// escalation flow.
func TestEscalationStatesTheStreak(t *testing.T) {
	src := readFileString(t, escalatePathFor(t))

	for _, want := range []string{"This is failure", "unbroken sequence since", "createdAt,comments"} {
		if !strings.Contains(src, want) {
			t.Errorf("%s no longer contains %q — a repeat failure must announce its streak in its own body instead of leaving the count to be reconstructed from the thread (#151)", escalateScript, want)
		}
	}

	// The count must key on the dedup marker, not on raw comment count: human
	// triage comments on the same thread are not failures.
	if !strings.Contains(src, `select(.body | contains($m))`) {
		t.Errorf("%s does not count prior failures by the dedup marker — counting every comment inflates the streak with human triage notes", escalateScript)
	}
}
