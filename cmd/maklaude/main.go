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
//
// Continuous monitoring, remediation, and approval flows are added in later
// milestones; scan is the deterministic, auditable core they build on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/scan"
	"github.com/Sayfan-AI/MaKlaude/internal/version"
)

const usage = `maklaude — autonomous Kubernetes operations

Usage:
  maklaude [command] [flags]

Commands:
  scan       Run a one-shot, read-only scan of every registered cluster
  version    Print version information and exit
  help       Show this help message and exit

Flags:
  -v, --version    Print version information and exit
  -h, --help       Show this help message and exit

Run "maklaude scan --help" for scan-specific flags.
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
