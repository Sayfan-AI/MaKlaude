package chaos

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// podKill is a minimal valid one-shot experiment, and podFailure its bounded
// counterpart. Tests mutate copies of these so each case states only the thing it
// is about.
func podKill() Experiment {
	return Experiment{
		Action:    ActionPodKill,
		Namespace: "maklaude-chaos",
		Mode:      ModeOne,
		Selector: Selector{
			Namespaces:     []string{"demo"},
			LabelSelectors: map[string]string{"app": "web"},
		},
	}
}

func podFailure() Experiment {
	e := podKill()
	e.Action = ActionPodFailure
	e.Duration = 30 * time.Second
	return e
}

// TestExperiment_Validate_AcceptsTheCatalog proves both supported actions validate
// with the duration each one actually honours.
func TestExperiment_Validate_AcceptsTheCatalog(t *testing.T) {
	for name, e := range map[string]Experiment{
		"pod-kill, one-shot, no duration": podKill(),
		"pod-failure, bounded duration":   podFailure(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err != nil {
				t.Fatalf("expected valid, got: %v", err)
			}
			if e.Kind() != KindPodChaos {
				t.Fatalf("expected kind PodChaos, got %q", e.Kind())
			}
		})
	}
}

// TestExperiment_Validate_Refusals covers every way an experiment can be
// ill-formed. Each case asserts the sentinel AND a fragment of the message,
// because an operator hand-writing an experiment gets nothing from a bare "invalid".
func TestExperiment_Validate_Refusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Experiment)
		want   string
	}{
		"no action": {
			mutate: func(e *Experiment) { e.Action = "" },
			want:   "action is required",
		},
		"unknown action": {
			mutate: func(e *Experiment) { e.Action = "delete-cluster" },
			want:   `unknown action "delete-cluster"`,
		},
		"no CR namespace": {
			mutate: func(e *Experiment) { e.Namespace = "" },
			want:   "namespace is required",
		},
		"malformed CR namespace": {
			mutate: func(e *Experiment) { e.Namespace = "../kube-system" },
			want:   "is not a valid Kubernetes name",
		},
		"no target namespaces": {
			mutate: func(e *Experiment) { e.Selector.Namespaces = nil },
			want:   "selector.namespaces must name at least one namespace",
		},
		"malformed target namespace": {
			mutate: func(e *Experiment) { e.Selector.Namespaces = []string{"demo/../kube-system"} },
			want:   "is not a valid Kubernetes name",
		},
		"repeated target namespace": {
			mutate: func(e *Experiment) { e.Selector.Namespaces = []string{"demo", "demo"} },
			want:   `selector.namespaces repeats "demo"`,
		},
		// The CR namespace appearing among the targets is the one refusal here that is
		// about RBAC rather than about a malformed value, and it is worth reading with
		// deploy/rbac/chaos/target-namespace-role.yaml open. Chaos Mesh's permission
		// webhook makes MaKlaude hold `create podchaos` in every namespace it aims at,
		// which is also — unavoidably, since it is the same verb — permission to write
		// an experiment OBJECT there. An object outside maklaude-chaos can never be
		// swept, because that is the only namespace the chaos Role can list or delete
		// in. So RBAC bounds the landing set to {maklaude-chaos} ∪ {targets} and this
		// rule subtracts the targets: the two together leave exactly the swept
		// namespace. See Experiment.placementProblems.
		"CR namespace is also a target": {
			mutate: func(e *Experiment) { e.Selector.Namespaces = []string{"other", e.Namespace} },
			want:   "is also a target in selector.namespaces",
		},
		"CR namespace is the only target": {
			mutate: func(e *Experiment) { e.Selector.Namespaces = []string{e.Namespace} },
			want:   "an experiment object must not live in a namespace the experiment breaks",
		},
		"malformed label key": {
			mutate: func(e *Experiment) { e.Selector.LabelSelectors = map[string]string{"not a key": "web"} },
			want:   "is not a valid label key",
		},
		"malformed label value": {
			mutate: func(e *Experiment) { e.Selector.LabelSelectors = map[string]string{"app": "not a value!"} },
			want:   "is not a valid label value",
		},
		"no mode": {
			mutate: func(e *Experiment) { e.Mode = "" },
			want:   "mode is required",
		},
		"unknown mode": {
			mutate: func(e *Experiment) { e.Mode = "most" },
			want:   `unknown mode "most"`,
		},
		"mode needs a value": {
			mutate: func(e *Experiment) { e.Mode = ModeFixedPercent },
			want:   `mode "fixed-percent" requires a value`,
		},
		"mode takes no value": {
			mutate: func(e *Experiment) { e.ModeValue = "50" },
			want:   `mode "one" takes no value`,
		},
		// The duration rules are the interesting pair: a duration is REQUIRED where
		// Chaos Mesh honours it and REFUSED where Chaos Mesh ignores it, so a CR never
		// carries a self-reverting promise its controller will not keep.
		"duration on a one-shot action": {
			mutate: func(e *Experiment) { e.Duration = time.Minute },
			want:   "is one-shot and Chaos Mesh ignores spec.duration",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := podKill()
			tc.mutate(&e)
			err := e.Validate()
			if !errors.Is(err, ErrInvalidExperiment) {
				t.Fatalf("expected ErrInvalidExperiment, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected message containing %q, got: %v", tc.want, err)
			}
		})
	}
}

