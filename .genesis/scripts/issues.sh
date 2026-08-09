#!/usr/bin/env bash
# Genesis issue manager — abstraction over gh CLI
# Supports: create, list, gates, stale-gates, red-prs, ready-prs,
#           unanswered-comments, close, assign, comment, label, view
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

# How far back a trailing human comment is still worth reporting as unanswered.
# A week-old comment on a thread nothing replied to has either been handled out
# of band or stopped mattering, and a section that keeps printing it forever is
# the noise that gets the whole report skipped.
DEFAULT_COMMENT_WINDOW_DAYS="${GENESIS_COMMENT_WINDOW_DAYS:-7}"

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

# Open PRs with at least one failing required check, rendered with their age.
#
# Why this exists: `genesis-ci-failure.yml` is the wake-on-CI-failure net, and it
# is *event-payload dependent* — it can only triage the run named in
# `github.event.workflow_run`. Only one run per concurrency group may be pending,
# so a burst evicts all but the newest, and the newest carries a DIFFERENT payload
# (usually a passing check) and skips. That is not a recoverable delay, it is a
# permanent drop: the failure's triage run never existed. It happened on the only
# genuine CI failure in the repo's history (2026-07-05, run 28736219761) and the
# net has 0 successes in 157 runs.
#
# The generalizable half: work derived from an *event payload* is lost under
# supersession, while work derived from *repo state* self-heals — the scheduled
# tick simply re-derives it. So "which PRs are red" is computed here from live
# state and printed by `summary` on every tick. A dropped triage event now costs
# at most one cycle instead of being invisible forever.
#
# Reads `gh pr list --json` output on stdin. Prints nothing when no PR is red, so
# callers can treat empty output as all-clear.
format_red_prs() {
    python3 -c "
import sys, json
from datetime import datetime, timezone

prs = json.load(sys.stdin)
now = datetime.now(timezone.utc)

rows = []
for p in prs:
    # statusCheckRollup mixes CheckRun (conclusion/name) and StatusContext
    # (state/context) shapes; a red PR is one where either says so. PENDING is
    # not failure — a PR mid-CI is not stuck and must not be reported as such.
    failing = []
    for c in p.get('statusCheckRollup') or []:
        verdict = c.get('conclusion') or c.get('state') or ''
        if verdict.upper() in ('FAILURE', 'ERROR', 'TIMED_OUT', 'CANCELLED'):
            failing.append(c.get('name') or c.get('context') or '?')
    if not failing:
        continue
    updated = datetime.fromisoformat(p['updatedAt'].replace('Z', '+00:00'))
    rows.append(((now - updated).days, p, failing))

# Stalest first: a PR that has been red for days is the one being forgotten.
rows.sort(key=lambda r: -r[0])

for age, p, failing in rows:
    draft = ' [draft]' if p.get('isDraft') else ''
    print('#%d  red %dd — %s%s (%s)\n      failing: %s' % (
        p['number'], age, p['title'], draft, p['headRefName'], ', '.join(sorted(set(failing)))))
"
}

