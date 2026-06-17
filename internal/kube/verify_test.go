package kube

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestWriteProbeClientForHandle_NilHandle proves the verification seam fails
// loudly (with a wrapped ErrBuildConfig) rather than panicking on a nil handle.
func TestWriteProbeClientForHandle_NilHandle(t *testing.T) {
	_, err := WriteProbeClientForHandle(nil)
	if !errors.Is(err, ErrBuildConfig) {
		t.Fatalf("expected ErrBuildConfig for a nil handle, got: %v", err)
	}
}

// TestWriteProbeClientForHandle_DeleteRefused is the in-process structural proof
// behind the e2e `assertWriteRefused` check: a FULL, write-capable clientset
// built by WriteProbeClientForHandle carries the same read-only transport guard
// every NewClient client carries, so a mutating call (DELETE) is refused with
// ErrWriteForbidden BEFORE any request reaches the network.
//
// The kubeconfig points at an unroutable address, so if the guard were absent
// the DELETE would fail with a connection error instead — proving the rejection
// here is the guard's doing, not the network's. This runs in the fast unit suite
// (no kind, no live API server), giving the production clientset path the same
// active-refusal coverage the e2e proves against a real cluster.
func TestWriteProbeClientForHandle_DeleteRefused(t *testing.T) {
	kubeconfig := writeKubeconfig(t, "https://127.0.0.1:1")
	h := handleFor(t, "write-probe", kubeconfig, "maklaude")

	cs, err := WriteProbeClientForHandle(h)
	if err != nil {
		t.Fatalf("building write-probe client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = cs.CoreV1().Pods("default").Delete(ctx, "anything", metav1.DeleteOptions{})
	if err == nil {
		t.Fatal("expected the DELETE to be refused by the read-only guard, got nil error")
	}
	// client-go can reconstruct/obscure the underlying transport error, so match
	// the sentinel's stable message as a fallback after the errors.Is check.
	if !errors.Is(err, ErrWriteForbidden) &&
		!strings.Contains(err.Error(), ErrWriteForbidden.Error()) &&
		!strings.Contains(err.Error(), "mutating request blocked") {
		t.Fatalf("expected the DELETE to be refused by the read-only transport guard "+
			"(ErrWriteForbidden), got: %v", err)
	}
}
