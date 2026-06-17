package main

import (
	"bytes"
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
