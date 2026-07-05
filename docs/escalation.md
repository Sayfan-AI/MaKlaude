# MaKlaude escalation and comms trail

MaKlaude turns the incidents it diagnoses on a cluster into a durable, auditable, human-facing trail: **one GitHub issue per active incident**, carrying the whole diagnosis and kept in sync as incidents recur and clear. This is the "communicate" half of the system, and it is deterministic. There is no LLM here, only a stable mapping from the current set of diagnosed incidents to the set of issues that should exist.

The flow is one-directional and read-only toward Kubernetes:

```
cluster -> health -> detect (findings) -> correlate (incidents) -> diagnose (hypotheses) -> escalate (GitHub issues) -> human
```

Since M3/T4 the escalator works at **incident granularity**, not per raw finding: the `correlate` layer collapses a fan of symptoms into one incident, `diagnose` ranks *why* it happened, and `escalate` opens one issue for that incident carrying its ranked root-cause hypotheses, the evidence behind each, the affected objects, and **manual, read-only next steps**. `escalate` consumes only what those upstream read-only layers produced. It talks to GitHub and nothing else, so no code path here can mutate a cluster — and the issue body never claims MaKlaude will take any action (M3 is strictly diagnostic). See [no-writes.md](no-writes.md).

## The lifecycle: one issue per incident

Each incident carries a stable `correlate.IncidentIdentity` - the same key across cycles for the same incident, independent of its severity, messages, timestamps, or the shifting set of effect findings. A health monitor re-diagnoses an ongoing incident on every cycle, so the identity is what keeps MaKlaude from burying you in duplicate alerts.

| Lifecycle moment | Trigger | What MaKlaude does |
| ---------------- | ------- | ------------------ |
| **Escalation** (incident first seen) | a new incident identity | Opens one GitHub issue for it, labeled `maklaude` (plus `needs:human` when its severity warrants a decision), with the incident summary, the ranked root-cause hypotheses + evidence, the affected objects, diagnostic-only next steps, and a hidden identity marker |
| **Recurrence** (incident seen again) | an incident identity already tracked | Updates that same issue - a refreshed body (latest diagnosis) and a recurrence comment. It never opens a second issue. This is the load-bearing dedup guarantee |
| **Resolution** (incident cleared) | a tracked identity absent from the current incidents | Closes the issue with a closing comment, so the record stays auditable instead of silently vanishing |

When the leading hypothesis is low-confidence, the body says so and presents the competing hypotheses honestly, rather than overcommitting to one cause.

## State lives in the issue, not in memory

The monitor process can restart at any time, so an in-memory "which issue maps to which incident" table cannot be the source of truth. Instead the incident identity is embedded in the issue itself:

- **Identity marker** - a hidden HTML comment in the issue body, `<!-- maklaude:identity=... -->`, invisible on GitHub but parseable by MaKlaude.
- **`maklaude` label** - applied to every issue MaKlaude opens. A coarse filter that lets MaKlaude find its own escalations, and lets you tell them apart from issues a human opened by hand.

On each cycle the escalator lists the open `maklaude` issues, parses each marker, and reconciles against the current incidents. Reconciliation is therefore correct across separate process runs. The in-memory map is only a cache layered on top of that durable truth.

(When the optional Slack integration is on, the Slack thread handle is stored as a second hidden marker, `<!-- maklaude:thread=... -->`, in the same issue body, so a recurrence or resolution threads correctly even after a restart. See [slack.md](slack.md).)

## What MaKlaude will and won't touch

The escalator manages **only** issues that carry both the `maklaude` label and a parseable identity marker. An issue a human opened by hand is never touched, and neither is a mislabeled issue whose marker is missing or unparseable. If two open issues ever claim the same identity (a crash mid-open, or a colliding human issue), the trail self-heals: the first is updated and the rest are closed as duplicates, converging on exactly one issue per incident.

## The reconcile core is a pure function

The diff that decides open/update/close, `Reconcile(subjects, tracked) -> []Action` (where each *subject* is an incident plus its ranked diagnosis), performs no I/O, reads no clock, and never talks to GitHub or a cluster. Given the same inputs it returns the same plan in the same order (closes first, then opens and updates, each sorted by identity). All the interesting logic - dedup, clearance, severity changes, multi-cluster isolation - is kept side-effect-free so it can be exhaustively unit-tested. The side-effecting shell (list, create, comment, close) lives behind the `IssueSink` interface and is driven by the `Escalator`, which just executes whatever plan `Reconcile` returns.

## Configuration

The GitHub sink is configured entirely from the environment:

| Variable | Purpose |
| -------- | ------- |
| `MAKLAUDE_GITHUB_REPO` | `owner/repo` of the issue tracker to use as the trail |
| `MAKLAUDE_GITHUB_TOKEN` | a token with `issues:write` on that repo |

When those are unset, the sink degrades to a no-op and MaKlaude runs without a GitHub trail. MaKlaude sends no email of its own. For notifications it relies on **GitHub's own per-issue emails**: watchers, assignees, and label subscribers are emailed by GitHub when an issue is opened, commented on, or closed. That is the "GitHub + email" trail with zero extra setup.

For the full operator setup (RBAC, registering a cluster, turning the trail on), see [quickstart.md](quickstart.md).

## Where it lives in the code

Everything is under [`internal/escalate`](../internal/escalate):

- `escalate.go` - the model and types (`Action`, `TrackedIssue`), plus the package overview.
- `subject.go` - the `Subject` type: an incident plus its ranked diagnosis, the unit the escalator reconciles.
- `reconcile.go` - the pure `Reconcile` diff.
- `escalator.go` - the shell that executes the plan through the sink.
- `issue.go` - the labels (`maklaude`, `needs:human`), the identity/thread markers, and how an incident + diagnosis becomes an issue title, diagnostic body, and comments.
- `github.go` / `sink.go` - the `IssueSink` interface, the real `GitHubSink`, and the in-memory `MemorySink` used in tests.
