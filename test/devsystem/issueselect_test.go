// Guard on the three behaviors in .genesis/scripts/issues.sh that make the
// `in-progress` label mean something: `label` accumulating repeated flags,
// `claim` reading its own write back, and `next` exiting 3 rather than 0 when
// there is nothing to work.
//
// Why this exists here as well as upstream: the script is genesis scaffolding,
// and genesis now covers these by execution too. But this repo runs a *copy*
// under .genesis/scripts/, backported by hand in 90575b0. A copy that drifts
// from the original fails silently, and the thing it takes down is the board
// this dev system uses to answer "is anyone working this" — so every agent goes
// on picking issues while nothing is marked, and two of them pick the same one.
//
// The failures are all quiet, which is why reading the script is not enough:
//
//   - The pre-fix `label` wrote two scalars, so
//     `label --id N --remove in-progress --remove needs:human` applied only the
//     last, printed the issue URL and exited 0. A partial removal was
//     indistinguishable from a full one, and the label left behind was
//     `in-progress` — the one the board reads.
//   - `claim` exists because "the agent should label it in-progress" is a rule a
//     model follows most of the time, and most of the time is not a state
//     machine.
//   - `next` returning 0 on an empty board makes a finished milestone look like
//     a successful pick of nothing.
//
// And the subtlest one: this has to work under `set -u` on **bash 3.2**, which
// is what macOS ships and therefore what `genesis serve` local mode runs, where
// expanding an EMPTY array is an unbound-variable error. So the add-only and
// remove-only cases are the ones that matter. The first cut of the fix used
// ${#ADD[@]} and broke exactly the remove-only call issue #202 was reported for.
// A test that always passes both flags would not have caught it.
package devsystem

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH stands in for the gh CLI. It records each invocation and answers from
// the environment, applying the script's own --jq expression to the canned JSON
// rather than returning a pre-filtered answer: for `next` the selection rule *is*
// the jq expression, so hand-filtering the fixture would leave the part most
// likely to be wrong untested.
const fakeGH = `#!/bin/sh
echo "$*" >> "$GH_CALLS"

JQ_EXPR=""
prev=""
for a in "$@"; do
  [ "$prev" = "--jq" ] && JQ_EXPR="$a"
  prev="$a"
done

emit() {
  if [ -n "$JQ_EXPR" ]; then
    printf '%s' "$1" | jq -r "$JQ_EXPR"
  else
    printf '%s\n' "$1"
  fi
}

case "$1 $2" in
  "issue view") emit "${GH_VIEW_JSON-}" ;;
  "issue list") emit "${GH_LIST_JSON-[]}" ;;
  "issue edit")
    [ -n "${GH_EDIT_FAILS-}" ] && exit 1
    echo "https://github.com/o/r/issues/1" ;;
esac
exit 0
`

// What `gh issue view --json labels` returns when a claim stuck, and when it
// silently did not. The second is the lying board `claim` exists to catch.
const (
	labelStuck   = `{"labels":[{"name":"in-progress"}]}`
	labelMissing = `{"labels":[]}`
)

type ghResult struct {
	code   int
	stdout string
	stderr string
	calls  []string
}

