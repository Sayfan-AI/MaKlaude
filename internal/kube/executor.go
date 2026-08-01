package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
)

// Sentinel errors specific to the write path. Everything the executor refuses or
// fails at wraps one of these (or a [WriteScope] sentinel), so a caller can tell
// "MaKlaude declined to try" from "MaKlaude tried and the API server said no" —
// a distinction the approval trail and the audit trail both depend on.
var (
	// ErrExecutorDisabled is returned by [NewExecutor] when execution is not
	// explicitly enabled. It is a CONSTRUCTION error, not a per-call one: a
	// deployment that has not opted in holds no write-capable object at all.
	ErrExecutorDisabled = errors.New("kube: executor disabled (execution not explicitly enabled)")

	// ErrInvalidTarget is returned when a target's namespace or name is missing or
	// is not a valid Kubernetes object name. The executor composes request paths
	// from these values, so validating them is both a correctness check and the
	// thing that stops a crafted name from escaping its [WriteScope] path.
	ErrInvalidTarget = errors.New("kube: invalid execution target")

	// ErrMissingPrecondition is returned when an action is attempted without the
	// target's expected resourceVersion. Every mutating action carries the
	// optimistic-concurrency token; there is no unconditional variant.
	ErrMissingPrecondition = errors.New("kube: missing resourceVersion precondition")

	// ErrInvalidPatch is returned when a caller-supplied patch body is not a JSON
	// object, or attempts to set identity/precondition fields the executor owns.
	ErrInvalidPatch = errors.New("kube: invalid patch body")

	// ErrPreconditionConflict wraps the API server's rejection of an action whose
	// target changed after the snapshot the proposal was reasoned about. It is the
	// expected, healthy outcome of a stale approval — not a malfunction — and is
	// distinct from [ErrExecute] so a caller can re-propose rather than escalate.
	ErrPreconditionConflict = errors.New("kube: target changed since the proposal was made")

	// ErrExecute wraps any other failure of an attempted action (RBAC denial,
	// connectivity, validation by the API server).
	ErrExecute = errors.New("kube: action failed")
)

// ExecuteMode is the kill switch over MaKlaude's entire write path. It has no
// per-cluster or per-action override: whatever a proposal says and whoever
// approved it, nothing mutates a cluster unless the mode permits it.
//
// The zero value is [ExecuteDisabled], so every path that forgets to set a mode
// — a zero-valued config struct, an unset environment variable, a new call site —
// gets the safe posture rather than the useful one.
type ExecuteMode int

const (
	// ExecuteDisabled permits nothing. [NewExecutor] refuses to build a client at
	// all, so a disabled deployment holds no write-capable object to misuse. This
	// is the zero value and the shipped default: propose-on, execute-off.
	ExecuteDisabled ExecuteMode = iota

	// ExecuteDryRun permits previews only. Every action is sent with
	// dryRun=All, and the [WriteScope] refuses any mutating request that lacks it,
	// so the API server validates and admits the change against real admission
	// controllers and then discards it. This is the mode that makes a proposal's
	// preview trustworthy without putting anything at stake.
	ExecuteDryRun

	// ExecuteEnabled permits real, approved mutations. It is the only mode under
	// which a cluster changes, and reaching it requires an operator to say so
	// explicitly.
	ExecuteEnabled
)

// String renders the mode as a stable lowercase token, used in errors, logs, and
// the audit trail.
func (m ExecuteMode) String() string {
	switch m {
	case ExecuteDisabled:
		return "disabled"
	case ExecuteDryRun:
		return "dry-run"
	case ExecuteEnabled:
		return "enabled"
	default:
		return fmt.Sprintf("executemode(%d)", int(m))
	}
}

// ParseExecuteMode maps an operator-supplied token to an [ExecuteMode]. It exists
// so a later config surface can adopt the kill switch without re-deriving the
// vocabulary, and so an unrecognised value is an error rather than a silent
// default in either direction.
//
// An empty value maps to [ExecuteDisabled]: "the operator said nothing" and "the
// operator said off" are the same posture. Note that as of this change nothing in
// MaKlaude's configuration calls this — no config file or flag can enable
// execution yet, which is the strongest form of "unreachable unless explicitly
// enabled" there is.
func ParseExecuteMode(s string) (ExecuteMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "disabled":
		return ExecuteDisabled, nil
	case "dry-run":
		return ExecuteDryRun, nil
	case "enabled":
		return ExecuteEnabled, nil
	default:
		return ExecuteDisabled, fmt.Errorf("%w: unknown execute mode %q (want disabled, dry-run, or enabled)",
			ErrInvalidTarget, s)
	}
}