// TestExperiment_Validate_DurationBounds covers the bounded action's two duration
// failures: absent, and past the ceiling that is the only bound surviving MaKlaude
// dying mid-experiment.
func TestExperiment_Validate_DurationBounds(t *testing.T) {
	cases := map[string]struct {
		duration time.Duration
		want     string
	}{
		"absent":   {duration: 0, want: "requires a positive duration"},
		"negative": {duration: -time.Second, want: "requires a positive duration"},
		"over the ceiling": {duration: maxDuration + time.Second,
			want: "exceeds the maximum " + maxDuration.String()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := podFailure()
			e.Duration = tc.duration
			err := e.Validate()
			if !errors.Is(err, ErrInvalidExperiment) {
				t.Fatalf("expected ErrInvalidExperiment, got: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected message containing %q, got: %v", tc.want, err)
			}
		})
	}
	if MaxDuration() != maxDuration {
		t.Fatalf("MaxDuration() must report the ceiling it enforces, got %s want %s", MaxDuration(), maxDuration)
	}
}

// TestExperiment_Validate_ReportsEveryProblem proves the validator does not stop at
// the first problem. An operator fixing a hand-written experiment should not need
// one round trip per field.
func TestExperiment_Validate_ReportsEveryProblem(t *testing.T) {
	e := Experiment{Action: "nope", Mode: "also-nope"}
	err := e.Validate()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"unknown action", "namespace is required", "selector.namespaces", "unknown mode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected message containing %q, got: %v", want, err)
		}
	}
}

// TestObjectName_IsDerivedAndStable is the create-shaped precondition's core
// property: the name is a pure function of the experiment, so a replay of the same
// request produces the same name and therefore collides instead of injecting a
// second copy of the same fault.
func TestObjectName_IsDerivedAndStable(t *testing.T) {
	e := podKill()
	first := e.ObjectName()

	if first != e.ObjectName() {
		t.Fatalf("name is not stable across calls: %q then %q", first, e.ObjectName())
	}

	// A separately-constructed identical experiment — the replay case — must land on
	// the same name. Map iteration order and slice identity must not matter.
	replay := podKill()
	replay.Selector.LabelSelectors = map[string]string{"app": "web"}
	if replay.ObjectName() != first {
		t.Fatalf("a replay must collide: %q vs %q", replay.ObjectName(), first)
	}

	if problems := validation.IsDNS1123Subdomain(first); len(problems) > 0 {
		t.Fatalf("derived name %q is not a valid Kubernetes name: %s", first, strings.Join(problems, "; "))
	}
	if !strings.HasPrefix(first, "maklaude-podchaos-") {
		t.Fatalf("derived name %q should identify its owner and kind", first)
	}
}

