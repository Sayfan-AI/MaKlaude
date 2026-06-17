package kube

import (
	"fmt"

	"k8s.io/client-go/kubernetes"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// WriteProbeClientForHandle builds a FULL, write-capable client-go clientset for
// the cluster described by h, using the exact same rest.Config that
// [NewClient] uses — including the read-only transport guard.
//
// It exists for one purpose: to PROVE the guard. A normal [Client] exposes no
// write methods, so it cannot demonstrate that a write would be refused; this
// helper deliberately hands back the raw clientset so a verification harness can
// ATTEMPT a mutating call (create/delete/...) and assert it fails with
// [ErrWriteForbidden] before any request reaches the API server. Because the
// returned clientset carries the same guarded transport every [NewClient] client
// carries, a refused write here is direct evidence that production clients refuse
// writes too.
//
// This is a verification/defense-in-depth seam, NOT a production path: nothing in
// MaKlaude's pipeline calls it, and it must never be used to actually mutate a
// cluster (the guard ensures it cannot). It returns an error wrapping
// [ErrBuildConfig] if the handle's kubeconfig/context cannot be loaded.
func WriteProbeClientForHandle(h *cluster.Handle) (kubernetes.Interface, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: nil cluster handle", ErrBuildConfig)
	}
	restCfg, err := restConfigForHandle(h)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: constructing write-probe clientset: %w", ErrBuildConfig, h.String(), err)
	}
	return cs, nil
}
