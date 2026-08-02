# MaKlaude

Build MaKlaude — an autonomous system for operating Kubernetes clusters on a human's behalf.

MaKlaude continuously watches the health of one or more clusters a human has put under its care: it detects problems, diagnoses root causes, and safely fixes what it confidently can. Anything risky or destructive it does NOT do on its own — it escalates to a human with enough context to decide, and acts only once approved. Throughout, it keeps humans informed through whatever channel they prefer (Slack, email, GitHub, etc.) so there's always a clear, auditable trail of what it saw, what it did, and what it's waiting on.

Guiding principles, not a blueprint — you decide the actual architecture, agents, and tools:
- Safety first. Read/diagnose freely; gate every mutating or destructive action behind explicit human approval until trust is earned. Least privilege everywhere.
- Multi-cluster from the start. A human can register several clusters; MaKlaude operates them without cross-contamination.
- Extensible. New operational capabilities (e.g. security/vulnerability scanning, cost and capacity awareness, GitOps-aware remediation) should be addable over time without redesign.
- Human-in-the-loop, not human-replaced. MaKlaude augments operators; it never silently takes irreversible action.

Important boundary: humans configure which clusters MaKlaude monitors and operates, and supply the credentials/access. Building that configuration surface and the operational system is your job; standing it up against real clusters is the human's job once it's built.

Treat the well-known "multi-agent Kubernetes DevOps" pattern (a coordinator delegating to specialized analyze / remediate / communicate roles) as inspiration only — feel free to surpass it. Aim higher than a minimal demo: build something an operator would actually trust with real clusters.

## Documentation

Full operator and architecture docs live in [`docs/`](docs/index.md). Start with [`docs/index.md`](docs/index.md) for the doc map and a suggested reading order.

**Safety posture, stated accurately.** MaKlaude's observation path never mutates a cluster — that part is unconditional and proven four ways ([no-writes guarantee](docs/no-writes.md)). MaKlaude as a *whole* is not read-only: since Milestone 4 there is a separate, opt-in write path for remediation a human approved. It is off by default in the strongest available sense — the shipped binary has no command or config key that reaches it — and when wired up it stays gated on a separately-installed RBAC bundle, an in-process kill switch, an attributable approval, and preconditions re-checked against a fresh read. Every *mutating* action needs an explicit, attributable human approval **by default**, and there is exactly one supported way out of *that* gate: an operator deliberately setting `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1`, which waives the requirement for consent and nothing else, and records every action it waives as unreviewed in the artifact, the chat notice, the process log, and the audit trail. See [Gated remediation](#gated-remediation), [Approval gate & autonomous mode](#approval-gate--autonomous-mode), [`docs/remediation.md`](docs/remediation.md), and [`docs/autonomous-mode.md`](docs/autonomous-mode.md).


## Architecture posture — deterministic product, AI dev system

MaKlaude is two layers: the running product is **deterministic Go** — the read-only `collect → detect → correlate → diagnose → escalate` path has no model in it and runs the same in tests, the `kind` e2e, and production — and the AI is the **dev system that builds and evolves it** (an orchestrator, workers, an evolver, and a human-interaction agent). The one runtime exception is the optional, gated `aidiagnose` seam, off by default (see below). The upshot: what runs against your clusters is deterministic and auditable; the intelligence was spent at build time, not wired into the hot path.

Full detail: **[docs/architecture.md](docs/architecture.md)**.


## Setup

This repo runs autonomously via GitHub Actions, but **genesis ships those workflows disabled and with no secrets**. They authenticate as the Genesis GitHub App and call the Anthropic API, so running them before the credentials exist would just fail on every trigger. The repo and issue #1 already exist; the autonomous loop stays dormant until you activate it.

1. **Install the Genesis GitHub App** on this repository, granting it `contents`, `issues`, `pull-requests`, and `workflows` permissions.

