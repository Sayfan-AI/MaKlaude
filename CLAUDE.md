# MaKlaude

## Goal

Build MaKlaude — an autonomous system for operating Kubernetes clusters on a human's behalf.

MaKlaude continuously watches the health of one or more clusters a human has put under its care: it detects problems, diagnoses root causes, and safely fixes what it confidently can. Anything risky or destructive it does NOT do on its own — it escalates to a human with enough context to decide, and acts only once approved. Throughout, it keeps humans informed through whatever channel they prefer (Slack, email, GitHub, etc.) so there's always a clear, auditable trail of what it saw, what it did, and what it's waiting on.

Guiding principles, not a blueprint — you decide the actual architecture, agents, and tools:
- Safety first. Read/diagnose freely; gate every mutating or destructive action behind explicit human approval until trust is earned. Least privilege everywhere.
- Multi-cluster from the start. A human can register several clusters; MaKlaude operates them without cross-contamination.
- Extensible. New operational capabilities (e.g. security/vulnerability scanning, cost and capacity awareness, GitOps-aware remediation) should be addable over time without redesign.
- Human-in-the-loop, not human-replaced. MaKlaude augments operators; it never silently takes irreversible action.

Important boundary: humans configure which clusters MaKlaude monitors and operates, and supply the credentials/access. Building that configuration surface and the operational system is your job; standing it up against real clusters is the human's job once it's built.

Treat the well-known "multi-agent Kubernetes DevOps" pattern (a coordinator delegating to specialized analyze / remediate / communicate roles) as inspiration only — feel free to surpass it. Aim higher than a minimal demo: build something an operator would actually trust with real clusters.




## Meta-Concepts

These are the principles this dev system operates by. Evolve them as the project matures.

- **GitHub as coordination layer** — issues track progress, PRs deliver changes, CI/CD enforces quality. Humans and agents speak the same protocol.
- **Quality gates and e2e testing** — code, tests, CI/CD, deployment are all first-class concerns.
- **Self-improvement** — continuously evolve agents, skills, and strategies.
- **Self-monitoring** — monitor progress, detect stuck/looping states, try to unblock, escalate to human when stuck.
- **Minimal human-in-the-loop** — do everything possible autonomously. Highlight what requires human action and offer to do it if given access.
- **Deterministic over agentic** — if a task is well-understood and doesn't need LLM judgment, build a deterministic tool (script, CLI, CI step). Reserve LLMs for fuzzy reasoning.
- **Incremental planning** — only detail the current milestone. Future milestones stay high-level until they're next.

## Agent Roster

- **Onboarding** — refines goal with human, produces milestones (runs once at project start)
- **Project manager** — owns roadmap, tracks progress, drills down current milestone into tasks
- **Human interaction** — all comms with user (reports, escalations, access requests). Speaks A2H protocol.
- **Evolver** — evolves the dev system itself (new agents, tools, skills, memory design, CLAUDE.md refinement). Escalates framework-level improvements to genesis.
- **Health / self-review** — monitors for stuck/looping, audits quality
- **Workers** — designed by the dev system for the specific goal

## Execution Model

GitHub Actions serve as the trigger layer:
- **Scheduled workflows** (cron) — periodic advancement of project state
- **Event-triggered workflows** — issue/PR events, human feedback, comments

Each trigger launches a Claude Agent SDK session as the orchestrator.

## Tech Stack Preferences

Defaults (override as needed):

- **Open source + free tier only**
- **Backend:** Rust (Go if K8s-heavy)
- **CLI:** Rust
- **Frontend:** Vite + React + TanStack Router + TanStack Query, Tailwind CSS, TypeScript (strict)
- **Desktop:** Tauri
- **Mobile:** React Native (Expo)
- **Internal services:** gRPC
- **Auth:** Ory stack (K8s), Rust crates (simple apps), Clerk (managed fallback)
- **Observability:** OpenTelemetry + Grafana Cloud free tier
- **Database:** Neon (serverless Postgres)
- **Deployment:** Cloudflare, cloud free-tier
- **Local dev:** Tilt + kind (K8s), LocalStack (AWS)

## Learnings

Curated memory — non-obvious lessons so future sessions don't relearn them. The evolver maintains this.

### Health detection
- **Pod container state oscillates; don't detect crashloop on instantaneous state.** A crashlooping pod cycles `Waiting(CrashLoopBackOff)` → `Running` → `Terminated(Error)` → back, so a point-in-time scan keyed only on `state.waiting.reason == "CrashLoopBackOff"` misses it about half the time — and the same race bites a real interval scan, not just the e2e. Detect on a *stable* signal: instantaneous `CrashLoopBackOff` **OR** `RestartCount >= 2` with a non-zero last/current termination. See `internal/health/collector.go` (`containerCrashLooping`) + regression test `TestCollect_CrashLoopMidCycle`.

