# Gated remediation — how a mutating action actually happens

Through Milestone 3, MaKlaude could only look. Milestone 4 gave it the ability to
*act*, and this page is the operator-facing account of what that means: what it
can change, what has to be true before it changes anything, what record it leaves,
and how to undo it.

Read [`autonomous-mode.md`](autonomous-mode.md) alongside this one. That page
describes the single supported way to switch the human requirement off; this page
describes everything that stays on when you do.

> **Where this stands today.** Every stage below is implemented, tested, and
> **reachable from the shipped binary** — from exactly one command,
> `maklaude remediate`. `maklaude scan` cannot execute anything, which is why the
> two are separate commands rather than one command with a flag: scan's promise
> that nothing it does can change a cluster is worth being unable to weaken by
> passing an argument.
>
> `remediate` is itself propose-only until an operator sets `MAKLAUDE_EXECUTE_MODE`,
> and with that variable unset it constructs no write-capable client at all. So
> "writes are off by default" is one variable an operator has to set deliberately,
> plus every gate below — none of which was relaxed to make the command reachable.
>
> Since Milestone 5 there is a second, narrower way for an action to be authorized:
> a rule an operator wrote, for a shape a recorded history of human approvals has
> **earned**. It is off by default in the same sense — no rule exists until an
> operator writes one — and it opens exactly one of the five gates below. See
> [`unattended-actions.md`](unattended-actions.md).

## The pipeline

`scan`'s read-only path ends at an escalation. Remediation extends it, and every
new stage is a separate package with a separate job:

| Stage | Package | What it does | Can it change a cluster? |
| ----- | ------- | ------------ | ------------------------ |
| Propose | [`internal/remediate`](../internal/remediate) | Turns a diagnosed root cause into zero or more typed, previewable `Proposal`s. A pure function of (snapshot, hypothesis) — holds no client | No |
| Approve | [`internal/approve`](../internal/approve) | Publishes a proposal as its own artifact and waits for an attributable decision. Holds no cluster client and cannot execute what it authorizes | No |
| Execute | [`internal/execute`](../internal/execute) | Re-checks preconditions against a fresh read, sends exactly one request, watches for convergence for a bounded window | Yes — this is the only one |
| Write | [`internal/kube`](../internal/kube) (`Executor`) | The single-request primitive underneath: one method, one path, one object, one `resourceVersion` | Yes |
| Audit | [`internal/audit`](../internal/audit) | Appends one immutable record per lifecycle event and renders the trail | No |

Nothing in the first two stages can mutate anything, which is what makes it safe
to compute proposals continuously and show them to a human before anything is at
stake.

## The action catalog is closed

Four operations, and no mechanism to express a fifth without a code change:

| Operation | Reversibility | What it does | Executor primitive |
| --------- | ------------- | ------------ | ------------------ |
| `rolloutrestart` | `reversible` | Stamps `kubectl.kubernetes.io/restartedAt` on a Deployment's pod template — `kubectl rollout restart` at the API level. The Deployment's own update strategy governs the replacement, so the workload is not taken down | `PatchDeployment` (strategic merge) |
| `rollbackrevision` | `reversible` | Replaces `/spec/template` with a previous revision's pod template — `kubectl rollout undo`. Needs a JSON patch rather than a strategic merge, because a merge cannot *remove* what the current revision added | `PatchDeploymentJSON` (RFC 6902) |
| `cordonnode` | `reversible` | Sets `spec.unschedulable` so the scheduler places no new pods on a node. Cordoning is **not** draining — pods already running there are left alone | `PatchNode` (strategic merge) |
| `deletepod` | `recreated-by-controller` | Deletes one already-failed pod whose controller will recreate it | `DeletePod` |

The third reversibility class, `irreversible`, exists in the model and nothing in
the catalog currently carries it. Proposals are ordered safest-first by this
class, because it is the field that tells a human how much scrutiny to apply.

## Every gate, and why each one is separate

An action reaches a cluster only if **all** of these are open. They are
deliberately independent: each is opened by a different person doing a different
thing, in a different place, and no one of them implies another.

| Gate | Where it lives | Default | Who opens it |
| ---- | -------------- | ------- | ------------ |
| API-server permission | [`deploy/rbac/write/`](../deploy/rbac/write/), a separate bundle binding a separate ServiceAccount (`maklaude-executor`) | not installed | a cluster admin, with `kubectl apply -k` |
| Kill switch | `kube.ExecuteMode`, fixed at construction | `ExecuteDisabled` — the zero value | an operator, by explicitly building an executor in a permitting mode |
| Human approval | `internal/approve` — an `approved` **label event** on the proposal artifact | no approval | a named person, from an identity MaKlaude cannot forge |
| Preconditions | re-checked immediately before the request, against a fresh cluster read | must hold | nobody: the cluster itself |
| Optimistic concurrency | `metadata.resourceVersion`, injected by the executor and enforced by the API server | must match | nobody: the API server |