// Outcome records what a single attempted action actually asked the API server to
// do. It is returned on success and is deliberately a plain, fully-resolved value
// rather than the mutated object: the audit trail's question is "what did MaKlaude
// send, to which cluster, against which object, for real or as a preview?", and
// answering it must not depend on re-reading anything.
type Outcome struct {
	// Cluster is the registered name of the cluster acted on.
	Cluster string

	// Target is the compact "kind/namespace/name" form of the object acted on.
	Target string

	// Scope is the rendered [WriteScope] the action ran under — the exact method
	// and path the transport admitted, and whether it was preview-only.
	Scope string

	// ResourceVersion is the optimistic-concurrency token the action was
	// conditioned on. Because the API server enforced it, its presence here means
	// the object had not changed since the proposal was computed.
	ResourceVersion string

	// DryRun reports whether the action was a server-side preview. A true value
	// means the cluster is unchanged.
	DryRun bool
}

// Executor is a write-capable client for exactly one cluster, and the only type
// in MaKlaude that can mutate one.
//
// It is a deliberate sibling of [Client], not an extension of it. Client's
// promise — no exported write method, plus a transport that refuses every
// mutating verb — is unchanged and unreachable from here: the two types build
// their rest.Configs through different functions that install different guards.
// Nothing an operator does to enable execution loosens the observation path,
// because the observation path does not consult the mode.
//
// Each action builds its own clientset, scoped to that action's single (method,
// path) pair, uses it once, and drops it. That is more work per action than
// caching a clientset, and it is the point: a cached write-capable clientset
// carries the union of every action's authority for as long as it lives, while a
// per-action one carries only the authority for the action a human approved.
// Actions are rare and human-gated, so the cost is a TLS handshake nobody is
// waiting on.
//
// An Executor holds its own [cluster.Handle] and no global state, so clusters
// stay isolated: an executor for one cluster cannot reach another, and building
// one has no effect on any other.
type Executor struct {
	// handle is the single cluster this executor may act on. Every action
	// re-derives its connection parameters from this handle, so the target cluster
	// is fixed at construction and cannot be redirected per call.
	handle *cluster.Handle

	// mode is the kill switch, fixed at construction. It is never
	// [ExecuteDisabled] on a constructed Executor — that case fails in
	// [NewExecutor] instead of producing an inert object.
	mode ExecuteMode
}

// NewExecutor builds a write-capable [Executor] for the cluster described by h.
//
// It refuses to build anything unless mode explicitly permits execution. Passing
// [ExecuteDisabled] — including by passing the zero value, or by forgetting to
// pass anything at all — fails with [ErrExecutorDisabled] and returns no
// executor. That is what makes the write path unreachable rather than merely
// unused: a deployment that has not opted in has no object to call a mutating
// method on, so there is no code path to audit, no flag to re-check at each call
// site, and nothing to accidentally hold onto.
//
// A nil handle or an unparseable kubeconfig/context fails with an error wrapping
// [ErrBuildConfig]. No network call is made here.
func NewExecutor(h *cluster.Handle, mode ExecuteMode) (*Executor, error) {
	if h == nil {
		return nil, fmt.Errorf("%w: nil cluster handle", ErrBuildConfig)
	}
	switch mode {
	case ExecuteDryRun, ExecuteEnabled:
		// Explicitly opted in.
	case ExecuteDisabled:
		return nil, fmt.Errorf("%w for %s", ErrExecutorDisabled, h.String())
	default:
		return nil, fmt.Errorf("%w for %s: unknown execute mode %d", ErrExecutorDisabled, h.String(), int(mode))
	}

	// Fail at construction, not at the first action, if this handle cannot yield a
	// usable config: an executor that exists but can never act is a worse thing to
	// hand an approval gate than a construction error.
	if _, err := restConfigForScope(h, WriteScope{}); err != nil {
		return nil, err
	}

	return &Executor{handle: h, mode: mode}, nil
}

