#!/usr/bin/env bash
# Genesis intent checkpoint — records WHAT AN AGENT RUN IS TRYING TO DO, early,
# so a max-turns death still leaves behind its reasoning.
#
# Why this exists. `escalate.sh` already answers "did anything land?" by listing
# every issue and PR the dying run touched (#97). What it cannot answer is *why*
# — the escalation reports the artifact, not the intent behind it. That gap was
# recorded in .genesis/design/agent-turn-budgets.md under "What is deliberately
# not solved", with the condition attached: if max-turns deaths continue after
# the artifact-discovery fix, checkpointing intent early is the next thing to
# try — not another budget raise (six of those, five deaths, see #97), and not
# splitting coordinator from worker (rejected in the same design doc). #101 was
# that continued death, so this is that next thing.
#
# The load-bearing property is WHEN, not WHAT. The rule the design doc landed on
# is "anything a human must receive cannot live inside the agent's turn budget",
# and the shape that satisfies it is `nudge-gates.sh` running BEFORE the agent
# step so its signal escapes a later death (#84). An intent checkpoint cannot run
# before the agent — only the agent knows its intent — so it gets the next best
# thing: it is the agent's FIRST write, before the unbounded implementation work
# that actually consumes the budget. Cost is one turn; what it buys is that the
# most expensive failure mode in the system (a run that dies having produced no
# artifact at all — #85) stops being fully silent.
#
# A file, not an issue comment, on purpose:
#   - Zero noise. Intent is only interesting when the run FAILS; posting it every
#     run would put a paragraph nobody reads on an issue every 6 hours.
#   - No token, no network, no API failure mode. A checkpoint that can 502 would
#     manufacture exactly the false escalation #91 removed.
#   - $RUNNER_TEMP persists across steps within a job, which is all that is
#     needed: the agent step writes, the `if: failure()` escalate step reads.
# Accepted consequence: a run cancelled mid-flight loses its checkpoint. That is
# the same benign-cancellation case escalate.sh already declines to report on.
#
# Usage:
#   checkpoint.sh "one paragraph: what I intend to do this run, and why"
#   echo "..." | checkpoint.sh          # intent on stdin
#   checkpoint.sh --path                # print the resolved file path, write nothing
#   checkpoint.sh --help                # print usage, write nothing
#
# Unknown flags are REFUSED rather than recorded (#136). Taking intent from "$*"
# with only `--path` special-cased meant `checkpoint.sh --help` wrote "--help"
# into the file and reported success — 12 such entries accumulated locally. That
# is the empty-intent bug in a costlier form: it makes the file EXIST, so
# escalate.sh renders a flag string as the run's reasoning instead of reporting
# "No intent checkpoint was recorded", and that absence report is what tells a
# human they are looking at the #85 signature. So the best-effort/never-fail rule
# below stays scoped to filesystem failures, which the caller cannot fix; a
# mistyped flag is a caller error and fails loudly, exactly as an empty intent
# already does.
#
# `--path` is the single source of truth for WHERE the checkpoint lives:
# escalate.sh asks this script rather than recomputing the default, so the two
# cannot drift apart. Two scripts independently deriving one shared default is a
# bug that reports "no intent recorded" while the file sits on disk.
#
# Optional env:
#   GENESIS_CHECKPOINT_FILE  override the checkpoint path (tests use this)
#   RUNNER_TEMP              GitHub Actions per-job temp dir; the normal location
set -euo pipefail

# Resolution order: explicit override, then the Actions per-job temp dir, then a
# local tmp so an interactive `genesis serve` / laptop run behaves identically.
resolve_path() {
  if [ -n "${GENESIS_CHECKPOINT_FILE:-}" ]; then
    printf '%s\n' "$GENESIS_CHECKPOINT_FILE"
    return
  fi
  printf '%s/genesis-intent.md\n' "${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
}

usage() {
  cat <<'EOF'
Usage:
  checkpoint.sh "one paragraph: what I intend to do this run, and why"
  echo "..." | checkpoint.sh     intent on stdin
  checkpoint.sh --path           print the resolved checkpoint path, write nothing
  checkpoint.sh --help           print this message, write nothing

Env:
  GENESIS_CHECKPOINT_FILE        override the checkpoint path (tests use this)
  GENESIS_CHECKPOINT_MAX_LINES   bound on the retained file (default 200)
  RUNNER_TEMP                    Actions per-job temp dir; the normal location
EOF
}

# Flags are matched before intent so a mistyped one can never become the record.
case "${1:-}" in
  --path)
    resolve_path
    exit 0
    ;;
  --help | -h)
    usage
    exit 0
    ;;
  -*)
    {
      echo "checkpoint.sh: unknown option '$1' — refusing to record it as intent."
      echo
      echo "A flag written into the checkpoint is worse than no checkpoint: it makes the"
      echo "file exist, so escalate.sh reports the flag as what the run meant to do instead"
      echo "of reporting that no intent was recorded — and that absence is the signal a"
      echo "human reads to identify a run that died before its first deliverable."
      echo
      echo "If the intent genuinely begins with a dash, pipe it on stdin."
      echo
      usage
    } >&2
    exit 2
    ;;
esac

# Intent from argv, else stdin.
if [ "$#" -gt 0 ]; then
  intent="$*"
else
  intent="$(cat)"
fi

if [ -z "${intent//[[:space:]]/}" ]; then
  echo "checkpoint.sh: refusing to record an empty intent" >&2
  exit 2
fi

FILE="$(resolve_path)"

# Best-effort from here on. A checkpoint is diagnostics, never the deliverable:
# if the filesystem says no, warn on stderr and exit 0 rather than handing the
# agent an error it would spend turns investigating. The failure is not silent —
# escalate.sh reports "no intent checkpoint was recorded", which is itself the
# signal that either the run died early or this write did not work.
if ! mkdir -p "$(dirname "$FILE")" 2>/dev/null; then
  echo "checkpoint.sh: cannot create $(dirname "$FILE") — intent not recorded" >&2
  exit 0
fi

stamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Append rather than overwrite: an agent whose plan changes mid-run should leave
# both entries, because "intended X, switched to Y, then died" is strictly more
# diagnostic than either alone. Bounded below so a chatty run cannot grow an
# escalation body without limit.
# Bold rather than a heading: escalate.sh embeds this verbatim under its own
# `###` section, and a nested `###` per entry breaks that document's outline.
if ! printf '**%s**\n\n%s\n\n' "$stamp" "$intent" >>"$FILE" 2>/dev/null; then
  echo "checkpoint.sh: cannot write $FILE — intent not recorded" >&2
  exit 0
fi

# Keep the file bounded (the escalation embeds it verbatim). Trim oldest first;
# a truncated tail is the useful end, since the last intent is the one the run
# died holding.
MAX_LINES="${GENESIS_CHECKPOINT_MAX_LINES:-200}"
lines=$(wc -l <"$FILE" 2>/dev/null || echo 0)
if [ "$lines" -gt "$MAX_LINES" ] 2>/dev/null; then
  if tail -n "$MAX_LINES" "$FILE" >"$FILE.trim" 2>/dev/null; then
    mv "$FILE.trim" "$FILE"
  fi
fi

echo "checkpoint.sh: intent recorded at $stamp ($FILE)"
