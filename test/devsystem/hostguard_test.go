// Host-guard tests.
//
// What the guard is: a PreToolUse hook that refuses `Bash` commands referencing
// paths that hold the operator's credentials. It exists because of a real run in
// this repo — an orchestrator session wanted to know how a shell alias was
// defined and ran
//
//	grep -rn "gci" ~/.zshrc ~/.dotfiles.local/*.sh ~/.dotfiles/**/*.zsh
//
// where that glob covers a file holding work credentials. Nothing was
// exfiltrated and nothing was being looked for, but matching lines would have
// reached the session transcript and from there the Loki sink. The agent was
// doing ordinary research in a directory it should never have been able to read.
//
// What the guard is NOT: containment. A determined route-finder gets around any
// per-command check, exactly as denying `gh api` moves the request to `curl`. It
// is a tripwire for the careless case and it fails open, so a bug in it cannot
// wedge the loop. Both properties are asserted below rather than asserted in
// prose, because the failure mode of a security speed bump is that it quietly
// stops being either safe or a speed bump.
//
// Why it only matters here: under GitHub Actions the agent runs in an ephemeral
// runner with no home directory worth reading. Under `genesis serve` — the mode
// this project executes in today — it runs as the operator, with the operator's
// entire home directory in reach. Same asymmetry `sessionnets_test.go` opens
// with: a protection wired only into workflow YAML is absent in the mode that is
// actually running.
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

// hookCommands returns the hook commands declared for one hook event in
// .claude/settings.json, in declaration order. Order is part of the contract
// being tested (the guard must be declared before the activity log), so this
// preserves it rather than returning a set.
func hookCommands(t *testing.T, event string) (path string, commands []string) {
	t.Helper()
	path, byEvent := allHookCommands(t)
	return path, byEvent[event]
}

// allHookCommands parses every hook event out of .claude/settings.json. The
// whole-file view exists for TestEveryHookedScriptExists, which is a claim about
// the set of referenced scripts rather than about any one hook.
func allHookCommands(t *testing.T) (path string, byEvent map[string][]string) {
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

	byEvent = make(map[string][]string, len(cfg.Hooks))
	for event, matchers := range cfg.Hooks {
		for _, matcher := range matchers {
			for _, h := range matcher.Hooks {
				byEvent[event] = append(byEvent[event], h.Command)
			}
		}
	}
	return path, byEvent
}

// guardPath locates the script under test.
func guardPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", ".genesis", "scripts", "host-guard.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("cannot locate host-guard.sh: %v", err)
	}
	return p
}

