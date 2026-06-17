// Command maklaude is the CLI entrypoint for the MaKlaude autonomous
// Kubernetes operations system.
//
// This is a skeleton: it builds, prints version and help, and does nothing
// else yet. Real functionality (cluster registration, diagnosis, remediation)
// is added in later milestones.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/version"
)

const usage = `maklaude — autonomous Kubernetes operations (skeleton)

Usage:
  maklaude [command]

Commands:
  version    Print version information and exit
  help       Show this help message and exit

Flags:
  -v, --version    Print version information and exit
  -h, --help       Show this help message and exit
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
	case "", "help":
		fmt.Fprint(out, usage)
		return 0
	default:
		fmt.Fprintf(out, "maklaude: unknown command %q\n\n", fs.Arg(0))
		fmt.Fprint(out, usage)
		return 2
	}
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}
