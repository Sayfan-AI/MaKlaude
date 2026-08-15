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
every case. Guaranteed teardown is [T3](https://github.com/Sayfan-AI/MaKlaude/issues/192).

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

## What the RBAC does *not* bound

Read this before assuming the Role is the safety net.

**It does not bound the blast radius of an experiment.** MaKlaude writes a CR;
Chaos Mesh's own controller does the killing, with its own substantial privileges. A
`PodChaos` created in `maklaude-chaos` whose selector names `kube-system` affects
`kube-system`, and no permission of MaKlaude's is consulted for that.

What bounds the damage is:

- the selector MaKlaude writes, validated in `internal/chaos` and always naming its
  target namespaces explicitly;
- Chaos Mesh's own installation scope — how widely you let *it* reach;
- M5's blast-radius budget, cooldown and circuit breaker, once chaos proposals run
  through the decision path (T4).

The Role is what stops MaKlaude reaching *past* Chaos Mesh. It is not what stops
Chaos Mesh reaching far.

### An open deployment question

Chaos Mesh can be installed with permission validation, where its admission webhook
checks whether the *requester* may act on the objects a selector matches. Under that
posture the `maklaude-chaos` identity will be **refused**, because it deliberately
holds no permissions on pods.

That's a genuine tension, not an oversight: MaKlaude's design says the injector must
not be able to touch pods directly, and that feature says the injector must be able
to. Resolving it means granting the target-namespace pod permissions Chaos Mesh
checks, which widens this identity — an operator's decision, not a default. Which
posture the `kind` job runs under is pinned by
[T8](https://github.com/Sayfan-AI/MaKlaude/issues/197).

MaKlaude also does not install Chaos Mesh. It writes the custom resources;
installing the controller and choosing its scope is yours.

## Verifying it yourself

```bash
# Manifest-level, seconds, no cluster: the Role grants exactly the three calls
# internal/chaos makes, it's namespaced rather than cluster-wide, and no other
# identity holds it.
go test ./test/rbac/

# In-process: the scope door, the derived name, the create/teardown preconditions.
go test ./internal/chaos/ ./internal/kube/
```

Against a live cluster where you applied `deploy/rbac/chaos`, the same questions via
`SubjectAccessReview` — which needs no Chaos Mesh installed, because it's evaluated
against the RBAC rules alone:

```bash
CHAOS=system:serviceaccount:maklaude:maklaude-chaos

# The catalog — each MUST print "true".
for verb in get create delete; do
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

The `e2e` CI job runs the full matrix — the catalog, every workload denial, the
other-namespace denials, the neighbouring chaos kinds, and a re-check that the
observation and executor identities gained nothing.

## What isn't built yet

This is T2 of nine. The write path exists and is unreachable from any config
surface, which is the same posture `kube.ExecuteMode` shipped in at M4: the
strongest form of "off" there is.

| Task | What it adds |
| ---- | ------------ |
| [T3](https://github.com/Sayfan-AI/MaKlaude/issues/192) | Experiment lifecycle and guaranteed teardown — a leaked experiment is a bug, not an inconvenience |
| [T4](https://github.com/Sayfan-AI/MaKlaude/issues/193) | Chaos as a proposal class through M5's decision path: same budget, cooldown and breaker; injections **never** promote to unattended |
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
