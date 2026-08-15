package cluster

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// eligibleFor returns a well-formed eligibility block for the named cluster.
func eligibleFor(name string) *ChaosEligibility {
	return &ChaosEligibility{Cluster: name, Acknowledgement: ChaosAcknowledgementFor(name)}
}

// registryWith builds a registry from specs, giving each one a placeholder
// kubeconfig so validation passes for reasons unrelated to chaos.
func registryWith(t *testing.T, specs ...Spec) (*Registry, error) {
	t.Helper()
	dir := t.TempDir()
	for i := range specs {
		if specs[i].Kubeconfig == "" {
			specs[i].Kubeconfig = writeKubeconfig(t, dir, specs[i].Name+".yaml")
		}
		if specs[i].Context == "" {
			specs[i].Context = specs[i].Name + "-ctx"
		}
	}
	return NewRegistry(&Config{Clusters: specs})
}

func TestChaosTarget_EligibleClusterMintsAToken(t *testing.T) {
	reg, err := registryWith(t, Spec{Name: "kind-lab", Chaos: eligibleFor("kind-lab")})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil", err)
	}

	target, err := reg.ChaosTarget("kind-lab")
	if err != nil {
		t.Fatalf("ChaosTarget(kind-lab) error = %v, want nil", err)
	}
	if got := target.Handle().Name(); got != "kind-lab" {
		t.Errorf("token handle name = %q, want %q", got, "kind-lab")
	}
	// The token quotes the human's own sentence, so an audit record can cite
	// consent rather than assert it.
	if got, want := target.Acknowledgement(), ChaosAcknowledgementFor("kind-lab"); got != want {
		t.Errorf("Acknowledgement() = %q, want %q", got, want)
	}
}

func TestChaosTarget_IneligibleClusterCannotMintAToken(t *testing.T) {
	// The default: a config written before Milestone 6, with no chaos block.
	reg, err := registryWith(t, Spec{Name: "prod"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil", err)
	}

	target, err := reg.ChaosTarget("prod")
	if err == nil {
		t.Fatal("ChaosTarget(prod) succeeded for a cluster with no eligibility block")
	}
	if !errors.Is(err, ErrChaosIneligible) {
		t.Errorf("error = %v, want it to wrap ErrChaosIneligible", err)
	}
	if target != nil {
		t.Error("a token was returned alongside the error; the caller must get nothing to use")
	}
	// The refusal has to be actionable: it names the cluster and the exact
	// sentence that would make it eligible.
	for _, want := range []string{`"prod"`, ChaosAcknowledgementFor("prod")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

func TestChaosTarget_UnknownClusterWrapsTheSameSentinel(t *testing.T) {
	// An unknown name and an ineligible cluster mean the same thing to a caller,
	// so one errors.Is check covers both. If these ever diverge, a caller that
	// only checks ErrChaosIneligible starts treating a typo as a hard error and
	// a real refusal as something else.
	reg, err := registryWith(t, Spec{Name: "kind-lab", Chaos: eligibleFor("kind-lab")})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil", err)
	}
	_, err = reg.ChaosTarget("kind-labb")
	if err == nil {
		t.Fatal("ChaosTarget() succeeded for a cluster that is not registered")
	}
	if !errors.Is(err, ErrChaosIneligible) {
		t.Errorf("error = %v, want it to wrap ErrChaosIneligible", err)
	}
}

// TestChaosEligibility_CopyPasteDoesNotCarryOver is the case the two-part
// acknowledgement exists for: an operator marks a scratch cluster eligible, then
// copies that whole config block when adding a production cluster. A bare
// `chaos: true` would survive the copy silently.
func TestChaosEligibility_CopyPasteDoesNotCarryOver(t *testing.T) {
	pasted := eligibleFor("kind-lab") // written for kind-lab, pasted under prod

	_, err := registryWith(t,
		Spec{Name: "kind-lab", Chaos: eligibleFor("kind-lab")},
		Spec{Name: "prod", Chaos: pasted},
	)
	if err == nil {
		t.Fatal("NewRegistry() = nil error, want the pasted eligibility block to be rejected")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v, want it to wrap ErrInvalidConfig", err)
	}
	// The message must say what actually happened, not just "invalid".
	for _, want := range []string{`"kind-lab"`, `"prod"`, "copied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q:\n%s", want, err.Error())
		}
	}

	// Belt and braces: even with validation bypassed entirely, resolution grants
	// prod nothing. Eligibility is granted because the marker matches, never
	// because validation happened to run first.
	h := &Handle{name: "prod", chaosAcknowledgement: resolveChaosEligibility("prod", pasted)}
	if _, err := h.ChaosTarget(); !errors.Is(err, ErrChaosIneligible) {
		t.Errorf("resolution granted prod a token from kind-lab's block: err = %v", err)
	}
}