// runGuard feeds a hook payload to host-guard.sh on stdin and returns its exit
// code and streams separately. The code is the entire interface a PreToolUse
// hook has for blocking a call — 2 blocks, anything else does not — so every
// assertion here is about the code, and stderr only matters as the explanation
// the agent is shown when it is blocked.
func runGuard(t *testing.T, payload string, env []string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("bash", guardPath(t))
	cmd.Stdin = strings.NewReader(payload)
	if env != nil {
		cmd.Env = env
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		// errors.As rather than a type assertion, for the reason runNets gives:
		// a non-zero exit is the outcome being measured, and an assertion would
		// report "cannot run the script" for a script that ran fine.
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run host-guard.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// bashPayload builds a PreToolUse payload for a Bash call. Marshalled rather
// than hand-written, so a command containing a quote or a backslash — which the
// real incident's command does — tests the guard instead of testing my ability
// to escape JSON by hand.
func bashPayload(t *testing.T, command string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": command},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// TestHostGuardBlocksTheCommandThatHappened is the case the guard exists for,
// kept verbatim rather than reduced to a synthetic `cat ~/.ssh/id_rsa`. The real
// command is a *research* command with a credential file caught in a glob, which
// is why a rule-following agent tripped it; a test that only covers commands
// that look malicious would pass while the actual incident sailed through.
func TestHostGuardBlocksTheCommandThatHappened(t *testing.T) {
	payload := bashPayload(t, `grep -rn "gci" ~/.zshrc ~/.dotfiles.local/*.sh ~/.dotfiles/**/*.zsh`)

	_, stderr, code := runGuard(t, payload, nil)
	if code != 2 {
		t.Fatalf("guard exited %d for the incident command; PreToolUse blocks only on 2", code)
	}
	// The agent sees stderr and nothing else. If it does not name the script and
	// suggest the legitimate alternative, the next run's response to being
	// blocked is to look for another route to the same file.
	for _, want := range []string{"host-guard.sh", "repository"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("block message should mention %q so the agent knows what stopped it and what to do instead; stderr was:\n%s", want, stderr)
		}
	}
}

// TestHostGuardBlocksCredentialPaths covers the list in all three spellings the
// script has to recognise. `~/.aws` and `$HOME/.aws` and `/Users/you/.aws` are
// the same directory, and a guard that only knows the tilde form is defeated by
// a shell that expands before the agent ever writes the string down.
func TestHostGuardBlocksCredentialPaths(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home directory: %v", err)
	}

	for _, command := range []string{
		"cat ~/.ssh/id_rsa",
		"cat $HOME/.ssh/config",
		"cat " + filepath.Join(home, ".ssh", "id_ed25519"),
		"cat ~/.aws/credentials",
		"cat $HOME/.aws/credentials",
		"cat " + filepath.Join(home, ".aws", "credentials"),
		"ls ~/.gnupg",
		"cat ~/.netrc",
		"cat ~/.dotfiles.local/perplexity.sh",
		"cat ~/.config/gh/hosts.yml",
		"cat ~/.claude.json",
		"ls ~/Library/Keychains",
		"cat /etc/shadow",
		"cat /etc/sudoers",
	} {
		if _, _, code := runGuard(t, bashPayload(t, command), nil); code != 2 {
			t.Errorf("guard exited %d, should have blocked: %s", code, command)
		}
	}
}

// TestHostGuardAllowsOrdinaryWork is the load-bearing half. A guard that blocks
// real work gets removed or routed around, so its false-positive cost is higher
// than its marginal security — the same reasoning that keeps `red-prs` and
// `ready-prs` empty-means-all-clear rather than chatty.
//
// `~/.kube` is deliberately absent from the block list and asserted here so a
// future tightening has to delete a test with a reason attached. Blocking it
// would break this project's actual job, and Milestone 6 sharpens that rather
// than softening it: the chaos write path needs a kubeconfig by explicit path.
func TestHostGuardAllowsOrdinaryWork(t *testing.T) {
	for _, command := range []string{
		"go test ./...",
		"go vet -tags e2e ./...",
		"gh pr list",
		"git status --short",
		"bash .genesis/scripts/issues.sh summary",
		"kubectl --kubeconfig ./clusters/dev.yaml get pods -n default",
		"kubectl get pods -n chaos-mesh --context kind-maklaude",
		"cat ~/.kube/config",
		"kubectl --kubeconfig ~/.kube/config get nodes",
	} {
		if _, _, code := runGuard(t, bashPayload(t, command), nil); code != 0 {
			t.Errorf("guard exited %d, should have allowed: %s", code, command)
		}
	}
}

// TestHostGuardIgnoresNonBashTools states the scope honestly. The guard reads
// one field of one tool's input; `Read`, `Grep` and `Glob` can name the same
// paths and are not inspected. That is a real hole, and pinning it here keeps
// the documentation's "tripwire, not containment" from being read as modesty.
func TestHostGuardIgnoresNonBashTools(t *testing.T) {
	for _, payload := range []string{
		`{"tool_name":"Read","tool_input":{"file_path":"~/.ssh/id_rsa"}}`,
		`{"tool_name":"Grep","tool_input":{"pattern":"key","path":"~/.aws"}}`,
	} {
		if _, _, code := runGuard(t, payload, nil); code != 0 {
			t.Errorf("guard exited %d on a non-Bash tool; it inspects Bash commands only", code)
		}
	}
}

// TestHostGuardFailsOpenOnBrokenPayload is the trade that makes this a tripwire
// rather than a wall, and it is the reason the guard can be wired ahead of every
// single tool call at all. A hook that returns 2 on a payload it failed to
// understand blocks the whole loop, and the loop's own recovery paths are Bash
// commands — so a parsing bug here would take out the machinery that would
// otherwise report it.
func TestHostGuardFailsOpenOnBrokenPayload(t *testing.T) {
	for _, payload := range []string{
		"",
		"not json",
		"{}",
		`{"tool_name":"Bash"}`,
		`{"tool_name":"Bash","tool_input":{}}`,
		`{"tool_name":"Bash","tool_input":{"command":null}}`,
		`{"tool_name":null}`,
	} {
		if _, _, code := runGuard(t, payload, nil); code != 0 {
			t.Errorf("guard exited %d on payload %q; an unparseable payload must fail open", code, payload)
		}
	}
}

// TestHostGuardDoesNotWedgeWithoutPython3 is the same fail-open property one
// layer down, and the one that would actually bite. The guard is a shell wrapper
// around a python3 script: on a machine without python3 the interpreter is
// missing, not the payload, and *that* is the case where a wedge is plausible.
// The guard becomes inert here — which is the correct trade for a tripwire, and
// is why the docs say plainly that it is not a containment layer.
func TestHostGuardDoesNotWedgeWithoutPython3(t *testing.T) {
	// exec.Command resolves "bash" against the parent's PATH before this env is
	// applied, so bash still starts; python3 is looked up inside the script and
	// is not found.
	env := []string{"PATH=" + t.TempDir(), "HOME=" + t.TempDir()}

	_, _, code := runGuard(t, bashPayload(t, "cat ~/.ssh/id_rsa"), env)
	if code == 2 {
		t.Errorf("guard returned a blocking 2 with no python3 available; it must not " +
			"block calls it cannot actually inspect, or every Bash call in the loop dies " +
			"on a machine missing an interpreter")
	}
}

// TestPreToolUseHookRunsHostGuard ties the script to its trigger, the same way
// TestSessionStartHookRunsSessionNets does for the session nets. Everything
// above tests a script; this tests that anything calls it. A guard nothing
// invokes is not a weak guard, it is a file — and "nothing invokes it" is the
// exact defect this repo has now hit twice, once for the stale-gate net under
// `genesis serve` and once for a workflow the scaffolder never copied.
func TestPreToolUseHookRunsHostGuard(t *testing.T) {
	path, commands := hookCommands(t, "PreToolUse")
	for _, c := range commands {
		if strings.Contains(c, "host-guard.sh") {
			return
		}
	}
	t.Errorf("no PreToolUse hook runs host-guard.sh (%s).\n"+
		"The script refuses Bash commands that reach for the operator's credential "+
		"files, and PreToolUse is the only seam that can refuse a call before it runs. "+
		"Unwired, it protects nothing: the incident it exists for (a research grep whose "+
		"glob covered a credentials file) would run exactly as it did.", path)
}

// TestHostGuardRunsBeforeTheActivityLog pins the ordering, since settings.json
// is JSON and cannot carry the reason itself.
//
// Two hooks share PreToolUse: the guard, which may block the call, and
// `log.sh pre-tool-use`, which ships the call to the Loki sink. Declaring the
// log first would record a blocked command as though it ran — and worse, the log
// carries the command *text*, so the credential path the guard just refused to
// let anyone read would be shipped to the sink by the hook beside it. The point
// of blocking that grep was to keep matched lines out of the transcript and the
// sink; logging it first donates the pathname to the sink anyway.
//
// Also asserted: the guard is its own command, never `&&`-chained to the log.
// Chaining is wrong in both directions — a `log.sh` that exits non-zero (the
// Loki 502 that already manufactured one false escalation, #91) would suppress
// the guard, and the guard's blocking exit 2 would suppress the log for every
// refused call.
func TestHostGuardRunsBeforeTheActivityLog(t *testing.T) {
	path, commands := hookCommands(t, "PreToolUse")

	guardIdx, logIdx := -1, -1
	for i, c := range commands {
		if strings.Contains(c, "host-guard.sh") && guardIdx < 0 {
			guardIdx = i
		}
		if strings.Contains(c, "log.sh") && logIdx < 0 {
			logIdx = i
		}
	}
	if guardIdx < 0 {
		t.Skip("host-guard.sh is not wired yet; TestPreToolUseHookRunsHostGuard owns that failure")
	}
	if logIdx < 0 {
		t.Fatalf("no PreToolUse hook runs log.sh (%s)", path)
	}
	if guardIdx > logIdx {
		t.Errorf("host-guard.sh must be declared before log.sh pre-tool-use in %s, so a "+
			"blocked command is not logged as though it ran and its credential path is not "+
			"shipped to the log sink by the hook beside the one that refused it; got %v",
			path, commands)
	}
	for _, c := range commands {
		if !strings.Contains(c, "host-guard.sh") {
			continue
		}
		for _, chain := range []string{"&&", "||", ";", "|"} {
			if strings.Contains(c, chain) {
				t.Errorf("host-guard.sh must be its own command, not %q-chained to another hook "+
					"(a failing log.sh would suppress the guard, and the guard's blocking exit 2 "+
					"would suppress the log): %q", chain, c)
			}
		}
	}
}

// TestEveryHookedScriptExists is the set-level guard, and it is here because
// this repo keeps relearning the same class from a different member: a hook or
// workflow step naming a script that is not actually present. `genesis-merge.yml`
// shipped as a template the scaffolder never copied, so the first dev system was
// born unable to merge its own pull requests. Upstream, `host-guard.sh` itself
// was referenced by the seed settings.json and by SEED_SCRIPTS while the file
// stayed untracked — the reference shipped and the script did not.
//
// A missing PreToolUse script is the worst placement for that mistake, since the
// hook fires on every tool call, so the failure is continuous rather than
// occasional. Asserting over the set means a hook added later cannot reopen the
// hole the same invisible way.
func TestEveryHookedScriptExists(t *testing.T) {
	path, byEvent := allHookCommands(t)
	root := filepath.Join("..", "..")

	referenced := 0
	for event, commands := range byEvent {
		for _, c := range commands {
			for _, token := range strings.Fields(c) {
				if !strings.HasPrefix(token, ".genesis/scripts/") {
					continue
				}
				referenced++
				if _, err := os.Stat(filepath.Join(root, token)); err != nil {
					t.Errorf("%s hook %q references %s, which does not exist (%s): a hook naming "+
						"a missing script fails on every matching tool call", event, c, token, path)
				}
			}
		}
	}
	if referenced == 0 {
		t.Errorf("no hook in %s references a .genesis/scripts/ path; either the parser stopped "+
			"working or the dev system's hooks were removed, and both make this test vacuous", path)
	}
}

// TestHostGuardIsExecutable is minor but cheap. The hooks invoke it as
// `bash .genesis/scripts/host-guard.sh`, so the mode bit is not what makes the
// hook work; it is what makes direct invocation work while debugging, and it
// keeps the file consistent with every other script in that directory.
func TestHostGuardIsExecutable(t *testing.T) {
	info, err := os.Stat(guardPath(t))
	if err != nil {
		t.Fatalf("stat host-guard.sh: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("host-guard.sh mode is %v; it should be executable like its siblings in "+
			".genesis/scripts/", info.Mode().Perm())
	}
}

// The tests below cover issue #211: the guard matched its credential-path
// patterns against the whole command string, so a heredoc writing *documentation
// about the guard* was refused as though it had read a credential file. Fixed
// upstream in genesis 2116c16 and backported here.
//
// Why the false positive is worth a fix rather than a shrug: the set of things
// this dev system legitimately writes that name these paths is not small, and it
// grows with the guard's own success. Every design doc, pull request body and
// escalation describing what the guard blocks has to name what it blocks, T7's
// documentation task included. Each refusal costs a retry and a moment of the
// agent deciding whether working around the guard is legitimate, and that last
// part is the real cost: a guard that cries wolf teaches the thing it guards to
// route around it.
//
// The discriminator is NOT "is it a heredoc." It is what consumes the body. The
// TestHostGuardExemptionIsNotABypass cases are the reason, and they must stay
// blocked: exempting heredoc bodies wholesale would turn a nuisance fix into a
// one-line evasion.

// TestHostGuardAllowsProseThatNamesCredentialPaths is the reported false
// positive. Naming a path is not reading it.
func TestHostGuardAllowsProseThatNamesCredentialPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{
			"documentation written to a file",
			"cat > /tmp/pr.md <<'EOF'\n" +
				"The guard refuses commands reaching for ~/.ssh, ~/.aws or ~/.netrc.\n" +
				"EOF",
		},
		{
			// How this loop actually posts a comment about the guard.
			"issue comment body on stdin",
			"gh issue comment 211 --body-file - <<'EOF'\n" +
				"This blocks reads of ~/.aws/credentials.\n" +
				"EOF",
		},
		{
			"tee to a file",
			"tee /tmp/x.md <<'EOF'\nwe block ~/.ssh here\nEOF",
		},
		{
			// <<- strips leading tabs, so the terminator matches after stripping.
			"indented terminator",
			"cat <<-'EOF'\n\tmentions ~/.ssh in prose\n\tEOF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runGuard(t, bashPayload(t, tc.command), nil)
			if code != 0 {
				t.Errorf("prose refused (exit %d): %s", code, stderr)
			}
		})
	}
}

