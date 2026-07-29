#!/usr/bin/env bash
# Genesis transcript retention — ship a finished agent run's FULL transcript
# (reasoning + tool calls + tool results) to the private Loki sink, so the middle
# of a run that died is recoverable afterwards.
#
# Why this exists. Six max-turns deaths produced six fixes, each recovering a
# different human-facing output from the dying run: `nudge-gates.sh` before the
# agent step (#84), `escalate.sh` artifact discovery after it (#97),
# `checkpoint.sh` intent at the front of it (#102), failure classification in the
# escalation (#104). What none of them recover is the MIDDLE — which approaches
# the agent tried, which dead ends it burned turns on, where the turns actually
# went (#106). Every budget and topology decision so far was made from a run log
# containing `init` + `result` and zero tool calls; #104's conclusion was that
# the evidence feeding those decisions was the wrong evidence.
#
# The transcript is not missing — it is DISCARDED. `claude-code-action` writes
# every SDK message to $RUNNER_TEMP/claude-execution-output.json, on the success
# path AND on the error path (base-action/src/run-claude-sdk.ts writes it in the
# catch as well, and an `error_max_turns` run still emits a `result` message, so
# the normal write happens too). The file is complete when the run dies; the
# runner is then torn down and it goes with it. So retention is a copy, not a
# capture — nothing needs to be turned on inside the agent.
#
# In particular this does NOT set `show_full_output`. That input streams tool
# results into the Actions run log, and this repo is PUBLIC: run logs and
# artifacts are world-readable and Actions masks only registered secrets, so
# enabling it is cheap and not cheap to do safely (#106 measured this). The
# transcript here goes to Loki — a private sink whose credentials are already
# repo secrets and already wired into every genesis workflow for activity
# logging (see log.sh, #78). Nothing world-readable gains content it does not
# have today: this script prints counts, HTTP codes and a query string, never
# transcript content. `test/devsystem/transcript_test.go` asserts that by
# running the script against a marked fixture and failing if the marker appears
# on stdout/stderr.
#
# Deterministic, and never the deliverable. No LLM in this path, and it always
# exits 0 — a retention failure must not turn a green run red, nor hand a live
# agent an error it would spend turns investigating. But it is never silent
# either: the outcome (retained / not retained, and why) is written to a status
# file that `escalate.sh` renders into the escalation issue, so the human reading
# "the run died" is told in the same breath whether the transcript survived and
# how to query it. That is the failure mode this whole line of work keeps
# hitting — something that quietly does nothing looks identical to something
# that worked.
#
# Usage:
#   retain-transcript.sh          # ship the transcript, write the status file
#   retain-transcript.sh --path   # print the status file path, write nothing
#
# `--path` is the single source of truth for WHERE the status lives, for the
# same reason checkpoint.sh has one: escalate.sh asks this script instead of
# recomputing the default, so the two cannot drift into "no transcript retained"
# being printed while the file sits on disk.
#
# Optional env:
#   GENESIS_EXECUTION_FILE      override the transcript path (default:
#                               $RUNNER_TEMP/claude-execution-output.json, the
#                               path base-action derives from RUNNER_TEMP)
#   GENESIS_TRANSCRIPT_STATUS   override the status file path (tests use this)
#   GENESIS_TRANSCRIPT_MAX_EVENTS  cap on events shipped (default 5000)
#   GENESIS_TRANSCRIPT_MAX_CHARS   cap on each event's serialized size (default 32768)
#   GENESIS_LOKI_URL/USER/TOKEN the sink, as in log.sh
set -uo pipefail

resolve_status_path() {
  if [ -n "${GENESIS_TRANSCRIPT_STATUS:-}" ]; then
    printf '%s\n' "$GENESIS_TRANSCRIPT_STATUS"
    return
  fi
  printf '%s/genesis-transcript-status.md\n' "${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
}

if [ "${1:-}" = "--path" ]; then
  resolve_status_path
  exit 0
fi

STATUS_FILE="$(resolve_status_path)"

# Report and exit. Writing the status is best-effort like everything else here;
# stdout carries the same line so it is visible in the run log even when the
# escalation never runs (a successful run has no escalation, and the baseline
# transcript of a healthy run is what a failing one gets compared against).
finish() {
  local msg="$1"
  mkdir -p "$(dirname "$STATUS_FILE")" 2>/dev/null || true
  printf '%s\n' "$msg" >"$STATUS_FILE" 2>/dev/null || true
  printf 'retain-transcript.sh: %s\n' "$(printf '%s' "$msg" | head -n 1)"
  exit 0
}

EXEC_FILE="${GENESIS_EXECUTION_FILE:-${RUNNER_TEMP:-${TMPDIR:-/tmp}}/claude-execution-output.json}"

if [ ! -s "$EXEC_FILE" ]; then
  # Not a neutral absence. The action writes this file on both the success and
  # the error path, so a missing one means the step died before the SDK produced
  # anything (bad auth, bad input, a cancelled job) — which is itself the most
  # useful thing this section can tell a human.
  finish "**Transcript: NOT retained** — no execution file at \`$EXEC_FILE\`. \`claude-code-action\` writes that file on both its success and error paths, so its absence means the agent step failed before the SDK produced any messages (auth, invalid input, or a cancellation) rather than during the run."
