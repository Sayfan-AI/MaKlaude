# Unattended actions — the disclosure trail

When MaKlaude changes a cluster with nobody watching, it opens a GitHub issue about it.
One issue per action, every time, and it stays open until a person closes it.

This page is about that issue: what it says, why it exists in the shape it does, and the
one label that takes the permission away.

> **This is not the same thing as [`autonomous-mode.md`](autonomous-mode.md).** That
> page documents `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` — a blanket switch that means *a
> person waived review for everything*. This page is about **earned autonomy**: a
> specific rule, for a specific cluster/namespace/operation, that fires only because the
> recorded history says a person approved that exact shape repeatedly and it worked.
> The distinction is load-bearing and the code refuses to collapse it — see
> [Telling the two apart](#telling-the-two-apart).

---

## Why the record is louder than the gated path

Every other write path in MaKlaude ends at a human. The approval gate puts a proposal to
a person and waits; the escalation trail tells a person something is wrong. This one has
no person in it.

So the record is not a supporting artifact. It **is** the entire oversight surface, and
four choices follow from that — each one a deliberate cost:

| Choice | Why, and what it costs |
| ------ | ---------------------- |
| **One artifact per action, never batched** | A digest of "MaKlaude auto-applied 4 things" is cheaper to read and is exactly the shape in which the fourth one goes unnoticed. The noise *is* the oversight. If the volume annoys you, narrow the rules — that is the intended outcome, not a side effect. |
| **The artifact is opened *before* the action runs** | An action that starts and never reports back — a crashed process, an evicted runner, a hung API call — leaves an open artifact with no outcome on it. Opening it afterwards would make that case leave nothing at all, and "nothing at all" is indistinguishable from an idle system. |
| **The artifact stays open** | Closing it is your acknowledgement, not MaKlaude's. An auto-applied action nobody has looked at is a real state and should be visible as one. |
| **It is never rendered as human-approved** | A trail that overstates human involvement is worse than no trail. Every heading asks the authority field before it names anybody. |

## What the artifact says

Opened before the action, under the label `maklaude-autonomous`:

1. **The banner.** `NO HUMAN APPROVED THIS ACTION.` first line, capitals, before anything
   else — because an issue notification shows a title and an opening line, and a chat
   message is skimmed. If exactly one fact survives being skimmed, it has to be this one.
2. **The posture.** Whether a cluster actually changed. A `dry-run` rehearsal and a real
   mutation produce artifacts that differ by one sentence and one table cell, and reading
   one as the other is the most consequential mistake available on this page.
3. **The action** — operation, target, cluster, resourceVersion, reversibility, execution
   mode, diagnosis, intent, expected effect, and the preconditions re-checked immediately
   before acting.
4. **Who permitted it** — the rule name, what it is recorded as (`policy:<rule-name>`),
   the shape, the **fix fingerprint**, and the **trust evidence**: the citation from the
   ledger that stood in for a review. The fingerprint is stated as its own field, in the
   clear, because equality is the only thing it is for: comparing it against another
   artifact's is how an operator answers "is this the same fix that was approved, or did
   it change?". The citation names it too and is blanked there — the redaction sweep can't
   distinguish a published hash from a leaked credential and is right to blank by default,
   so the token is published structurally instead of by weakening the sweep.
5. **The ceiling it ran under** — that the blast-radius budget admitted it, that admission
   consumed one of the pass's auto-applies for that cluster, and that the target's
   cooldown has started.
6. **Revoking this** — see below.

Once the action finishes, the body is rewritten with:

7. **The outcome** — stated in the heading, so it is legible from a notification digest:
   landed and converged, landed and did *not* converge, abandoned cleanly, rehearsed only,
   or did not run. Below it, the full [`audit.Lifecycle`](../internal/audit/render.go)
   rendering: what was sent, the pre-state, the convergence verdict, the rollback story.
8. **What followed** — the consequences the blast-radius layer decided on and whether they
   were carried out. A success says *"Nothing. The action succeeded"* rather than omitting
   the section.
9. **A hidden machine-readable marker** carrying the lifecycle. See
   [Rebuilding the ledger](#rebuilding-the-trust-ledger-from-the-artifacts).

Two labels are applied by MaKlaude and are worth knowing:

- **`maklaude-applied`** — a real mutation landed. Applied the instant the write returns,
  *before* the observation window. Its **absence** on a finished artifact is the only
  evidence that a process died mid-action.
- **`needs:human`** — a failed unattended action. Nobody was watching when it ran, so
  nobody learns unless it is pushed.

## Revoking an action's autonomy

> **Add the `autonomy:revoked` label to the disclosure issue.**

That is the whole signal. It revokes autonomy for that action's **shape** — its
`(cluster, operation)` pair — and it takes effect on MaKlaude's next cycle, which reads
the open disclosures before it decides anything. No configuration change, no restart,
nothing else to remember.

The shape is deliberately **broader than the unit trust is earned at**. Trust is earned
per *fix* (see [The cold start](#the-cold-start-nothing-is-trusted-on-day-one)), but
adding this label is a decision that this kind of act on this cluster needs a person
again — so it blocks every fix of that shape, not the single one you happened to be
looking at. Revocation fails wide, for the same reason demotion does.

Three properties worth stating:

- **It is checked before budget is consumed.** Admission is a reservation — it counts
  against the pass cap and starts the target's cooldown — so a revoked shape never burns
  the pass's allowance on actions it is not permitted to take.
- **The proposal is not dropped, it is gated.** A revoked shape goes to the ordinary human
  approval path, exactly as an untrusted one does. Revoking autonomy stops MaKlaude acting
  alone; it does not stop MaKlaude asking.
- **It is an override, not a demotion.** It does not rewrite the trust ledger, because
  your decision to stop trusting a shape is not something that happened to a cluster.
  Lifting it is removing the label or closing the issue — both one action.

To stop **all** unattended action on a cluster rather than one shape, trip its circuit
breaker; the state summary's **Autonomy (blast radius)** section reports the breakers
unconditionally.

**If the disclosure trail cannot be read, nothing is auto-applied that pass.** A failed
read produces an empty revocation list, an empty list is indistinguishable from "nothing
is revoked", and acting on that basis would turn a network blip into a grant of authority.
The state summary says so in those words.

## Telling the two apart

The blanket bypass and an earned rule both record `audit.AuthorityPolicy` and both put a
`policy:` marker where a login would go. They are not the same thing and the code will not
let them read as the same thing:

| | Blanket bypass | Earned rule |
| --- | --- | --- |
| Recorded identity | `policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` | `policy:<rule-name>` |
| What it means | A person waived review for everything | A person approved this shape repeatedly, it converged, and the ledger promoted it |
| Evidence behind it | None — it is a switch | A trust citation, **required**: a slip with no citation is refused at the mint |
| Where it is recorded | The approval artifact it was granted against | Its own disclosure artifact |
| Trail | `maklaude-proposal` | `maklaude-autonomous` |

The two identities cannot collide: rule names are validated lowercase, and the bypass's
marker is an upper-case environment variable name. `approve.GrantAutonomous` refuses
outright if a rule ever renders as the bypass, and the disclosure body states which of the
two authorized the action rather than merely mentioning both.

## What has to be true before anything runs unattended

All four, and none of them defaults:

| # | What | Where it comes from |
| - | ---- | ------------------- |
| 1 | **A rule** naming the cluster, the namespace, and the operation. There is no wildcard; an empty selector makes a rule invalid rather than permissive, and one invalid rule gates the entire ruleset | `MAKLAUDE_AUTONOMY_RULES` |
| 2 | **Earned trust** — a ledger citation for that shape. A ledger that claims trust and cites nothing is refused | `MAKLAUDE_TRUST_LEDGER` |
| 3 | **A blast-radius budget** — the per-pass cap, the per-target cooldown, and the per-cluster circuit breaker. Eligibility with no ceiling is how one bad diagnosis becomes fifty restarts, so an unbounded deployment auto-applies nothing | `MAKLAUDE_AUTONOMY_STATE` |
| 4 | **A disclosure trail** to write to. An unattended mutation with no record is the one outcome this milestone forbids, so *nowhere to disclose* means *nothing to disclose* | `MAKLAUDE_GITHUB_REPO` + `MAKLAUDE_GITHUB_TOKEN` |

Plus the two gates that were already there: the write-path kill switch
(`MAKLAUDE_EXECUTE_MODE`) and everything in [`remediation.md`](remediation.md) — the
scoped RBAC bundle, the precondition re-check, the `resourceVersion` enforcement. Earned
autonomy opens exactly one of them.

## Enabling it

**Autonomy is off by default**, and off means no rule exists — not a flag set to false.
With `MAKLAUDE_AUTONOMY_RULES` unset, `autonomy.Decide` answers
`autonomy-not-configured` for every proposal and every one of them takes the human gate,
which is Milestone 4's behaviour exactly.

Turning it on is three variables and one file:

```bash
export MAKLAUDE_EXECUTE_MODE=enabled                              # or dry-run to rehearse
export MAKLAUDE_AUTONOMY_RULES=/etc/maklaude/autonomy.yaml        # the grant
export MAKLAUDE_TRUST_LEDGER=/var/lib/maklaude/trust.jsonl        # the history it is earned from
export MAKLAUDE_AUTONOMY_STATE=/var/lib/maklaude/autonomy-state.json  # the ceiling it runs under
export MAKLAUDE_GITHUB_REPO=owner/repo                            # where each action is disclosed
export MAKLAUDE_GITHUB_TOKEN=...                                  # token with issues:write
```

The rules file is documented, with a worked example and the mistakes it is shaped to
prevent, in [`autonomy.example.yaml`](../autonomy.example.yaml). The shape of one rule:

```yaml
version: 1
rules:
  - name: staging-web-restart
    clusters: [staging]
    namespaces: [web, api]
    operations: [rolloutrestart]
    maxReversibility: reversible   # optional; omitted means the strictest class
```

It is a **separate file from the cluster registry** on purpose. That file is copied,
templated and committed, and "the checked-in example turned autonomy on" is a failure
mode worth designing out.

### Half-configuring it is an error, not a quieter posture

Setting the rules variable without the ledger or the state file makes
`maklaude remediate` **refuse to start**, naming the variable to fix. That is deliberate,
and the reason is the failure mode rather than tidiness: rules with no ledger can never
promote a shape, and rules with no ceiling have nothing to bound them, so either one
produces a deployment where autonomy is configured, valid, and silently incapable of ever
firing — which is indistinguishable from one where nothing has earned trust yet. A
malformed or unreadable rules file refuses to start for the same reason; an empty ruleset
would grant nothing, which is a perfectly safe way to be completely wrong.

Two configurations are reported rather than refused, and both leave autonomy **off**:

- **The kill switch.** `MAKLAUDE_EXECUTE_MODE=disabled` with autonomy fully configured is
  not an error. Setting the kill switch must always be a safe action; a binary that
  refused to start because of it would turn the kill switch into an outage.
- **A disclosure trail that reaches nobody.** With no `MAKLAUDE_GITHUB_*` configuration
  the trail degrades to an in-memory one, so an unattended action's only record would die
  with the process. Autonomy does not engage, the gated path is unaffected, and the report
  names the variables to set.

### Confirming what you configured

The report's first autonomy line is the posture, printed every pass whether or not
anything happened:

```
Unattended actions: ON — 2 autonomy rule(s) from /etc/maklaude/autonomy.yaml; trust ledger /var/lib/maklaude/trust.jsonl
  a shape still gates until it has EARNED autonomy, and every auto-apply is disclosed
```

```
Unattended actions: OFF — no autonomy rules are configured (MAKLAUDE_AUTONOMY_RULES is unset), so every proposal takes the human gate
```

`OFF` always states its cause, because "autonomy is off" has half a dozen of them and
they all look identical from the outside: a report with no unattended actions in it. The
distinction that matters is between an operator who has one variable left to set and one
who believes autonomy is on and is quietly wrong.

### The cold start: nothing is trusted on day one

A freshly enabled deployment auto-applies **nothing**, and keeps auto-applying nothing
until a shape has earned it. There is no seed, no bootstrap list and no "start trusted"
flag, because trust is derived from recorded history and never declared — a config file
that granted autonomy without evidence would be the blank cheque
`MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` already is, wearing the word "earned".

So the first pass after enabling autonomy looks exactly like the last pass before it:
every proposal goes to a person. What changed is that each approval is now **evidence**.

Promotion needs **3 human-approved executions that converged, carrying the same
fingerprint** — and nothing demoting on the shape. The three don't have to be recent. An
approval stays good until the fix it was given for changes or stops working, so an
approval from a year ago counts exactly as much as one from this morning.

An auto-applied success is worth nothing here: only an execution a *human* approved can
promote, so autonomy doesn't compound. Enabling autonomy on a cluster and seeing nothing
auto-applied for weeks is the mechanism working.

#### Trust attaches to the fix, not to the operation

A **fingerprint** is a short opaque token summarizing everything a cached approval
depends on: the planner version, the operation, *which* object (cluster, kind, namespace,
name), the diagnosed cause, the reversibility, which preconditions the fix carries, and
the expected values of the preconditions that bind. Change any of those and the proposal
gets a new fingerprint, which no past approval covers, so it returns to the gate.

Deliberately **not** in it: the target's `resourceVersion` (it moves on every write by
anyone, so including it would re-gate everything every cycle — drift is caught at execute
time by sending the precondition to the API server instead), the crashlooping pod's
identity (a different object after every restart), the hypothesis and confidence, and
every prose field. Trust must not lapse because someone fixed a typo. The full field-by-
field reasoning is in [`internal/remediate/fingerprint.go`](../internal/remediate/fingerprint.go).

This is what closes the hole the `(cluster, operation)` key had: three approved
rollout-restarts on `prod` used to earn the right to restart *any* deployment on `prod`
indefinitely, including workloads that didn't exist when the approvals were given.

#### What ends trust

Promotion is scoped to the fix; **demotion is scoped to the shape**. One demoting outcome
anywhere in the shape's last **10** recorded executions blocks *every* fingerprint of that
shape, not just the one that went wrong.

That asymmetry is the safety argument. A restart that failed is evidence about restarting
things on that cluster. If demotion were fingerprint-scoped, any change to the fix would
mint a fresh fingerprint with a clean record — turning "the fix changed" from a reason to
re-earn trust into a way to launder a failure. The narrow scope earns, the broad scope
blocks, and each direction is the one that fails closed.

| Trigger | Scope | What it means |
| ------- | ----- | ------------- |
| The fix changed | The fingerprint | The approved thing and the proposed thing aren't the same thing, so the cached approval doesn't cover it. Re-approval required |
| Failure, rollback, or drift-abort | The shape, for its next 10 recorded executions | The fix stopped working. Immediate on a single occurrence |
| **Regression** — the same fault diagnosed again on the same object within **1 hour** of an execution that reported convergence | The shape | A fix that has to be applied again isn't a fix. Recorded as `regressed`, and it demotes |
| **Unobserved streak** — the 3 most recent executions all inconclusive | The shape | MaKlaude can no longer tell whether this fix works, so it stops acting as if it does |

The last two carry weight that used to be carried by a counting window, and they're the
reason removing it was safe. The window forced a person back into the loop on a schedule
regardless of whether the health signal was telling the truth — dumb, but a backstop.
Removing it would otherwise put the whole safety burden on MaKlaude's own convergence
check, which is a bounded observation immediately after an action: structurally the same
thing as the milestone-1 crashloop detector that read a pod one instant after a restart
and concluded it was fine. Recurrence catches the convergence check saying yes and being
wrong; the unobserved streak catches it being unable to say anything at all. Both source
the backstop from evidence rather than from a schedule.

The **10** is a recovery bound, not an evaluation window: without it, one failure would
block a shape forever and the only escape would be hand-editing the ledger. A demoted
shape climbs back by accumulating 10 further recorded executions with nothing demoting
among them — and since an auto-applied success isn't recorded at all, every one of those
is an execution a human approved. Recovery is real, bounded, and driven entirely by
people.

### What it will and will not do once trusted

The bounds are **not configurable** — a rules file can narrow what runs unattended, never
raise the ceiling it runs under:

| Bound | Value | What it does |
| ----- | ----- | ------------ |
| Per-cluster, per-pass cap | 2 | The most actions one cluster may auto-apply in a single pass. Admission *consumes*: it counts against the cap and starts the cooldown |
| Per-target cooldown | 30 minutes | One target is off limits to autonomy for this long after an auto-apply is admitted for it |
| Circuit breaker | 2 consecutive failures | Trips that cluster's breaker and takes it **fully gated** until a human clears it. Consecutive: one success resets the count |

A breaker is not a quiet state. Every pass prints the **Autonomy (blast radius)** section
whether or not anything is tripped, tripped breakers name the cluster and the instant, and
a failure run that has *not* yet tripped is printed separately — the warning before the
outage. A tripped breaker also survives a restart, because the condition that tripped it
is a cluster MaKlaude is wrong about and that outlasts any one process.

Note what a bound does **not** do: it holds an action back from running *unattended*, not
from running at all. A suppressed auto-apply and a tripped breaker both fall through to
the ordinary human approval path, so a tripped cluster is one where MaKlaude still
proposes and asks — not one where it has gone quiet.

### Revoking it — five scopes

| Scope | How | Takes effect |
| ----- | --- | ------------ |
| One **shape** | Add `autonomy:revoked` to its disclosure issue | Next cycle, before anything is decided. See [above](#revoking-an-actions-autonomy) |
| One **cluster** | Remove it from the rule's `clusters:` list, or trip its breaker | Next pass |
| All **earned trust** | Delete the trust ledger file | Next pass — every shape re-gates until the history is rebuilt from the artifacts |
| All **unattended action** | Unset `MAKLAUDE_AUTONOMY_RULES` | Next start |
| All **writes**, gated too | `MAKLAUDE_EXECUTE_MODE=disabled`, or `kubectl delete -k deploy/rbac/write` | Next start; the RBAC deletion is immediate and needs no restart |

Deleting the ledger is safe in the direction that matters: it is a cache of the approval
artifacts, not the authority, so nothing is lost that a rebuild cannot recover — and until
it is rebuilt, nothing is trusted.

## Rebuilding the trust ledger from the artifacts

The trust ledger is a **cache** of the artifacts, not the authority over them. The hidden
marker in each disclosure body is what makes that true rather than aspirational: it
carries the fields `trust.EntryFrom` reads — proposal identity, **fingerprint**, cluster,
operation, authority, convergence verdict, failure class, clean-abort flag,
rollback-attempted flag, and the attempt's finishing instant — so a ledger rebuilt from
nothing but the bodies reproduces the live one entry for entry.

The fingerprint is carried with `omitempty`, so an artifact written before fingerprints
existed rebuilds into an entry with the empty fingerprint. That's the correct reading
rather than a lossy one: the empty fingerprint matches no computed fingerprint, so an
old entry can never authorize anything on its own, and the rebuilt history stays a
faithful record of what was approved.

Two design notes that are easy to get wrong in the other direction:

- **The marker carries a projection, not the records.** Everything omitted is either free
  text redaction already touched or navigational, and none of it can change which entry a
  lifecycle projects to. So a world-readable artifact carries no cluster-derived free text
  in its hidden part.
- **An unreadable marker fails loudly; an absent one does not.** A missing marker is an
  action still in flight and contributes nothing, which is normal. A marker that exists and
  cannot be parsed is history about to be lost, and a rebuild that silently produces a
  *shorter* history than the truth is the dangerous direction — a lost failure re-grants
  autonomy, while a lost approval merely delays it.

## An unattended success never builds more trust

Only a **human-approved** execution can promote. An auto-applied execution that converged
is recorded nowhere in the ledger — it neither promotes nor erodes — and that's a
correctness requirement rather than an oversight, for two reasons:

- **It preserves the meaning of the count.** The number measures how much a human has
  sanctioned, not how much MaKlaude has done.
- **It keeps recovery in human hands.** Demotion is cleared by 10 further recorded
  executions with nothing demoting among them. If auto-applied successes were recorded,
  a demoted shape could clear its own block simply by succeeding at what it was demoted
  for — autonomy buying back its own autonomy.

Note what the exclusion does **not** cover, because the two halves are easy to conflate.
An auto-applied execution that **failed**, rolled back, drift-aborted, or regressed *is*
recorded and demotes exactly like a human-approved one. Autonomy can't earn itself more
autonomy, and it's fully able to lose the autonomy it has.

## Where this lives in the code

| Concern | Code | Tests |
| ------- | ---- | ----- |
| The artifact: rendering, labels, the revocation section | [`internal/disclose/issue.go`](../internal/disclose/issue.go) | [`internal/disclose/issue_test.go`](../internal/disclose/issue_test.go) |
| The machine-readable marker and the rebuild | [`internal/disclose/marker.go`](../internal/disclose/marker.go) | [`internal/disclose/marker_test.go`](../internal/disclose/marker_test.go) |
| Opening, completing, and reading revocations | [`internal/disclose/trail.go`](../internal/disclose/trail.go) | [`internal/disclose/trail_test.go`](../internal/disclose/trail_test.go) |
| The permission slip an unattended action runs under | [`internal/approve/autonomous.go`](../internal/approve/autonomous.go) | [`internal/approve/autonomous_test.go`](../internal/approve/autonomous_test.go) |
| The order it all happens in | [`internal/operate/autoapply.go`](../internal/operate/autoapply.go) | [`internal/operate/autoapply_test.go`](../internal/operate/autoapply_test.go) |
| Deciding one proposal | [`internal/autonomy/`](../internal/autonomy) | same package |
| Bounding them across proposals and time | [`internal/budget/`](../internal/budget) | same package |
| Earning and losing trust | [`internal/trust/`](../internal/trust) | same package |

## Related

- [`autonomous-mode.md`](autonomous-mode.md) — the blanket bypass, which this is not.
- [`remediation.md`](remediation.md) — the gated write path earned autonomy opens one gate of.
- [`rbac.md`](rbac.md) — what MaKlaude is permitted to do at all, which no authority changes.
- [`no-writes.md`](no-writes.md) — the observation path's guarantee, untouched by any of this.
