#!/usr/bin/env bash
#
# setup.sh — provision MaKlaude's Slack channel from scratch, idempotently.
#
# Prereq: a MaKlaude Slack app created from deploy/slack/manifest.yaml and
# installed to your workspace, with the Bot User OAuth Token (xoxb-…) exported as
# MAKLAUDE_SLACK_BOT_TOKEN. See docs/slack.md for the full walkthrough.
#
# What it does (all idempotent — safe to re-run):
#   1. Validates the bot token (auth.test) and prints the workspace + bot user.
#   2. Resolves the target channel by name; CREATES it if it doesn't exist.
#   3. Ensures the bot is a member (joins if needed).
#   4. Invites the operator (if --operator / MAKLAUDE_SLACK_OPERATOR is set).
#   5. Prints the exact `export MAKLAUDE_SLACK_CHANNEL=…` line to wire into env.
#   With --verify, posts a throwaway escalation→update→resolution thread to prove
#   end-to-end delivery + threading.
#
# It NEVER prints the token and writes nothing to the repo.
#
# Usage:
#   export MAKLAUDE_SLACK_BOT_TOKEN=xoxb-…
#   deploy/slack/setup.sh [--channel maklaude] [--operator U0123456789] [--private] [--verify]
set -euo pipefail

CHANNEL_NAME="${MAKLAUDE_SLACK_CHANNEL_NAME:-maklaude}"
OPERATOR="${MAKLAUDE_SLACK_OPERATOR:-}"
PRIVATE=false
VERIFY=false

while [ $# -gt 0 ]; do
  case "$1" in
    --channel)  CHANNEL_NAME="$2"; shift 2 ;;
    --operator) OPERATOR="$2"; shift 2 ;;
    --private)  PRIVATE=true; shift ;;
    --verify)   VERIFY=true; shift ;;
    -h|--help)  sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

: "${MAKLAUDE_SLACK_BOT_TOKEN:?set MAKLAUDE_SLACK_BOT_TOKEN (xoxb-… bot token) — see deploy/slack/manifest.yaml}"
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

api="https://slack.com/api"
auth=(-H "Authorization: Bearer ${MAKLAUDE_SLACK_BOT_TOKEN}")

# jq-free JSON field extraction via python3. Reads stdin, prints d[<dotted path>].
jget() { python3 -c "import sys,json
d=json.load(sys.stdin)
for k in '''$1'''.split('.'):
    d=d.get(k,{}) if isinstance(d,dict) else {}
print(d if d!={} else '')"; }

# die_unless_ok <json> <context> — fail loudly on a Slack error response.
die_unless_ok() {
  local ok; ok="$(printf '%s' "$1" | jget ok)"
  if [ "$ok" != "True" ]; then
    echo "Slack API error during $2: $(printf '%s' "$1" | jget error)" >&2
    exit 1
  fi
}

echo "==> Validating bot token (auth.test)…"
authresp="$(curl -s "${auth[@]}" "$api/auth.test")"
die_unless_ok "$authresp" "auth.test"
echo "    workspace: $(printf '%s' "$authresp" | jget team)  ($(printf '%s' "$authresp" | jget url))"
echo "    bot user : $(printf '%s' "$authresp" | jget user) ($(printf '%s' "$authresp" | jget user_id))"

echo "==> Resolving channel #${CHANNEL_NAME}…"
CHANNEL_ID=""
cursor=""
while : ; do
  list="$(curl -s "${auth[@]}" \
    "$api/conversations.list?types=public_channel,private_channel&limit=1000&cursor=${cursor}")"
  die_unless_ok "$list" "conversations.list"
  CHANNEL_ID="$(printf '%s' "$list" | NAME="$CHANNEL_NAME" python3 -c "import os,sys,json
n=os.environ['NAME']
print(next((c['id'] for c in json.load(sys.stdin).get('channels',[]) if c['name']==n),''))")"
  [ -n "$CHANNEL_ID" ] && break
  cursor="$(printf '%s' "$list" | python3 -c "import sys,json;print(json.load(sys.stdin).get('response_metadata',{}).get('next_cursor',''))")"
  [ -z "$cursor" ] && break
done

if [ -n "$CHANNEL_ID" ]; then
  echo "    found existing channel: $CHANNEL_ID"
else
  echo "    not found — creating (private=$PRIVATE)…"
  create="$(curl -s "${auth[@]}" -H 'Content-type: application/json' \
    -d "{\"name\":\"${CHANNEL_NAME}\",\"is_private\":${PRIVATE}}" "$api/conversations.create")"
  die_unless_ok "$create" "conversations.create"
  CHANNEL_ID="$(printf '%s' "$create" | jget channel.id)"
  echo "    created: $CHANNEL_ID"
fi

echo "==> Ensuring bot is a member…"
join="$(curl -s "${auth[@]}" -H 'Content-type: application/json' \
  -d "{\"channel\":\"${CHANNEL_ID}\"}" "$api/conversations.join")"
# already_in_channel is a benign success for our purpose.
if [ "$(printf '%s' "$join" | jget ok)" != "True" ]; then
  err="$(printf '%s' "$join" | jget error)"
  [ "$err" = "method_not_supported_for_channel_type" ] || { echo "    join warning: $err" >&2; }
fi

if [ -n "$OPERATOR" ]; then
  echo "==> Inviting operator ${OPERATOR}…"
  inv="$(curl -s "${auth[@]}" -H 'Content-type: application/json' \
    -d "{\"channel\":\"${CHANNEL_ID}\",\"users\":\"${OPERATOR}\"}" "$api/conversations.invite")"
  if [ "$(printf '%s' "$inv" | jget ok)" != "True" ]; then
    err="$(printf '%s' "$inv" | jget error)"
    # already_in_channel / cant_invite_self are benign.
    case "$err" in already_in_channel|cant_invite_self) ;; *) echo "    invite warning: $err" >&2 ;; esac
  fi
fi

if [ "$VERIFY" = true ]; then
  echo "==> Verifying delivery (posting a throwaway escalation thread)…"
  mention=""; [ -n "$OPERATOR" ] && mention="<@${OPERATOR}> "
  root="$(curl -s "${auth[@]}" -H 'Content-type: application/json' \
    -d "{\"channel\":\"${CHANNEL_ID}\",\"text\":\":rotating_light: *MaKlaude setup verify* — ${mention}this is a connectivity test, safe to delete.\"}" \
    "$api/chat.postMessage")"
  die_unless_ok "$root" "chat.postMessage(root)"
  ts="$(printf '%s' "$root" | jget ts)"
  for note in ":arrows_counterclockwise: *Update* — threaded reply OK" ":white_check_mark: *Resolved* — verify complete"; do
    reply="$(curl -s "${auth[@]}" -H 'Content-type: application/json' \
      -d "{\"channel\":\"${CHANNEL_ID}\",\"thread_ts\":\"${ts}\",\"text\":\"${note}\"}" "$api/chat.postMessage")"
    die_unless_ok "$reply" "chat.postMessage(reply)"
  done
  echo "    posted thread ts=${ts}"
fi

echo
echo "✅ Channel ready. Wire it into MaKlaude's environment:"
echo
echo "    export MAKLAUDE_SLACK_CHANNEL=${CHANNEL_ID}"
echo
echo "(plus MAKLAUDE_SLACK_BOT_TOKEN, MAKLAUDE_SLACK_APP_TOKEN, and optionally"
echo " MAKLAUDE_SLACK_OPERATOR — see docs/slack.md.)"
