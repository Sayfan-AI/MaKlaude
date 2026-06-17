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


## Setup

This repo runs autonomously via GitHub Actions, but **genesis ships those workflows disabled and with no secrets**. They authenticate as the Genesis GitHub App and call the Anthropic API, so running them before the credentials exist would just fail on every trigger. The repo and issue #1 already exist; the autonomous loop stays dormant until you activate it.

1. **Install the Genesis GitHub App** on this repository, granting it `contents`, `issues`, `pull-requests`, and `workflows` permissions.

2. **Activate the dev system** — from a clone of this repo, run one command:

   ```bash
   .genesis/scripts/activate.sh
   ```

   It reads the App ID, App private key, and Anthropic key from your `~/.config/genesis/.env` (shared across all your genesis projects), verifies the App is installed here, sets them as this repo's Actions secrets, and enables the workflows. It refuses to run if any value is missing/placeholder or the App isn't installed. The next trigger (an issue/PR/comment event, a push, or the cron) then wakes the orchestrator and onboarding begins on issue #1.

3. **(Optional) Observability & notifications** — set Loki credentials in `.genesis/config.toml` for logs, and configure the A2H gateway if you want Slack/email instead of GitHub-issue comms.

---

## Development

MaKlaude is written in **Go** (1.24+). The codebase follows the standard Go
project layout:

```
cmd/maklaude/      # CLI entrypoint (skeleton: builds, prints version/help)
internal/          # private packages
  version/         #   build/version metadata
  cluster/         #   cluster registry & config surface (see below)
  kube/            #   read-only Kubernetes client/transport
  health/          #   judgment-free snapshot collection (read-only)
  detect/          #   deterministic findings from a snapshot
  escalate/        #   human comms trail: issue-per-problem (see below)
```

## Comms trail & escalation

MaKlaude keeps humans informed through an **auditable comms trail** rather than
a stream of alerts. The `internal/escalate` package turns the deterministic
[`detect.Finding`](internal/detect/finding.go)s into exactly **one tracked
GitHub issue per active problem**, and keeps that issue in sync as the problem
recurs and clears.

The whole model hangs off the stable `detect.Identity` key (the same ongoing
problem yields the same identity every cycle):

- **One issue per problem.** A newly seen identity opens a single, well-formed
  issue: severity, cluster, the affected object, the human-readable message, and
  the detection time. The cluster is named in the title so multi-cluster setups
  stay legible at a glance.
- **Recurrence updates, never duplicates.** When the same problem is detected
  again on a later cycle, MaKlaude refreshes the existing issue's body and adds a
  recurrence comment — it does **not** open a second issue. This dedup is the
  core guarantee, and it is unit-tested without any network.
- **Clearance closes the trail.** When a previously-active problem is no longer
  in the current findings, its issue is closed with a closing comment, so the
  record stays complete and self-explanatory.
- **`needs:human` gating.** Warnings and criticals are labelled `needs:human`
  (in addition to the `maklaude` management label) to flag that a decision is
  wanted. Info-level findings are recorded but not gated. MaKlaude never takes a
  mutating action on a cluster — escalation is purely informational.
- **Restart-safe.** Each issue embeds its identity in a hidden marker
  (`<!-- maklaude:identity=… -->`). The escalator rediscovers which open issue
  maps to which problem by listing issues, so it stays correct even if the
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
`(findings, tracked issues)` — no I/O, no clock — and the GitHub interaction
sits behind the small `escalate.IssueSink` interface, so the interesting logic
is exhaustively unit-tested with a fake in-memory sink. The package touches
GitHub and **never** a Kubernetes cluster, keeping MaKlaude's read-only safety
boundary intact.

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
clusters — a later milestone turns a handle into a live Kubernetes client.

### Read-only access (RBAC)

MaKlaude only ever reads a cluster. The least-privilege RBAC bundle in
[`deploy/rbac/`](deploy/rbac/) grants its ServiceAccount exactly the
`get`/`list`/`watch` access the code needs and **no mutating verbs**. Apply it
with `kubectl apply -k deploy/rbac`. See [`docs/rbac.md`](docs/rbac.md) for the
full access model, how to mint a kubeconfig for the ServiceAccount and register
it above, and how to verify the access is truly read-only.

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

Try the CLI skeleton:

```bash
task build
./bin/maklaude version
./bin/maklaude help
```

---

*Bootstrapped by [Genesis](https://github.com/Sayfan-AI/genesis) — an autonomous agentic AI dev system.*