// TestHostGuardExemptionIsNotABypass pins the direction a wrong answer must not
// fail in. A missing entry in the text-sink allowlist is another false positive;
// a body that reaches an interpreter unexamined is a hole.
func TestHostGuardExemptionIsNotABypass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
	}{
		{"fed straight to bash", "bash <<'EOF'\ncat ~/.ssh/id_rsa\nEOF"},
		{"fed to python3", "python3 <<'EOF'\ncat ~/.ssh/id_rsa\nEOF"},
		// The sink name here is `cat`, which IS allowlisted. The pipe is what
		// disqualifies it, which is why the check is on the whole opener.
		{"allowlisted sink piped to bash", "cat <<'EOF' | bash\ncat ~/.ssh/id_rsa\nEOF"},
		{
			"written to a file and then run",
			"cat > /tmp/x.sh <<'EOF' ; bash /tmp/x.sh\ncat ~/.ssh/id_rsa\nEOF",
		},
		{
			"substitution in the opener",
			"cat > $(echo /tmp/x.sh) <<'EOF'\ncat ~/.ssh/id_rsa\nEOF",
		},
		{
			// Not a well-formed heredoc, so there is no body to classify. Three
			// lines rather than two on purpose: with two, the read is the last
			// line and an implementation that guessed the terminator as
			// end-of-input would keep it and still block, hiding a real hole.
			"unterminated heredoc",
			"cat > /tmp/x.md <<'EOF'\ncat ~/.ssh/id_rsa\ntrailing line",
		},
		{
			// Stripping a body must not blind the guard to the rest of the command.
			"read outside the heredoc",
			"cat ~/.ssh/id_rsa; cat > /tmp/x.md <<'EOF'\nharmless prose\nEOF",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, code := runGuard(t, bashPayload(t, tc.command), nil)
			if code != 2 {
				t.Errorf("BYPASS: exit %d, want 2 (blocked)", code)
			}
		})
	}
}
