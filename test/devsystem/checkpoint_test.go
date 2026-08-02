package devsystem

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Why these tests: a run that dies at max-turns loses its *reasoning*.
// escalate.sh answers "did anything land?" by listing touched issues and PRs
// (#97), but that says nothing about why the agent thought it was the right
// thing to land — and for the costliest failure shape, a run that dies having
// produced no artifact at all (#85), artifact discovery has literally nothing to
// report. .genesis/design/agent-turn-budgets.md left this as the one thing
// deliberately unsolved, with a precondition: if max-turns deaths continued past
// artifact discovery, checkpoint intent early — NOT another budget raise (six
// tried, five deaths) and NOT a coordinator/worker split (rejected there).
// #101 was that continued death.
//
// Two properties have to hold mechanically. The instruction must reach every
// open-ended runner (membership, per the rule the turn-budget floors and the
// concurrency group each learned by failing as opt-in properties), and the
// escalation must actually surface what was recorded — an intent written to a
// file nobody reads is worse than none, because it looks like the gap is closed.

const checkpointScript = ".genesis/scripts/checkpoint.sh"

// checkpointPathInvocationRE matches escalate.sh asking checkpoint.sh for the
// path, tolerating the shell quoting around the script argument.
var checkpointPathInvocationRE = regexp.MustCompile(`checkpoint\.sh"?\s+--path`)

func checkpointPathFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", checkpointScript)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return p
}

// TestOpenEndedWorkflowsInstructIntentCheckpoint is the membership guard.
//
// Scope is drawn from minTurns rather than a separate list: a workflow at
// orchestratorClassFloor is by definition one whose work is chosen *during* the
// run, which is exactly the case where intent is unrecoverable after the fact. A
// narrow fixed-procedure runner (genesis-merge.yml) is exempt because its intent
// IS its prompt — the escalation already names the workflow, so nothing is lost.
// Reusing the existing classification means a new Claude-invoking workflow gets
// classified once, in one place, and both this guard and the turn-budget floor
// follow from it; that is what keeps the exemption principled instead of an
// ad-hoc name that quietly grows.
func TestOpenEndedWorkflowsInstructIntentCheckpoint(t *testing.T) {
	checked := 0
	for name, body := range claudeWorkflows(t) {
		floor, classified := minTurns[name]
		if !classified {
			// TestClaudeWorkflowsMeetTurnFloor already fails loudly for this;
			// don't double-report, but don't silently treat it as exempt either.
			continue
		}
		if floor != orchestratorClassFloor {
			continue
		}
		checked++

		if !strings.Contains(body, "checkpoint.sh") {
			t.Errorf("%s runs an open-ended agent (budget floor %d) but its prompt never tells it to run %s — if this run dies at max-turns, the escalation can report what landed but nothing about why (#101)",
				name, floor, checkpointScript)
			continue
		}
		// Placement is the whole point. An instruction to checkpoint that sits
		// after the implementation work is worthless: the budget is already spent
		// by the time it would run. The prompt must say it happens FIRST.
		if !strings.Contains(body, "FIRST") {
			t.Errorf("%s mentions %s but does not mark it as the run's FIRST action — a checkpoint written after the unbounded work has already been crowded out of the budget it exists to escape (#101)",
				name, checkpointScript)
		}
	}
	if checked == 0 {
		t.Fatalf("no workflow classified at orchestratorClassFloor — classification changed, so this test guards nothing")
	}
}