// Name returns the registered name of the cluster this executor acts on.
func (e *Executor) Name() string { return e.handle.Name() }

// Mode returns the executor's kill-switch setting. It is never
// [ExecuteDisabled].
func (e *Executor) Mode() ExecuteMode { return e.mode }

// dryRun reports whether this executor's actions are previews.
func (e *Executor) dryRun() bool { return e.mode == ExecuteDryRun }

// dryRunOptions returns the API options value for the executor's mode: the
// server-side dryRun=All list for a preview, nil for real execution.
func (e *Executor) dryRunOptions() []string {
	if e.dryRun() {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// RestartDeploymentRollout triggers a fresh rollout of a Deployment without
// otherwise changing its spec, by stamping the conventional restartedAt
// annotation onto its pod template — the API-level equivalent of
// `kubectl rollout restart`. The Deployment's own update strategy governs the
// replacement, so the workload is not taken down.
//
// restartedAt is supplied by the caller rather than read from a clock here, so
// the request this produces is a pure function of its inputs and a preview is
// byte-identical to the execution it previews.
//
// resourceVersion is the object's version at proposal time and is enforced by the
// API server: if the Deployment changed since, the action fails with
// [ErrPreconditionConflict] and nothing is applied.
func (e *Executor) RestartDeploymentRollout(ctx context.Context, namespace, name, restartedAt, resourceVersion string) (*Outcome, error) {
	if strings.TrimSpace(restartedAt) == "" {
		return nil, fmt.Errorf("%w: empty restartedAt timestamp", ErrInvalidPatch)
	}
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						restartedAtAnnotation: restartedAt,
					},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding restart patch: %w", ErrInvalidPatch, err)
	}
	return e.PatchDeployment(ctx, namespace, name, body, resourceVersion)
}

// restartedAtAnnotation is the annotation kubectl uses to trigger a rollout
// restart. MaKlaude writes the same key so a restart it performs is
// indistinguishable from — and does not fight with — one an operator performs by
// hand.
const restartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// PatchDeployment applies a strategic-merge patch to a single Deployment,
// conditioned on resourceVersion.
//
// It is one of three primitives the write path exposes, each being exactly one
// API call against exactly one object. Composing a patch body for a specific
// remediation (a rollout restart, a rollback to a previous revision) belongs to
// the layer that understands proposals; this layer's job is to make sure whatever
// is sent goes to the approved object, carries the approved precondition, and
// cannot reach anything else.
//
// The precondition is not optional and not the caller's to supply: the executor
// injects metadata.resourceVersion into the patch body itself, and refuses a body
// that names a different one. So there is no unconditioned variant of this call
// to reach for under time pressure, and no way for a patch to quietly retarget
// itself — a body that sets metadata.name, metadata.namespace, or metadata.uid is
// refused with [ErrInvalidPatch].
func (e *Executor) PatchDeployment(ctx context.Context, namespace, name string, patch []byte, resourceVersion string) (*Outcome, error) {
	if err := validateNamespacedTarget(namespace, name); err != nil {
		return nil, err
	}
	body, err := withResourceVersion(patch, resourceVersion)
	if err != nil {
		return nil, err
	}

	scope := WriteScope{
		Method:        http.MethodPatch,
		Path:          "/apis/apps/v1/namespaces/" + namespace + "/deployments/" + name,
		RequireDryRun: e.dryRun(),
	}
	target := "deployment/" + namespace + "/" + name

	return e.act(scope, target, resourceVersion, func(cs kubernetes.Interface) error {
		_, err := cs.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, body,
			metav1.PatchOptions{DryRun: e.dryRunOptions()})
		return err
	})
}

// CordonNode marks a node unschedulable so the scheduler places no new pods on
// it, conditioned on resourceVersion. Pods already running on the node are left
// alone: cordoning is not draining, and eviction is deliberately outside this
// catalog.
func (e *Executor) CordonNode(ctx context.Context, name, resourceVersion string) (*Outcome, error) {
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	return e.PatchNode(ctx, name, patch, resourceVersion)
}

