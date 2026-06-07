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

*Bootstrapped by [Genesis](https://github.com/Sayfan-AI/genesis) — an autonomous agentic AI dev system.*
