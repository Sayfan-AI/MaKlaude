#!/usr/bin/env bash
# Genesis failure escalation — open (or update) a needs:human issue when an
# autonomous run fails (e.g. the agent hit max-turns, an API error, or an
# unrecoverable task), so a human is never left in the dark. Deterministic: no LLM
# in this path. Called from each orchestrator/merge/triage workflow's
# `if: failure()` step (one shared script instead of duplicated inline YAML).
#
# Dedup is PER WORKFLOW, not global. An earlier version reused any open
# automation:failure issue, so two different workflows failing in the same window
# landed in one issue (see #38: Genesis Evolver + Genesis Orchestrator conflated).
# That made triage harder — an agent had to untangle which failures belonged to
# which workflow — and risked closing the issue while another workflow's failure
# was still unresolved, plus it muddied the per-workflow failure cadence the
# evolver reads to tell recurring from one-off. Keying dedup on the workflow name
# (via a stable hidden marker) gives at most one open issue per workflow: clean
# cadence, clean triage, bounded issue count. Mirrors internal/escalate's
# issue-per-problem design for cluster findings.
#
# The escalation also reports WHAT THE FAILED RUN LANDED, because "the run died"
# is not actually the question a human has — "did anything land, or is the repo
# where it was?" is. A run that hits max-turns has usually already produced its
# deliverable and lost only the wrap-up: #87 died at turn 41 seconds after
# opening PR #86, #96 died at turn 46 seconds after merging PR #95. Both
# escalations said only "run failed", so both cost a human a manual hunt, and
# #87's fix sat in an auto-merged PR while its escalation stayed open. CLAUDE.md
# records "verify the artifact first" as a rule an agent must remember; this
# makes it something the escalation just tells you (#97).
#
# The escalation also reports WHAT THE RUN WAS TRYING TO DO, when the agent
# recorded it via checkpoint.sh. Artifact discovery answers "did anything land?";
# it cannot answer "why did it think that was the right thing to land?", and a
# run that died producing NO artifact (#85) gets nothing at all from it. The
# design doc named intent checkpointing as the next thing to try if max-turns
# deaths continued past artifact discovery; #101 was that continued death.
#
# Required env:
#   GH_TOKEN  token with issues:write (the workflow's app token)
#   GH_REPO   owner/repo
#   WF_NAME   the failing workflow's name
#   RUN_URL   URL of the failed run
# Optional env:
#   GENESIS_RUN_STARTED           ISO8601 UTC start of the failed run; when unset,
#                                 falls back to a lookback window
#   GENESIS_ARTIFACT_LOOKBACK_MIN lookback in minutes for that fallback (default 120)
set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN required}"
: "${GH_REPO:?GH_REPO required}"
WF_NAME="${WF_NAME:-unknown workflow}"
RUN_URL="${RUN_URL:-(run url unavailable)}"
LOOKBACK_MIN="${GENESIS_ARTIFACT_LOOKBACK_MIN:-120}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The intent the agent checkpointed before it started work, if any. Ask
# checkpoint.sh where the file is instead of recomputing its default — two
# scripts deriving one shared path independently drift, and the failure mode is
# this section reporting "no intent recorded" while the file sits on disk.
CHECKPOINT_FILE="$(bash "$SCRIPT_DIR/checkpoint.sh" --path 2>/dev/null || true)"
if [ -n "$CHECKPOINT_FILE" ] && [ -s "$CHECKPOINT_FILE" ]; then
  intent=$(printf 'The agent recorded this before starting work. It is what the run *meant* to do — treat it as a hypothesis to check against what actually landed below, not as a description of what happened.\n\n%s' "$(cat "$CHECKPOINT_FILE")")
else
  # Not a neutral absence: either the run died before its first write (the #85
  # shape — no artifact, no intent, and the costliest failure in the system), or
  # the agent skipped the instruction. Both are worth saying out loud.
  intent='No intent checkpoint was recorded. Either the run died before its first action, or it skipped the checkpoint step — if there is also no artifact below, this run left no trace of its stated reasoning and the retained transcript (see below) is the only source.'
fi

# Where the transcript went. Intent says what the run meant to do and artifacts
# say what it produced; neither says where the turns actually WENT — which
# approaches it tried, which dead ends it burned turns on. That is the middle of
# the run, and it used to die with the runner: the Actions log holds `init` +
# `result` and zero tool calls (#104 measured 469 lines). retain-transcript.sh
# copies the full SDK message list to the private Loki sink after the agent step
# (#106). Ask it for its status path rather than recomputing the default, for the
# same reason as the checkpoint above — two derivations of one path drift, and
# the symptom is this section claiming nothing was retained while it sits on disk.
TRANSCRIPT_STATUS="$(bash "$SCRIPT_DIR/retain-transcript.sh" --path 2>/dev/null || true)"
if [ -n "$TRANSCRIPT_STATUS" ] && [ -s "$TRANSCRIPT_STATUS" ]; then
  transcript="$(cat "$TRANSCRIPT_STATUS")"
