#!/usr/bin/env bash
# Genesis run outcome — classify HOW a failed agent run died, before anything
# tries to infer WHY from its side effects.
#
# Why this exists. `escalate.sh` splits every failure into one of two turn-budget
# classes and tells the reader which list to append the budget to: deliverable
# landed -> `budgetsFailedDuringWrapUp` (does not move the floor), nothing landed
# -> `budgetsFailedBeforeDelivering` (does move it, mechanically, via
# TestOrchestratorFloorExceedsObservedFailures). That split (#104) was the fix for
# six non-converging budget raises, and it is correct — but it is applied
# unconditionally, and it silently assumes the run died at max-turns at all. The
# only input to the choice is "did an artifact appear?", which cannot tell a
# max-turns death apart from a run that never really started.
#
# #108 and #109 are what that costs. Both runs on 2026-07-30 died with:
#
#     "subtype": "success", "is_error": true, "num_turns": 1, "total_cost_usd": 0
#
# One turn against a `--max-turns 60` budget, zero dollars, ~177s wall clock —
# an API-level failure on the first request, with no relationship to the turn
# budget whatsoever. Both escalations nonetheless printed the #85-signature note
# instructing a reader to append 60 to `budgetsFailedBeforeDelivering`. Following
# that instruction produces budget raise #7 from evidence of a non-budget
# failure, and it does not even fail cleanly: 60 is already in
# `budgetsFailedDuringWrapUp`, so the append also trips
# TestBudgetFailureClassesAreDisjoint and leaves the build red with two
# contradictory demands.
#
# So this is the third class the #104 split missed: NOT A TURN-BUDGET FAILURE.
# The generalizable shape is the one that keeps recurring here — #104 found a
# guard whose check was mechanical but whose input was remembered; this is a
# classifier whose branches are mechanical but whose *precondition* was assumed.
# Splitting a population is only sound once you have established every member
# belongs to it.
#
# The evidence was never missing. `claude-code-action` writes the full SDK
# message list to $RUNNER_TEMP/claude-execution-output.json on the success path
# AND the error path, and the terminal `result` message carries subtype,
# is_error, num_turns, duration and cost. `retain-transcript.sh` already reads
# that exact file one step earlier. Same lesson as #106: check whether the tool
# already produces the artifact before designing a way to produce it.
#
# PUBLIC REPO CONSTRAINT — the reason this reports scalars and not error text.
# Escalation issues are world-readable. The SDK's free-form error/result strings
# can echo prompts, file contents or tool output, which is exactly the leak class
# #106 declined to open when it left `show_full_output` off. So this script emits
# ONLY bounded, structured scalars that cannot carry a payload: a sanitized
# subtype token, a boolean, and three numbers. Everything else stays in the
# private Loki sink, which the escalation already links. Guarded by execution in
# test/devsystem/runoutcome_test.go, because "no leak" is a claim about what a
# process emits and reading shell for it is how leaks happen on public repos.
#
# Output contract (single parse, no drift between a class and its prose):
#   line 1     the class token, one of the CLASS_* values below
#   line 2     blank
#   line 3..   markdown prose for the escalation body
#
# Always exits 0. This is diagnostics, never a deliverable: a classifier that can
# fail hands the caller an error to investigate, and an escalation path that can
# itself fail is the false-escalation bug #91 removed.
#
# Usage:
#   run-outcome.sh          # class token, blank line, prose
#   run-outcome.sh --class  # class token only
#
# Optional env:
#   GENESIS_EXECUTION_FILE  override the SDK message file (tests use this);
#                           default $RUNNER_TEMP/claude-execution-output.json,
#                           the path base-action derives from RUNNER_TEMP
set -uo pipefail

# Class tokens. `max-turns` is the ONLY one under which a turn budget is even a
# candidate explanation; every other class must actively tell the reader to leave
# both budget lists alone, because saying nothing is what let the #85 note run.
CLASS_MAX_TURNS="max-turns"
CLASS_AGENT_ERROR="agent-error"
CLASS_AGENT_OK="agent-ok"
CLASS_NO_OUTPUT="no-agent-output"

EXEC_FILE="${GENESIS_EXECUTION_FILE:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/claude-execution-output.json}"

