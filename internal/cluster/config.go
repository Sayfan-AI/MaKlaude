// Package cluster provides the configuration surface for registering the
// Kubernetes clusters MaKlaude operates on a human's behalf.
//
// A human declares the clusters under MaKlaude's care in a YAML config file.
// Each cluster is described by reference only — a unique name, a path to a
// kubeconfig file on disk, and a context within that kubeconfig. Credentials
// are never stored in this config and never read by this package; only the
// filesystem path is recorded. Turning a validated cluster descriptor into a
// live Kubernetes client is the job of a later milestone.
//
// The package is deliberately read-only with respect to clusters: it loads,
// validates, and resolves configuration into isolated [Handle] values. There
// is no shared or global mutable state across clusters, so operating on one
// cluster can never affect another.
package cluster

import (
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level structure of a MaKlaude cluster configuration file.
//
// Example YAML:
//
//	clusters:
//	  - name: prod-us-east
//	    kubeconfig: /home/alice/.kube/prod-us-east.yaml
//	    context: prod-us-east
//	  - name: staging
//	    kubeconfig: ~/.kube/config
//	    context: staging
type Config struct {
	// Clusters is the list of clusters MaKlaude is configured to operate.
	Clusters []Spec `yaml:"clusters"`
}

// Spec describes a single cluster by reference. It intentionally
// holds no credentials — only a filesystem path to a kubeconfig file and the
// context to select within it.
type Spec struct {
	// Name is a unique, human-friendly identifier for the cluster. It is used
	// to address the cluster throughout MaKlaude and must be unique across the
	// config.
	Name string `yaml:"name"`
	// Kubeconfig is the filesystem path to the kubeconfig file granting access
	// to the cluster. A leading "~" is expanded to the user's home directory.
	// The file's contents are never read or stored by this package.
	Kubeconfig string `yaml:"kubeconfig"`
	// Context is the name of the kubeconfig context to use for this cluster.
	Context string `yaml:"context"`
}

// LoadConfig reads and parses the YAML configuration file at path. It does not
// validate the semantic content of the config beyond YAML well-formedness;
// call [Config.Validate] or use [NewRegistry] / [NewRegistryFromFile] for full
// validation.
//
// A missing file or malformed YAML fails loudly with a wrapped, actionable
// error.
func LoadConfig(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied config, not user input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cluster config: file %q does not exist", path)
		}
		return nil, fmt.Errorf("cluster config: cannot open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	cfg, err := ParseConfig(f)
	if err != nil {
		return nil, fmt.Errorf("cluster config %q: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig decodes a Config from r. It rejects unknown fields so that typos
// in the config surface as clear errors rather than being silently ignored.
func ParseConfig(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	return &cfg, nil
}
