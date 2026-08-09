package devsystem

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Why these tests: #104 split the max-turns failure history into two classes with
// opposite fixes — deliverable landed (`budgetsFailedDuringWrapUp`, does not move
// the floor) and nothing landed (`budgetsFailedBeforeDelivering`, mechanically
// does). That split was right and it stopped six non-converging budget raises.
// What it did not do is check its own precondition: the escalation applied the
// split to EVERY failure, using artifact presence as the only input, and artifact
// presence cannot distinguish a max-turns death from a run that never reached the
// model.
//
// #108 and #109 are what that costs. Both died with `subtype: success`,
// `is_error: true`, `num_turns: 1`, `total_cost_usd: 0` — one turn against a
// `--max-turns 60` budget, an API failure on the first request — and both
// escalations printed the #85-signature note telling a reader to append 60 to
// `budgetsFailedBeforeDelivering`. Following it yields budget raise #7 from
// evidence about something other than budgets, and it does not even fail cleanly:
// 60 already sits in `budgetsFailedDuringWrapUp`, so the append additionally trips
// TestBudgetFailureClassesAreDisjoint and leaves two contradictory red tests.
//
// So the property to guard is that the *precondition* holds before the split
// runs: an escalation may only route a budget into either list when the run
// actually died at `error_max_turns`. Tested by execution rather than review,
// because the classifier reads a real SDK message file and the failure mode is a
// wrong branch, not absent code — and because the same execution proves the
// second property, which the public-repo constraint makes non-negotiable: the
// classifier must emit only bounded scalars, never the SDK's free-form error
// text, since escalation issues are world-readable (#106's reason for leaving
// `show_full_output` off).

const runOutcomeScript = ".genesis/scripts/run-outcome.sh"

func runOutcomePathFor(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", runOutcomeScript)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %s: %v", runOutcomeScript, err)
	}
	return p
}

// execFixture writes an SDK message file whose terminal `result` carries the
// given fields, plus a preceding assistant message holding a marker string. The
// marker stands in for everything a real transcript contains that must never
// reach a public issue — prompt text, file contents, tool output.
const leakMarker = "SUPERSECRET-TRANSCRIPT-CONTENT"

func execFixture(t *testing.T, dir string, result map[string]any) string {
	t.Helper()
	return execFixtureEvents(t, dir, nil, result)
}

// execFixtureEvents is execFixture with extra events (e.g. `api_retry` system
// messages) inserted between the boilerplate and the terminal result, matching
// where the SDK emits them.
func execFixtureEvents(t *testing.T, dir string, extra []any, result map[string]any) string {
	t.Helper()
	events := []any{
		map[string]any{"type": "system", "subtype": "init", "model": "claude-opus-5[1m]"},
		map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": leakMarker}},
			},
		},
	}
	events = append(events, extra...)
	if result != nil {
		events = append(events, result)
	}
	b, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	p := filepath.Join(dir, "claude-execution-output.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// runOutcome executes the classifier against a fixture and returns its class
// token and prose. Non-zero exit is a failure in itself: this is diagnostics on
// the failure path, and a classifier that can fail turns an escalation into the
// false-escalation bug #91 removed.
func runOutcome(t *testing.T, execFile string, args ...string) (class, prose, stderr string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{runOutcomePathFor(t)}, args...)...)
	cmd.Env = append(os.Environ(), "GENESIS_EXECUTION_FILE="+execFile)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s exited non-zero (%v)\nstdout: %s\nstderr: %s", runOutcomeScript, err, out.String(), errb.String())
	}
	lines := strings.SplitN(out.String(), "\n", 3)
	class = strings.TrimSpace(lines[0])
	if len(lines) == 3 {
		prose = lines[2]
	}
	return class, prose, errb.String()
}

// budgetLists are the two histories in workflows_test.go that an escalation may
// route a budget into. Naming either one is an instruction to record a
// `--max-turns` value as evidence, so no non-max-turns class may mention them
// except to forbid the append.
var budgetLists = []string{"budgetsFailedBeforeDelivering", "budgetsFailedDuringWrapUp"}