// TestChaosEligibility_MalformedAndPartialFailClosed walks every way a marker can
// be wrong. Each case must both reject the config and leave the cluster
// ineligible — a fail-closed marker that still mints a token is the worst of
// both.
func TestChaosEligibility_MalformedAndPartialFailClosed(t *testing.T) {
	const name = "kind-lab"
	tests := []struct {
		desc string
		e    *ChaosEligibility
	}{
		{"both fields empty", &ChaosEligibility{}},
		{"cluster only", &ChaosEligibility{Cluster: name}},
		{"acknowledgement only", &ChaosEligibility{Acknowledgement: ChaosAcknowledgementFor(name)}},
		{"cluster names another cluster", &ChaosEligibility{Cluster: "other", Acknowledgement: ChaosAcknowledgementFor(name)}},
		{"acknowledgement names another cluster", &ChaosEligibility{Cluster: name, Acknowledgement: ChaosAcknowledgementFor("other")}},
		{"acknowledgement truncated", &ChaosEligibility{Cluster: name, Acknowledgement: "I accept that MaKlaude may deliberately break"}},
		{"acknowledgement reworded", &ChaosEligibility{Cluster: name, Acknowledgement: "I accept that MaKlaude can break the cluster named " + name}},
		{"acknowledgement wrong case", &ChaosEligibility{Cluster: name, Acknowledgement: strings.ToLower(ChaosAcknowledgementFor(name))}},
		{"acknowledgement is yes", &ChaosEligibility{Cluster: name, Acknowledgement: "yes"}},
		{"acknowledgement is true", &ChaosEligibility{Cluster: name, Acknowledgement: "true"}},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			if _, err := registryWith(t, Spec{Name: name, Chaos: tt.e}); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("NewRegistry() error = %v, want ErrInvalidConfig", err)
			}
			h := &Handle{name: name, chaosAcknowledgement: resolveChaosEligibility(name, tt.e)}
			if _, err := h.ChaosTarget(); !errors.Is(err, ErrChaosIneligible) {
				t.Errorf("a malformed marker still minted a token: err = %v", err)
			}
		})
	}
}

// TestChaosEligibility_AbsentIsSilentlyIneligible pins the one shape that is not
// an error. Absence is the default posture of every config that predates
// Milestone 6, and warning about it would train operators to ignore the warning.
func TestChaosEligibility_AbsentIsSilentlyIneligible(t *testing.T) {
	reg, err := registryWith(t, Spec{Name: "prod"}, Spec{Name: "staging"})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil for a config with no chaos blocks", err)
	}
	if got := reg.ChaosTargets(); len(got) != 0 {
		t.Errorf("ChaosTargets() = %d targets, want 0", len(got))
	}
}

// TestChaosEligibility_NilProblemsIsAProblem guards the verifier's own
// fail-closed property. A nil block means "absent", and if problems() answered
// "nothing wrong" for it, resolveChaosEligibility would grant eligibility to
// every cluster in the config.
func TestChaosEligibility_NilProblemsIsAProblem(t *testing.T) {
	var nilBlock *ChaosEligibility
	if got := nilBlock.problems("kind-lab"); len(got) == 0 {
		t.Error("problems() on a nil block reported no problems; an absent marker must never verify")
	}
	if got := resolveChaosEligibility("kind-lab", nil); got != "" {
		t.Errorf("resolveChaosEligibility() with no block = %q, want empty", got)
	}
}

// TestChaosEligibility_UnnamedClusterCannotBeEligible closes the gap where a
// cluster with no name meets an eligibility block: the missing name is already a
// validation error, but resolution must not treat an empty name as a match.
func TestChaosEligibility_UnnamedClusterCannotBeEligible(t *testing.T) {
	if got := resolveChaosEligibility("", &ChaosEligibility{Cluster: "", Acknowledgement: ChaosAcknowledgementFor("")}); got != "" {
		t.Errorf("resolveChaosEligibility() for an unnamed cluster = %q, want empty", got)
	}
}

