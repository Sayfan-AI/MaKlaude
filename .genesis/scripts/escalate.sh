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
# Required env:
#   GH_TOKEN  token with issues:write (the workflow's app token)
#   GH_REPO   owner/repo
#   WF_NAME   the failing workflow's name
#   RUN_URL   URL of the failed run
set -euo pipefail

: "${GH_TOKEN:?GH_TOKEN required}"
: "${GH_REPO:?GH_REPO required}"
WF_NAME="${WF_NAME:-unknown workflow}"
RUN_URL="${RUN_URL:-(run url unavailable)}"

# Stable per-workflow dedup key. Hidden HTML comment so it never renders but is
# reliably greppable in the issue body.
marker="<!-- genesis-failure-wf: ${WF_NAME} -->"

# Reuse an open escalation issue for THIS workflow if one exists, so repeated
# failures of the same workflow append context instead of spawning duplicates.
# Different workflows get separate issues.
existing=$(gh issue list --state open --label "automation:failure" --json number,body \
  | jq -r --arg m "$marker" '[.[] | select(.body | contains($m)) | .number] | first // empty')

body=$(printf 'A workflow run failed and the loop could not self-advance.\n\n- Workflow: **%s**\n- Failed run: %s\n\nLikely cause: the agent hit max-turns, an API error, or an unrecoverable task. Review the run, unblock, and close this issue once resolved.\n\n%s' "$WF_NAME" "$RUN_URL" "$marker")

if [ -n "$existing" ]; then
  gh issue comment "$existing" --body "$body"
else
  gh issue create \
    --title "Autonomous system needs help: ${WF_NAME} run failed" \
    --label "needs:human" --label "automation:failure" \
    --body "$body"
fi