else
  # The retention step is `if: always()`, so a missing status means it never ran
  # at all — the job died before reaching it, or the workflow is missing the
  # step (which test/devsystem/transcript_test.go exists to prevent).
  transcript='**Transcript: unknown** — no retention status was written, so the `retain-transcript.sh` step did not run. Either the job was torn down before reaching it, or this workflow is missing the step; `test/devsystem/transcript_test.go` asserts every Claude-invoking workflow has one.'
fi

# HOW the run died, established before anything infers WHY from its side effects.
#
# The deliverable-landed split below is the #104 fix for six non-converging
# budget raises, and it is right — but it was applied unconditionally, and it
# quietly assumed the run died at max-turns at all. Artifact presence cannot tell
# a max-turns death apart from a run that never reached the model. #108 and #109
# are the bill: both died `subtype: success, is_error: true, num_turns: 1,
# total_cost_usd: 0` — one turn against a budget of 60, an API failure on the
# first request — and both escalations printed the #85 note telling a reader to
# append 60 to `budgetsFailedBeforeDelivering`. That is raise #7 from non-budget
# evidence, and it does not even fail cleanly: 60 is already in
# `budgetsFailedDuringWrapUp`, so the append also trips
# TestBudgetFailureClassesAreDisjoint.
#
# So the split only runs once `error_max_turns` is confirmed. run-outcome.sh
# reads the terminal `result` message from the SDK output file that
# `retain-transcript.sh` already consumes one step earlier, and returns its class
# on line 1 with prose from line 3. Every non-max-turns class carries its own
# "do NOT touch either budget list" instruction, because saying nothing about the
# budget is exactly what let the #85 note run on evidence that never supported it.
outcome_raw="$(bash "$SCRIPT_DIR/run-outcome.sh" 2>/dev/null || true)"
death_class="$(printf '%s' "$outcome_raw" | sed -n '1p')"
outcome="$(printf '%s' "$outcome_raw" | tail -n +3)"
if [ -z "$outcome" ]; then
  # Same posture as every other section here: absence is reported, not omitted,
  # and it withholds the budget instruction rather than guessing a class.
  death_class="unknown"
  outcome='**How this run died could not be determined** — `run-outcome.sh` returned nothing, so the terminal SDK result was not readable. Do not append this workflow'"'"'s `--max-turns` to either budget list on this evidence; no `error_max_turns` was observed.'
fi

# Whether a deliverable landed is not just triage colour — it is the ONLY signal
# that separates the two max-turns failure classes, and they have opposite fixes.
# Six consecutive budget raises (15→30→40→45→60) failed to converge because both
# classes were read as "the budget is too low". A death *after* the deliverable
# landed says nothing about the budget: implementation cost is unbounded (it
# scales with whatever task the run chose mid-run), so for any floor N some task
# overruns it. Only a death with nothing landed is evidence N is too small.
#
# The escalation is where that call actually gets made, so it states the rule
# rather than leaving it to be remembered. Guarded by the two lists in
# test/devsystem/workflows_test.go; reasoning in
# .genesis/design/agent-turn-budgets.md.
WRAPUP_NOTE='**If the deliverable landed, this is a wrap-up truncation — do NOT raise `--max-turns`.** No floor fixes this class: implementation cost scales with the task the run picked, so any budget can be overrun. Record the budget in `budgetsFailedDuringWrapUp` (test/devsystem/workflows_test.go) and, if a human-facing output was lost, move that output outside the agent step the way `nudge-gates.sh`/`escalate.sh`/`checkpoint.sh` already are. See #97, #101.'

NO_ARTIFACT_NOTE='**If nothing landed and no intent was recorded above, this is the #85 signature** — the run died before its first deliverable, which is the one case where the turn budget genuinely is too small. Append this workflow'"'"'s `--max-turns` to `budgetsFailedBeforeDelivering` (test/devsystem/workflows_test.go); that fails the build until `orchestratorClassFloor` moves above it.'

# The window this run could plausibly have written in. A lookback can only
# over-report (an artifact from an adjacent run), which is a bounded and honest
# error — every repo-mutating agent shares one concurrency group, so overlap is
# rare — whereas under-reporting recreates the exact "run failed, nothing else
# said" triage cost this section exists to remove.
since="${GENESIS_RUN_STARTED:-$(date -u -d "-${LOOKBACK_MIN} minutes" +%Y-%m-%dT%H:%M:%SZ)}"

