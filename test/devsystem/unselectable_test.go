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
	"regexp"
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
for required in ("number", "title", "labels", "createdAt", "updatedAt"):
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
	UpdatedAt string      `json:"updatedAt"`
}

// unselIssue builds an open issue aged ageDays with the given labels, untouched
// since it was filed. Every case below is this same issue with one label added or
// removed, so a failing test names the single predicate at fault.
func unselIssue(number int, title string, ageDays int, labels ...string) unselStubIssue {
	filed := time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339)
	i := unselStubIssue{
		Number:    number,
		Title:     title,
		State:     "OPEN",
		CreatedAt: filed,
		UpdatedAt: filed,
	}
	for _, l := range labels {
		i.Labels = append(i.Labels, stubLabel{Name: l})
	}
	return i
}

// touched marks an issue as having had activity hoursAgo — a comment, a linked
// PR, a label change. This is what separates a claim a live run just made from
// one that outlived the run that made it, and it is the only thing standing
// between the stale-label half of the detector and reporting the whole in-flight
// board on every tick.
func touched(i unselStubIssue, hoursAgo int) unselStubIssue {
	i.UpdatedAt = time.Now().UTC().Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339)
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
	return runUnselectableScript(t,
		filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), open, closed, extraEnv)
}

// runUnselectableScript is the same harness against a named copy of the script,
// so TestSelectionExclusionsHaveOneDefinition can patch the exclusion list and
// observe both readers. It never touches the repo's own copy: `genesis serve`
// runs against this working tree, and a run that called issues.sh during the
// mutation window would silently mislabel the board (same reasoning as ISSUES_SH
// in issueselect_test.go).
func runUnselectableScript(t *testing.T, script string, open, closed []unselStubIssue, extraEnv []string) string {
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

	cmd := exec.Command("bash", script, "unselectable-work")
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

// TestUnselectableDetectsAStaleBlockedLabel is the first half of #216. A
// milestone label is necessary for selection and not sufficient: `next` also
// refuses `blocked`, so an issue carrying `milestone:6` plus `blocked` is
// selectable by the milestone test and refused by the selector. Nothing removes
// the label when the blocker closes, and three issues hit that in one night — T5
// #194, T6 #195 and T8 #197 all kept a `blocked` label past their blockers and a
// human removed each by hand. The reason must name the label, because "remove
// this label" is the whole action.
func TestUnselectableDetectsAStaleBlockedLabel(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(197, "T8 — End-to-end chaos scenario on kind in CI", 2, "enhancement", "blocked", "milestone:6")},
		nil)

	for _, want := range []string{"#197", "`blocked`", "untouched for", "next"} {
		if !strings.Contains(out, want) {
			t.Errorf("a stale `blocked` label on an ACTIVE milestone was not reported with %q — the selector refuses it and no section printed it:\n%s", want, out)
		}
	}
}

// TestUnselectableDetectsAStaleClaim is the variant with no human applier to
// notice, and the one that was live when #216 was filed: `in-progress` is written
// by `claim` at pickup and NOTHING removes it if the run that claimed the issue
// dies. Sessions ending at error_max_turns are routine here (see the turn-budget
// bullets in CLAUDE.md), so every later run then skips the issue as "someone is
// on this" while no net contradicts it. T6 #195 sat exactly this way: claimed,
// zero comments, no branch, no PR.
func TestUnselectableDetectsAStaleClaim(t *testing.T) {
	out := runUnselectable(t,
		[]unselStubIssue{unselIssue(195, "T6 — Correctness scoring", 1, "enhancement", "in-progress", "milestone:6")},
		[]unselStubIssue{completionGate(182, "5")})

	for _, want := range []string{"#195", "`in-progress`", "untouched for"} {
		if !strings.Contains(out, want) {
			t.Errorf("a claim that outlived the run holding it was not reported with %q:\n%s", want, out)
		}
	}
}

// TestUnselectableIgnoresWorkInFlight is the negative control the stale-label
// half needs, and it carries as much weight as the detections (#112): these
// labels are LEGITIMATE while something is happening. A run working an issue
// touches it within minutes — a comment, a linked PR — and a `blocked` label
// applied an hour ago is an agent recording a dependency it just found. Reporting
// either would print the in-flight board on every tick, and an over-broad net is
// skipped just as fast as an empty one.
func TestUnselectableIgnoresWorkInFlight(t *testing.T) {
	for _, label := range []string{"blocked", "in-progress"} {
		t.Run(label, func(t *testing.T) {
			out := runUnselectable(t,
				[]unselStubIssue{touched(unselIssue(216, "A task a live run is working", 3, "bug", label, "milestone:6"), 1)},
				nil)
			if strings.TrimSpace(out) != "" {
				t.Errorf("an issue touched an hour ago under %q was reported as unselectable:\n%s", label, out)
			}
		})
	}
}

