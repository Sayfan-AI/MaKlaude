# Design: why turn budgets stopped converging, and what replaces raising them

Resolves the design question in MaKlaude #97. Lives in `.genesis/` because the
problem is framework-level, not product-level — this is written to be lifted
into `Sayfan-AI/genesis` verbatim (see #81).

## The observation

Six raises, five deaths, one shape:

| # | runner | budget | outcome |
|---|---|---|---|
| #38 | evolver | 20 | died daily Jun 22–24 |
| #80/#81 | events orchestrator | 20 | died at turn 21 mid-task on #78/PR #79 |
| #85 | evolver | 30 | died at turn 31 with **no artifact at all** |
| #87 | scheduled orchestrator | 40 | died at turn 41, seconds after opening PR #86 |
| #96 | evolver | 45 | died at turn 46, seconds after PR #95 merged |

#89 fixed *instance-vs-class* (raise the floor, not the runner that failed).
#96 fixed *floor-below-an-observed-failure* (`observedFailedBudgets` in
`test/devsystem/workflows_test.go` now mechanically forces the floor above every
budget seen to die). Neither explains why the number keeps needing to go up.

The last two failures share a signature the first three do not: **the deliverable
landed and only the bookkeeping was truncated.** #87 died having pushed PR #86.
#96 died having merged PR #95 and posted its #81 comment. In both, the expensive
part — explore, write, test, commit, push, open the PR — consumed the budget,
and what got cut was the cheap part a human actually reads: the diagnosis
comment, the issue close, the label cleanup.

## Why no number is safe

Implementation cost is unbounded: it scales with whatever task the run happens to
pick up, which is chosen *during* the run. Wrap-up cost is small and roughly
constant. Putting both in one budget means the unbounded term can always crowd
out the bounded one, and the bounded one is the only part with external value.

That is the actual defect. It is a *sequencing and ownership* defect, not a
sizing defect.

## The four questions #97 asked

**Q1 — do subagent turns draw from the coordinator's `--max-turns` pool in
`claude-code-action`?**

Not established, and this design deliberately does not depend on the answer. The
repo's own evidence is inconclusive: the evolver both dispatches subagents and
died at 45, which is consistent with either semantics. Making the fix contingent
on an unverified property of a third-party action would be a fifth guess in a
sequence of four wrong ones.

**Q2 — if implementation moved to its own run, what carries state between runs?**

Would have to be the issue body plus a branch: the only durable, greppable
handoff surface available. Note this is already how the loop recovers today —
`issues.sh summary` plus an open PR is precisely the state a fresh run
reconstitutes from. That is evidence the handoff surface works, and also evidence
that a *second* run is not needed to get it.

**Q3 — what is the deterministic fallback when the worker dies?**

