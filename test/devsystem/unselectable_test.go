// Tests for `issues.sh unselectable-work` — the state-derived visibility for
// work that exists and that the loop's own selection rules can never choose.
//
// Why this exists (#204). `summary` prints five other unconditional sections and
// every one of them keys on a state that needs someone to ACT: it failed, it's
// blocked, it's gated, it's red, nobody answered, it's mergeable. An issue filed
// with no `milestone:N` label is in none of those states. It shows up under Open
// Issues looking completely healthy — and it is unreachable, because the
// orchestrator's hard rules say milestone work outranks discretionary work and
// that a discretionary finding is "filed and moved on" from. Three hit this in
// one day: #167 dropped out of M5 when the label came off and sat until a human
// re-added it, and #186 and #202 were filed unmilestoned, #202 becoming
// selectable only because a human labelled it by hand minutes before filing
// #204.
//
// So the missing category is not another state of distress. It is
// SELECTABILITY, which is the same move this repo made after #141 — "a safety
// net can only see what it queries; before adding another one, ask what KIND of
// thing the existing nets all query, because the gap is usually a whole category
// rather than another instance." That time the category was an utterance. This
// time it is reachability.
//
// It also cannot be caught by sharper judgment, which is why it is a detector
// rather than a guideline: the evidence is an ABSENCE spread over days — no run
// failed, no check went red, nothing looped — and no single agent cycle holds the
// history to notice "this has been unselectable since Tuesday". That is exactly
// why the 21-day stale gate (#84) went unnoticed until a nudge fired by chance,
// and the answer there was a deterministic check.
//
// Because this prints on EVERY tick, the exclusions below carry more weight than
// the detections (#112): a backstop that cries wolf is one the orchestrator
// learns to skip. Each exclusion is its own test, in the shape of
// unanswered_test.go, so a failure names the single predicate that broke.
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

// unselGhStub answers the two `gh issue list` calls unselectable-work makes —
// the open set and the closed set — and exits non-zero on anything else, so a
// silent change in how the script queries GitHub surfaces as a test failure
// rather than as an empty section that reads like all-clear. It also asserts the
// requested `--json` fields, because dropping `labels` or `createdAt` would make
// every issue look reachable.
const unselGhStub = `#!/usr/bin/env python3
import os, sys
argv = sys.argv[1:]
if argv[:2] != ["issue", "list"]:
    sys.stderr.write("stub gh: unsupported call: %s\n" % argv)
    sys.exit(64)
state = None
fields = ""
for i, a in enumerate(argv):
    if a == "--state" and i + 1 < len(argv):
        state = argv[i + 1]
    if a == "--json" and i + 1 < len(argv):
        fields = argv[i + 1]
for required in ("number", "title", "labels", "createdAt"):
    if required not in fields:
        sys.stderr.write("stub gh: --json is missing %s: %r\n" % (required, fields))
        sys.exit(64)
if os.environ.get("STUB_ISSUES_FAIL") == "1":
    sys.stderr.write("stub gh: simulated API failure\n")
    sys.exit(1)
path = {"open": os.environ.get("STUB_OPEN"), "closed": os.environ.get("STUB_CLOSED")}.get(state)
if not path:
    sys.stderr.write("stub gh: unsupported --state %r\n" % state)
    sys.exit(64)
with open(path) as f:
    sys.stdout.write(f.read())
`

// unselStubIssue carries the fields unselectable-work filters on. stubLabel is
// shared with readyprs_test.go — same package, one shape for a label.
type unselStubIssue struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	State     string      `json:"state"`
	Labels    []stubLabel `json:"labels"`
	CreatedAt string      `json:"createdAt"`
}

// unselIssue builds an open issue aged ageDays with the given labels. Every case
// below is this same issue with one label added or removed, so a failing test
// names the single predicate at fault.
func unselIssue(number int, title string, ageDays int, labels ...string) unselStubIssue {
	i := unselStubIssue{
		Number:    number,
		Title:     title,
		State:     "OPEN",
		CreatedAt: time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339),
	}
	for _, l := range labels {
		i.Labels = append(i.Labels, stubLabel{Name: l})
	}
	return i
}

// completionGate is the closed "Milestone N complete" sign-off the detector reads
// to decide a milestone has been left behind. Title wording matches the
// orchestrator's hard rule and the live #182.
func completionGate(number int, milestone string) unselStubIssue {
	i := unselIssue(number, "Milestone "+milestone+" complete — a signed-off milestone", 30, "needs:human", "milestone:"+milestone)
	i.State = "CLOSED"
	return i
}

// runUnselectable runs `issues.sh unselectable-work` against stubbed open and
// closed issue lists. It fails the test on a non-zero exit: this section is
// printed unconditionally by `summary`, so a command that can fail would take the
// whole state assessment down with it.
func runUnselectable(t *testing.T, open, closed []unselStubIssue) string {
	t.Helper()
	return runUnselectableEnv(t, open, closed, nil)
}