// TestEscalationSurfacesRecordedIntent pins the read side. The checkpoint is
// only worth a turn if the failure path renders it.
func TestEscalationSurfacesRecordedIntent(t *testing.T) {
	b, err := os.ReadFile(escalatePathFor(t))
	if err != nil {
		t.Fatalf("read %s: %v", escalateScript, err)
	}
	src := string(b)

	if !strings.Contains(src, "was trying to do") {
		t.Errorf("%s builds an escalation body without an intent section — the agent spends a turn recording intent that no human ever sees (#101)", escalateScript)
	}

	// The path must come FROM checkpoint.sh, not be recomputed here. Two scripts
	// independently deriving one shared default is a bug whose symptom is the
	// escalation reporting "no intent recorded" while the file sits on disk.
	// Quoting varies ("$DIR/checkpoint.sh" --path), so match the invocation
	// rather than one literal spelling of it.
	if !checkpointPathInvocationRE.MatchString(src) {
		t.Errorf("%s does not resolve the checkpoint path via `checkpoint.sh --path` — recomputing the default in two places lets them drift, and the failure is silent (#101)", escalateScript)
	}

	// Absence must be reported as a signal, not omitted. "No intent recorded"
	// plus "no artifact" is the #85 shape and a human needs to be told that.
	if !strings.Contains(src, "No intent checkpoint was recorded") {
		t.Errorf("%s does not say anything when no checkpoint exists — a run that died before its first action then looks identical to one that never had to report (#85)", escalateScript)
	}
}

// TestCheckpointRoundTrip executes the script, because the properties that
// matter here are runtime ones: does `--path` agree with where a write lands,
// and does it work with no token and no network.
func TestCheckpointRoundTrip(t *testing.T) {
	script := checkpointPathFor(t)
	tmp := t.TempDir()

	// RUNNER_TEMP only — no GENESIS_CHECKPOINT_FILE override — so this exercises
	// the same resolution the real Actions job uses.
	env := append(os.Environ(), "RUNNER_TEMP="+tmp)
	env = append(env, "GENESIS_CHECKPOINT_FILE=")

	pathCmd := exec.Command("bash", script, "--path")
	pathCmd.Env = env
	out, err := pathCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("checkpoint.sh --path: %v\n%s", err, out)
	}
	resolved := strings.TrimSpace(string(out))
	if !strings.HasPrefix(resolved, tmp) {
		t.Fatalf("--path returned %q, which is not under RUNNER_TEMP %q — the escalate step would read a different file than the agent wrote", resolved, tmp)
	}

	const intent = "Take the intent-checkpoint gap named in the turn-budget design doc."
	writeCmd := exec.Command("bash", script, intent)
	writeCmd.Env = env
	if out, err := writeCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint.sh write: %v\n%s", err, out)
	}

	got, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read back %s: %v — --path and the write disagree", resolved, err)
	}
	if !strings.Contains(string(got), intent) {
		t.Errorf("checkpoint file %s does not contain the recorded intent; got:\n%s", resolved, got)
	}

	// Appending, not overwriting: "intended X, switched to Y, then died" is more
	// diagnostic than either entry alone.
	const second = "Revised: also update the design doc."
	appendCmd := exec.Command("bash", script, second)
	appendCmd.Env = env
	if out, err := appendCmd.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint.sh second write: %v\n%s", err, out)
	}
	got2, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read back after append: %v", err)
	}
	if !strings.Contains(string(got2), intent) || !strings.Contains(string(got2), second) {
		t.Errorf("second checkpoint replaced the first instead of appending; got:\n%s", got2)
	}
}

// TestCheckpointNeedsNoCredentials guards the reason this is a file and not an
// issue comment. A checkpoint that can fail on a transient API error would
// manufacture the same false escalation #91 removed, and it runs on every single
// agent run.
func TestCheckpointNeedsNoCredentials(t *testing.T) {
	script := checkpointPathFor(t)
	tmp := t.TempDir()

	// env -i equivalent: PATH only, so no GH_TOKEN, no GH_REPO, nothing.
	cmd := exec.Command("bash", script, "intent recorded with no credentials present")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"RUNNER_TEMP=" + tmp,
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint.sh requires credentials or extra env (%v) — it must never be able to fail an agent run:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "genesis-intent.md")); err != nil {
		t.Errorf("no checkpoint written with a bare environment: %v", err)
	}
}

