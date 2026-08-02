// Tests for `issues.sh unanswered-comments` — the state-derived visibility for
// the one input no other safety net can see: a person having said something.
//
// Why this exists, measured on #141 (2026-08-02, UTC): a human posted an
// approval carrying two conditions at 06:29:18; PR #154 was merged on its own
// green checks at 06:31:43 and the issue closed at 06:31:44. The comment went
// unread for 2m25s and then the work it constrained shipped without two of the
// three negative cases it asked for. Nothing in the loop could have noticed —
// `genesis-merge.yml` gates on bot-author plus green checks and reads no
// comment, `ready-prs` sees only the `needs:human` label, and red-prs /
// stale-gates / escalate.sh / run-outcome.sh are all shaped around failure. This
// was not a failure: the PR was correct and correctly merged on the evidence its
// merger had.
//
// It is therefore the same invisible-nothing-happened class as a gate that waits
// forever (#84), a triage event dropped by supersession (#100), a run that dies
// mid-task (#97/#106/#110) and a green PR nobody merges (#112) — and it gets the
// same treatment: derived from repo state, printed unconditionally, empty means
// all-clear.
//
// Because it prints every tick, the exclusions below carry more weight than the
// detections, per #112. The dominant false-positive class in this repo is the
// closing note — "LGTM", "Milestone 4 signed off", "Closing." — a human comment
// that trails a thread precisely because the human finished with it. Replaying
// the repo's full comment history through this rule produced 18 such candidates
// and zero reports, which is the property TestUnansweredExcludesClosingNotes
// pins.
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

// commentGhStub answers the two `gh api` shapes unanswered-comments makes: the
// repo-wide comment feed, and one thread lookup per candidate. Anything else
// exits non-zero, so a silent change in how the script queries GitHub surfaces
// as a failure rather than as an empty section that reads like all-clear.
const commentGhStub = `#!/usr/bin/env python3
import json, os, re, sys
argv = sys.argv[1:]
if argv[:1] != ["api"] or len(argv) < 2:
    sys.stderr.write("stub gh: unsupported call: %s\n" % argv)
    sys.exit(64)
path = argv[1]
if "issues/comments" in path:
    if os.environ.get("STUB_COMMENTS_FAIL") == "1":
        sys.stderr.write("stub gh: simulated API failure\n")
        sys.exit(1)
    with open(os.environ["STUB_COMMENTS"]) as f:
        sys.stdout.write(f.read())
    sys.exit(0)
m = re.search(r"/issues/(\d+)$", path)
if m:
    with open(os.environ["STUB_THREADS"]) as f:
        threads = json.load(f)
    key = m.group(1)
    if key not in threads:
        sys.stderr.write("stub gh: no thread %s\n" % key)
        sys.exit(1)
    print(json.dumps(threads[key]))
    sys.exit(0)
sys.stderr.write("stub gh: unsupported api path: %s\n" % path)
sys.exit(64)
`

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type stubComment struct {
	IssueURL  string `json:"issue_url"`
	CreatedAt string `json:"created_at"`
	HTMLURL   string `json:"html_url"`
	User      ghUser `json:"user"`
}

type stubThread struct {
	Number      int               `json:"number"`
	Title       string            `json:"title"`
	State       string            `json:"state"`
	ClosedAt    *string           `json:"closed_at"`
	ClosedBy    *ghUser           `json:"closed_by"`
	PullRequest map[string]string `json:"pull_request,omitempty"`
}

var (
	human = ghUser{Login: "a-human", Type: "User"}
	bot   = ghUser{Login: "genesis-dev-bot[bot]", Type: "Bot"}
)

func stamp(minutesAgo int) string {
	return time.Now().UTC().Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339)
}

