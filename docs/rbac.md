# MaKlaude RBAC

MaKlaude observes the health of the clusters under its care and, by default,
cannot mutate them at all. This document describes the least-privilege RBAC that
grants it exactly the access it needs, how an operator installs it, how to wire
the resulting identity into MaKlaude, and how to verify each grant.

There are **two bundles and two identities**, and the separation between them is
the point:

| Bundle | Identity | Grants | Installed by |
| ------ | -------- | ------ | ------------ |
| [`deploy/rbac/`](../deploy/rbac/) | `maklaude` | `get`/`list`/`watch` on a fixed resource set — **no mutating verb anywhere** | `kubectl apply -k deploy/rbac` |
| [`deploy/rbac/write/`](../deploy/rbac/write/) — optional | `maklaude-executor` | the same reads, **plus** exactly three mutating verbs | `kubectl apply -k deploy/rbac/write` |

Install only the first and MaKlaude provably cannot change anything: everything
that watches, collects, detects, correlates, diagnoses, and proposes runs as
`maklaude`. The write bundle is a *delta* bound to a *different* account, so
installing it grants the observation identity nothing — which is asserted in CI
against a live cluster, not just claimed here.

Most of this document is about the read-only bundle. The write bundle has its own
section: [The optional minimal-write bundle](#the-optional-minimal-write-bundle).

## The access model

MaKlaude authenticates to each cluster as a single ServiceAccount
(`maklaude` in the `maklaude` namespace) whose only permissions come from one
ClusterRole (`maklaude-readonly`). That role grants the **read triad**
(`get`, `list`, `watch`) on a small, fixed set of resources — and nothing else.

| API group | Resources | Verbs | Why MaKlaude reads it |
| --------- | --------- | ----- | --------------------- |
| `""` (core/v1) | `nodes` | get, list, watch | Node Ready / memory / disk / PID pressure & schedulability; allocatable cpu/memory |
| `""` (core/v1) | `pods` | get, list, watch | Pod phase, restart counts, CrashLoopBackOff detection, node assignment, ownerReferences, per-container waiting/termination facts, resource requests; single-pod `get` before fetching that pod's logs |
| `""` (core/v1) | `pods/log` | get | Recent, bounded container logs — fetched **lazily**, only for pods already implicated in a finding (never cluster-wide) |
| `""` (core/v1) | `events` | get, list, watch | Recent Warning events (scheduling/image/probe failures) |
| `""` (core/v1) | `namespaces` | get, list, watch | Namespace enumeration exposed by the read-only client |
| `apps` (apps/v1) | `deployments` | get, list, watch | Desired / ready / available / updated replica counts |
| `apps` (apps/v1) | `replicasets` | get, list, watch | Desired / ready / available replica counts |

Every rule above maps directly to a call site in the codebase
(`internal/health/collector.go` and `internal/kube/client.go`). The collector
lists each resource across **all** namespaces every cycle, which is why a
cluster-scoped `ClusterRole` (rather than a namespaced `Role`) is required.

### The read-only guarantee

The ClusterRole contains **no mutating verbs**. There is no `create`, `update`,
`patch`, `delete`, or `deletecollection` anywhere in it — only `get`, `list`,
and `watch` (and a `get` on the `pods/log` subresource, which is itself
read-only: logs can only be fetched, never written). It also grants **no access
to `secrets` or `configmaps`**.

Pod logs are read **lazily and bounded**: MaKlaude only fetches a container's
recent logs (default ~50 tailed lines, plus previous-instance logs for a
crashlooping container) for a pod already implicated in a finding — never
cluster-wide and never during the eager health scan. This is why `pods/log`
carries only `get`, not `list`/`watch`.

`watch` is granted even though the current collector re-`list`s each cycle. It is
included so MaKlaude can later move to efficient informer/watch-based collection
without an RBAC change. `watch` is itself a read-only verb, so this does not
widen MaKlaude's blast radius.

This RBAC scope is MaKlaude's outermost safety boundary, and it is reinforced in
depth by the code: the `internal/kube` client exposes only read methods, never
hands out the underlying clientset, and wraps its HTTP transport in a guard that
rejects any non-GET/HEAD/OPTIONS request before it reaches the network. RBAC,
the client surface, and the transport guard are three independent layers that all
have to fail before MaKlaude could mutate a cluster.

### Discovery / reachability

MaKlaude's health probe (`kube.Client.CheckReachable`) calls the discovery
client, which hits the non-resource URLs `/version` and `/api*`. The bundle does
**not** grant these explicitly: Kubernetes ships a built-in `system:discovery`
ClusterRole bound to the `system:authenticated` group (every authenticated
identity, including this ServiceAccount), so discovery already works. Granting
`nonResourceURLs` here would be redundant, so the role stays resources-only.

## Granting MaKlaude read-only access

### 1. Apply the bundle

```bash
kubectl apply -k deploy/rbac
```

This creates the `maklaude` namespace, the `maklaude` ServiceAccount, the
`maklaude-readonly` ClusterRole, and the ClusterRoleBinding that ties them
together. (If your environment provisions namespaces out-of-band, you can remove
`namespace.yaml` from `deploy/rbac/kustomization.yaml` and create the namespace
yourself.)

### 2. Mint a kubeconfig for the ServiceAccount

MaKlaude's cluster registry references each cluster by a **kubeconfig file path**
and a **context name** — never inline credentials (see
[`config.example.yaml`](../config.example.yaml) and the "Cluster configuration"
section of the [README](../README.md)). So the operator's job is to produce a
kubeconfig that authenticates as the `maklaude` ServiceAccount.

On Kubernetes 1.24+ ServiceAccounts no longer auto-create a token Secret; mint a
short-lived token with `kubectl create token` (rotate it before it expires) or,
for a long-lived token, create a bound token Secret. The example below uses a
request-scoped token:

```bash
# Cluster API server URL and CA, taken from your current admin kubeconfig.
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl config view --minify --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > /tmp/maklaude-ca.crt

# A token for the maklaude ServiceAccount (set --duration as your policy allows).
TOKEN=$(kubectl -n maklaude create token maklaude --duration=8760h)

# Assemble a standalone kubeconfig for MaKlaude.
KCFG=$HOME/.kube/maklaude-<cluster-name>.yaml
kubectl --kubeconfig "$KCFG" config set-cluster <cluster-name> \
  --server="$SERVER" --certificate-authority=/tmp/maklaude-ca.crt --embed-certs=true
kubectl --kubeconfig "$KCFG" config set-credentials maklaude --token="$TOKEN"
kubectl --kubeconfig "$KCFG" config set-context <cluster-name> \
  --cluster=<cluster-name> --user=maklaude
kubectl --kubeconfig "$KCFG" config use-context <cluster-name>
```

> Treat this kubeconfig as a credential: keep it off version control and protect
> the file (e.g. `chmod 600`). MaKlaude's config file only stores the *path* to
> it, never its contents.

### 3. Register the cluster with MaKlaude

Point MaKlaude's config at the kubeconfig you just produced. Each entry needs a
unique `name`, the `kubeconfig` path, and the `context` to select within it:

```yaml
clusters:
  - name: prod-us-east
    kubeconfig: /home/alice/.kube/maklaude-prod-us-east.yaml
    context: prod-us-east
```

Repeat steps 1–3 per cluster. Each cluster gets its own kubeconfig and its own
registry entry; MaKlaude keeps clusters fully isolated.

## Verifying the access is read-only

Use `kubectl auth can-i`, impersonating the ServiceAccount, to confirm the
identity can read what it needs and cannot mutate anything. Run these against the
cluster where you applied the bundle:

```bash
SA=system:serviceaccount:maklaude:maklaude

# Full grant listing — should show only get/list/watch on the resources above.
kubectl auth can-i --list --as="$SA"

# Reads MaKlaude relies on — each should print "yes".
kubectl auth can-i list pods        --as="$SA" -A
kubectl auth can-i get  pods/log    --as="$SA" -A
kubectl auth can-i list nodes       --as="$SA"
kubectl auth can-i list events      --as="$SA" -A
kubectl auth can-i list deployments.apps --as="$SA" -A
kubectl auth can-i list replicasets.apps --as="$SA" -A

# Mutations and sensitive reads — each MUST print "no".
kubectl auth can-i create pods      --as="$SA"
kubectl auth can-i delete pods      --as="$SA"
kubectl auth can-i patch deployments.apps --as="$SA"
kubectl auth can-i get secrets      --as="$SA" -A
kubectl auth can-i get configmaps   --as="$SA" -A
```

If any mutating check returns `yes`, the bundle has been modified — revert to the
manifests in `deploy/rbac/`, which grant only `get`/`list`/`watch`.

## The optional minimal-write bundle

Everything above describes an identity that cannot change a cluster. Remediation
needs one that can, for a very small number of actions, and
[`deploy/rbac/write/`](../deploy/rbac/write/) is it.

It is a separate bundle rather than extra rules on `maklaude-readonly`, and it
binds a separate ServiceAccount (`maklaude-executor`). That mirrors the code —
`kube.Client` and `kube.Executor` are sibling types with different transport
guards installed by different constructors — and it buys one thing prose cannot:
in an apiserver audit log, a mutating request attributed to
`system:serviceaccount:maklaude:maklaude` is by construction a bug or an
intrusion, never normal operation.

### What it grants

One rule per primitive in
[`internal/kube/executor.go`](../internal/kube/executor.go). The catalog is
closed — every primitive is one single-object API call, and between them they need
three verbs:

| API group | Resource | Verb | Executor method | What it does |
| --------- | -------- | ---- | --------------- | ------------ |
| `apps` | `deployments` | patch | `PatchDeployment` | Strategic-merge patch of one Deployment. `RestartDeploymentRollout` uses it to stamp the `kubectl.kubernetes.io/restartedAt` annotation (`kubectl rollout restart` at the API level) |
| `apps` | `deployments` | patch | `PatchDeploymentJSON` | RFC 6902 JSON patch of one Deployment — the same verb, a different patch type. `RollbackDeploymentToRevision` uses it to `replace` `/spec/template` with a previous revision's pod template (`kubectl rollout undo`). A strategic merge cannot express this: it merges containers and env vars by name, so anything the current revision added would survive the rollback |
| `""` (core/v1) | `nodes` | patch | `PatchNode` | Strategic-merge patch of one Node. `CordonNode` uses it to set `spec.unschedulable` |
| `""` (core/v1) | `pods` | delete | `DeletePod` | Deletes one already-failed pod whose controller will recreate it |

Plus, via a second binding, the *same* `maklaude-readonly` ClusterRole the
observation identity holds. The write role is additive, not self-sufficient: an
executor has to re-read its target to obtain the `resourceVersion` it conditions
the action on, and a revision rollback additionally reads the target revision's
ReplicaSet — the pod template it restores only exists in the cluster. Those reads
run on a client built from the *zero* `WriteScope`, which admits read verbs and
refuses every mutating one, so the read half of a rollback holds no authority to
change anything. Reusing the read role verbatim means the two can't drift — but it
also means **the base bundle must be applied first**, or the executor's read
binding dangles on a ClusterRole that doesn't exist. (RBAC allows the dangling
reference and silently grants nothing, so the symptom is unexplained `Forbidden`
errors on reads, not an apply-time failure.)

