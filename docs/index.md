# MaKlaude documentation

MaKlaude is an autonomous system for operating Kubernetes clusters on a human's behalf. It watches cluster health read-only, escalates problems as a durable GitHub-issue trail, and reaches you through the channels you configure, while every mutating action stays behind a human gate **by default**. There are two ways to run it unattended, both off by default and deliberately unalike: a blanket bypass that waives review and records every action it waives as unreviewed ([autonomous-mode.md](autonomous-mode.md)), and *earned* autonomy — a per-shape rule that fires only once a recorded history of human approvals says it may, and that discloses every action it takes ([unattended-actions.md](unattended-actions.md)).

This is the map of the operator and architecture docs. If you just want to get MaKlaude running against a cluster, start with the quickstart and follow the reading order below.

## The docs

| Doc | What it covers |
| --- | -------------- |
| [architecture.md](architecture.md) | The two-layer posture: a deterministic Go product built and evolved by an AI dev system, with one optional gated LLM seam. Read this for the mental model. |
| [quickstart.md](quickstart.md) | Operator setup end to end: grant read-only access, register a cluster, run the monitor, and optionally route escalations to GitHub. **Start here to run it.** |
| [rbac.md](rbac.md) | The access model: the read-only ServiceAccount MaKlaude observes with, the two separate optional identities beside it — one that can execute three approved actions, one that can only create and delete chaos experiments — and how to grant and verify each. |
| [no-writes.md](no-writes.md) | The four-layer guarantee that MaKlaude's **observation** path never mutates a cluster, what that guarantee does and does not cover now that a write path exists, and how to re-verify it yourself. |
| [remediation.md](remediation.md) | The gated-write seam end to end: the closed four-action catalog, the five independent gates, the `kube.ExecuteMode` kill switch, the scoped single-request write, rollback, and the audit trail. |
| [chaos.md](chaos.md) | The deliberate write path (Milestone 6): MaKlaude breaking a cluster on purpose through Chaos Mesh, on clusters a human explicitly marked eligible. Covers the three gates, the third identity, the closed fault catalog, what a create is conditioned on when there's no `resourceVersion` to condition it on, and — stated plainly — what the RBAC does *not* bound. |
| [autonomous-mode.md](autonomous-mode.md) | The approval bypass: exactly what `MAKLAUDE_DANGEROUSLY_AUTO_APPROVE` gives up, what it emphatically does not, and why `MAKLAUDE_GITHUB_SELF_LOGIN` became mandatory alongside it. **Read before running MaKlaude unattended.** |
| [unattended-actions.md](unattended-actions.md) | The other way to run unattended, and the narrow one: *earned* autonomy, scoped per cluster/namespace/operation and granted only by a recorded history. Covers how to enable it (and why half-enabling it refuses to start), what "earned" concretely requires, the caps/cooldown/breaker it runs under, the five scopes of revocation, the one-issue-per-action disclosure trail, and how the trust ledger is rebuilt from the artifacts. The rules-file format is [`autonomy.example.yaml`](../autonomy.example.yaml). |
| [escalation.md](escalation.md) | How detected problems become a comms trail: one GitHub issue per problem, keyed by identity, with escalation, recurrence, and resolution. |
| [slack.md](slack.md) | The optional Slack / ChatOps mirror of the escalation trail: threaded escalations, the `needs:human` mobile push, and inbound replies. |

## Suggested reading order

1. **[architecture.md](architecture.md)** - the two-layer shape (deterministic product, AI dev system) and where the one optional LLM seam sits.
2. **[quickstart.md](quickstart.md)** - get MaKlaude watching a cluster.
3. **[rbac.md](rbac.md)** and **[no-writes.md](no-writes.md)** - the safety model the quickstart leans on: least privilege going in, and the proof that nothing goes out.
4. **[escalation.md](escalation.md)** - how MaKlaude tells you what it found and keeps that trail honest as problems recur and clear.
5. **[slack.md](slack.md)** - only if you want a real-time, team-visible channel on top of the GitHub trail.
6. **[remediation.md](remediation.md)** - only when you want MaKlaude to *fix* things and not just report them. It is the counterpart to no-writes.md: what the write path can do, and every gate it has to pass first.
7. **[chaos.md](chaos.md)** - only if you want MaKlaude to break a cluster on purpose so its behaviour under fault is measured rather than assumed. Read it after remediation.md, since it reuses that path's scope guard and kill switch and adds a third identity beside it.
8. **[autonomous-mode.md](autonomous-mode.md)** - only if you want MaKlaude to act without asking. It is the one page that describes a safety property being deliberately switched off, so it is last for a reason: nothing else here assumes you have read it, and you should not read it first.
9. **[unattended-actions.md](unattended-actions.md)** - the same question answered narrowly instead of bluntly. Read it after autonomous-mode.md, because its whole point is the contrast: a blanket switch that cites nothing, against a per-shape rule that must cite the history that earned it and that one label revokes.

The optional, gated **LLM-assisted diagnosis** layer (read-only, redacted, cost-bounded, off by default) is documented in [architecture.md](architecture.md#the-one-optional-ai-seam) and the [README](../README.md#llm-assisted-diagnosis-optional-gated); its safety posture is summarized in [no-writes.md](no-writes.md).

For the code itself, the escalation model lives in [`internal/escalate`](../internal/escalate), the notification seam in [`internal/notify`](../internal/notify), deterministic diagnosis in [`internal/diagnose`](../internal/diagnose), and the optional LLM refinement in [`internal/aidiagnose`](../internal/aidiagnose).
