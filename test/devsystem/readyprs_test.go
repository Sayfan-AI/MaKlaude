// Tests for `issues.sh ready-prs` — the state-derived visibility for a PR that
// is finished and merely unmerged.
//
// Why this exists: `genesis-merge.yml` auto-merges `genesis-dev-bot[bot]` PRs
// only, and that restriction is deliberate — the repo is public, `pull_request`
// fires for fork PRs from anyone, and auto-merging "any green PR" would let an
// arbitrary contributor land on main unreviewed. The documented consequence is
// that a human-authored PR is merged by the *orchestrator* on a following run,
// one run late by design.
//
// The lag is by design. Permanent invisibility was not: that merge depended on a
// run happening to notice the PR, and no section of `summary` put it there. A
// human PR whose tracking issue is closed early, or that has no linked issue at
// all, was unreachable from the report every run reads. It is the last member of
// the invisible-nothing-happened class — a gate that waits forever (#84), a
// triage event that is dropped (#100), a run that dies mid-task (#97/#106/#110)
// — all of which share the property that nothing errors, so nothing is seen.
//
// Because this prints on EVERY tick, the exclusions below carry more weight than
// the detections: a backstop that cries wolf is one the orchestrator learns to
// skip, which is the lesson red-prs was built around. Each false-positive case
// here is a way "ready" could be claimed for a PR that is not.
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

type stubAuthor struct {
	Login string `json:"login"`
	IsBot bool   `json:"is_bot"`
}

type stubLabel struct {
	Name string `json:"name"`
}

// readyStubPR carries the fields ready-prs filters on. It is a superset of the
// red-prs stub so one fixture can be fed to both commands — see
// TestReadyAndRedPRsAreDisjoint.
type readyStubPR struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	HeadRef   string      `json:"headRefName"`
	IsDraft   bool        `json:"isDraft"`
	UpdatedAt string      `json:"updatedAt"`
	Rollup    []stubCheck `json:"statusCheckRollup"`
	Mergeable string      `json:"mergeable"`
	MergeSt   string      `json:"mergeStateStatus"`
	Labels    []stubLabel `json:"labels"`
	Author    stubAuthor  `json:"author"`
}

// readyPR builds the PR this report exists for: human-authored, not draft, all
// checks green, MERGEABLE/CLEAN, unlabeled. Every exclusion case below is this
// same PR with exactly one field spoiled, so a test that fails names the single
// predicate that broke.
func readyPR(number int, title string, ageDays int) readyStubPR {
	return readyStubPR{
		Number:    number,
		Title:     title,
		HeadRef:   "branch-for-pr",
		UpdatedAt: time.Now().UTC().AddDate(0, 0, -ageDays).Format(time.RFC3339),
		Rollup:    []stubCheck{{Name: "CI", Conclusion: "SUCCESS"}},
		Mergeable: "MERGEABLE",
		MergeSt:   "CLEAN",
		Author:    stubAuthor{Login: "a-human"},
	}
}