### What it deliberately withholds

Each omission is load-bearing, and each is asserted absent in CI:

- **`deletecollection`** — the most dangerous verb available here: one request
  can remove every pod matching a selector. The executor's `WriteScope` pins an
  exact object path, so it cannot express a bulk delete; RBAC has to agree rather
  than merely not be asked.
- **`delete` on deployments or nodes** — deleting a workload or a node is an
  outage, not a remediation. Nothing in the catalog does it.
- **`update`** — sends a whole object, which would let a malformed body replace a
  spec MaKlaude never read. The executor only ever patches.
- **`create`, including `pods/eviction`** — cordoning is not draining. Eviction
  is outside the catalog.
- **`patch` on pods** — the pod primitive is delete-only.
- **`secrets` or any other read** — the write role adds no read of any kind.
- **`bind`, `escalate`, `impersonate`** — each would let this identity grant
  itself everything on this list.

### Dry-run still needs the write verb

Kubernetes authorizes a `dryRun=All` request with the **identical verb** it would
use for a real one; `SubjectAccessReview` has no notion of a preview. So MaKlaude
running in `execute=dry-run` — where every action is sent with `dryRun=All`, the
API server validates it against real admission controllers and then discards it —
still requires this bundle.

The consequence to internalize before reading `auth can-i` output: for the
executor identity, `kubectl auth can-i patch deployments` prints `yes` even on a
deployment where MaKlaude has never been permitted to change anything.
Preview-only is enforced by the mode (`kube.ExecuteDryRun`) and by the transport
(`kube.ErrDryRunRequired`, which refuses a mutating request lacking the marker) —
**not** by RBAC.

