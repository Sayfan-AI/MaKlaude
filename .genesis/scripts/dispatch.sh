#!/usr/bin/env bash
# Genesis workflow dispatch — the one way this dev system fires a
# workflow_dispatch at itself, with retries.
#
# Why this is a script and not a bare `gh workflow run` in each workflow: the
# deterministic re-trigger steps are how the orchestrator is *guaranteed* to
# wake after a merge or a push, so they run with `if: always()` and their
# failure fails the job — which trips escalate.sh and manufactures a
# `needs:human` gate. A single unretried API call on that path means one
# transient 5xx from api.github.com invents a human gate about nothing:
# run 30194633666 died on `HTTP 502: Server Error` and opened #88, for work an
# orchestrator run had already picked up 13 minutes earlier. 5xx from GitHub is
# expected background noise, not a signal (#91).
#
# Failure semantics, chosen deliberately (issue #91 offered retry-then-fail vs.
# retry-then-warn): retry, then FAIL. A dispatch that is still undeliverable
# after several spaced attempts is no longer noise — it means the orchestrator
# will not wake on this event, and the only remaining backstop is the 6-hourly
# cron tick. That degradation is worth a human's attention, so the escalation
# stays; it just no longer fires on the first hiccup. Callers that genuinely
# prefer a warning can add `|| true` at the call site, deliberately and visibly.
#
# Retries cover 5xx/timeout-shaped transients AND, incidentally, a slow
# just-pushed ref: nothing here inspects the error, because `gh` does not give a
# machine-readable status and a dispatch is idempotent enough that re-attempting
# a genuinely-rejected call costs only seconds. A permanent error (bad token,
# unknown workflow) simply burns all attempts and then fails, as it should.
#
# Usage:
#   dispatch.sh <workflow-file> [ref]
#     workflow-file  e.g. genesis-orchestrator.yml
#     ref            git ref to run on (default: main)
#
# Required env:
#   GH_TOKEN  token with actions:write
#   GH_REPO   owner/repo
# Optional env:
#   DISPATCH_ATTEMPTS  attempts before failing (default 3)
#   DISPATCH_DELAY     seconds between attempts (default 5)
#   DISPATCH_DRY_RUN   set to 1 to print the command instead of running it

set -euo pipefail

workflow="${1:-}"
ref="${2:-main}"
attempts="${DISPATCH_ATTEMPTS:-3}"
delay="${DISPATCH_DELAY:-5}"

if [[ -z "$workflow" ]]; then
  echo "usage: dispatch.sh <workflow-file> [ref]" >&2
  exit 2
fi

if [[ "${DISPATCH_DRY_RUN:-}" == "1" ]]; then
  echo "gh workflow run $workflow --ref $ref (dry run, ${attempts} attempts, ${delay}s apart)"
  exit 0
fi

# --ref is always pinned: without it `gh` does a default-branch lookup that
# needs repo metadata an actions-only app token does not have (a hard failure
# we already hit once in genesis-push-trigger.yml).
for attempt in $(seq 1 "$attempts"); do
  if gh workflow run "$workflow" --ref "$ref"; then
    echo "dispatched $workflow on $ref (attempt $attempt/$attempts)"
    exit 0
  fi
  if [[ "$attempt" -lt "$attempts" ]]; then
    echo "::warning::dispatch of $workflow failed (attempt $attempt/$attempts) — retrying in ${delay}s"
    sleep "$delay"
  fi
done

echo "::error::could not dispatch $workflow on $ref after $attempts attempts — the orchestrator will not wake on this event; the 6-hourly scheduled tick is the remaining backstop" >&2
exit 1
