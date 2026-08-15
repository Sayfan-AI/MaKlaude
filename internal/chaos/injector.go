package chaos

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/Sayfan-AI/MaKlaude/internal/cluster"
	"github.com/Sayfan-AI/MaKlaude/internal/kube"
)

// Injected records what a single successful injection actually asked the API
// server to do.
//
// Like [kube.Outcome] it is a plain, fully-resolved value rather than the created
// object: the record's question is "what did MaKlaude send, to which cluster,
// naming which object, for real or as a preview?", and answering it must not depend
// on re-reading anything from a cluster that is by then deliberately broken.
//
// It carries no timestamp. Clocks are read by the layer that records events, not
// by the layer that composes requests, so an injection is a pure function of its
// inputs and a dry-run preview is byte-identical to the injection it previews.
type Injected struct {
	// Cluster is the registered name of the cluster the experiment was created on.
	Cluster string

	// Acknowledgement is the human's verbatim eligibility sentence for that cluster.
	// The record quotes consent rather than asserting it.
	Acknowledgement string

	// Request is the experiment as validated, so a reader can re-derive the object
	// name and compare two records without parsing the CR.
	Request Experiment

	// Kind, Namespace and Name identify the created CR.
	Kind      Kind
	Namespace string
	Name      string

	// UID is the created object's unique identity, and the precondition
	// [Injector.Remove] tears down against. Empty under a dry run, where nothing
	// was created.
	UID string

	// ResourceVersion is the created object's version as the API server returned it.
	ResourceVersion string

	// Scope is the rendered [kube.WriteScope] the create ran under — the exact
	// method and path the transport admitted, and whether it was preview-only.
	Scope string

	// DryRun reports whether this was a server-side preview. A true value means no
	// fault was injected: the API server validated the object against real admission
	// controllers, including any Chaos Mesh webhook, and discarded it.
	DryRun bool
}

// Removal records the outcome of a teardown.
type Removal struct {
	// Cluster, Kind, Namespace and Name identify what was torn down.
	Cluster   string
	Kind      Kind
	Namespace string
	Name      string

	// Scope is the rendered [kube.WriteScope] the delete ran under.
	Scope string

	// AlreadyAbsent reports that the object was already gone when the delete was
	// attempted. It is a success, not a failure — see [Injector.Remove] — and it is
	// surfaced rather than smoothed over because "torn down" and "was never there"
	// are different facts about a cluster.
	AlreadyAbsent bool

	// DryRun reports whether the delete was a server-side preview, in which case the
	// experiment is still live.
	DryRun bool
}

// Injector creates and deletes Chaos Mesh experiments on exactly one
// chaos-eligible cluster.
//
// It is the chaos counterpart of [kube.Executor] and shares its shape on purpose:
// the kill switch is the same [kube.ExecuteMode] (there is no separate chaos
// on/off knob to leave in the wrong position), each action builds its own client
// scoped to that action's single request and drops it, and the target cluster is
// fixed at construction so no call can redirect it.
//
// It differs in what it holds. An Executor holds a [cluster.Handle] — any cluster
// MaKlaude was configured with. An Injector holds a [cluster.ChaosTarget], which
// only exists for a cluster whose config carries a human-written eligibility
// acknowledgement, so the ineligible case is a compile-time absence of an argument
// rather than a runtime check somebody has to remember.
type Injector struct {
	// target is the sole cluster this injector may break, and the proof a human
	// said so.
	target cluster.ChaosTarget

	// mode is the shared write-path kill switch, fixed at construction. It is never
	// [kube.ExecuteDisabled] on a constructed Injector — that case fails in
	// [NewInjector] rather than producing an inert object.
	mode kube.ExecuteMode
}