// runIssuesPRCmd runs an issues.sh PR subcommand against a stubbed PR list.
func runIssuesPRCmd(t *testing.T, sub string, prs []readyStubPR) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(prGhStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	// A nil slice must marshal to `[]`, not `null` — json.load would otherwise
	// hand the script None and the test would pass for the wrong reason.
	if prs == nil {
		prs = []readyStubPR{}
	}
	b, err := json.Marshal(prs)
	if err != nil {
		t.Fatalf("marshal prs: %v", err)
	}
	prsPath := filepath.Join(dir, "prs.json")
	if err := os.WriteFile(prsPath, b, 0o644); err != nil {
		t.Fatalf("write prs: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), sub)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_PRS="+prsPath,
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issues.sh %s failed: %v\n%s", sub, err, out)
	}
	return string(out)
}

func runReadyPRs(t *testing.T, prs []readyStubPR) string {
	t.Helper()
	return runIssuesPRCmd(t, "ready-prs", prs)
}

// TestReadyPRsDetectsAMergeablePR is the positive case: the live instance that
// motivated this — human-authored, three green checks, MERGEABLE/CLEAN, no
// needs:human — must be reported, with its age, so staleness needs no judgment.
func TestReadyPRsDetectsAMergeablePR(t *testing.T) {
	p := readyPR(111, "Classify how a run died", 4)
	p.Rollup = []stubCheck{
		{Name: "Build, lint & test", Conclusion: "SUCCESS"},
		{Name: "e2e on kind", Conclusion: "SUCCESS"},
		{Context: "legacy/status", State: "SUCCESS"}, // StatusContext shape
	}

	out := runReadyPRs(t, []readyStubPR{p})
	for _, want := range []string{"#111", "ready 4d", "Classify how a run died", "branch-for-pr"} {
		if !strings.Contains(out, want) {
			t.Errorf("ready-prs output missing %q:\n%s", want, out)
		}
	}
}

// TestReadyPRsExcludesNotActuallyReady is the false-positive guard, and the
// point of the whole file. This section prints every tick; anything it lists
// that is not one merge away is noise, and noise is what gets a backstop
// ignored. Each case spoils exactly one field of an otherwise-ready PR.
func TestReadyPRsExcludesNotActuallyReady(t *testing.T) {
	spoil := func(f func(*readyStubPR)) readyStubPR {
		p := readyPR(42, "otherwise ready", 1)
		f(&p)
		return p
	}

	cases := []struct {
		name string
		why  string
		in   readyStubPR
	}{
		{"draft", "a draft is not offered for merge at all",
			spoil(func(p *readyStubPR) { p.IsDraft = true })},

		{"checks pending", "an empty verdict is a check still in flight, not a passing one",
			spoil(func(p *readyStubPR) { p.Rollup = []stubCheck{{Name: "CI"}} })},

		{"one check pending among green", "ready means ALL checks are in, not most",
			spoil(func(p *readyStubPR) {
				p.Rollup = []stubCheck{{Name: "CI", Conclusion: "SUCCESS"}, {Name: "e2e"}}
			})},

		{"no checks yet", "CI has not reported; nothing has been verified",
			spoil(func(p *readyStubPR) { p.Rollup = nil })},

		{"failing check", "this PR belongs to red-prs and must not be called ready",
			spoil(func(p *readyStubPR) {
				p.Rollup = []stubCheck{{Name: "CI", Conclusion: "FAILURE"}}
			})},

		{"errored check", "same as failing — a non-SUCCESS conclusion is not ready",
			spoil(func(p *readyStubPR) {
				p.Rollup = []stubCheck{{Name: "CI", Conclusion: "ERROR"}}
			})},

		{"behind base", "needs a rebase, not a merge",
			spoil(func(p *readyStubPR) { p.MergeSt = "BEHIND" })},

		{"blocked", "needs a review or a required check, not a merge",
			spoil(func(p *readyStubPR) { p.MergeSt = "BLOCKED" })},

		{"dirty", "has conflicts; merging is impossible",
			spoil(func(p *readyStubPR) { p.MergeSt = "DIRTY" })},

		{"merge state unknown", "GitHub has not computed mergeability; do not guess ready",
			spoil(func(p *readyStubPR) { p.MergeSt = "UNKNOWN" })},

		{"conflicting", "MERGEABLE is a separate axis from CLEAN and both must hold",
			spoil(func(p *readyStubPR) { p.Mergeable = "CONFLICTING" })},

		{"needs:human", "a person is deliberately holding it; that outranks computed state",
			spoil(func(p *readyStubPR) { p.Labels = []stubLabel{{Name: "needs:human"}} })},

		{"bot author via is_bot", "genesis-merge.yml owns bot PRs; listing them races the auto-merger",
			spoil(func(p *readyStubPR) {
				p.Author = stubAuthor{Login: "genesis-dev-bot", IsBot: true}
			})},

		{"bot author via login suffix", "same, when the payload carries the [bot] login instead",
			spoil(func(p *readyStubPR) {
				p.Author = stubAuthor{Login: "genesis-dev-bot[bot]"}
			})},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := runReadyPRs(t, []readyStubPR{c.in}); strings.TrimSpace(out) != "" {
				t.Errorf("%s PR reported as ready to merge (%s) — this prints every tick, so a false positive here is a false alarm every tick:\n%s",
					c.name, c.why, out)
			}
		})
	}
}

// TestReadyPRsEmptyWhenNoPRs keeps "no output" meaning "nothing waiting" for
// callers, including the unconditional summary section.
func TestReadyPRsEmptyWhenNoPRs(t *testing.T) {
	if out := runReadyPRs(t, nil); strings.TrimSpace(out) != "" {
		t.Errorf("expected no output with zero open PRs, got:\n%s", out)
	}
}

