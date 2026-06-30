# MaKlaude Slack / ChatOps integration

MaKlaude can optionally mirror its escalation trail into Slack so operators get
real-time, threaded notifications and (in a later task) can converse with it in
chat. Slack is **strictly optional and comms-only**: it notifies and converses,
it never gains any cluster-mutating capability. With no Slack configuration,
MaKlaude behaves **exactly** as it did in Milestone 1 — GitHub issues plus
GitHub's own per-issue notification emails — with zero behavior change. This is
the same graceful-degradation seam the GitHub trail uses (see the
[no-writes guarantee](no-writes.md) and [`internal/escalate`](../internal/escalate)).

> **Status.** As of M2 T4 **both directions are live**. **Outbound** (T3): a
> configured deployment posts each escalation as a thread root and replies
> recurrences/resolutions into that **same** thread over the Slack Web API
> (`chat.postMessage`), with durable cross-restart continuity — see
> [`internal/notify/slack_notifier.go`](../internal/notify/slack_notifier.go) and
> [`internal/escalate/escalator.go`](../internal/escalate/escalator.go).
> **Inbound** (T4): a human reply in an escalation thread is captured, resolved
> back to the incident/issue/cluster it belongs to (reusing the same durable
> thread mapping), and **mirrored onto the backing GitHub issue** as a comment so
> the audit trail records the full two-way conversation — see
> [`internal/notify/slack_inbound.go`](../internal/notify/slack_inbound.go) and
> [`internal/escalate/mirror.go`](../internal/escalate/mirror.go). The default
> inbound transport is **Socket Mode**; an optional **HTTP Events API** transport
> is also supported and **every HTTP request is signature-verified** before it is
> parsed. An unconfigured deployment still degrades to the no-op with zero behavior
> change versus Milestone 1.
>
> **Safety (locked):** inbound is strictly read / notify / converse. A captured
> reply only ever becomes a GitHub comment — there is **no code path** from an
> inbound event to a cluster mutation or any actionable behavior. Anything
> actionable still routes through MaKlaude's existing human gates.

## How notifications work

MaKlaude models each problem as a conversation, keyed by the same stable
*identity* the GitHub escalator uses to dedup an active issue across cycles:

| Lifecycle step | `Notifier` method | What the live backend does |
| -------------- | ----------------- | ----------------------------- |
| Problem first escalated | `NotifyEscalation` | Post a top-level message — the thread root — linking back to the GitHub issue, and return its `thread_ts`. A **needs:human** escalation also @-mentions the configured operator so a mobile push fires |
| Problem recurs / changes | `NotifyUpdate` | Reply into the same thread (using the recovered `thread_ts`) |
| Problem clears | `NotifyResolution` | Post a closing reply into the same thread |

### needs:human ⇒ operator @-mention (mobile push)

When an escalation warrants a human decision (the `needs:human` gate — warning or
critical severity, the same gate that labels the GitHub issue), the thread root
**@-mentions the operator** configured in `MAKLAUDE_SLACK_OPERATOR`. Slack treats
a direct mention as a notification, so the operator gets a real ping — including a
**mobile push** — rather than a message sitting silently in a channel. Info-level
escalations are recorded without a mention. When no operator is configured the
post simply carries no mention (no behavior change). The mention is purely a
notification; it triggers **no** action.

## Inbound: replies understood in context

A human reply in an escalation thread is **captured and mirrored onto the backing
GitHub issue**, so the audit trail records both sides of the conversation. The
inbound listener:

1. Receives the Slack `message` event (Socket Mode by default, or the optional
   HTTP Events API). It ignores anything that is not a fresh human reply inside a
   thread — thread roots, MaKlaude's own bot posts (so nothing echoes back),
   message edits/deletes, and top-level messages are all dropped.
2. Resolves the reply's `thread_ts` back to the incident/issue/cluster using the
   **same durable thread marker** the outbound side persists (`ParseThreadMarker`),
   so inbound and outbound agree on which conversation is which — even across a
   monitor restart.
3. Posts the reply as a **comment on the matching issue**, attributed to its Slack
   author. A reply whose thread maps to no open issue is a best-effort no-op (never
   an error), so an out-of-band reply never disrupts the listener.

