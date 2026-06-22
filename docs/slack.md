# MaKlaude Slack / ChatOps integration

MaKlaude can optionally mirror its escalation trail into Slack so operators get
real-time, threaded notifications and (in a later task) can converse with it in
chat. Slack is **strictly optional and comms-only**: it notifies and converses,
it never gains any cluster-mutating capability. With no Slack configuration,
MaKlaude behaves **exactly** as it did in Milestone 1 — GitHub issues plus
GitHub's own per-issue notification emails — with zero behavior change. This is
the same graceful-degradation seam the GitHub trail uses (see the
[no-writes guarantee](no-writes.md) and [`internal/escalate`](../internal/escalate)).

> **Status.** This task (M2 T1) establishes the configuration surface, the
> [`Notifier`](../internal/notify/notify.go) interface, and the no-op default.
> It does **not** post to Slack yet — even a fully-configured deployment still
> degrades to the no-op. The live Slack backend (outbound posting + Socket Mode
> inbound) lands in T2. The env vars below are stable and safe to set now; they
> become active in T2.

## How notifications work

MaKlaude models each problem as a conversation, keyed by the same stable
*identity* the GitHub escalator uses to dedup an active issue across cycles:

| Lifecycle step | `Notifier` method | What the live backend will do |
| -------------- | ----------------- | ----------------------------- |
| Problem first escalated | `NotifyEscalation` | Post a top-level message — the thread root — linking back to the GitHub issue |
| Problem recurs / changes | `NotifyUpdate` | Reply into the same thread |
| Problem clears | `NotifyResolution` | Post a closing reply into the same thread |

The identity → Slack thread mapping is persisted in the backing **GitHub issue
via a hidden marker** (the approved default), so no new datastore is introduced.
T1 establishes this seam; T2 implements it.

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
| `MAKLAUDE_SLACK_SIGNING_SECRET` | no | Signing secret to verify inbound HTTP Events API requests; only needed if you run HTTP mode instead of Socket Mode. **Secret — never logged.** |

When the three required variables are all set, MaKlaude considers Slack
**configured** ([`SlackConfig.Configured`](../internal/notify/slack.go)). When
any is missing, it falls back to the no-op notifier and the GitHub + email trail
is unchanged.

## Obtaining the tokens

Create a Slack app at <https://api.slack.com/apps> (a manifest-based app is
simplest), then:

1. **Enable Socket Mode** (Settings → Socket Mode) and generate an **app-level
   token** with the `connections:write` scope. This is `MAKLAUDE_SLACK_APP_TOKEN`
   (`xapp-…`).
2. **Add bot scopes** (Features → OAuth & Permissions → Bot Token Scopes). For
   posting escalation threads the live backend will need at least `chat:write`;
   `channels:read`/`groups:read` help resolve a `#name` to a channel ID.
3. **Install the app to your workspace** and copy the **Bot User OAuth Token**
   (`xoxb-…`) into `MAKLAUDE_SLACK_BOT_TOKEN`.
4. **Invite the bot to the channel** you set in `MAKLAUDE_SLACK_CHANNEL` so it
   can post there (`/invite @your-app`).
5. (Optional, HTTP mode only) Copy the **Signing Secret** (Settings → Basic
   Information → App Credentials) into `MAKLAUDE_SLACK_SIGNING_SECRET`.

> Treat all three tokens/secrets as credentials: supply them via your secret
> manager or process environment at runtime, keep them out of version control,
> and never paste them into logs, issues, or config files committed to the repo.

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