// comment builds one entry of the repo-wide feed.
func comment(number int, by ghUser, minutesAgo int) stubComment {
	return stubComment{
		IssueURL:  fmt.Sprintf("https://api.github.com/repos/owner/repo/issues/%d", number),
		CreatedAt: stamp(minutesAgo),
		HTMLURL:   fmt.Sprintf("https://github.com/owner/repo/issues/%d#issuecomment-%d", number, number*10),
		User:      by,
	}
}

// openThread is the thread this report exists for: still open, so a trailing
// human comment is unambiguously waiting on the loop.
func openThread(number int, title string) stubThread {
	return stubThread{Number: number, Title: title, State: "open"}
}

// closedOverHuman is the #141 shape and the ONLY closed shape that reports: the
// human spoke, and then the loop closed the thread on top of them.
func closedOverHuman(number int, title string, closedMinutesAgo int) stubThread {
	at := stamp(closedMinutesAgo)
	closer := bot
	return stubThread{Number: number, Title: title, State: "closed", ClosedAt: &at, ClosedBy: &closer}
}

func runUnanswered(t *testing.T, args []string, comments []stubComment, threads map[string]stubThread, failAPI bool) string {
	t.Helper()

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(commentGhStub), 0o755); err != nil {
		t.Fatalf("write gh stub: %v", err)
	}

	// A nil slice must marshal to `[]`, not `null` — the script's json parse
	// would otherwise get None and the test would pass for the wrong reason.
	if comments == nil {
		comments = []stubComment{}
	}
	if threads == nil {
		threads = map[string]stubThread{}
	}
	write := func(name string, v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	cmd := exec.Command("bash", append(
		[]string{filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "unanswered-comments"},
		args...)...)
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_COMMENTS="+write("comments.json", comments),
		"STUB_THREADS="+write("threads.json", threads),
		"GH_TOKEN=stub", "GH_REPO=owner/repo",
	)
	if failAPI {
		env = append(env, "STUB_COMMENTS_FAIL=1")
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("issues.sh unanswered-comments failed: %v\n%s", err, out)
	}
	return string(out)
}

func unanswered(t *testing.T, comments []stubComment, threads map[string]stubThread) string {
	t.Helper()
	return runUnanswered(t, nil, comments, threads, false)
}

// TestUnansweredDetectsATrailingHumanCommentOnAnOpenThread is the base positive
// case. The age is reported so "how long has this been ignored" needs no
// judgment, and the comment URL is printed because that is the actionable thing.
func TestUnansweredDetectsATrailingHumanCommentOnAnOpenThread(t *testing.T) {
	out := unanswered(t,
		[]stubComment{
			comment(141, bot, 200),
			comment(141, human, 150),
		},
		map[string]stubThread{"141": openThread(141, "T1 — wire the gated path into the binary")})

	for _, want := range []string{
		"#141",
		"unanswered 2h",
		"@a-human",
		"T1 — wire the gated path into the binary",
		"#issuecomment-1410",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unanswered-comments output missing %q:\n%s", want, out)
		}
	}
}

// TestUnansweredDetectsTheLoopClosingOverAHuman replays #141 itself: the human
// comment lands, and the loop closes the thread two minutes later. The close is
// what makes it invisible everywhere else — a closed issue appears in no other
// section of `summary` — so this is the case the whole report was built for, and
// it has to survive the close rather than be filtered by it.
func TestUnansweredDetectsTheLoopClosingOverAHuman(t *testing.T) {
	out := unanswered(t,
		[]stubComment{
			comment(141, bot, 75),   // "the last done criterion is implemented — PR #154"
			comment(141, human, 64), // the approval, carrying two conditions
		},
		map[string]stubThread{"141": closedOverHuman(141, "T1 — the conditions arrived late", 62)})

	if !strings.Contains(out, "#141") {
		t.Fatalf("the loop closing an issue over an unanswered human comment must be reported:\n%s", out)
	}
	// The note has to say the thread is closed, because that changes the action
	// from "reply" to "reopen or reply".
	if !strings.Contains(out, "reopen") {
		t.Errorf("a closed thread must be labelled as such so the reader knows reopening is required:\n%s", out)
	}
}

