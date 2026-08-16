// Package chaos is MaKlaude's deliberate write path: it breaks a cluster on
// purpose so MaKlaude's own behaviour under fault can be measured rather than
// assumed.
//
// Everything that has proved remediation works so far has relied on hand-seeded
// fixtures. Those prove the detectors read a broken cluster correctly. They prove
// nothing about what happens when a cluster breaks *while MaKlaude is working*,
// which is the only condition the system will actually meet in production. This
// package is how that condition gets created, on clusters a human explicitly
// offered up for it.
//
// # Why this is not internal/kube
//
// A fault injector and a remediator are different things with different blast
// radii, so they get different packages, different clients and different
// ServiceAccounts (deploy/rbac/chaos). A mutating request attributed to the chaos
// identity is by construction an experiment; a mutating request attributed to the
// executor identity is by construction an approved fix. Collapsing them into one
// identity would make an audit log unable to tell those apart, which is the one
// question a person reading it after an incident actually has.
//
// What is NOT duplicated is the transport guard. Every write here goes through
// [kube.WriteScope] via [kube.ChaosRestConfig] — the same whole-request pin the
// executor uses, entered through a door that demands a [cluster.ChaosTarget] and
// refuses a mutating scope outside the chaos-mesh.org API group. See
// internal/kube/chaosscope.go for why reusing the guard beats writing a second one.
//
// # What this package can and cannot reach
//
// It can create and delete Chaos Mesh custom resources, in one namespace, on a
// cluster whose config carries a human-written eligibility acknowledgement. It
// cannot patch a Deployment, cordon a Node, or delete a Pod: the scope door
// refuses the path in-process, and the chaos ServiceAccount's Role grants nothing
// outside chaos-mesh.org.
//
// State that plainly, because the inverse is easy to imply and false: **the chaos
// identity's RBAC does not bound the blast radius of an experiment.** MaKlaude
// asks Chaos Mesh to kill a pod; Chaos Mesh's own controller does the killing with
// its own privileges. What bounds the damage is the selector this package writes
// (validated here, and always naming its target namespaces explicitly), Chaos
// Mesh's own installation scope, and — from T4 onward — M5's blast-radius budget,
// cooldown and circuit breaker, which chaos proposals are subject to like any
// other action. RBAC on the CR is what stops MaKlaude reaching *past* Chaos Mesh;
// it is not what stops Chaos Mesh reaching far.
package chaos

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Sentinel errors this package produces. Every refusal and failure wraps one, so
// a caller can tell "MaKlaude declined to inject" from "MaKlaude injected and the
// API server said no" — a distinction the experiment record depends on.
var (
	// ErrInvalidExperiment is returned when a requested experiment is not
	// well-formed: an unknown kind or action, a mode missing its value, a selector
	// naming no namespace, a duration on an action that ignores it. Nothing is sent
	// when this happens.
	ErrInvalidExperiment = errors.New("chaos: invalid experiment")

	// ErrExperimentExists is returned when an identical experiment is already live
	// on the cluster. It is the create-shaped precondition's refusal (see
	// [Experiment.ObjectName]) and it is a healthy outcome, not a malfunction: a
	// replay collided instead of injecting a second copy of the same fault.
	ErrExperimentExists = errors.New("chaos: an identical experiment is already live")

	// ErrMissingUID is returned when a teardown is attempted without the UID of the
	// object that was created. Teardown's precondition is object identity, not
	// object version — see [Injector.Remove] — and there is no unconditional
	// variant.
	ErrMissingUID = errors.New("chaos: missing UID precondition for teardown")

	// ErrInject wraps any other failure of an attempted chaos write (RBAC denial,
	// connectivity, rejection by the API server or by a Chaos Mesh admission
	// webhook).
	ErrInject = errors.New("chaos: experiment failed")
)

// APIGroup and APIVersion identify the Chaos Mesh custom resources this package
// writes. The group also appears in [kube.ChaosAPIPathPrefix], which is what the
// scope door verifies against; this package composes paths from that constant
// rather than restating the group, so there is one string to keep right.
const (
	APIGroup   = "chaos-mesh.org"
	APIVersion = "v1alpha1"
)

