// Command maklaude is the CLI entrypoint for the MaKlaude autonomous
// Kubernetes operations system.
//
// It currently exposes:
//
//   - version / help — informational commands.
//   - scan          — a one-shot, read-only sweep of every registered cluster:
//     it collects health, detects problems, and reconciles findings into the
//     comms trail, then prints a structured report (text or JSON). scan never
//     mutates a cluster; its only writes are to the escalation trail, and those
//     degrade to an in-memory dry-run unless GitHub is configured.
//   - remediate     — one pass of the remediation cycle: observe, diagnose,
//     propose, preview, ask a human, and execute only what they have already
//     approved. It is off by default in the strongest sense available — without
//     MAKLAUDE_EXECUTE_MODE it proposes and stops, constructing no write-capable
//     client at all — and it relaxes none of the five gates the remediation
//     design rests on. It also carries the unattended half, off by default in the
//     same sense: with MAKLAUDE_AUTONOMY_RULES unset no rule exists, so every
//     proposal takes the human gate.
//
// The two commands are separate rather than one command with a flag because they
// make different promises. scan's is that nothing it does can change a cluster,
// and that promise is worth being unable to weaken by passing an argument.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/operate"
	"github.com/Sayfan-AI/MaKlaude/internal/scan"
	"github.com/Sayfan-AI/MaKlaude/internal/version"
)

const usage = `maklaude — autonomous Kubernetes operations

Usage:
  maklaude [command] [flags]

Commands:
  scan       Run a one-shot, read-only scan of every registered cluster
  remediate  Run one pass of the gated remediation cycle (propose-only by default)
  version    Print version information and exit
  help       Show this help message and exit

Flags:
  -v, --version    Print version information and exit
  -h, --help       Show this help message and exit

Run "maklaude scan --help" or "maklaude remediate --help" for per-command flags.
`

const scanUsage = `maklaude scan — one-shot, read-only scan of every registered cluster

For each cluster in the config it collects health signals, detects problems
deterministically, and reconciles the findings into the escalation trail, then
prints a report. It performs NO mutating action against any cluster.

Usage:
  maklaude scan --config <path> [--json]

Flags:
  --config <path>   Path to the cluster registry config (see config.example.yaml). Required.
  --json            Emit the report as JSON instead of human-readable text.
  -h, --help        Show this help message and exit
`

const remediateUsage = `maklaude remediate — one pass of the gated remediation cycle

For each cluster in the config it observes, diagnoses, and proposes remediations,
then — only if execution has been explicitly enabled — previews each proposal as a
server-side dry run, puts it to a human on the approval trail, and executes exactly
what a human has already approved.

EXECUTION IS OFF BY DEFAULT. With MAKLAUDE_EXECUTE_MODE unset this command proposes
and stops: it builds no write-capable client, opens no approval request, and sends
nothing to any cluster.

  export MAKLAUDE_EXECUTE_MODE=disabled   # default. Propose only.
  export MAKLAUDE_EXECUTE_MODE=dry-run    # full cycle, every request dryRun=All.
  export MAKLAUDE_EXECUTE_MODE=enabled    # an approved action changes the cluster.

Enabling it is necessary but not sufficient. An action must still pass every gate
it always had: the separate write RBAC bundle (deploy/rbac/write), an approval label
from an identity MaKlaude cannot forge, preconditions re-checked against a fresh
read, and the resourceVersion the proposal was computed from. See docs/remediation.md.

Approvals travel over the same MAKLAUDE_GITHUB_* configuration the escalation trail
uses. Without it the gate degrades to an in-memory trail nobody can approve on, so
the cycle asks and nothing is ever authorized.

EARNED AUTONOMY IS ALSO OFF BY DEFAULT, and off means no rule exists. An operator can
allow specific (cluster, namespace, operation) shapes to run without a human once a
recorded history of human approvals has earned it. All three are required together,
and setting the first without the others refuses to start:

  export MAKLAUDE_AUTONOMY_RULES=/etc/maklaude/autonomy.yaml            # the grant
  export MAKLAUDE_TRUST_LEDGER=/var/lib/maklaude/trust.jsonl            # the history
  export MAKLAUDE_AUTONOMY_STATE=/var/lib/maklaude/autonomy-state.json  # the ceiling

Nothing is trusted on a fresh install, so every proposal gates until a shape earns it.
The report's "Unattended actions:" line states the posture every pass, and says why
autonomy is off when it is. See autonomy.example.yaml and docs/unattended-actions.md.

Usage:
  maklaude remediate --config <path> [--json]

Flags:
  --config <path>   Path to the cluster registry config (see config.example.yaml). Required.
  --json            Emit the report as JSON instead of human-readable text.
  -h, --help        Show this help message and exit
`