This is strictly **read / notify / converse**: a captured reply only ever becomes
a GitHub comment. There is **no path** from an inbound event to a cluster
mutation; anything actionable still routes through MaKlaude's existing human gates.

### HTTP Events API: request signatures are verified

If an operator runs the optional HTTP transport instead of Socket Mode, **every
request is signature-verified before it is parsed or mirrored**. MaKlaude computes
`HMAC-SHA256("v0:{timestamp}:{body}", signing_secret)` and compares it in constant
time to the `X-Slack-Signature` header, also rejecting any request whose
`X-Slack-Request-Timestamp` is more than five minutes old (replay protection). A
missing, mis-signed, or stale request is rejected with `401` and never reaches the
issue trail. The signing secret is used only to verify and is never logged. Slack's
one-time URL-verification handshake is answered after the signature check.

### Durable, cross-restart thread continuity

The full lifecycle of one problem stays in **one** Slack thread — no duplicate
threads on recurrence — and this holds **even across a monitor restart**. The
thread handle is stored where the rest of MaKlaude's state already lives: the
**backing GitHub issue**, the auditable source of truth. No new datastore is
introduced.

1. On first escalation, `NotifyEscalation` posts the root and **returns** Slack's
   `thread_ts`. The escalator writes it into the issue body as a second hidden
   marker (`<!-- maklaude:thread=… -->`), alongside the existing identity marker.
2. On every later reconcile the escalator re-lists open issues and **recovers**
   that `thread_ts` from the marker (`ParseThreadMarker`), regardless of whether
   the process has restarted.
3. The recovered handle is passed back to `NotifyUpdate` / `NotifyResolution`, so
   the recurrence and the resolution reply into the **original** thread.

Because continuity is owned by the escalate/scan layer (the only layer that can
see both the issue store and the notifier), `notify` never needs to import
`escalate` — there is no import cycle. If a thread handle cannot be recovered (an
issue opened before this feature existed, or Slack reachable only after the root
was lost), the notifier **degrades gracefully**: it posts a self-labelled
top-level message rather than erroring, so a notification is never dropped and a
reconcile is never failed. The Slack side is strictly **best-effort** — a Slack
error is recorded but never strands the GitHub trail.

## Connection model

The approved default uses two Slack tokens:

- **Socket Mode (inbound)** — an app-level token (`xapp-…`) opens an outbound
  WebSocket, so MaKlaude needs **no public HTTP endpoint** and no inbound
  firewall changes. This is the default and recommended inbound transport.
- **Web API (outbound)** — a bot token (`xoxb-…`) authenticates message posting
  and threaded replies.

A **signing secret** is only needed if an operator chooses to run the HTTP
Events API instead of Socket Mode; it is therefore optional and not part of the
minimum configuration.

## Configuration (environment variables)

Like the GitHub trail, Slack is configured entirely from runtime environment
variables. Secrets are **operator-supplied at runtime, never committed to the
repo, and never logged** (MaKlaude redacts them before any diagnostic output —
see [`internal/notify/slack.go`](../internal/notify/slack.go)).

| Variable | Required | Description |
| -------- | -------- | ----------- |
| `MAKLAUDE_SLACK_BOT_TOKEN` | yes | Bot token (`xoxb-…`) for outbound Web API posts. **Secret — never logged.** |
| `MAKLAUDE_SLACK_APP_TOKEN` | yes | App-level token (`xapp-…`) for Socket Mode inbound. **Secret — never logged.** |
| `MAKLAUDE_SLACK_CHANNEL` | yes | Target channel for escalation threads — a channel ID (`C0123456789`) or `#name`. Not a secret. |
| `MAKLAUDE_SLACK_OPERATOR` | no | Operator to @-mention on `needs:human` escalations so a mobile push fires — a user ID (`U0123456789`), a user-group ID (`S0123456789`), or a literal `<…>` mention token. Not a secret. When unset, escalations post without a mention. |
| `MAKLAUDE_SLACK_SIGNING_SECRET` | no | Signing secret to verify inbound HTTP Events API requests; only needed if you run HTTP mode instead of Socket Mode. **Secret — never logged.** |

When the three required variables are all set, MaKlaude considers Slack
**configured** ([`SlackConfig.Configured`](../internal/notify/slack.go)). When
any is missing, it falls back to the no-op notifier and the GitHub + email trail
is unchanged.

