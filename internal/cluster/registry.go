package cluster

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Handle is a resolved, validated descriptor for a single cluster. It is the
// stable reference the rest of MaKlaude uses to address a cluster.
//
// A Handle owns its own immutable copy of the cluster's configuration. There
// is no shared or global mutable state between handles, so two handles built
// from the same config are fully independent: operating on one can never
// affect another. A later milestone turns a Handle into a live Kubernetes
// client; this package goes no further than holding the validated reference.
type Handle struct {
	name       string
	kubeconfig string
	context    string
}

// Name returns the unique name of the cluster.
func (h *Handle) Name() string { return h.name }

// Kubeconfig returns the resolved (home-expanded) filesystem path to the
// cluster's kubeconfig file. The file's contents are not read or stored.
func (h *Handle) Kubeconfig() string { return h.kubeconfig }

// Context returns the kubeconfig context selected for the cluster.
func (h *Handle) Context() string { return h.context }

// String returns a short, secret-free description of the handle suitable for
// logs. It never includes credentials — only the name, path, and context.
func (h *Handle) String() string {
	return fmt.Sprintf("cluster %q (kubeconfig=%s, context=%s)", h.name, h.kubeconfig, h.context)
}

// Registry holds the validated set of clusters MaKlaude operates. Handles are
// resolved up front and looked up by name. The Registry exposes no mutating
// operations, so it is safe to share read-only across the system.
type Registry struct {
	order   []string
	handles map[string]*Handle
}

// NewRegistryFromFile loads, validates, and resolves the cluster config at
// path into a Registry. It is the primary entrypoint for registering clusters.
//
// It fails loudly with a clear, actionable error if the file is missing, the
// YAML is malformed, or the config is semantically invalid (see
// [Config.Validate]).
func NewRegistryFromFile(path string) (*Registry, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return NewRegistry(cfg)
}

// NewRegistry validates cfg and resolves it into a Registry of isolated
// handles. Each handle holds an independent copy of its configuration.
//
// Validation aggregates every problem it finds (so the operator can fix them
// all at once) and returns them as a single error. The error unwraps to the
// sentinel [ErrInvalidConfig] for programmatic checks.
func NewRegistry(cfg *Config) (*Registry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is nil", ErrInvalidConfig)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	reg := &Registry{
		order:   make([]string, 0, len(cfg.Clusters)),
		handles: make(map[string]*Handle, len(cfg.Clusters)),
	}
	for i := range cfg.Clusters {
		c := cfg.Clusters[i]
		// Each Handle gets its own copy of the (resolved) fields — no aliasing
		// of slices or pointers, so handles are fully isolated.
		h := &Handle{
			name:       strings.TrimSpace(c.Name),
			kubeconfig: expandPath(strings.TrimSpace(c.Kubeconfig)),
			context:    strings.TrimSpace(c.Context),
		}
		reg.order = append(reg.order, h.name)
		reg.handles[h.name] = h
	}
	return reg, nil
}

// Get returns the handle for the named cluster. The boolean is false if no
// such cluster is registered.
func (r *Registry) Get(name string) (*Handle, bool) {
	h, ok := r.handles[name]
	return h, ok
}

// Names returns the registered cluster names in the order they were declared
// in the config.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Handles returns all registered handles in declaration order.
func (r *Registry) Handles() []*Handle {
	out := make([]*Handle, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.handles[name])
	}
	return out
}

// Len returns the number of registered clusters.
func (r *Registry) Len() int { return len(r.order) }

// ErrInvalidConfig is the sentinel error all configuration validation failures
// wrap. Use errors.Is(err, ErrInvalidConfig) to detect them.
var ErrInvalidConfig = errors.New("invalid cluster config")

// Validate checks the semantic correctness of the config and reports every
// problem it finds at once. It verifies that:
//
//   - at least one cluster is declared;
//   - every cluster has a non-empty name, kubeconfig path, and context;
//   - cluster names are unique;
//   - every referenced kubeconfig file exists on disk and is a regular file.
//
// The returned error (if any) wraps [ErrInvalidConfig] and enumerates all
// failures in a stable, readable form.
func (c *Config) Validate() error {
	var problems []string

	if c == nil || len(c.Clusters) == 0 {
		return fmt.Errorf("%w: no clusters defined (the 'clusters' list is empty or missing)", ErrInvalidConfig)
	}

	seen := make(map[string]int) // name -> first index it appeared at
	dupes := make(map[string]bool)

	for i, cc := range c.Clusters {
		// A label to identify the offending entry in messages: prefer the
		// name, fall back to the 1-based position.
		label := fmt.Sprintf("cluster #%d", i+1)
		name := strings.TrimSpace(cc.Name)
		if name != "" {
			label = fmt.Sprintf("cluster %q", name)
		}

		if name == "" {
			problems = append(problems, fmt.Sprintf("%s: missing required field 'name'", label))
		} else {
			if _, ok := seen[name]; ok {
				dupes[name] = true
			} else {
				seen[name] = i
			}
		}

		kubeconfig := strings.TrimSpace(cc.Kubeconfig)
		if kubeconfig == "" {
			problems = append(problems, fmt.Sprintf("%s: missing required field 'kubeconfig'", label))
		} else if err := checkKubeconfigFile(expandPath(kubeconfig)); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %s", label, err))
		}

		if strings.TrimSpace(cc.Context) == "" {
			problems = append(problems, fmt.Sprintf("%s: missing required field 'context'", label))
		}
	}

	if len(dupes) > 0 {
		names := make([]string, 0, len(dupes))
		for n := range dupes {
			names = append(names, fmt.Sprintf("%q", n))
		}
		sort.Strings(names)
		problems = append(problems, fmt.Sprintf("duplicate cluster name(s): %s (names must be unique)", strings.Join(names, ", ")))
	}

	if len(problems) == 0 {
		return nil
	}

	sort.Strings(problems)
	return fmt.Errorf("%w:\n  - %s", ErrInvalidConfig, strings.Join(problems, "\n  - "))
}

// checkKubeconfigFile verifies that path refers to an existing regular file.
// It reports an actionable error otherwise and never reads the file contents.
func checkKubeconfigFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("kubeconfig file %q does not exist", path)
		}
		return fmt.Errorf("cannot access kubeconfig file %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("kubeconfig path %q is a directory, not a file", path)
	}
	return nil
}

// expandPath expands a leading "~" or "~/" to the current user's home
// directory. Any other path is returned unchanged. If the home directory
// cannot be determined, the original path is returned so validation surfaces a
// clear "does not exist" error rather than a confusing one.
func expandPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
