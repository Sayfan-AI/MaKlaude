#!/usr/bin/env bash
# Genesis stale-gate nudge — deterministic escalation for human-in-the-loop
# gates nobody has answered.
#
# Why this is a script and not an agent instruction: the plan/completion gates
# are the dev system's central human-in-the-loop mechanism, and their failure
# mode is *silent indefinite stalling*. Every safety net we have catches runs
# that DIE; nothing catches runs that succeed at doing nothing. Milestone 4's
# plan gate (#76) sat unanswered for 21 days across ~85 scheduled ticks. Every
# one of those runs behaved correctly per the hard rule — "if a needs:human
# plan issue is open, do nothing and wait" — so every one did nothing, said
# nothing, and left no trace. The nudge finally went out by chance, on a run
# that happened to reason about the timeline. Per CLAUDE.md's
# deterministic-over-agentic principle, "is this gate older than N days" needs
# no LLM judgment, so it no longer gets one (#84).
#
# Exactly one nudge per gate, ever: the `nudged:stale` label is the marker, so
# re-runs are no-ops and the "don't re-notify the user" rule holds. There is
# deliberately no re-nudge timer — nagging a human every N days is worse than
# useless. The standing backstop instead is `issues.sh summary`, which now
# always prints every gate's age, so an ignored gate stays in front of every
# run rather than fading out.
#
# Called from the scheduled orchestrator workflow BEFORE the agent step, so the
# signal escapes even if that run later dies at max-turns.
#
# Required env:
#   GH_TOKEN  token with issues:write (the workflow's app token)
#   GH_REPO   owner/repo
# Optional env:
#   GENESIS_GATE_STALE_DAYS  threshold in days (default 3)
#   GENESIS_NUDGE_DRY_RUN    set to 1 to report without writing
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ISSUES="$SCRIPT_DIR/issues.sh"

STALE_DAYS="${GENESIS_GATE_STALE_DAYS:-3}"
DRY_RUN="${GENESIS_NUDGE_DRY_RUN:-0}"
NUDGE_LABEL="nudged:stale"

# tsv keeps the parse trivial and the threshold logic in one place (issues.sh).
stale=$("$ISSUES" stale-gates --stale-days "$STALE_DAYS" --format tsv)

if [ -z "$stale" ]; then
    echo "No gate has been waiting ${STALE_DAYS}+ days — nothing to nudge."
    exit 0
fi

# Ensure the marker label exists before trying to apply it; `gh issue edit
# --add-label` fails on an unknown label. --force makes this idempotent.
if [ "$DRY_RUN" != "1" ]; then
    gh label create "$NUDGE_LABEL" \
        --color "d93f0b" \
        --description "Stale needs:human gate — one nudge already posted" \
        --force >/dev/null 2>&1 || true
fi

nudged=0
skipped=0

while IFS=$'\t' read -r number age title; do
    [ -n "$number" ] || continue

    # Idempotency: the label is the record that this gate was already escalated.
    # Substring match on the raw JSON rather than a `| grep` pipeline — grep -q
    # exits early, SIGPIPEs gh, and `set -o pipefail` would then read the
    # already-nudged case as not-nudged and post a duplicate.
    labels_json=$(gh issue view "$number" --json labels)
    if [[ "$labels_json" == *"\"$NUDGE_LABEL\""* ]]; then
        echo "#$number (${age}d) — nudge already posted, skipping."
        skipped=$((skipped + 1))
        continue
    fi

    body=$(printf 'This gate has been waiting on a human for **%s days** and is blocking the loop.\n\nThe autonomous system is not broken — it is doing exactly what it should: while a `needs:human` gate is open it does not create task issues or start work. But that means a gate nobody answers parks the project indefinitely with no other signal, which is why this nudge is deterministic (see #84) rather than something an agent has to notice.\n\n**To unblock:** close this issue to approve, or comment with what you want changed.\n\nThis is the only automated nudge for this gate — the `%s` label makes sure of that. Gate age is also reported in every run state assessment, so this will not be silently forgotten either way.' \
        "$age" "$NUDGE_LABEL")

    if [ "$DRY_RUN" = "1" ]; then
        echo "[dry-run] would nudge #$number (${age}d): $title"
        nudged=$((nudged + 1))
        continue
    fi

    gh issue comment "$number" --body "$body"
    gh issue edit "$number" --add-label "$NUDGE_LABEL"
    echo "#$number (${age}d) — nudge posted: $title"
    nudged=$((nudged + 1))
done <<< "$stale"

echo "Stale gates: ${nudged} nudged, ${skipped} already nudged (threshold ${STALE_DAYS}d)."
