# Architecture posture - deterministic product, AI dev system

MaKlaude is two layers, and it's worth being explicit about which is which. The distinction is the whole point of how it's built, and it's what makes it safe to run against real clusters.

## The two layers

- **The running product is deterministic Go.** The operational path - collect a read-only snapshot, `detect` findings, `correlate` them into incidents, `diagnose` ranked root causes, `escalate` to the comms trail - is rule-based code with no model in it. The same code runs in the unit tests, the `kind` end-to-end test, and production.
- **The AI is the dev system that builds and evolves MaKlaude, not MaKlaude itself.** An orchestrator, workers, an evolver, and a human-interaction agent (Claude Code, driven by GitHub Actions) plan the milestones, write the code, review it behind quality gates, and improve the system over time. That is where the LLMs live.

The practical upshot for an operator: what runs against your clusters is deterministic and auditable. The intelligence was spent at build time, not wired into the hot path.

## The operational path is deterministic

`maklaude scan` runs one read-only pass per registered cluster. Every stage is deterministic rule code over a snapshot:

| Stage | Package | What it does | Model in the path? |
| ----- | ------- | ------------ | ------------------ |
| Collect | `internal/health` | Read-only snapshot of cluster state | No |
| Detect | `internal/detect` | Turn the snapshot into typed findings by rule | No |
| Correlate | `internal/correlate` | Group related findings into one incident (root cause + effects) | No |
| Diagnose | `internal/diagnose` | Rank root-cause hypotheses by rule, most-confident-first | No |
| Escalate | `internal/escalate` | Reconcile incidents into the issue-per-problem comms trail | No |

`diagnose.Diagnose(snap, incident)` is a pure function: no I/O, no clock, no cluster access, no LLM. Given the same inputs it always returns the same ranked hypotheses in the same order. That is why the same code can back the unit tests, the `kind` e2e, and production unchanged. The read-only guarantee that wraps all of this is documented in [no-writes.md](no-writes.md) and [rbac.md](rbac.md).

## The gated-write seam

Milestone 4 added the ability to *act*, and it is a seam in the same sense the AI one is: a separate boundary with its own identity, its own client type, and its own gates, deliberately not an extension of the path above.

| Stage | Package | What it does | Model in the path? | Can it change a cluster? |
| ----- | ------- | ------------ | ------------------ | ------------------------ |
| Propose | `internal/remediate` | Turn a diagnosed root cause into typed, previewable proposals by rule | No | No |
| Approve | `internal/approve` | Publish a proposal and wait for an attributable decision | No | No |
| Execute | `internal/execute` | Re-check preconditions, send exactly one request, watch for convergence | No | Yes |
| Audit | `internal/audit` | Append one immutable record per lifecycle event | No | No |

Two structural properties carry the safety, and both are about types rather than discipline:

- **The write path is a sibling of the read path, not a mode of it.** `kube.Client` and `kube.Executor` build their `rest.Config`s through different functions that install different transport guards. The observation path's guard is unconditional and unparameterised, so nothing an operator does to enable execution — and no future refactor of the write path — can loosen it.
- **Off by default is a posture the binary holds, not a flag it reads.** `kube.ExecuteMode`'s zero value is `disabled`, under which `kube.NewExecutor` refuses to build anything at all: a deployment that has not opted in holds no write-capable object, because none was ever constructed. The write path is reachable from one command (`maklaude remediate`) and only once an operator sets `MAKLAUDE_EXECUTE_MODE`; `maklaude scan` cannot reach it under any argument.

Beyond those, an action needs a separately-installed RBAC bundle bound to a separate ServiceAccount, an `approved` label event from an identity MaKlaude cannot forge, preconditions that still hold against a fresh read, and a matching `resourceVersion`. The seam stays deterministic throughout: proposals are computed by rule, and no model participates in deciding what to change or whether to change it. See [remediation.md](remediation.md).

## The unattended seam

Milestone 5 added a sixth condition on top of those five and removed none of them: an action whose shape a recorded history of human approvals has **earned** can run without a person. It is off by default — no rule exists until an operator writes one — and it is four packages, each of which deliberately refuses to reach past its own concern:

| Stage | Package | What it does | Model in the path? |
| ----- | ------- | ------------ | ------------------ |
| Decide | `internal/autonomy` | Answers auto-apply / gate / refuse for **one** proposal. A pure function — no client, no file, no clock, no environment | No |
| Earn | `internal/trust` | Derives whether a fix has earned autonomy, from a durable history of recorded executions. Promotion is scoped to the fix's fingerprint; demotion is scoped to its `(cluster, operation)` shape | No |
| Bound | `internal/budget` | Caps auto-applies per cluster per pass, cools down a target, and trips a per-cluster circuit breaker | No |
| Disclose | `internal/disclose` | Opens one GitHub artifact per unattended action *before* it runs, and reads the label that revokes a shape | No |