// TestRunOutcomeClassifiesTheDeathMode is the core precondition check. The
// #108/#109 case is called out by name because it is the one that actually
// happened, and the one whose misclassification is silently expensive.
func TestRunOutcomeClassifiesTheDeathMode(t *testing.T) {
	cases := []struct {
		name      string
		result    map[string]any
		wantClass string
	}{
		{
			// The only class under which a turn budget is a candidate explanation.
			name: "genuine max-turns death",
			result: map[string]any{
				"type": "result", "subtype": "error_max_turns", "is_error": true,
				"num_turns": 60, "duration_ms": 900000, "total_cost_usd": 4.2,
			},
			wantClass: "max-turns",
		},
		{
			// #108 and #109 verbatim: one turn, zero dollars, is_error true, and a
			// subtype of "success" that reads like the opposite of what happened.
			name: "first-turn API error reported as subtype success",
			result: map[string]any{
				"type": "result", "subtype": "success", "is_error": true,
				"num_turns": 1, "duration_ms": 177026, "total_cost_usd": 0,
			},
			wantClass: "agent-error",
		},
		{
			// The agent finished; something later in the job failed. Nothing to do
			// with turns, and the artifact list will look identical to a real death.
			name: "agent succeeded and a later step failed",
			result: map[string]any{
				"type": "result", "subtype": "success", "is_error": false,
				"num_turns": 12, "duration_ms": 60000, "total_cost_usd": 0.5,
			},
			wantClass: "agent-ok",
		},
		{
			// Killed mid-run (job timeout, cancellation): messages but no result.
			name:      "no terminal result message",
			result:    nil,
			wantClass: "no-agent-output",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := execFixture(t, t.TempDir(), tc.result)
			class, prose, _ := runOutcome(t, f)
			if class != tc.wantClass {
				t.Fatalf("classified as %q, want %q — the escalation branches on this token, and a wrong branch attaches the wrong budget instruction to the failure (#108, #109)", class, tc.wantClass)
			}
			if strings.TrimSpace(prose) == "" {
				t.Fatalf("class %q produced no prose — the escalation embeds this section, and an empty one reads as \"cause unknown\", which is the state this classifier exists to end", class)
			}

			// Every non-max-turns class must actively forbid the append. Silence is
			// what let the #85 note run on evidence that never supported it.
			if tc.wantClass == "max-turns" {
				return
			}
			if !strings.Contains(prose, "Do NOT append") {
				t.Errorf("class %q does not tell the reader to leave the budget lists alone — this is the sentence that stops budget raise #7 (#108, #109). Prose:\n%s", class, prose)
			}
			for _, list := range budgetLists {
				if !strings.Contains(prose, list) {
					t.Errorf("class %q never names %s — the forbidding has to be specific, because \"append to both to be safe\" trips TestBudgetFailureClassesAreDisjoint and is the shortcut a hurried reader takes", class, list)
				}
			}
		})
	}
}

// TestRunOutcomeReportsTheScalarsThatSettleIt pins the facts, not just the verdict.
// `num_turns: 1` printed next to a `--max-turns` of 60 is the whole argument in
// the #108/#109 shape; a verdict without its evidence is one a human cannot
// overrule when the classifier is the thing that is wrong.
func TestRunOutcomeReportsTheScalarsThatSettleIt(t *testing.T) {
	f := execFixture(t, t.TempDir(), map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"num_turns": 1, "duration_ms": 177026, "total_cost_usd": 0,
	})
	_, prose, _ := runOutcome(t, f)

	for _, want := range []string{"num_turns", "1", "total_cost_usd", "duration_ms", "subtype", "is_error"} {
		if !strings.Contains(prose, want) {
			t.Errorf("outcome prose omits %q — without the scalars the reader has a verdict and no way to check it:\n%s", want, prose)
		}
	}
}

