# Autonomous mode — the approval bypass

MaKlaude's default posture is that **a named human approves every mutating action
before it runs**. Autonomous mode is the one supported way to turn that off:
`MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1` lets MaKlaude authorize its own proposals and
close the remediation loop unattended.

Read this page before you set it. The short version is on the tin — it is
*dangerously* auto-approve, in the spirit of `claude --dangerously-skip-permissions` —
but the useful version is the exact list of what it gives up and, just as important,
what it does not.

> **Nothing here bypasses the write-path kill switch.** Autonomous mode answers
> "may this run?". `kube.ExecuteMode` answers "may a real write leave the process?".
> They are independent gates and both must open. Auto-approval while the executor is
> in `ExecuteDryRun` produces an unattended *rehearsal*, not an unattended change.

> **This is not the only way MaKlaude can act unattended, and it is the blunt one.**
> [`unattended-actions.md`](unattended-actions.md) documents *earned* autonomy: a rule
> scoped to one cluster, namespace and operation, which fires only because the recorded
> history says a person approved that exact shape repeatedly and it converged every time.
> This page's switch waives review for **everything** and cites nothing; an earned rule
> waives it for one shape and must cite the history that earned it. The audit trail keeps
> them apart by name — `policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` here,
> `policy:<rule-name>` there — and a renderer that collapsed the two would be a bug.

---

## What it gives up

**One thing: a person deciding.** That is the whole of it, and it is a lot.

| You lose | Concretely |
| -------- | ---------- |
| Human judgment on the specific action | Nobody looks at the dry-run diff, the diagnosis, or the blast radius before MaKlaude acts. The catalog's four operations are the only bound on what it will do. |
| The chance to say "not this one, not right now" | The approval issue is still opened, but it is a **notice**, not a question. If nobody reads it within one reconciliation cycle, the action runs. |
| Named accountability | The audit trail records `policy:MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` as the authorizer. No login appears, because none applied. Attribution stops at "the operator who set this variable", which is a much weaker claim than "this person reviewed this change". |
| A meaningful `needs:human` signal | The label is still applied so the issue surfaces in your queue, but MaKlaude is not waiting for you to act on it. |

## What it does **not** give up

Autonomous mode waives **consent** and nothing else. Every other refusal in the gate
and in the executor still fires:

| Still enforced | What it means |
| -------------- | ------------- |
| A human's `rejected` label | A person saying no beats configuration saying go. Adding `rejected` to the issue stops that action and keeps it stopped while the proposal persists. |
| resourceVersion drift | If the target object changed since the artifact displayed it, the action MaKlaude previewed and the action now possible are not the same action. It re-renders and re-decides against what is true now, rather than acting on the old view. |
| A failed dry-run | If the API server rejects the action as a preview, MaKlaude will not send it for real. |
| The executed-label idempotency flag | An artifact already marked `maklaude-executed` is never authorized a second time, across process restarts. |
| Precondition re-checking | Every precondition the artifact listed is re-evaluated against a fresh cluster read immediately before the action. Any that no longer holds aborts it, having sent nothing. |
| Missing rollback plan | An operation with no defined way to undo it is refused, however it is labelled. |
| Irreversible actions | The executor refuses them outright. An approval — human or policy — cannot make one reversible. |
| Self-heal | If the problem clears on its own before the action runs, the request is withdrawn **without running anything**. |
| Multi-cluster isolation | An authorization is still scoped to one cluster, one object, one resourceVersion, once. |
| The write-path kill switch | `kube.ExecuteMode` still governs whether a real mutation is sent. |

## It never claims a human reviewed it

This is the property the implementation bends hardest to keep. A trail that overstates
human involvement is worse than no trail: it launders an unreviewed action into a
reviewed one, permanently, in the artifact an incident review will trust.

So an auto-approved action is visibly distinct everywhere it appears:

- **The approval issue body** leads with `AUTONOMOUS MODE IS ENABLED`, says in so many
  words that no human will review the action, and replaces "How to decide" with
  "How to stop this".
- **The chat notice** (if Slack is configured) says the same, instead of the usual
  "nothing runs until a human adds the `approved` label".
- **The authorization comment** on the issue reads *"Auto-approved — NO HUMAN REVIEWED
  THIS"*, names the environment variable that waived the requirement, and does not
  `@`-mention anyone.
- **The execution comment** reads *"Executed — NO HUMAN REVIEWED THIS"* rather than
  "approved by @someone".
- **The audit trail** records `audit.AuthorityPolicy` with the policy marker as the
  identity and **no** approval timestamp, and `audit.Lifecycle` renders an explicit
  *"No human reviewed this action. It ran on configured policy alone."*
- **The process log** emits one `WARNING: AUTO-APPROVED WITHOUT HUMAN REVIEW` line per
  action, naming the operation, the target, the cluster, and the resourceVersion.

A genuine human approval is still recorded as one even while autonomous mode is on: if
you add the `approved` label from your own account, the trail names you. The authority
describes what happened to *that action*, not how the process was configured.

