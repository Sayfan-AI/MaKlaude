# MaKlaude documentation

MaKlaude is an autonomous system for operating Kubernetes clusters on a human's behalf. It watches cluster health read-only, escalates problems as a durable GitHub-issue trail, and reaches you through the channels you configure, while every mutating action stays behind a human gate.

This is the map of the operator and architecture docs. If you just want to get MaKlaude running against a cluster, start with the quickstart and follow the reading order below.

## The docs

| Doc | What it covers |
| --- | -------------- |
| [architecture.md](architecture.md) | The two-layer posture: a deterministic Go product built and evolved by an AI dev system, with one optional gated LLM seam. Read this for the mental model. |
| [quickstart.md](quickstart.md) | Operator setup end to end: grant read-only access, register a cluster, run the monitor, and optionally route escalations to GitHub. **Start here to run it.** |
| [rbac.md](rbac.md) | The read-only access model, and how to grant and verify a least-privilege ServiceAccount for MaKlaude. |
| [no-writes.md](no-writes.md) | The four-layer guarantee that MaKlaude never mutates a cluster, and how to re-verify it yourself. |
| [escalation.md](escalation.md) | How detected problems become a comms trail: one GitHub issue per problem, keyed by identity, with escalation, recurrence, and resolution. |
| [slack.md](slack.md) | The optional Slack / ChatOps mirror of the escalation trail: threaded escalations, the `needs:human` mobile push, and inbound replies. |

## Suggested reading order

1. **[architecture.md](architecture.md)** - the two-layer shape (deterministic product, AI dev system) and where the one optional LLM seam sits.
2. **[quickstart.md](quickstart.md)** - get MaKlaude watching a cluster.
3. **[rbac.md](rbac.md)** and **[no-writes.md](no-writes.md)** - the safety model the quickstart leans on: least privilege going in, and the proof that nothing goes out.
4. **[escalation.md](escalation.md)** - how MaKlaude tells you what it found and keeps that trail honest as problems recur and clear.
5. **[slack.md](slack.md)** - only if you want a real-time, team-visible channel on top of the GitHub trail.

The optional, gated **LLM-assisted diagnosis** layer (read-only, redacted, cost-bounded, off by default) is documented in [architecture.md](architecture.md#the-one-optional-ai-seam) and the [README](../README.md#llm-assisted-diagnosis-optional-gated); its safety posture is summarized in [no-writes.md](no-writes.md).

For the code itself, the escalation model lives in [`internal/escalate`](../internal/escalate), the notification seam in [`internal/notify`](../internal/notify), deterministic diagnosis in [`internal/diagnose`](../internal/diagnose), and the optional LLM refinement in [`internal/aidiagnose`](../internal/aidiagnose).