// keyPrefix namespaces the labels and annotations MaKlaude stamps on an
// experiment. It is a DNS-style key prefix as Kubernetes convention requires and
// is never resolved as a host.
const keyPrefix = "chaos.maklaude.dev/"

// Kind is a Chaos Mesh custom resource kind.
//
// The catalog is closed and currently has one member, which is deliberate. Each
// Chaos Mesh kind has its own required spec stanza — NetworkChaos needs a
// `delay`/`loss` block, StressChaos needs `stressors` — and a spec shape guessed
// from memory rather than pinned against the CRD produces an object the API server
// accepts and the controller ignores. That failure is invisible: the experiment
// reports injected and nothing breaks, so a run measures MaKlaude's behaviour
// under a fault that never happened. Adding a kind is an entry in [kindResource]
// plus its actions and a test against the real CRD in the e2e job (T8), and that
// is a better trade than shipping three kinds where two are approximations.
type Kind string

// KindPodChaos makes pods fail or disappear. It is the fault that most directly
// exercises what MaKlaude was built to notice: a crashlooping or missing pod is
// the shape its health collector detects and its remediation catalog acts on.
const KindPodChaos Kind = "PodChaos"

// kindResource maps a kind to the plural resource name in its API path. Chaos Mesh
// pluralises "chaos" as "chaos" (podchaos, not podchaoses), which is why this is a
// table rather than a lowercase-and-add-s.
var kindResource = map[Kind]string{
	KindPodChaos: "podchaos",
}

// Resource returns the plural resource name used in the kind's API path, or the
// empty string for a kind this package does not support.
func (k Kind) Resource() string { return kindResource[k] }

// Action is what an experiment does to the objects its selector matches.
type Action string

const (
	// ActionPodKill deletes a matched pod. Its controller recreates it, so the
	// fault MaKlaude sees is an abrupt, transient loss — the shape a node failure
	// or an OOM kill produces.
	//
	// It is a one-shot action: the pod is killed once and the experiment is over.
	// The CR object itself remains until it is deleted, which is why teardown is a
	// separate guarantee (T3) and not something a duration achieves.
	ActionPodKill Action = "pod-kill"

	// ActionPodFailure makes a matched pod unavailable for a bounded window by
	// replacing its containers' image with a pause image. Unlike pod-kill the effect
	// PERSISTS, and Chaos Mesh reverts it when spec.duration elapses — so this is
	// the one action in the catalog for which a duration is both required and
	// meaningful.
	ActionPodFailure Action = "pod-failure"
)

// SelfLimit is how an action's fault ends WITHOUT MaKlaude doing anything.
//
// This is the type that carries Milestone 6's load-bearing property — nothing
// survives the process — and it exists as a declared field rather than as prose
// because the property has to hold for actions nobody has written yet. The
// dangerous shape is an action whose fault PERSISTS and whose expiry depends on
// MaKlaude coming back to end it: teardown is a request, and a request needs a
// process, and a process can be SIGKILLed between the create and the defer. An
// action in that shape must not be addable by filling in a kind and an action
// string, which is what [TestEveryActionDeclaresASelfLimit] makes true — the zero
// value belongs to no action, so a new catalog entry fails the build until its
// author says how its fault ends on its own.
//
// Note what this is NOT: it is not the CR object's lifetime. Every action leaves
// the custom resource behind after its fault is over — one-shot actions
// immediately, duration-bounded ones on expiry — and that residue is what the
// [Reaper] sweeps. A fault that self-limits and an object that self-deletes are
// different guarantees, and Chaos Mesh only provides the first.
type SelfLimit int

