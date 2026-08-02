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
   the shape, and the **trust evidence**: the citation from the ledger that stood in for a
   review.
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
`(cluster, operation)` pair, the granularity trust is earned at — and it takes effect on
MaKlaude's next cycle, which reads the open disclosures before it decides anything. No
configuration change, no restart, nothing else to remember.

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

1. **A rule** naming the cluster, the namespace, and the operation. There is no wildcard;
   an empty selector makes a rule invalid rather than permissive, and one invalid rule
   gates the entire ruleset.
2. **Earned trust** — a ledger citation for that shape. A ledger that claims trust and
   cites nothing is refused.
3. **A blast-radius budget** — the per-pass cap, the per-target cooldown, and the
   per-cluster circuit breaker. Eligibility with no ceiling is how one bad diagnosis
   becomes fifty restarts, so an unbounded deployment auto-applies nothing.
4. **A disclosure trail** to write to. An unattended mutation with no record is the one
   outcome this milestone forbids, so *nowhere to disclose* means *nothing to disclose*.

Plus the two gates that were already there: the write-path kill switch
(`MAKLAUDE_EXECUTE_MODE`) and everything in [`remediation.md`](remediation.md) — the
scoped RBAC bundle, the precondition re-check, the `resourceVersion` enforcement. Earned
autonomy opens exactly one of them.

> **Configuring the rules is not wired yet.** The mechanism is complete and tested; where
> the ruleset and the ledger path come from is the configuration surface, and it lands
> with the document that describes it (task T7, issue #147). A deployment today has no
> rules, so `autonomy` is off and every proposal takes the human gate — which is
> Milestone 4's behaviour exactly.

## Rebuilding the trust ledger from the artifacts

The trust ledger is a **cache** of the artifacts, not the authority over them. The hidden
marker in each disclosure body is what makes that true rather than aspirational: it
carries the fields `trust.EntryFrom` reads — proposal identity, cluster, operation,
authority, convergence verdict, failure class, clean-abort flag, rollback-attempted flag,
and the attempt's finishing instant — so a ledger rebuilt from nothing but the bodies
reproduces the live one entry for entry.

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

Only a **human-approved** execution can promote a shape. An auto-applied success is
recorded nowhere in the ledger, and that is a correctness requirement rather than an
oversight: the ledger's standing is computed over a fixed window of the most recent
entries, so writing non-promoting successes into it would push the human approvals that
earned the trust out of the window and silently un-earn it. A shape that worked perfectly
would revoke its own autonomy after a handful of successes.

Failures **are** recorded, which re-gates the shape until its history recovers.

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