// NewInjector builds an [Injector] for the cluster the target names.
//
// It refuses to build anything unless mode explicitly permits execution, and
// reuses [kube.ErrExecutorDisabled] to say so rather than minting a chaos-specific
// sentinel: a caller asking "is MaKlaude's write path off?" should get one answer,
// not one per package. Passing the zero value — a forgotten field, an unset
// environment variable, a new call site — yields no injector at all, so a
// deployment that has not opted in holds nothing that can inject.
//
// A nil target, or one whose handle cannot yield a usable client, fails rather
// than deferring the problem to the first experiment: an injector that exists and
// can never act is a worse thing to hand a decision path than a construction
// error. The probe uses the zero-value scope, which grants nothing mutating, so
// construction checks reachability without acquiring authority.
func NewInjector(target cluster.ChaosTarget, mode kube.ExecuteMode) (*Injector, error) {
	if target == nil {
		return nil, fmt.Errorf("%w: nil chaos target", cluster.ErrChaosIneligible)
	}
	switch mode {
	case kube.ExecuteDryRun, kube.ExecuteEnabled:
		// Explicitly opted in.
	case kube.ExecuteDisabled:
		return nil, fmt.Errorf("%w for chaos on %s", kube.ErrExecutorDisabled, target.Handle().String())
	default:
		return nil, fmt.Errorf("%w for chaos on %s: unknown execute mode %d",
			kube.ErrExecutorDisabled, target.Handle().String(), int(mode))
	}

	if _, err := kube.ChaosRestConfig(target, kube.WriteScope{}); err != nil {
		return nil, err
	}

	return &Injector{target: target, mode: mode}, nil
}

// Name returns the registered name of the cluster this injector breaks.
func (i *Injector) Name() string { return i.target.Handle().Name() }

// Mode returns the write-path kill switch setting. It is never
// [kube.ExecuteDisabled].
func (i *Injector) Mode() kube.ExecuteMode { return i.mode }

// dryRun reports whether this injector's writes are previews.
func (i *Injector) dryRun() bool { return i.mode == kube.ExecuteDryRun }

// dryRunOptions returns the API options value for the injector's mode: the
// server-side dryRun=All list for a preview, nil for a real write.
func (i *Injector) dryRunOptions() []string {
	if i.dryRun() {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// gvr is the dynamic client's coordinate for a kind.
func (k Kind) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: APIGroup, Version: APIVersion, Resource: k.Resource()}
}

// collectionPath is where a create goes: a POST addresses the COLLECTION, not the
// object, because the object does not exist yet and its name travels in the body.
//
// This is composed from [kube.ChaosAPIPathPrefix] — the same constant the scope
// door verifies against — rather than from a local copy of the group. It must also
// equal the path the dynamic client derives from the same kind's GroupVersionResource,
// and nothing here asserts that: if the two ever disagree the scope guard refuses
// the request before it reaches the network, so a mismatch is a loud failure rather
// than an unguarded write.
func (e Experiment) collectionPath() string {
	return kube.ChaosAPIPathPrefix + APIVersion + "/namespaces/" + e.Namespace + "/" + e.Kind().Resource()
}

// objectPath is where a read or a delete of the experiment's CR goes.
func (e Experiment) objectPath() string {
	return e.collectionPath() + "/" + e.ObjectName()
}