const (
	// selfLimitUnset is the zero value. No action may carry it, which is the point:
	// a catalog entry added without stating how its fault ends gets this by default
	// and fails the set guard rather than shipping an unbounded fault.
	selfLimitUnset SelfLimit = iota

	// SelfLimitServerDuration means Chaos Mesh's own controller reverts the fault
	// when spec.duration elapses. This is the mechanism that survives MaKlaude's
	// death, because the enforcing party is on the cluster: if the process is killed
	// the instant after the create, the fault still ends on schedule. Actions in this
	// class REQUIRE a positive duration no greater than [maxDuration].
	SelfLimitServerDuration

	// SelfLimitInstant means the fault is a single event with no persisting state, so
	// there is nothing to revert and nothing to expire. A pod is killed once and its
	// controller recreates it; by the time MaKlaude could die, the fault is already
	// over. Actions in this class REFUSE a duration, because Chaos Mesh ignores
	// spec.duration for them and a CR carrying one would tell a human triaging an
	// incident that the fault self-reverts in 30 seconds when the controller will do
	// no such thing.
	SelfLimitInstant
)

// String renders the mechanism for an error message or a proposal.
func (s SelfLimit) String() string {
	switch s {
	case SelfLimitServerDuration:
		return "server-side duration"
	case SelfLimitInstant:
		return "instantaneous"
	default:
		return "undeclared"
	}
}

// actionKinds maps each supported action to the kind that owns it and to how its
// fault ends on its own.
//
// Whether the action honours spec.duration is DERIVED from the self-limit rather
// than stored beside it. It was a second field once, and two fields that must
// agree are two fields that can disagree — the failure being a CR that says
// "duration: 30s" next to an action the controller applies once, or an action
// whose fault persists with no duration to end it. One field, one answer.
var actionKinds = map[Action]struct {
	kind      Kind
	selfLimit SelfLimit
}{
	ActionPodKill:    {kind: KindPodChaos, selfLimit: SelfLimitInstant},
	ActionPodFailure: {kind: KindPodChaos, selfLimit: SelfLimitServerDuration},
}

// Mode selects how many of the objects matching a selector an experiment affects.
type Mode string

const (
	// ModeOne affects exactly one matched object, chosen by Chaos Mesh.
	ModeOne Mode = "one"
	// ModeAll affects every matched object.
	ModeAll Mode = "all"
	// ModeFixed affects a fixed number of matched objects; Value is that number.
	ModeFixed Mode = "fixed"
	// ModeFixedPercent affects a percentage of matched objects; Value is that
	// percentage.
	ModeFixedPercent Mode = "fixed-percent"
	// ModeRandomMaxPercent affects a random number of matched objects up to a
	// percentage; Value is that percentage.
	ModeRandomMaxPercent Mode = "random-max-percent"
)

// modeNeedsValue records which modes carry a Value. It is a closed table so an
// unknown mode is an error rather than a string passed through to the API server,
// and so the two mistakes that change an experiment's size — a value on a mode
// that ignores it, a missing value on a mode that needs one — are both refused
// here. Whether a given mode is *acceptable* for a given cluster is a blast-radius
// question, which belongs to M5's budget and breaker (T4), not to this table.
var modeNeedsValue = map[Mode]bool{
	ModeOne:              false,
	ModeAll:              false,
	ModeFixed:            true,
	ModeFixedPercent:     true,
	ModeRandomMaxPercent: true,
}

// Selector names the objects an experiment may affect.
type Selector struct {
	// Namespaces is the set of namespaces the experiment may reach. At least one is
	// required and it is never inferred: Chaos Mesh defaults an empty selector to
	// the namespace of the CR itself, so an omitted list means "somewhere" to a
	// person reading the object and "here" to the controller. Since the CR lives in
	// MaKlaude's own chaos namespace and the target is somewhere else entirely,
	// those two readings differ by the whole point of the experiment.
	Namespaces []string

	// LabelSelectors narrows the match within those namespaces. It may be empty,
	// in which case every pod in the named namespaces matches and [Mode] alone
	// bounds how many are affected.
	LabelSelectors map[string]string
}

