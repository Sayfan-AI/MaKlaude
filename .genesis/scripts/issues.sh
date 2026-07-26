#!/usr/bin/env bash
# Genesis issue manager — abstraction over gh CLI
# Supports: create, list, gates, stale-gates, close, assign, comment, label, view
set -euo pipefail

CMD="${1:-help}"
shift || true

# JSON fields to fetch for list/view queries
FIELDS="number,title,state,url,labels,assignees,createdAt,updatedAt"

# Label marking a human-in-the-loop gate (plan approval, milestone sign-off,
# escalation) that is waiting on a person.
GATE_LABEL="needs:human"

# Label applied by nudge-gates.sh once a stale gate has been escalated. It is
# the idempotency marker — exactly one nudge per gate.
NUDGE_LABEL="nudged:stale"

# Age in days at which an unanswered gate counts as stale. The scheduled
# orchestrator ticks every 6h, so 3 days is ~12 ticks — comfortably past the
# "stuck for more than 2 cycles" guideline without being twitchy.
DEFAULT_STALE_DAYS="${GENESIS_GATE_STALE_DAYS:-3}"

format_issues() {
    python3 -c "
import sys, json
issues = json.load(sys.stdin)
for i in issues:
    labels = ','.join(l['name'] for l in i.get('labels', []))
    assignees = ','.join(a['login'] for a in i.get('assignees', []))
    parts = [f'#{i[\"number\"]}', f'[{i[\"state\"]}]', i['title']]
    if labels:
        parts.append(f'({labels})')
    if assignees:
        parts.append(f'-> {assignees}')
    print(' '.join(parts))
" 2>/dev/null || cat
}

# Render open gate issues with their AGE, which is the whole point: a gate
# nobody closes stalls the project silently — every orchestrator run correctly
# does nothing, so there is no failure to notice. Age turns that into a fact
# the run cannot miss. "Is this older than N days" needs no LLM judgment, so
# per CLAUDE.md's deterministic-over-agentic principle it does not get one.
#
# Args: STALE_DAYS  threshold in days
#       ONLY_STALE  "stale-only" to suppress gates under the threshold
#       FORMAT      "text" (default) or "tsv" (number<TAB>age<TAB>title)
# Reads `gh issue list --json` output on stdin. Prints nothing when there is
# nothing to report, so callers can treat empty output as "all clear".
format_gates() {
    python3 -c "
import sys, json
from datetime import datetime, timezone

stale_days = int('$1')
only_stale = '$2' == 'stale-only'
fmt = '$3' or 'text'

issues = json.load(sys.stdin)
now = datetime.now(timezone.utc)

rows = []
for i in issues:
    created = datetime.fromisoformat(i['createdAt'].replace('Z', '+00:00'))
    age = (now - created).days
    if only_stale and age < stale_days:
        continue
    rows.append((age, i))

# Oldest first — the most-stalled gate is the one a human should look at.
rows.sort(key=lambda r: -r[0])

for age, i in rows:
    if fmt == 'tsv':
        print('%d\t%d\t%s' % (i['number'], age, i['title']))
        continue
    labels = [l['name'] for l in i.get('labels', [])]
    flags = []
    if age >= stale_days:
        flags.append('STALE')
    if '$NUDGE_LABEL' in labels:
        flags.append('nudge posted')
    suffix = ' [' + ', '.join(flags) + ']' if flags else ''
    print('#%d  %2dd waiting — %s%s' % (i['number'], age, i['title'], suffix))
"
}

# All open gate issues as JSON, for format_gates to render.
fetch_gates() {
    gh issue list --state open --label "$GATE_LABEL" --json "$FIELDS" --limit 100
}