// TestUnansweredExcludesClosingNotes is the point of the file. This section
// prints every tick, and the dominant human-trailing-comment shape in this repo
// is not a condition at all — it is somebody finishing with a thread. Replaying
// the repo's real history produced 18 candidates of exactly these shapes and
// zero reports; if any case here starts printing, the section becomes noise and
// the orchestrator learns to skip the whole report.
func TestUnansweredExcludesClosingNotes(t *testing.T) {
	closedBy := func(u ghUser, closedMinutesAgo int) stubThread {
		at := stamp(closedMinutesAgo)
		return stubThread{Number: 42, Title: "otherwise reportable", State: "closed", ClosedAt: &at, ClosedBy: &u}
	}

	cases := []struct {
		name     string
		why      string
		comments []stubComment
		thread   stubThread
	}{
		{
			"bot replied last",
			"the loop has answered; nothing is waiting",
			[]stubComment{comment(42, human, 90), comment(42, bot, 30)},
			openThread(42, "otherwise reportable"),
		},
		{
			"comment posted after the close",
			`the "LGTM" / "signed off" shape — a person writing a closing note, not a condition`,
			[]stubComment{comment(42, human, 20)},
			closedBy(human, 25),
		},
		{
			"human closed their own thread",
			"a person who comments and then closes has answered themselves",
			[]stubComment{comment(42, human, 40)},
			closedBy(human, 30),
		},
		{
			"closed with no recorded closer",
			"cannot prove the loop closed over them, so do not claim it did",
			[]stubComment{comment(42, human, 40)},
			func() stubThread {
				at := stamp(30)
				return stubThread{Number: 42, Title: "otherwise reportable", State: "closed", ClosedAt: &at}
			}(),
		},
		{
			"older than the window",
			"a week-old comment nothing replied to was handled out of band or stopped mattering",
			[]stubComment{comment(42, human, 60*24*8)},
			openThread(42, "otherwise reportable"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := unanswered(t, c.comments, map[string]stubThread{"42": c.thread})
			if strings.TrimSpace(out) != "" {
				t.Errorf("%s was reported as unanswered (%s) — this prints every tick, so a false positive here is a false alarm every tick:\n%s",
					c.name, c.why, out)
			}
		})
	}
}

// TestUnansweredReadsTheNewestCommentNotTheFirst guards the ordering assumption.
// The feed is served newest-first, but the script re-establishes order itself:
// taking the wrong comment as "newest" inverts every verdict in the file, and it
// would do so silently. Both orderings of the same pair must agree.
func TestUnansweredReadsTheNewestCommentNotTheFirst(t *testing.T) {
	threads := map[string]stubThread{"7": openThread(7, "ordering")}
	humanLast := []stubComment{comment(7, bot, 90), comment(7, human, 10)}
	botLast := []stubComment{comment(7, human, 90), comment(7, bot, 10)}

	for _, order := range [][]stubComment{humanLast, {humanLast[1], humanLast[0]}} {
		if out := unanswered(t, order, threads); !strings.Contains(out, "#7") {
			t.Errorf("a human comment newer than the bot's must report regardless of feed order:\n%s", out)
		}
	}
	for _, order := range [][]stubComment{botLast, {botLast[1], botLast[0]}} {
		if out := unanswered(t, order, threads); strings.TrimSpace(out) != "" {
			t.Errorf("a bot comment newer than the human's means answered, regardless of feed order:\n%s", out)
		}
	}
}