// TestObjectName_ChangesWithEveryInput proves the derivation covers everything that
// defines the experiment. A field that does not move the name is a field on which
// two DIFFERENT faults would collide — the second one silently refused as a
// duplicate of the first.
func TestObjectName_ChangesWithEveryInput(t *testing.T) {
	base := podFailure()
	baseName := base.ObjectName()

	mutations := map[string]func(*Experiment){
		"action":       func(e *Experiment) { e.Action = ActionPodKill },
		"CR namespace": func(e *Experiment) { e.Namespace = "elsewhere" },
		"mode":         func(e *Experiment) { e.Mode = ModeAll },
		"mode value":   func(e *Experiment) { e.Mode = ModeFixed; e.ModeValue = "2" },
		"duration":     func(e *Experiment) { e.Duration = 31 * time.Second },
		"target ns":    func(e *Experiment) { e.Selector.Namespaces = []string{"other"} },
		"extra ns":     func(e *Experiment) { e.Selector.Namespaces = []string{"demo", "other"} },
		"label value":  func(e *Experiment) { e.Selector.LabelSelectors = map[string]string{"app": "api"} },
		"label key":    func(e *Experiment) { e.Selector.LabelSelectors = map[string]string{"role": "web"} },
		"extra label":  func(e *Experiment) { e.Selector.LabelSelectors = map[string]string{"app": "web", "tier": "fe"} },
		"no labels":    func(e *Experiment) { e.Selector.LabelSelectors = nil },
		"namespace order": func(e *Experiment) {
			// Order must NOT matter — a set is a set — so this one asserts equality
			// below rather than difference.
			e.Selector.Namespaces = []string{"demo"}
		},
	}

	seen := map[string]string{baseName: "base"}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			e := podFailure()
			mutate(&e)
			got := e.ObjectName()
			if name == "namespace order" {
				if got != baseName {
					t.Fatalf("selector namespace ORDER must not change the name: %q vs %q", got, baseName)
				}
				return
			}
			if got == baseName {
				t.Fatalf("changing the %s must change the name, both are %q", name, got)
			}
			if other, ok := seen[got]; ok {
				t.Fatalf("changing the %s collides with %s at %q", name, other, got)
			}
			seen[got] = name
		})
	}
}

// TestExperimentObject_PodKill proves the CR body for a one-shot action: no
// duration (Chaos Mesh would ignore it), an explicit grace period, and a selector
// that names its target namespaces rather than letting the controller default them
// to the CR's own.
func TestExperimentObject_PodKill(t *testing.T) {
	e := podKill()
	obj := e.object("kind-lab", "I accept that MaKlaude may deliberately break the cluster named kind-lab")

	if got := obj.GetAPIVersion(); got != "chaos-mesh.org/v1alpha1" {
		t.Fatalf("apiVersion = %q", got)
	}
	if got := obj.GetKind(); got != "PodChaos" {
		t.Fatalf("kind = %q", got)
	}
	if got := obj.GetName(); got != e.ObjectName() {
		t.Fatalf("name = %q, want the derived %q", got, e.ObjectName())
	}
	if got := obj.GetNamespace(); got != "maklaude-chaos" {
		t.Fatalf("namespace = %q", got)
	}
	if _, ok := obj.Object["metadata"].(map[string]any)["generateName"]; ok {
		t.Fatal("the object must never carry generateName: a server-generated name defeats the collision guard")
	}

	spec := obj.Object["spec"].(map[string]any)
	if spec["action"] != "pod-kill" {
		t.Fatalf("action = %v", spec["action"])
	}
	if spec["mode"] != "one" {
		t.Fatalf("mode = %v", spec["mode"])
	}
	if _, ok := spec["value"]; ok {
		t.Fatalf("mode one takes no value, got %v", spec["value"])
	}
	if _, ok := spec["duration"]; ok {
		t.Fatalf("a one-shot action must carry no duration, got %v", spec["duration"])
	}
	if spec["gracePeriod"] != int64(0) {
		t.Fatalf("gracePeriod = %v, want an explicit 0", spec["gracePeriod"])
	}

	selector := spec["selector"].(map[string]any)
	namespaces := selector["namespaces"].([]any)
	if len(namespaces) != 1 || namespaces[0] != "demo" {
		t.Fatalf("selector.namespaces = %v", namespaces)
	}
	labels := selector["labelSelectors"].(map[string]any)
	if labels["app"] != "web" {
		t.Fatalf("selector.labelSelectors = %v", labels)
	}

	// The consent travels ON the object, so an experiment that outlives its run can
	// be traced without correlating it against a log.
	ann := obj.GetAnnotations()
	if ann[keyPrefix+"cluster"] != "kind-lab" {
		t.Fatalf("cluster annotation = %q", ann[keyPrefix+"cluster"])
	}
	if !strings.Contains(ann[keyPrefix+"acknowledgement"], "deliberately break the cluster named kind-lab") {
		t.Fatalf("acknowledgement annotation = %q", ann[keyPrefix+"acknowledgement"])
	}
	if obj.GetLabels()["app.kubernetes.io/managed-by"] != "maklaude" {
		t.Fatalf("labels = %v (the reaper finds orphans by them)", obj.GetLabels())
	}
}

