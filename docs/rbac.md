# MaKlaude read-only RBAC

MaKlaude is a **read-only** Kubernetes monitoring system. It observes the health
of the clusters under its care and never mutates them. This document describes
the least-privilege RBAC bundle that grants MaKlaude exactly the access it needs,
how an operator installs it, how to wire the resulting identity into MaKlaude,
and how to verify the access really is read-only.

The bundle lives in [`deploy/rbac/`](../deploy/rbac/) and applies as one unit
with `kubectl apply -k deploy/rbac`.

## The access model

MaKlaude authenticates to each cluster as a single ServiceAccount
(`maklaude` in the `maklaude` namespace) whose only permissions come from one
ClusterRole (`maklaude-readonly`). That role grants the **read triad**
(`get`, `list`, `watch`) on a small, fixed set of resources — and nothing else.

| API group | Resources | Verbs | Why MaKlaude reads it |
| --------- | --------- | ----- | --------------------- |
| `""` (core/v1) | `nodes` | get, list, watch | Node Ready / memory / disk / PID pressure & schedulability |
| `""` (core/v1) | `pods` | get, list, watch | Pod phase, restart counts, CrashLoopBackOff detection |
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
and `watch`. It also grants **no access to `secrets` or `configmaps`**.

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

## Validation status

The manifests in this bundle are validated structurally with
`kubectl kustomize build deploy/rbac` (the bundle assembles cleanly) and a YAML
parser. Full server-side validation (`kubectl apply --dry-run=server`) and the
`auth can-i` checks above require a live API server, so **end-to-end
verification against a real `kind` cluster is covered by tasks T8/T9** (the
kind-based CI/e2e harness).
