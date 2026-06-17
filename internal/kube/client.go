// Package kube turns a validated [cluster.Handle] into a live, strictly
// read-only Kubernetes client.
//
// MaKlaude's foundational safety promise is that its observation layer can
// never mutate a cluster. This package makes that promise structural rather
// than merely conventional:
//
//   - The public surface exposes only read operations (get/list/watch). There
//     is no exported method that creates, updates, patches, or deletes anything,
//     and the underlying client-go clientset is never handed out, so a caller
//     simply has no way to express a write.
//
//   - As defense in depth, every client's HTTP transport is wrapped by a
//     read-only guard (see [ErrWriteForbidden]) that rejects any request whose
//     verb is not GET/HEAD/OPTIONS before it reaches the network. Even if a
//     future code path obtained a writable client built on this config, the
//     mutating request would be refused at the wire.
//
// Each [Client] is built from a single cluster's kubeconfig path and context
// and owns its own rest.Config, transport, and clientset. There is no shared or
// global mutable state between clients, so clusters are fully isolated: building
// or using one client can never affect another.
//
// Reachability and configuration problems are never swallowed. They surface as
// clear, wrapped, actionable errors that name the cluster and unwrap to the
// package sentinels [ErrBuildConfig] and [ErrUnreachable].
package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// Package sentinel errors. Every failure this package produces wraps one of
// these so callers can branch on the failure class with errors.Is.
var (
	// ErrBuildConfig wraps failures to construct a usable client from a cluster
	// handle (missing/invalid kubeconfig, unknown context, etc.). These are
	// configuration problems, distinct from a cluster simply being unreachable.
	ErrBuildConfig = errors.New("kube: cannot build client")

	// ErrUnreachable wraps failures to reach or authenticate against a cluster's
	// API server when performing a request. These are runtime/connectivity
	// problems, distinct from a malformed configuration.
	ErrUnreachable = errors.New("kube: cluster unreachable")
)

// Client is a strictly read-only Kubernetes client for a single cluster.
//
// It is created with [NewClient] from a [cluster.Handle] and exposes only
// read operations. The client is safe for concurrent use and holds no mutable
// state of its own. Two clients built from different handles share nothing.
type Client struct {
	// clusterName is the registered name of the cluster, used to make errors
	// and logs unambiguous in a multi-cluster deployment.
	clusterName string
	// clientset is the underlying typed client-go clientset. It is kept
	// unexported and never returned, so the only way to act on the cluster is
	// through this type's read-only methods.
	clientset kubernetes.Interface
}

// NewClient builds a read-only [Client] for the cluster described by h.
//
// It loads the cluster's kubeconfig from the handle's resolved path, selects
// the handle's context, installs the read-only transport guard, and constructs
// a typed clientset against the resulting configuration. No network call is
// made here; reachability is verified lazily by the read methods (or eagerly by
// [Client.CheckReachable]).
//
// A nil handle, an unreadable/invalid kubeconfig, or an unknown context fails
// with an error wrapping [ErrBuildConfig] that names the cluster.
func NewClient(h *cluster.Handle) (*Client, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: nil cluster handle", ErrBuildConfig)
	}

	restCfg, err := restConfigForHandle(h)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: constructing clientset: %w", ErrBuildConfig, h.String(), err)
	}

	return &Client{
		clusterName: h.Name(),
		clientset:   clientset,
	}, nil
}

// restConfigForHandle constructs a *rest.Config from a handle's kubeconfig path
// and context, with the read-only transport guard installed. It is the single
// point where a cluster's connection parameters are assembled, so the guard can
// never be bypassed by a caller.
func restConfigForHandle(h *cluster.Handle) (*rest.Config, error) {
	// Load strictly from the handle's explicit kubeconfig path and context.
	// Defaulting to in-cluster config or $KUBECONFIG is deliberately avoided so
	// a client only ever talks to the cluster the operator configured.
	loadingRules := &clientcmd.ClientConfigLoadingRules{
		ExplicitPath: h.Kubeconfig(),
	}
	overrides := &clientcmd.ConfigOverrides{
		CurrentContext: h.Context(),
	}
	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("%w for %s: %w", ErrBuildConfig, h.String(), err)
	}

	// Defense in depth: wrap the transport so only read verbs ever reach the
	// network. WrapTransport composes with any existing transport (TLS, auth),
	// and the guard is the outermost layer, so it sees every request first.
	restCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return newReadOnlyTransport(rt)
	}

	return restCfg, nil
}

// Name returns the registered name of the cluster this client talks to.
func (c *Client) Name() string { return c.clusterName }

// CheckReachable verifies that the cluster's API server is reachable and
// responding, using a lightweight read (server version discovery). It performs
// a real GET against the API server and is the canonical health probe for a
// configured cluster.
//
// On success it returns the reported server version. On failure it returns an
// error wrapping [ErrUnreachable] that names the cluster — connectivity, TLS,
// and auth problems are surfaced, never swallowed.
func (c *Client) CheckReachable(_ context.Context) (*version.Info, error) {
	// ServerVersion issues a GET to /version; the read-only guard permits it.
	info, err := c.clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", ErrUnreachable, c.clusterName, err)
	}
	return info, nil
}

// ListNamespaces returns every namespace in the cluster. It is a read-only
// (list) operation. Connectivity failures surface as an error wrapping
// [ErrUnreachable] naming the cluster.
func (c *Client) ListNamespaces(ctx context.Context) ([]corev1.Namespace, error) {
	list, err := c.clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w %q: listing namespaces: %w", ErrUnreachable, c.clusterName, err)
	}
	return list.Items, nil
}

// ListPods returns the pods in the given namespace. An empty namespace lists
// pods across all namespaces. It is a read-only (list) operation; connectivity
// failures surface as an error wrapping [ErrUnreachable] naming the cluster.
func (c *Client) ListPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	list, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w %q: listing pods in namespace %q: %w",
			ErrUnreachable, c.clusterName, namespace, err)
	}
	return list.Items, nil
}

// ListNodes returns every node in the cluster. It is a read-only (list)
// operation; connectivity failures surface as an error wrapping
// [ErrUnreachable] naming the cluster.
func (c *Client) ListNodes(ctx context.Context) ([]corev1.Node, error) {
	list, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w %q: listing nodes: %w", ErrUnreachable, c.clusterName, err)
	}
	return list.Items, nil
}

// GetNamespace returns a single namespace by name. It is a read-only (get)
// operation; failures surface as an error wrapping [ErrUnreachable] naming the
// cluster (this includes the API server's "not found" response).
func (c *Client) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	ns, err := c.clientset.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w %q: getting namespace %q: %w", ErrUnreachable, c.clusterName, name, err)
	}
	return ns, nil
}
