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
  intent='No intent checkpoint was recorded. Either the run died before its first action, or it skipped the checkpoint step — if there is also no artifact below, this run left no trace of its reasoning and the transcript in the run log is the only source.'
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

if [ -n "$artifacts" ]; then
  landed=$(printf 'Touched since %s — **triage these before assuming the run achieved nothing**:\n\n%s\n\nIf the deliverable is already here (a green PR, a posted diagnosis), the run lost only its wrap-up: finish the bookkeeping and close this issue rather than redoing the work.\n\n%s' "$since" "$artifacts" "$WRAPUP_NOTE")
else
  landed=$(printf 'No issue or PR was touched since %s. Also check for a pushed branch with no PR before concluding nothing landed.\n\n%s' "$since" "$NO_ARTIFACT_NOTE")
fi

# Stable per-workflow dedup key. Hidden HTML comment so it never renders but is
# reliably greppable in the issue body.
marker="<!-- genesis-failure-wf: ${WF_NAME} -->"

# Reuse an open escalation issue for THIS workflow if one exists, so repeated
# failures of the same workflow append context instead of spawning duplicates.
# Different workflows get separate issues.
existing=$(gh issue list --state open --label "automation:failure" --json number,body \
  | jq -r --arg m "$marker" '[.[] | select(.body | contains($m)) | .number] | first // empty')

body=$(printf 'A workflow run failed and the loop could not self-advance.\n\n- Workflow: **%s**\n- Failed run: %s\n\nLikely cause: the agent hit max-turns, an API error, or an unrecoverable task.\n\n### What this run was trying to do\n\n%s\n\n### What this run may already have landed\n\n%s\n\n%s' "$WF_NAME" "$RUN_URL" "$intent" "$landed" "$marker")

if [ -n "$existing" ]; then
  gh issue comment "$existing" --body "$body"
else
  gh issue create \
    --title "Autonomous system needs help: ${WF_NAME} run failed" \
    --label "needs:human" --label "automation:failure" \
    --body "$body"
fi
