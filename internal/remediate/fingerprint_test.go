package remediate

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
)

// fixture is a well-formed proposal with every fingerprinted field populated, so a
// case can vary exactly one of them and the assertion names one cause.
func fixture() Proposal {
	return Proposal{
		Identity:      newProposalIdentity(OpRolloutRestart, fixtureTarget()),
		Cluster:       "prod",
		Operation:     OpRolloutRestart,
		Cause:         diagnose.CauseBadImage,
		Confidence:    diagnose.ConfidenceHigh,
		Reversibility: ReversibilityReversible,
		Target:        fixtureTarget(),
		Title:         "Restart deployment rollout",
		Intent:        "Roll the pods so they re-pull the image.",
		ProposedAt:    time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC),
		Preconditions: []Precondition{
			{Kind: PreconditionUnchanged, Expect: "424242", Description: "unchanged"},
			{Kind: PreconditionPodCrashLooping, Expect: "payments/web-abc123", Description: "still crashlooping"},
		},
	}
}

func fixtureTarget() Target {
	return Target{Cluster: "prod", Kind: "deployment", Namespace: "payments", Name: "web", ResourceVersion: "424242"}
}

// The fingerprint is a cached approval's validity token, so the first thing to pin is
// that it is stable: the same proposal must fingerprint identically every time, in
// every process, or a trusted fix would lose and regain autonomy at random.
func TestFingerprint_IsDeterministic(t *testing.T) {
	want := fixture().Fingerprint()
	for i := 0; i < 100; i++ {
		if got := fixture().Fingerprint(); got != want {
			t.Fatalf("call %d: %s, want %s — the fingerprint is not deterministic", i, got, want)
		}
	}
	if !strings.HasPrefix(string(want), fingerprintScheme+":") {
		t.Errorf("fingerprint %q does not carry the scheme prefix, so a stored token could not be told apart "+
			"from one composed under different rules", want)
	}
}