fi

LOKI_URL="${GENESIS_LOKI_URL:-}"
if [ -z "$LOKI_URL" ]; then
  finish "**Transcript: NOT retained** — \`GENESIS_LOKI_URL\` is unset, so there is no private sink to ship to. The transcript existed at \`$EXEC_FILE\` on the runner and was discarded with it. Set the \`GENESIS_LOKI_*\` repo secrets to retain it; deliberately NOT falling back to the run log or an artifact, both of which are world-readable on this public repo (#106)."
fi

PROJECT="unknown"
DIR="$(pwd)"
while [ "$DIR" != "/" ]; do
  if [ -f "$DIR/.genesis/config.toml" ]; then
    PROJECT=$(grep -m1 '^name' "$DIR/.genesis/config.toml" | sed 's/.*= *"//' | sed 's/".*//' || echo "unknown")
    break
  fi
  DIR="$(dirname "$DIR")"
done

BATCH_DIR="$(mktemp -d 2>/dev/null)" || finish "**Transcript: NOT retained** — could not create a temp dir to stage the Loki payloads."
trap 'rm -rf "$BATCH_DIR"' EXIT

# One python3 call turns the SDK message array into batched Loki push payloads.
# Notes on the three things that are easy to get wrong here:
#
#   1. Nanosecond, strictly-increasing timestamps. Loki silently drops an entry
#      that duplicates an existing (timestamp, line) within a stream and acks it
#      204 — log.sh learned this by losing same-second tool calls. base+index
#      also preserves turn order in Grafana, which is the whole point of reading
#      a transcript.
#   2. Low-cardinality stream labels. run_id/session/type live in the LINE, where
#      `| json` promotes them to filterable fields at query time; as labels they
#      would mint a new stream per run forever.
#   3. Caps are reported, not silent. A truncated event says so, and a run whose
#      event count exceeds the cap keeps the TAIL — the end of a dying run is the
#      part worth having — and reports how many were dropped.
SUMMARY=$(python3 - "$EXEC_FILE" "$BATCH_DIR" "$PROJECT" <<'PY' 2>/dev/null
import json, os, sys, time

exec_file, batch_dir, project = sys.argv[1], sys.argv[2], sys.argv[3]

max_events = int(os.environ.get("GENESIS_TRANSCRIPT_MAX_EVENTS") or 5000)
max_chars = int(os.environ.get("GENESIS_TRANSCRIPT_MAX_CHARS") or 32768)

try:
    with open(exec_file) as fh:
        events = json.load(fh)
except Exception as exc:
    print("ERROR\tcould not parse %s as JSON (%s)" % (exec_file, type(exc).__name__))
    sys.exit(0)

if not isinstance(events, list):
    print("ERROR\t%s is not the expected JSON array of SDK messages" % exec_file)
    sys.exit(0)

dropped = 0
if len(events) > max_events:
    dropped = len(events) - max_events
    events = events[-max_events:]

run_id = os.environ.get("GITHUB_RUN_ID", "")
attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "")
workflow = os.environ.get("GITHUB_WORKFLOW", "")

session = ""
for ev in events:
    if isinstance(ev, dict) and ev.get("session_id"):
        session = str(ev["session_id"])
        break


def summarize(ev):
    """A one-line human gloss, so Grafana is readable without expanding `event`."""
    if not isinstance(ev, dict):
        return "non-object event"
    kind = ev.get("type", "?")
    msg = ev.get("message")
    blocks = msg.get("content") if isinstance(msg, dict) else None
    if isinstance(blocks, str):
        return "%s: %s" % (kind, blocks)
    parts = []
    if isinstance(blocks, list):
        for b in blocks:
            if not isinstance(b, dict):
                continue
            bt = b.get("type")
            if bt == "text":
                parts.append("text: " + " ".join(str(b.get("text", "")).split()))
            elif bt == "thinking":
                parts.append("thinking: " + " ".join(str(b.get("thinking", "")).split()))
            elif bt == "tool_use":
                parts.append("tool_use: %s" % b.get("name", "?"))
            elif bt == "tool_result":
                parts.append("tool_result%s" % (" (error)" if b.get("is_error") else ""))
            else:
                parts.append(str(bt))
    if ev.get("subtype"):
        parts.insert(0, "subtype=%s" % ev["subtype"])
    return "%s: %s" % (kind, " | ".join(parts)) if parts else kind


def truncate(s):
    if len(s) <= max_chars:
        return s, False
    # Keep both ends: a tool result's head says what ran, its tail says how it
    # failed, and a blind head-cut throws the second one away.
    keep = max_chars // 2
    return s[:keep] + "\n...[truncated]...\n" + s[-keep:], True

