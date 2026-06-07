#!/usr/bin/env bash
# Activate this genesis dev system — the single command that wakes it up.
#
# Genesis ships a dev repo's workflows DISABLED and with no secrets. Run this once
# from a clone of the dev repo, after you've populated ~/.config/genesis/.env, and
# it does every remaining step in one go:
#   1. reads ANTHROPIC_API_KEY / GENESIS_GITHUB_APP_ID / GENESIS_GITHUB_APP_SECRET
#      from ~/.config/genesis/.env  (shared across all your genesis projects),
#   2. verifies the genesis GitHub App is actually installed on this repo,
#   3. sets the values as THIS repo's GitHub Actions secrets
#      (ANTHROPIC_API_KEY, GENESIS_APP_ID, GENESIS_APP_PRIVATE_KEY),
#   4. enables the workflows genesis disabled at publish.
#
# It refuses to run if any value is missing/placeholder or if the App isn't
# installed. You can also export the three vars yourself instead of using the .env.
#
# Usage: .genesis/scripts/activate.sh
set -euo pipefail

ENV_FILE="${GENESIS_CONFIG_DIR:-$HOME/.config/genesis}/.env"

# --- 1. validate the three values are present and not placeholders --------------
# Validate BEFORE sourcing into this shell, so placeholder values never leak into
# the environment (and on to gh / child processes): the candidate values are
# loaded in a throwaway subshell and checked there; the file is only sourced for
# real once all three are clean.
is_placeholder() {
    case "$1" in
        "" | *PLACEHOLDER* | *REPLACE_WITH* | *"paste the full PEM"*) return 0 ;;
        *) return 1 ;;
    esac
}
validate_values() {
    local v missing=()
    for v in ANTHROPIC_API_KEY GENESIS_GITHUB_APP_ID GENESIS_GITHUB_APP_SECRET; do
        is_placeholder "${!v:-}" && missing+=("$v")
    done
    if [ "${#missing[@]}" -gt 0 ]; then
        echo "ERROR: missing or placeholder value(s): ${missing[*]}" >&2
        return 1
    fi
}

if [ -f "$ENV_FILE" ]; then
    # Check in a subshell first - never source placeholders into the real env.
    if ! ( set -a; . "$ENV_FILE"; set +a; validate_values ); then
        echo "Populate $ENV_FILE (shared across all your projects), then re-run." >&2
        exit 1
    fi
    set -a
    # shellcheck disable=SC1090
    . "$ENV_FILE"
    set +a
else
    # No env file - fall back to vars the caller exported.
    if ! validate_values; then
        echo "No $ENV_FILE found and the vars aren't exported - create/populate it, then re-run." >&2
        exit 1
    fi
fi

REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner)" \
    || { echo "ERROR: run this from inside a clone of the dev repo." >&2; exit 1; }

# --- 2. verify the genesis GitHub App is installed on this repo -----------------
# The /repos/{repo}/installation endpoint needs a JWT signed by the App key — a
# plain user token gets 401 — so we mint a short-lived App JWT from the App ID + PEM.
b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }
app_jwt() {
    command -v openssl >/dev/null 2>&1 || return 2
    local now iat exp header payload signing sig
    now="$(date +%s)"; iat=$((now - 60)); exp=$((now + 540))
    header='{"alg":"RS256","typ":"JWT"}'
    payload="{\"iat\":$iat,\"exp\":$exp,\"iss\":\"$GENESIS_GITHUB_APP_ID\"}"
    signing="$(printf '%s' "$header" | b64url).$(printf '%s' "$payload" | b64url)"
    sig="$(printf '%s' "$signing" \
        | openssl dgst -sha256 -sign <(printf '%s' "$GENESIS_GITHUB_APP_SECRET") \
        | b64url)" || return 2
    printf '%s.%s' "$signing" "$sig"
}
verify_app_installed() {
    if ! command -v openssl >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
        echo "WARNING: openssl/curl not found; skipping App-install check." >&2
        return 0
    fi
    local jwt code
    jwt="$(app_jwt)" || { echo "WARNING: couldn't mint App JWT; skipping App-install check." >&2; return 0; }
    code="$(curl -s -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $jwt" \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/$REPO/installation")"
    case "$code" in
        200) echo "Genesis App is installed on $REPO." ;;
        404) echo "ERROR: the genesis GitHub App is not installed on $REPO." >&2
             echo "Install it on the repo's org/account, then re-run." >&2
             exit 1 ;;
        401) echo "ERROR: App JWT rejected (HTTP 401) — check GENESIS_GITHUB_APP_ID and the PEM." >&2
             exit 1 ;;
        *)   echo "WARNING: couldn't confirm App install (HTTP $code); continuing." >&2 ;;
    esac
}
verify_app_installed

# --- 3. seed the repo's Actions secrets -----------------------------------------
echo "Seeding secrets onto $REPO ..."
# Pipe via stdin (printf is a builtin) so values never reach the process arg list.
printf '%s' "$ANTHROPIC_API_KEY"         | gh secret set ANTHROPIC_API_KEY
printf '%s' "$GENESIS_GITHUB_APP_ID"     | gh secret set GENESIS_APP_ID
printf '%s' "$GENESIS_GITHUB_APP_SECRET" | gh secret set GENESIS_APP_PRIVATE_KEY

# --- 4. enable the workflows genesis disabled at publish ------------------------
echo "Enabling workflows ..."
gh workflow list --all --json id,name,state \
    | python3 -c "
import json, sys
for wf in json.load(sys.stdin):
    if wf['state'] == 'disabled_manually':
        print(wf['id'], wf['name'])
" \
    | while read -r id name; do
        gh workflow enable "$id" && echo "enabled: $name"
    done

echo "Dev system activated on $REPO. The next trigger will wake the orchestrator."