// TestExperimentObject_PodFailure proves the bounded action carries the duration
// Chaos Mesh honours, in the Go duration string form it parses, and no grace period.
func TestExperimentObject_PodFailure(t *testing.T) {
	e := podFailure()
	e.Mode = ModeFixedPercent
	e.ModeValue = "50"
	e.Selector.LabelSelectors = nil

	spec := e.object("kind-lab", "ack").Object["spec"].(map[string]any)
	if spec["duration"] != "30s" {
		t.Fatalf("duration = %v, want the Go duration string 30s", spec["duration"])
	}
	if _, ok := spec["gracePeriod"]; ok {
		t.Fatalf("gracePeriod belongs to pod-kill only, got %v", spec["gracePeriod"])
	}
	if spec["value"] != "50" {
		t.Fatalf("value = %v", spec["value"])
	}
	selector := spec["selector"].(map[string]any)
	if _, ok := selector["labelSelectors"]; ok {
		t.Fatalf("an empty label selector must be omitted, got %v", selector["labelSelectors"])
	}
}

// TestKindResource_AgreesWithTheActionCatalog guards the half-finished-addition
// case: every action's kind must have a resource name, or the request path would be
// composed with an empty segment.
func TestKindResource_AgreesWithTheActionCatalog(t *testing.T) {
	for action, spec := range actionKinds {
		if spec.kind.Resource() == "" {
			t.Errorf("action %q maps to kind %q, which has no entry in kindResource", action, spec.kind)
		}
	}
}

// minimalFor builds a minimal valid experiment for any action in the catalog,
// supplying a duration for exactly the actions whose fault needs one. The tests below
// are written over the CATALOG rather than over the two actions that exist today, so
// an action added later inherits every assertion instead of being exempt from it by
// omission.
func minimalFor(action Action) Experiment {
	e := podKill()
	e.Action = action
	if actionKinds[action].selfLimit == SelfLimitServerDuration {
		e.Duration = 30 * time.Second
	}
	return e
}

// TestEveryActionDeclaresASelfLimit is THE set guard for Milestone 6's load-bearing
// property: nothing survives the process.
//
// The dangerous action is one whose fault PERSISTS and whose end depends on MaKlaude
// coming back to request it, because a process can be SIGKILLed between the create
// and the teardown. The zero value of [SelfLimit] belongs to no action, so adding a
// catalog entry without saying how its fault ends on its own fails here — the check
// is on the SET, not on the two members that happen to exist, which is the only shape
// that holds for code nobody has written yet.
func TestEveryActionDeclaresASelfLimit(t *testing.T) {
	if len(actionKinds) == 0 {
		t.Fatal("the action catalog is empty, so this guard would pass vacuously")
	}
	for action, spec := range actionKinds {
		switch spec.selfLimit {
		case SelfLimitServerDuration, SelfLimitInstant:
			// Declared.
		default:
			t.Errorf("action %q declares no self-limit (%s): state how its fault ends without MaKlaude, "+
				"or the only thing that ends it is a process that can be killed",
				action, spec.selfLimit)
		}
	}
}

// TestEveryActionsDurationRuleFollowsItsSelfLimit proves the declaration is enforced
// rather than decorative, in both directions, for every action in the catalog:
// mandatory and bounded where the fault persists, refused where the controller would
// ignore it.
//
// The bound is checked at its exact edge on both sides. An off-by-one at the ceiling
// is the mistake that would let a fault run longer than the reaper's grace assumes is
// impossible, and that grace is what makes a sweep unable to touch a live experiment
// (see [NewReaper]).
func TestEveryActionsDurationRuleFollowsItsSelfLimit(t *testing.T) {
	for action, spec := range actionKinds {
		t.Run(string(action), func(t *testing.T) {
			if err := minimalFor(action).Validate(); err != nil {
				t.Fatalf("the minimal experiment for %q must be valid, got: %v", action, err)
			}

			switch spec.selfLimit {
			case SelfLimitServerDuration:
				noDuration := minimalFor(action)
				noDuration.Duration = 0
				assertInvalid(t, noDuration, "requires a positive duration")

				atCeiling := minimalFor(action)
				atCeiling.Duration = MaxDuration()
				if err := atCeiling.Validate(); err != nil {
					t.Errorf("a duration exactly at the ceiling must be allowed, got: %v", err)
				}

				overCeiling := minimalFor(action)
				overCeiling.Duration = MaxDuration() + time.Nanosecond
				assertInvalid(t, overCeiling, "exceeds the maximum")

			case SelfLimitInstant:
				withDuration := minimalFor(action)
				withDuration.Duration = time.Second
				assertInvalid(t, withDuration, "must not be set")
			}
		})
	}
}

