# MaKlaude's no-writes guarantee

MaKlaude's foundational safety promise is simple: **its observation layer never
mutates a cluster.** It reads health signals, detects problems, and escalates to
humans — it does not create, update, patch, or delete anything in any cluster it
watches. (Its only writes are to the *comms trail* — GitHub issues — and even
those degrade to an in-memory dry-run unless GitHub is explicitly configured; see
the [README](../README.md). No cluster is ever touched.)

This document explains how that promise is enforced and, crucially, **cites the
exact code and tests that back each layer** so the guarantee stays verifiable
rather than aspirational. The design is belt-and-suspenders: four independent
layers would all have to fail before MaKlaude could mutate a cluster.

## The layers

| # | Layer | What it guarantees | Where it lives | What proves it |
| - | ----- | ------------------ | -------------- | -------------- |
| 1 | Structural in-process guard | A mutating HTTP verb is refused before it reaches the network — on the read-only client *and* on any write-capable clientset built from the same config | [`internal/kube/transport.go`](../internal/kube/transport.go), [`internal/kube/client.go`](../internal/kube/client.go), [`internal/kube/verify.go`](../internal/kube/verify.go) | [`internal/kube/transport_test.go`](../internal/kube/transport_test.go), [`internal/kube/client_test.go`](../internal/kube/client_test.go), [`internal/kube/verify_test.go`](../internal/kube/verify_test.go) |
| 2 | Least-privilege RBAC | The API server itself rejects any mutating verb — the SA only has `get`/`list`/`watch` | [`deploy/rbac/`](../deploy/rbac/), documented in [`docs/rbac.md`](rbac.md) | `kubectl auth can-i` checks in the e2e CI job and in [`docs/rbac.md`](rbac.md#verifying-the-access-is-read-only) |
| 3 | State invariance | After a full scan, seeded objects are byte-for-byte unchanged — a write would have bumped `resourceVersion`/`generation`/`managedFields` | the e2e harness | [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go) (`TestE2E_ReadOnlyScan`, "ZERO writes: object state is unchanged") |
| 4 | Audit-log corroboration | The apiserver's own record shows no mutating verb attributed to MaKlaude's SA | the e2e harness + apiserver audit log | [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go) (`assertNoMutatingAudit`) |

Layer 1 is in the fast unit suite (`task test`). Layers 2–4 run against a live
`kind` cluster in the e2e CI job (`task e2e`, behind the `e2e` build tag); see
[`.github/workflows/e2e.yml`](../.github/workflows/e2e.yml).

---

### Layer 1 — Structural in-process guard

MaKlaude's Kubernetes access is read-only *by construction*, in two reinforcing
ways inside [`internal/kube`](../internal/kube/):

1. **No write methods exist on the public surface.** `kube.Client`
   ([`client.go`](../internal/kube/client.go)) exposes only reads
   (`ListPods`, `ListNodes`, `GetNamespace`, `CheckReachable`, …). The underlying
   `kubernetes.Interface` clientset is stored unexported and **never handed out**,
   so a caller simply has no way to express a write.

2. **A transport-level guard rejects mutating verbs before the network.** Every
   client built by `kube.NewClient` installs a `WrapTransport` hook
   (`restConfigForHandle` in [`client.go`](../internal/kube/client.go)) that wraps
   the HTTP transport in a `readOnlyRoundTripper`
   ([`transport.go`](../internal/kube/transport.go)). Its `RoundTrip` permits only
   an allowlist of read verbs — `GET`, `HEAD`, `OPTIONS` (`readOnlyMethods`) — and
   fails everything else (`POST`/`PUT`/`PATCH`/`DELETE`/`CONNECT`/`TRACE` and any
   non-standard verb) with the sentinel **`kube.ErrWriteForbidden`** *before*
   delegating to the inner transport. A blocked write therefore never touches the
   network. The guard is the outermost transport layer, holds no per-cluster
   state, and each client builds its own — so clusters stay isolated.

Because the guard lives on the `rest.Config`, it protects even a *write-capable*
clientset built from that config. `kube.WriteProbeClientForHandle`
([`verify.go`](../internal/kube/verify.go)) deliberately hands back a full
clientset built from the exact same `restConfigForHandle` — its sole purpose is
to let a test *attempt* a mutating call and assert it is refused. Nothing in the
production pipeline calls it.

**Tests that back this layer:**

- [`transport_test.go`](../internal/kube/transport_test.go) —
  `TestReadOnlyTransport_BlocksMutatingVerbs` asserts every mutating verb (plus a
  bogus `DELETECOLLECTION` verb, proving the allowlist is closed) returns
  `ErrWriteForbidden` and **never invokes the inner transport**;
  `TestReadOnlyTransport_AllowsReadVerbs` and `…_EmptyMethodTreatedAsGet` confirm
  reads pass through; `TestReadOnlyTransport_BlockEndToEnd` wires the guard around
  a real `httptest.Server` and proves a `POST` is blocked while the server is hit
  exactly once (by the `GET`).
- [`client_test.go`](../internal/kube/client_test.go) —
  `TestClient_ReadOperationsBlockedFromWriting` proves the guard is actually
  installed on a real `rest.Config` produced by `restConfigForHandle`: a
  hand-built `DELETE` through that transport is refused with `ErrWriteForbidden`.
- [`verify_test.go`](../internal/kube/verify_test.go) —
  `TestWriteProbeClientForHandle_DeleteRefused` is the in-process counterpart to
  the e2e active-refusal proof: a real `client-go` `Delete` call through the
  write-probe clientset is refused with `ErrWriteForbidden`. The kubeconfig points
  at an unroutable address, so the refusal is demonstrably the guard's doing (not
  a connection error) — and it runs in the fast unit suite, with no `kind`
  cluster required.