func runUnselectableEnv(t *testing.T, open, closed []unselStubIssue, extraEnv []string) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(unselGhStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	write := func(name string, issues []unselStubIssue) string {
		// A nil slice must marshal to `[]`, not `null` — json.loads would
		// otherwise hand the script None and the test would pass for the wrong
		// reason.
		if issues == nil {
			issues = []unselStubIssue{}
		}
		b, err := json.Marshal(issues)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "unselectable-work")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_OPEN="+write("open.json", open),
		"STUB_CLOSED="+write("closed.json", closed),
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issues.sh unselectable-work failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestUnselectableDetectsAnUnmilestonedIssue is the live instance that motivated
// this: #202 was filed as a real bug, carried no milestone:N label, and could
// not be selected by any run until a human labelled it by hand. It must be
// reported with its age, so "how long has this been abandoned" needs no
// judgment, and with a reason that says what to do about it.
func TestUnselectableDetectsAnUnmilestonedIssue(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(202, "issues.sh label silently drops all but the last --add", 4, "bug")},
		nil)

	for _, want := range []string{"#202", "open 4d", "issues.sh label silently drops", "no milestone:N label"} {
		if !strings.Contains(out, want) {
			t.Errorf("unselectable-work output missing %q:\n%s", want, out)
		}
	}
}

// TestUnselectableDetectsWorkOnASignedOffMilestone covers the adjacent case: the
// label is present, but its milestone has been signed off, so the loop has moved
// on and nothing will come back for it. The report names the gate that closed
// the milestone, because that is the fact a reader needs to decide between
// re-milestoning the issue and closing it.
func TestUnselectableDetectsWorkOnASignedOffMilestone(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(167, "Trust should expire on invalidation", 9, "enhancement", "milestone:5")},
		[]unselStubIssue{completionGate(182, "5")})

	for _, want := range []string{"#167", "open 9d", "milestone:5", "signed off", "#182"} {
		if !strings.Contains(out, want) {
			t.Errorf("unselectable-work output missing %q:\n%s", want, out)
		}
	}
}

// TestUnselectableIgnoresAnActiveMilestone is the ordinary case and by far the
// most common: a task issue on the milestone in progress is exactly what the
// loop is supposed to select. Reporting it would make the section noise on every
// tick, which is the failure mode #112 warns about.
func TestUnselectableIgnoresAnActiveMilestone(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(196, "T7 — The narrowed no-writes guarantee", 1, "documentation", "milestone:6")},
		[]unselStubIssue{completionGate(182, "5")})

	if strings.TrimSpace(out) != "" {
		t.Errorf("an issue on the ACTIVE milestone was reported as unselectable:\n%s", out)
	}
}

// TestUnselectableTreatsAnOpenCompletionGateAsActive pins the direction of the
// sign-off test. A completion gate that is still OPEN means the milestone is not
// signed off — and the hard rules go further: human-added work on an active
// milestone explicitly pauses that gate. Only a CLOSED gate may make its
// milestone's issues unreachable.
func TestUnselectableTreatsAnOpenCompletionGateAsActive(t *testing.T) {
	gate := completionGate(205, "6")
	gate.State = "OPEN"

	out := runUnselectable(t,
		[]unselStubIssue{
			unselIssue(197, "T8 — End-to-end chaos scenario on kind in CI", 1, "enhancement", "milestone:6"),
			gate,
		},
		nil)

	if strings.Contains(out, "#197") {
		t.Errorf("an OPEN completion gate was treated as a sign-off, so live milestone work was reported as unreachable:\n%s", out)
	}
}

// TestUnselectableExcludesHumanGates: a needs:human issue is deliberately outside
// the milestone task flow — a plan gate, a completion gate, or an
// automation:failure escalation (escalate.sh applies both labels, so this one
// exemption covers both). It is also already printed unconditionally under Human
// Gates with its age, so reporting it here would double-report the one thing a
// person is already looking at.
func TestUnselectableExcludesHumanGates(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{
			unselIssue(188, "Milestone 6 plan — Chaos engineering", 2, "needs:human"),
			unselIssue(150, "A genesis run failed", 5, "needs:human", "automation:failure"),
		},
		nil)

	if strings.TrimSpace(out) != "" {
		t.Errorf("a needs:human gate was reported as unselectable — it is already in the Human Gates section:\n%s", out)
	}
}

// TestUnselectableExcludesOnboarding: issue #1 produced the milestone roadmap, so
// it predates every milestone by construction and can never carry one. Reporting
// it would be a permanent false positive on the one issue that legitimately has
// no milestone.
func TestUnselectableExcludesOnboarding(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(1, "Onboarding: MaKlaude", 20, "genesis:onboarding")},
		nil)

	if strings.TrimSpace(out) != "" {
		t.Errorf("the onboarding issue was reported as unselectable:\n%s", out)
	}
}

