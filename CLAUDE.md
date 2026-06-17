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

### Testing / CI
- **Local kind on macOS is NOT a faithful proxy for CI on Linux — CI is the source of truth.** macOS Docker Desktop remaps bind-mount ownership to the host user, so a root-written file (e.g. the apiserver audit log) is readable locally but `root:0600` and unreadable by the non-root runner on Linux CI. A green local e2e gave false confidence; the bug existed only on Linux. Two rules, both applied: (1) optional test corroboration must **degrade gracefully** (warn + skip), never hard-fail, when the primary proof already holds; (2) when CI must read an apiserver-written file, `chmod` it readable in the workflow. See `test/e2e/e2e_test.go` (`assertNoMutatingAudit`) + `.github/workflows/e2e.yml`.