### Layer 2 — Least-privilege RBAC

The bundle in [`deploy/rbac/`](../deploy/rbac/) binds MaKlaude's ServiceAccount
(`maklaude` in namespace `maklaude`) to a single ClusterRole
(`maklaude-readonly`) that grants only the read triad — `get`, `list`, `watch` —
on a small, fixed set of resources (plus a `get` on the `pods/log` subresource,
used to lazily read a bounded tail of an implicated pod's container logs; logs
can only be fetched, never written), and **no mutating verbs** (no `create`,
`update`, `patch`, `delete`, `deletecollection`) and no access to `secrets` or
`configmaps`. This is the outermost boundary: even if layer 1 were somehow
bypassed, the API server itself would reject the write.

The full access model, how to mint a kubeconfig for the SA, and the exact
`kubectl auth can-i` commands an operator runs to confirm the access is read-only
are documented in [`docs/rbac.md`](rbac.md). The e2e CI job applies this bundle
and runs the scan **as this SA**, so layers 3 and 4 below are observed under the
real least-privilege identity.

### Layer 3 — State invariance

`TestE2E_ReadOnlyScan` in [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go) seeds
a live `kind` cluster with two failure scenarios, captures each seeded pod's
`resourceVersion`, `generation`, and `managedFields` count, runs the **real
pipeline** once, then re-reads them. Any write — even a no-op apply — would bump
at least one of these fields, so the test turns "did anything mutate?" into a
precise equality check. A mismatch fails the build with a `ZERO-WRITES VIOLATION`.

### Layer 4 — Audit-log corroboration

`assertNoMutatingAudit` in [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go)
reads the apiserver audit log (when `MAKLAUDE_E2E_AUDIT_LOG` is set; the kind
config in [`test/e2e/kind/audit-policy.yaml`](../test/e2e/kind/audit-policy.yaml)
enables it) and fails the build if **any** mutating verb (`create`, `update`,
`patch`, `delete`, `deletecollection`) is attributed to
`system:serviceaccount:maklaude:maklaude`. This is the strongest external
corroboration: it is the cluster's own independent record, not MaKlaude's. When
the log is unavailable the check skips with a warning — layers 1–3 still hold.

---

## How to re-verify the guarantee yourself

```bash
# Layer 1 (in-process guard) — runs in seconds, no cluster needed:
task test          # exercises internal/kube/*_test.go above

# Layers 2–4 (RBAC + state invariance + audit) — needs a kind cluster:
task e2e           # see README "End-to-end test (kind)"; CI runs this on every PR

# Layer 2 (RBAC) — against any cluster where you applied deploy/rbac:
SA=system:serviceaccount:maklaude:maklaude
kubectl auth can-i --list --as="$SA"      # only get/list/watch on the documented resources
kubectl auth can-i delete pods --as="$SA" # MUST print "no"
```

See [`docs/rbac.md`](rbac.md) for the complete `auth can-i` verification matrix.