// Experiment is a requested fault: what to break, where, how many, and for how
// long.
//
// It is a plain value with no cluster in it. The cluster comes from the
// [Injector], which holds a [cluster.ChaosTarget], so an experiment cannot name
// its own target and no request can be redirected at a cluster nobody marked
// eligible.
type Experiment struct {
	// Action is what to do; it determines the kind (see [Experiment.Kind]).
	Action Action

	// Namespace is where the CR OBJECT lives — MaKlaude's own chaos namespace, the
	// one its Role is scoped to. It is not where the fault lands; that is
	// Selector.Namespaces, and it must not be one of them — see
	// [Experiment.placementProblems], which is half of a bound RBAC cannot express
	// alone.
	Namespace string

	// Selector names the objects the fault may affect.
	Selector Selector

	// Mode selects how many matched objects are affected.
	Mode Mode

	// ModeValue is the count or percentage the mode needs, as Chaos Mesh's own
	// string-typed spec.value. It must be set for exactly the modes in
	// [modeNeedsValue] that need it.
	ModeValue string

	// Duration bounds how long the fault persists, for the actions whose effect
	// Chaos Mesh reverts on expiry. It must be set for exactly those actions and
	// left zero for the one-shot ones — see [actionKinds].
	Duration time.Duration
}

// maxDuration is the longest fault this package will ask for.
//
// The ceiling exists because a duration is the only bound that survives MaKlaude
// dying: if the process is killed between injecting and tearing down, an expiring
// fault reverts itself and a long one does not. It is not the teardown guarantee —
// one-shot actions have no duration at all and the CR outlives the fault in every
// case, which is what the [Reaper] is for — but for the actions that honour it, it
// is the difference between a self-limiting experiment and an outage that waits
// for a human.
//
// It has a second job that is easy to miss: it is the FLOOR of the reaper's orphan
// grace. No fault this package asks for can still be running more than maxDuration
// after its object was created, so an owned object older than that cannot belong to
// a live experiment under any MaKlaude process — which is what lets the reaper
// sweep without an exclusion list of names somebody has to remember to pass. See
// [NewReaper]. Raising this constant therefore widens the reaper's blind window by
// the same amount, deliberately and visibly.
const maxDuration = 10 * time.Minute

// Kind returns the Chaos Mesh kind that owns this experiment's action, or the
// empty string if the action is not in the catalog.
func (e Experiment) Kind() Kind { return actionKinds[e.Action].kind }

// SelfLimit reports how this experiment's fault ends without MaKlaude, or
// [selfLimitUnset] rendered as "undeclared" for an action not in the catalog.
//
// It is exported because a proposal renderer and an audit trail both need to say
// it: "MaKlaude will make these pods unavailable, and Chaos Mesh will put them back
// in 2m whether or not MaKlaude is alive" is the sentence that makes a chaos
// proposal reviewable, and a human should not have to know the action catalog to
// read it.
func (e Experiment) SelfLimit() SelfLimit { return actionKinds[e.Action].selfLimit }

// Validate reports whether the experiment is well-formed, returning an error
// wrapping [ErrInvalidExperiment] describing every problem it found.
//
// It reports all problems at once rather than the first. An operator fixing a
// hand-written experiment should not need one round trip per field, and a caller
// composing one programmatically wants the whole disagreement in the record.
func (e Experiment) Validate() error {
	var problems []string

	spec, known := actionKinds[e.Action]
	switch {
	case strings.TrimSpace(string(e.Action)) == "":
		problems = append(problems, "action is required")
	case !known:
		problems = append(problems, fmt.Sprintf("unknown action %q (supported: %s)", e.Action, supportedActions()))
	case spec.kind.Resource() == "":
		// Unreachable while actionKinds and kindResource agree; checked so that a
		// half-finished kind addition fails loudly here rather than composing a
		// request path with an empty resource segment.
		problems = append(problems, fmt.Sprintf("action %q maps to kind %q, which has no resource name", e.Action, spec.kind))
	case spec.selfLimit == selfLimitUnset:
		// Also unreachable while TestEveryActionDeclaresASelfLimit passes, and checked
		// for the same reason as the case above: the set guard is a build-time
		// assertion about the catalog, and this is the run-time refusal for the one
		// path that could reach production if the guard were ever deleted. An action
		// whose fault has no declared end must not be injectable, because the only
		// thing that would then end it is MaKlaude coming back — see [SelfLimit].
		problems = append(problems, fmt.Sprintf(
			"action %q declares no self-limit, so nothing but MaKlaude would end its fault", e.Action))
	}

	if err := validateName("namespace", e.Namespace); err != nil {
		problems = append(problems, err.Error())
	}

	problems = append(problems, e.Selector.problems()...)
	problems = append(problems, e.placementProblems()...)
	problems = append(problems, e.modeProblems()...)

	if known {
		problems = append(problems, e.durationProblems(spec.selfLimit)...)
	}

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidExperiment, strings.Join(problems, "; "))
	}
	return nil
}