This is the load-bearing question, and #97 already names its own answer:
`nudge-gates.sh` runs **before** the agent step in `genesis-orchestrator.yml`, so
the signal escapes even if the agent later dies at max-turns (#84). The
generalizable rule behind it:

> **Anything a human must receive cannot live inside the agent's turn budget.**

Wrap-up is exactly "something a human must receive". So wrap-up moves out of the
budget — into a deterministic step that runs regardless of how the agent
terminated. That fix is available *without* splitting coordinator from worker.

**Q4 — does this apply to the evolver too, or only the orchestrator class?**

To every Claude-invoking workflow. The invariant is about the *step*, not the
agent, so it is enforced by membership (see the guard below) rather than
per-runner review — the same shape as the turn-budget floors and the concurrency
group, both of which failed exactly once as opt-in properties.

## Decision

**Reject the coordinator/worker split. Adopt deterministic wrap-up instead.**

Reasons, in order of weight:

1. It does not fix the observed failure. In #87 and #96 the *coordinator's* own
   work — assess, read the issue, verify the artifact, report — is what got
   truncated. Moving implementation elsewhere shrinks the run but leaves wrap-up
   inside a budget that can still be exhausted, because verification cost is not
   knowable in advance either.
2. It rests on an unverified premise (Q1). If subagent turns *do* share the pool,
   the split buys nothing at all without also splitting the workflow run — which
   is a large change to the trigger topology for a benefit that (1) already
   undercuts.
3. It makes failure less legible, not more. Today one run maps to one escalation.
   A two-run split creates a new silent-stall surface: a dispatched worker run
   that never starts (the pending-cancellation window in the shared concurrency
   group is documented in `genesis-evolver.yml`) leaves a coordinator waiting on
   an artifact that will never appear. That is the #84 failure mode reintroduced
   at a new layer.
4. The cheap fix is strictly better on the metric that matters. The cost of a
   max-turns death is not the lost turns — it is a human opening an escalation
   that says "run failed" and having to reconstruct by hand whether anything
   landed. Deterministic wrap-up removes that cost for every runner at once.

Recorded so this is not re-litigated on the next max-turns failure: **raising a
budget is no longer a fix, and neither is splitting the agent. The question to
ask instead is "did the human-facing output escape the agent's budget?"**

## What was implemented

`escalate.sh` — already the deterministic `if: failure()` path on every
Claude-invoking workflow — now answers the triage question itself. Before it
opens or updates the escalation, it queries the REST issues endpoint (`sort=updated`,
`since=<run window>`; **not** the search API, which is index-lagged and would
routinely miss an artifact created seconds earlier) and lists every issue and PR
touched during the run.

So an escalation now reads "the run died **and here is the PR it opened first**"
rather than "the run died". The human triage step that CLAUDE.md currently
records as a remembered rule — *verify the artifact first, do not assume nothing
happened* — becomes something the escalation just tells you.

Guard: `test/devsystem/escalate_test.go` asserts (a) every workflow containing
`anthropics/claude-code-action` also has an `if: failure()` step invoking
`escalate.sh` — membership, so a new Claude workflow without a wrap-up path fails
the build — and (b) `escalate.sh` still performs artifact discovery and still
avoids the search API.

## Follow-up: intent checkpointing (the condition fired)

This section previously read "deliberately not solved", with a condition
attached: a run that dies at max-turns still loses its *reasoning* — the
escalation reports what landed, not why the agent thought it was landing it — and
if max-turns deaths continued **after** artifact discovery shipped, checkpointing
intent early was the next thing to try, *not* another raise and *not* a split.

**#101 was that continued death.** The evolver hit max-turns again after PR #99
landed artifact discovery. Triage was genuinely cheaper — the escalation named PR
#100 and a human could see the deliverable had escaped — but it still said
nothing about why the run chose that work. So the condition fired and the
follow-up is now implemented.

### What was implemented

`.genesis/scripts/checkpoint.sh`. The agent calls it once, as its first write,
with a sentence on what it intends to do and why. `escalate.sh` renders it under
**"What this run was trying to do"**, above the landed-artifacts section.

Three design points worth keeping:

1. **A file, not an issue comment.** Intent only matters when the run *fails*;
   posting it every run would put a paragraph nobody reads on an issue every six
   hours. `$RUNNER_TEMP` persists across steps within a job, which is exactly the
   span needed (agent step writes → `if: failure()` step reads). It also means no
   token and no network, so the checkpoint has no API failure mode — a step that
   could 502 on every agent run would manufacture the same false escalation #91
   removed. Accepted cost: a run cancelled mid-flight loses its checkpoint, which
   is the benign-cancellation case `escalate.sh` already declines to report on.
2. **`checkpoint.sh --path` is the single source of truth for the location.**
   `escalate.sh` asks the script rather than recomputing the default. Two scripts
   independently deriving one shared path drift, and the symptom would be an
   escalation reporting "no intent recorded" while the file sits on disk.
3. **Absence is reported, not omitted.** "No intent checkpoint was recorded" is
   itself diagnostic: combined with no artifact, that is the #85 shape — died
   before the first action — and a human should be told which of the two
   happened rather than shown a blank section.

Note this does **not** violate the rule the rest of this doc turns on. Intent is
the one thing that *cannot* be moved outside the agent's budget, because only the
agent knows it. What it gets instead is position: it is the first turn spent,
before the unbounded implementation work that is what actually exhausts the
budget. Same shape as `nudge-gates.sh` running before the agent step (#84) —
early, so it escapes a later death.

### Guard

`test/devsystem/checkpoint_test.go`: membership (every workflow classified at
`orchestratorClassFloor` instructs the checkpoint, and marks it as the run's
FIRST action — placement is the whole point, since a checkpoint written after the
implementation has already been crowded out) plus the read side (`escalate.sh`
renders the intent section, resolves the path via `checkpoint.sh --path`, and
says something explicit when no checkpoint exists) plus a round-trip that
executes the script with a bare environment.

Scope comes from the existing `minTurns` classification rather than a new list.
A workflow at `orchestratorClassFloor` is by definition one whose work is chosen
*during* the run, which is precisely when intent is unrecoverable afterwards; the
narrow fixed-procedure runner (`genesis-merge.yml`) is exempt because its intent
*is* its prompt and the escalation already names the workflow. Deriving both
guards from one classification is what keeps that exemption principled instead of
an ad-hoc name that quietly grows.

### Still open

The instruction currently lives in the **workflow prompts** only. Mirroring it
into `.claude/agents/orchestrator.md` and `.claude/agents/evolver.md` — where an
agent reading its own definition would also see it — was attempted and the edit
was **declined** (the same `.claude/` write restriction that blocked correcting
`evolver.md`'s stale step 4, see CLAUDE.md). The prompt is the surface the
membership test guards, so the property holds; the agent-md mirror is a
readability improvement a human can apply.

### What to do if a max-turns death still loses information after this

Both cheap fixes are now in place: what landed (artifact discovery) and what was
intended (this). What remains unrecoverable is the *middle* — the specific dead
ends a run burned turns on. That is a transcript-retention problem, not a budget
or topology problem, so the next thing to try is exporting the agent transcript
as a run artifact rather than any change to how the work is divided. Raising a
budget and splitting coordinator from worker both remain rejected; see above.