// TestUnselectableExcludesDeclinedWork: "we are deliberately not doing this" is a
// legitimate third answer to why an issue carries no milestone, alongside "it was
// forgotten" and "it is scheduled". Without this exemption the only way to
// silence a true-but-unhelpful report is to close the issue, which loses the
// record of the decision.
func TestUnselectableExcludesDeclinedWork(t *testing.T) {
	for _, label := range []string{"wontfix", "duplicate", "invalid"} {
		t.Run(label, func(t *testing.T) {
			out := runUnselectable(t,
				[]unselStubIssue{unselIssue(77, "A finding we decided against", 12, "bug", label)},
				nil)
			if strings.TrimSpace(out) != "" {
				t.Errorf("an issue labelled %q was reported as unselectable:\n%s", label, out)
			}
		})
	}
}

// TestUnselectableIgnoresClosedIssues: only open work can be abandoned. A closed
// unmilestoned issue is a finished decision, and the closed set is read solely to
// find the completion gates.
func TestUnselectableIgnoresClosedIssues(t *testing.T) {
	done := unselIssue(185, "The disclosure artifact redacts the fix fingerprint", 6, "bug")
	done.State = "CLOSED"

	out := runUnselectable(t, nil, []unselStubIssue{done})

	if strings.TrimSpace(out) != "" {
		t.Errorf("a CLOSED issue was reported as unselectable:\n%s", out)
	}
}

// TestUnselectableReportsEvolverRoutedWork records a deliberate NON-exemption.
// `needs:evolver` routes a finding to the framework; it does not make the local
// half selectable. #204 is the case in point — it carries that label and a
// milestone, while #202 sat unmilestoned carrying exactly that kind of
// framework-facing finding and was abandoned.
func TestUnselectableReportsEvolverRoutedWork(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(204, "The evolver has no signal for unselectable work", 3, "bug", "needs:evolver")},
		nil)

	if !strings.Contains(out, "#204") {
		t.Errorf("a needs:evolver issue with no milestone was NOT reported; routing a finding upstream does not make the local half selectable:\n%s", out)
	}
}

// TestUnselectableKeepsMultiMilestoneWorkSelectable: an issue that outlived one
// milestone and was re-labelled onto the next is reachable through the live one.
// One active milestone is enough.
func TestUnselectableKeepsMultiMilestoneWorkSelectable(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(186, "Backport host-guard.sh", 7, "enhancement", "milestone:5", "milestone:6")},
		[]unselStubIssue{completionGate(182, "5")})

	if strings.TrimSpace(out) != "" {
		t.Errorf("an issue carrying one signed-off and one ACTIVE milestone was reported as unreachable:\n%s", out)
	}
}

// TestUnselectableOrdersStalestFirst: same ordering as gates, red-prs, ready-prs
// and unanswered-comments. The item that has been unreachable longest is the one
// being forgotten, so it goes at the top.
func TestUnselectableOrdersStalestFirst(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{
			unselIssue(300, "Filed yesterday", 1, "bug"),
			unselIssue(301, "Filed three weeks ago", 21, "bug"),
			unselIssue(302, "Filed last week", 7, "bug"),
		},
		nil)

	order := []string{"#301", "#302", "#300"}
	at := make([]int, len(order))
	for i, n := range order {
		at[i] = strings.Index(out, n)
		if at[i] < 0 {
			t.Fatalf("output missing %s:\n%s", n, out)
		}
	}
	for i := 1; i < len(at); i++ {
		if at[i-1] > at[i] {
			t.Errorf("output is not stalest-first (%s before %s):\n%s", order[i], order[i-1], out)
		}
	}
}

// TestUnselectableEmptyMeansAllClear: nothing to report must print nothing and
// exit zero, which is what lets the `summary` section be unconditional.
func TestUnselectableEmptyMeansAllClear(t *testing.T) {
	out := runUnselectable(t, nil, nil)
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected no output when every open issue is reachable, got:\n%s", out)
	}
}

// TestUnselectableSaysSoWhenTheIssueListCannotBeRead: an unreadable API must not
// look like all-clear. Silence is the whole contract of this section, so a failed
// read has to say it did not run — the same property unanswered-comments pins.
func TestUnselectableSaysSoWhenTheIssueListCannotBeRead(t *testing.T) {
	out := runUnselectableEnv(t,
		[]unselStubIssue{unselIssue(202, "An unmilestoned bug", 4, "bug")},
		nil,
		[]string{"STUB_ISSUES_FAIL=1"})

	if !strings.Contains(out, "did not run") {
		t.Errorf("a failed issue-list read printed no warning, so an empty section reads as all-clear:\n%s", out)
	}
}

// TestSummaryPrintsUnselectableWork is the membership half: the detector is
// worthless if the state assessment every run reads does not print it. Same
// guard shape as red-prs, ready-prs, unanswered-comments and failure-streaks.
func TestSummaryPrintsUnselectableWork(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	summary := src[strings.Index(src, "    summary)"):]
	if end := strings.Index(summary, "\n    close)"); end > 0 {
		summary = summary[:end]
	}
	if !strings.Contains(summary, "Unselectable Work") || !strings.Contains(summary, "format_unselectable_work") {
		t.Errorf("issues.sh `summary` does not print unselectable work — an unmilestoned issue would then look healthy under Open Issues again, which is how #167, #186 and #202 were abandoned:\n%s", summary)
	}
}
