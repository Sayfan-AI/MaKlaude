package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantSub  string
	}{
		{"no args shows help", nil, 0, "Usage:"},
		{"help command", []string{"help"}, 0, "Usage:"},
		{"version command", []string{"version"}, 0, "maklaude "},
		{"version flag", []string{"--version"}, 0, "maklaude "},
		{"version shorthand", []string{"-v"}, 0, "maklaude "},
		{"unknown command", []string{"frobnicate"}, 2, "unknown command"},
		{"scan lists in help", nil, 0, "scan"},
		{"scan without config errors", []string{"scan"}, 2, "--config is required"},
		{"scan help", []string{"scan", "-h"}, 0, "maklaude scan"},
		{"scan missing config file", []string{"scan", "--config", "/nonexistent/x.yaml"}, 2, "does not exist"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := run(tt.args, &buf)
			if code != tt.wantCode {
				t.Errorf("run(%v) code = %d, want %d", tt.args, code, tt.wantCode)
			}
			if !strings.Contains(buf.String(), tt.wantSub) {
				t.Errorf("run(%v) output = %q, want it to contain %q", tt.args, buf.String(), tt.wantSub)
			}
		})
	}
}

// TestRunScan_InvalidConfig exercises the scan path against a syntactically valid
// but semantically invalid config (references a kubeconfig that does not exist),
// confirming the command surfaces the registry's validation error with exit 2.
func TestRunScan_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "clusters:\n  - name: c1\n    kubeconfig: " + filepath.Join(dir, "missing.kubeconfig") + "\n    context: ctx\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var buf bytes.Buffer
	code := run([]string{"scan", "--config", cfgPath}, &buf)
	if code != 2 {
		t.Errorf("scan with invalid config: code = %d, want 2", code)
	}
	if !strings.Contains(buf.String(), "invalid cluster config") {
		t.Errorf("expected validation error, got %q", buf.String())
	}
}
