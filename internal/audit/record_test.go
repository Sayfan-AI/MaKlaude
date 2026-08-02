package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/redact"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// seededSecret is a GitHub token shape the redactor recognizes. Tests plant it in
// one field at a time so a failure names the exact field that leaked rather than
// reporting that "something" got through.
const seededSecret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// fullRecord builds a record with every field populated, so a test asserting what
// survives redaction is asserting over the whole type rather than over the handful
// of fields whoever wrote the test remembered.
func fullRecord() Record {
	return Record{
		Phase: PhaseVerified,
		Action: Action{
			Identity:      remediate.ProposalIdentity("proposal|cordonnode|prod|node/node-a"),
			Cluster:       "prod",
			Operation:     remediate.OpCordonNode,
			Target:        remediate.Target{Cluster: "prod", Kind: "node", Name: "node-a", ResourceVersion: "1001"},
			Reversibility: remediate.ReversibilityReversible,
			Title:         "Cordon NotReady node",
			ProposedAt:    time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		},
		Approver: Approver{
			Authority:    AuthorityHuman,
			Identity:     "the-gigi",
			ApprovedAt:   time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
			AuthorizedAt: time.Date(2026, 8, 1, 11, 0, 2, 0, time.UTC),
			Ref:          "42",
		},
		Change: Change{
			Sent:            true,
			Applied:         true,
			Mode:            "enabled",
			Scope:           "PATCH /api/v1/nodes/node-a",
			ResourceVersion: "1001",
			Attempts:        1,
			RecordedOnTrail: true,
			StartedAt:       time.Date(2026, 8, 1, 11, 0, 3, 0, time.UTC),
			FinishedAt:      time.Date(2026, 8, 1, 11, 0, 5, 0, time.UTC),
		},
		PreState: PreState{
			Captured:        true,
			Kind:            "node",
			ResourceVersion: "1001",
			ObservedAt:      time.Date(2026, 8, 1, 11, 0, 3, 0, time.UTC),
			Fields:          []PreStateField{{Name: "unschedulable", Value: "false"}, {Name: "ready", Value: "false"}},
		},
		Outcome: Outcome{
			Convergence: "converged",
			Detail:      `node "node-a" is cordoned`,
			ObservedFor: 2 * time.Second,
			Failure:     "none",
		},
		Rollback: Rollback{
			Kind:      "performable",
			Note:      "Uncordon the node to make it schedulable again.",
			Available: true,
		},
	}
}

// TestRecord_RedactsEveryClusterDerivedFreeTextField plants the same recognizable
// token in every field whose content originates outside MaKlaude and proves none of
// them survives.
//
// One field per subtest rather than one record with the token everywhere: a single
// combined assertion tells you a leak happened, and this tells you which field
// leaked. Adding a free-text field to [Record] and forgetting to redact it is the
// realistic way this protection decays, and it decays silently.
func TestRecord_RedactsEveryClusterDerivedFreeTextField(t *testing.T) {
	cases := map[string]func(*Record){
		"the free-form detail":        func(r *Record) { r.Detail = "context: " + seededSecret },
		"the convergence detail":      func(r *Record) { r.Outcome.Detail = "container said " + seededSecret },
		"the terminating error":       func(r *Record) { r.Outcome.Error = "admission denied: " + seededSecret },
		"the rollback note":           func(r *Record) { r.Rollback.Note = "run: kubectl --token=" + seededSecret },
		"the rollback description":    func(r *Record) { r.Rollback.Description = "uncordon with " + seededSecret },
		"a captured pre-state value":  func(r *Record) { r.PreState.Fields[0].Value = seededSecret },
		"a later pre-state value too": func(r *Record) { r.PreState.Fields[1].Value = seededSecret },
	}

	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			rec := fullRecord()
			plant(&rec)

			got := rec.redacted()
			if leaked(got) {
				t.Fatalf("the secret survived redaction in %s: %+v", name, got)
			}
			if !strings.Contains(renderAll(got), redact.Placeholder) {
				t.Fatalf("redaction removed the secret without leaving a placeholder, so a reader cannot tell material was removed: %+v", got)
			}
		})
	}
}

