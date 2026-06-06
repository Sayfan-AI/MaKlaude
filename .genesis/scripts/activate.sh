#!/usr/bin/env bash
# Genesis activation — wake the dev system after the human supplies credentials.
#
# Genesis ships this repo's GitHub Actions workflows DISABLED, so they don't fail
# on every trigger before the Genesis App is installed and the secrets are set.
# Once you've done both (see the README "Setup" section), run this once to enable
# them. The next trigger — an issue/PR/comment event, a push, or the cron — then
# wakes the orchestrator and onboarding begins on issue #1.
#
# Usage: .genesis/scripts/activate.sh [--force]
#   --force   enable the workflows even if the required secrets look unset
set -euo pipefail

FORCE=0
[[ "${1:-}" == "--force" ]] && FORCE=1

REQUIRED=(ANTHROPIC_API_KEY GENESIS_APP_ID GENESIS_APP_PRIVATE_KEY)

# Best-effort check that the secrets exist (needs admin on the repo). The first
# column of `gh secret list` is the secret name.
existing="$(gh secret list 2>/dev/null | awk '{print $1}')"
missing=()
for s in "${REQUIRED[@]}"; do
    grep -qx "$s" <<<"$existing" || missing+=("$s")
done
if [ "${#missing[@]}" -gt 0 ] && [ "$FORCE" -ne 1 ]; then
    echo "Required secrets are not set: ${missing[*]}" >&2
    echo "Set them (see the README 'Setup' section), then re-run." >&2
    echo "To enable the workflows anyway, re-run with --force." >&2
    exit 1
fi

# Re-enable every workflow genesis disabled at publish. They're all
# disabled_manually on a fresh repo, so enabling that set wakes the dev system.
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

echo "Dev system activated. The next trigger will wake the orchestrator."