// placementProblems reports an experiment whose CR object would be created in a
// namespace the experiment itself breaks.
//
// This is not tidiness, and it is one half of a bound RBAC cannot express on its
// own. Chaos Mesh's permission validation (the `vauth.kb.io` webhook, ON by default)
// authorizes a create by asking the API server whether the REQUESTER may create this
// chaos kind in every namespace the SELECTOR names — verb `create`, group
// `chaos-mesh.org`, resource `podchaos`, one SubjectAccessReview per target
// namespace. So aiming a fault at a namespace requires `create podchaos` there,
// which is what deploy/rbac/chaos/target-namespace-role.yaml grants, once per
// namespace an operator is willing to have broken.
//
// That grant unavoidably also permits creating an experiment OBJECT in the target
// namespace: the webhook checks the same verb a write uses, so RBAC cannot tell "may
// aim here" apart from "may write here". An object outside MaKlaude's own chaos
// namespace is unreachable by every teardown path that exists — [Reaper.Reap] sweeps
// one namespace, and the chaos Role grants `list` and `delete` in that one alone, so
// a stray object could be neither found nor removed. It would be exactly the
// outlives-the-process leak this milestone is about.
//
// So the namespaces a create can land in are bounded twice, by two mechanisms that
// have to agree: RBAC narrows the set to {the chaos namespace} ∪ {the target
// namespaces}, and this rule removes the target namespaces from it. What is left is
// the swept namespace, alone.
func (e Experiment) placementProblems() []string {
	for _, ns := range e.Selector.Namespaces {
		if ns != e.Namespace {
			continue
		}
		return []string{fmt.Sprintf(
			"namespace %q is also a target in selector.namespaces: an experiment object must not live in a namespace the experiment breaks, "+
				"because the reaper sweeps only MaKlaude's own chaos namespace and an object anywhere else can never be collected", e.Namespace)}
	}
	return nil
}

// modeProblems reports what is wrong with the experiment's mode and value.
func (e Experiment) modeProblems() []string {
	needsValue, known := modeNeedsValue[e.Mode]
	if !known {
		if strings.TrimSpace(string(e.Mode)) == "" {
			return []string{"mode is required"}
		}
		return []string{fmt.Sprintf("unknown mode %q (supported: %s)", e.Mode, supportedModes())}
	}

	value := strings.TrimSpace(e.ModeValue)
	switch {
	case needsValue && value == "":
		return []string{fmt.Sprintf("mode %q requires a value", e.Mode)}
	case !needsValue && value != "":
		return []string{fmt.Sprintf("mode %q takes no value, got %q", e.Mode, e.ModeValue)}
	}
	return nil
}

// durationProblems reports what is wrong with the experiment's duration, given how
// its action's fault ends on its own.
//
// Both directions are refusals rather than corrections, and both happen HERE —
// before any request is composed, let alone sent — so an experiment whose fault
// would outlive its bound never reaches a cluster. There is no clamping: silently
// shortening a 30-minute request to 10 would inject a different experiment than the
// one the caller asked for and than the one the record would describe.
func (e Experiment) durationProblems(selfLimit SelfLimit) []string {
	switch {
	case selfLimit == SelfLimitServerDuration && e.Duration <= 0:
		return []string{fmt.Sprintf(
			"action %q persists until Chaos Mesh reverts it, so it requires a positive duration (at most %s) — "+
				"without one the only thing that would end the fault is MaKlaude, and MaKlaude can be killed",
			e.Action, maxDuration)}
	case selfLimit == SelfLimitServerDuration && e.Duration > maxDuration:
		return []string{fmt.Sprintf("duration %s exceeds the maximum %s", e.Duration, maxDuration)}
	case selfLimit == SelfLimitInstant && e.Duration != 0:
		return []string{fmt.Sprintf(
			"action %q is one-shot and Chaos Mesh ignores spec.duration for it, so a duration (%s) must not be set",
			e.Action, e.Duration)}
	}
	return nil
}