// TestChaosEligibility_FoldedScalarWhitespaceIsAccepted keeps the sentence
// writable in YAML. `>-` folds a long line into one with newlines collapsed, and
// an operator who has typed the sentence correctly must not be defeated by how
// the file wraps.
func TestChaosEligibility_FoldedScalarWhitespaceIsAccepted(t *testing.T) {
	const name = "kind-lab"
	body := "clusters:\n" +
		"  - name: " + name + "\n" +
		"    kubeconfig: KUBECONFIG\n" +
		"    context: " + name + "\n" +
		"    chaos:\n" +
		"      cluster: " + name + "\n" +
		"      acknowledgement: >-\n" +
		"        I accept that MaKlaude may deliberately break the cluster\n" +
		"        named " + name + "\n"

	dir := t.TempDir()
	kc := writeKubeconfig(t, dir, "kc.yaml")
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "KUBECONFIG", kc)), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	reg, err := NewRegistryFromFile(path)
	if err != nil {
		t.Fatalf("NewRegistryFromFile() error = %v, want nil", err)
	}
	if _, err := reg.ChaosTarget(name); err != nil {
		t.Errorf("ChaosTarget(%s) error = %v, want nil for a folded acknowledgement", name, err)
	}
}

// TestChaosTarget_IsSealedAgainstForgery is the guard on the guarantee, not on a
// line of code. Nothing outside this package may satisfy ChaosTarget, or a
// caller could hand the chaos write path a token it minted itself and the type
// system would stop enforcing anything. Deleting the unexported method would
// compile and every other test here would still pass.
func TestChaosTarget_IsSealedAgainstForgery(t *testing.T) {
	iface := reflect.TypeOf((*ChaosTarget)(nil)).Elem()
	if iface.Kind() != reflect.Interface {
		t.Fatalf("ChaosTarget is a %s, want an interface", iface.Kind())
	}

	unexported := 0
	for i := range iface.NumMethod() {
		if !iface.Method(i).IsExported() {
			unexported++
		}
	}
	if unexported == 0 {
		t.Error("ChaosTarget has no unexported method, so any package can forge a chaos capability")
	}
}

// TestChaosTargets_ListsOnlyEligibleClustersInOrder covers the reaper's and the
// startup summary's view: a mixed config must expose exactly the eligible
// clusters, and mixing must not leak eligibility between neighbours.
func TestChaosTargets_ListsOnlyEligibleClustersInOrder(t *testing.T) {
	reg, err := registryWith(t,
		Spec{Name: "prod"},
		Spec{Name: "kind-lab", Chaos: eligibleFor("kind-lab")},
		Spec{Name: "staging"},
		Spec{Name: "kind-lab-2", Chaos: eligibleFor("kind-lab-2")},
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v, want nil", err)
	}

	var got []string
	for _, target := range reg.ChaosTargets() {
		got = append(got, target.Handle().Name())
	}
	want := []string{"kind-lab", "kind-lab-2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChaosTargets() = %v, want %v", got, want)
	}

	for _, name := range []string{"prod", "staging"} {
		if _, err := reg.ChaosTarget(name); !errors.Is(err, ErrChaosIneligible) {
			t.Errorf("%s became eligible next to an eligible cluster: err = %v", name, err)
		}
	}
}

// TestHandle_StringMarksChaosEligibility keeps an eligible cluster visible in
// logs rather than only in a config file.
func TestHandle_StringMarksChaosEligibility(t *testing.T) {
	eligible := &Handle{name: "kind-lab", chaosAcknowledgement: ChaosAcknowledgementFor("kind-lab")}
	if !strings.Contains(eligible.String(), "chaos-eligible") {
		t.Errorf("String() = %q, want it to mark chaos eligibility", eligible.String())
	}
	ordinary := &Handle{name: "prod"}
	if strings.Contains(ordinary.String(), "chaos") {
		t.Errorf("String() = %q, want no chaos marker on an ineligible cluster", ordinary.String())
	}
}

// TestExampleConfigMarksNoClusterChaosEligible pins the posture of the file
// operators copy. The committed example documents the key in comments; a live
// block in it would ship a chaos-eligible cluster to everyone who copies the
// file, which is the "the checked-in example turned it on" failure this project
// already designed against for autonomy.
func TestExampleConfigMarksNoClusterChaosEligible(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%s) error = %v", path, err)
	}
	for i, c := range cfg.Clusters {
		if c.Chaos != nil {
			t.Errorf("example cluster #%d (%q) carries a live chaos block; it must be commented out", i+1, c.Name)
		}
	}
}
