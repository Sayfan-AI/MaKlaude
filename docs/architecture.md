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

## The one optional AI seam

There is exactly one place a model can run at runtime: `internal/aidiagnose` (Milestone 3, T5). It can call a model to *refine* a diagnosis - sharpen a low-confidence hypothesis, or propose a cause the rules cannot express - for the cases the deterministic rules handle poorly. It is a strict, isolated safety boundary:

- **Off by default.** It runs only when a human sets `MAKLAUDE_LLM_DIAGNOSIS` plus an API key. The deterministic core ships and runs fully without it.
- **Read-only by construction.** The provider seam turns a redacted text prompt into text suggestions. It holds no cluster client and no mutating capability, so an LLM can *inform* a diagnosis but can never act on a cluster.
- **Redacted, bounded, degrade-safe.** Evidence is redacted before egress and size-capped, calls are token-capped, deadline-bounded, and budget-capped, and any failure (disabled, over budget, timeout, error, even a panic) degrades to the deterministic hypotheses unchanged.

So even with the seam enabled, an LLM can only sharpen or add a hypothesis on top of a diagnosis that is already correct without it. See the [README's LLM-assisted diagnosis section](../README.md#llm-assisted-diagnosis-optional-gated) for the operator-facing detail.

## Why the split matters

This is the "deterministic over agentic" principle made literal: an LLM earns a seam only where fuzzy judgment actually helps, and everything else stays deterministic by construction. The operational core is provable, testable, and auditable precisely because there is no model in it. And the line keeps moving that way over time - one of the evolver's jobs is to compile stable agentic patterns down into deterministic scripts, so the system trends toward *less* LLM-in-the-loop as it matures, not more.