### Two independent gates

Installing this bundle does not enable execution, and enabling execution does not
grant permission. Both have to be opened, separately, by a person:

| Gate | Where it lives | Default | How to close it |
| ---- | -------------- | ------- | --------------- |
| API-server permission | this bundle | not installed | `kubectl delete -k deploy/rbac/write` |
| In-process kill switch | `kube.ExecuteMode` | `ExecuteDisabled` (the zero value; `kube.NewExecutor` refuses to build anything under it) | leave it unset |

Deleting the bundle is the cheaper revocation: it stops every write at the API
server without touching MaKlaude's config or restarting it, and leaves the
read / diagnose / propose path fully working.

### Installing and verifying it

```bash
kubectl apply -k deploy/rbac         # first — reads, and the maklaude SA
kubectl apply -k deploy/rbac/write   # the executor SA and the write delta
```

Mint a kubeconfig for `maklaude-executor` exactly as in step 2 above,
substituting the account name (`kubectl -n maklaude create token maklaude-executor`).

```bash
EXEC=system:serviceaccount:maklaude:maklaude-executor
SA=system:serviceaccount:maklaude:maklaude

# The catalog — each MUST print "yes".
kubectl auth can-i patch deployments.apps --as="$EXEC" -A
kubectl auth can-i patch nodes            --as="$EXEC"
kubectl auth can-i delete pods            --as="$EXEC" -A

# The dangerous neighbours — each MUST print "no".
kubectl auth can-i deletecollection pods  --as="$EXEC" -A
kubectl auth can-i delete deployments.apps --as="$EXEC" -A
kubectl auth can-i update deployments.apps --as="$EXEC" -A
kubectl auth can-i patch pods             --as="$EXEC" -A
kubectl auth can-i create pods/eviction   --as="$EXEC" -A
kubectl auth can-i get secrets            --as="$EXEC" -A

# The point of the separate identity: the observation account is UNCHANGED.
# Each MUST still print "no".
kubectl auth can-i patch deployments.apps --as="$SA" -A
kubectl auth can-i delete pods            --as="$SA" -A
```

### Narrowing it further

`deployments` and `pods` are namespaced. If MaKlaude operates a known set of
namespaces, copy those two rules into a `Role` per namespace and bind them with
`RoleBinding`s, leaving only the `nodes` rule in a `ClusterRole`. That is strictly
tighter than what ships and requires no code change. Nodes are cluster-scoped and
cannot be narrowed this way.

## Validation status

Both bundles assemble cleanly under `kubectl kustomize`, and their contents are
asserted by the unit suite in [`test/rbac/`](../test/rbac/), which runs on every
PR without needing a cluster: `maklaude-readonly` grants **no** mutating verb,
`maklaude-write` grants **exactly** the executor's three, no binding hands a
mutating role to the observation identity, and the base kustomization does not
pull in the write delta.

Against a live API server, the `e2e` CI job applies both bundles to a `kind`
cluster and runs every `auth can-i` assertion above — including the re-check that
the observation identity is still denied writes *after* the write bundle is
installed. The two layers answer different questions: the unit suite catches a
widened manifest in seconds, `can-i` proves the cluster actually behaves that way.