// TestUnansweredIdentifiesBotsByTypeAndBySuffix — an App comment carries type
// "Bot"; the [bot] login suffix is the fallback, matching how ready-prs spots a
// bot author. Missing either way makes the loop's own comments look like a
// person's and every thread reports forever.
func TestUnansweredIdentifiesBotsByTypeAndBySuffix(t *testing.T) {
	for _, b := range []ghUser{
		{Login: "genesis-dev-bot[bot]", Type: "Bot"},
		{Login: "genesis-dev-bot[bot]"},  // suffix only
		{Login: "some-app", Type: "Bot"}, // type only
	} {
		out := unanswered(t,
			[]stubComment{comment(9, human, 90), comment(9, b, 10)},
			map[string]stubThread{"9": openThread(9, "bot identity")})
		if strings.TrimSpace(out) != "" {
			t.Errorf("comment by %+v was not recognised as the loop replying:\n%s", b, out)
		}
	}
}

// TestUnansweredOrdersStalestFirst — matches gates, red-prs and ready-prs. The
// comment ignored longest leads, because that is the one being forgotten.
func TestUnansweredOrdersStalestFirst(t *testing.T) {
	out := unanswered(t,
		[]stubComment{comment(11, human, 5), comment(12, human, 60*24*3)},
		map[string]stubThread{
			"11": openThread(11, "fresh"),
			"12": openThread(12, "ancient"),
		})

	i, j := strings.Index(out, "#12"), strings.Index(out, "#11")
	if i < 0 || j < 0 || i > j {
		t.Errorf("expected #12 (3d) before #11 (5m):\n%s", out)
	}
	if !strings.Contains(out, "unanswered 3d") || !strings.Contains(out, "unanswered 5m") {
		t.Errorf("expected ages at day and minute resolution — the #141 window was 2m25s, so days alone cannot express it:\n%s", out)
	}
}

// TestUnansweredLabelsPRsAsPRs — the action differs (merge vs close), so the
// reader should not have to open the link to find out which kind of thread it is.
func TestUnansweredLabelsPRsAsPRs(t *testing.T) {
	th := openThread(21, "a pull request")
	th.PullRequest = map[string]string{"url": "https://api.github.com/repos/owner/repo/pulls/21"}

	out := unanswered(t, []stubComment{comment(21, human, 15)}, map[string]stubThread{"21": th})
	if !strings.Contains(out, `on PR "a pull request"`) {
		t.Errorf("a PR thread must be identified as a PR:\n%s", out)
	}
}

// TestUnansweredEmptyWhenNothingWaiting keeps "no output" meaning "nobody is
// waiting on a reply", which is what lets the summary section be unconditional.
func TestUnansweredEmptyWhenNothingWaiting(t *testing.T) {
	if out := unanswered(t, nil, nil); strings.TrimSpace(out) != "" {
		t.Errorf("expected no output with an empty comment feed, got:\n%s", out)
	}
}

// TestUnansweredReportsItsOwnFailure — the section's empty state MEANS all-clear,
// so an unreadable API must say so rather than print nothing. Same rule the
// escalation series settled on for a missing intent checkpoint: absence is
// reported, not omitted. It must still exit 0, because a diagnostics command
// that can fail would take `summary` down with it.
func TestUnansweredReportsItsOwnFailure(t *testing.T) {
	out := runUnanswered(t, nil, []stubComment{comment(1, human, 5)}, nil, true)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("an unreadable comments API printed nothing, which reads as all-clear")
	}
	if !strings.Contains(out, "did not run") {
		t.Errorf("the failure notice must say the check did not run:\n%s", out)
	}
}

// TestUnansweredWindowIsConfigurable — the window is the only knob, and
// nudge-gates-style tooling needs to be able to widen it without editing the
// script.
func TestUnansweredWindowIsConfigurable(t *testing.T) {
	comments := []stubComment{comment(31, human, 60*24*10)} // 10 days old
	threads := map[string]stubThread{"31": openThread(31, "old but wanted")}

	if out := unanswered(t, comments, threads); strings.TrimSpace(out) != "" {
		t.Fatalf("10 days is outside the 7-day default window:\n%s", out)
	}
	if out := runUnanswered(t, []string{"--window-days", "30"}, comments, threads, false); !strings.Contains(out, "#31") {
		t.Errorf("--window-days 30 should bring a 10-day-old comment back:\n%s", out)
	}
}

