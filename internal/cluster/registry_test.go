package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig creates an empty placeholder kubeconfig file in dir and
// returns its path. The contents are irrelevant: this package only checks for
// the file's existence, never reads credentials.
func writeKubeconfig(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("# placeholder kubeconfig\n"), 0o600); err != nil {
		t.Fatalf("writing placeholder kubeconfig: %v", err)
	}
	return path
}

func TestNewRegistry_TwoClustersAreIsolated(t *testing.T) {
	dir := t.TempDir()
	kc1 := writeKubeconfig(t, dir, "prod.yaml")
	kc2 := writeKubeconfig(t, dir, "staging.yaml")

	cfg := &Config{Clusters: []Spec{
		{Name: "prod", Kubeconfig: kc1, Context: "prod-ctx"},
		{Name: "staging", Kubeconfig: kc2, Context: "staging-ctx"},
	}}

	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil", err)
	}
	if reg.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", reg.Len())
	}

	prod, ok := reg.Get("prod")
	if !ok {
		t.Fatal("Get(prod) not found")
	}
	staging, ok := reg.Get("staging")
	if !ok {
		t.Fatal("Get(staging) not found")
	}

	// Distinct handles with distinct, correct fields.
	if prod == staging {
		t.Fatal("handles for prod and staging are the same pointer")
	}
	if prod.Kubeconfig() == staging.Kubeconfig() {
		t.Errorf("handles share a kubeconfig path: %q", prod.Kubeconfig())
	}
	if prod.Context() != "prod-ctx" || staging.Context() != "staging-ctx" {
		t.Errorf("contexts not resolved independently: prod=%q staging=%q", prod.Context(), staging.Context())
	}

	// Mutating the source config after construction must not affect handles:
	// the registry must own immutable copies, not alias the input.
	cfg.Clusters[0].Name = "MUTATED"
	cfg.Clusters[0].Context = "MUTATED"
	if prod.Name() != "prod" {
		t.Errorf("handle name changed after mutating source config: got %q", prod.Name())
	}
	if prod.Context() != "prod-ctx" {
		t.Errorf("handle context changed after mutating source config: got %q", prod.Context())
	}
}

func TestRegistry_NamesPreserveOrderAndAreCopies(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Clusters: []Spec{
		{Name: "alpha", Kubeconfig: writeKubeconfig(t, dir, "a.yaml"), Context: "a"},
		{Name: "beta", Kubeconfig: writeKubeconfig(t, dir, "b.yaml"), Context: "b"},
		{Name: "gamma", Kubeconfig: writeKubeconfig(t, dir, "c.yaml"), Context: "c"},
	}}
	reg, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	names := reg.Names()
	want := []string{"alpha", "beta", "gamma"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("Names() = %v, want %v", names, want)
	}

	// Mutating the returned slice must not corrupt the registry's internal state.
	names[0] = "tampered"
	if again := reg.Names(); again[0] != "alpha" {
		t.Errorf("Names() returned an aliased slice; internal state mutated to %v", again)
	}
}

func TestValidate_ErrorCases(t *testing.T) {
	dir := t.TempDir()
	goodKC := writeKubeconfig(t, dir, "good.yaml")
	missingKC := filepath.Join(dir, "does-not-exist.yaml")

	tests := []struct {
		name        string
		cfg         *Config
		wantSubstrs []string
	}{
		{
			name:        "nil config",
			cfg:         nil,
			wantSubstrs: []string{"no clusters defined"},
		},
		{
			name:        "empty cluster list",
			cfg:         &Config{},
			wantSubstrs: []string{"no clusters defined"},
		},
		{
			name: "missing name",
			cfg: &Config{Clusters: []Spec{
				{Name: "", Kubeconfig: goodKC, Context: "ctx"},
			}},
			wantSubstrs: []string{"missing required field 'name'"},
		},
		{
			name: "missing kubeconfig",
			cfg: &Config{Clusters: []Spec{
				{Name: "c1", Kubeconfig: "", Context: "ctx"},
			}},
			wantSubstrs: []string{"missing required field 'kubeconfig'"},
		},
		{
			name: "missing context",
			cfg: &Config{Clusters: []Spec{
				{Name: "c1", Kubeconfig: goodKC, Context: ""},
			}},
			wantSubstrs: []string{"missing required field 'context'"},
		},
		{
			name: "missing kubeconfig file on disk",
			cfg: &Config{Clusters: []Spec{
				{Name: "c1", Kubeconfig: missingKC, Context: "ctx"},
			}},
			wantSubstrs: []string{"does not exist"},
		},
		{
			name: "duplicate names",
			cfg: &Config{Clusters: []Spec{
				{Name: "dup", Kubeconfig: goodKC, Context: "ctx"},
				{Name: "dup", Kubeconfig: goodKC, Context: "ctx2"},
			}},
			wantSubstrs: []string{"duplicate cluster name", `"dup"`},
		},
		{
			name: "aggregates multiple problems",
			cfg: &Config{Clusters: []Spec{
				{Name: "", Kubeconfig: "", Context: ""},
			}},
			wantSubstrs: []string{
				"missing required field 'name'",
				"missing required field 'kubeconfig'",
				"missing required field 'context'",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Validate() error does not wrap ErrInvalidConfig: %v", err)
			}
			for _, sub := range tt.wantSubstrs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("Validate() error = %q, want it to contain %q", err.Error(), sub)
				}
			}
		})
	}
}

func TestNewRegistry_RejectsInvalidConfig(t *testing.T) {
	_, err := NewRegistry(&Config{})
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewRegistry(empty) error = %v, want ErrInvalidConfig", err)
	}
	_, err = NewRegistry(nil)
	if err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewRegistry(nil) error = %v, want ErrInvalidConfig", err)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	tests := []struct {
		in   string
		want string
	}{
		{"~", home},
		{"~/.kube/config", filepath.Join(home, ".kube", "config")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := expandPath(tt.in); got != tt.want {
			t.Errorf("expandPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHandle_StringIsSecretFree(t *testing.T) {
	h := &Handle{name: "prod", kubeconfig: "/home/alice/.kube/prod.yaml", context: "prod-ctx"}
	s := h.String()
	for _, want := range []string{"prod", "/home/alice/.kube/prod.yaml", "prod-ctx"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, want it to contain %q", s, want)
		}
	}
}
