#!/usr/bin/env bash
# Genesis activity logger — called by CC hooks
# Reads hook context from stdin (JSON), pushes to Grafana Loki
# Falls back to stderr if Loki is not configured
set -euo pipefail

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

# Build log line
TIMESTAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LOG_LINE="ts=$TIMESTAMP hook=$HOOK_EVENT project=$PROJECT"

# Extract key fields from stdin JSON if available
if [ -n "$STDIN_DATA" ]; then
    SESSION_ID=$(echo "$STDIN_DATA" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('session_id',''))" 2>/dev/null || echo "")
    TOOL_NAME=$(echo "$STDIN_DATA" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('tool_name',''))" 2>/dev/null || echo "")
    AGENT_TYPE=$(echo "$STDIN_DATA" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('agent_type',''))" 2>/dev/null || echo "")

    [ -n "$SESSION_ID" ] && LOG_LINE="$LOG_LINE session=$SESSION_ID"
    [ -n "$TOOL_NAME" ] && LOG_LINE="$LOG_LINE tool=$TOOL_NAME"
    [ -n "$AGENT_TYPE" ] && LOG_LINE="$LOG_LINE agent=$AGENT_TYPE"
fi

# Try to push to Loki
LOKI_URL="${GENCTL_LOKI_URL:-}"
LOKI_USER="${GENCTL_LOKI_USER:-}"
LOKI_TOKEN="${GENCTL_LOKI_TOKEN:-}"

if [ -n "$LOKI_URL" ]; then
    TIMESTAMP_NS=$(date +%s)000000000
    PAYLOAD=$(cat <<EOJSON
{"streams":[{"stream":{"project":"$PROJECT","hook_event":"$HOOK_EVENT"},"values":[["$TIMESTAMP_NS","$LOG_LINE"]]}]}
EOJSON
)

    if [ -n "$LOKI_USER" ] && [ -n "$LOKI_TOKEN" ]; then
        curl -sf -X POST "${LOKI_URL}/loki/api/v1/push" \
            -H "Content-Type: application/json" \
            -u "${LOKI_USER}:${LOKI_TOKEN}" \
            -d "$PAYLOAD" >/dev/null 2>&1 || true
    else
        curl -sf -X POST "${LOKI_URL}/loki/api/v1/push" \
            -H "Content-Type: application/json" \
            -d "$PAYLOAD" >/dev/null 2>&1 || true
    fi
fi

# Always log to stderr as fallback (visible in CI logs)
echo "[genesis] $LOG_LINE" >&2
