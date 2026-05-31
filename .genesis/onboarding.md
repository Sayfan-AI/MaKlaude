# Onboarding: MaKlaude

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




## Instructions

This is the onboarding issue — the one-time handoff from goal to roadmap. The onboarding agent (via the human interaction agent) should:

1. Review the goal above and ask the human clarifying questions until the goal is well understood.
2. Break the goal into high-level milestones, each with clear done criteria. Detail only milestone 1's intent — keep later milestones high-level (incremental planning).
3. Record the agreed milestone roadmap in this issue (description or a comment) so it persists after the issue is closed.
4. Label this issue `needs:human` and **STOP**. Do NOT plan tasks, create task issues, or start any work.

Onboarding is complete when **the human closes this issue** — that close is the human's approval of the roadmap. After it closes, the orchestrator picks up milestone 1 through the standard milestone-plan gate: it proposes the milestone 1 task breakdown in a `Milestone 1 plan` issue (`needs:human`) and waits for the human to approve that too before any work begins.
