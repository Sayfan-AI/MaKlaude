#!/usr/bin/env bash
# Genesis host guard — refuse Bash commands that reach for the operator's
# secrets. Wired to PreToolUse, which can block a call by exiting 2.
#
# WHAT THIS IS NOT: containment. A determined route-finder gets around any
# per-command check, and this project's own security chapter says so plainly —
# deny `gh api` and the agent reaches the same API with `curl`. Treat this as a
# tripwire for the *accidental* case, which is the case that actually happened:
# an agent hunting for the definition of a shell alias ran
#
#   grep -rn "gci" ~/.zshrc ~/.dotfiles.local/*.sh ~/.dotfiles/**/*.zsh
#
# and that glob covers a file holding work credentials. Nothing was exfiltrated,
# nothing was even looked for, and the matched lines would still have landed in a
# transcript and a log sink. A guard that costs one process spawn and stops the
# careless case earns its place; a guard sold as a boundary does not.
#
# THE REAL FIX is not on this machine. Under GitHub Actions the agent runs in an
# ephemeral runner with no home directory worth reading. Under `genesis serve` it
# runs as you, with your entire home directory in reach, and no hook changes that.
# If you need containment rather than a speed bump, run the loop somewhere that
# isn't your laptop.
#
# Fails OPEN: a bug here must not wedge the loop. That is a deliberate trade and
# the reason this is a tripwire rather than a wall.
set -uo pipefail

RAW="$(cat)"

python3 - "$RAW" <<'PY'
import json, os, re, sys

raw = sys.argv[1] if len(sys.argv) > 1 else ""
try:
    ctx = json.loads(raw) if raw.strip() else {}
except (ValueError, TypeError):
    sys.exit(0)  # unparseable payload: fail open, never block on our own bug

if (ctx.get("tool_name") or "") != "Bash":
    sys.exit(0)

command = ((ctx.get("tool_input") or {}).get("command")) or ""
if not isinstance(command, str) or not command:
    sys.exit(0)

# A heredoc body that is being written as TEXT is prose, and prose that merely
# names a credential path is not a read of one. Refusing it is a false positive,
# and this guard's own documentation is the most common victim: any doc, pull
# request body or escalation that says what the guard blocks has to name the
# paths it blocks.
#
# The discriminator is NOT "is it a heredoc." It is *what consumes the body*. A
# body going to a file is text; a body going to `bash` is a program, and
# exempting heredocs wholesale would hand back a trivial evasion:
#
#     bash <<'EOF'
#     cat ~/.ssh/id_rsa
#     EOF
#
# So this exempts a body only when its opener is a single, simple command whose
# name is a known text sink. Everything else keeps its body in scope. The two
# ways to be wrong here do not cost the same: a missing entry in the sink list is
# another false positive, while a missing entry in a list of interpreters is a
# hole. So this allowlists the safe shape rather than denylisting the unsafe one,
# and anything it cannot confidently classify stays in scope.
TEXT_SINKS = {"cat", "tee", "gh", "printf"}

# Any of these on the opener line means the body's destination is not decidable
# from one command: a pipe or substitution can feed it to an interpreter, and a
# chain can write it to a file and then run that file.
UNDECIDABLE = ("|", ";", "&&", "||", "`", "$(", "\n")

_HEREDOC = re.compile(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1")


def strip_text_heredocs(cmd):
    """Remove heredoc bodies that are unambiguously being written as text."""
    lines = cmd.split("\n")
    keep = []
    i = 0
    while i < len(lines):
        line = lines[i]
        keep.append(line)
        match = _HEREDOC.search(line)
        i += 1
        if not match:
            continue

        marker = match.group(2)
        dashed = line[match.start():match.start() + 3].startswith("<<-")

        # Locate the terminator first. Without one this is not a well-formed
        # heredoc and nothing is stripped.
        end = None
        for j in range(i, len(lines)):
            candidate = lines[j].strip() if dashed else lines[j]
            if candidate == marker:
                end = j
                break
        if end is None:
            continue

        opener = line[:match.start()]
        first = opener.strip().split()[0] if opener.strip().split() else ""
        simple = not any(tok in line for tok in UNDECIDABLE)
        if first in TEXT_SINKS and simple:
            keep.append(lines[end])  # keep the terminator, drop the body
            i = end + 1

    return "\n".join(keep)


try:
    scanned = strip_text_heredocs(command)
except Exception:
    scanned = command  # classification failed: scan everything, block more not less

home = os.path.expanduser("~")
# Paths that hold credentials and have no business in a dev-system session. The
# repo's own kubeconfigs are referenced by explicit path from cluster config, so
# ~/.kube is deliberately absent: blocking it would break real work.
SENSITIVE = [
    r"~/\.ssh", r"\$HOME/\.ssh", re.escape(home) + r"/\.ssh",
    r"~/\.aws", r"\$HOME/\.aws", re.escape(home) + r"/\.aws",
    r"~/\.gnupg", re.escape(home) + r"/\.gnupg",
    r"~/\.netrc", re.escape(home) + r"/\.netrc",
    r"~/\.dotfiles", re.escape(home) + r"/\.dotfiles",
    r"~/\.config/gh", re.escape(home) + r"/\.config/gh",
    r"~/\.claude\.json", re.escape(home) + r"/\.claude\.json",
    r"~/Library/Keychains", re.escape(home) + r"/Library/Keychains",
    r"/etc/(shadow|sudoers)",
]
for pattern in SENSITIVE:
    if re.search(pattern, scanned):
        sys.stderr.write(
            "blocked by .genesis/scripts/host-guard.sh: this command reaches "
            "outside the repository for a path that holds operator credentials "
            f"(matched {pattern!r}).\n"
            "Nothing in a dev-system task needs the operator's secrets. If you "
            "are looking for a shell alias or tool configuration, read the "
            "repository's own files instead.\n"
        )
        sys.exit(2)  # PreToolUse: 2 blocks the call
sys.exit(0)
PY
