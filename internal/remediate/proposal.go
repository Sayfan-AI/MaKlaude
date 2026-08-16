// Package remediate turns a diagnosed root cause into zero or more structured,
// previewable [Proposal]s — concrete remediation actions a human can inspect
// and approve — using deterministic rules only.
//
// It is the fourth interpretive layer above collection, and the first that
// contemplates changing anything. The health package gathers raw facts; detect
// turns each fact into a finding; correlate groups findings into incidents;
// diagnose proposes ranked root causes. All four are read-only in spirit and in
// fact. This package remains read-only too: it PLANS mutating actions, it never
// performs one. Nothing here holds a client, opens a connection, or touches a
// cluster — [Propose] is a pure function of (snapshot, hypothesis), exactly as
// [diagnose.Diagnose] is a pure function of (snapshot, incident). Execution is a
// separate, later, explicitly-gated concern; keeping planning pure is what makes
// a proposal safe to compute continuously, cheap to unit-test for exact output,
// and possible to show a human BEFORE anything is at stake.
//
// # What a proposal carries, and why
//
// A human being asked to approve a mutating action against a production cluster
// needs more than a verb. Every proposal therefore carries, as typed fields
// rather than prose a caller must parse:
//
//   - A [Target] — cluster, kind, namespace, name, and the object's
//     resourceVersion at the moment the proposal was computed. The
//     resourceVersion is the optimistic-concurrency token: it is what lets a
//     later executor refuse an action whose object has changed since the
//     snapshot it was reasoned about.
//   - An [Operation] — the exact operation, from a small closed catalog. It is a
//     stable enum, not a shell command, so nothing downstream has to interpret a
//     string to know what will happen.
//   - An [Proposal.Intent] and [Proposal.ExpectedEffect] — why MaKlaude wants to
//     do this, and what the cluster will look like afterwards, both in plain
//     language an operator can check against their own judgment.
//   - A [Reversibility] class — whether the action can be undone, leaves a
//     controller to rebuild what it removed, or is permanent. This is the single
//     most important field for a human deciding how much scrutiny to apply, and
//     it is why proposals are ordered safest-first.
//   - [Proposal.Preconditions] — the conditions that must still hold at
//     execution time. A proposal is computed against a snapshot that is already
//     seconds old by the time a human reads it; preconditions are how an
//     executor confirms the world has not moved underneath the plan.
//
// # The catalog is deliberately small
//
// Only four operations exist, every one of them chosen because its blast radius
// is understood and bounded: restart a Deployment's rollout, roll a Deployment
// back one revision, delete a single controller-owned failed pod, and cordon a
// NotReady node. Deleting PVCs or namespaces, draining a node with eviction,
// scaling to zero, and editing resource limits are all deliberately absent.
//
// The corresponding property is that a diagnosis outside the catalog yields
// NOTHING. An out-of-memory kill is diagnosed confidently by the layer below and
// produces no proposal here, because the fix (raising a memory limit) is not an
// operation this package is willing to plan; a restart would merely re-OOM.
// Insufficient cluster capacity likewise produces nothing. Returning an empty
// slice is a correct, expected, first-class outcome — over-reach is the failure
// mode this layer exists to avoid, so a cause with no safe action says so rather
// than reaching for the nearest available verb.
//
// # Determinism and stable identity
//
// Both properties mirror the layers below, for the same reasons. Given a fixed
// snapshot and a fixed hypothesis, [Propose] always returns the same proposals,
// carrying the same preconditions in the same order, sorted the same way: by
// [Reversibility] ascending (safest first) and then by [ProposalIdentity]
// ascending as a fully decisive tiebreak. Proposals never read a clock — each
// inherits its hypothesis's detection time — so planning stays a pure function
// of its input and the output is byte-stable.
//
// A proposal's identity is derived purely from its operation and its target's
// cluster/kind/namespace/name. It deliberately excludes the resourceVersion, the
// hypothesis that justified it, the confidence, the wording, and the evidence —
// all of which shift while the underlying situation persists. So "restart
// deployment web in cluster prod" is ONE proposal with ONE identity across
// collection cycles and across however many hypotheses independently arrive at
// it, which is what lets the approval gate track a single pending decision
// rather than re-asking a human every cycle.
package remediate