// TestUnselectableStaleWindowIsConfigurable pins the threshold as a knob rather
// than a constant baked into the reasoning, in the shape of --stale-days on gates
// and --window-days on unanswered-comments. A repo whose sessions are capped
// higher than this one's 3600s needs a wider window, and the floor below is why
// that matters rather than being a preference.
func TestUnselectableStaleWindowIsConfigurable(t *testing.T) {
	claimed := []unselStubIssue{touched(unselIssue(195, "T6 — Correctness scoring", 3, "enhancement", "in-progress", "milestone:6"), 1)}

	if out := runUnselectable(t, claimed, nil); strings.TrimSpace(out) != "" {
		t.Errorf("a 1h-old claim was reported under the default window:\n%s", out)
	}
	out := runUnselectableEnv(t, claimed, nil, []string{"GENESIS_CLAIM_STALE_HOURS=0.5"})
	if !strings.Contains(out, "#195") {
		t.Errorf("a 1h-old claim was NOT reported with GENESIS_CLAIM_STALE_HOURS=0.5:\n%s", out)
	}
}

// TestUnselectableDefaultWindowClearsTheSessionCap is the floor the default has
// to respect, and it is the half of #216 the human's live instance sharpened. A
// claim younger than the control plane's session cap can still belong to a
// running session — T6 #195's holder was terminated on `Session timeout (3600s
// total)` — so a window at or below one hour reports live work as abandoned. The
// ceiling comes from the same instance in the other direction: the human removed
// that label by hand about 75 minutes after the claim, so a window they beat
// reports nothing anybody needed. This asserts the floor, since that is the side
// where being wrong is a false accusation rather than a missed report.
func TestUnselectableDefaultWindowClearsTheSessionCap(t *testing.T) {
	const sessionCapHours = 1 // `Session timeout (3600s total)` — see DEFAULT_CLAIM_STALE_HOURS

	out := runUnselectable(t,
		[]unselStubIssue{touched(unselIssue(195, "T6 — Correctness scoring", 3, "enhancement", "in-progress", "milestone:6"), sessionCapHours)},
		nil)
	if strings.TrimSpace(out) != "" {
		t.Errorf("a claim exactly as old as the %dh session cap was reported as stale — the default window does not clear the cap, so a live session's issue is named as abandoned:\n%s", sessionCapHours, out)
	}
}

// TestSelectionExclusionsHaveOneDefinition is the property the rest of this file
// cannot express by adding cases, and it is the actual defect #216 reported: the
// selector and the net each decided what "selectable" means, and they decided it
// differently. Adding `blocked` and `in-progress` to the net's reason list would
// have fixed the symptom and left one system holding two definitions of one word,
// so the NEXT label added to the selector diverges again, silently, with the net
// still green.
//
// So the test is drift, not content: patch a fourth label into the single
// SELECTION_EXCLUSIONS list and require BOTH readers to honor it — the selector
// must refuse an issue carrying it (exit 3, nothing to work) and the net must
// report that issue. A reimplementation in either half fails this without anyone
// having to remember the other half exists.
func TestSelectionExclusionsHaveOneDefinition(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	decl := regexp.MustCompile(`(?m)^SELECTION_EXCLUSIONS=\(([^)]*)\)$`)
	if got := len(decl.FindAllString(src, -1)); got != 1 {
		t.Fatalf("issues.sh has %d SELECTION_EXCLUSIONS declarations, want exactly 1 — the selector and the unselectable-work net drift apart the moment there is more than one list (#216)", got)
	}
	patched := decl.ReplaceAllString(src, "SELECTION_EXCLUSIONS=(needs:review ${1})")

	script := filepath.Join(t.TempDir(), "issues.sh")
	if err := os.WriteFile(script, []byte(patched), 0o755); err != nil {
		t.Fatalf("write patched issues.sh: %v", err)
	}

	// Selector half: `next` must refuse the new label. Exit 3 is "nothing to
	// work", which is a distinct outcome from an error.
	t.Setenv("ISSUES_SH", script)
	filed := time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)
	r := runIssues(t, map[string]string{
		"GH_LIST_JSON": `[{"number":900,"createdAt":"` + filed + `","labels":[{"name":"needs:review"},{"name":"milestone:6"}]}]`,
	}, "next", "--milestone", "6")
	if r.code != 3 {
		t.Errorf("`next` selected an issue carrying a label that was added to SELECTION_EXCLUSIONS (exit %d, stdout %q) — the selector does not read the shared list", r.code, r.stdout)
	}

	// Net half: the same label must make the same issue visible, or work the
	// selector refuses is reported nowhere.
	out := runUnselectableScript(t, script,
		[]unselStubIssue{unselIssue(900, "Work no run can select", 2, "needs:review", "milestone:6")},
		nil, nil)
	if !strings.Contains(out, "#900") || !strings.Contains(out, "`needs:review`") {
		t.Errorf("unselectable-work did not report an issue carrying a label added to SELECTION_EXCLUSIONS — the net does not read the shared list:\n%s", out)
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