case "$CMD" in
    create)
        TITLE="" LABELS="" BODY="" MILESTONE="" ASSIGNEE=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --title) TITLE="$2"; shift 2 ;;
                --labels) LABELS="$2"; shift 2 ;;
                --body) BODY="$2"; shift 2 ;;
                --milestone) MILESTONE="$2"; shift 2 ;;
                --assignee) ASSIGNEE="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -z "$TITLE" ]; then
            echo "Usage: issues.sh create --title TITLE [--labels LABELS] [--body BODY] [--milestone N] [--assignee USER]" >&2
            exit 1
        fi
        ARGS=(issue create --title "$TITLE")
        [ -n "$LABELS" ] && ARGS+=(--label "$LABELS")
        [ -n "$BODY" ] && ARGS+=(--body "$BODY")
        # Add milestone label
        [ -n "$MILESTONE" ] && ARGS+=(--label "milestone:$MILESTONE")
        [ -n "$ASSIGNEE" ] && ARGS+=(--assignee "$ASSIGNEE")
        gh "${ARGS[@]}"
        ;;

    list)
        STATE="open" LABEL="" ASSIGNEE="" SINCE="" SEARCH=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --status) STATE="$2"; shift 2 ;;
                --milestone) LABEL="milestone:$2"; shift 2 ;;
                --label) LABEL="$2"; shift 2 ;;
                --assignee) ASSIGNEE="$2"; shift 2 ;;
                --since) SINCE="$2"; shift 2 ;;
                --search) SEARCH="$2"; shift 2 ;;
                --all) STATE="all"; shift ;;
                *) shift ;;
            esac
        done
        ARGS=(issue list --state "$STATE" --json "$FIELDS" --limit 100)
        [ -n "$LABEL" ] && ARGS+=(--label "$LABEL")
        [ -n "$ASSIGNEE" ] && ARGS+=(--assignee "$ASSIGNEE")
        [ -n "$SEARCH" ] && ARGS+=(--search "$SEARCH")

        if [ -n "$SINCE" ]; then
            # Filter by updated date using jq-style python filtering
            gh "${ARGS[@]}" | python3 -c "
import sys, json
from datetime import datetime, timedelta, timezone

since_str = '$SINCE'
# Parse relative time like '24 hours ago', '7 days ago'
parts = since_str.split()
if len(parts) == 3 and parts[2] == 'ago':
    n = int(parts[0])
    unit = parts[1].rstrip('s')
    if unit == 'hour':
        cutoff = datetime.now(timezone.utc) - timedelta(hours=n)
    elif unit == 'day':
        cutoff = datetime.now(timezone.utc) - timedelta(days=n)
    elif unit == 'week':
        cutoff = datetime.now(timezone.utc) - timedelta(weeks=n)
    else:
        cutoff = datetime.min.replace(tzinfo=timezone.utc)
else:
    cutoff = datetime.fromisoformat(since_str.replace('Z', '+00:00'))

issues = json.load(sys.stdin)
filtered = [i for i in issues if datetime.fromisoformat(i['updatedAt'].replace('Z', '+00:00')) >= cutoff]
json.dump(filtered, sys.stdout)
" | format_issues
        else
            gh "${ARGS[@]}" | format_issues
        fi
        ;;

    gates|stale-gates)
        # gates       — every open human-in-the-loop gate, with age, stale ones flagged
        # stale-gates — only the gates past the threshold (empty output = all clear)
        STALE_DAYS="$DEFAULT_STALE_DAYS" FORMAT="text"
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --stale-days) STALE_DAYS="$2"; shift 2 ;;
                --format) FORMAT="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        ONLY_STALE=""
        [ "$CMD" = "stale-gates" ] && ONLY_STALE="stale-only"
        fetch_gates | format_gates "$STALE_DAYS" "$ONLY_STALE" "$FORMAT"
        ;;

    blocked)
        # Shortcut: list all blocked issues
        gh issue list --state open --label "blocked" --json "$FIELDS" --limit 100 | format_issues
        ;;

    recent)
        # Shortcut: recently updated issues (last 24h by default)
        HOURS="${1:-24}"
        gh issue list --state all --json "$FIELDS" --limit 100 | python3 -c "
