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
	"errors"
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
		// errors.As rather than a type assertion: a non-zero exit is the
		// expected outcome to measure here, and an assertion would t.Fatal on a
		// wrapped ExitError, reporting "cannot run the script" for a script that
		// ran fine and returned a code.
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run session-nets.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
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

// TestSessionNetsNudgesOnceAcrossTwoRuns is the property that makes carrying the
// net in two places safe. Under Actions the workflow's pre-agent step runs
// nudge-gates.sh and then the agent's own session start runs it again, so the
// net fires twice per run by design; when a key is provisioned and the Actions
// path comes back, both carriers are live simultaneously. Exactly one nudge has
// to reach the human out of that, and it is guaranteed by the `nudged:stale`
// marker label rather than by a mode check — deliberately, because a second
// guard that can disagree with the first is worse than none. Proven here rather
// than assumed: the gh stub persists `issue edit --add-label`, so the second run
// reads the label the first run wrote, which is the real idempotency path.
func TestSessionNetsNudgesOnceAcrossTwoRuns(t *testing.T) {
	env, _, logPath := gateEnv(t, []stubIssue{gate(76, "Milestone 4 plan", 21)})

	first, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("first session-nets.sh run exited %d", code)
	}
	if !strings.Contains(first, "#76") {
		t.Fatalf("first run should have nudged #76; stdout was:\n%s", first)
	}

	second, _, code := runNets(t, env)
	if code != 0 {
		t.Fatalf("second session-nets.sh run exited %d", code)
	}

	if got := countCalls(t, logPath, "issue comment 76"); got != 1 {
		t.Errorf("want exactly 1 nudge on #76 across two runs, got %d — the "+
			"Actions-plus-hook case would double-nudge\ncalls:\n%s",
			got, strings.Join(calls(t, logPath), "\n"))
	}
	if strings.TrimSpace(second) != "" {
		t.Errorf("a run that wrote nothing must add nothing to the session context; stdout was:\n%s", second)
	}
}

// sessionStartCommands returns the SessionStart hook commands from
// .claude/settings.json in declaration order.
func sessionStartCommands(t *testing.T) (path string, commands []string) {
	t.Helper()
	path = filepath.Join("..", "..", ".claude", "settings.json")
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
			commands = append(commands, h.Command)
		}
	}
	return path, commands
}

// TestSessionStartHookRunsSessionNets ties the script to its trigger, the same
// way TestScheduledOrchestratorRunsNudge ties nudge-gates.sh to the workflow
// step. The check is worthless if nothing calls it — and "nothing calls it" is
// exactly the defect being fixed, one mode over.
func TestSessionStartHookRunsSessionNets(t *testing.T) {
	path, commands := sessionStartCommands(t)
	for _, c := range commands {
		if strings.Contains(c, "session-nets.sh") {
			return
		}
	}
	t.Errorf("no SessionStart hook runs session-nets.sh (%s).\n"+
		"The deterministic nets are wired into workflow YAML, and `genesis serve` "+
		"disables every genesis-* workflow and runs `claude -p` directly — the "+
		"SessionStart hook is the only seam both modes share, so without it the "+
		"stale-gate check does not run in the mode this project executes in.", path)
}

// TestSessionStartNetIsIndependentOfTheActivityLog states the ordering, since
// settings.json is JSON and cannot carry the reason itself.
//
// Two hooks now share SessionStart: `log.sh session-start`, which records that a
// session began, and `session-nets.sh`, which spends up to
// GENESIS_SESSION_NETS_TIMEOUT seconds on the network. The claim asserted here
// holds under either hook-execution semantics, which is why it is the claim
// rather than a run-order one: they are two separate commands, never chained,
// and the log comes first in declaration order. Chaining is the failure to
// avoid in both directions — `log.sh` exiting non-zero (the Loki 502 that
// already manufactured one false escalation, #91) would suppress the net, and
// the net's timeout would delay the record of the session having started. Order
// then costs nothing and buys one thing: if the runner honours declaration
// order, a session that stalls in the net is still logged as having begun.
func TestSessionStartNetIsIndependentOfTheActivityLog(t *testing.T) {
	path, commands := sessionStartCommands(t)

	logIdx, netIdx := -1, -1
	for i, c := range commands {
		if strings.Contains(c, "log.sh") && logIdx < 0 {
			logIdx = i
		}
		if strings.Contains(c, "session-nets.sh") && netIdx < 0 {
			netIdx = i
		}
	}
	if logIdx < 0 {
		t.Fatalf("no SessionStart hook runs log.sh (%s)", path)
	}
	if netIdx < 0 {
		t.Skip("session-nets.sh is not wired yet; TestSessionStartHookRunsSessionNets owns that failure")
	}
	if logIdx > netIdx {
		t.Errorf("log.sh session-start should be declared before session-nets.sh in %s, "+
			"so a session that stalls in the net is still recorded as having begun; got %v",
			path, commands)
	}
	for _, c := range commands {
		if !strings.Contains(c, "session-nets.sh") {
			continue
		}
		for _, chain := range []string{"&&", "||", ";", "|"} {
			if strings.Contains(c, chain) {
				t.Errorf("session-nets.sh must be its own command, not %q-chained to another hook "+
					"(a failing log.sh would suppress the net, and the net's timeout would delay the log): %q",
					chain, c)
			}
		}
	}
}