// TestRunOutcomeNamesTheAPICause pins #151: four escalations (#108, #109, #149,
// #150) told a reader "auth, quota, or an upstream API failure — read the
// retained transcript" about a failure whose cause was `api_error_status: 401`,
// a bounded integer sitting in the same terminal result the classifier had
// already parsed. It stopped one field short. The classifier must read the
// cause fields when they exist, name the cause — for 401, the action a human
// can take, since no agent here can rotate a secret — and fall back to the
// three-way guess only when the evidence genuinely is not in the file.
func TestRunOutcomeNamesTheAPICause(t *testing.T) {
	apiRetry := func(status int, errTok string, attempt, maxRetries int) map[string]any {
		return map[string]any{
			"type": "system", "subtype": "api_retry",
			"error": errTok, "error_status": status,
			"attempt": attempt, "max_retries": maxRetries,
		}
	}
	errResult := func(fields map[string]any) map[string]any {
		r := map[string]any{
			"type": "result", "subtype": "success", "is_error": true,
			"num_turns": 1, "duration_ms": 177026, "total_cost_usd": 0,
		}
		for k, v := range fields {
			r[k] = v
		}
		return r
	}
	guess := "auth, quota, or an upstream"

	t.Run("401 names the credential and the human action", func(t *testing.T) {
		f := execFixtureEvents(t, t.TempDir(),
			[]any{apiRetry(401, "authentication_failed", 9, 10), apiRetry(401, "authentication_failed", 10, 10)},
			errResult(map[string]any{"terminal_reason": "api_error", "api_error_status": 401}))
		class, prose, _ := runOutcome(t, f)
		if class != "agent-error" {
			t.Fatalf("classified as %q, want agent-error — the cause fields must not change the class, only the prose", class)
		}
		for _, want := range []string{"401", "ANTHROPIC_API_KEY", "rotate", "authentication_failed", "10/10"} {
			if !strings.Contains(prose, want) {
				t.Errorf("the 401 verdict omits %q — this is the line that replaces a three-way guess with the action a human can take (#151):\n%s", want, prose)
			}
		}
		if strings.Contains(prose, guess) {
			t.Errorf("the three-way guess still renders when the artifact names the cause — enumerating candidates next to the answer re-creates the ambiguity #151 removed:\n%s", prose)
		}
	})

	t.Run("unrecognized status still reports the number", func(t *testing.T) {
		f := execFixture(t, t.TempDir(), errResult(map[string]any{"terminal_reason": "api_error", "api_error_status": 418}))
		_, prose, _ := runOutcome(t, f)
		if !strings.Contains(prose, "418") {
			t.Errorf("an unmapped status is dropped instead of reported — the integer is the evidence even when no canned action exists:\n%s", prose)
		}
		if strings.Contains(prose, guess) {
			t.Errorf("a known status fell through to the guess:\n%s", prose)
		}
	})

	t.Run("retry evidence alone is enough for the status", func(t *testing.T) {
		f := execFixtureEvents(t, t.TempDir(),
			[]any{apiRetry(401, "authentication_failed", 10, 10)},
			errResult(nil))
		_, prose, _ := runOutcome(t, f)
		if !strings.Contains(prose, "ANTHROPIC_API_KEY") {
			t.Errorf("a 401 visible only in the api_retry messages was not surfaced — the retry stream carries the same bounded status the result does:\n%s", prose)
		}
	})

	t.Run("no cause fields falls back to the guess", func(t *testing.T) {
		f := execFixture(t, t.TempDir(), errResult(nil))
		_, prose, _ := runOutcome(t, f)
		if !strings.Contains(prose, guess) {
			t.Errorf("with no cause evidence in the file the prose must say the cause is unknown and where to look, not stay silent:\n%s", prose)
		}
	})
}

// TestRunOutcomeLeaksNoTranscriptContent is the public-repo constraint, and the
// reason this suite executes the script instead of reading it. The escalation
// body lands in a world-readable issue; the SDK's free-form error and result
// strings can echo prompts, file contents and tool output. #106 declined to turn
// on `show_full_output` for exactly this reason, and a classifier that quotes the
// error text would open the same hole through the back door.
func TestRunOutcomeLeaksNoTranscriptContent(t *testing.T) {
	dir := t.TempDir()
	f := execFixture(t, dir, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"num_turns": 1, "duration_ms": 177026, "total_cost_usd": 0,
		// Both fields a real SDK result can carry free-form text in.
		"result": leakMarker + " in result",
		"error":  leakMarker + " in error",
	})

	class, prose, stderr := runOutcome(t, f)
	if class != "agent-error" {
		t.Fatalf("classified as %q, want agent-error", class)
	}
	for name, s := range map[string]string{"prose": prose, "stderr": stderr} {
		if strings.Contains(s, leakMarker) {
			t.Errorf("%s contains transcript content (%q) — escalation issues are world-readable on this public repo, so only bounded scalars may cross this boundary (#106)", name, leakMarker)
		}
	}

	// A hostile subtype must not smuggle markup or content into the issue either;
	// it degrades to a fixed token rather than being echoed.
	f2 := execFixture(t, t.TempDir(), map[string]any{
		"type": "result", "subtype": "error " + leakMarker + " <img src=x>", "is_error": true,
		"num_turns": 1, "duration_ms": 100, "total_cost_usd": 0,
	})
	_, prose2, _ := runOutcome(t, f2)
	if strings.Contains(prose2, leakMarker) {
		t.Errorf("a free-form subtype reached the escalation body verbatim — sanitize it to an identifier or degrade it to a fixed token:\n%s", prose2)
	}

	// The #151 cause fields are string-typed too, and they cross the same
	// boundary: a hostile terminal_reason or retry error token must degrade the
	// same way the subtype does, or the cause line becomes the new back door.
	f3 := execFixtureEvents(t, t.TempDir(),
		[]any{map[string]any{
			"type": "system", "subtype": "api_retry",
			"error": leakMarker + " <img src=x>", "error_status": 401,
			"attempt": 10, "max_retries": 10,
		}},
		map[string]any{
			"type": "result", "subtype": "success", "is_error": true,
			"num_turns": 1, "duration_ms": 100, "total_cost_usd": 0,
			"terminal_reason": "api " + leakMarker, "api_error_status": 401,
		})
	_, prose3, _ := runOutcome(t, f3)
	if strings.Contains(prose3, leakMarker) {
		t.Errorf("a free-form cause field reached the escalation body verbatim — terminal_reason and the retry error must be sanitized like the subtype (#151):\n%s", prose3)
	}
}