base_ns = time.time_ns()
truncated_count = 0
values = []
for i, ev in enumerate(events):
    raw, was_truncated = truncate(json.dumps(ev, default=str))
    if was_truncated:
        truncated_count += 1
    entry = {
        "i": i,
        "type": ev.get("type") if isinstance(ev, dict) else "unknown",
        "subtype": (ev.get("subtype") if isinstance(ev, dict) else None) or "",
        "run_id": run_id,
        "attempt": attempt,
        "workflow": workflow,
        "session": session,
        "summary": truncate(summarize(ev))[0],
        "truncated": was_truncated,
        "event": raw,
    }
    values.append([str(base_ns + i), json.dumps(entry)])

# Batch so no single push carries the whole transcript. Bounded by BOTH count
# and bytes: 500 small events and 20 huge ones are equally valid transcripts.
MAX_PER_BATCH = 500
MAX_BYTES = 2 * 1024 * 1024
stream = {"project": project, "service_name": project, "kind": "transcript"}

batches, cur, cur_bytes = [], [], 0
for v in values:
    size = len(v[1])
    if cur and (len(cur) >= MAX_PER_BATCH or cur_bytes + size > MAX_BYTES):
        batches.append(cur)
        cur, cur_bytes = [], 0
    cur.append(v)
    cur_bytes += size
if cur:
    batches.append(cur)

for n, batch in enumerate(batches):
    with open(os.path.join(batch_dir, "batch-%04d.json" % n), "w") as fh:
        json.dump({"streams": [{"stream": stream, "values": batch}]}, fh)

print("OK\t%d\t%d\t%d\t%d\t%s" % (len(values), dropped, truncated_count, len(batches), session))
PY
)

if [ -z "$SUMMARY" ]; then
  finish "**Transcript: NOT retained** — could not build the Loki payload from \`$EXEC_FILE\` (python3 missing or failed). The transcript is on the runner and will be discarded with it."
fi

if [ "${SUMMARY%%$'\t'*}" != "OK" ]; then
  finish "**Transcript: NOT retained** — ${SUMMARY#*$'\t'}"
fi

IFS=$'\t' read -r _ EVENT_COUNT DROPPED TRUNCATED BATCH_COUNT SESSION <<<"$SUMMARY"

push_batch() {
  if [ -n "${GENESIS_LOKI_USER:-}" ] && [ -n "${GENESIS_LOKI_TOKEN:-}" ]; then
    curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 30 \
      -X POST "${LOKI_URL}/loki/api/v1/push" \
      -H "Content-Type: application/json" \
      -u "${GENESIS_LOKI_USER}:${GENESIS_LOKI_TOKEN}" \
      --data-binary "@$1"
  else
    curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 30 \
      -X POST "${LOKI_URL}/loki/api/v1/push" \
      -H "Content-Type: application/json" \
      --data-binary "@$1"
  fi
}

sent=0
failed=0
codes=""
for batch in "$BATCH_DIR"/batch-*.json; do
  [ -e "$batch" ] || continue
  code="$(push_batch "$batch" 2>/dev/null)" || code="000"
  case "$code" in
    2*) sent=$((sent + 1)) ;;
    *)
      failed=$((failed + 1))
      case "$codes" in *"$code"*) ;; *) codes="${codes}${codes:+,}${code:-000}" ;; esac
      ;;
  esac
done

QUERY="{project=\"$PROJECT\", kind=\"transcript\"}"
if [ -n "${GITHUB_RUN_ID:-}" ]; then
  QUERY="$QUERY | json | run_id=\`${GITHUB_RUN_ID}\`"
else
  QUERY="$QUERY | json"
fi

notes=""
[ "$DROPPED" != "0" ] && notes="${notes} ${DROPPED} oldest event(s) exceeded the cap and were dropped (the tail is kept — the end of a dying run is the part worth having)."
[ "$TRUNCATED" != "0" ] && notes="${notes} ${TRUNCATED} event(s) were individually too large and were truncated head+tail."
[ -n "$SESSION" ] && notes="${notes} Session \`${SESSION}\`."

if [ "$failed" -gt 0 ] && [ "$sent" -eq 0 ]; then
  finish "**Transcript: NOT retained** — all ${BATCH_COUNT} Loki push(es) failed (HTTP ${codes}). ${EVENT_COUNT} events were built but none landed; the transcript is on the runner and will be discarded with it."
fi

if [ "$failed" -gt 0 ]; then
  finish "$(printf '**Transcript: PARTIALLY retained** — %d of %d Loki batches landed, %d failed (HTTP %s). What did land is queryable:\n\n```logql\n%s\n```\n%s' "$sent" "$BATCH_COUNT" "$failed" "$codes" "$QUERY" "$notes")"
fi

finish "$(printf '**Transcript: retained** — %d events (reasoning, tool calls and tool results) shipped to the private Loki sink in %d batch(es). This is where the turns went; read it before inferring anything about the budget:\n\n```logql\n%s\n```\n%s' "$EVENT_COUNT" "$BATCH_COUNT" "$QUERY" "$notes")"