// TestReadyPRsOrdersStalestFirst — a PR sitting green for days is the one being
// forgotten, so it leads. Matches gates and red-prs ordering.
func TestReadyPRsOrdersStalestFirst(t *testing.T) {
	out := runReadyPRs(t, []readyStubPR{
		readyPR(31, "fresh", 0),
		readyPR(32, "ancient", 9),
	})
	if i, j := strings.Index(out, "#32"), strings.Index(out, "#31"); i < 0 || j < 0 || i > j {
		t.Errorf("expected #32 (ready 9d) before #31 (ready 0d):\n%s", out)
	}
	if !strings.Contains(out, "ready 9d") {
		t.Errorf("expected the age to be reported so staleness needs no LLM judgment:\n%s", out)
	}
}

// TestReadyAndRedPRsAreDisjoint pins the invariant that keeps the two reports
// from contradicting each other: no PR may be both "triage this failure" and
// "merging is the only step left". Requiring every check to have concluded
// SUCCESS (rather than merely not-failed) is what makes it hold, so this fails
// if that predicate is ever loosened to "no failures".
func TestReadyAndRedPRsAreDisjoint(t *testing.T) {
	mixedFailure := readyPR(51, "one red among green", 2)
	mixedFailure.Rollup = []stubCheck{
		{Name: "CI", Conclusion: "SUCCESS"},
		{Name: "e2e", Conclusion: "FAILURE"},
	}
	// mergeStateStatus stays CLEAN here on purpose: GitHub reports CLEAN when no
	// branch protection requires the failing check, so "CLEAN" alone would let a
	// red PR through. The check verdicts have to be read independently.
	prs := []readyStubPR{
		readyPR(50, "fully green", 1),
		mixedFailure,
		func() readyStubPR {
			p := readyPR(52, "all red", 3)
			p.Rollup = []stubCheck{{Name: "CI", Conclusion: "TIMED_OUT"}}
			return p
		}(),
	}

	red := runIssuesPRCmd(t, "red-prs", prs)
	ready := runIssuesPRCmd(t, "ready-prs", prs)

	for _, n := range []string{"#50", "#51", "#52"} {
		inRed, inReady := strings.Contains(red, n), strings.Contains(ready, n)
		if inRed && inReady {
			t.Errorf("%s appears in BOTH red-prs and ready-prs — the reports must partition, not overlap\nred:\n%s\nready:\n%s", n, red, ready)
		}
	}
	if !strings.Contains(ready, "#50") {
		t.Errorf("the fully green PR should be ready:\n%s", ready)
	}
	for _, n := range []string{"#51", "#52"} {
		if !strings.Contains(red, n) {
			t.Errorf("%s has a failing check and should be red:\n%s", n, red)
		}
	}
}

// TestSummaryReportsReadyPRs is the membership half: the command existing is
// useless if the state assessment every run reads does not print it. That was
// the entire defect — the merge depended on a run happening to notice. The
// section must be unconditional, same as Human Gates (#84) and Red PRs (#100).
func TestSummaryReportsReadyPRs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	summary := src[strings.Index(src, "    summary)"):]
	if end := strings.Index(summary, "\n    close)"); end > 0 {
		summary = summary[:end]
	}
	if !strings.Contains(summary, "Ready to Merge") || !strings.Contains(summary, "format_ready_prs") {
		t.Errorf("issues.sh `summary` does not print ready-to-merge PRs — a finished PR would then be invisible again:\n%s", summary)
	}
	// Unconditional means no `if`/`[ ... ]` wrapper suppressing it when empty:
	// an empty section is the all-clear signal and has to be printed to mean it.
	idx := strings.Index(summary, "Ready to Merge")
	tail := summary[idx:]
	if end := strings.Index(tail, "format_ready_prs"); end > 0 {
		if between := tail[:end]; strings.Contains(between, "if ") {
			t.Errorf("Ready to Merge section is conditional; it must print every run:\n%s", between)
		}
	}
}

// TestReadyPRsIsDocumented — the orchestrator discovers subcommands from
// `--help`; one that is not listed is one that is not used.
func TestReadyPRsIsDocumented(t *testing.T) {
	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "--help")
	out, _ := cmd.CombinedOutput() // usage exits 1 by design
	if !strings.Contains(string(out), "ready-prs") {
		t.Errorf("issues.sh usage does not document ready-prs:\n%s", out)
	}
}