// TestRecord_KeepsTheStructuredIdentifiers is the other half of the redaction
// contract, and the half a "redact everything" implementation would fail.
//
// The trail's whole value is that a record can be traced back to an action, an
// object, and a person. The high-entropy sweep would happily blank a long
// deployment name or a proposal identity, and a record whose target reads
// "[REDACTED]" is not an audit record — so the identifiers are deliberately exempt
// and that exemption is asserted rather than assumed.
func TestRecord_KeepsTheStructuredIdentifiers(t *testing.T) {
	rec := fullRecord()
	// A name long enough to trip the high-entropy sweep if it were ever applied here.
	rec.Action.Target.Name = "checkout-api-canary-deployment-blue"
	rec.Action.Identity = remediate.ProposalIdentity("proposal|cordonnode|prod|node/checkout-api-canary-deployment-blue")

	got := rec.redacted()

	for what, value := range map[string]string{
		"the proposal identity": string(got.Action.Identity),
		"the target name":       got.Action.Target.Name,
		"the cluster":           got.Action.Cluster,
		"the operation":         string(got.Action.Operation),
		"the title":             got.Action.Title,
		"the approver":          got.Approver.Identity,
		"the artifact ref":      got.Approver.Ref,
		"the pre-state kind":    got.PreState.Kind,
		"the resourceVersion":   got.Change.ResourceVersion,
	} {
		if strings.Contains(value, redact.Placeholder) {
			t.Errorf("%s was redacted (%q); the trail's linkage depends on it surviving", what, value)
		}
	}
	if got.Action.Target.Name != rec.Action.Target.Name {
		t.Errorf("target name changed: %q → %q", rec.Action.Target.Name, got.Action.Target.Name)
	}
}

// TestRecord_KeepsTheWriteScopeIntact is the same exemption as the identifiers
// above, split out because it is the one that was wrong (#132) and the one whose
// membership a future reader is most likely to doubt.
//
// A real API path is a single unbroken run of characters the high-entropy sweep
// matches, so redacting this field does not trim it — it destroys it whole, and
// every record ever written said "PATCH /[REDACTED]". The scope carries no secret
// to protect: it is a mutating method plus a path built from a fixed
// group/version/resource triple and API-server-validated DNS-1123 names, with no
// query string. The case below is the exact scope the T6 e2e produces, which is
// where the collapse was found.
func TestRecord_KeepsTheWriteScopeIntact(t *testing.T) {
	const scope = "PATCH /apis/apps/v1/namespaces/maklaude-e2e/deployments/wedged"

	rec := fullRecord()
	rec.Change.Scope = scope
	// The executed phase is the one whose rendering carries the scope at all, so it
	// is the only phase where the collapse was visible and the only one worth
	// asserting the rendering of.
	rec.Phase = PhaseExecuted

	got := rec.redacted()

	if got.Change.Scope != scope {
		t.Errorf("the write scope did not survive redaction: %q → %q", scope, got.Change.Scope)
	}
	// The record's rendering is what a human actually reads, so the intact value has
	// to reach it too — a field kept whole and then dropped from the rendering
	// answers nothing.
	if !strings.Contains(got.String(), scope) {
		t.Errorf("the rendered record does not carry the write scope %q:\n%s", scope, got.String())
	}
}

// TestRecord_RedactionDoesNotAliasTheOriginal proves redaction copies the pre-state
// fields rather than writing through the caller's slice. A record is a value, and a
// sanitizing step that mutated its caller's data in place would be a surprise
// wherever the unsanitized copy was still being used.
func TestRecord_RedactionDoesNotAliasTheOriginal(t *testing.T) {
	rec := fullRecord()
	rec.PreState.Fields[0].Value = seededSecret

	_ = rec.redacted()

	if rec.PreState.Fields[0].Value != seededSecret {
		t.Fatalf("redacted() mutated its receiver's pre-state field: %q", rec.PreState.Fields[0].Value)
	}
}

// TestApprover_StatesTheKindOfAuthority is the guarantee issue #124 depends on: an
// action nobody reviewed must never render as one somebody did.
//
// The policy case is the one that matters. The bypass it anticipates does not exist
// yet, which is exactly why the rendering is pinned now — a record shape that can
// only express "a human approved this" is one that gets stretched into a lie the
// day the bypass lands.
func TestApprover_StatesTheKindOfAuthority(t *testing.T) {
	cases := map[string]struct {
		approver   Approver
		want       []string
		mustNotSay []string
	}{
		"a human": {
			approver: Approver{Authority: AuthorityHuman, Identity: "the-gigi"},
			want:     []string{"@the-gigi", "human approval"},
		},
		"policy waived it": {
			approver:   Approver{Authority: AuthorityPolicy, Identity: "MAKLAUDE_DANGEROUSLY_AUTO_APPROVE"},
			want:       []string{"policy waived approval", "no human reviewed this"},
			mustNotSay: []string{"@"},
		},
		"nobody": {
			approver:   Approver{},
			want:       []string{"unattributed", "no valid authorization"},
			mustNotSay: []string{"@"},
		},
		"an identity with no authority is not silently promoted": {
			approver:   Approver{Identity: "someone"},
			want:       []string{"unattributed"},
			mustNotSay: []string{"someone"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := tc.approver.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("approver rendered as %q, want it to mention %q", got, want)
				}
			}
			for _, bad := range tc.mustNotSay {
				if strings.Contains(got, bad) {
					t.Errorf("approver rendered as %q, which must not contain %q", got, bad)
				}
			}
		})
	}
}