// Every field a change to which must invalidate a cached approval. Each case mutates
// exactly one, so a failure says which field stopped counting.
func TestFingerprint_ChangesWhenTheFixChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Proposal)
	}{
		{"operation", func(p *Proposal) { p.Operation = OpDeletePod }},
		{"target cluster", func(p *Proposal) { p.Target.Cluster = "staging" }},
		{"target kind", func(p *Proposal) { p.Target.Kind = "statefulset" }},
		{"target namespace", func(p *Proposal) { p.Target.Namespace = "checkout" }},
		// The case issue #167 was filed about: a workload that did not exist when the
		// approvals were given must not inherit them.
		{"target name", func(p *Proposal) { p.Target.Name = "api" }},
		{"cause", func(p *Proposal) { p.Cause = diagnose.CauseOOMKill }},
		{"reversibility", func(p *Proposal) { p.Reversibility = ReversibilityRecreatedByController }},
		{
			name: "a precondition dropped",
			mutate: func(p *Proposal) {
				p.Preconditions = p.Preconditions[:1] // leaves only the unchanged check
			},
		},
		{
			name: "a precondition added",
			mutate: func(p *Proposal) {
				p.Preconditions = append(p.Preconditions, Precondition{Kind: PreconditionPodHasController})
			},
		},
		{
			name: "a binding precondition's expectation",
			mutate: func(p *Proposal) {
				p.Preconditions = append(p.Preconditions, Precondition{Kind: PreconditionRevisionExists, Expect: "7"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			tc.mutate(&p)
			if got, base := p.Fingerprint(), fixture().Fingerprint(); got == base {
				t.Errorf("changing the %s left the fingerprint at %s; a cached approval would carry over to a "+
					"fix a person was never shown", tc.name, got)
			}
		})
	}
}

// A binding precondition's VALUE is part of the fix. Rolling a deployment back to
// revision 5 and to revision 9 are two different outcomes, and a human who approved
// one has not approved the other.
func TestFingerprint_BindingPreconditionValuesSeparateFixes(t *testing.T) {
	five, nine := fixture(), fixture()
	five.Preconditions = append(five.Preconditions, Precondition{Kind: PreconditionRevisionExists, Expect: "5"})
	nine.Preconditions = append(nine.Preconditions, Precondition{Kind: PreconditionRevisionExists, Expect: "9"})

	if five.Fingerprint() == nine.Fingerprint() {
		t.Error("rolling back to revision 5 and to revision 9 share a fingerprint; approving one would authorize the other")
	}
}

// The other half of the issue's table, and the more dangerous one to get wrong: a
// fingerprint that moves when nothing about the fix moved re-gates every proposal
// forever, which presents as autonomy that is configured, valid, and silently never
// fires.
func TestFingerprint_SurvivesWhatIsNotTheFix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Proposal)
	}{
		// The big one. resourceVersion changes on every write to the object by anyone,
		// so if it bound the fingerprint no fix would ever be the same fix twice.
		{
			name: "the target's resourceVersion",
			mutate: func(p *Proposal) {
				p.Target.ResourceVersion = "999999"
				p.Preconditions[0].Expect = "999999"
			},
		},
		// The crashlooping pod is a new object after every restart. Binding it would make
		// each occurrence of one recurring fault a different fix.
		{
			name:   "which pod is crashlooping",
			mutate: func(p *Proposal) { p.Preconditions[1].Expect = "payments/web-zzz999" },
		},
		{"the proposal's identity string", func(p *Proposal) { p.Identity = "proposal|whatever" }},
		{"confidence", func(p *Proposal) { p.Confidence = diagnose.ConfidenceLow }},
		{"the hypothesis it came from", func(p *Proposal) { p.Hypothesis = "hypothesis|other" }},
		{"the incident it came from", func(p *Proposal) { p.Incident = "incident|other" }},
		{"when it was proposed", func(p *Proposal) { p.ProposedAt = p.ProposedAt.Add(72 * time.Hour) }},
		{"the title", func(p *Proposal) { p.Title = "Restart the rollout" }},
		{"the intent", func(p *Proposal) { p.Intent = "Reworded entirely." }},
		{"the expected effect", func(p *Proposal) { p.ExpectedEffect = "Something else." }},
		{
			name:   "a precondition's human-readable description",
			mutate: func(p *Proposal) { p.Preconditions[1].Description = "Reworded." },
		},
		{
			name:   "the evidence behind it",
			mutate: func(p *Proposal) { p.Evidence = []detect.Finding{{Identity: "finding|new"}} },
		},
		// The planner emits preconditions in a fixed order today. Sorting means a
		// reordering later is not silently a mass invalidation of every outstanding
		// approval, which would look exactly like a bug.
		{
			name: "the order preconditions are listed in",
			mutate: func(p *Proposal) {
				p.Preconditions[0], p.Preconditions[1] = p.Preconditions[1], p.Preconditions[0]
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			tc.mutate(&p)
			if got, base := p.Fingerprint(), fixture().Fingerprint(); got != base {
				t.Errorf("changing %s moved the fingerprint to %s (was %s); a trusted fix would re-gate for "+
					"no reason a person could act on", tc.name, got, base)
			}
		})
	}
}

// Field boundaries must be unambiguous. Namespaces and object names are
// cluster-controlled strings, so a delimiter-joined encoding would let two different
// proposals hash identically — and a fingerprint collision is a proposal inheriting an
// approval that was given for something else.
func TestFingerprint_FieldsCannotBleedIntoEachOther(t *testing.T) {
	a, b := fixture(), fixture()
	a.Target.Namespace, a.Target.Name = "payments", "web-api"
	b.Target.Namespace, b.Target.Name = "payments-web", "api"

	if a.Fingerprint() == b.Fingerprint() {
		t.Errorf("%s/%s and %s/%s share a fingerprint; the encoding lets one field run into the next",
			a.Target.Namespace, a.Target.Name, b.Target.Namespace, b.Target.Name)
	}
}

