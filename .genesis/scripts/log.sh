#!/usr/bin/env bash
# Genesis activity logger — called by CC hooks
# Reads hook context from stdin (JSON), pushes to Grafana Loki
#
# Credentials come from the environment only — GENESIS_LOKI_URL (host, no path),
# GENESIS_LOKI_USER, GENESIS_LOKI_TOKEN. In GitHub Actions they arrive as repo
# secrets that activate.sh seeded and the genesis-* workflows pass through via
# the claude-code-action `settings` env block.
#
# Without a URL this only echoes to stderr. Be aware that stderr is NOT a real
# fallback in Actions: Claude Code captures hook stderr into its own transcript,
# so it does not appear in the run log. Unconfigured Loki means no activity trail.
#
# This script runs on EVERY tool call, so it does exactly one python3 spawn and
# bounds its curl. It must never exit non-zero — a PreToolUse hook that fails
# can block the agent's tool call.
set -uo pipefail

HOOK_EVENT="${1:-unknown}"

# Read stdin (CC hook JSON context)
STDIN_DATA=""
if [ ! -t 0 ]; then
    STDIN_DATA=$(cat)
fi

# Load config
CONFIG_FILE=""
DIR="$(pwd)"
while [ "$DIR" != "/" ]; do
    if [ -f "$DIR/.genesis/config.toml" ]; then
        CONFIG_FILE="$DIR/.genesis/config.toml"
        break
    fi
    DIR="$(dirname "$DIR")"
done

# Extract project name from config (simple grep, no toml parser needed)
PROJECT="unknown"
if [ -n "$CONFIG_FILE" ]; then
    PROJECT=$(grep -m1 '^name' "$CONFIG_FILE" | sed 's/.*= *"//' | sed 's/".*//' || echo "unknown")
fi