2. **Activate the dev system** — from a clone of this repo, run one command:

   ```bash
   .genesis/scripts/activate.sh
   ```

   It reads the App ID, App private key, and Anthropic key from your `~/.config/genesis/.env` (shared across all your genesis projects), verifies the App is installed here, sets them as this repo's Actions secrets, and enables the workflows. It refuses to run if any value is missing/placeholder or the App isn't installed. The next trigger (an issue/PR/comment event, a push, or the cron) then wakes the orchestrator and onboarding begins on issue #1.

3. **(Optional) Observability & notifications** — to ship agent activity logs to Grafana Cloud Loki, add all three of `GENESIS_LOKI_URL` (the Loki host, no path), `GENESIS_LOKI_USER` (numeric instance ID), and `GENESIS_LOKI_TOKEN` to `~/.config/genesis/.env` **before** running `activate.sh` — it seeds them as repo secrets and the workflows pass them to the logging hooks. Without them, activity logs still appear in the Actions run logs, just not in Loki. Configure the A2H gateway if you want Slack/email instead of GitHub-issue comms.

---

## Operator quickstart

If you just want to point MaKlaude at a cluster and run a read-only scan, follow
the three-step **[operator quickstart](docs/quickstart.md)**: grant read-only
RBAC, register the cluster, and run `maklaude scan`. Everything `scan` does is a
read — see the **[no-writes guarantee](docs/no-writes.md)** for how that is
enforced and tested. The separate write path is not reachable from the binary and
takes a deliberate opt-in to enable at all; see
**[Gated remediation](#gated-remediation)**.

## Development

MaKlaude is written in **Go** (1.24+). The codebase follows the standard Go
project layout:

```
cmd/maklaude/      # CLI entrypoint (version/help + `scan`, see below)
internal/          # private packages
  version/         #   build/version metadata
  cluster/         #   cluster registry & config surface (see below)
  kube/            #   read-only Kubernetes client/transport
  health/          #   judgment-free snapshot collection (read-only)
  detect/          #   deterministic findings from a snapshot
  correlate/       #   groups findings into incidents (root cause + effects)
  diagnose/        #   deterministic ranked root-cause hypotheses per incident
  aidiagnose/      #   OPTIONAL, gated LLM refinement of hypotheses (see below)
  escalate/        #   human comms trail: issue-per-problem (see below)
  scan/            #   one-shot pipeline wiring (collect -> detect -> escalate)
test/e2e/          # end-to-end test on kind (build tag `e2e`) + seed manifests
```

## Running a scan

`maklaude scan` runs the full read-only pipeline once across every registered
cluster: for each cluster it collects health signals, detects problems
deterministically, **correlates** the findings into incidents, **diagnoses** each
incident into ranked root-cause hypotheses, and reconciles those incidents into
the comms trail, then prints a report. It performs **no mutating action** against
any cluster — its only writes are to the escalation trail, and those degrade to an
in-memory dry-run unless GitHub is configured (see below).

```bash
maklaude scan --config config.yaml        # human-readable report
maklaude scan --config config.yaml --json # machine-readable report (used by e2e)
```

The JSON report carries, per cluster, the reachability, the raw findings
(identity / severity / object / title / message, most-urgent-first), the
correlated **incidents** (each with its primary object, affected objects, and
ranked root-cause hypotheses), and the escalation outcome (opened / updated /
closed), plus cross-cluster totals.

## LLM-assisted diagnosis (optional, gated)

MaKlaude's root-cause diagnosis (`internal/diagnose`) is **deterministic** and
ships fully functional on its own. Since M3/T5 you can **optionally** let a Claude
model *refine* those hypotheses for cases the rules handle poorly — sharpening a
low-confidence hypothesis, or proposing a cause the deterministic rules cannot
express. This lives in `internal/aidiagnose` and is the **one place** where
cluster-derived data could leave the process, so it is built as a strict,
isolated **safety boundary** and is **off by default**:

- **Read-only by construction.** The provider interface exposes exactly one
  capability — turn a redacted text prompt into text suggestions. It holds no
  cluster client and no mutating capability, so an LLM can *inform* a diagnosis
  but can never act on a cluster.
- **Redaction before egress.** Evidence is assembled from the snapshot/incident
  and passed through a redactor at the egress boundary, stripping secret values,
  tokens, credentials, and obvious PII **before** anything reaches the provider.
  This is proven against seeded secrets in unit tests.
- **Cost-bounded.** Evidence is size-capped, the response token count is capped,
  each call is deadline-bounded, and a per-cycle call budget caps how many calls
  one scan can make.
- **Graceful degradation.** Unconfigured, disabled, over budget, erroring, or
  timing out all resolve to the same safe outcome: the deterministic hypotheses,
  unchanged. The refiner never panics and never fails a scan.
- **Audited.** Every provider call's purpose and outcome is recorded (cluster,
  incident, model, evidence size, outcome), and every refined hypothesis carries a
  `refined` source marker distinct from `deterministic`, so the comms trail always
  shows what came from a rule versus a model.