// PatchNode applies a strategic-merge patch to a single node, conditioned on
// resourceVersion. Nodes are cluster-scoped, so there is no namespace. The same
// precondition injection and retarget refusal as [Executor.PatchDeployment]
// apply.
func (e *Executor) PatchNode(ctx context.Context, name string, patch []byte, resourceVersion string) (*Outcome, error) {
	if err := validateObjectName("name", name); err != nil {
		return nil, err
	}
	body, err := withResourceVersion(patch, resourceVersion)
	if err != nil {
		return nil, err
	}

	scope := WriteScope{
		Method:        http.MethodPatch,
		Path:          "/api/v1/nodes/" + name,
		RequireDryRun: e.dryRun(),
	}

	return e.act(scope, "node/"+name, resourceVersion, func(cs kubernetes.Interface) error {
		_, err := cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, body,
			metav1.PatchOptions{DryRun: e.dryRunOptions()})
		return err
	})
}

// DeletePod deletes a single pod, conditioned on resourceVersion.
//
// The precondition travels as a DeleteOptions precondition rather than in a body,
// and the [WriteScope] pins the exact object path — which is also what excludes a
// collection delete, since the collection's path is a strict prefix of this one
// and the scope matches exactly rather than by prefix.
//
// Whether deleting a given pod is acceptable at all — it must have already failed
// AND have a controller that will recreate it — is a question about the proposal,
// checked by the layer that holds one. This layer enforces the target and the
// precondition.
func (e *Executor) DeletePod(ctx context.Context, namespace, name, resourceVersion string) (*Outcome, error) {
	if err := validateNamespacedTarget(namespace, name); err != nil {
		return nil, err
	}
	if strings.TrimSpace(resourceVersion) == "" {
		return nil, fmt.Errorf("%w: deleting pod %s/%s", ErrMissingPrecondition, namespace, name)
	}

	scope := WriteScope{
		Method:        http.MethodDelete,
		Path:          "/api/v1/namespaces/" + namespace + "/pods/" + name,
		RequireDryRun: e.dryRun(),
	}
	target := "pod/" + namespace + "/" + name

	return e.act(scope, target, resourceVersion, func(cs kubernetes.Interface) error {
		return cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{ResourceVersion: &resourceVersion},
			DryRun:        e.dryRunOptions(),
		})
	})
}

// act is the single funnel every mutating action passes through: it builds a
// clientset whose transport admits only this action's request, runs the action
// once, and classifies the result.
//
// Every write in this package goes through here, which is what makes the scoping
// property auditable — there is one place to read to know that no action can be
// issued without a scope, and no scope can be wider than one request.
func (e *Executor) act(scope WriteScope, target, resourceVersion string, do func(kubernetes.Interface) error) (*Outcome, error) {
	cs, err := e.clientsetForScope(scope)
	if err != nil {
		return nil, err
	}

	if err := do(cs); err != nil {
		if apierrors.IsConflict(err) {
			return nil, fmt.Errorf("%w: %s %q on cluster %q (expected resourceVersion %s): %w",
				ErrPreconditionConflict, scope.Method, target, e.Name(), resourceVersion, err)
		}
		return nil, fmt.Errorf("%w: %s %q on cluster %q: %w",
			ErrExecute, scope.Method, target, e.Name(), err)
	}

	return &Outcome{
		Cluster:         e.Name(),
		Target:          target,
		Scope:           scope.String(),
		ResourceVersion: resourceVersion,
		DryRun:          e.dryRun(),
	}, nil
}

// clientsetForScope builds a fresh clientset for one action, guarded by scope.
func (e *Executor) clientsetForScope(scope WriteScope) (kubernetes.Interface, error) {
	restCfg, err := restConfigForScope(e.handle, scope)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("%w for %s: constructing scoped write clientset: %w",
			ErrBuildConfig, e.handle.String(), err)
	}
	return cs, nil
}

