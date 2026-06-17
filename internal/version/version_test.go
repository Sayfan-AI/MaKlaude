package version

import (
	"strings"
	"testing"
)

func TestInfoContainsVersion(t *testing.T) {
	got := Info()
	if !strings.Contains(got, Version) {
		t.Errorf("Info() = %q, want it to contain Version %q", got, Version)
	}
	if !strings.HasPrefix(got, "maklaude ") {
		t.Errorf("Info() = %q, want it to start with %q", got, "maklaude ")
	}
}

func TestDefaultsNonEmpty(t *testing.T) {
	for name, val := range map[string]string{
		"Version": Version,
		"Commit":  Commit,
		"Date":    Date,
	} {
		if val == "" {
			t.Errorf("%s should not be empty", name)
		}
	}
}
