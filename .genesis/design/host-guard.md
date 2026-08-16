# Design: the host guard is a tripwire, not containment

Backport of genesis `4755cb7`, tracked here as MaKlaude issue #186 (T9 of the
Milestone 6 breakdown, plan issue #188). Lives in `.genesis/` because the problem
is framework-level rather than product-level: it is about the agent that builds
MaKlaude, not about MaKlaude's access to your clusters. Nothing here changes what
the product can do to a cluster — for that, see [`docs/no-writes.md`](../../docs/no-writes.md),
[`docs/rbac.md`](../../docs/rbac.md) and [`docs/chaos.md`](../../docs/chaos.md).

## What happened

An orchestrator session in this repo wanted to know how a shell alias was
defined, and ran:

```
grep -rn "gci" ~/.zshrc ~/.dotfiles.local/*.sh ~/.dotfiles/**/*.zsh
```

That glob covers a file holding work credentials. Nothing was exfiltrated,
nothing was being looked for, and the command is ordinary research — which is
exactly what makes it the interesting case. Matching lines would have reached the
session transcript, and from there the Loki sink that `log.sh` ships every tool
call to. A rule-following agent doing legitimate work read a directory it should
never have been able to read, and no rule it was following said otherwise.

## The asymmetry that makes this local-only

Under **GitHub Actions**, the agent runs in an ephemeral runner: fresh
filesystem, no operator home directory, credentials limited to the secrets the
workflow injects. The blast radius of a stray `grep` is a directory tree that
gets destroyed minutes later.

Under **`genesis serve`** — the mode this project actually runs in today, since
every `genesis-*` workflow is disabled locally — the agent runs *as you*, in your
shell, with your entire home directory in reach. `~/.aws`, `~/.ssh`, `~/.gnupg`,
`~/.netrc`, `~/.config/gh`, your dotfiles: all readable, all one glob away from
a transcript and a log sink.

So the containment story that holds on Actions holds because of the runner, not
because of anything the dev system does. Locally there is no containment layer at
all, and this guard does not add one.

## What the guard is

`.genesis/scripts/host-guard.sh`, wired to `PreToolUse` in
`.claude/settings.json`. It reads the hook payload, and for `Bash` calls only, it
refuses the call (exit 2 — the one code `PreToolUse` treats as a block) when the
command text references a path that holds operator credentials: `~/.ssh`,
`~/.aws`, `~/.gnupg`, `~/.netrc`, `~/.dotfiles`, `~/.config/gh`, `~/.claude.json`,
`~/Library/Keychains`, `/etc/shadow`, `/etc/sudoers`. Each is matched in three
spellings — tilde, `$HOME`, and the expanded absolute path — because those are
the same directory and a guard that knows only the tilde form is defeated by a
shell that expanded before the agent wrote the string down.

It is declared **before** `log.sh pre-tool-use`, and the ordering is load-bearing
in a way that is easy to miss: `log.sh` ships the command *text* to the Loki sink.
Logging first would donate the credential pathname to the sink that the block
exists to keep it out of, and would record a refused command as though it ran.

## What the guard is not

**Containment.** A determined route-finder gets around any per-command check.
This project already has the proof in a different domain: denying `gh api` moves
the same request to `curl`. Base64, a variable holding the path, a Python
one-liner, a `find` rooted at `/` — the guard sees command text and command text
has unbounded spellings. It is a speed bump for the *accidental* case, which is
the case that actually happened.

**Complete, even within the accidental case.** It inspects `Bash` only. `Read`,
`Grep` and `Glob` can name the same files and are not checked. That hole is
pinned by a test (`TestHostGuardIgnoresNonBashTools`) rather than left as a known
gap in someone's memory, so the honest scope is asserted rather than described.

**Reliable.** It fails **open**, deliberately and in two ways: an unparseable
payload exits 0, and on a machine with no `python3` the guard is inert. A hook
that returns a blocking 2 on something it failed to understand would wedge every
`Bash` call in the loop — including the recovery paths, which are themselves
`Bash` calls. A tripwire that can stop the loop is worse than no tripwire.

If you need containment rather than a speed bump, the fix is not on this machine:
run the loop somewhere that is not your laptop.

## Why `~/.kube` is deliberately absent

Blocking `~/.kube` would break this project's actual job. Cluster kubeconfigs are
referenced by explicit path from cluster config, and Milestone 6 sharpens this
rather than softening it — the chaos write path needs a kubeconfig by explicit
path to create and delete Chaos Mesh experiments. `TestHostGuardAllowsOrdinaryWork`
asserts `~/.kube` is allowed, so a future tightening has to delete a test with
its reason attached instead of quietly breaking the loop.

The same reasoning caps how aggressive the list should get. A guard that blocks
real work gets removed or routed around, so its false-positive cost is higher
than its marginal security — the identical trade that keeps `red-prs` and
`ready-prs` empty-means-all-clear rather than chatty.

## Why it belongs to Milestone 6

Placed here by the human on #186: chaos work makes host containment more
relevant, not less. M6 gives MaKlaude its first deliberate *write* path to a
cluster, and that path is exercised by an agent running unsandboxed on the
operator's machine under `genesis serve`. The chaos write path is the
cluster-side half of that widening; this guard is the host-side half. Neither is
a boundary on its own, and the M6 docs say so in both directions.

## Tests

`test/devsystem/hostguard_test.go`, and the split is intentional — behaviour is
tested by execution, wiring is tested against `settings.json`:

- the real incident's command is blocked, verbatim rather than reduced to a
  synthetic `cat ~/.ssh/id_rsa`
- every credential path is blocked, in all three spellings
- ordinary work is allowed: `go test`, `gh pr list`, `kubectl --kubeconfig`,
  `~/.kube/config`
- non-`Bash` tools are ignored (the acknowledged hole)
- broken payloads and a missing `python3` fail open
- a `PreToolUse` hook actually runs the script, and runs it before `log.sh`
- **every** `.genesis/scripts/` path referenced by any hook exists

That last one is a set-level guard rather than a check on this script, because
"referenced but not shipped" is a class this project has already been bitten by
twice. `genesis-merge.yml` shipped as a template the scaffolder never copied, so
the first dev system was born unable to merge its own pull requests. And upstream,
`host-guard.sh` itself was referenced by the seed `settings.json` and listed in
`SEED_SCRIPTS` while the file stayed **untracked** in git — the reference shipped
and the script did not. A `PreToolUse` hook is the worst place for that mistake,
since it fires on every tool call, so the failure is continuous rather than
occasional.