# emit CLASS  — prose arrives on stdin, as a heredoc at each call site.
#
# Prose on stdin rather than in `$(cat <<EOF ...)`: bash 3.2 (which is what
# macOS still ships, and this repo's tests run locally as well as on Linux CI)
# mis-parses a heredoc nested inside command substitution and dies with
# "unexpected EOF while looking for matching `''". Redirecting into the function
# sidesteps the parser bug entirely and reads better besides.
emit() {
  if [ "${WANT_CLASS_ONLY:-0}" = "1" ]; then
    printf '%s\n' "$1"
    exit 0
  fi
  printf '%s\n\n' "$1"
  cat
  exit 0
}

WANT_CLASS_ONLY=0
if [ "${1:-}" = "--class" ]; then
  WANT_CLASS_ONLY=1
fi

# No execution file. The action writes it on both paths, so its absence means the
# agent step died before the SDK produced any message at all — bad auth, invalid
# input, a cancelled job, or a failure in a step other than the agent. None of
# those are turn budgets, and this is the case most likely to be misread as #85
# (nothing landed, no intent recorded — because nothing ever ran).
if [ ! -s "$EXEC_FILE" ]; then
  emit "$CLASS_NO_OUTPUT" <<EOF
**This run produced no agent output at all, so it is NOT a turn-budget failure.**
There is no SDK message file at \`$EXEC_FILE\`. \`claude-code-action\` writes that
file on its success path and its error path alike, so an absent one means the run
died before the agent emitted a single message — bad credentials, invalid action
input, a cancellation, or a failure in a step other than the agent step.

**Do NOT append anything to \`budgetsFailedBeforeDelivering\` or
\`budgetsFailedDuringWrapUp\`** (test/devsystem/workflows_test.go). Both lists are
histories of \`error_max_turns\` deaths; a run that never reached the model is not
evidence about any budget, and appending to the first list mechanically forces a
floor raise that nothing supports. Check the failed step in the run log instead.
EOF
fi

# One python3 call, the same interpreter retain-transcript.sh relies on. Reads
# the terminal `result` message and returns four bounded scalars plus the class.
# Nothing free-form crosses this boundary; see the PUBLIC REPO note above.
PARSED=$(python3 - "$EXEC_FILE" <<'PY' 2>/dev/null
import json, re, sys

try:
    with open(sys.argv[1]) as fh:
        events = json.load(fh)
except Exception:
    sys.exit(1)

if not isinstance(events, list):
    sys.exit(1)

# The terminal result message. Take the LAST one: a retried run can emit more
# than one, and the death is described by the final one.
result = None
for ev in events:
    if isinstance(ev, dict) and ev.get("type") == "result":
        result = ev
if result is None:
    sys.exit(2)

# Sanitize every field into a shape that cannot carry a payload into a
# world-readable issue: subtype to an identifier-ish token, the rest to numbers
# and a bool. An unrecognized subtype degrades to "unparseable" rather than
# being echoed.
raw_subtype = result.get("subtype")
subtype = ""
if isinstance(raw_subtype, str):
    cleaned = re.sub(r"[^A-Za-z0-9_-]", "", raw_subtype)[:40]
    subtype = cleaned if cleaned == raw_subtype[:40] else "unparseable"

is_error = bool(result.get("is_error"))


def num(key):
    v = result.get(key)
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return ""
    return repr(round(v, 6) if isinstance(v, float) else v)


if subtype == "error_max_turns":
    klass = "max-turns"
elif is_error:
    klass = "agent-error"
else:
    klass = "agent-ok"

print(klass)
print(subtype)
print("true" if is_error else "false")
print(num("num_turns"))
print(num("duration_ms"))
print(num("total_cost_usd"))
print(len(events))
PY
)
PARSE_RC=$?

# An unreadable or result-less file is the same story as a missing one for the
# purpose of the budget lists: no `error_max_turns` was observed, so no budget
# evidence exists. Say which of the two it was, because "the file was corrupt" and
# "the run was killed before its result" want different follow-ups.
if [ "$PARSE_RC" -ne 0 ] || [ -z "$PARSED" ]; then
  if [ "$PARSE_RC" -eq 2 ]; then
    detail="The file parsed but contains no terminal \`result\` message, so the agent step was killed mid-run (a job timeout or a cancelled run) rather than finishing with an error."
  else
    detail="The file at \`$EXEC_FILE\` could not be parsed as an SDK message list, so the run's outcome cannot be read from it."
  fi
  emit "$CLASS_NO_OUTPUT" <<EOF
**No terminal result was recoverable, so this is NOT a confirmed turn-budget failure.**
$detail

**Do NOT append anything to \`budgetsFailedBeforeDelivering\` or
\`budgetsFailedDuringWrapUp\`** (test/devsystem/workflows_test.go) on this evidence.
Both lists are histories of \`error_max_turns\` deaths, and no \`error_max_turns\`
was observed here. The retained transcript linked below is the place to look.
EOF
fi

CLASS=$(printf '%s' "$PARSED" | sed -n '1p')
SUBTYPE=$(printf '%s' "$PARSED" | sed -n '2p')
IS_ERROR=$(printf '%s' "$PARSED" | sed -n '3p')
NUM_TURNS=$(printf '%s' "$PARSED" | sed -n '4p')
DURATION_MS=$(printf '%s' "$PARSED" | sed -n '5p')
COST_USD=$(printf '%s' "$PARSED" | sed -n '6p')
EVENT_COUNT=$(printf '%s' "$PARSED" | sed -n '7p')

# A fixed-shape table of the scalars. This is the part a human reads first, and
# in the #108/#109 shape it settles the question on its own: `num_turns` of 1
# against a `--max-turns` of 60 needs no further argument.
facts=$(printf '| Field | Value |\n| --- | --- |\n| `subtype` | `%s` |\n| `is_error` | `%s` |\n| `num_turns` | `%s` |\n| `duration_ms` | `%s` |\n| `total_cost_usd` | `%s` |\n| SDK messages | `%s` |' \
  "${SUBTYPE:-(absent)}" "${IS_ERROR:-(absent)}" "${NUM_TURNS:-(absent)}" \
  "${DURATION_MS:-(absent)}" "${COST_USD:-(absent)}" "${EVENT_COUNT:-(absent)}")

case "$CLASS" in
  "$CLASS_MAX_TURNS")
    emit "$CLASS_MAX_TURNS" <<EOF
**This run really did die at \`error_max_turns\`.** The turn budget is a candidate
explanation, so the deliverable-landed split below applies — read it and record
the budget in whichever list it names.

$facts
EOF
    ;;
  "$CLASS_AGENT_ERROR")
    emit "$CLASS_AGENT_ERROR" <<EOF
**The agent errored out; it did NOT exhaust its turn budget.** The terminal result
reports \`subtype: $SUBTYPE\` with \`is_error: true\`, not \`error_max_turns\`.

$facts

**Do NOT append \`$NUM_TURNS\` or this workflow's \`--max-turns\` to
\`budgetsFailedBeforeDelivering\` or \`budgetsFailedDuringWrapUp\`**
(test/devsystem/workflows_test.go). Both lists are histories of \`error_max_turns\`
deaths and both are load-bearing: the first mechanically forces
\`orchestratorClassFloor\` upward, so a non-budget failure recorded there produces
an unjustified raise, and appending to both to be safe trips
TestBudgetFailureClassesAreDisjoint. A low \`num_turns\` with \`total_cost_usd\` of
\`0\` is the signature of a request that never billed — auth, quota, or an upstream
API failure. Read the retained transcript below before changing anything.
EOF
    ;;
  *)
    emit "$CLASS_AGENT_OK" <<EOF
**The agent finished without erroring, so the failure is downstream of it — and it
is NOT a turn-budget failure.** The terminal result reports \`subtype: $SUBTYPE\`
with \`is_error: false\`, meaning a later step in the job is what failed.

$facts

**Do NOT append anything to \`budgetsFailedBeforeDelivering\` or
\`budgetsFailedDuringWrapUp\`** (test/devsystem/workflows_test.go). The agent
completed; no budget was exhausted. Find the failing step in the run log.
EOF
    ;;
esac