It requires an explicit **double opt-in** — the feature flag *and* an API key —
so a stray key alone does nothing. Configuration is entirely via environment
(never the cluster config file, which stays secret-free):

| Variable                   | Description                                                              |
| -------------------------- | ------------------------------------------------------------------------ |
| `MAKLAUDE_LLM_DIAGNOSIS`   | Feature flag; must be truthy (`true`/`1`/`yes`/…) to enable the layer.   |
| `MAKLAUDE_LLM_API_KEY`     | Claude API key (falls back to `ANTHROPIC_API_KEY`). Never logged.        |
| `MAKLAUDE_LLM_MODEL`       | Optional model id override (default `claude-sonnet-5`).                  |
| `MAKLAUDE_LLM_API_BASE`    | Optional API base override (proxy / compatible gateway).                 |
| `MAKLAUDE_LLM_MAX_EVIDENCE`| Optional cap on redacted evidence bytes per call (default 8000).         |
| `MAKLAUDE_LLM_MAX_TOKENS`  | Optional response-token cap per call (default 1024).                     |
| `MAKLAUDE_LLM_CALL_BUDGET` | Optional per-scan-cycle provider-call budget (default 8).                |
| `MAKLAUDE_LLM_TIMEOUT`     | Optional per-call timeout as a Go duration (default `20s`).              |

With `MAKLAUDE_LLM_DIAGNOSIS` unset (the default), diagnosis runs the byte-stable
deterministic core alone, exactly as if T5 were absent.

## Comms trail & escalation

MaKlaude keeps humans informed through an **auditable comms trail** rather than
a stream of alerts. Since M3/T4 the `internal/escalate` package escalates at
**incident granularity**: it turns each correlated
[`correlate.Incident`](internal/correlate/incident.go) — plus its ranked
[`diagnose.Hypothesis`](internal/diagnose/hypothesis.go)es — into exactly **one
tracked GitHub issue per active incident**, and keeps that issue in sync as the
incident recurs and clears. This carries the *diagnosis* (a correlated incident +
ranked root-cause hypotheses + the exact evidence) into the trail, not just a raw
symptom, so one real cause no longer fans out into a pile of separate issues.

The whole model hangs off the stable `correlate.IncidentIdentity` key (the same
ongoing incident yields the same identity every cycle):

- **One diagnostic issue per incident.** A newly seen incident opens a single,
  well-formed issue: the incident summary (cluster, severity, primary object),
  the **ranked root-cause hypotheses** each with its confidence, explanation, and
  the specific evidence findings grouped under it, the affected objects, and
  **manual, read-only next steps** (kubectl describe/logs/get/top, inspect image,
  check quotas). When the leading hypothesis is low-confidence, the body honestly
  surfaces the competing hypotheses rather than overcommitting. The cluster is
  named in the title so multi-cluster setups stay legible at a glance.
- **Read-only by construction.** M3 diagnoses; it does not remediate. The issue
  body **never** claims MaKlaude will run, apply, delete, scale, or otherwise
  mutate anything — every suggested step is an investigation for a human to run.
  This is asserted in unit tests.
- **Recurrence updates, never duplicates.** When the same incident is diagnosed
  again on a later cycle, MaKlaude refreshes the existing issue's body and adds a
  recurrence comment — it does **not** open a second issue. This dedup is the
  core guarantee, and it is unit-tested without any network.