// problems reports every reason the selector is not usable.
func (s Selector) problems() []string {
	var problems []string

	if len(s.Namespaces) == 0 {
		problems = append(problems, "selector.namespaces must name at least one namespace (an empty list means the CR's own namespace to Chaos Mesh, which is never the intent here)")
	}
	seen := map[string]bool{}
	for _, ns := range s.Namespaces {
		if err := validateName("selector.namespaces entry", ns); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if seen[ns] {
			problems = append(problems, fmt.Sprintf("selector.namespaces repeats %q", ns))
		}
		seen[ns] = true
	}

	for k, v := range s.LabelSelectors {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("selector.labelSelectors key %q is not a valid label key: %s", k, strings.Join(errs, "; ")))
		}
		if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
			problems = append(problems, fmt.Sprintf("selector.labelSelectors value %q (key %q) is not a valid label value: %s", v, k, strings.Join(errs, "; ")))
		}
	}

	sort.Strings(problems)
	return problems
}

// validateName rejects an empty or malformed Kubernetes name.
//
// This is a safety check as much as an input check, for the same reason it is in
// internal/kube: request paths are composed from these values, and the
// [kube.WriteScope] that guards the request is composed from them too, so a value
// containing "/" or ".." could produce a path that is not the object it claims to
// be while still matching its own scope. Constraining every path segment to a
// DNS-1123 subdomain removes that at the boundary.
func validateName(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if problems := validation.IsDNS1123Subdomain(value); len(problems) > 0 {
		return fmt.Errorf("%s %q is not a valid Kubernetes name: %s", field, value, strings.Join(problems, "; "))
	}
	return nil
}