// The fingerprint is written to world-readable artifacts, so it must carry no
// cluster-derived content — only a digest of it. This is why the trust model can store
// the token on an [audit.Action] without widening the surface that package redacts.
func TestFingerprint_LeaksNothingItWasComputedFrom(t *testing.T) {
	p := fixture()
	p.Target.Namespace = "secret-namespace"
	p.Target.Name = "secret-workload"
	p.Preconditions[1].Expect = "secret-namespace/secret-pod"

	got := string(p.Fingerprint())
	for _, secret := range []string{"secret-namespace", "secret-workload", "secret-pod", "payments", "prod"} {
		if strings.Contains(got, secret) {
			t.Errorf("fingerprint %q contains %q; it is rendered into public artifacts and must be opaque", got, secret)
		}
	}
}

// The planner version is in every fingerprint, so bumping it invalidates every
// outstanding approval. That is the intended effect — the reasoning that produced the
// fix changed — and it is asserted because the constant's whole value is that
// forgetting to bump it is the dangerous direction.
func TestFingerprint_CoversThePlannerVersion(t *testing.T) {
	p := fixture()
	if !strings.Contains(fingerprintPreimage(p), strconv.Itoa(PlannerVersion)) {
		t.Errorf("the planner version is not part of the fingerprint's inputs: %q", fingerprintPreimage(p))
	}

	// And the preimage really is what gets hashed, so a test reading it is reading the
	// same bytes the token was made from.
	if fingerprintPreimage(p) == fingerprintPreimage(mutated(p, func(q *Proposal) { q.Operation = OpDeletePod })) {
		t.Error("the preimage does not vary with the operation, so it is not the fingerprint's input")
	}
}

// Every real proposal the planner emits must fingerprint to something. A proposal that
// fingerprinted to the empty string would be permanently ungatable-out-of: the empty
// fingerprint matches no ledger entry, so it could never earn trust and the operator
// would see autonomy silently never fire.
func TestFingerprint_IsNeverEmptyForARealProposal(t *testing.T) {
	for _, op := range []Operation{OpRolloutRestart, OpRollbackRevision, OpDeletePod, OpCordonNode} {
		p := fixture()
		p.Operation = op
		if p.Fingerprint() == "" {
			t.Errorf("%s fingerprinted to the empty token", op)
		}
	}
	// Even a zero proposal produces one. The fingerprint is a hash of whatever it was
	// given, so "empty" is reserved for "nobody computed one" and cannot be reached by
	// computing one.
	if (Proposal{}).Fingerprint() == "" {
		t.Error("the zero proposal fingerprinted to the empty token, which is the value reserved for 'not computed'")
	}
}

// The two precondition classifications, asserted directly rather than only through
// their effect, because they are opposite-defaulting allowlists and a new kind lands
// on one side or the other by omission.
func TestPreconditionFingerprintClassification(t *testing.T) {
	all := []PreconditionKind{
		PreconditionUnchanged, PreconditionPodCrashLooping, PreconditionPodFailed,
		PreconditionPodHasController, PreconditionNodeNotReady, PreconditionNodeSchedulable,
		PreconditionRevisionExists,
	}

	for _, k := range all {
		if k == PreconditionUnchanged {
			if k.InFingerprint() {
				t.Errorf("%s is in the fingerprint; its expectation is the resourceVersion, which churns constantly", k)
			}
			continue
		}
		if !k.InFingerprint() {
			t.Errorf("%s is excluded from the fingerprint; kinds are included by default so a dropped guard "+
				"cannot survive as stale trust", k)
		}
	}

	// Only one kind's VALUE identifies the fix. A kind added here later without thought
	// re-gates every proposal carrying it, so the direction of this default matters as
	// much as the membership.
	for _, k := range all {
		want := k == PreconditionRevisionExists
		if got := k.BindsFingerprint(); got != want {
			t.Errorf("%s.BindsFingerprint() = %v, want %v", k, got, want)
		}
	}
}

// mutated returns a copy of p with one change applied, so a case can compare two
// proposals without a temporary variable per side.
func mutated(p Proposal, change func(*Proposal)) Proposal {
	change(&p)
	return p
}