// restConfigForScope assembles a *rest.Config for a single scoped action.
//
// It is deliberately a separate function from [restConfigForHandle] rather than a
// parameterisation of it. The observation path's guard is not conditional on
// anything, so no argument, mode, or future refactor of this function can weaken
// it — the two paths share the loading rules (so both talk only to the cluster the
// operator configured) and nothing else.
func restConfigForScope(h *cluster.Handle, scope WriteScope) (*rest.Config, error) {
	restCfg, err := baseRestConfig(h)
	if err != nil {
		return nil, err
	}

	// Pin JSON for the write path. Left to negotiate, client-go sends built-in
	// request bodies as protobuf — which is the right default for the observation
	// path's large list reads and the wrong one here, for two reasons. The scoped
	// guard has to read a DELETE's DeleteOptions body to verify the dry-run marker
	// the API server will actually honour (see hasServerDryRun), and a binary body
	// leaves that check guessing. And every mutating request MaKlaude sends is
	// something a human approved and may later need to read back out of an audit
	// trail or an apiserver audit log; JSON is legible there and protobuf is not.
	// Reads are untouched — this is set only on the config the executor builds.
	restCfg.ContentType = "application/json"

	restCfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return newScopedWriteTransport(rt, scope)
	}
	return restCfg, nil
}

// validateNamespacedTarget checks both halves of a namespaced target.
func validateNamespacedTarget(namespace, name string) error {
	if err := validateObjectName("namespace", namespace); err != nil {
		return err
	}
	return validateObjectName("name", name)
}

// validateObjectName rejects an empty or malformed Kubernetes object name.
//
// This is a safety check, not only an input check. Request paths are composed
// from these values, so a name containing "/" or ".." would otherwise produce a
// path that is not the object it claims to be — and since the [WriteScope] is
// composed from the same values, a crafted name could make an out-of-scope
// request match its own scope. Constraining both to a DNS-1123 subdomain (no
// slashes, no dots-only segments, no whitespace, no percent-encoding) removes
// that possibility at the boundary.
func validateObjectName(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalidTarget, field)
	}
	if problems := validation.IsDNS1123Subdomain(value); len(problems) > 0 {
		return fmt.Errorf("%w: %s %q is not a valid Kubernetes object name: %s",
			ErrInvalidTarget, field, value, strings.Join(problems, "; "))
	}
	return nil
}

// withResourceVersion injects the optimistic-concurrency precondition into a
// strategic-merge patch body and refuses a body that tries to own it.
//
// Kubernetes enforces metadata.resourceVersion in a patch body as a precondition
// and rejects a mismatch with 409 Conflict, so injecting it here — rather than
// trusting each call site to include it — is what makes "conditioned on the
// snapshot it was reasoned about" a property of the write path instead of a
// convention. The refusals matter as much as the injection: a patch that sets
// metadata.name, metadata.namespace, or metadata.uid is attempting to describe a
// different object than the one the [WriteScope] admits, and a patch that sets a
// different resourceVersion is attempting to relax the precondition. Both are
// refused rather than overwritten, because silently correcting a body that
// disagrees with its own target would hide a real caller bug.
func withResourceVersion(patch []byte, resourceVersion string) ([]byte, error) {
	if strings.TrimSpace(resourceVersion) == "" {
		return nil, fmt.Errorf("%w: patch requires a resourceVersion", ErrMissingPrecondition)
	}
	if len(patch) == 0 {
		return nil, fmt.Errorf("%w: empty patch", ErrInvalidPatch)
	}

	var doc map[string]any
	if err := json.Unmarshal(patch, &doc); err != nil {
		return nil, fmt.Errorf("%w: patch is not a JSON object: %w", ErrInvalidPatch, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: patch is JSON null", ErrInvalidPatch)
	}

	meta := map[string]any{}
	if raw, ok := doc["metadata"]; ok {
		typed, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: patch metadata is not an object", ErrInvalidPatch)
		}
		meta = typed
	}
	for _, field := range []string{"name", "namespace", "uid"} {
		if _, ok := meta[field]; ok {
			return nil, fmt.Errorf("%w: patch may not set metadata.%s (the target is fixed by the approved scope)",
				ErrInvalidPatch, field)
		}
	}
	if existing, ok := meta["resourceVersion"]; ok && existing != resourceVersion {
		return nil, fmt.Errorf("%w: patch sets metadata.resourceVersion %v, expected %q",
			ErrInvalidPatch, existing, resourceVersion)
	}

	meta["resourceVersion"] = resourceVersion
	doc["metadata"] = meta

	body, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("%w: re-encoding patch: %w", ErrInvalidPatch, err)
	}
	return body, nil
}
