// Package version exposes build/version metadata for the maklaude binary.
//
// The values here can be overridden at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/Sayfan-AI/MaKlaude/internal/version.Version=v0.1.0"
package version

import (
	"fmt"
	"runtime"
)

// These variables are intended to be set at build time via -ldflags.
// They default to development-friendly placeholder values.
var (
	// Version is the semantic version of the build.
	Version = "0.0.0-dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build timestamp.
	Date = "unknown"
)

// Info returns a single-line, human-readable description of the build.
func Info() string {
	return fmt.Sprintf(
		"maklaude %s (commit %s, built %s, %s)",
		Version, Commit, Date, runtime.Version(),
	)
}
