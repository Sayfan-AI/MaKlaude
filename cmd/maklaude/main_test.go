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
		{"remediate lists in help", nil, 0, "remediate"},
		{"remediate without config errors", []string{"remediate"}, 2, "--config is required"},
		{"remediate help", []string{"remediate", "-h"}, 0, "maklaude remediate"},
		{"remediate missing config file", []string{"remediate", "--config", "/nonexistent/x.yaml"}, 2, "does not exist"},
		// The help text must state the default posture, because an operator who reads
		// only --help is exactly the operator most likely to assume a command named
		// "remediate" remediates.
		{"remediate help states execution is off", []string{"remediate", "-h"}, 0, "EXECUTION IS OFF BY DEFAULT"},
		{"remediate help names the opt-in", []string{"remediate", "-h"}, 0, "MAKLAUDE_EXECUTE_MODE"},
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

// TestRunRemediate_DefaultsToProposeOnly runs the command end to end with the
// environment untouched and checks that it reports the disabled posture.
//
// It is the CLI-level counterpart of TestRun_DisabledBuildsNoExecutor: that test proves
// no write client is constructed, this one proves the binary an operator actually runs
// is the one in that state by default. The registry names a kubeconfig that does not
// resolve to a live cluster, so the per-cluster read fails — which is fine and is not
// what is under test. The report header is produced either way, and it is the header
// that carries the claim.
func TestRunRemediate_DefaultsToProposeOnly(t *testing.T) {
	t.Setenv("MAKLAUDE_EXECUTE_MODE", "")

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "clusters:\n  - name: c1\n    kubeconfig: " + kubeconfig + "\n    context: ctx\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var buf bytes.Buffer
	if code := run([]string{"remediate", "--config", cfgPath}, &buf); code != 0 {
		t.Fatalf("remediate: code = %d, want 0; output:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"execution DISABLED", "no executor was built"} {
		if !strings.Contains(out, want) {
			t.Errorf("the default remediate report does not say %q:\n%s", want, out)
		}
	}
}

// TestRunRemediate_RefusesAnUnreadableOptIn checks that a typo in the one variable that
// turns on the write path stops the run rather than being read as "off".
//
// Guessing low would silently ignore an operator who meant to enable execution, and
// they would have no way to tell from the output that they had not; guessing high needs
// no explanation.
func TestRunRemediate_RefusesAnUnreadableOptIn(t *testing.T) {
	t.Setenv("MAKLAUDE_EXECUTE_MODE", "enabledd")

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "clusters:\n  - name: c1\n    kubeconfig: " + kubeconfig + "\n    context: ctx\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	var buf bytes.Buffer
	if code := run([]string{"remediate", "--config", cfgPath}, &buf); code != 2 {
		t.Fatalf("remediate with an unreadable opt-in: code = %d, want 2; output:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "MAKLAUDE_EXECUTE_MODE") {
		t.Errorf("the error does not name the variable an operator has to fix: %q", buf.String())
	}
}