# The other half of the same invisible-nothing-happened class: a PR that is
# *finished* and merely unmerged. `genesis-merge.yml` auto-merges bot PRs only —
# deliberately, because the repo is public and `pull_request` fires for fork PRs
# from anyone, so "merge any green PR" would let an arbitrary contributor land on
# main unreviewed. A human-authored PR is therefore merged by the orchestrator on
# a following run, "one run late by design".
#
# The lag is by design; permanent invisibility is not. Until now that merge
# depended on a run happening to notice the PR, and nothing put it in front of
# one: a human PR whose tracking issue is closed early, or that has no linked
# issue at all, is unreachable from every section of `summary`. So the same
# treatment red-prs got — derive it from repo state, print it unconditionally.
#
# Ready means merging is the ONLY remaining step. Everything else is somebody
# else's section: red checks are red-prs', BEHIND/BLOCKED/DIRTY need a rebase or
# a review rather than a merge, and `needs:human` means a person is deliberately
# holding it. Because this prints on every tick, a false positive is far more
# expensive than a false negative — it trains the orchestrator to skip the whole
# report (the lesson red-prs was built around) — so every predicate below is the
# conservative direction, including requiring checks to exist and to have
# concluded SUCCESS rather than merely not-failed.
#
# Reads `gh pr list --json` output on stdin. Prints nothing when nothing is
# ready, so callers can treat empty output as all-clear.
format_ready_prs() {
    python3 -c "
import sys, json
from datetime import datetime, timezone

prs = json.load(sys.stdin)
now = datetime.now(timezone.utc)

rows = []
for p in prs:
    if p.get('isDraft'):
        continue

    # A bot PR belongs to genesis-merge.yml. Listing it here races the
    # auto-merger and would have the orchestrator duplicate work that is
    # already automated, so it is excluded on purpose.
    author = p.get('author') or {}
    if author.get('is_bot') or str(author.get('login', '')).endswith('[bot]'):
        continue

    # A person holding the PR outranks any state we can compute.
    if any(l.get('name') == 'needs:human' for l in p.get('labels') or []):
        continue

    # Only GitHub knows whether the merge button is actually live; MERGEABLE
    # alone is not enough (a PR needing a rebase or a review is still
    # 'mergeable'). CLEAN is the state where merging is the only step left.
    if p.get('mergeable') != 'MERGEABLE' or p.get('mergeStateStatus') != 'CLEAN':
        continue

    # Same rollup shape as format_red_prs: CheckRun (name/conclusion) mixed with
    # StatusContext (context/state). Require every entry to have concluded
    # SUCCESS. An empty verdict is a check still in flight, so a PR mid-CI is
    # not ready; an empty rollup means CI has not reported at all. Both are
    # excluded. Requiring SUCCESS (not merely 'not failed') also makes this
    # provably disjoint from red-prs: any verdict red-prs counts as failing is
    # by construction not SUCCESS.
    checks = p.get('statusCheckRollup') or []
    if not checks:
        continue
    if not all((c.get('conclusion') or c.get('state') or '').upper() == 'SUCCESS' for c in checks):
        continue

    updated = datetime.fromisoformat(p['updatedAt'].replace('Z', '+00:00'))
    rows.append(((now - updated).days, p))

# Stalest first, matching gates and red-prs: the PR that has been sitting ready
# longest is the one being forgotten.
rows.sort(key=lambda r: -r[0])

for age, p in rows:
    print('#%d  ready %dd — %s (%s)' % (p['number'], age, p['title'], p['headRefName']))
"
}

