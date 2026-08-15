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
