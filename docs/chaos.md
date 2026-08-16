# MaKlaude's chaos write path

Everything that has proved MaKlaude's remediation works so far has relied on
hand-seeded fixtures. Those prove the detectors read a broken cluster correctly.
They prove nothing about what happens when a cluster breaks *while MaKlaude is
working*, which is the only condition it will actually meet in production.

Milestone 6 closes that gap by letting MaKlaude break its own managed clusters on
purpose, through [Chaos Mesh](https://chaos-mesh.org/), on clusters a human has
explicitly offered up for it. This page documents the write path itself — the
identity, the client, the catalog, and the precondition a create needs. It is the
counterpart to [`no-writes.md`](no-writes.md): what MaKlaude can now do on purpose,
and every gate it has to pass first.

> **Nothing here is reachable by default, and nothing in this milestone marks a
> real cluster eligible.** M6 lands and proves itself on `kind`. The registry
> opt-in ships in full anyway, because the door has to be built properly before
> anything walks through it.

## Three independent gates

All three have to be open, and they are opened by a person in three different
places. Closing any one closes the path.

| # | Gate | Where | How to close it |
| - | ---- | ----- | --------------- |
| 1 | RBAC | [`deploy/rbac/chaos/`](../deploy/rbac/chaos/) — a third ServiceAccount, `maklaude-chaos`, and a namespaced Role | `kubectl delete -k deploy/rbac/chaos` (no MaKlaude restart) |
| 2 | Per-cluster eligibility | the cluster config's `chaos:` block, verified by [`internal/cluster/chaos.go`](../internal/cluster/chaos.go) | delete the block |
| 3 | The write-path kill switch | `kube.ExecuteMode`, shared with approved remediation | set it to disabled (the zero value) |

Gate 2 is the one that is specific to chaos, and it is a *type*, not a boolean.
`cluster.ChaosTarget` is an interface sealed by an unexported method, so only
`internal/cluster` can mint one and only for a cluster whose marker verified.
`chaos.NewInjector` takes a `ChaosTarget`, which means an ineligible cluster is a
**missing argument** rather than a check somebody has to remember:

```go
// There is no way to write this for an ineligible cluster. The compiler refuses it.
injector, err := chaos.NewInjector(target, kube.ExecuteEnabled)
```

### Marking a cluster eligible

```yaml
clusters:
  - name: kind-maklaude
    kubeconfig: ~/.kube/config
    context: kind-maklaude
    chaos:
      cluster: kind-maklaude
      acknowledgement: >-
        I accept that MaKlaude may deliberately break the cluster named
        kind-maklaude
```

Both fields are required and both name the cluster the block sits under. That
redundancy *is* the mechanism: the realistic accident isn't a typo, it's a
copy-paste — an operator marks a scratch cluster, then copies the block when adding
production. A bare `chaos: true` survives that copy silently; this doesn't. Pasted
under a different cluster it names the wrong one, the config is rejected, and a
directly-constructed handle still mints no token.

Absence is ineligibility, which is every config written before this milestone.

## The fault catalog

Closed, and currently one Chaos Mesh kind. Adding one is an entry in
`kindResource` plus its actions in `actionKinds`, a grant in the Role, and a test
against the real CRD. It is deliberately not a matter of passing a string through.

| Kind | Action | Effect | Duration |
| ---- | ------ | ------ | -------- |
| `PodChaos` | `pod-kill` | Deletes a matched pod; its controller recreates it. The shape a node failure or an OOM kill produces. | **Must not be set** — one-shot |
| `PodChaos` | `pod-failure` | Makes a matched pod unavailable, reverted when the duration elapses. | **Required**, ≤ 10 minutes |

The duration rule is not cosmetic. Chaos Mesh ignores `spec.duration` for a
one-shot action, so a CR carrying `duration: 30s` beside `action: pod-kill` would
say something the controller won't do — and a human reading that CR mid-incident
would reasonably believe the fault self-reverts in 30 seconds. So a duration is
required where it's honoured and refused where it's ignored, rather than passed
through and quietly dropped.

The ceiling exists because a duration is the only bound that survives MaKlaude
dying: if the process is killed between injecting and tearing down, an expiring
fault reverts itself and a long one doesn't. It is **not** the teardown guarantee —
one-shot actions have no duration at all, and the CR object outlives the fault in
every case. See [How a fault ends without MaKlaude](#how-a-fault-ends-without-maklaude).

The ceiling has a second job that's easy to miss: it's the floor of the reaper's
orphan grace. Raising it widens the reaper's blind window by the same amount.

`mode` and `value` come straight from Chaos Mesh (`one`, `all`, `fixed`,
`fixed-percent`, `random-max-percent`), validated as a closed set with the value
required for exactly the modes that take one. Whether a given mode is *acceptable*
is a blast-radius question, which belongs to M5's budget, cooldown and breaker once
chaos runs through the decision path
([T4](https://github.com/Sayfan-AI/MaKlaude/issues/193)) — not to this validation.

The selector must name at least one target namespace, always. Chaos Mesh defaults
an empty selector to the namespace of the CR itself, so an omitted list means
"somewhere" to a person reading the object and "here" to the controller — and since
the CR lives in MaKlaude's own chaos namespace while the target is somewhere else
entirely, those two readings differ by the whole point of the experiment.

## What a create is conditioned on

Every other mutating call in MaKlaude carries the target's `resourceVersion`, and
there is deliberately no unconditional variant (`kube.ErrMissingPrecondition`). A
create can't: it's a `POST` of an object that doesn't exist yet, so optimistic
concurrency has nothing to be optimistic about. The guard has to come from
somewhere else, and before the object exists the only thing available is its name.

So the name is **derived** from everything that defines the experiment — action, CR
namespace, mode, value, duration, target namespaces, label selectors — as
`maklaude-podchaos-<digest>`:

- **Derived means a replay is the same request.** Two calls asking for the same
  fault, in the same place, at the same size, for the same duration produce the same
  name, so the second one collides instead of injecting a second copy of a fault
  MaKlaude already has running. Retrying is safe; duplicating is impossible.
- **Never server-generated.** Chaos Mesh's `generateName` would defeat exactly
  that: every retry would succeed with a fresh name, and a network timeout on a
  request the API server actually accepted — the case a retry exists for — would
  leave two live experiments and a caller holding one name.
- **Never caller-supplied.** Then the collision property would depend on every call
  site choosing names the same way, which is a convention. This is a function.

The collision is enforced twice, and the two aren't redundant:

1. A **read first**, through a client built with the zero-value scope, so the
   pre-flight check for a write provably cannot write. A live experiment found here
   fails with `chaos.ErrExperimentExists` and nothing is sent. This is the
   diagnosis: it names the object in the way.
2. The **API server's own uniqueness check**. The read above is a
   time-of-check/time-of-use race by construction, so it can't be the guarantee. A
   `POST` of an existing name returns 409 `AlreadyExists`, mapped to the same
   sentinel. A replay collides whoever wins the race.

An existing object is never **adopted**. An injector that treated "already there"
as success would report a fault it didn't create, with a duration and a selector it
didn't choose.

## What a teardown is conditioned on

The object's **UID**, not its `resourceVersion`. A resourceVersion asks "has this
object changed since I reasoned about it?", which is right for a patch and wrong
here: a live experiment's status is updated by its controller constantly, so an RV
precondition would make teardown fail precisely because the experiment is doing its
job. The question teardown actually has is "is this the same object I created?" — a
name can be recycled, and deleting a stranger's experiment because it inherited a
name would be a write nobody authorised. That question is UID, and a mismatch comes
back as `kube.ErrPreconditionConflict`.

Two outcomes that look like failures and aren't:

- **Already absent.** Teardown's goal is that no MaKlaude experiment outlives its
  run, and an object already gone satisfies it. It's reported as success *with*
  `Removal.AlreadyAbsent` set rather than smoothed into an ordinary success, because
  "torn down" and "was never there" are different facts about a cluster. The failure
  teardown must never produce is the opposite one: reporting success while the CR is
  live.
- **A conflict.** That's the recycled-name case, and it means MaKlaude's experiment
  is already gone.

The delete overrides neither propagation policy nor grace period, so Chaos Mesh's
finalizer runs and a persisting fault is reverted before the object disappears.
Forcing the object away would remove the record while leaving the fault.

## How a fault ends without MaKlaude

A leaked chaos experiment on your cluster is the worst outcome this milestone can
produce, and "the deferred cleanup runs" doesn't achieve the opposite. A `defer` is
code in a process: it doesn't run on `SIGKILL`, on an OOM kill, on a panic in another
goroutine, or on a CI runner that vanishes. Each of those leaves a CR on the cluster
that MaKlaude is no longer around to delete.

So there are **two mechanisms, and neither subsumes the other.**

**1. The fault self-limits, and Chaos Mesh is what enforces it.** Every action in the
catalog declares how its fault ends with MaKlaude absent, and that declaration is a
field rather than a convention — the zero value belongs to no action, so a new action
added without one fails the build (`TestEveryActionDeclaresASelfLimit`). There are two
mechanisms today:

| Action | How the fault ends | What that means if MaKlaude is killed |
| ------ | ------------------ | ------------------------------------- |
| `pod-failure` | Chaos Mesh's controller reverts it when `spec.duration` elapses | The fault ends on schedule. The enforcing party is on the cluster, not in this program. |
| `pod-kill` | The fault is a single event — the pod is killed once, its controller recreates it | It's already over. There's nothing to revert, which is why a duration is *refused* here rather than required. |

Refusing an out-of-bounds duration happens at validation, before a request is
composed, so an experiment whose fault would outlive its bound never reaches a
cluster. There's no clamping: silently shortening a 30-minute request to 10 would
inject a different experiment than the one you asked for and than the one the record
describes.

**2. The reaper removes what's left.** Every action leaves the CR object behind after
its fault is over — one-shot actions immediately, duration-bounded ones on expiry —
and the object is what a next cycle collects. `chaos.Reaper` lists MaKlaude's
experiments and deletes the ones old enough to be residue, each through the same
UID-conditioned `Injector.Remove` as a deliberate teardown.

That means: **the fault is over within 10 minutes no matter what happens to MaKlaude,
and the record of it is gone on the next sweep.**

**A teardown that succeeds has been *accepted*, not completed**, and the difference is
load-bearing rather than pedantic. Chaos Mesh holds a finalizer on every experiment and
clears it only once its controller has recovered the fault, so the object sits in
`Terminating` in between: a second `Remove` in that window reports success with
`AlreadyAbsent` **false** (the object really is still there), and re-injecting the same
experiment fails with `ErrExperimentExists` until the name is free. `AlreadyAbsent` is
therefore the receipt for *recovery finished*, not merely for *object deleted*. MaKlaude
never forces the object away — `--force`-style deletion would drop the record while
leaving the pods broken. The upside is the guarantee itself: because recovery belongs to
Chaos Mesh, a MaKlaude that is `SIGKILL`ed one instant after the delete is accepted still
leaves a cluster that un-breaks itself.

`internal/chaos` proves the first half by killing a real process. A child process
injects a persisting fault, gets `SIGKILL`ed the instant the create lands, and the test
then asserts that no teardown request ever reached the API server and that the object
left behind carries a bounded `spec.duration`. A test that faked the death by returning
early would prove the wrong thing — a returning function still runs its defers.

### What stops the reaper deleting *your* experiments

A reaper is, mechanically, a bulk delete of other people's objects waiting for a bug.
Chaos Mesh is a shared installation: your own experiment sits in the same namespace and
looks broadly like MaKlaude's. Deleting one of those is worse than the leak the reaper
prevents.

Ownership needs **three independent signals, all of them**:

1. **The labels** `app.kubernetes.io/managed-by=maklaude` and
   `app.kubernetes.io/component=chaos` — re-checked in-process, not merely passed to
   the server as a label selector. A selector is a filter, and a filter that's ignored
   (by a proxy, a server-side bug, a caller that changes it) returns *more* than was
   asked for; code that trusts it deletes whatever came back.
2. **The `chaos.maklaude.dev/cluster` annotation**, which must name this cluster.
   Labels get copied when someone clones a manifest to try it themselves; the
   annotation names the cluster the experiment was authorised for.
3. **The name shape.** MaKlaude's object names are derived — `maklaude-<kind>-<digest>`
   — so no hand-written or server-generated name can match. This is the signal you
   can't reproduce by copying metadata, which is what makes it worth having alongside
   the other two.

And an **age grace**, which is the part that replaces a knob with an argument. The
obvious design for "don't sweep what's in use" is a list of live experiment names
passed in by the caller — a convention that works until one call site forgets, and
which could never know about a *concurrently running* MaKlaude's experiments anyway.
Instead: no fault MaKlaude asks for can outlive the 10-minute ceiling, so an owned
object older than that can't belong to a live experiment under any MaKlaude, anywhere.
A grace at or above that ceiling is therefore structurally unable to reach a running
fault. `NewReaper` **refuses** a shorter one rather than clamping it, which is also
what rejects the value that matters most: `0` is what a forgotten field gets, and "reap
everything owned, however young" is both plausible-looking and destructive.

Two more properties worth knowing:

- **One failed delete doesn't abort a sweep.** Five leaked experiments where the first
  delete is denied is exactly when the other four matter most. Failures are collected
  on the returned `Sweep`, which is non-nil even alongside an error — a partial sweep's
  record of what it *did* remove is what you need when part of it failed.
- **A sweep that can't enumerate the cluster is an error, not an all-clear.**
  `ErrReapFailed` exists so "I couldn't look" never reads as "nothing leaked".

The RBAC follows the code rather than anticipating it: `list` on `podchaos` arrived
with the reaper and not before. It brought neither `watch` (the reaper polls on a
schedule; nothing here reconciles) nor `deletecollection` — which is what a sweep would
be if written the obvious way, and is precisely what a shared Chaos Mesh makes unsafe.

## Installing Chaos Mesh on `kind`

MaKlaude doesn't install Chaos Mesh on your clusters — see
[What the RBAC does *not* bound](#what-the-rbac-does-not-bound). For a local `kind`
cluster used for development and testing, one script does it reproducibly:

```bash
task chaos:install     # pinned chart, kind's containerd socket, waits until serving
task chaos:uninstall   # removes the release; deliberately leaves the CRDs
```

The CI `chaos on kind` job calls the same `scripts/install-chaos-mesh.sh`, so a failure
you reproduce locally runs the code CI ran, and the pinned version has exactly one home
in the repo. Three details it handles that are easy to get wrong by hand:

- **The container runtime.** The chart defaults to Docker's socket. A `kind` node runs
  containerd, and pointing `chaos-daemon` at the wrong socket doesn't fail loudly — the
  daemon comes up, the controller accepts a `PodChaos`, and the fault never lands. An
  experiment that reports injected while nothing broke is the exact failure the closed
  action catalog exists to prevent on the CR side.
- **Readiness is not the same as serving.** `helm --wait` returns when the Deployments
  report ready; the script additionally waits for the `podchaos` CRD to be
  `Established` and probes until the admission webhook answers. Without that, the first
  create races the install and reads as a flaky test.
- **It refuses any cluster that doesn't look like `kind`.** This installs a component
  that can kill pods cluster-wide, and a mistyped context is the whole risk. Chaos Mesh
  on a cluster you care about is your decision to make with your own tooling.

Uninstall leaves the CRDs in place on purpose: removing a CRD cascades to every custom
resource of that kind, including experiments MaKlaude didn't create.

## The guard is the executor's guard

The chaos path gets its own package and identity. It does **not** get its own
transport guard. Every write goes through `kube.WriteScope` — the same
whole-request pin approved remediation uses: one method, one exact path, optionally
forced `dryRun=All` — entered through one door:

```go
func kube.ChaosRestConfig(target cluster.ChaosTarget, scope WriteScope) (*rest.Config, error)
```

That signature carries both narrowings. It needs a capability token no ineligible
cluster can mint, and it refuses a mutating scope whose path isn't a
`chaos-mesh.org` CR path — so "no mutating verb except chaos CRDs, on chaos-eligible
clusters" is enforced at the door rather than asserted about the callers behind it.
Eligibility is permission to break a cluster with experiments; it is *not*
permission to patch its deployments or delete its pods, and those fail with
`kube.ErrNotChaosScope` even on an eligible cluster.

The client is dynamic rather than typed for a plain reason: `kube.Executor`'s every
method funnels through a `kubernetes.Interface`, and the typed clientset has no
notion of a `chaos-mesh.org` resource. Pulling Chaos Mesh's own API module in would
drag `controller-runtime` along with it. A dynamic client speaks unstructured
objects over the same `rest.Config`, and therefore through the same guard.

The CR body is composed from validated fields, never accepted from a caller — which
is why there is no `InjectRaw`. A create's target name travels in the **body** (the
scope can only pin the collection path, since that's where a `POST` goes), so a
caller-supplied object would be a way to name an object the scope never approved.
Building the body is part of the guard, not a convenience.

## Which namespaces MaKlaude can break, and how that is decided

The `maklaude-chaos` Role in
[`deploy/rbac/chaos/role.yaml`](../deploy/rbac/chaos/role.yaml) covers the namespace
experiment **objects** live in. It says nothing about where a fault lands, and read
alone it invites the conclusion that nothing does: MaKlaude writes a CR, Chaos Mesh's
controller does the killing with its own substantial privileges, and none of
MaKlaude's permissions are consulted for the killing itself.

Something does, and it is the file most likely to be missed because it is
deliberately *not* in the bundle:
[`target-namespace-role.yaml`](../deploy/rbac/chaos/target-namespace-role.yaml),
applied once per namespace you are willing to have broken.

```bash
kubectl apply -f deploy/rbac/chaos/target-namespace-role.yaml        -n <target-ns>
kubectl apply -f deploy/rbac/chaos/target-namespace-rolebinding.yaml -n <target-ns>
```

### Why that file exists — Chaos Mesh's contract, measured

Chaos Mesh installs with permission validation **on by default**. Its `vauth.kb.io`
validating webhook authorizes a create by asking the API server, once per namespace
the experiment's **selector** names, whether the *requester* may create this chaos
kind there:

```yaml
# One SubjectAccessReview per target namespace, with exactly these attributes.
verb: create
group: chaos-mesh.org
resource: podchaos      # strings.ToLower(kind)
namespace: <each namespace in the selector>
```

That's read out of
[`pkg/webhook/validate_auth.go`](https://github.com/chaos-mesh/chaos-mesh/blob/v2.8.3/pkg/webhook/validate_auth.go)
at the chart version [`scripts/install-chaos-mesh.sh`](../scripts/install-chaos-mesh.sh)
pins, not inferred from behaviour. Three consequences, all load-bearing:

- **It checks `chaos-mesh.org` permissions, not pod permissions.** An earlier version
  of this document called that a genuine tension — the injector must not touch pods,
  and permission validation appeared to require that it could. It doesn't, and the
  resolution costs the chaos identity **no** grant on any workload. `create podchaos`
  in the target namespace is the entire requirement.
- **So the reachable namespaces are an allowlist a person writes.** A `PodChaos` in
  `maklaude-chaos` whose selector names `kube-system` is *denied* unless somebody ran
  the apply above for `kube-system`. The denial reads
  `system:serviceaccount:maklaude:maklaude-chaos is forbidden on namespace kube-system`,
  and it is the bound working rather than a bug.
- **A cluster-scoped experiment is impossible.** A selector naming no namespace makes
  the same webhook demand cluster-wide `create podchaos`, which no `Role` can confer.
  `internal/chaos` also refuses to compose such a selector. Two independent layers,
  the same answer.

The toggle is `dashboard.securityMode` (default `true`) — misleadingly named, since
the chart feeds it to the *controller* as `SECURITY_MODE` and disabling the dashboard
does not disable the check. The install script sets it explicitly so this bound keeps
existing if the upstream default ever flips.

### What the target grant is, and what it deliberately is not

One verb — `create podchaos.chaos-mesh.org` — which is exactly the review above.
It confers no read of the namespace, no access to a pod, and no `list`/`delete` on
experiments there. That last absence needs its reason stated, because the verb it
*does* grant unavoidably also permits writing an experiment **object** into the
target namespace: the webhook checks the same verb a write uses, so RBAC cannot tell
"may aim here" from "may write here".

An object outside `maklaude-chaos` is unreachable by every teardown path that exists
— `Reaper.Reap` sweeps one namespace, and the chaos Role grants `list` and `delete`
in that one alone — so it would be precisely the outlives-the-process leak this
milestone is about. `internal/chaos` therefore refuses any experiment whose CR
namespace appears in its own selector (`Experiment.placementProblems`). The two
mechanisms are meant to be read together:

| Mechanism | Namespaces a create could land in |
|---|---|
| RBAC alone | `maklaude-chaos` ∪ the granted target namespaces |
| The validator subtracts | the granted target namespaces |
| What remains | `maklaude-chaos` — the one namespace the reaper sweeps |

### The other bounds, which this is not a substitute for

- the selector MaKlaude writes, validated in `internal/chaos` and always naming its
  target namespaces explicitly;
- Chaos Mesh's own installation scope — how widely you let *it* reach;
- M5's blast-radius budget, cooldown and circuit breaker, once chaos proposals run
  through the decision path (T4).

The chaos Role is what stops MaKlaude reaching *past* Chaos Mesh. The target-namespace
Role is what stops it pointing Chaos Mesh wherever it likes. Neither survives an
operator turning permission validation off, which is why the posture is pinned rather
than inherited.

MaKlaude also does not install Chaos Mesh. It writes the custom resources;
installing the controller and choosing its scope is yours.

## Verifying it yourself

```bash
# Manifest-level, seconds, no cluster: the Role grants exactly the four calls
# internal/chaos makes, it's namespaced rather than cluster-wide, and no other
# identity holds it.
go test ./test/rbac/

# In-process: the scope door, the derived name, the create/teardown preconditions,
# every action's declared self-limit, and the reaper's ownership test. Includes the
# SIGKILL test — it spawns a child process, injects a real fault against a stub
# apiserver, kills it, and asserts what a dead run leaves behind.
go test ./internal/chaos/ ./internal/kube/

# Against a live kind cluster with Chaos Mesh: create -> observe -> terminate, plus a
# sweep that must collect MaKlaude's leftover object and leave the operator's alone.
task chaos:install
kubectl apply -f test/e2e/manifests/chaos-target.yaml
kubectl apply -f test/e2e/manifests/chaos-foreign.yaml

# The per-namespace capability. Skip it and every experiment below is denied by
# admission — "forbidden on namespace maklaude-chaos-target" — which is the bound
# described above doing its job, not a broken test.
kubectl apply -f deploy/rbac/chaos/target-namespace-role.yaml        -n maklaude-chaos-target
kubectl apply -f deploy/rbac/chaos/target-namespace-rolebinding.yaml -n maklaude-chaos-target

task e2e:chaos   # see the target's desc for the kubeconfig env vars it needs
```

Against a live cluster where you applied `deploy/rbac/chaos`, the same questions via
`SubjectAccessReview` — which needs no Chaos Mesh installed, because it's evaluated
against the RBAC rules alone:

```bash
CHAOS=system:serviceaccount:maklaude:maklaude-chaos

# The catalog — each MUST print "true".
for verb in get list create delete; do
  kubectl create -o jsonpath='{.status.allowed}{"\n"}' -f - <<EOF
apiVersion: authorization.k8s.io/v1
kind: SubjectAccessReview
spec:
  user: "$CHAOS"
  resourceAttributes:
    verb: "$verb"
    group: chaos-mesh.org
    resource: podchaos
    namespace: maklaude-chaos
EOF
done

# The workload it breaks — MUST print "false" (no namespace = any namespace).
kubectl create -o jsonpath='{.status.allowed}{"\n"}' -f - <<EOF
apiVersion: authorization.k8s.io/v1
kind: SubjectAccessReview
spec:
  user: "$CHAOS"
  resourceAttributes:
    verb: delete
    resource: pods
EOF
```

Two CI jobs run the matrix, and the split is deliberate. `e2e on kind` asserts the
grants through `SubjectAccessReview` on a cluster with **no** Chaos Mesh, because that
proves the Role is evaluated against RBAC rules alone. `chaos on kind` re-asserts them
with `kubectl auth can-i` against the real CRD — the complementary question, since
`can-i` resolves the resource name through discovery — and then runs the lifecycle. Both
cover every workload denial, the other-namespace denials, the neighbouring chaos kinds,
and a re-check that the observation and executor identities gained nothing.

The target-namespace capability is checked as a **before/after pair**, and the ordering
is the point. `chaos on kind` asserts `create podchaos` in `maklaude-chaos-target` is
DENIED before the grant is applied and ALLOWED after, so the permission is demonstrably
caused by that apply rather than by a cluster that permits everything. It then asserts
the grant conferred nothing else there — no read of the namespace, no verb on a pod, no
`list`/`delete` on experiments — and that `default` and `kube-system` still refuse,
which is what makes it an allowlist rather than a switch. `TestE2E_ChaosTargetRoleIsTheAllowlist`
closes the loop from MaKlaude's side: a real experiment aimed at `default`, in dry-run,
must be refused *by admission* — the test rejects MaKlaude's own local refusals
explicitly, since an error from the wrong layer is no evidence about the allowlist.

`chaos on kind` also asserts, on the way out and with `always()` so a *failed* run is
covered too, that no MaKlaude-derived experiment object outlived the job and that the
operator's own experiment is still there. A leak is most likely precisely when a test
failed partway through, which is the case a passing-run-only check would never see.

## What isn't built yet

This is T3 of nine. The write path and its teardown guarantee exist and are unreachable
from any config surface, which is the same posture `kube.ExecuteMode` shipped in at M4:
the strongest form of "off" there is. Nothing schedules a sweep yet either — `Reaper`
has no production caller until chaos becomes a proposal class in T4.

| Task | What it adds |
| ---- | ------------ |
| [T4](https://github.com/Sayfan-AI/MaKlaude/issues/193) | Chaos as a proposal class through M5's decision path: same budget, cooldown and breaker; injections **never** promote to unattended. Also where the reaper gets scheduled. |
| [T5](https://github.com/Sayfan-AI/MaKlaude/issues/194) | Fault injection *during* remediation — the condition none of the fixtures could create |
| [T6](https://github.com/Sayfan-AI/MaKlaude/issues/195) | Correctness scoring: did it fix the fault, and should it have been allowed |
| [T7](https://github.com/Sayfan-AI/MaKlaude/issues/196) | The narrowed no-writes guarantee, encoded in tests as precisely as in prose |
| [T8](https://github.com/Sayfan-AI/MaKlaude/issues/197) | The end-to-end chaos scenario on `kind` in CI |

Chaos proposals will never promote to unattended execution, and that's settled
rather than pending. Promotion's evidence means "this fix worked three times"; for
chaos the same evidence only means "the injection succeeded three times", which is a
statement about Chaos Mesh rather than about safety. An experiment's value is that
its outcome is unknown, and a track record of clean injections is not evidence the
next one is safe.