Two consequences worth internalizing:

- **Installing the RBAC bundle does not enable execution, and enabling execution
  does not grant permission.** Deleting the bundle is the cheaper revocation — it
  stops every write at the API server without touching MaKlaude's config or
  restarting it, and leaves the read / diagnose / propose path fully working.
- **Autonomous mode opens exactly one of these gates.**
  `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1` waives *consent*. It does not install
  RBAC, does not move the kill switch, does not skip a precondition, and does not
  relax the `resourceVersion` check. Auto-approval while the executor is in
  dry-run produces an unattended **rehearsal**, not a change. See
  [`autonomous-mode.md`](autonomous-mode.md).
- **So does earned autonomy, and it is the same one.** A rule an operator wrote,
  for a shape the recorded history has earned, substitutes for the human-approval
  gate and for nothing else: same RBAC bundle, same kill switch, same fresh
  precondition re-check, same `resourceVersion`. It also adds bounds the gated path
  does not have — a per-pass cap, a per-target cooldown, a per-cluster circuit
  breaker, and one disclosure artifact per action, opened *before* it runs. See
  [`unattended-actions.md`](unattended-actions.md).

### The kill switch has three positions

`kube.ExecuteMode` is the one switch over the entire write path, with no
per-cluster and no per-action override:

| Mode | Token | What happens |
| ---- | ----- | ------------ |
| `ExecuteDisabled` | `disabled` | Nothing. `kube.NewExecutor` refuses to build a client at all, so a deployment that has not opted in holds **no write-capable object** to misuse. This is the zero value and the shipped default |
| `ExecuteDryRun` | `dry-run` | Previews only. Every action is sent with `dryRun=All`, and the transport guard *refuses* any mutating request that lacks the marker, so the API server validates the change against real admission controllers and then discards it |
| `ExecuteEnabled` | `enabled` | Real, approved mutations. The only mode under which a cluster changes |

`kube.ParseExecuteMode` maps an operator token to a mode; an empty value and
`disabled` are the same posture, and an unrecognized value is an **error** rather
than a silent default in either direction.