`internal/rules` turns the operator's file into the ruleset, and `internal/operate` owns the one thing none of the four would touch: the order they go in. Two properties are worth carrying over from the gated seam:

- **The decision is deterministic and has no model in it.** Identical inputs produce an identical verdict, including the reason token and the rule name. A decision to mutate a cluster with nobody watching is exactly the wrong place for a probabilistic component, so there is no LLM anywhere in this path.
- **Trust is derived, never declared.** There is no config key that asserts a shape is trustworthy — the honest version of that is `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE`, which says so in its name. So on day one nothing is trusted and everything gates. See [unattended-actions.md](unattended-actions.md).

## The deliberate-break seam

Milestone 6 added the ability to *break a cluster on purpose*. It is the project's first write path that is not remediation, and it is a seam in the same sense as the two above: its own package, its own ServiceAccount, its own eligibility gate. What it deliberately is **not** is a second decision-maker — it reuses the gated seam's scope guard and kill switch, and it asks the unattended seam for permission rather than judging for itself.

| Stage | Package | What it does | Model in the path? | Can it change a cluster? |
| ----- | ------- | ------------ | ------------------ | ------------------------ |
| Mark | `internal/cluster` | Turns a human-written per-cluster eligibility marker into a `ChaosTarget` capability token. An unmarked cluster mints none, and a token cannot be forged or copied onto another cluster | No | No |
| Ask | `internal/chaos` (`proposal.go`) | Turns a requested fault into a `Proposal` that answers exactly what `autonomy.DecideRequest` and `budget.Admit` ask | No | No |
| Aim | `internal/kube` (`chaosscope.go`) | Builds the write-capable config from a token, admitting exactly one request shape and refusing every mutating scope outside `chaos-mesh.org` | No | No |
| Inject | `internal/chaos` (`injector.go`) | Creates and deletes the Chaos Mesh custom resource, conditioned on a name it derives since a create has no `resourceVersion` | No | Yes |
| Reap | `internal/chaos` (`reaper.go`) | Sweeps experiment objects a killed run left behind, one at a time, each conditioned on its UID | No | Yes |

Two properties carry the safety here, and both are the same shape as the gated seam's:

- **Eligibility is a capability, not a check.** A `ChaosTarget` is the only `internal/cluster` type that `internal/chaos`'s exported signatures admit, so there is no way to express "inject into this cluster handle" — the compiler rejects the sentence. That is what makes "a chaos write cannot reach an unmarked cluster" a property of the package rather than of a conditional somebody has to remember to write.
- **The narrowing is stated, not disguised.** MaKlaude as a whole no longer promises "no mutating verb"; it promises "no mutating verb except chaos CRDs, on chaos-eligible clusters." The observation identity's guarantee is untouched and separately proven. [no-writes.md](no-writes.md#the-milestone-6-exception-and-what-holds-it-in-place) has both claims side by side, with what enforces each and the four things neither promises. See [chaos.md](chaos.md).

## The one optional AI seam

There is exactly one place a model can run at runtime: `internal/aidiagnose` (Milestone 3, T5). It can call a model to *refine* a diagnosis - sharpen a low-confidence hypothesis, or propose a cause the rules cannot express - for the cases the deterministic rules handle poorly. It is a strict, isolated safety boundary:

- **Off by default.** It runs only when a human sets `MAKLAUDE_LLM_DIAGNOSIS` plus an API key. The deterministic core ships and runs fully without it.
- **Read-only by construction.** The provider seam turns a redacted text prompt into text suggestions. It holds no cluster client and no mutating capability, so an LLM can *inform* a diagnosis but can never act on a cluster.
- **Redacted, bounded, degrade-safe.** Evidence is redacted before egress and size-capped, calls are token-capped, deadline-bounded, and budget-capped, and any failure (disabled, over budget, timeout, error, even a panic) degrades to the deterministic hypotheses unchanged.

So even with the seam enabled, an LLM can only sharpen or add a hypothesis on top of a diagnosis that is already correct without it. See the [README's LLM-assisted diagnosis section](../README.md#llm-assisted-diagnosis-optional-gated) for the operator-facing detail.

## Why the split matters

This is the "deterministic over agentic" principle made literal: an LLM earns a seam only where fuzzy judgment actually helps, and everything else stays deterministic by construction. The operational core is provable, testable, and auditable precisely because there is no model in it. And the line keeps moving that way over time - one of the evolver's jobs is to compile stable agentic patterns down into deterministic scripts, so the system trends toward *less* LLM-in-the-loop as it matures, not more.