import sys, json
from datetime import datetime, timedelta, timezone
cutoff = datetime.now(timezone.utc) - timedelta(hours=$HOURS)
issues = json.load(sys.stdin)
filtered = [i for i in issues if datetime.fromisoformat(i['updatedAt'].replace('Z', '+00:00')) >= cutoff]
json.dump(filtered, sys.stdout)
" | format_issues
        ;;

    summary)
        # Overview of project state: open, blocked, recently closed
        echo "=== Open Issues ==="
        gh issue list --state open --json "$FIELDS" --limit 100 | format_issues
        echo ""
        # Gates come with their age, unconditionally. This is the standing
        # backstop against a silently parked project: an unanswered gate is
        # always in front of the run, not something it has to derive from dates.
        echo "=== Human Gates (awaiting a person, oldest first) ==="
        fetch_gates | format_gates "$DEFAULT_STALE_DAYS" "" "text"
        echo ""
        echo "=== Blocked ==="
        gh issue list --state open --label "blocked" --json "$FIELDS" --limit 100 | format_issues
        echo ""
        echo "=== Recently Closed (7 days) ==="
        gh issue list --state closed --json "$FIELDS" --limit 100 | python3 -c "
import sys, json
from datetime import datetime, timedelta, timezone
cutoff = datetime.now(timezone.utc) - timedelta(days=7)
issues = json.load(sys.stdin)
filtered = [i for i in issues if datetime.fromisoformat(i['updatedAt'].replace('Z', '+00:00')) >= cutoff]
json.dump(filtered, sys.stdout)
" | format_issues
        ;;

    close)
        ID="" REASON=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --id) ID="$2"; shift 2 ;;
                --reason) REASON="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -z "$ID" ]; then
            echo "Usage: issues.sh close --id ID [--reason REASON]" >&2
            exit 1
        fi
        ARGS=(issue close "$ID")
        [ -n "$REASON" ] && ARGS+=(--reason "$REASON")
        gh "${ARGS[@]}"
        ;;

    assign)
        ID="" TO=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --id) ID="$2"; shift 2 ;;
                --to) TO="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -z "$ID" ] || [ -z "$TO" ]; then
            echo "Usage: issues.sh assign --id ID --to ASSIGNEE" >&2
            exit 1
        fi
        gh issue edit "$ID" --add-assignee "$TO"
        ;;

    label)
        ID="" ADD="" REMOVE=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --id) ID="$2"; shift 2 ;;
                --add) ADD="$2"; shift 2 ;;
                --remove) REMOVE="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -z "$ID" ]; then
            echo "Usage: issues.sh label --id ID --add LABEL | --remove LABEL" >&2
            exit 1
        fi
        [ -n "$ADD" ] && gh issue edit "$ID" --add-label "$ADD"
        [ -n "$REMOVE" ] && gh issue edit "$ID" --remove-label "$REMOVE"
        ;;

    comment)
        ID="" BODY=""
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --id) ID="$2"; shift 2 ;;
                --body) BODY="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        if [ -z "$ID" ] || [ -z "$BODY" ]; then
            echo "Usage: issues.sh comment --id ID --body BODY" >&2
            exit 1
        fi
        gh issue comment "$ID" --body "$BODY"
        ;;

    view)
        ID="${1:-}"
        if [ -z "$ID" ]; then
            echo "Usage: issues.sh view ID" >&2
            exit 1
        fi
        gh issue view "$ID"
        ;;

    *)
        cat >&2 <<'EOF'
Usage: issues.sh COMMAND [OPTIONS]

Commands:
  create    Create a new issue
  list      List issues with filtering
  gates       List open needs:human gates with their age (stale ones flagged)
  stale-gates List only gates past the staleness threshold (empty = all clear)
  blocked   List all blocked issues
  recent    List recently updated issues (default: last 24h)
  summary   Overview of project state
  close     Close an issue
  assign    Assign an issue
  label     Add/remove labels
  comment   Comment on an issue
  view      View issue details

List filters:
  --status STATE       open|closed|all (default: open)
  --milestone N        Filter by milestone label
  --label LABEL        Filter by label
  --assignee USER      Filter by assignee
  --since "N hours ago"  Filter by update time
  --search QUERY       Full-text search
  --all                Show all states

Gate filters (gates / stale-gates):
  --stale-days N       Staleness threshold in days (default 3, or
                       GENESIS_GATE_STALE_DAYS)
  --format text|tsv    tsv emits number<TAB>age<TAB>title for scripting
EOF
        exit 1
        ;;
esac