Note the interaction with RBAC that surprises people:
**a dry run still needs the write verb.** Kubernetes authorizes a `dryRun=All`
request with the identical verb it would use for a real one — `SubjectAccessReview`
has no notion of a preview — so `kubectl auth can-i patch deployments` prints
`yes` for the executor identity even where MaKlaude has never been permitted to
change anything. Preview-only is enforced by the mode and the transport, **not**
by RBAC. [`rbac.md`](rbac.md#dry-run-still-needs-the-write-verb) has the detail.

## The request is scoped to one request

The observation path's guard refuses every mutating verb. The write path does not
loosen that guard — it is a **sibling** of it, a different type installed by a
different constructor, so nothing an operator does to enable execution can affect
a `kube.Client`.

What the write path installs instead is a `WriteScope`: one HTTP method and one
exact path, matched exactly rather than by prefix. Each action builds its own
clientset for its own scope, uses it once, and drops it. That costs a TLS
handshake per action, and it is the point — a cached write-capable clientset
carries the union of every action's authority for as long as it lives, while a
per-action one carries only the authority for the action a human approved. Exact
matching is also what excludes a collection delete, whose path is a strict prefix
of a single pod's.

Three refusals sit on top of it, all in
[`internal/kube/executor.go`](../internal/kube/executor.go):

- **No unconditioned variant exists.** The executor injects
  `metadata.resourceVersion` into the patch body itself rather than trusting the
  caller, so there is no unconditional call to reach for under time pressure. A
  target that moved since the proposal fails with `ErrPreconditionConflict` —
  the expected, healthy outcome of a stale approval, and distinct from a real
  failure so a caller can re-propose rather than escalate. Note the boundary: only
  a 409 becomes that sentinel. A target that *vanished* rather than moved returns
  404 and is classified `execute-failed`, which is the wrong verdict for a delete
  whose goal state has thereby been reached — see
  [issue #214](https://github.com/Sayfan-AI/MaKlaude/issues/214) and the window-2
  table in [`chaos.md`](chaos.md#when-a-fault-lands-during-a-remediation).
- **A patch cannot retarget itself.** A body that sets `metadata.name`,
  `metadata.namespace`, or `metadata.uid` is refused, as is one naming a
  different `resourceVersion`. Refused rather than silently corrected, because
  a body that disagrees with its own target is a caller bug worth surfacing.
- **Names are validated as DNS-1123 subdomains.** Request paths are composed
  from the namespace and name, so a value containing `/` or `..` could otherwise
  produce a path that is not the object it claims to be — and, since the scope is
  composed from the same values, could make an out-of-scope request match its own
  scope.

## What execution deliberately does not do

- **It does not block.** Convergence is watched for a bounded window and then
  reported. A timeout is a *verdict*, not a failure, and never triggers another
  request — a monitoring loop with other clusters to look at must not be held by
  one slow rollout.
- **It does not thrash.** A failure stops the action; it does not re-drive it.
  The only retryable response is one the API server rejected outright, because a
  mutation whose outcome is unknown must never be repeated.
- **It does not roll back on its own.** A rollback is itself a mutating action,
  and MaKlaude taking one unbidden — because it did not like what it saw in the
  observation window — would be exactly the unapproved autonomy this milestone
  exists to prevent.

All three are asserted under a fault that lands *while the action is in flight*,
rather than only against a still cluster:
[When a fault lands during a remediation](chaos.md#when-a-fault-lands-during-a-remediation).

## Undoing an action

`Runner.Execute` captures the target's pre-state and reports that a rollback is
*available*. `Runner.Rollback` performs one only when a caller asks, holding the
same permission slip that authorized the action being undone — a rollback is not
a second, unreviewed grant of authority.

If the target is already back at its pre-action state, nothing is sent, and the
trail records exactly that rather than a redundant write.

## The audit trail

Every lifecycle event appends one immutable record: `proposed`, `approved`,
`executed`, `verified`, `failed`, `rolledback`. Three properties are worth knowing
before you have to read one during an incident:

- **Each record stands alone.** It carries the proposal identity, the cluster, the
  operation, the target, *and* the approver — so a reader who finds one record
  quoted out of context can still answer "which action is this, and who allowed
  it". A trail whose entries only make sense in sequence becomes unreadable the
  moment one is lost or truncated.
- **Order is a fact, not a race.** `Trail.Append` stamps a monotonically
  increasing sequence number under a lock. Wall-clock timestamps are recorded too,
  but ordering is defined by the sequence — two records written in the same
  nanosecond, or either side of a clock adjustment, still have a defined order.
  Recorded time is also kept distinct from event time: when a human approved,
  when the gate honored it, when the proposal was computed, and when the attempt
  started and finished are five separate fields.
- **Free text is redacted before it is stored.** A record is rendered into a
  GitHub issue, which on a public repository is world-readable, and its free text
  is cluster-derived — an API server error, a container's own message. Structured
  identifiers are deliberately *not* redacted: over-redacting the proposal
  identity, the target, the operation, or the approver's login would destroy the
  linkage the trail exists to provide.

The in-memory `Trail` is not the durable record. The durable one is the comms
artifact — the approval issue, where each phase is rendered as a comment that
outlives the process by design. A restart loses the `Trail` and loses nothing that
matters. Making it durable elsewhere (a file, a database, a log sink) means
writing another `Sink`, which is why the execution layer depends on the interface
rather than on the type.

## Verifying the posture yourself

```bash
# The whole write path, unit-tested, no cluster needed:
task test

# End to end against a live kind cluster — seeds a wedged Deployment, drives
# propose → approve → execute → verify, and asserts against the apiserver's own
# audit log that EXACTLY ONE executor request landed, on the approved object,
# and that nothing the observation identity sent was accepted:
task e2e
```

The e2e also runs `TestE2E_ObservationIdentityCannotExecute`, which aims a dry-run
patch at a Deployment from the observation identity precisely so RBAC can refuse
it. That refusal appearing in the apiserver audit log is the guarantee working;
see [`no-writes.md`](no-writes.md#layer-4--audit-log-corroboration) for why the
assertion is about what was *accepted* rather than about what was tried.

## Related

- [`no-writes.md`](no-writes.md) — the four-layer proof that the **observation**
  path still cannot write, unchanged by any of the above.
- [`rbac.md`](rbac.md#the-optional-minimal-write-bundle) — the executor identity:
  what it grants, what it withholds, and the `auth can-i` matrix.
- [`autonomous-mode.md`](autonomous-mode.md) — the approval bypass, and the exact
  list of what it does and does not waive.
- [README: Approval gate & autonomous mode](../README.md#approval-gate--autonomous-mode)
  — the proposal artifact and the label-event decision signal.