// TestRunOutcomeReportsAbsenceRatherThanGuessing covers the case most likely to be
// misread as #85: no execution file at all. `claude-code-action` writes that file
// on its success path and its error path alike, so an absent one means the run
// died before the agent emitted anything — which produces no artifact and no
// intent checkpoint, i.e. it is indistinguishable from #85 from the artifact list
// alone. It must be named, and it must not move a floor.
func TestRunOutcomeReportsAbsenceRatherThanGuessing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	class, prose, _ := runOutcome(t, missing)
	if class != "no-agent-output" {
		t.Fatalf("a missing execution file classified as %q, want no-agent-output", class)
	}
	if !strings.Contains(prose, "Do NOT append") {
		t.Errorf("a run that never reached the model does not forbid the budget append — it produces the same empty artifact list as #85 and is the likeliest misclassification:\n%s", prose)
	}

	// A corrupt file is the same story for the budget lists, and must not crash.
	dir := t.TempDir()
	bad := filepath.Join(dir, "claude-execution-output.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	class, prose, _ = runOutcome(t, bad)
	if class != "no-agent-output" {
		t.Fatalf("a corrupt execution file classified as %q, want no-agent-output", class)
	}
	if !strings.Contains(prose, "Do NOT append") {
		t.Errorf("a corrupt execution file does not forbid the budget append:\n%s", prose)
	}
}

// TestRunOutcomeClassOnlyMode pins the `--class` contract. escalate.sh parses line
// 1 of the default output, so the two must agree; a drift here reproduces the
// checkpoint/transcript path-drift bug in a new place, where the symptom is an
// escalation branching on a token the script never emits.
func TestRunOutcomeClassOnlyMode(t *testing.T) {
	f := execFixture(t, t.TempDir(), map[string]any{
		"type": "result", "subtype": "error_max_turns", "is_error": true,
		"num_turns": 60, "duration_ms": 900000, "total_cost_usd": 4.2,
	})
	full, _, _ := runOutcome(t, f)
	only, _, _ := runOutcome(t, f, "--class")
	if full != only {
		t.Errorf("--class returned %q but line 1 of the default output is %q — escalate.sh reads line 1, so the two must not drift", only, full)
	}
}

// TestEscalationGatesBudgetAdviceOnTheDeathClass is the membership half: the
// classifier is worthless if the escalation still routes budgets unconditionally.
// This asserts escalate.sh actually consults it and branches on the result.
func TestEscalationGatesBudgetAdviceOnTheDeathClass(t *testing.T) {
	src := readFileString(t, escalatePathFor(t))

	if !strings.Contains(src, "run-outcome.sh") {
		t.Fatalf("%s never invokes %s — without it the escalation infers the failure class from artifact presence alone, which is what made #108 and #109 recommend budget raise #7", escalateScript, runOutcomeScript)
	}

	// The #85 note is the one that mechanically moves orchestratorClassFloor, so
	// its render must be conditional on the class rather than on artifacts alone.
	if !strings.Contains(src, `"$death_class" = "max-turns"`) {
		t.Errorf("%s does not gate its budget notes on a confirmed `max-turns` death — the two lists in workflows_test.go are histories of `error_max_turns` deaths, and routing any other failure into them is unsupported evidence that forces a floor raise", escalateScript)
	}

	// The outcome must actually reach the issue body, ahead of the sections whose
	// reading depends on it.
	if !strings.Contains(src, "How this run died") {
		t.Errorf("%s builds an escalation body without a death-class section — the class changes how every section below it should be read", escalateScript)
	}
	howIdx := strings.Index(src, "### How this run died")
	landedIdx := strings.Index(src, "### What this run may already have landed")
	if howIdx < 0 || landedIdx < 0 || howIdx > landedIdx {
		t.Errorf("the death-class section does not precede the landed-artifacts section in the escalation body — a reader reaches the budget instruction before learning the run used one turn")
	}
}