import (
	"strconv"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

// Operation is the exact mutating action a [Proposal] plans, drawn from a small
// closed catalog. It is a stable string enum rather than free text or a rendered
// command so consumers can branch on it, an allowlist can name it, and it can
// compose into a stable [ProposalIdentity]. The values are lowercase and
// delimiter-free so they slot cleanly into an identity key.
type Operation string

const (
	// OpRolloutRestart restarts a Deployment's pods by triggering a fresh rollout
	// (the API-level equivalent of `kubectl rollout restart`). The Deployment's
	// spec is otherwise unchanged, and its own rolling-update strategy governs the
	// replacement, so the workload is not taken down.
	OpRolloutRestart Operation = "rolloutrestart"

	// OpRollbackRevision rolls a Deployment back to its immediately previous
	// revision (the API-level equivalent of `kubectl rollout undo`). It is the
	// answer to a bad rollout: the previous revision is known-good precisely
	// because it was running before.
	OpRollbackRevision Operation = "rollbackrevision"

	// OpDeletePod deletes a single pod. It is only ever proposed for a pod that
	// has already failed AND has a controller to recreate it — see
	// [ReversibilityRecreatedByController] for why that pairing is what makes the
	// action acceptable at all.
	OpDeletePod Operation = "deletepod"

	// OpCordonNode marks a node unschedulable so no new pods are placed on it.
	// Pods already running on the node are left alone: cordoning is not draining,
	// and eviction is deliberately outside this catalog.
	OpCordonNode Operation = "cordonnode"
)

// String renders the operation as its stable token, so it can be used directly
// in identities, logs, and human-facing renderings.
func (o Operation) String() string { return string(o) }

// Catalog returns every operation this package can plan, in the order the constants
// are declared. The returned slice is a fresh copy, so a caller cannot edit the
// catalog by editing what it was handed.
//
// It exists because "is this operation one MaKlaude is built to perform at all" is a
// question asked outside this package — by a scorer reading an audit record after the
// fact, which has only the recorded token and no proposal to consult. Answering it
// from a private switch somewhere else would mean a second list to keep in step with
// this one.
//
// Note what this is NOT: it is not the set of operations that may run unattended.
// [autonomy] keeps its own deliberately separate switch for that, so growing this
// catalog does not silently widen what an operator's rules can name — see the comment
// on `autonomy.catalogOperation`. The two lists answer different questions and should
// not be unified.
func Catalog() []Operation {
	return []Operation{OpRolloutRestart, OpRollbackRevision, OpDeletePod, OpCordonNode}
}

// InCatalog reports whether op is one of this package's catalog operations. An empty
// or unrecognized token is not, which is the fail-closed direction: a consumer that
// gates on catalog membership should treat an operation it cannot place as outside it.
func InCatalog(op Operation) bool {
	for _, candidate := range Catalog() {
		if candidate == op {
			return true
		}
	}
	return false
}

// Reversibility classifies what it would take to undo a [Proposal]'s effect. It
// is the field a human approver should read first, and the primary sort key for
// proposals, so the levels are ordered by increasing risk: the zero value is the
// safest class, and sorting ascending puts the safest actions in front of a
// human first.
type Reversibility int

const (
	// ReversibilityReversible marks an action whose effect can be undone by a
	// single opposite action, restoring the prior state: a cordoned node can be
	// uncordoned, a rolled-back Deployment can be rolled forward again. Nothing is
	// destroyed.
	ReversibilityReversible Reversibility = iota

	// ReversibilityRecreatedByController marks an action that destroys an object
	// permanently but whose *function* is restored automatically, because a
	// controller's whole job is to observe the absence and rebuild it. Deleting a
	// ReplicaSet-owned pod is the canonical case: that exact pod is gone for good
	// — its name, its identity, its logs — but a replacement appears without
	// anyone acting. It is genuinely riskier than reversible (the object itself is
	// unrecoverable) and genuinely safer than irreversible (the workload is not),
	// which is exactly why it is its own class rather than being rounded to
	// either neighbour.
	ReversibilityRecreatedByController

	// ReversibilityIrreversible marks an action whose effect cannot be undone and
	// which nothing will repair on its own — deleting a PVC, a namespace, or an
	// unowned pod. No operation in this package's catalog is currently of this
	// class; the level exists so the classification is complete, so a proposal can
	// never be silently mis-sorted as safer than it is if the catalog grows, and
	// so the approval gate can hold irreversible actions to a higher bar from the
	// day the first one appears.
	ReversibilityIrreversible
)

// String renders the reversibility as a stable lowercase token. The tokens are
// part of the package's contract: test fixtures and human-facing renderings rely
// on them, so they must not change casually.
func (r Reversibility) String() string {
	switch r {
	case ReversibilityReversible:
		return "reversible"
	case ReversibilityRecreatedByController:
		return "recreated-by-controller"
	case ReversibilityIrreversible:
		return "irreversible"
	default:
		return "reversibility(" + strconv.Itoa(int(r)) + ")"
	}
}

// Target identifies the single Kubernetes object a [Proposal] would act on. It
// is a typed value rather than a rendered command so a later executor can act on
// it without re-parsing, and so an allowlist can match on its fields.
//
// A proposal always has exactly one target. An action that would touch several
// objects is several proposals, each separately previewable and separately
// approvable — a human approving "cordon node-a" has not thereby approved
// anything about the pods on it.
type Target struct {
	// Cluster is the registered name of the cluster the object lives in. It is
	// carried explicitly (rather than assumed from context) because multi-cluster
	// is a first-class concern and a mutating action pointed at the wrong cluster
	// is the worst failure this system could have.
	Cluster string

	// Kind is the object's kind in lowercase, stable form ("deployment", "pod",
	// "node"), matching [detect.Object.Kind].
	Kind string

	// Namespace is the object's namespace, empty for cluster-scoped objects such
	// as nodes.
	Namespace string

	// Name is the object's name.
	Name string

	// ResourceVersion is the object's resourceVersion in the snapshot the proposal
	// was computed from. It is the optimistic-concurrency token: an executor that
	// sends it as a precondition will have the action rejected if the object has
	// been modified since — by a human, by a controller, or by an earlier
	// remediation. It is deliberately NOT part of [ProposalIdentity], because a
	// proposal recomputed next cycle against a bumped resourceVersion is the same
	// proposal.
	ResourceVersion string
}

// String renders the target as "kind/namespace/name", omitting the namespace
// segment for cluster-scoped objects — the same compact form [detect.Object]
// uses, kept consistent on purpose.
func (t Target) String() string {
	if t.Namespace == "" {
		return t.Kind + "/" + t.Name
	}
	return t.Kind + "/" + t.Namespace + "/" + t.Name
}

// PreconditionKind names a condition that must still hold when a [Proposal] is
// executed. It is a small string enum so an executor can implement one check per
// kind and be exhaustive over them, rather than pattern-matching prose.
type PreconditionKind string

const (
	// PreconditionUnchanged requires the target object's resourceVersion to still
	// equal [Precondition.Expect]. It is present on every proposal: if the object
	// changed after the snapshot, the reasoning that produced the proposal was
	// about a cluster that no longer exists.
	PreconditionUnchanged PreconditionKind = "unchanged"

	// PreconditionPodCrashLooping requires the named pod (in [Precondition.Expect],
	// as "namespace/name") to still be crashlooping. A crashloop that resolved
	// itself needs no restart.
	PreconditionPodCrashLooping PreconditionKind = "podcrashlooping"

	// PreconditionPodFailed requires the target pod to still be in a failed or
	// evicted state. Deleting a pod that has recovered would destroy a working one.
	PreconditionPodFailed PreconditionKind = "podfailed"

	// PreconditionPodHasController requires the target pod to still be owned by a
	// controller that will recreate it. Without the controller the same deletion is
	// [ReversibilityIrreversible] rather than
	// [ReversibilityRecreatedByController] — a different action than the one that
	// was approved, so it must be rechecked and not merely assumed.
	PreconditionPodHasController PreconditionKind = "podhascontroller"

	// PreconditionNodeNotReady requires the target node to still be NotReady.
	// Cordoning a node that has recovered would needlessly remove capacity.
	PreconditionNodeNotReady PreconditionKind = "nodenotready"

	// PreconditionNodeSchedulable requires the target node to still be
	// schedulable, i.e. not already cordoned — by a human, or by an earlier run of
	// this same proposal.
	PreconditionNodeSchedulable PreconditionKind = "nodeschedulable"

	// PreconditionRevisionExists requires the rollback target revision (in
	// [Precondition.Expect], as a decimal string) to still exist for the target
	// Deployment. Kubernetes prunes old ReplicaSets past a Deployment's revision
	// history limit, so a revision that existed at proposal time can be gone by
	// approval time.
	PreconditionRevisionExists PreconditionKind = "revisionexists"
)

// Precondition is one condition that must still hold when a [Proposal] is
// executed. It pairs a machine-checkable [PreconditionKind] and expected value
// with a human-readable description, so the same value serves the executor's
// gate and the approval preview a person actually reads.
type Precondition struct {
	// Kind names the check to perform. See [PreconditionKind].
	Kind PreconditionKind

	// Expect is the machine-comparable value the check compares against — a
	// resourceVersion, a "namespace/name", a revision number as a decimal string.
	// It is empty for kinds that are self-contained (the target alone says what to
	// check).
	Expect string

	// Description states the condition in plain language, for the approval
	// preview. It is derived at construction and is deliberately not the thing an
	// executor branches on.
	Description string
}

// ProposalIdentity is the stable, deterministic key for a [Proposal]. It is what
// makes an action the SAME proposed action across collection cycles: it is
// derived purely from the [Operation] and the [Target]'s cluster, kind,
// namespace, and name.
//
// It intentionally ignores the target's resourceVersion, the hypothesis and
// confidence that justified the action, the wording, and the evidence — all of
// which shift while the underlying situation persists. So a pending approval
// survives the cluster ticking over underneath it, two different hypotheses that
// independently arrive at the same action collapse to one decision rather than
// two, and an approval gate can dedup rather than re-asking a human every cycle.
//
// ProposalIdentity is a comparable value (a plain string under the hood) so it
// can be used directly as a map key.
type ProposalIdentity string

// newProposalIdentity composes a proposal identity from its operation and
// target. The "proposal" prefix keeps the key namespaced and self-describing (so
// it never collides with a finding, incident, or hypothesis identity if they are
// stored side by side), the cluster is included explicitly so identities never
// collide across clusters, and the operation distinguishes different actions
// against one object.
func newProposalIdentity(op Operation, t Target) ProposalIdentity {
	return ProposalIdentity("proposal|" + string(op) + "|" + t.Cluster + "|" + t.String())
}

// Proposal is a single, deterministic, previewable remediation action: what
// MaKlaude would do about one diagnosed cause, on exactly one object, and
// everything a human needs to decide whether to let it. It is a plain value — no
// behaviour, no live references, no client — so it is trivially serializable and
// comparable, which the approval gate, the audit trail, and the executor all
// rely on.
//
// A proposal is a plan and never an act. Constructing one changes nothing.
type Proposal struct {
	// Identity is the stable dedup key. The same proposed action produces the same
	// Identity on every cycle. See [ProposalIdentity].
	Identity ProposalIdentity

	// Hypothesis is the identity of the diagnosis that justified this action. It
	// is the audit link back through the hypothesis to its incident and findings —
	// so "why was this proposed?" is answerable without re-deriving anything.
	//
	// When two hypotheses independently arrive at the same action they collapse to
	// one proposal (see [ProposalIdentity]), and this names the most-confident of
	// them.
	Hypothesis diagnose.HypothesisIdentity

	// Incident is the identity of the incident behind that hypothesis, carried
	// through so a consumer can group proposals by incident without a lookup.
	Incident correlate.IncidentIdentity

	// Cause is the root-cause class the hypothesis proposed. It is carried so an
	// allowlist can be written in terms an operator recognises ("auto-approve a
	// rollback when the cause is a bad image") without dereferencing the
	// hypothesis.
	Cause diagnose.Cause

	// Confidence is the justifying hypothesis's confidence in that cause. It is
	// deliberately NOT the confidence that the action is correct — it is how sure
	// MaKlaude is about the *diagnosis* the action responds to, which is what a
	// human approving the action needs to weigh.
	Confidence diagnose.Confidence

	// Cluster is the registered name of the cluster this proposal concerns,
	// matching [Target.Cluster]. A proposal never spans clusters.
	Cluster string

	// Operation is the exact action to perform. See [Operation].
	Operation Operation

	// Target is the single object the action would touch. See [Target].
	Target Target

	// Reversibility is how hard the action would be to undo. See [Reversibility].
	// Proposals are sorted by it ascending, so the safest are presented first.
	Reversibility Reversibility

	// Title is a short, stable human-readable label for the action (for example
	// "Restart deployment rollout"), suitable as an approval-request subject line.
	// It is deliberately NOT part of the identity.
	Title string

	// Intent explains, in plain language, why MaKlaude wants to take this action
	// given the diagnosis. It answers "why this?".
	Intent string

	// ExpectedEffect describes what the cluster should look like after the action
	// succeeds. It answers "what will happen?", and is what a human checks their
	// own understanding against before approving.
	ExpectedEffect string

	// Preconditions are the conditions that must still hold at execution time,
	// in a stable order. An executor must check every one and refuse the action if
	// any fails. See [Precondition].
	Preconditions []Precondition

	// Evidence is the subset of the hypothesis's findings that bear on this
	// specific action, in the hypothesis's own stable order. It is what makes a
	// proposal auditable: an operator can see exactly which observations lead to
	// this exact action on this exact object. The slice is a fresh copy; mutating
	// it does not affect the hypothesis.
	Evidence []detect.Finding

	// ProposedAt is the hypothesis's detection time, carried through (and thus,
	// transitively, the snapshot's collection time). Proposals never read their own
	// clock, so planning stays a pure function of its input.
	ProposedAt time.Time
}