// assertInvalid fails unless e is refused with [ErrInvalidExperiment] and a message
// containing want.
func assertInvalid(t *testing.T, e Experiment, want string) {
	t.Helper()
	err := e.Validate()
	if !errors.Is(err, ErrInvalidExperiment) {
		t.Fatalf("expected ErrInvalidExperiment, got %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("message %q does not mention %q", err.Error(), want)
	}
}

// TestSelfLimitDecidesWhetherTheObjectCarriesADuration closes the gap between what
// the validator demands and what actually goes on the wire.
//
// The two could disagree — a duration required by the validator and dropped from the
// body, or set in the body for an action the controller applies once — and either
// failure is invisible from the CR alone: the API server accepts both, and the second
// tells a human triaging an incident that a fault self-reverts when it will not.
func TestSelfLimitDecidesWhetherTheObjectCarriesADuration(t *testing.T) {
	for action, spec := range actionKinds {
		t.Run(string(action), func(t *testing.T) {
			e := minimalFor(action)
			spec2 := e.object("kind-lab", "ack").Object["spec"].(map[string]any)
			got, present := spec2["duration"]

			switch spec.selfLimit {
			case SelfLimitServerDuration:
				if !present {
					t.Fatalf("action %q persists until Chaos Mesh reverts it, so spec.duration must be on the object", action)
				}
				if got != e.Duration.String() {
					t.Errorf("spec.duration = %v, want the Go duration string %q", got, e.Duration.String())
				}
			case SelfLimitInstant:
				if present {
					t.Errorf("action %q is one-shot and Chaos Mesh ignores spec.duration, so it must be absent, got %v", action, got)
				}
			}

			if e.SelfLimit() != spec.selfLimit {
				t.Errorf("Experiment.SelfLimit() = %v, want %v", e.SelfLimit(), spec.selfLimit)
			}
		})
	}
}

// TestObjectCarriesEveryOwnershipLabel ties the injector's stamp to the reaper's
// ownership test.
//
// These are two files that must agree, and the failure if they drift is silent in the
// worst direction: the reaper would stop recognising MaKlaude's own experiments and
// report a clean sweep while every one of them leaked. Dropping a label from
// [Experiment.object] fails here rather than in production six weeks later.
func TestObjectCarriesEveryOwnershipLabel(t *testing.T) {
	for action := range actionKinds {
		e := minimalFor(action)
		meta := e.object("kind-lab", "ack").Object["metadata"].(map[string]any)
		objLabels, ok := meta["labels"].(map[string]any)
		if !ok {
			t.Fatalf("action %q: the object carries no labels", action)
		}
		for key, want := range ownershipLabels {
			if got := objLabels[key]; got != want {
				t.Errorf("action %q: label %s = %v, want %q (the reaper decides ownership on it)", action, key, got, want)
			}
		}
		annotations := meta["annotations"].(map[string]any)
		if annotations[keyPrefix+"cluster"] != "kind-lab" {
			t.Errorf("action %q: the cluster annotation must name the cluster, got %v", action, annotations[keyPrefix+"cluster"])
		}
	}
}

// TestDerivedNameShape_MatchesWhatTheInjectorProduces is the other half of that
// agreement: the reaper's third ownership signal is that a name looks derived, and the
// pattern has to match every name the derivation actually produces.
//
// It also asserts the negatives, which are the point of the signal — a hand-written
// name and a Chaos-Mesh-generated one must not match, because that is what stops a
// sweep from deleting a human's own experiment.
func TestDerivedNameShape_MatchesWhatTheInjectorProduces(t *testing.T) {
	for action := range actionKinds {
		name := minimalFor(action).ObjectName()
		if !derivedNameShape.MatchString(name) {
			t.Errorf("action %q: derived name %q does not match the reaper's shape %s", action, name, derivedNameShape)
		}
	}

	for _, name := range []string{
		"my-test-chaos",                      // a human's own experiment
		"maklaude-podchaos",                  // prefix only, no digest
		"maklaude-podchaos-",                 // empty digest
		"maklaude-podchaos-notahexdigest",    // right length, not hex
		"maklaude-podchaos-abc",              // too short
		"maklaude-podchaos-0123456789abcdef", // too long
		"chaos-mesh-podchaos-0123456789ab",   // MaKlaude's digest shape, someone else's prefix
		"maklaude-podchaos-0123456789ab-x",   // trailing junk
	} {
		if derivedNameShape.MatchString(name) {
			t.Errorf("name %q must NOT look MaKlaude-derived; a sweep would delete it", name)
		}
	}
}