# A human said something and nothing has answered it.
#
# Why this exists — the window, measured on #141 (2026-08-02, UTC):
#
#   06:18:37  bot: "the last done criterion is implemented — PR #154", CI running
#   06:29:18  HUMAN approves the approach and attaches two conditions
#   06:31:43  PR #154 merged by genesis-dev-bot[bot] on its own green checks
#   06:31:44  #141 closed
#
# The comment sat unread for 2m25s and then the work it constrained was merged
# and its issue closed. Two of the three negative cases it asked for were absent
# from what shipped. No existing net could have caught it: every one of them keys
# on CI state, issue/PR state, or run outcome, and none keys on *a person having
# said something*. `genesis-merge.yml` gates on exactly two facts (bot author,
# green checks) and never reads a comment; `ready-prs` excludes `needs:human` but
# a person who comments without labelling is invisible; `red-prs`, `stale-gates`,
# `escalate.sh` and `run-outcome.sh` are all about failure, and this was not a
# failure — PR #154 was correct, green, and correctly merged on the evidence its
# merger had.
#
# So this is the invisible-nothing-happened class again, with the sign flipped
# the same way #112 flipped it: a gate that waits forever (#84), a triage event
# dropped by supersession (#100), a run that dies mid-task (#97/#106/#110), a
# green PR nobody merges (#112). Same treatment as all four — derive it from repo
# state, print it unconditionally, empty means all-clear. Not a new label
# convention: "humans must label a comment that carries conditions" is an opt-in
# invariant, and the member who forgets is exactly the case it exists for.
#
# The rule, which needs no judgment: a thread's NEWEST comment is human-authored.
# The exclusions are where the care goes, because this prints every tick:
#
#   - bot-authored newest comment  — the loop has replied; nothing is waiting.
#   - older than the window        — see DEFAULT_COMMENT_WINDOW_DAYS.
#   - closed, comment AFTER close  — that is a closing note ("LGTM", "signed
#                                    off"), the single largest false-positive
#                                    class in this repo's history.
#   - closed by a human            — a person who comments and then closes their
#                                    own thread has answered themselves.
#
# which leaves exactly one reportable closed shape: the human spoke, and then the
# LOOP closed the thread over them. That is #141 precisely, and it stays
# actionable after the close (reopen, or answer and reopen).
#
# Known boundary, stated rather than silently missed: only conversation comments
# are read (`issues/N/comments`, which covers PRs). Inline review comments and
# review bodies live on a different endpoint; a review that requests changes is
# already visible as a non-CLEAN mergeStateStatus, which `ready-prs` excludes.
#
# Known limitation, from #156: "newer than the last bot comment" is a proxy for
# "unanswered". A bot reply that does not actually address the human's point
# clears the flag. Acknowledging is one comment, so clearing it deliberately is
# cheap; clearing it accidentally requires the loop to have said something.
#
# Prints nothing when nothing is waiting. If the API cannot be read it says so
# rather than printing nothing, because silence here would mean "all clear".
format_unanswered_comments() {
    python3 - "$1" <<'PY'
import json
import subprocess
import sys
from datetime import datetime, timezone

window_days = int(sys.argv[1])
now = datetime.now(timezone.utc)


def ts(value):
    return datetime.fromisoformat(value.replace('Z', '+00:00'))


def gh_json(path):
    proc = subprocess.run(['gh', 'api', path], capture_output=True, text=True)
    if proc.returncode != 0:
        return None
    try:
        return json.loads(proc.stdout)
    except ValueError:
        return None


# GitHub App comments carry type "Bot"; the [bot] login suffix is the belt to
# that suspenders, matching how format_ready_prs identifies a bot author.
#
# One historical caveat worth knowing rather than encoding: before
# 2026-08-02T03:39Z local `genesis serve` sessions commented as the human's own
# account, so comments older than that can be agent output wearing a User type.
# No cutoff constant is needed — every such thread was also *closed* by that same
# account, and the closed branch below requires a bot closer.
def is_bot(actor):
    if not isinstance(actor, dict):
        return False
    return actor.get('type') == 'Bot' or str(actor.get('login', '')).endswith('[bot]')


def ago(delta):
    secs = int(delta.total_seconds())
    if secs >= 86400:
        return '%dd' % (secs // 86400)
    if secs >= 3600:
        return '%dh' % (secs // 3600)
    return '%dm' % max(secs // 60, 0)


# One repo-wide call for the most recent comments. This is also the bound on the
# work: a thread whose newest comment falls outside this page has had no recent
# conversation, which is the same thing the window means.
comments = gh_json('repos/{owner}/{repo}/issues/comments?sort=created&direction=desc&per_page=100')
if comments is None:
    print('(the comments API could not be read, so this check did not run — '
          'do NOT read the empty section above it as all-clear)')
    sys.exit(0)

# Newest comment per thread. The feed is served newest-first, but ordering is
# re-established here rather than assumed: taking the wrong comment as "newest"
# would silently invert every verdict below.
newest = {}
for c in comments:
    if not isinstance(c, dict) or not c.get('created_at'):
        continue
    tail = str(c.get('issue_url', '')).rsplit('/', 1)[-1]
    if not tail.isdigit():
        continue
    num = int(tail)
    prior = newest.get(num)
    if prior is None or ts(c['created_at']) > ts(prior['created_at']):
        newest[num] = c

rows = []
for num, c in newest.items():
    if is_bot(c.get('user')):
        continue
    created = ts(c['created_at'])
    age = now - created
    if age.total_seconds() >= window_days * 86400:
        continue

    thread = gh_json('repos/{owner}/{repo}/issues/%d' % num)
    if thread is None:
        continue

    note = ''
    if thread.get('state') != 'open':
        closed_at = thread.get('closed_at')
        if not closed_at or ts(closed_at) <= created:
            continue
        if not is_bot(thread.get('closed_by')):
            continue
        note = '  [the loop closed this over them — reopen or answer]'

    rows.append((age, num, c, thread, note))

# Stalest first, matching gates, red-prs and ready-prs: the comment that has gone
# unanswered longest is the one being forgotten.
rows.sort(key=lambda r: -r[0].total_seconds())

for age, num, c, thread, note in rows:
    kind = 'PR' if thread.get('pull_request') else 'issue'
    print('#%d  unanswered %s — @%s on %s "%s"%s\n      %s' % (
        num, ago(age), (c.get('user') or {}).get('login', '?'),
        kind, thread.get('title', ''), note, c.get('html_url', '')))
PY
}

# Open automation:failure escalations with their REPEAT COUNTS.
#
# Why this exists: escalate.sh dedups per workflow, so repeat failures of one
# workflow become comments on one issue rather than new issues. That is correct
# — 14 issues for one cause would be worse — but it means a total loop outage
# presents, to anyone scanning the issue list, as a single issue titled "a run
# failed", and the recurrence count lives only in a comment thread nobody
# re-reads. The 3.5-day outage of 2026-07-30 (#150) looked exactly like a
# one-off from `summary` (#151). A single failure is noise; fourteen in a row is
# an outage, and only the streak makes the difference visible.
#
# Same family as gates/red-prs/ready-prs/unanswered-comments: derived from repo
# state, printed unconditionally by `summary`, empty means all-clear. One
# failure = the issue body + one per comment carrying the escalation's
# per-workflow dedup marker; human triage comments carry no marker and are not
# counted.
#
# Reads `gh issue list --json` output (automation:failure issues, with comments)
# on stdin. Prints nothing when no failure escalation is open.
format_failure_streaks() {
    python3 -c "
import json
import re
import sys
from datetime import datetime

issues = json.load(sys.stdin)

MARKER = re.compile(r'<!-- genesis-failure-wf: (.+?) -->')


def ts(value):
    return datetime.fromisoformat(value.replace('Z', '+00:00'))


rows = []
for i in issues:
    body = i.get('body') or ''
    m = MARKER.search(body)
    wf = m.group(1) if m else i.get('title', '?')
    marker = m.group(0) if m else '<!-- genesis-failure-wf:'
    count = 1
    last = ts(i['createdAt'])
    for c in i.get('comments') or []:
        if marker in (c.get('body') or ''):
            count += 1
            created = c.get('createdAt')
            if created and ts(created) > last:
                last = ts(created)
    rows.append((count, i, wf, last))

# Longest streak first: the workflow that has failed the most times running is
# the outage, not the one that failed most recently.
rows.sort(key=lambda r: -r[0])

for count, i, wf, last in rows:
    streak = '%d consecutive failures' % count if count > 1 else '1 failure'
    print('#%d  %s since %s (newest %s) — %s' % (
        i['number'], streak,
        ts(i['createdAt']).strftime('%Y-%m-%d %H:%MZ'),
        last.strftime('%Y-%m-%d %H:%MZ'), wf))
"
}

# Open failure escalations, with comments so the streak needs no extra calls.
fetch_failures() {
    gh issue list --state open --label "automation:failure" \
        --json number,title,body,createdAt,comments --limit 100
}

# Open PRs plus everything format_red_prs and format_ready_prs filter on. One
# query serves both so `summary` pays for a single PR round-trip.
fetch_prs() {
    gh pr list --state open --limit 100 \
        --json number,title,headRefName,isDraft,updatedAt,statusCheckRollup,mergeable,mergeStateStatus,labels,author
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

    red-prs)
        # Open PRs with failing checks (empty output = all clear). State-derived,
        # so it recovers a dropped ci-failure triage event — see format_red_prs.
        fetch_prs | format_red_prs
        ;;

    ready-prs)
        # Open PRs where merging is the only remaining step (empty = nothing
        # waiting). Auto-merge covers bot PRs only, so a human PR is merged by
        # the orchestrator — this is what puts it in front of one.
        fetch_prs | format_ready_prs
        ;;

    unanswered-comments)
        # Threads whose newest comment is a person's, that the loop has not
        # answered (empty = nothing waiting on a reply). State-derived, so it
        # does not depend on a run having been handed the comment as a trigger.
        WINDOW_DAYS="$DEFAULT_COMMENT_WINDOW_DAYS"
        while [[ $# -gt 0 ]]; do
            case "$1" in
                --window-days) WINDOW_DAYS="$2"; shift 2 ;;
                *) shift ;;
            esac
        done
        format_unanswered_comments "$WINDOW_DAYS"
        ;;

    failure-streaks)
        # Open automation:failure escalations with repeat counts, longest streak
        # first (empty = no failure escalation open). A dedup'd escalation makes
        # an outage look like one issue; the count is what tells them apart.
        fetch_failures | format_failure_streaks
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
        # Unconditional, same reasoning as the gates section: the wake-on-failure
        # workflow is event-payload dependent and its event is routinely dropped by
        # concurrency supersession, so a red PR must also be visible from state
        # alone. Empty here means no PR is red.
        echo "=== Red PRs (failing checks — triage or escalate) ==="
        fetch_prs | format_red_prs
        echo ""
        # Unconditional for the third time, and for the third variant of the same
        # failure: a gate waits forever, a triage event is dropped, a finished PR
        # is never merged. None of them error, so none of them are visible unless
        # state-derived and printed every tick. Empty here means nothing is
        # waiting on a merge.
        echo "=== Ready to Merge (green, clean, unmerged — merge or say why not) ==="
        fetch_prs | format_ready_prs
        echo ""
        # Unconditional for the fourth time, and for the one input none of the
        # other three can see: a person having spoken. Every other section is
        # derived from CI state, issue/PR state or run outcome, so a comment
        # carrying conditions on work in flight reaches no run unless it happens
        # to be that run's trigger. Empty here means nobody is waiting on a reply.
        echo "=== Unanswered Human Comments (a person spoke; answer before acting) ==="
        format_unanswered_comments "$DEFAULT_COMMENT_WINDOW_DAYS"
        echo ""
        # Unconditional for the fifth time: escalate.sh dedups repeat failures
        # into one issue per workflow, so an outage and a one-off look identical
        # from the sections above — one open issue either way. The streak count
        # is the only visible difference, and it must not live solely in a
        # comment thread nobody re-reads (#151). Empty here means no failure
        # escalation is open at all.
        echo "=== Automation Failure Streaks (repeat failures on one escalation = outage) ==="
        fetch_failures | format_failure_streaks
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
        # Plain `[ -n ... ] && cmd` here would make a successful add-only call
        # exit 1: the trailing `[ -n "$REMOVE" ]` fails when --remove is unset,
        # and the case arm's status is the script's. Callers treat non-zero as
        # "the label was not applied", which is exactly wrong.
        if [ -n "$ADD" ]; then gh issue edit "$ID" --add-label "$ADD"; fi
        if [ -n "$REMOVE" ]; then gh issue edit "$ID" --remove-label "$REMOVE"; fi
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
  red-prs     List open PRs with failing checks, stalest first (empty = all clear)
  ready-prs   List open PRs where merging is the only remaining step — green,
              MERGEABLE/CLEAN, not draft, no needs:human, non-bot author —
              stalest first (empty = nothing waiting on a merge)
  unanswered-comments
              List issues/PRs whose newest comment is a person's and that the
              loop has not answered, stalest first. Closed threads are reported
              only when the loop closed them AFTER the comment (empty = nothing
              waiting on a reply)
  failure-streaks
              List open automation:failure escalations with repeat counts,
              longest streak first (empty = no failure escalation open)
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

Comment filters (unanswered-comments):
  --window-days N      How far back a trailing human comment still counts as
                       unanswered (default 7, or GENESIS_COMMENT_WINDOW_DAYS)
EOF
        exit 1
        ;;
esac