# REST issues endpoint, NOT `gh issue list --search`: search is index-lagged by
# up to a minute or two and the artifacts that matter here were created seconds
# before the run died, so search would routinely miss precisely the ones worth
# reporting. `since` + `sort=updated` is served from the primary and is exact.
# The endpoint returns pull requests as well as issues (a PR is an issue with a
# `pull_request` key), so one call covers both; an issue comment bumps
# `updated_at`, so a posted diagnosis shows up too.
artifacts=$(gh api --method GET "repos/${GH_REPO}/issues" \
  -f state=all -f sort=updated -f direction=desc -f per_page=30 -f since="$since" \
  --jq '.[] | "- \(if .pull_request then "PR" else "Issue" end) #\(.number) (\(.state)) — \(.title)\n  \(.html_url)"' \
  2>/dev/null || true)

# The budget notes are gated on the death class, not merely on artifact presence.
# Artifact discovery is still worth printing for every class — "did anything
# land?" is the first question a human has regardless of why the run died — but
# the *instruction* attached to it is only sound when the run actually exhausted
# its turns. For every other class the outcome section above already carries the
# correct instruction, which is to leave both lists alone.
if [ "$death_class" = "max-turns" ]; then
  wrapup_note="$WRAPUP_NOTE"
  no_artifact_note="$NO_ARTIFACT_NOTE"
else
  wrapup_note='This run did not die at `error_max_turns` (see the section above), so nothing here is evidence about the turn budget — triage the artifacts on their own terms.'
  no_artifact_note='This run did not die at `error_max_turns` (see the section above), so **an empty list here is not the #85 signature** and must not be read as one. #85 is specifically a max-turns death that produced nothing; a run that errored or never started produces nothing too, and looks identical from here.'
fi

if [ -n "$artifacts" ]; then
  landed=$(printf 'Touched since %s — **triage these before assuming the run achieved nothing**:\n\n%s\n\nIf the deliverable is already here (a green PR, a posted diagnosis), the run lost only its wrap-up: finish the bookkeeping and close this issue rather than redoing the work.\n\n%s' "$since" "$artifacts" "$wrapup_note")
else
  landed=$(printf 'No issue or PR was touched since %s. Also check for a pushed branch with no PR before concluding nothing landed.\n\n%s' "$since" "$no_artifact_note")
fi

# Stable per-workflow dedup key. Hidden HTML comment so it never renders but is
# reliably greppable in the issue body.
marker="<!-- genesis-failure-wf: ${WF_NAME} -->"

# Reuse an open escalation issue for THIS workflow if one exists, so repeated
# failures of the same workflow append context instead of spawning duplicates.
# Different workflows get separate issues.
existing=$(gh issue list --state open --label "automation:failure" --json number,body \
  | jq -r --arg m "$marker" '[.[] | select(.body | contains($m)) | .number] | first // empty')

# "How this run died" comes FIRST among the diagnostic sections on purpose: every
# section under it — intent, artifacts, and the budget instruction attached to
# them — is read differently depending on the answer, and putting it last is how
# a reader reaches the #85 note before learning the run used one turn.
body=$(printf 'A workflow run failed and the loop could not self-advance.\n\n- Workflow: **%s**\n- Failed run: %s\n\n### How this run died\n\n%s\n\n### What this run was trying to do\n\n%s\n\n### What this run may already have landed\n\n%s\n\n### Where the transcript is\n\n%s\n\n%s' "$WF_NAME" "$RUN_URL" "$outcome" "$intent" "$landed" "$transcript" "$marker")

if [ -n "$existing" ]; then
  # A repeat failure is not an isolated incident, and the escalation must say so
  # in its own body rather than leaving the count buried in a comment thread
  # nobody re-reads. The 3.5-day outage of 2026-07-30 (#150) presented, to anyone
  # scanning the issue list, as two issues titled "a run failed" — 14 consecutive
  # failures were visible only by opening #108 and counting (#151). The streak is
  # derived from the issue itself: the body plus every prior comment carrying
  # this workflow's dedup marker is one failure each, and this comment makes N.
  # Best-effort on purpose — a failed read degrades to the plain body, because an
  # escalation path that can itself fail is the false-escalation bug #91 removed.
  streak_info="$(gh issue view "$existing" --json createdAt,comments 2>/dev/null || true)"
  if [ -n "$streak_info" ]; then
    first_seen="$(printf '%s' "$streak_info" | jq -r '.createdAt // empty' 2>/dev/null || true)"
    prior="$(printf '%s' "$streak_info" | jq -r --arg m "$marker" '[.comments[]? | select(.body | contains($m))] | length' 2>/dev/null || true)"
    if [ -n "$first_seen" ] && [ -n "$prior" ]; then
      streak_n=$((prior + 2))
      body=$(printf '**This is failure %s of this workflow in an unbroken sequence since %s.** One failure is an incident; a streak is an outage — the cause is almost certainly the one already diagnosed above, so read the newest prior comment before treating this one as new information.\n\n%s' "$streak_n" "$first_seen" "$body")
    fi
  fi
  gh issue comment "$existing" --body "$body"
else
  gh issue create \
    --title "Autonomous system needs help: ${WF_NAME} run failed" \
    --label "needs:human" --label "automation:failure" \
    --body "$body"
fi