// TestSummaryReportsUnansweredComments is the membership half, and the half that
// actually fixes #141: the command existing changes nothing if the state
// assessment every run reads does not print it. Unconditional, same as Human
// Gates (#84), Red PRs (#100) and Ready to Merge (#112).
func TestSummaryReportsUnansweredComments(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"))
	if err != nil {
		t.Fatalf("read issues.sh: %v", err)
	}
	src := string(b)

	summary := src[strings.Index(src, "    summary)"):]
	if end := strings.Index(summary, "\n    close)"); end > 0 {
		summary = summary[:end]
	}
	if !strings.Contains(summary, "Unanswered Human Comments") || !strings.Contains(summary, "format_unanswered_comments") {
		t.Fatalf("issues.sh `summary` does not print unanswered human comments — a condition posted mid-flight would then be invisible again:\n%s", summary)
	}
	// Unconditional means no `if`/`[ ... ]` wrapper suppressing it when empty:
	// an empty section is the all-clear signal and has to be printed to mean it.
	tail := summary[strings.Index(summary, "Unanswered Human Comments"):]
	if end := strings.Index(tail, "format_unanswered_comments"); end > 0 {
		if between := tail[:end]; strings.Contains(between, "if ") {
			t.Errorf("Unanswered Human Comments section is conditional; it must print every run:\n%s", between)
		}
	}
}

// TestUnansweredIsDocumented — the orchestrator discovers subcommands from
// `--help`; one that is not listed is one that is not used.
func TestUnansweredIsDocumented(t *testing.T) {
	cmd := exec.Command("bash", filepath.Join("..", "..", ".genesis", "scripts", "issues.sh"), "--help")
	out, _ := cmd.CombinedOutput() // usage exits 1 by design
	if !strings.Contains(string(out), "unanswered-comments") {
		t.Errorf("issues.sh usage does not document unanswered-comments:\n%s", out)
	}
}

// TestOrchestratorClassMustRecheckBeforeMerging is the behavioural half, and the
// half that actually closes the #141 window. A report only helps the *next* run:
// the damage was done at merge time, minutes after the summary had been read.
// So the instruction to re-check has to reach the run that merges.
//
// Two placement decisions, both deliberate:
//
// It is NOT in genesis-merge.yml. That runner is classified narrowScopeFloor —
// a fixed procedure on a small budget so that needing more turns fails fast —
// and "is this comment a condition?" is exactly the unbounded judgment that
// classification exists to keep out. The exemption is therefore derived from the
// existing minTurns table rather than from a second list, the same way
// checkpoint_test.go scopes itself; a new Claude workflow is covered or exempt
// by being classified, never by being forgotten.
//
// It is in the workflow prompts rather than in .claude/agents/orchestrator.md,
// where a Hard Rule would belong, because the sandbox declines writes under
// .claude/ — the same restriction recorded in CLAUDE.md against #102's attempt
// to mirror the checkpoint instruction. The reasoning is in CLAUDE.md's
// Learnings, which every session reads in every execution mode.
func TestOrchestratorClassMustRecheckBeforeMerging(t *testing.T) {
	for name, body := range claudeWorkflows(t) {
		floor, classified := minTurns[name]
		if !classified {
			continue // TestClaudeWorkflowsMeetTurnFloor already fails on this
		}
		if floor != orchestratorClassFloor {
			continue // narrow fixed-procedure runner — exempt by classification
		}
		if !strings.Contains(body, "unanswered-comments") {
			t.Errorf("%s runs an open-ended agent that can merge and close, but its prompt never tells it to re-check for unanswered human comments — a condition posted while CI ran would be merged over again (#141, #156)", name)
		}
	}
}