// Inject creates the experiment's custom resource, which is what asks Chaos Mesh
// to break something.
//
// The write is guarded four ways, and the last one is this task's design work:
//
//  1. the cluster is eligible, because an [Injector] cannot exist without a
//     [cluster.ChaosTarget];
//  2. the mode permits writing at all, checked at construction;
//  3. the transport admits exactly one request — POST to this experiment's
//     collection path in this namespace — and refuses every other method, path and
//     namespace, including any mutating request outside the chaos API group (see
//     [kube.ChaosRestConfig]);
//  4. the create is conditioned on the ABSENCE of a conflicting experiment, under a
//     name derived from the experiment itself.
//
// Guard 4 replaces the resourceVersion precondition every other MaKlaude write
// carries, which a create cannot have — see [Experiment.ObjectName] for why the
// derived name is what makes the request idempotent. It is enforced twice and the
// two are not redundant:
//
//   - A read first, through a client built with the ZERO scope, so the client that
//     performs the pre-flight check provably cannot write anything. A live
//     experiment found here fails with [ErrExperimentExists] and nothing is sent.
//     This is the diagnosis: it names the object that is in the way.
//   - The API server's own uniqueness check. The read above is a
//     time-of-check/time-of-use race by construction — an experiment can appear in
//     the gap — so it cannot be the guarantee. The guarantee is that a POST of an
//     existing name returns 409 AlreadyExists, which is also mapped to
//     [ErrExperimentExists]. A replay therefore collides rather than duplicating,
//     whoever wins the race.
//
// Note what this deliberately does not do: it never ADOPTS the existing object. An
// injector that treated "already there" as success would report a fault it did not
// create, with a duration and a selector it did not choose.
func (i *Injector) Inject(ctx context.Context, e Experiment) (*Injected, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}

	name := e.ObjectName()
	kind := e.Kind()

	if err := i.assertAbsent(ctx, e); err != nil {
		return nil, err
	}

	scope := kube.WriteScope{
		Method:        http.MethodPost,
		Path:          e.collectionPath(),
		RequireDryRun: i.dryRun(),
	}
	client, err := i.resourceClient(scope, e)
	if err != nil {
		return nil, err
	}

	created, err := client.Create(ctx, e.object(i.Name(), i.target.Acknowledgement()),
		metav1.CreateOptions{DryRun: i.dryRunOptions()})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("%w: %s %q on cluster %q (the API server rejected the create as a duplicate)",
				ErrExperimentExists, kind, e.Namespace+"/"+name, i.Name())
		}
		return nil, fmt.Errorf("%w: creating %s %q on cluster %q: %w",
			ErrInject, kind, e.Namespace+"/"+name, i.Name(), err)
	}

	return &Injected{
		Cluster:         i.Name(),
		Acknowledgement: i.target.Acknowledgement(),
		Request:         e,
		Kind:            kind,
		Namespace:       e.Namespace,
		Name:            name,
		UID:             string(created.GetUID()),
		ResourceVersion: created.GetResourceVersion(),
		Scope:           scope.String(),
		DryRun:          i.dryRun(),
	}, nil
}

// assertAbsent fails with [ErrExperimentExists] if the experiment's object is
// already on the cluster.
//
// The client it reads through is built with the zero-value scope, so it is
// structurally incapable of mutating anything: the pre-flight check for a write
// cannot itself write. Any error other than "not found" is surfaced rather than
// treated as absence — a create attempted because a read failed is a create
// attempted with no precondition at all.
func (i *Injector) assertAbsent(ctx context.Context, e Experiment) error {
	client, err := i.resourceClient(kube.WriteScope{}, e)
	if err != nil {
		return err
	}

	existing, err := client.Get(ctx, e.ObjectName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("%w: checking whether %s %q already exists on cluster %q: %w",
			ErrInject, e.Kind(), e.Namespace+"/"+e.ObjectName(), i.Name(), err)
	}

	return fmt.Errorf("%w: %s %q is live on cluster %q (uid %s); tear it down or wait for it to finish",
		ErrExperimentExists, e.Kind(), e.Namespace+"/"+e.ObjectName(), i.Name(), existing.GetUID())
}