## Set up from scratch

Everything an operator needs to stand this up lives in [`deploy/slack/`](../deploy/slack):
the [app manifest](../deploy/slack/manifest.yaml) (the exact capability MaKlaude
needs — and nothing that mutates a cluster), the
[`setup.sh`](../deploy/slack/setup.sh) channel provisioner, and an
[env example](../deploy/slack/slack.env.example).

**1 — Create the Slack app from the manifest.** Go to
<https://api.slack.com/apps> → **Create New App** → **From a manifest** → pick
your workspace → paste [`deploy/slack/manifest.yaml`](../deploy/slack/manifest.yaml)
→ **Create**. The manifest turns on Socket Mode, declares the bot scopes
(`chat:write`, `channels:read`, `channels:join`, `channels:history`,
`channels:manage`, and the `groups:*` parity scopes for private channels), and
subscribes to the `message.channels` / `message.groups` events the inbound
listener consumes. The manifest is the **single source of truth** for what
MaKlaude requests — the scope rationale is documented inline in that file.

**2 — Mint the two tokens** (a manifest can't carry secrets):

| Token | Where | Env var |
| ----- | ----- | ------- |
| App-level token (`xapp-…`), scope `connections:write` | Basic Information → App-Level Tokens → Generate | `MAKLAUDE_SLACK_APP_TOKEN` |
| Bot User OAuth Token (`xoxb-…`) | OAuth & Permissions → Install to Workspace → copy | `MAKLAUDE_SLACK_BOT_TOKEN` |

(Signing secret — Basic Information → App Credentials — is **only** needed for the
optional HTTP Events API mode; in the default Socket Mode you can skip it.)

**3 — Provision the channel.** With the bot token exported, run the provisioner.
It is idempotent: it resolves the channel by name, creates it if missing, joins
the bot, invites the operator, and (with `--verify`) posts a throwaway
escalation→update→resolution thread to prove delivery + threading. It prints the
`MAKLAUDE_SLACK_CHANNEL` line to wire in and never prints the token.

```sh
export MAKLAUDE_SLACK_BOT_TOKEN=xoxb-…
deploy/slack/setup.sh --channel maklaude --operator U0123456789 --verify
```

**4 — Wire the environment.** Copy
[`deploy/slack/slack.env.example`](../deploy/slack/slack.env.example) to a local,
**gitignored** file, fill in the two tokens plus the channel ID from step 3, and
`source` it before running MaKlaude:

```sh
cp deploy/slack/slack.env.example .env.slack.local   # .env.* is gitignored
# …edit in the real values…
set -a; source .env.slack.local; set +a
```

> Treat all tokens/secrets as credentials: supply them via your secret manager or
> process environment at runtime, keep them out of version control, and never
> paste them into logs, issues, or config files committed to the repo.

### Verify end-to-end against the live workspace

`setup.sh --verify` proves the bot token + channel. To exercise **MaKlaude's own
notifier** (thread root → threaded update → threaded resolution, plus the
`needs:human` @-mention) against the live workspace, run the skip-gated manual
test:

```sh
set -a; source .env.slack.local; set +a
MAKLAUDE_SLACK_LIVE=1 go test -run TestLiveSlackManual ./internal/notify -v -count=1
```

It is skipped by default (no `MAKLAUDE_SLACK_LIVE=1`), so it never runs in CI or
`task test`.

## Unconfigured ⇒ clean fallback

If `MAKLAUDE_SLACK_*` is unset (or incomplete), MaKlaude wires a
[`NopNotifier`](../internal/notify/notify.go) and reports `live=false`. Nothing
is posted to Slack and there is **zero behavior change versus Milestone 1**: the
escalation trail remains GitHub issues plus GitHub's per-issue notification
emails, exactly as documented in the
[quickstart](quickstart.md#optional-route-escalations-to-github). This mirrors
how the GitHub trail itself degrades to an in-memory dry-run when
`MAKLAUDE_GITHUB_*` is unset.

## See also

- [`docs/quickstart.md`](quickstart.md) — register clusters, grant read-only
  access, run the monitor, and route escalations to GitHub.
- [`docs/no-writes.md`](no-writes.md) — the test-backed guarantee that MaKlaude
  never mutates a cluster; the Slack integration adds no exception to it.
