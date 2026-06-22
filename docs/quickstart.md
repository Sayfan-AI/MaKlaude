# Operator quickstart

This guide takes you from nothing to a running read-only scan in three steps:
**register a cluster**, **grant read-only access**, and **run the monitor**.
MaKlaude only ever *reads* your clusters — see the
[no-writes guarantee](no-writes.md) for how that is enforced and tested.

## Prerequisites

- The `maklaude` binary. Build it from this repo with [Task](https://taskfile.dev):

  ```bash
  task build          # produces ./bin/maklaude
  ./bin/maklaude version
  ```

- `kubectl` access to each cluster you want MaKlaude to watch, and permission to
  create the read-only RBAC bundle on it.

## 1. Grant MaKlaude read-only access (RBAC)

Apply the least-privilege bundle to the cluster. It creates the `maklaude`
namespace, ServiceAccount, a `maklaude-readonly` ClusterRole (only
`get`/`list`/`watch`, no mutating verbs, no `secrets`/`configmaps`), and the
binding:

```bash
kubectl apply -k deploy/rbac
```

Then mint a kubeconfig that authenticates **as that ServiceAccount** (MaKlaude
references clusters by kubeconfig path, never inline credentials). The full
recipe — minting a token and assembling a standalone kubeconfig — and the
`kubectl auth can-i` commands to confirm the access really is read-only are in
[`docs/rbac.md`](rbac.md). The short version:

```bash
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
kubectl config view --minify --raw \
  -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > /tmp/maklaude-ca.crt
TOKEN=$(kubectl -n maklaude create token maklaude --duration=8760h)

KCFG=$HOME/.kube/maklaude-prod-us-east.yaml
kubectl --kubeconfig "$KCFG" config set-cluster prod-us-east \
  --server="$SERVER" --certificate-authority=/tmp/maklaude-ca.crt --embed-certs=true
kubectl --kubeconfig "$KCFG" config set-credentials maklaude --token="$TOKEN"
kubectl --kubeconfig "$KCFG" config set-context prod-us-east --cluster=prod-us-east --user=maklaude
kubectl --kubeconfig "$KCFG" config use-context prod-us-east
```

> Treat this kubeconfig as a credential: keep it off version control and
> `chmod 600` it. MaKlaude stores only its *path*, never its contents.

Repeat per cluster — each gets its own kubeconfig and stays fully isolated.

## 2. Register the cluster

Declare the clusters MaKlaude operates in a YAML config. The format is
**secret-safe by design**: each cluster is referenced by a *path* to an existing
kubeconfig file and a *context* name — never inline credentials. Start from
[`config.example.yaml`](../config.example.yaml):

```yaml
clusters:
  - name: prod-us-east                               # unique, human-friendly identifier
    kubeconfig: /home/alice/.kube/maklaude-prod-us-east.yaml   # path to the SA kubeconfig from step 1
    context: prod-us-east                            # context to select within it

  - name: staging
    kubeconfig: ~/.kube/config                       # a leading "~" expands to your home directory
    context: staging
```

Each entry requires a unique `name`, a `kubeconfig` path (the file must exist on
disk), and a `context`. The config is loaded and validated by `internal/cluster`,
which **fails loudly** and aggregates every problem at once (missing/empty file,
malformed YAML, unknown fields, empty list, missing fields, duplicate names, or a
kubeconfig path that does not exist). See the
[Cluster configuration](../README.md#cluster-configuration) section of the README
for the full field reference.

## 3. Run the monitor

`maklaude scan` runs the full read-only pipeline once across every registered
cluster: it collects health signals, detects problems deterministically, and
reconciles findings into the comms trail, then prints a report. It performs **no
mutating action** against any cluster.

```bash
./bin/maklaude scan --config config.yaml          # human-readable report
./bin/maklaude scan --config config.yaml --json   # machine-readable JSON report
```

`--config <path>` is required. `--json` switches the report to JSON; the report
carries, per cluster, the reachability, the findings (identity / severity /
object / title / message, most-urgent-first), and the escalation outcome
(opened / updated / closed), plus cross-cluster totals.

### (Optional) Route escalations to GitHub

With no GitHub configuration, escalation degrades to a safe **in-memory dry-run**
(nothing is written anywhere). To open/track one GitHub issue per active problem,
set these before running the scan:

| Variable                | Description                                               |
| ----------------------- | --------------------------------------------------------- |
| `MAKLAUDE_GITHUB_REPO`  | `owner/repo` to use as the comms trail.                   |
| `MAKLAUDE_GITHUB_TOKEN` | Token with `issues:write` on that repo. Never logged.     |
| `MAKLAUDE_GITHUB_API`   | Optional REST API base override (GitHub Enterprise).      |

See the [Comms trail & escalation](../README.md#comms-trail--escalation) section
of the README for the issue lifecycle (open / recur / clear) and the
`needs:human` gating model.

## Next steps

- [`docs/no-writes.md`](no-writes.md) — the documented, test-backed guarantee that
  MaKlaude issues no mutating API calls.
- [`docs/rbac.md`](rbac.md) — the full read-only access model and verification.
- [`docs/slack.md`](slack.md) — optional Slack / ChatOps notifications; unset by
  default and degrades cleanly to GitHub + email with zero behavior change.