// runIssues executes the repo's own copy of issues.sh with gh stubbed out.
//
// /bin/bash explicitly, not "bash": on macOS the first is 3.2 and a Homebrew
// bash 5 on PATH would hide the empty-array trap this test is here to pin.
func runIssues(t *testing.T, env map[string]string, args ...string) ghResult {
	t.Helper()

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	callLog := filepath.Join(dir, "calls")
	if err := os.WriteFile(callLog, nil, 0o644); err != nil {
		t.Fatalf("create call log: %v", err)
	}

	// Absolute, because cmd.Dir is itself repoRoot() and a relative script path
	// would then be resolved a second time, from outside the repo.
	//
	// ISSUES_SH exists so the suite can be pointed at a deliberately broken copy
	// to confirm these tests actually fail when the behavior regresses. Mutating
	// the repo's own copy would be the obvious way to check that, and it is not
	// safe: `genesis serve` runs against this working tree, and a run that called
	// issues.sh during the mutation window would silently mislabel the board.
	// Unset in CI, so the default is always the real copy.
	script := os.Getenv("ISSUES_SH")
	if script == "" {
		script = filepath.Join(repoRoot(), ".genesis", "scripts", "issues.sh")
	}
	script, err := filepath.Abs(script)
	if err != nil {
		t.Fatalf("resolve issues.sh: %v", err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("issues.sh is missing: %v", err)
	}

	cmd := exec.Command("/bin/bash", append([]string{script}, args...)...)
	cmd.Dir = repoRoot()
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CALLS="+callLog,
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	_ = cmd.Run()

	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	return ghResult{
		code:   cmd.ProcessState.ExitCode(),
		stdout: out.String(),
		stderr: errb.String(),
		calls:  calls,
	}
}

func (r ghResult) called(want string) bool {
	for _, c := range r.calls {
		if c == want {
			return true
		}
	}
	return false
}

// Both flags get their own case on purpose. ADD and REMOVE are accumulated by
// separate lines, so a regression can land on one and not the other. An earlier
// version of this file covered only --remove (the direction issue #202 reported),
// and a mutation that broke --add accumulation passed the whole suite.
func TestLabelAppliesEveryRepeatedFlag(t *testing.T) {
	for _, tc := range []struct {
		name, flag, ghFlag string
	}{
		// The exact call from issue #202.
		{"repeated removes", "--remove", "--remove-label"},
		{"repeated adds", "--add", "--add-label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runIssues(t, nil, "label", "--id", "199",
				tc.flag, "in-progress", tc.flag, "needs:human")

			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			for _, label := range []string{"in-progress", "needs:human"} {
				want := "issue edit 199 " + tc.ghFlag + " " + label
				if !r.called(want) {
					t.Errorf("did not run %q; calls: %v", want, r.calls)
				}
			}
		})
	}
}

// A single-flag call leaves the other array empty, which is the bash 3.2 + set -u
// trap. Both directions, because the guard is per-array and one of them was
// missed the first time.
func TestLabelSurvivesAnEmptyArray(t *testing.T) {
	for _, tc := range []struct{ name, flag, want string }{
		{"remove only", "--remove", "issue edit 7 --remove-label in-progress"},
		{"add only", "--add", "issue edit 7 --add-label in-progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runIssues(t, nil, "label", "--id", "7", tc.flag, "in-progress")
			if strings.Contains(r.stderr, "unbound variable") {
				t.Fatalf("empty array expansion under set -u: %s", r.stderr)
			}
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if !r.called(tc.want) {
				t.Errorf("did not run %q; calls: %v", tc.want, r.calls)
			}
		})
	}
}

// Exiting 0 here would report success for having changed nothing.
func TestLabelWithNoLabelsIsAnError(t *testing.T) {
	r := runIssues(t, nil, "label", "--id", "7")
	if r.code == 0 {
		t.Errorf("exit 0 for a no-op; stdout: %s", r.stdout)
	}
	if len(r.calls) != 0 {
		t.Errorf("touched gh for a no-op: %v", r.calls)
	}
}

// A partial failure that exits 0 is the whole class of bug here.
func TestLabelFailsWhenAnEditFails(t *testing.T) {
	r := runIssues(t, map[string]string{"GH_EDIT_FAILS": "1"},
		"label", "--id", "7", "--add", "bug")
	if r.code == 0 {
		t.Error("exit 0 though the gh edit failed")
	}
}

func TestClaimVerifiesTheLabelStuck(t *testing.T) {
	r := runIssues(t, map[string]string{"GH_VIEW_JSON": labelStuck}, "claim", "--id", "42")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "claimed #42") {
		t.Errorf("no confirmation on stdout: %q", r.stdout)
	}
	if !r.called("issue edit 42 --add-label in-progress") {
		t.Errorf("never wrote the label: %v", r.calls)
	}
	// The read-back is the point. Without it this is just a label call.
	var readBack bool
	for _, c := range r.calls {
		if strings.HasPrefix(c, "issue view 42") {
			readBack = true
		}
	}
	if !readBack {
		t.Errorf("never read the label back: %v", r.calls)
	}
}