- **Clearance closes the trail.** When a previously-active incident is no longer
  present, its issue is closed with a closing comment, so the record stays
  complete and self-explanatory.
- **`needs:human` gating.** Warning- and critical-severity incidents are labelled
  `needs:human` (in addition to the `maklaude` management label) to flag that a
  decision is wanted. Info-level incidents are recorded but not gated. **The
  escalation trail itself is purely informational** — it never proposes or performs
  a mutating action, and its issue bodies never claim MaKlaude will change anything.
  Mutating actions live on a separate trail behind the approval gate; see
  [Approval gate & autonomous mode](#approval-gate--autonomous-mode).
- **Restart-safe.** Each issue embeds its incident identity in a hidden marker
  (`<!-- maklaude:identity=… -->`). The escalator rediscovers which open issue
  maps to which incident by listing issues, so it stays correct even if the
  monitor process restarts — it never relies solely on in-memory state.

### Email notifications (M1)

For M1, MaKlaude relies on **GitHub's own notification emails** — it does not
ship a separate SMTP layer. Watchers, assignees, and `needs:human` label
subscribers are emailed by GitHub whenever an issue is opened, commented on, or
closed, which is exactly the open/recur/clear lifecycle above. A dedicated email
channel can be added later behind the same `IssueSink` boundary without touching
the reconcile logic.

### Configuration

GitHub access is injected via environment variables; with none set, escalation
degrades gracefully to a **no-op, side-effect-free dry run** (an in-memory sink),
so unit tests and the e2e harness run without real credentials.

| Variable                | Description                                                     |
| ----------------------- | --------------------------------------------------------------- |
| `MAKLAUDE_GITHUB_REPO`  | `owner/repo` of the repository to use as the comms trail.       |
| `MAKLAUDE_GITHUB_TOKEN` | Token with `issues:write` on that repo. Never logged.           |
| `MAKLAUDE_GITHUB_API`   | Optional REST API base override (for GitHub Enterprise).         |

The reconcile core (`escalate.Reconcile`) is a **pure function** of
`(subjects, tracked issues)` — where each subject is an incident plus its ranked
diagnosis; no I/O, no clock — and the GitHub interaction
sits behind the small `escalate.IssueSink` interface, so the interesting logic
is exhaustively unit-tested with a fake in-memory sink. The package touches
GitHub and **never** a Kubernetes cluster, keeping MaKlaude's read-only safety
boundary intact.

## Gated remediation

Since Milestone 4, MaKlaude can carry out a fix — but only one a human approved,
on the object they approved it for, through a path that shares nothing with the
observation path.

**It is off by default, and "off" is stronger than a setting.** The shipped
binary has two commands, `version` and `scan`. Nothing in the config file, the
CLI, or a scheduled loop constructs an executor, so there is currently no way to
reach the write path from a running MaKlaude at all. Wiring it up is a separate,
deliberate step — which is why the sections below describe gates rather than
enabling instructions: there is no `remediation:` block to add to your config,
and no environment variable that turns writes on.

### The catalog is closed

Four operations, each one API call against one object:

| Operation | Reversibility | What it does |
| --------- | ------------- | ------------ |
| `rolloutrestart` | reversible | `kubectl rollout restart` at the API level — stamps `restartedAt` on a Deployment's pod template; the update strategy governs the replacement |
| `rollbackrevision` | reversible | `kubectl rollout undo` — replaces `/spec/template` with a previous revision's pod template |
| `cordonnode` | reversible | Sets `spec.unschedulable`. Cordoning is **not** draining; running pods are left alone |
| `deletepod` | recreated-by-controller | Deletes one already-failed pod whose controller will recreate it |

### Five independent gates

An action reaches a cluster only if all of these are open, and each is opened by
a different person doing a different thing:

| Gate | Default | Notes |
| ---- | ------- | ----- |
| RBAC bundle `deploy/rbac/write` | not installed | binds a **separate** ServiceAccount, `maklaude-executor`. Deleting it is the cheapest revocation: writes stop at the API server, and read/diagnose/propose keeps working |
| Kill switch `kube.ExecuteMode` | `disabled` (the zero value) | `kube.NewExecutor` refuses to build anything under it, so an opted-out deployment holds no write-capable object |
| Human approval | none | an `approved` **label event** on the proposal artifact, from an identity MaKlaude cannot forge — see below |
| Preconditions | must hold | re-checked against a fresh cluster read immediately before the request |
| `resourceVersion` | must match | injected by the executor, enforced by the API server; a target that moved fails the action and applies nothing |

Installing the RBAC bundle does not enable execution, and enabling execution does
not grant permission.

### The kill switch has three positions

| Mode | Effect |
| ---- | ------ |
| `disabled` | Nothing runs; no executor is built. Shipped default |
| `dry-run` | Previews only. Every action carries `dryRun=All`, and the transport **refuses** a mutating request that lacks it, so the API server validates against real admission controllers and discards |
| `enabled` | Real, approved mutations. The only mode under which a cluster changes |

A dry run still needs the write verb — Kubernetes authorizes `dryRun=All` with
the identical verb it would use for a real request. Preview-only is enforced by
the mode and the transport, never by RBAC. See
[`docs/rbac.md`](docs/rbac.md#dry-run-still-needs-the-write-verb).

### Reversibility and the audit trail

`Execute` captures the target's pre-state and reports that a rollback is
*available*; it never performs one on its own, because an unbidden rollback is
itself an unapproved mutating action. `Rollback` runs only when asked, under the
same permission slip that authorized the action being undone.

Every lifecycle event appends one immutable, self-contained record — `proposed`,
`approved`, `executed`, `verified`, `failed`, `rolledback` — sequenced under a
lock so ordering is a fact rather than a race, with free text redacted before
storage and structured identifiers deliberately kept intact. The durable copy is
the approval artifact itself, which outlives the process.

Full detail: **[docs/remediation.md](docs/remediation.md)**.

## Approval gate & autonomous mode

Mutating actions do not travel on the escalation trail. They get their own
artifact — a `maklaude-proposal` issue carrying the exact operation, the target,
the dry-run preview, the reversibility class and rollback plan, the diagnosis
behind it, and the preconditions that will be re-checked — and nothing runs until
that artifact carries an `approved` label. The signal is a **label event**, not
prose, because GitHub records who applied it and when, from an identity MaKlaude
cannot forge and did not supply.

An approval is scope-bound and single-use: it covers one operation, on one object,
at one observed `resourceVersion`, once. It stops applying if the object moves, if
the artifact is refreshed with a newer preview after the approval, if it goes
stale, or if the problem clears on its own — in which case the request is withdrawn
**without running anything**. A pending approval is not a queued job. The gate lives
in [`internal/approve`](internal/approve); it holds no cluster client and cannot
execute what it authorizes.

### Autonomous mode (dangerous, off by default)

An operator who has watched MaKlaude propose actions for a while and wants the loop
to close unattended can waive the human requirement:

```bash
export MAKLAUDE_DANGEROUSLY_AUTO_APPROVE=1
```

It is named the way it is on purpose. It waives **consent and nothing else** — a
human's `rejected` label still stops the action, `resourceVersion` drift still
refuses, a failed dry-run still blocks, the executed-label idempotency flag still
holds across restarts, preconditions are still re-checked against a fresh cluster
read immediately before the action, and the write-path kill switch
(`kube.ExecuteMode`) is a separate gate that must open independently: auto-approval
while the executor is in dry-run produces an unattended *rehearsal*, not a change.

It never fakes a human. An auto-approved action's permission slip carries a policy
marker rather than a login, and the artifact, the chat notice, the process log, and
the audit trail each say **"no human reviewed this"** in those words. A trail that
overstates human involvement is worse than no trail.

Values are strict: `1`/`true` on, `0`/`false`/unset off, and **anything else is a
fatal startup error** rather than a guess — the lazy "non-empty is truthy" parse
would turn `=no` into an armed autonomous mode set by somebody trying to disable it.

### `MAKLAUDE_GITHUB_SELF_LOGIN` is required when the gate is on

The gate's self-approval defense needs to know which account MaKlaude *is*.

| Variable | Description |
| -------------------------------- | ---------------------------------------------------------- |
| `MAKLAUDE_GITHUB_SELF_LOGIN`     | The login MaKlaude's `MAKLAUDE_GITHUB_TOKEN` belongs to, so a decision label it applied to its own artifact is recognized and refused. **Required** when a live comms trail is configured and `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` is off. |
| `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` | Waives the human approval requirement. Off unless set to `1` or `true`. |

Starting a live gate with neither set is a **fatal error**, not a warning. Without
the login, MaKlaude running under a person's own token cannot tell its own label
events from a human's — so the gate would look armed and quietly approve everything
MaKlaude asked for, with nothing in the trail saying otherwise. Either name the
identity or say out loud that no approval promise is being made; the silent middle
is what this refuses.

Full detail, including the exact list of what the bypass gives up:
**[docs/autonomous-mode.md](docs/autonomous-mode.md)**.

## Cluster configuration

MaKlaude operates the Kubernetes clusters a human puts under its care. You
declare those clusters in a YAML config file. The format is **secret-safe by
design**: each cluster is referenced by a *path* to an existing kubeconfig
file and a context name — credentials are never stored in or read from this
config, and nothing here should ever be committed to version control.

A starter file lives at [`config.example.yaml`](config.example.yaml):

```yaml
clusters:
  - name: prod-us-east           # unique, human-friendly identifier
    kubeconfig: /home/alice/.kube/prod-us-east.yaml  # path to an existing kubeconfig
    context: prod-us-east        # context to select within that kubeconfig

  - name: staging
    kubeconfig: ~/.kube/config   # a leading "~" expands to your home directory
    context: staging
```

Each entry requires three fields:

| Field        | Description                                                        |
| ------------ | ------------------------------------------------------------------ |
| `name`       | Unique, human-friendly cluster identifier (must be unique).        |
| `kubeconfig` | Filesystem path to an existing kubeconfig file (never inline creds).|
| `context`    | Name of the context to select within that kubeconfig.              |

The configuration is loaded and validated by the `internal/cluster` package.
Validation **fails loudly with clear, actionable errors** and aggregates every
problem it finds at once. It rejects: a missing or empty file, malformed YAML,
unknown fields, an empty `clusters` list, missing required fields, duplicate
cluster names, and any referenced kubeconfig file that does not exist on disk.

Each successfully validated cluster resolves to an isolated `Handle` (name,
kubeconfig path, context) with **no shared or global mutable state** across
clusters. A handle becomes a live read-only `kube.Client` — and, only under the
opt-in write path, a per-action `kube.Executor` scoped to that one cluster, so
clusters cannot cross-contaminate in either direction.

There is deliberately **no remediation section in this file.** Enabling writes is
not a config-file decision: it takes a separately-installed RBAC bundle plus an
explicitly-constructed executor, and neither is reachable from here. See
[Gated remediation](#gated-remediation).

### Read-only access (RBAC)

The scan path only ever reads a cluster. The least-privilege RBAC bundle in
[`deploy/rbac/`](deploy/rbac/) grants its ServiceAccount exactly the
`get`/`list`/`watch` access the code needs and **no mutating verbs**. Apply it
with `kubectl apply -k deploy/rbac`. See [`docs/rbac.md`](docs/rbac.md) for the
full access model, how to mint a kubeconfig for the ServiceAccount and register
it above, and how to verify the access is truly read-only.

Remediation needs a second identity, and it lives in a **separate** bundle
(`deploy/rbac/write/`) binding a separate ServiceAccount, `maklaude-executor`.
Applying the base bundle alone leaves MaKlaude unable to change anything at the
API server, which is the intended default. See
[`docs/rbac.md`](docs/rbac.md#the-optional-minimal-write-bundle).

### Task runner

This project uses [**Task**](https://taskfile.dev) as its task runner via
`Taskfile.yml`. **There is no Makefile and `make` is never used** — this is an
explicit project rule. Install Task (`go install github.com/go-task/task/v3/cmd/task@latest`
or see the docs), then:

| Command           | What it does                                            |
| ----------------- | ------------------------------------------------------- |
| `task`            | List all available tasks                                |
| `task build`      | Build the `maklaude` binary into `./bin`                |
| `task test`       | Run unit tests with the race detector and coverage      |
| `task e2e`        | Run the end-to-end test (needs a seeded kind cluster; see below) |
| `task lint`       | Run `golangci-lint` (auto-installs the pinned version)  |
| `task vet`        | Run `go vet ./...`                                       |
| `task fmt`        | Format all Go source with `gofmt`                       |
| `task fmt:check`  | Fail if any file is not `gofmt`-clean                   |
| `task tidy`       | Run `go mod tidy` and `go mod verify`                   |
| `task ci`         | Full quality gate: build + fmt-check + vet + lint + test |
| `task check`      | Alias for `task ci`                                     |
| `task clean`      | Remove build artifacts                                  |

### Quality gate

CI (`.github/workflows/ci.yml`) runs on every pull request and on pushes to
`main`. It builds the project, runs `golangci-lint`, and executes the unit
tests — the same checks `task ci` runs locally. Keep the gate green.

### End-to-end test (kind)

A separate CI job (`.github/workflows/e2e.yml`) runs on every pull request: it
creates a real [kind](https://kind.sigs.k8s.io/) cluster, applies the read-only
RBAC bundle, seeds four failure scenarios — a **crashlooping** pod, an
**unschedulable/pending** pod, a Deployment on an **unpullable image**, and a
Deployment **wedged by a bad rollout** (see
[`test/e2e/manifests/`](test/e2e/manifests/)) — waits for each to manifest, then
runs the pipeline **as MaKlaude's least-privilege ServiceAccount** and asserts:

1. **Findings** — a critical `pod.crashloop` and a warning `pod.pending` are detected.
2. **Escalation** — the findings correlate into incidents and a diagnostic issue is opened per incident (in-memory dry-run, no external writes).
3. **Zero writes** — proven four ways, belt-and-suspenders:
   - RBAC: the SA has only `get`/`list`/`watch` (verified with `kubectl auth can-i`);
   - state invariance: the seeded objects' `resourceVersion`/`generation`/`managedFields` are unchanged across the scan;
   - active refusal: a deliberate write through the same guarded transport every client uses is refused with `kube.ErrWriteForbidden`;
   - audit log: the apiserver audit log shows **no** mutating verb attributed to the MaKlaude SA.

   The no-writes assertions are part of the test and **fail the build** if violated.
   See [`docs/no-writes.md`](docs/no-writes.md) for the full belt-and-suspenders
   guarantee and the exact code/tests that back each layer.
4. **Gated remediation** — the wedged Deployment is driven all the way through:
   the pipeline proposes a rollback, a dry run of that exact request is previewed
   against the API server, an explicit human approval is simulated on the
   approval artifact, and only then is the action executed as the separate
   `maklaude-executor` identity. The cluster must **converge** back to healthy,
   the **audit trail** must name the approver, the **approval artifact** must
   show the whole `proposed → approved → executed → verified` lifecycle, and —
   the sharp one — exactly **one** mutating request may have landed on the
   cluster, the one that was approved. See
   [`test/e2e/remediation_test.go`](test/e2e/remediation_test.go).

The test is gated behind the `e2e` build tag (`task e2e`) and expects
`MAKLAUDE_E2E_KUBECONFIG`, `MAKLAUDE_E2E_EXECUTOR_KUBECONFIG`,
`MAKLAUDE_E2E_CONTEXT`, and (optionally) `MAKLAUDE_E2E_AUDIT_LOG`; the CI job sets
them. `MAKLAUDE_GITHUB_*` is left unset so escalation stays a safe dry-run.

Try the CLI:

```bash
task build
./bin/maklaude version
./bin/maklaude help
./bin/maklaude scan --config config.yaml   # one-shot read-only scan
```

---

*Bootstrapped by [Genesis](https://github.com/Sayfan-AI/genesis) — an autonomous agentic AI dev system.*
