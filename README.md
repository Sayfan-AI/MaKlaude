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

This repo runs autonomously via GitHub Actions, but **genesis ships those workflows disabled**. They authenticate as the Genesis GitHub App and call the Anthropic API, so running them before the credentials exist would just fail on every trigger. The repo and issue #1 already exist; the autonomous loop stays dormant until you complete these steps.

1. **Install the Genesis GitHub App** on this repository, granting it `contents`, `issues`, `pull-requests`, and `workflows` permissions.

2. **Set the required secrets** (run from a clone of this repo, or append `-R <owner>/MaKlaude`):

   ```bash
   gh secret set ANTHROPIC_API_KEY                          # your Anthropic API key
   gh secret set GENESIS_APP_ID                             # the Genesis GitHub App's ID
   gh secret set GENESIS_APP_PRIVATE_KEY < genesis-app.pem  # the App's private key (PEM)
   ```

3. **Activate the dev system** — enable the workflows once the App and secrets are in place:

   ```bash
   .genesis/scripts/activate.sh
   ```

   It checks the secrets are set, then enables all the workflows genesis disabled. The next trigger (an issue/PR/comment event, a push, or the cron) wakes the orchestrator and onboarding begins on issue #1.

4. **(Optional) Observability & notifications** — set Loki credentials in `.genesis/config.toml` for logs, and configure the A2H gateway if you want Slack/email instead of GitHub-issue comms.

---

*Bootstrapped by [Genesis](https://github.com/Sayfan-AI/genesis) — an autonomous agentic AI dev system.*