// gh exits 0 but the label is absent. Reporting success here is what makes the
// board lie, and a lying board is worse than a failed run because nothing
// escalates.
func TestClaimRefusesSuccessWhenTheLabelDidNotStick(t *testing.T) {
	r := runIssues(t, map[string]string{"GH_VIEW_JSON": labelMissing}, "claim", "--id", "42")
	if r.code == 0 {
		t.Fatal("reported success though in-progress is absent")
	}
	if !strings.Contains(r.stderr, "did not stick") {
		t.Errorf("unhelpful stderr: %q", r.stderr)
	}
}

// Oldest first so nothing starves, and picking is the same call as marking, so
// the caller cannot forget to mark what it took.
func TestNextTakesTheOldestEligibleAndClaimsIt(t *testing.T) {
	listing := `[{"number":50,"createdAt":"2026-08-02T00:00:00Z","labels":[]},` +
		`{"number":40,"createdAt":"2026-08-01T00:00:00Z","labels":[]}]`

	r := runIssues(t, map[string]string{
		"GH_LIST_JSON": listing,
		"GH_VIEW_JSON": labelStuck,
	}, "next", "--milestone", "6")

	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	// Nothing but the number on stdout, so callers can do
	// ISSUE=$(issues.sh next --milestone 6).
	if got := strings.TrimSpace(r.stdout); got != "40" {
		t.Errorf("stdout = %q, want \"40\"", got)
	}
	if !r.called("issue edit 40 --add-label in-progress") {
		t.Errorf("picked without claiming: %v", r.calls)
	}
}

func TestNextSkipsIneligibleIssues(t *testing.T) {
	for _, label := range []string{"blocked", "in-progress", "needs:human"} {
		t.Run(label, func(t *testing.T) {
			listing := `[{"number":40,"createdAt":"2026-08-01T00:00:00Z",` +
				`"labels":[{"name":"` + label + `"}]},` +
				`{"number":50,"createdAt":"2026-08-02T00:00:00Z","labels":[]}]`

			r := runIssues(t, map[string]string{
				"GH_LIST_JSON": listing,
				"GH_VIEW_JSON": labelStuck,
			}, "next", "--milestone", "6")

			if got := strings.TrimSpace(r.stdout); got != "50" {
				t.Errorf("stdout = %q, want \"50\" (skipping the %s issue)", got, label)
			}
		})
	}
}

// 3, not 0 and not 1: an empty board is neither success nor failure. A caller
// that conflates them either does nothing forever or escalates a finished
// milestone as a crash.
func TestNextExitsThreeWhenThereIsNothingToWork(t *testing.T) {
	r := runIssues(t, map[string]string{"GH_LIST_JSON": "[]"}, "next", "--milestone", "6")

	if r.code != 3 {
		t.Errorf("exit %d, want 3; stderr: %s", r.code, r.stderr)
	}
	if got := strings.TrimSpace(r.stdout); got != "" {
		t.Errorf("stdout = %q, want empty", got)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "--add-label") {
			t.Errorf("claimed something on an empty board: %v", r.calls)
		}
	}
}

// A missing --milestone is a caller bug, and it must stay distinguishable from
// an empty milestone or a typo looks like a finished one.
func TestNextSeparatesUsageErrorFromEmptyBoard(t *testing.T) {
	r := runIssues(t, nil, "next")
	if r.code == 3 || r.code == 0 {
		t.Errorf("exit %d for a missing --milestone, want a usage error", r.code)
	}
}

// If the claim can't be verified, `next` must not hand back a number the caller
// will treat as marked.
func TestNextReportsNoPickWhenTheClaimFails(t *testing.T) {
	listing := `[{"number":40,"createdAt":"2026-08-01T00:00:00Z","labels":[]}]`

	r := runIssues(t, map[string]string{
		"GH_LIST_JSON": listing,
		"GH_VIEW_JSON": labelMissing,
	}, "next", "--milestone", "6")

	if r.code == 0 {
		t.Error("reported success though the claim could not be verified")
	}
	if got := strings.TrimSpace(r.stdout); got != "" {
		t.Errorf("handed back %q despite an unverified claim", got)
	}
}