# One python3 call does all of it: parse the hook JSON, stamp a real nanosecond
# timestamp, and build the payload with json.dumps so a value containing a quote
# or newline can't produce malformed JSON.
#
# The nanosecond timestamp is the whole point. Loki drops entries that duplicate
# an existing (timestamp, line) within a stream, so a second-resolution stamp
# made two same-second `Bash` calls collapse into one — silent data loss, ack'd
# with HTTP 204. Verified: 6 identical pushes, 1 line stored.
#
# Prints the human-readable log line first, then the JSON payload.
BUILD_OUT=$(printf '%s' "$STDIN_DATA" | python3 -c '
import json, re, sys, time

hook, project = sys.argv[1], sys.argv[2]
raw = sys.stdin.read()
ctx = {}
if raw.strip():
    try:
        parsed = json.loads(raw)
        if isinstance(parsed, dict):
            ctx = parsed
    except ValueError:
        pass

ns = time.time_ns()
secs, rem = divmod(ns, 1_000_000_000)
ts = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(secs)) + ".%03dZ" % (rem // 1_000_000)

# Grafana derives its level from the line; a failed tool call should read as an
# error so it stands out in Explore and can be alerted on.
level = "error" if hook.endswith("failure") else "info"

fields = [("ts", ts), ("level", level), ("hook", hook), ("project", project)]
for key, name in (("session_id", "session"), ("tool_name", "tool"), ("agent_type", "agent")):
    value = ctx.get(key)
    if value:
        fields.append((name, str(value)))

# Secrets before anything else. A Bash command is the single most useful field
# here and also the most likely to carry a credential — `curl -u user:token`,
# an inline API key, a `gh` call with a PAT. Logs are not a safe place for those,
# and Loki has no delete.
SECRET_RE = re.compile(
    r"(glc_[A-Za-z0-9+/=_-]+|glsa_[A-Za-z0-9_-]+|sk-ant-[A-Za-z0-9_-]+"
    r"|gh[pousr]_[A-Za-z0-9]+|github_pat_[A-Za-z0-9_]+|AKIA[0-9A-Z]{16}"
    r"|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)"
)


def scrub(text):
    text = SECRET_RE.sub("<redacted>", text)
    # user:password in a URL or a -u/--user flag, which the pattern list cannot
    # anticipate because the secret has no recognisable prefix.
    text = re.sub(r"(-u|--user)\s+\S+:\S+", r"\1 <redacted>", text)
    text = re.sub(r"://[^/\s:@]+:[^/\s@]+@", "://<redacted>@", text)
    return text


def brief(value, limit=180):
    """One short, safe line describing what a tool call actually did."""
    if isinstance(value, dict):
        for key in ("command", "file_path", "pattern", "path", "url", "query", "description"):
            inner = value.get(key)
            if isinstance(inner, str) and inner.strip():
                value = inner
                break
        else:
            value = " ".join(f"{k}={v}" for k, v in list(value.items())[:3])
    text = " ".join(str(value).split())
    text = scrub(text)
    return text[:limit] + ("…" if len(text) > limit else "")


target = brief(ctx.get("tool_input"))
if target:
    fields.append(("target", target))

# Post hooks carry the result. Capture whether it failed and why — not the body,
# which for a Bash call is entire command output and would balloon both cost and
# the chance of logging something sensitive.
response = ctx.get("tool_response")
if isinstance(response, dict):
    failed = response.get("is_error") or response.get("error") or response.get("interrupted")
    if failed:
        fields.append(("status", "error"))
        detail = response.get("error") or response.get("stderr") or ""
        if detail:
            fields.append(("error", brief(str(detail), 160)))
    else:
        fields.append(("status", "ok"))
    for key, name in (("exitCode", "exit_code"), ("exit_code", "exit_code")):
        if isinstance(response.get(key), int):
            fields.append((name, response[key]))
            break

def fmt(value):
    # logfmt: quote only when the value would otherwise break parsing.
    return json.dumps(value) if any(c in value for c in " \"=\\") else value

line = " ".join("%s=%s" % (k, fmt(v)) for k, v in fields)

# Labels stay deliberately low-cardinality. session/tool/agent live in the line,
# where `| logfmt` promotes them to filterable fields at query time — a
# session_id label would mean one stream per session, forever.
payload = {
    "streams": [
        {
            "stream": {
                "project": project,
                "hook_event": hook,
                "service_name": project,
            },
            "values": [[str(ns), line]],
        }
    ]
}
print(line)
print(json.dumps(payload))
' "$HOOK_EVENT" "$PROJECT" 2>/dev/null)

if [ -n "$BUILD_OUT" ]; then
    LOG_LINE="${BUILD_OUT%%$'\n'*}"
    PAYLOAD="${BUILD_OUT#*$'\n'}"
else
    # python3 missing or blew up — still leave a trace rather than nothing.
    LOG_LINE="ts=$(date -u +%Y-%m-%dT%H:%M:%SZ) level=info hook=$HOOK_EVENT project=$PROJECT"
    PAYLOAD=""
fi

LOKI_URL="${GENESIS_LOKI_URL:-}"
LOKI_USER="${GENESIS_LOKI_USER:-}"
LOKI_TOKEN="${GENESIS_LOKI_TOKEN:-}"

# Bounded: a hung or slow Loki must not add latency to every tool call.
push() {
    if [ -n "$LOKI_USER" ] && [ -n "$LOKI_TOKEN" ]; then
        curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --max-time 5 \
            -X POST "${LOKI_URL}/loki/api/v1/push" \
            -H "Content-Type: application/json" \
            -u "${LOKI_USER}:${LOKI_TOKEN}" \
            -d "$PAYLOAD"
    else
        curl -s -o /dev/null -w '%{http_code}' --connect-timeout 2 --max-time 5 \
            -X POST "${LOKI_URL}/loki/api/v1/push" \
            -H "Content-Type: application/json" \
            -d "$PAYLOAD"
    fi
}

if [ -n "$LOKI_URL" ] && [ -n "$PAYLOAD" ]; then
    CODE="$(push 2>/dev/null)" || CODE="000"
    case "$CODE" in
        200 | 204) ;;
        # Never silent. The original swallowed every failure with `|| true`, which
        # is how months of 401s and dropped lines went unnoticed.
        *) echo "[genesis] loki push failed (HTTP ${CODE:-000}) hook=$HOOK_EVENT" >&2 ;;
    esac
fi

echo "[genesis] $LOG_LINE" >&2
exit 0