// supportedActions renders the catalog for an error message, in stable order.
func supportedActions() string {
	out := make([]string, 0, len(actionKinds))
	for a := range actionKinds {
		out = append(out, string(a))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// supportedModes renders the modes for an error message, in stable order.
func supportedModes() string {
	out := make([]string, 0, len(modeNeedsValue))
	for m := range modeNeedsValue {
		out = append(out, string(m))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// nameScheme prefixes every derived object name. It is a format version for the
// derivation itself: change it when the fields that go into a name change, so that
// names produced by two different builds are not silently assumed comparable.
const nameScheme = "n1"

// nameDigestChars is how much of the digest goes into an object name. Twelve hex
// characters is 48 bits, which for the handful of experiments a cluster hosts at
// once makes an accidental collision irrelevant while keeping the name short
// enough to read in `kubectl get podchaos` output.
const nameDigestChars = 12

// ObjectName returns the name the experiment's CR will carry.
//
// This is the create-shaped precondition, and it is the answer to a real gap
// rather than a naming convention. Every mutation in internal/kube carries the
// target's resourceVersion, and there is deliberately no unconditional variant —
// but a create is a POST of an object that does not exist yet, so optimistic
// concurrency has nothing to be optimistic about. The guard has to come from
// somewhere else, and the only thing available before the object exists is its
// name.
//
// So the name is DERIVED from everything that defines the experiment, never
// supplied by a caller and never generated by the server:
//
//   - Derived means a replay is the same request. Two calls asking for the same
//     fault, in the same place, at the same size, for the same duration produce
//     the same name, so the second one COLLIDES (409 AlreadyExists) instead of
//     injecting a second copy of a fault MaKlaude already has running. That makes
//     the create idempotent in the only sense that matters here: retrying is safe,
//     and duplicating is impossible.
//   - Not server-generated because Chaos Mesh's generateName would defeat exactly
//     that. Every retry would succeed with a fresh name, and a network timeout on
//     a request the API server actually accepted — the case a retry exists for —
//     would leave two live experiments and a caller holding one name.
//   - Not caller-supplied because then the collision property would depend on
//     every call site choosing names the same way, which is a convention. This is
//     a function.
//
// The name is not a secret and is safe to log: it is a digest of an experiment's
// own shape, and every input to it is either a MaKlaude-side constant or a
// Kubernetes object name the operator configured.
func (e Experiment) ObjectName() string {
	h := sha256.New()
	// Length-prefix every field so no two different experiments can produce the
	// same byte stream by moving a delimiter into a value. Label keys/values are
	// already constrained by validation, but the digest should not depend on that.
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = fmt.Fprintf(h, "%d:%s\n", len(p), p)
		}
	}

	write(nameScheme, string(e.Kind()), string(e.Action), e.Namespace, string(e.Mode), e.ModeValue)
	write(e.Duration.String())

	namespaces := append([]string(nil), e.Selector.Namespaces...)
	sort.Strings(namespaces)
	write(namespaces...)

	keys := make([]string, 0, len(e.Selector.LabelSelectors))
	for k := range e.Selector.LabelSelectors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write(k, e.Selector.LabelSelectors[k])
	}

	digest := hex.EncodeToString(h.Sum(nil))[:nameDigestChars]
	return fmt.Sprintf("maklaude-%s-%s", strings.ToLower(string(e.Kind())), digest)
}

// object renders the experiment as the CR body that will be POSTed.
//
// The body is composed here from validated fields rather than accepted from a
// caller, which is why this package exposes no InjectRaw. A create's target name
// travels in the BODY — the [kube.WriteScope] can only pin the collection path,
// since that is where a POST goes — so a caller-supplied object would be a way to
// name an object the scope never approved. Building the body is therefore part of
// the guard, not a convenience.
func (e Experiment) object(clusterName, acknowledgement string) *unstructured.Unstructured {
	spec := map[string]any{
		"action": string(e.Action),
		"mode":   string(e.Mode),
		"selector": map[string]any{
			"namespaces": toAnySlice(e.Selector.Namespaces),
		},
	}
	if value := strings.TrimSpace(e.ModeValue); value != "" {
		spec["value"] = value
	}
	if len(e.Selector.LabelSelectors) > 0 {
		labels := map[string]any{}
		for k, v := range e.Selector.LabelSelectors {
			labels[k] = v
		}
		spec["selector"].(map[string]any)["labelSelectors"] = labels
	}
	if e.Duration > 0 {
		// Chaos Mesh parses spec.duration as a Go duration string, which is what
		// time.Duration renders, so no unit conversion happens anywhere in this path.
		spec["duration"] = e.Duration.String()
	}
	if e.Action == ActionPodKill {
		// Explicit rather than relying on the CRD's default. A pod-kill with no grace
		// period is the fault this action is for — an abrupt loss, the shape a node
		// failure produces — and stating it in the object means a person reading the CR
		// does not have to know what Chaos Mesh defaults to.
		spec["gracePeriod"] = int64(0)
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/" + APIVersion,
		"kind":       string(e.Kind()),
		"metadata": map[string]any{
			"name":      e.ObjectName(),
			"namespace": e.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/name":       "maklaude",
				"app.kubernetes.io/component":  "chaos",
				"app.kubernetes.io/managed-by": "maklaude",
			},
			"annotations": map[string]any{
				// The cluster and the human's own acknowledgement sentence travel ON the
				// object. An experiment that outlives the run that created it — the leak
				// T3 exists to make impossible — is then self-describing: whoever finds
				// it can tell which cluster it was authorised for and read the consent
				// verbatim, without correlating it against a log that may have rotated.
				keyPrefix + "cluster":         clusterName,
				keyPrefix + "acknowledgement": acknowledgement,
			},
		},
		"spec": spec,
	}}
}

// toAnySlice converts a string slice for embedding in an unstructured object,
// whose contract is that every value is a JSON-compatible type.
func toAnySlice(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
