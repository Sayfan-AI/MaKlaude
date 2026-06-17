#!/usr/bin/env bash
# Genesis failure escalation — open (or update) a single needs:human issue when an
# autonomous run fails (e.g. the agent hit max-turns, an API error, or an
# unrecoverable task), so a human is never left in the dark. Deterministic: no LLM
# in this path. Called from each orchestrator/merge/triage workflow's
# `if: failure()` step (one shared script instead of duplicated inline YAML).
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

# Dedup on the dedicated label: reuse an open escalation issue if one exists, so
# repeated failures append context instead of spawning a pile of duplicates.
existing=$(gh issue list --state open --label "automation:failure" --json number --jq '.[0].number // empty')

body=$(printf 'A workflow run failed and the loop could not self-advance.\n\n- Workflow: **%s**\n- Failed run: %s\n\nLikely cause: the agent hit max-turns, an API error, or an unrecoverable task. Review the run, unblock, and close this issue once resolved.' "$WF_NAME" "$RUN_URL")

if [ -n "$existing" ]; then
  gh issue comment "$existing" --body "$body"
else
  gh issue create \
    --title "Autonomous system needs help: a workflow run failed" \
    --label "needs:human" --label "automation:failure" \
    --body "$body"
fi
