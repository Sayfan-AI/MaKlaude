// Session-net tests.
//
// Why these exist: `nudge-gates.sh` is the deterministic answer to a gate
// nobody answers (#84), and it was wired as a step in genesis-orchestrator.yml
// — before the agent step, so the signal escapes a run that later dies. That
// wiring is complete under GitHub Actions and absent everywhere else. Under
// `genesis serve`, which is the mode this project runs in today, all six
// genesis-* workflows are disabled and the control plane launches `claude -p`
// directly without ever running anything under `.genesis/scripts/`. So the net
// existed, was tested, was correctly placed, and did not run.
//
// `session-nets.sh` closes that by hanging the same net off the SessionStart
// hook, which both modes share. The properties worth pinning are not "does it
// nudge" — stalegates_test.go already owns that — but the three ways this
// change could make things worse than the gap it fills: writing while acting as
// a person (which would burn the one-shot nudge on a comment GitHub notifies
// nobody about), narrating a no-op into every session's context, and failing a
// session start.
package devsystem

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runNets executes session-nets.sh and returns stdout, stderr and the exit
// code separately. The split matters: this script's contract is that a
// diagnostic goes to stderr (the transcript) while stdout is injected into the
// agent's context, and CombinedOutput would erase the distinction.
func runNets(t *testing.T, env []string) (stdout, stderr string, code int) {
	t.Helper()
	path := filepath.Join("..", "..", ".genesis", "scripts", "session-nets.sh")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cannot locate session-nets.sh: %v", err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = env
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("run session-nets.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// TestSessionNetsNudgesWhenActingAsTheApp is the reason the script exists: in a
// mode with no workflow step, a stale gate still gets its one nudge.
func TestSessionNetsNudgesWhenActingAsTheApp(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{
		gate(76, "Milestone 4 plan", 21),
		gate(90, "Filed yesterday", 1),
	})

	stdout, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("session-nets.sh exited %d; a SessionStart hook must never fail a session", code)
	}
	if got := countCalls(t, logPath, "issue comment 76"); got != 1 {
		t.Errorf("want 1 nudge comment on the 21-day gate, got %d\ncalls:\n%s",
			got, strings.Join(calls(t, logPath), "\n"))
	}
	if !strings.Contains(stdout, "#76") {
		t.Errorf("a nudge that was posted should reach the session's context; stdout was:\n%s", stdout)
	}
	if got := countCalls(t, logPath, "issue comment 90"); got != 0 {
		t.Errorf("fresh gate must not be nudged, got %d comments", got)
	}
}

// TestSessionNetsWritesNothingAsAHuman is the load-bearing exclusion. The hook
// fires for every Claude session in this repo, a person's included. Running the
// nudge there would post the comment under that person's account — GitHub does
// not notify you of your own comment, so it reaches nobody — and would apply
// `nudged:stale`, permanently suppressing the bot's one real nudge. That
// converts a missing net into a dead one, which is strictly worse.
func TestSessionNetsWritesNothingAsAHuman(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{gate(76, "Milestone 4 plan", 21)})
	env = append(env, "STUB_IDENTITY=user")

	stdout, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("session-nets.sh exited %d in a human session", code)
	}
	for _, c := range calls(t, logPath) {
		if strings.HasPrefix(c, "issue comment") || strings.HasPrefix(c, "issue edit") ||
			strings.HasPrefix(c, "label create") {
			t.Errorf("a human session must not write to GitHub, but called: %q", c)
		}
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a human session should add nothing to context; stdout was:\n%s", stdout)
	}
}

// TestSessionNetsIsSilentWhenAllGatesFresh guards the common case. This runs at
// the top of every session, so the all-clear has to cost nothing — a net that
// announces its own no-op each time is the noise that gets the whole report
// skipped, the same reasoning that keeps `red-prs` and `ready-prs` empty-means-
// all-clear.
func TestSessionNetsIsSilentWhenAllGatesFresh(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{gate(90, "Filed yesterday", 1)})

	stdout, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("session-nets.sh exited %d with nothing to do", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("all-clear must print nothing into the session context; stdout was:\n%s", stdout)
	}
	for _, c := range calls(t, logPath) {
		if strings.HasPrefix(c, "issue comment") || strings.HasPrefix(c, "issue edit") {
			t.Errorf("no writes expected when every gate is fresh, got: %q", c)
		}
	}
}

// TestSessionNetsSurvivesABrokenNet covers the failure that would be self-
// inflicted: a hook that exits non-zero degrades every session in the repo,
// including sessions that have nothing to do with gates. A broken `gh`, an
// expired token or a Loki-style outage must cost a diagnostic, not a session.
func TestSessionNetsSurvivesABrokenNet(t *testing.T) {
	env, _, _ := gateEnv(t, []stubIssue{gate(76, "Milestone 4 plan", 21)})
	// Point the stub at a state file that does not exist: identity still
	// answers, then `issue list` blows up underneath nudge-gates.sh.
	for i, kv := range env {
		if strings.HasPrefix(kv, "STUB_STATE=") {
			env[i] = "STUB_STATE=" + filepath.Join(t.TempDir(), "absent.json")
		}
	}

	stdout, stderr, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("session-nets.sh exited %d when the net failed; it must exit 0", code)
	}
	if !strings.Contains(stderr, "session-nets") {
		t.Errorf("a lost net should say so on stderr; stderr was:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("a diagnostic belongs on stderr, not in the agent's context; stdout was:\n%s", stdout)
	}
}

// TestSessionNetsSurvivesNoGh is the same property one layer out: no `gh` on
// PATH at all (a fresh checkout, a stripped container) must still be a clean
// session start.
func TestSessionNetsSurvivesNoGh(t *testing.T) {
	dir := t.TempDir()
	env := append(os.Environ(), "PATH="+dir, "GH_TOKEN=stub")

	_, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("session-nets.sh exited %d with no gh on PATH; it must exit 0", code)
	}
}

// TestSessionStartHookRunsSessionNets ties the script to its trigger, the same
// way TestScheduledOrchestratorRunsNudge ties nudge-gates.sh to the workflow
// step. The check is worthless if nothing calls it — and "nothing calls it" is
// exactly the defect being fixed, one mode over.
func TestSessionStartHookRunsSessionNets(t *testing.T) {
	path := filepath.Join("..", "..", ".claude", "settings.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var cfg struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	for _, matcher := range cfg.Hooks["SessionStart"] {
		for _, h := range matcher.Hooks {
			if strings.Contains(h.Command, "session-nets.sh") {
				return
			}
		}
	}
	t.Errorf("no SessionStart hook runs session-nets.sh (%s).\n"+
		"The deterministic nets are wired into workflow YAML, and `genesis serve` "+
		"disables every genesis-* workflow and runs `claude -p` directly — the "+
		"SessionStart hook is the only seam both modes share, so without it the "+
		"stale-gate check does not run in the mode this project executes in.", path)
}