## Turning it on

```bash
export MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1
```

Accepted values are `1` and `true` for on, and `0`, `false`, or unset for off, all
case-insensitive. **Anything else is a fatal startup error**, deliberately — the lazy
"any non-empty value is truthy" parse would turn `=no` and `=off` into an armed
autonomous mode set by somebody trying to disable it.

Turning it back off is unsetting the variable and restarting. Every open request then
goes back to waiting for a human, and MaKlaude re-renders each artifact so its body
stops describing a posture that is no longer in force.

## The pairing: `MAKLAUDE_GITHUB_SELF_LOGIN` is now required

Autonomous mode shipped together with a second change, and the two are one decision.

MaKlaude's self-approval defense refuses a decision label applied by MaKlaude's own
account — a system that can approve its own proposals has no gate at all. It recognizes
itself two ways: the actor is a GitHub App (a `Bot`, or a login ending in `[bot]`), or
the login matches `MAKLAUDE_GITHUB_SELF_LOGIN`.

The bot check covers the deployment MaKlaude ships as. It does **not** cover MaKlaude
running under a person's own personal access token, where its label events look exactly
like a human's — and `MAKLAUDE_GITHUB_SELF_LOGIN` was optional, so in practice the
check failed **open**: MaKlaude could label its own approval issue and the gate read a
human approval.

That could not simply be made mandatory before, because there was no third answer.
Requiring it would have made a shared-identity deployment unusable — the operator's own
approvals would be refused as self-approval — and the only alternative was leaving the
gate silently inoperative. Autonomous mode is the third answer.

So the rule is now:

> When a **live GitHub comms trail** is configured and autonomous mode is **off**,
> `MAKLAUDE_GITHUB_SELF_LOGIN` must name the account MaKlaude acts as. Starting without
> it is a fatal error, not a warning.

```bash
export MAKLAUDE_GITHUB_SELF_LOGIN=maklaude-bot   # whoever MAKLAUDE_GITHUB_TOKEN belongs to
```

Two escapes, both deliberate:

- Set `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1` instead. You have then said out loud that no
  approval promise is being made, which is honest.
- Run with no GitHub credentials at all. The gate degrades to an in-memory sink, no
  artifact is ever created, no artifact is ever labelled, and therefore no authorization
  is ever issued — there is no labeler to mistake for a human, so there is nothing for
  the identity to protect.

What is no longer available is the silent middle, where the gate looks armed and is not.

## When you should not use this

- **Against a cluster you cannot afford to have restarted at 3am.** The catalog is
  small and reversible, but "reversible" is not "harmless".
- **Before you have watched MaKlaude propose actions for a while with the gate on.**
  The proposals are the thing to build trust in; autonomous mode only removes the pause.
- **While MaKlaude and you share one GitHub identity** — see below.

## Related

- **Shared identity in local mode** (issue #125) is the structural fix behind the
  self-login requirement. Under `genesis serve`, MaKlaude inherits the operator's own
  token, so agent and operator are literally the same GitHub account and no
  label-attribution logic can tell them apart. Giving the local agent its own identity
  is what would make attribution possible there again; until then, that deployment must
  either set the self-login (accepting that its own operator's approvals are refused) or
  run in autonomous mode.
- [`no-writes.md`](no-writes.md) — the four-layer guarantee that the *observation* path
  never mutates a cluster. Autonomous mode does not touch it.
- [`rbac.md`](rbac.md) — the separate, least-privilege identity that can execute the
  approved actions at all. Autonomous mode changes who authorizes an action, never what
  MaKlaude is permitted to do.
- [`remediation.md`](remediation.md) — the whole gated-write path. Autonomous mode opens
  exactly one of its five gates; the RBAC bundle, the `kube.ExecuteMode` kill switch, the
  precondition re-check, and the `resourceVersion` enforcement are all untouched by it.

## Where this lives in the code

| Concern | Code | Tests |
| ------- | ---- | ----- |
| The bypass, its env var, the policy marker, and the self-identity rule | [`internal/approve/autoapprove.go`](../internal/approve/autoapprove.go) | [`internal/approve/autoapprove_test.go`](../internal/approve/autoapprove_test.go) |
| Where consent is waived and where it deliberately is not | [`internal/approve/reconcile.go`](../internal/approve/reconcile.go) (`Decide`, `disqualify`) | same |
| The permission slip and its authority | [`internal/approve/authorization.go`](../internal/approve/authorization.go) | same |
| How a waived action reads in the audit trail | [`internal/execute/audit.go`](../internal/execute/audit.go), [`internal/audit/`](../internal/audit) | [`internal/execute/audit_test.go`](../internal/execute/audit_test.go) |
| Composition with the write-path kill switch | [`internal/execute/execute.go`](../internal/execute/execute.go) | [`internal/execute/execute_test.go`](../internal/execute/execute_test.go) |