### Dev system
- **Agent turn budgets fail silently, and the floor must move for the CLASS, not the runner that failed.** A Claude-invoking workflow with too low a `--max-turns` doesn't error usefully — it dies at `error_max_turns`, and the only visible trace is a safety-net escalation issue. This has now bitten four times: evolver at 20 (#38), events orchestrator at 20 (#81, died mid-task on #78/PR #79), evolver at 30 (#85, died with *no* artifact — no branch, PR, issue, or comment), scheduled orchestrator at 40 (#87, died at turn 41 seconds after opening PR #86 — deliverable escaped, bookkeeping truncated). Each of the first three fixes raised only the runner that happened to fail, so the class floor stayed put and the next-tightest runner failed next. **Generalizable rule: when a shared failure mode recurs on a different member each time, raise the guard for the shape, not the instance.** Floors now: orchestrator-class (open-ended agent definition — assess, read issues/PRs, dispatch subagents, complete a unit of work) **40 minimum**; narrow fixed-procedure agents like auto-merge keep a deliberately small budget (15) so needing more turns fails fast. Enforced by `test/devsystem/workflows_test.go` via named constants, which also fails on a Claude-invoking workflow that isn't classified. Two corollaries: (1) a run that dies at max-turns *may still have landed its work* — check for a pushed branch/PR before treating the escalation as "nothing happened" (#87's fix was merged by auto-merge while the escalation sat open); (2) budgets are a symptom — the open question is whether an orchestrator should implement inline at all, versus dispatching a worker and spending its own turns only on coordination (#89).
- **A correctly-waiting run is still a stalled project — gate staleness must be deterministic.** The plan/completion gates' failure mode is *silent indefinite stalling*, and it's the one failure mode no safety net covered: `automation:failure` catches runs that **die**, nothing catches runs that **succeed at doing nothing**. Milestone 4's plan gate (#76) sat `needs:human` for 21 days across ~85 scheduled ticks; every run correctly obeyed "if a `needs:human` plan issue is open, do nothing and wait", so every run did nothing, said nothing, and left no trace. `main` was green the whole time. The guideline "escalate if stuck more than 2 cycles" never fired because "stuck" required an LLM to compare `createdAt` against today and *choose* to care — it finally went out on day 21 by chance. "Is this gate older than N days" needs no judgment: `issues.sh gates`/`stale-gates` report age, `summary` always prints a **Human Gates** section, and `nudge-gates.sh` posts exactly one nudge per gate (marker label `nudged:stale`), wired **before** the agent step in `genesis-orchestrator.yml` so the signal escapes even if the agent dies at max-turns. Guarded by `test/devsystem/stalegates_test.go`. Generalizable rule: **when a hard rule tells an agent to do nothing, something deterministic must still measure how long it's been doing nothing.**
- **An opt-in invariant is not an invariant — test the membership, not the member.** Serializing the repo-mutating agents is done with a shared `concurrency.group`, which every workflow must opt into individually. The evolver never did, so two Claude sessions ran fully in parallel on this repo — the exact "two orchestrators duplicating work" case the group exists to prevent. It held for the entire sampled history (15/15 days; 2026-07-27 the two runs started **2 seconds** apart) and produced no failure, no red CI, and no escalation, because parallel agents don't error — they just race on pushes, issues and PRs. The trigger was a *cron collision*: *GitHub dispatches all of a repo's crons that share one expression in the same batch and then delays that batch by the same 1.5–3.5h*, so `0 6 * * *` (evolver) and `0 */6 * * *` (orchestrator) landed together to the second. Writing a different hour is what decorrelates them; both properties are now guarded by `test/devsystem/concurrency_test.go` (group membership per workflow + no two genesis crons sharing a firing minute). Two things worth knowing: (1) `cancel-in-progress: false` still lets GitHub cancel an *older pending* run when a newer one queues into the group, and a run cancelled while pending never starts a job — so it cannot trip `if: failure()` and is invisible; staggering the cron is what keeps that window small. (2) **Generalizable rule: when a safety property depends on every member of a set opting in, the thing to test is the set.** Same shape as the turn-budget floors above — an unclassified workflow fails the test rather than silently defaulting to unsafe.
- **Auto-merge is bot-only on purpose; the orchestrator merges human PRs.** `genesis-merge.yml` merges only `genesis-dev-bot[bot]` PRs. The repo is public, so `pull_request` fires for fork PRs from anyone — auto-merging "any green PR" would let an arbitrary contributor land on main unreviewed. Accepted consequence: a **human-authored** PR is not auto-merged. The orchestrator merges it on the run that follows (the PR event, or the next scheduled tick) after verifying checks are green and no `needs:human` label is set; if the diff is large, ambiguous, or safety-critical, it comments, labels `needs:human`, and stops instead. So human PRs land one run late by design — that lag is not a bug.

### Testing / CI
- **Local kind on macOS is NOT a faithful proxy for CI on Linux — CI is the source of truth.** macOS Docker Desktop remaps bind-mount ownership to the host user, so a root-written file (e.g. the apiserver audit log) is readable locally but `root:0600` and unreadable by the non-root runner on Linux CI. A green local e2e gave false confidence; the bug existed only on Linux. Two rules, both applied: (1) optional test corroboration must **degrade gracefully** (warn + skip), never hard-fail, when the primary proof already holds; (2) when CI must read an apiserver-written file, `chmod` it readable in the workflow. See `test/e2e/e2e_test.go` (`assertNoMutatingAudit`) + `.github/workflows/e2e.yml`.