// TestAuthority_HumanReviewedIsExactlyOneValue guards the predicate every consumer
// uses to decide whether to say "reviewed". Widening it by accident — treating a
// future authority as human because it is "not unattributed" — is the failure this
// catches.
func TestAuthority_HumanReviewedIsExactlyOneValue(t *testing.T) {
	for _, a := range []Authority{AuthorityUnattributed, AuthorityPolicy, Authority(99)} {
		if a.HumanReviewed() {
			t.Errorf("%s reports HumanReviewed", a)
		}
	}
	if !AuthorityHuman.HumanReviewed() {
		t.Error("AuthorityHuman does not report HumanReviewed")
	}
}

// TestTokensAreStableAndCoverUnknownValues pins the strings that appear in stored
// records and rendered artifacts. They are a contract; an unrecognized value must
// render as an obvious placeholder rather than as an existing token.
func TestTokensAreStableAndCoverUnknownValues(t *testing.T) {
	phases := map[Phase]string{
		PhaseUnknown:    "unknown",
		PhaseProposed:   "proposed",
		PhaseApproved:   "approved",
		PhaseExecuted:   "executed",
		PhaseVerified:   "verified",
		PhaseFailed:     "failed",
		PhaseRolledBack: "rolled-back",
		Phase(99):       "phase(99)",
	}
	for phase, want := range phases {
		if got := phase.String(); got != want {
			t.Errorf("phase %d rendered %q, want %q", int(phase), got, want)
		}
	}

	authorities := map[Authority]string{
		AuthorityUnattributed: "unattributed",
		AuthorityHuman:        "human",
		AuthorityPolicy:       "policy",
		Authority(99):         "authority(99)",
	}
	for authority, want := range authorities {
		if got := authority.String(); got != want {
			t.Errorf("authority %d rendered %q, want %q", int(authority), got, want)
		}
	}
}

// TestChange_DurationRefusesNonsense proves an unset or inverted pair of timestamps
// reports zero rather than a negative or absurd duration. An audit trail claiming an
// action took minus four hours is a trail nobody trusts about anything else either.
func TestChange_DurationRefusesNonsense(t *testing.T) {
	start := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]Change{
		"nothing set":     {},
		"no finish":       {StartedAt: start},
		"no start":        {FinishedAt: start},
		"finish precedes": {StartedAt: start, FinishedAt: start.Add(-time.Hour)},
	}
	for name, c := range cases {
		if got := c.Duration(); got != 0 {
			t.Errorf("%s: duration = %s, want 0", name, got)
		}
	}
	ok := Change{StartedAt: start, FinishedAt: start.Add(3 * time.Second)}
	if got := ok.Duration(); got != 3*time.Second {
		t.Errorf("duration = %s, want 3s", got)
	}
}

// TestOutcome_FailedTreatsTheAbsentAndTheNoneTokenAlike guards the one comparison
// every consumer makes. A zero-value Outcome and one explicitly carrying "none" are
// both "did not fail", and a check written as `Failure != ""` would call the first
// of those a success and the second a failure.
func TestOutcome_FailedTreatsTheAbsentAndTheNoneTokenAlike(t *testing.T) {
	if (Outcome{}).Failed() {
		t.Error("a zero outcome reports failed")
	}
	if (Outcome{Failure: "none"}).Failed() {
		t.Error(`an outcome carrying "none" reports failed`)
	}
	if !(Outcome{Failure: "drifted"}).Failed() {
		t.Error("a drifted outcome does not report failed")
	}
}

// leaked reports whether the seeded secret survives anywhere in a record.
func leaked(rec Record) bool {
	return strings.Contains(renderAll(rec), seededSecret)
}

// renderAll flattens every string a record carries, so a leak check cannot miss a
// field by only looking at the ones it thought to name.
func renderAll(rec Record) string {
	parts := []string{
		rec.Detail,
		string(rec.Action.Identity), rec.Action.Cluster, string(rec.Action.Operation),
		rec.Action.Target.String(), rec.Action.Target.ResourceVersion, rec.Action.Title,
		rec.Approver.Identity, rec.Approver.Ref,
		rec.Change.Mode, rec.Change.Scope, rec.Change.ResourceVersion,
		rec.PreState.Kind, rec.PreState.ResourceVersion,
		rec.Outcome.Convergence, rec.Outcome.Detail, rec.Outcome.Failure, rec.Outcome.Error,
		rec.Rollback.Kind, rec.Rollback.Note, rec.Rollback.Description,
		rec.String(),
	}
	for _, f := range rec.PreState.Fields {
		parts = append(parts, f.Name, f.Value)
	}
	return strings.Join(parts, "\n")
}
