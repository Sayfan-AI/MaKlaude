#!/usr/bin/env bash
# Genesis session nets — run the loop's deterministic pre-run checks from a
# Claude Code SessionStart hook, so they fire in every execution mode instead of
# only under GitHub Actions.
#
# Why this exists: every deterministic net this dev system has learned to build
# is wired into workflow YAML. `nudge-gates.sh` is a step in
# genesis-orchestrator.yml placed *before* the agent step, which is exactly
# right under Actions and is nothing at all under `genesis serve`. Serve
# disables every genesis-* workflow (all six read `disabled_manually`) and
# launches `claude -p` directly; it never runs anything under
# `.genesis/scripts/`. So in the mode this project actually runs in today, the
# stale-gate check does not execute, and "has anyone noticed this gate" is back
# to being a judgment an agent has to make — the precise failure #84 replaced
# with a script after milestone 4's plan gate sat 21 days across ~85 ticks with
# every run correctly doing nothing.
#
# The seam both modes share is the SessionStart hook in `.claude/settings.json`.
# Verified rather than assumed: a headless `claude -p` in this repo fires the
# project hook, and the serve-launched session that found this bug shows up in
# the same Loki stream as two ad-hoc probes. Placement follows the same rule the
# workflow step follows — the net runs before the agent gets its first turn, so
# the signal escapes a session that later dies or wanders.
#
# Three properties this has to hold, each of which is a way to make things worse
# than leaving the gap open:
#
#   1. It must never fail a session. A SessionStart hook that exits non-zero
#      degrades every session in the repo, including a human's. `set -e` is
#      deliberately absent and the last statement is `exit 0`.
#
#   2. It must only write while acting as the loop. `nudge-gates.sh` posts
#      exactly one nudge per gate, ever, and burns the `nudged:stale` label as
#      the marker. If a human's own interactive session ran it, the comment
#      would be authored by that human — GitHub does not notify you of your own
#      comment, so the nudge would reach nobody *and* consume the one shot the
#      bot had. Silently converting a missing net into a dead one is worse than
#      the missing net.
#
#   3. It must be bounded and quiet. Network work is wrapped in `timeout` where
#      one is available, so a hung `gh` cannot stall every session start, and
#      the all-clear case prints nothing — SessionStart stdout is injected into
#      the session's context, and a net that narrates its own no-ops every time
#      is one the reader learns to skip.
#
# Under Actions this runs a second time, inside the agent step, after the
# workflow's own pre-agent step already ran it. That is a no-op by construction:
# the `nudged:stale` label suppresses the duplicate, which is the same guard
# that makes re-runs safe. Not special-cased, because a mode check here would be
# a second classification of the thing this script exists to stop depending on.
#
# Optional env:
#   GENESIS_SESSION_NETS_TIMEOUT  seconds to allow the nudge (default 60)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NET_TIMEOUT="${GENESIS_SESSION_NETS_TIMEOUT:-60}"

# SessionStart hands the hook a JSON context on stdin. Nothing here needs it,
# but leaving it unread can hand the writer a SIGPIPE.
[ -t 0 ] || cat >/dev/null 2>&1 || true

# Bound anything that touches the network, when the platform has a bounding
# tool. macOS without coreutils has neither `timeout` nor `gtimeout`; there the
# call runs unbounded rather than not at all, because Claude Code caps hook
# runtime itself and a missing net is the failure this script exists to fix.
run_bounded() {
    if command -v timeout >/dev/null 2>&1; then
        timeout "$NET_TIMEOUT" "$@"
    elif command -v gtimeout >/dev/null 2>&1; then
        gtimeout "$NET_TIMEOUT" "$@"
    else
        "$@"
    fi
}

# True only when this session's GitHub credential is the Genesis App
# installation token — i.e. this is the loop, not a person.
#
# The probe is a *valid* request with no side effect, not a malformed one. An
# invalid request answers "did it error", which conflates every error class; a
# capability probe built that way already produced a confident wrong answer here
# once (three endpoints agreeing on 422 that meant nothing). `/installation/
# repositories` is readable only by an installation token and returns 403 for a
# user token, so success is positive evidence of the identity rather than the
# absence of one error. Measured both ways on this repo: installation token →
# the repo count, personal token → 403; and `gh api user` is the exact mirror,
# 403 for the App and a login for a person.
acting_as_app() {
    [ -n "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ] || return 1
    run_bounded gh api /installation/repositories --jq '.total_count' \
        >/dev/null 2>&1
}

if ! acting_as_app; then
    exit 0
fi

# Stale-gate nudge. Output is suppressed unless the script actually wrote
# something, so the common case adds nothing to the session's context.
if out=$(run_bounded bash "$SCRIPT_DIR/nudge-gates.sh" 2>&1); then
    case "$out" in
        *"nudge posted"*) printf '%s\n' "$out" ;;
    esac
else
    # Losing a net is worth one line, but on stderr: Claude Code captures hook
    # stderr into the transcript rather than the session context, which is where
    # a diagnostic belongs and where it cannot be mistaken for an instruction.
    printf 'session-nets: nudge-gates.sh failed or timed out; stale gates unchecked this session\n' >&2
fi

exit 0