// TestCheckpointRejectsEmptyIntent — "recorded an empty intent" is strictly
// worse than "recorded nothing", because escalate.sh would then print an empty
// section instead of the explicit no-checkpoint diagnosis.
func TestCheckpointRejectsEmptyIntent(t *testing.T) {
	script := checkpointPathFor(t)
	tmp := t.TempDir()

	cmd := exec.Command("bash", script, "   ")
	cmd.Env = append(os.Environ(), "RUNNER_TEMP="+tmp, "GENESIS_CHECKPOINT_FILE=")
	if err := cmd.Run(); err == nil {
		t.Error("checkpoint.sh accepted a whitespace-only intent — the escalation would render an empty section rather than the explicit no-checkpoint diagnosis")
	}
	if _, err := os.Stat(filepath.Join(tmp, "genesis-intent.md")); err == nil {
		t.Error("checkpoint.sh created a file for a whitespace-only intent")
	}
}

// TestCheckpointRejectsUnknownFlags — the same principle as the empty-intent
// rejection above, in the form that actually bit (#136). Intent came from "$*"
// with only `--path` special-cased, so `checkpoint.sh --help` recorded "--help"
// as the run's reasoning and printed "intent recorded"; 12 such entries piled up
// locally in eight hours. It is worse than recording nothing precisely because
// it CREATES the file: escalate.sh branches on existence, so the "No intent
// checkpoint was recorded" diagnosis — the thing that tells a human they are
// looking at a run that died before its first deliverable — never fires, and the
// section reads as a confident answer instead.
//
// Executed rather than read, because what matters is the exit status and whether
// a file appeared, not whether the script contains a `case` arm.
func TestCheckpointRejectsUnknownFlags(t *testing.T) {
	script := checkpointPathFor(t)

	// -h and --help are handled separately (usage, exit 0); everything else that
	// looks like a flag must be refused rather than guessed at.
	for _, flag := range []string{"--verbose", "--intent", "--file", "-x", "--paths"} {
		t.Run(flag, func(t *testing.T) {
			tmp := t.TempDir()
			cmd := exec.Command("bash", script, flag)
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+tmp, "GENESIS_CHECKPOINT_FILE=")
			var stderr strings.Builder
			cmd.Stderr = &stderr

			if err := cmd.Run(); err == nil {
				t.Errorf("checkpoint.sh accepted %q — a mistyped flag becomes the recorded intent, and the escalation then reports it as what the run meant to do", flag)
			}
			if _, err := os.Stat(filepath.Join(tmp, "genesis-intent.md")); err == nil {
				t.Errorf("checkpoint.sh created a checkpoint file for %q; the file existing is what suppresses the no-intent diagnosis in escalate.sh", flag)
			}
			// The caller is an agent that will otherwise retry the same guess.
			if !strings.Contains(stderr.String(), "stdin") {
				t.Errorf("refusing %q does not point at stdin as the escape hatch for intent that legitimately starts with a dash; stderr:\n%s", flag, stderr.String())
			}
		})
	}
}

// TestCheckpointHelpWritesNothing — `--help` is the flag agents actually reach
// for, so it is the one that must both work and stay out of the record.
// Supporting it removes the incentive that produced #136 in the first place;
// exiting 0 keeps it from looking like a failure worth investigating turns over.
func TestCheckpointHelpWritesNothing(t *testing.T) {
	script := checkpointPathFor(t)

	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			tmp := t.TempDir()
			cmd := exec.Command("bash", script, flag)
			cmd.Env = append(os.Environ(), "RUNNER_TEMP="+tmp, "GENESIS_CHECKPOINT_FILE=")
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("checkpoint.sh %s exited non-zero (%v) — asking a script what it takes must not read as a failure", flag, err)
			}
			if _, err := os.Stat(filepath.Join(tmp, "genesis-intent.md")); err == nil {
				t.Errorf("checkpoint.sh %s wrote a checkpoint file — this is the exact #136 defect", flag)
			}
			// Usage is only useful if it names the other two ways in.
			for _, want := range []string{"--path", "stdin"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("usage for %s does not mention %q; got:\n%s", flag, want, out)
				}
			}
		})
	}
}