// Remove deletes an injected experiment's custom resource, which is how a fault is
// given back.
//
// Its precondition is the object's UID, not its resourceVersion, and the swap is
// deliberate. A resourceVersion asks "has this object changed since I reasoned
// about it?", which is the right question for a patch and the wrong one here: a
// live chaos experiment's status is updated by its controller constantly, so an
// RV precondition would make teardown fail precisely because the experiment is
// doing its job. The question teardown actually has is "is this the same object I
// created?" — a name can be recycled, and deleting a stranger's experiment because
// it inherited a name would be a write MaKlaude was never authorised for. That
// question is UID, and a mismatch is refused by the API server with a conflict.
//
// Two outcomes that look like failures and are not:
//
//   - Already absent. Teardown's goal is that no MaKlaude experiment outlives its
//     run, and an object that is already gone satisfies it. The failure teardown
//     must never produce is the opposite one — reporting success while the CR is
//     live — so a NotFound is reported as success WITH [Removal.AlreadyAbsent] set,
//     not silently smoothed into an ordinary success.
//   - A conflict. That is the recycled-name case above, and it means MaKlaude's
//     experiment is already gone; it surfaces as [kube.ErrPreconditionConflict] so a
//     caller can distinguish "mine is gone" from "the delete failed".
//
// The delete overrides neither propagation policy nor grace period, so Chaos Mesh's
// finalizer runs and a persisting fault is reverted before the object disappears.
// Forcing the object away would remove the record while leaving the fault.
func (i *Injector) Remove(ctx context.Context, in Injected) (*Removal, error) {
	if err := validateName("namespace", in.Namespace); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidExperiment, err)
	}
	if err := validateName("name", in.Name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidExperiment, err)
	}
	if in.Kind.Resource() == "" {
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidExperiment, in.Kind)
	}
	if strings.TrimSpace(in.UID) == "" {
		return nil, fmt.Errorf("%w: tearing down %s %q on cluster %q",
			ErrMissingUID, in.Kind, in.Namespace+"/"+in.Name, i.Name())
	}

	scope := kube.WriteScope{
		Method:        http.MethodDelete,
		Path:          objectPathFor(in.Namespace, in.Kind, in.Name),
		RequireDryRun: i.dryRun(),
	}
	client, err := i.clientForScope(scope, in.Kind, in.Namespace)
	if err != nil {
		return nil, err
	}

	uid := types.UID(in.UID)
	removal := &Removal{
		Cluster:   i.Name(),
		Kind:      in.Kind,
		Namespace: in.Namespace,
		Name:      in.Name,
		Scope:     scope.String(),
		DryRun:    i.dryRun(),
	}

	err = client.Delete(ctx, in.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
		DryRun:        i.dryRunOptions(),
	})
	switch {
	case apierrors.IsNotFound(err):
		removal.AlreadyAbsent = true
		return removal, nil
	case apierrors.IsConflict(err):
		return nil, fmt.Errorf("%w: %s %q on cluster %q is no longer uid %s, so it is not the experiment MaKlaude created: %w",
			kube.ErrPreconditionConflict, in.Kind, in.Namespace+"/"+in.Name, i.Name(), in.UID, err)
	case err != nil:
		return nil, fmt.Errorf("%w: deleting %s %q on cluster %q: %w",
			ErrInject, in.Kind, in.Namespace+"/"+in.Name, i.Name(), err)
	}

	return removal, nil
}

// objectPathFor composes the object path from its parts, for the teardown case
// where the caller holds a record rather than the original [Experiment].
func objectPathFor(namespace string, kind Kind, name string) string {
	return kube.ChaosAPIPathPrefix + APIVersion + "/namespaces/" + namespace + "/" + kind.Resource() + "/" + name
}

// resourceClient builds a client for one scoped request against the experiment's
// kind and namespace.
func (i *Injector) resourceClient(scope kube.WriteScope, e Experiment) (dynamic.ResourceInterface, error) {
	return i.clientForScope(scope, e.Kind(), e.Namespace)
}

// clientForScope builds a fresh dynamic client for one scoped request and narrows
// it to one kind in one namespace.
//
// It is dynamic rather than typed for a plain reason: [kube.Executor]'s every
// method funnels through a kubernetes.Interface, and the typed clientset has no
// notion of a chaos-mesh.org resource — the types are not in client-go and pulling
// Chaos Mesh's own API module in would drag controller-runtime along with it. A
// dynamic client speaks unstructured objects over the same rest.Config, and
// therefore through the same transport guard, which is the property that matters
// here.
//
// Like the executor's per-action clientset, this is built per request and dropped:
// a cached write-capable client carries the union of every request's authority for
// as long as it lives, and experiments are rare enough that a TLS handshake nobody
// is waiting on is the right trade.
func (i *Injector) clientForScope(scope kube.WriteScope, kind Kind, namespace string) (dynamic.ResourceInterface, error) {
	restCfg, err := kube.ChaosRestConfig(i.target, scope)
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("%w: constructing chaos client for cluster %q: %w", ErrInject, i.Name(), err)
	}
	return client.Resource(kind.gvr()).Namespace(namespace), nil
}

// MaxDuration reports the longest fault this package will ask for. It is exported
// so a config surface, a proposal renderer, or a doc test can quote the ceiling
// rather than restate it. See [maxDuration].
func MaxDuration() time.Duration { return maxDuration }