// run executes the CLI against the given args (excluding the program name)
// and writes output to out. It returns a process exit code.
func run(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("maklaude", flag.ContinueOnError)
	fs.SetOutput(out)
	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version information and exit")
	fs.BoolVar(&showVersion, "v", false, "print version information and exit (shorthand)")
	fs.Usage = func() { fmt.Fprint(out, usage) }

	if err := fs.Parse(args); err != nil {
		// flag already prints the error / usage on parse failure.
		return 2
	}

	if showVersion {
		fmt.Fprintln(out, version.Info())
		return 0
	}

	switch fs.Arg(0) {
	case "version":
		fmt.Fprintln(out, version.Info())
		return 0
	case "scan":
		return runScan(fs.Args()[1:], out)
	case "remediate":
		return runRemediate(fs.Args()[1:], out)
	case "", "help":
		fmt.Fprint(out, usage)
		return 0
	default:
		fmt.Fprintf(out, "maklaude: unknown command %q\n\n", fs.Arg(0))
		fmt.Fprint(out, usage)
		return 2
	}
}

// runScan parses the scan subcommand's flags, builds the cluster registry from
// the config, runs the read-only pipeline once, and writes the report. It
// returns a process exit code: 0 on success, 2 on a usage/config error.
func runScan(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(out)
	var configPath string
	var asJSON bool
	fs.StringVar(&configPath, "config", "", "path to the cluster registry config file (required)")
	fs.BoolVar(&asJSON, "json", false, "emit the report as JSON instead of text")
	fs.Usage = func() { fmt.Fprint(out, scanUsage) }

	if err := fs.Parse(args); err != nil {
		// -h/--help requests usage and is a success, not an error; flag has
		// already printed scanUsage via fs.Usage.
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if configPath == "" {
		fmt.Fprintln(out, "maklaude scan: --config is required")
		fmt.Fprint(out, scanUsage)
		return 2
	}

	reg, err := cluster.NewRegistryFromFile(configPath)
	if err != nil {
		fmt.Fprintf(out, "maklaude scan: %v\n", err)
		return 2
	}

	report, err := scan.NewPipeline().Run(context.Background(), reg)
	if err != nil {
		fmt.Fprintf(out, "maklaude scan: %v\n", err)
		return 1
	}

	var writeErr error
	if asJSON {
		writeErr = report.WriteJSON(out)
	} else {
		writeErr = report.WriteText(out)
	}
	if writeErr != nil {
		fmt.Fprintf(out, "maklaude scan: %v\n", writeErr)
		return 1
	}
	return 0
}

// runRemediate parses the remediate subcommand's flags, builds the cluster registry,
// runs ONE pass of the gated remediation cycle, and writes the report.
//
// Exit codes: 0 on success, 2 on a usage/config error, 1 on a run failure. Note what
// is deliberately NOT an error code: a pass that proposed actions and executed none
// exits 0, because that is the gate working. Only a run that could not be performed
// at all fails.
//
// The [operate.New] error path is the one worth reading twice. It fires on an
// unrecognized MAKLAUDE_EXECUTE_MODE, and on the two approval-gate misconfigurations
// that would otherwise produce a gate that looks functional while authorizing things
// nobody decided. Refusing to start is the only place those can be made visible: by
// the time they matter, the artifact already reads like a human decision.
func runRemediate(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("remediate", flag.ContinueOnError)
	fs.SetOutput(out)
	var configPath string
	var asJSON bool
	fs.StringVar(&configPath, "config", "", "path to the cluster registry config file (required)")
	fs.BoolVar(&asJSON, "json", false, "emit the report as JSON instead of text")
	fs.Usage = func() { fmt.Fprint(out, remediateUsage) }

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if configPath == "" {
		fmt.Fprintln(out, "maklaude remediate: --config is required")
		fmt.Fprint(out, remediateUsage)
		return 2
	}

	reg, err := cluster.NewRegistryFromFile(configPath)
	if err != nil {
		fmt.Fprintf(out, "maklaude remediate: %v\n", err)
		return 2
	}

	cycle, _, err := operate.New()
	if err != nil {
		fmt.Fprintf(out, "maklaude remediate: %v\n", err)
		return 2
	}

	report, err := cycle.Run(context.Background(), reg)
	if err != nil {
		fmt.Fprintf(out, "maklaude remediate: %v\n", err)
		return 1
	}

	var writeErr error
	if asJSON {
		writeErr = report.WriteJSON(out)
	} else {
		writeErr = report.WriteText(out)
	}
	if writeErr != nil {
		fmt.Fprintf(out, "maklaude remediate: %v\n", writeErr)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
