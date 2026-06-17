package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	kc1 := writeKubeconfig(t, dir, "prod.yaml")
	kc2 := writeKubeconfig(t, dir, "staging.yaml")

	body := "" +
		"clusters:\n" +
		"  - name: prod\n" +
		"    kubeconfig: " + kc1 + "\n" +
		"    context: prod-ctx\n" +
		"  - name: staging\n" +
		"    kubeconfig: " + kc2 + "\n" +
		"    context: staging-ctx\n"

	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	reg, err := NewRegistryFromFile(path)
	if err != nil {
		t.Fatalf("NewRegistryFromFile() error = %v, want nil", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", reg.Len())
	}
	if _, ok := reg.Get("prod"); !ok {
		t.Error("prod not registered")
	}
	if _, ok := reg.Get("staging"); !ok {
		t.Error("staging not registered")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("LoadConfig() = nil error, want error for missing file")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention the file does not exist", err.Error())
	}
}

func TestParseConfig_MalformedYAML(t *testing.T) {
	_, err := ParseConfig(strings.NewReader("clusters: [this is : not valid"))
	if err == nil {
		t.Fatal("ParseConfig() = nil error, want error for malformed YAML")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("error = %q, want it to mention invalid YAML", err.Error())
	}
}

func TestParseConfig_Empty(t *testing.T) {
	_, err := ParseConfig(strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("ParseConfig(empty) error = %v, want an 'empty' error", err)
	}
}

func TestParseConfig_UnknownField(t *testing.T) {
	body := "" +
		"clusters:\n" +
		"  - name: c1\n" +
		"    kubeconfig: /tmp/kc\n" +
		"    context: ctx\n" +
		"    secret: hunter2\n" // not an allowed field

	_, err := ParseConfig(strings.NewReader(body))
	if err == nil {
		t.Fatal("ParseConfig() = nil error, want error for unknown field")
	}
}

func TestExampleConfigParses(t *testing.T) {
	// The committed sample config must remain syntactically valid and use only
	// known fields, so the documented format never drifts from the code.
	path := filepath.Join("..", "..", "config.example.yaml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%s) error = %v", path, err)
	}
	if len(cfg.Clusters) < 2 {
		t.Errorf("example config has %d clusters, want >= 2", len(cfg.Clusters))
	}
	for i, c := range cfg.Clusters {
		if c.Name == "" || c.Kubeconfig == "" || c.Context == "" {
			t.Errorf("example cluster #%d has an empty required field: %+v", i+1, c)
		}
	}
	// The example must never contain anything resembling inline credentials.
	raw, err := os.ReadFile(path) //nolint:gosec // test reads the committed sample file.
	if err != nil {
		t.Fatalf("reading example config: %v", err)
	}
	for _, banned := range []string{"client-certificate-data", "token:", "password", "BEGIN "} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("example config appears to contain a credential (%q); it must reference paths only", banned)
		}
	}
}

func TestLoadConfig_InvalidWrapsSentinel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	// Valid YAML, but semantically invalid (no clusters).
	if err := os.WriteFile(path, []byte("clusters: []\n"), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	_, err := NewRegistryFromFile(path)
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewRegistryFromFile() error = %v, want ErrInvalidConfig", err)
	}
}
