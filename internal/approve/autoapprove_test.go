package approve

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// The tests in this file cover the two halves of the autonomous-mode change, which
// shipped together because neither is safe alone: the bypass that lets MaKlaude act
// with no human in the loop, and the requirement — newly enforceable BECAUSE the bypass
// exists — that a gate still claiming to need a human must know which account MaKlaude
// is.
//
// Two properties get disproportionate coverage, and both are properties about what a
// human READS rather than about what the code computes. The first is that no artifact,
// comment, log line, or audit record ever describes a policy-waived action as though a
// person approved it: a wrong value there is not a bug that surfaces later, it is a
// permanent false entry in the record an incident review trusts. The second is that
// everything the bypass does NOT waive still fires, because a bypass that quietly took
// the drift check with it would be indistinguishable from this one right up until it
// restarted a deployment that had already been fixed.

// envOf returns a getenv function over a fixed map, so the env-reading paths are
// testable without t.Setenv's process-global mutation.
func envOf(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestAutoApproveFromEnvRefusesAnythingItCannotReadAsAYesOrANo(t *testing.T) {
	// The direction of the danger is asymmetric, which is why guessing is refused
	// rather than defaulted. "non-empty means on" turns =no and =off into an armed
	// autonomous mode set by somebody trying to disable it.
	on := []string{"1", "true", "TRUE", "True", " true "}
	off := []string{"", "0", "false", "FALSE", "  "}
	ambiguous := []string{"no", "off", "yes", "on", "y", "n", "2", "-1", "enabled", "disabled", "dangerously"}

	for _, v := range on {
		got, err := AutoApproveFromEnv(envOf(map[string]string{AutoApproveEnv: v}))
		if err != nil || !got {
			t.Errorf("%s=%q: got (%t, %v), want (true, nil)", AutoApproveEnv, v, got, err)
		}
	}
	for _, v := range off {
		got, err := AutoApproveFromEnv(envOf(map[string]string{AutoApproveEnv: v}))
		if err != nil || got {
			t.Errorf("%s=%q: got (%t, %v), want (false, nil)", AutoApproveEnv, v, got, err)
		}
	}
	for _, v := range ambiguous {
		got, err := AutoApproveFromEnv(envOf(map[string]string{AutoApproveEnv: v}))
		if !errors.Is(err, ErrAmbiguousAutoApprove) {
			t.Errorf("%s=%q: err = %v, want ErrAmbiguousAutoApprove — an unrecognized value must stop the process, not pick a meaning", AutoApproveEnv, v, err)
		}
		if got {
			t.Errorf("%s=%q: reported the bypass ON while also erroring", AutoApproveEnv, v)
		}
	}

	// A caller with no environment to consult gets the safe posture, not a panic.
	if got, err := AutoApproveFromEnv(nil); got || err != nil {
		t.Errorf("AutoApproveFromEnv(nil) = (%t, %v), want (false, nil)", got, err)
	}
}

func TestGateConfigRequiresASelfIdentityUnlessTheRequirementIsWaived(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name: "a known self-identity with the gate on is the shipped posture",
			env:  map[string]string{SelfLoginEnv: "maklaude-bot"},
		},
		{
			name: "an unknown self-identity with the gate on is the hole this closes",
			env:  map[string]string{},
			// Nothing is set, so the gate would enforce a human approval it has no way to
			// distinguish from its own.
			wantErr: true,
		},
		{
			name: "whitespace is not an identity",
			env:  map[string]string{SelfLoginEnv: "   "},
			// Fails for the same reason: a value that trims to nothing names no account.
			wantErr: true,
		},
		{
			name: "the bypass makes an unknown self-identity honest rather than hidden",
			env:  map[string]string{AutoApproveEnv: "1"},
		},
		{
			name: "both set is fine — a waived gate can still recognize itself",
			env:  map[string]string{AutoApproveEnv: "1", SelfLoginEnv: "maklaude-bot"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := GateConfigFromEnv(envOf(tc.env))
			if err != nil {
				t.Fatalf("reading the gate config: %v", err)
			}
			err = cfg.Check()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Check() = %v, want error = %t", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrSelfIdentityUnknown) {
				t.Fatalf("Check() = %v, want ErrSelfIdentityUnknown", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), AutoApproveEnv) {
				t.Errorf("the misconfiguration error does not name the deliberate way out (%s): %s", AutoApproveEnv, err)
			}
		})
	}
}

// TestSinkFromEnvRefusesToBuildALiveGateItCannotEnforce is the wiring-level half of
// the fix. The unit above proves the rule; this proves it is actually applied at the
// only place a production process builds a sink, and that the credential-less path is
// exempt for the stated reason rather than by oversight.
func TestSinkFromEnvRefusesToBuildALiveGateItCannotEnforce(t *testing.T) {
	live := func(t *testing.T) {
		t.Helper()
		t.Setenv("MAKLAUDE_GITHUB_REPO", "Sayfan-AI/MaKlaude")
		t.Setenv("MAKLAUDE_GITHUB_TOKEN", "not-a-real-token")
	}

	t.Run("a live trail with no self-identity and no waiver refuses", func(t *testing.T) {
		live(t)
		t.Setenv(SelfLoginEnv, "")
		t.Setenv(AutoApproveEnv, "")

		sink, _, err := SinkFromEnv()
		if !errors.Is(err, ErrSelfIdentityUnknown) {
			t.Fatalf("err = %v, want ErrSelfIdentityUnknown", err)
		}
		if sink != nil {
			t.Error("a sink was returned alongside the refusal; a caller ignoring the error would run an unenforceable gate")
		}
	})

	t.Run("a live trail with a self-identity builds", func(t *testing.T) {
		live(t)
		t.Setenv(SelfLoginEnv, "maklaude-bot")
		t.Setenv(AutoApproveEnv, "")

		sink, isLive, err := SinkFromEnv()
		if err != nil || !isLive || sink == nil {
			t.Fatalf("SinkFromEnv() = (%v, %t, %v), want a live sink", sink, isLive, err)
		}
	})

	t.Run("a live trail with the requirement waived builds without one", func(t *testing.T) {
		live(t)
		t.Setenv(SelfLoginEnv, "")
		t.Setenv(AutoApproveEnv, "1")

		sink, isLive, err := SinkFromEnv()
		if err != nil || !isLive || sink == nil {
			t.Fatalf("SinkFromEnv() = (%v, %t, %v), want a live sink", sink, isLive, err)
		}
	})

	t.Run("the credential-less path stays side-effect-free rather than fatal", func(t *testing.T) {
		// Nothing outside this process can label an in-memory artifact, so there is no
		// labeler to mistake for a human and nothing for the identity to protect. Making
		// this fatal would impose startup friction on a read-only deployment in exchange
		// for closing a hole that does not exist in it.
		t.Setenv("MAKLAUDE_GITHUB_REPO", "")
		t.Setenv("MAKLAUDE_GITHUB_TOKEN", "")
		t.Setenv(SelfLoginEnv, "")
		t.Setenv(AutoApproveEnv, "")

		sink, isLive, err := SinkFromEnv()
		if err != nil {
			t.Fatalf("SinkFromEnv() = %v, want the degraded memory sink", err)
		}
		if isLive {
			t.Error("an unconfigured trail reported itself live")
		}
		if _, ok := sink.(*MemorySink); !ok {
			t.Errorf("sink is %T, want *MemorySink", sink)
		}
	})

	t.Run("an unreadable bypass value is fatal wherever the trail points", func(t *testing.T) {
		t.Setenv("MAKLAUDE_GITHUB_REPO", "")
		t.Setenv("MAKLAUDE_GITHUB_TOKEN", "")
		t.Setenv(AutoApproveEnv, "off")

		if _, _, err := SinkFromEnv(); !errors.Is(err, ErrAmbiguousAutoApprove) {
			t.Fatalf("err = %v, want ErrAmbiguousAutoApprove", err)
		}
	})
}

// TestSelfApprovalByMaKlaudesConfiguredLoginIsRefused is the defense that was
// inoperative before this change, tested end to end through the sink rather than as a
// pure value.
//
// The distinction matters. [Decide] refusing a PendingAction whose ApproverIsSelf is
// already true was covered and always passed; what was broken was one layer down, where
// nothing ever SET that flag for a MaKlaude running under a personal account, because
// [SelfLoginEnv] was unset everywhere and the login branch of isSelfActor was
// unreachable. So this drives a real label event by MaKlaude's own configured login and
// asserts the whole path — sink attribution, decision, refusal — rather than the
// conclusion in isolation.
func TestSelfApprovalByMaKlaudesConfiguredLoginIsRefused(t *testing.T) {
	h := newHarness(t) // self login "maklaude-bot", bypass off
	req := testRequest()

	h.pass(req)
	artifact := h.only()

	// MaKlaude labels its own approval issue, from the account it runs as. In
	// `genesis serve` local mode this is not hypothetical: MaKlaude holds the
	// operator's own token and can do exactly this.
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "maklaude-bot", h.at.Add(time.Second)); err != nil {
		t.Fatalf("recording the self-applied label: %v", err)
	}
	h.at = h.at.Add(2 * time.Second)

	res := h.pass(req)
	if len(res.Authorized) != 0 {
		t.Fatal("MaKlaude approved its own proposal and the gate honored it — there is no gate")
	}
	if res.Refused != 1 {
		t.Fatalf("refused = %d, want 1", res.Refused)
	}

	after := h.only()
	if after.HasLabel(ApprovedLabel) {
		t.Error("the self-applied approval label survived the refusal")
	}
	last := after.Comments[len(after.Comments)-1]
	if !strings.Contains(last, ReasonSelfApproval.String()) {
		t.Errorf("the refusal does not carry the %s token: %s", ReasonSelfApproval, last)
	}
	if !strings.Contains(last, "MaKlaude's own account") {
		t.Errorf("the refusal does not explain itself to a human: %s", last)
	}
}

// TestBypassGrantsAnAuthorizationWhoseApproverIsNotALogin is the core promise of the
// autonomous path: it produces a real, usable permission slip, and that slip does not
// impersonate a person.
func TestBypassGrantsAnAuthorizationWhoseApproverIsNotALogin(t *testing.T) {
	req := testRequest()
	policy := autonomousPolicy()

	got := Decide(req, undecidedPending(req, policy), policy, passAt)
	if got.Kind != ActionAuthorize {
		t.Fatalf("kind = %s (reason %s), want authorize", got.Kind, got.Reason)
	}
	if got.Reason != ReasonAutoApproved {
		t.Fatalf("reason = %s, want %s — the token lands in the trail, and %q on an action nobody approved is the sentence this must never write",
			got.Reason, ReasonAutoApproved, ReasonApprovalValid)
	}

	auth := got.Authorization
	if !auth.Valid() {
		t.Fatal("the bypass produced no usable permission slip")
	}
	if auth.Authority() != AuthorityPolicy {
		t.Fatalf("Authority() = %s, want %s", auth.Authority(), AuthorityPolicy)
	}
	if auth.Authority().HumanReviewed() {
		t.Fatal("a policy-waived authorization reports that a human reviewed it")
	}
	if got := auth.Approver(); got != AutoApprovePolicy {
		t.Fatalf("Approver() = %q, want the policy marker %q", got, AutoApprovePolicy)
	}
	// A GitHub login is alphanumerics and hyphens only, so a colon makes the marker
	// structurally impossible to read as a person — a stronger guarantee than a value
	// that merely looks unlikely.
	if !strings.Contains(auth.Approver(), ":") {
		t.Errorf("Approver() = %q could be a registrable GitHub username", auth.Approver())
	}
	if !auth.ApprovedAt().IsZero() {
		t.Errorf("ApprovedAt() = %s, want the zero time — nothing was decided, so there is no instant to record", auth.ApprovedAt())
	}
	if !auth.AuthorizedAt().Equal(passAt) {
		t.Errorf("AuthorizedAt() = %s, want the pass time %s", auth.AuthorizedAt(), passAt)
	}

	// Everything a slip is FOR still holds: it is scoped to one action on one cluster
	// at one resourceVersion, exactly as a human-approved one is.
	if !auth.Matches(req.Proposal) {
		t.Error("the auto-approved slip does not match the proposal it was granted for")
	}
	if auth.Cluster() != "prod" || auth.Target().ResourceVersion != "1000" {
		t.Errorf("scope = cluster %q rv %q, want prod/1000", auth.Cluster(), auth.Target().ResourceVersion)
	}

	line := auth.String()
	for _, want := range []string{"AUTO-APPROVED", "NO HUMAN REVIEWED THIS"} {
		if !strings.Contains(line, want) {
			t.Errorf("String() = %q, missing %q — this is what lands in a log where nobody is reading carefully", line, want)
		}
	}
}

// TestBypassWaivesConsentAndNothingElse is the safety half. Each case takes the
// arrangement the bypass WOULD authorize and breaks exactly one non-consent condition,
// asserting that no permission slip comes out.
//
// The two shapes of refusal are both here on purpose. An artifact carrying an approval
// label is REFUSED (the label must come off and its author told why); one that never
// had one is HELD, because there is nothing to strip, nothing new to say, and a comment
// posted every pass forever is how a trail teaches its readers to stop reading it.
func TestBypassWaivesConsentAndNothingElse(t *testing.T) {
	policy := autonomousPolicy()
	base := testRequest()

	drifted := testRequest()
	drifted.Proposal.Target.ResourceVersion = "1001"

	failedPreview := testRequest()
	failedPreview.Preview.Error = "admission webhook denied the request"

	noPlan := testRequest()
	noPlan.Proposal.Operation = remediate.Operation("scale-to-zero-and-pray")

	cases := []struct {
		name       string
		req        Request
		pending    PendingAction
		wantKind   ActionKind
		wantReason Reason
	}{
		{
			name:       "a human's refusal outranks configured policy",
			req:        base,
			pending:    mutate(base, func(p *PendingAction) { p.State = StateRejected }),
			wantKind:   ActionHold,
			wantReason: ReasonRejected,
		},
		{
			name:       "the idempotency flag still stops a second run",
			req:        base,
			pending:    mutate(base, func(p *PendingAction) { p.Executed = true }),
			wantKind:   ActionHold,
			wantReason: ReasonAlreadyExecuted,
		},
		{
			name:       "drift on an approved artifact still refuses",
			req:        drifted,
			pending:    approvedPending(base),
			wantKind:   ActionRefuse,
			wantReason: ReasonDrift,
		},
		{
			name: "drift on an undecided artifact re-renders before anything is authorized against it",
			req:  drifted,
			// The artifact displays rv 1000 and the object is at 1001. Authorizing now
			// would leave a trail describing an action against a state it never showed.
			pending:    undecidedPending(base, policy),
			wantKind:   ActionRefresh,
			wantReason: ReasonPreviewChanged,
		},
		{
			name:       "a failed dry-run still blocks",
			req:        failedPreview,
			pending:    undecidedPending(failedPreview, policy),
			wantKind:   ActionHold,
			wantReason: ReasonPreviewFailed,
		},
		{
			name:       "an operation with no rollback plan still blocks",
			req:        noPlan,
			pending:    undecidedPending(noPlan, policy),
			wantKind:   ActionHold,
			wantReason: ReasonNoRollbackPlan,
		},
		{
			name: "a body written under the human-gated posture re-renders before it is acted on",
			req:  base,
			// The operator has just turned the bypass on. Every open artifact still
			// promises that nothing runs without a label; one refresh pass fixes that
			// before anything is authorized under the new posture.
			pending:    undecidedPending(base, DefaultPolicy()),
			wantKind:   ActionRefresh,
			wantReason: ReasonPreviewChanged,
		},
		{
			name:       "a proposal with no artifact is still asked before it is answered",
			req:        base,
			pending:    PendingAction{},
			wantKind:   ActionOpen,
			wantReason: ReasonNewProposal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.req, tc.pending, policy, passAt)
			if got.Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s", got.Kind, tc.wantKind)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", got.Reason, tc.wantReason)
			}
			if got.Authorization.Valid() {
				t.Fatalf("the bypass authorized past a non-consent check (kind %s, reason %s)", got.Kind, got.Reason)
			}
		})
	}
}

// TestBypassStillDistinguishesARealHumanApproval checks that turning the bypass on does
// not flatten every authorization into a policy waiver.
//
// It matters in both directions. Recording a genuine review as a waiver understates
// what happened and would train an operator to ignore the warning; recording a
// self-applied or unattributable label as a human approval is the forgery the whole
// change exists to prevent. The rule is that attribution requires positive evidence —
// a named actor who is demonstrably not MaKlaude.
func TestBypassStillDistinguishesARealHumanApproval(t *testing.T) {
	req := testRequest()
	policy := autonomousPolicy()

	cases := []struct {
		name          string
		pending       PendingAction
		wantAuthority Authority
		wantApprover  string
		wantReason    Reason
	}{
		{
			name:          "a named person who is not MaKlaude is recorded as a human approval",
			pending:       approvedPending(req),
			wantAuthority: AuthorityHuman,
			wantApprover:  "the-gigi",
			wantReason:    ReasonApprovalValid,
		},
		{
			name: "a label MaKlaude applied to its own artifact is a policy waiver, not an approval",
			pending: mutate(req, func(p *PendingAction) {
				p.Approver = "maklaude-bot"
				p.ApproverIsSelf = true
			}),
			wantAuthority: AuthorityPolicy,
			wantApprover:  AutoApprovePolicy,
			wantReason:    ReasonAutoApproved,
		},
		{
			name:          "an approval with no recoverable actor is a policy waiver too",
			pending:       mutate(req, func(p *PendingAction) { p.Approver = "" }),
			wantAuthority: AuthorityPolicy,
			wantApprover:  AutoApprovePolicy,
			wantReason:    ReasonAutoApproved,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(req, tc.pending, policy, passAt)
			if got.Kind != ActionAuthorize {
				t.Fatalf("kind = %s (reason %s), want authorize", got.Kind, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", got.Reason, tc.wantReason)
			}
			if got := got.Authorization.Authority(); got != tc.wantAuthority {
				t.Errorf("Authority() = %s, want %s", got, tc.wantAuthority)
			}
			if got := got.Authorization.Approver(); got != tc.wantApprover {
				t.Errorf("Approver() = %q, want %q", got, tc.wantApprover)
			}
		})
	}
}

// TestHumanApprovalIsUnaffectedByTheBypassBeingOff is the control for the table above:
// with the bypass off, the self-applied and unattributed cases are refusals rather than
// waivers, so the two tables together show the bypass changing exactly those two
// outcomes and nothing else about them.
func TestHumanApprovalIsUnaffectedByTheBypassBeingOff(t *testing.T) {
	req := testRequest()

	for name, tc := range map[string]struct {
		pending PendingAction
		want    Reason
	}{
		"self-applied": {
			pending: mutate(req, func(p *PendingAction) { p.Approver = "maklaude-bot"; p.ApproverIsSelf = true }),
			want:    ReasonSelfApproval,
		},
		"unattributed": {
			pending: mutate(req, func(p *PendingAction) { p.Approver = "" }),
			want:    ReasonUnattributedApproval,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := Decide(req, tc.pending, DefaultPolicy(), passAt)
			if got.Reason != tc.want || got.Authorization.Valid() {
				t.Fatalf("reason = %s (authorized = %t), want %s and no slip", got.Reason, got.Authorization.Valid(), tc.want)
			}
		})
	}
}

// TestAutonomousArtifactNeverPromisesAHumanWillLook covers the artifact itself, which
// is the only thing most readers will ever see.
//
// The failure this guards is specific and nasty: the human-gated body's opening line
// says nothing runs until somebody adds a label, and under the bypass that sentence is
// false at the exact moment a reader most needs it to be true. An artifact that keeps
// making the promise while MaKlaude acts is worse than one that says nothing.
func TestAutonomousArtifactNeverPromisesAHumanWillLook(t *testing.T) {
	req := testRequest()
	body := Body(req, previewAt, autonomousPolicy())

	if strings.Contains(body, "Nothing runs until a human") {
		t.Error("the autonomous body still promises that nothing runs without a human")
	}
	for _, want := range []string{
		"AUTONOMOUS MODE IS ENABLED",
		AutoApproveEnv,
		"no human will review it",
		"not waiting for you",
		"How to stop this",
		"`" + RejectedLabel + "`", // the one action still available to a reader
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the autonomous body is missing %q", want)
		}
	}

	// It must still carry everything an approver needed, because a reader who wants to
	// stop the action needs the same facts as one who wanted to allow it.
	for _, want := range []string{"Exactly what will run", "Dry-run preview", "Reversibility and rollback", "restart count 7"} {
		if !strings.Contains(body, want) {
			t.Errorf("the autonomous body dropped %q, which a reader deciding whether to stop this still needs", want)
		}
	}

	// And the human-gated body must be unchanged, or this test would pass by having
	// made both bodies say the same thing.
	gated := Body(req, previewAt, DefaultPolicy())
	if !strings.Contains(gated, "Nothing runs until a human") {
		t.Error("the human-gated body lost its central promise")
	}
	if strings.Contains(gated, "AUTONOMOUS MODE IS ENABLED") {
		t.Error("the human-gated body claims autonomous mode is on")
	}
}

func TestGateMarkerRoundTripsAndForcesARefreshWhenThePostureFlips(t *testing.T) {
	req := testRequest()

	gated := Body(req, previewAt, DefaultPolicy())
	if got := ParseGateMarker(gated); got != gateHumanGated {
		t.Fatalf("gate marker = %q, want %q", got, gateHumanGated)
	}
	auto := Body(req, previewAt, autonomousPolicy())
	if got := ParseGateMarker(auto); got != gateAutonomous {
		t.Fatalf("gate marker = %q, want %q", got, gateAutonomous)
	}

	// Absence reads as unknown and re-renders, so a body written before the marker
	// existed is not assumed to describe the posture currently in force.
	if got := ParseGateMarker("no markers here"); got != "" {
		t.Errorf("ParseGateMarker on an old body = %q, want empty", got)
	}
	stale := undecidedPending(req, DefaultPolicy())
	stale.GateMode = ""
	if previewCurrent(req, stale, DefaultPolicy()) {
		t.Error("a body with no gate marker was treated as current; an unparseable artifact must fail closed and re-render")
	}
}

// TestAutoApprovalIsAnnouncedEverywhereAHumanMightLook drives the gate end to end with
// the bypass on and checks all three places the fact has to land: the log line an
// operator watching the process sees, the comment on the durable artifact, and the
// permission slip handed to the executor.
//
// Three, rather than one, because each has a different reader and a different failure.
// The log reaches somebody now and is lost on restart; the comment is permanent and is
// what an incident review reads; the slip is what the audit trail is derived from.
func TestAutoApprovalIsAnnouncedEverywhereAHumanMightLook(t *testing.T) {
	h := newPolicyHarness(t, autonomousPolicy())
	req := testRequest()

	// Pass 1 opens the artifact as a notice.
	if res := h.pass(req); res.Opened != 1 || len(res.Authorized) != 0 {
		t.Fatalf("opening pass: opened=%d authorized=%d, want 1 and 0", res.Opened, len(res.Authorized))
	}
	opened := h.only()
	if !strings.Contains(opened.Body, "AUTONOMOUS MODE IS ENABLED") {
		t.Error("the artifact opened without saying autonomous mode is on")
	}
	if !opened.HasLabel(NeedsHumanLabel) {
		t.Error("the artifact does not carry the label that puts it in front of an operator; under autonomous mode seeing it is the only thing left to do")
	}

	// Pass 2 authorizes with no human having touched anything.
	h.at = h.at.Add(time.Second)
	res := h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d, want 1 — the bypass did not close the loop", len(res.Authorized))
	}
	auth := res.Authorized[0]
	if auth.Authority() != AuthorityPolicy {
		t.Errorf("Authority() = %s, want %s", auth.Authority(), AuthorityPolicy)
	}

	logged := h.logs.String()
	for _, want := range []string{"WARNING", "AUTO-APPROVED WITHOUT HUMAN REVIEW", AutoApproveEnv, "deployment/shop/web"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the process log is missing %q: %s", want, logged)
		}
	}

	after := h.only()
	last := after.Comments[len(after.Comments)-1]
	for _, want := range []string{"NO HUMAN REVIEWED THIS", AutoApproveEnv, "waived by configuration"} {
		if !strings.Contains(last, want) {
			t.Errorf("the authorization comment is missing %q: %s", want, last)
		}
	}
	if strings.Contains(last, "Approval honored") {
		t.Errorf("the authorization comment reads as a human approval: %s", last)
	}
	if strings.Contains(last, "@"+AutoApprovePolicy) {
		t.Errorf("the authorization comment @-mentions the policy marker as though it were a person: %s", last)
	}
}

// TestHumanApprovalIsNotWarnedAboutInAutonomousMode guards the warning's signal-to-noise
// ratio. The gate warns on the AUTHORITY of the action, not on whether the bypass is
// configured, so a process in autonomous mode that receives a genuine human approval
// stays quiet — otherwise an operator learns to filter out the line before it ever
// carries real news.
func TestHumanApprovalIsNotWarnedAboutInAutonomousMode(t *testing.T) {
	h := newPolicyHarness(t, autonomousPolicy())
	req := testRequest()

	h.pass(req)
	artifact := h.only()
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", h.at.Add(time.Second)); err != nil {
		t.Fatalf("recording the human decision: %v", err)
	}
	h.at = h.at.Add(2 * time.Second)

	res := h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d, want 1", len(res.Authorized))
	}
	if got := res.Authorized[0].Authority(); got != AuthorityHuman {
		t.Fatalf("Authority() = %s, want %s — a real approval must not be flattened into a waiver", got, AuthorityHuman)
	}
	if strings.Contains(h.logs.String(), "AUTO-APPROVED") {
		t.Errorf("a human-approved action was warned about: %s", h.logs.String())
	}
	last := h.only().Comments[len(h.only().Comments)-1]
	if !strings.Contains(last, "Approval honored") || !strings.Contains(last, "@the-gigi") {
		t.Errorf("the artifact does not credit the human who approved it: %s", last)
	}
}

// TestExecutionCommentNeverDressesAWaiverAsAReview covers the last and highest-stakes
// rendering: everything before it describes something that MIGHT happen, and this
// describes a cluster that already changed.
func TestExecutionCommentNeverDressesAWaiverAsAReview(t *testing.T) {
	req := testRequest()
	policy := autonomousPolicy()

	waived := Decide(req, undecidedPending(req, policy), policy, passAt).Authorization
	human := Decide(req, approvedPending(req), DefaultPolicy(), passAt).Authorization

	got := ExecutionComment(waived, "sent via PATCH")
	for _, want := range []string{"NO HUMAN REVIEWED THIS", AutoApproveEnv} {
		if !strings.Contains(got, want) {
			t.Errorf("the waived execution note is missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "approved by @") {
		t.Errorf("the waived execution note claims somebody approved it: %s", got)
	}
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("the waived execution note printed a zero timestamp instead of dropping the clause: %s", got)
	}

	got = ExecutionComment(human, "sent via PATCH")
	if !strings.Contains(got, "approved by @the-gigi") {
		t.Errorf("the human execution note does not name the approver: %s", got)
	}
	if strings.Contains(got, "NO HUMAN REVIEWED THIS") {
		t.Errorf("a human-approved execution was recorded as unreviewed: %s", got)
	}
}

// TestRefusalUnderTheBypassDoesNotImplyMaKlaudeIsWaiting covers the sentence a reader
// acts on. "Re-add the label if you still want this" is correct with the gate on and
// misleading with it off: MaKlaude re-decides on its own next pass, so a reader who
// believes it is waiting has just lost their only window to intervene.
func TestRefusalUnderTheBypassDoesNotImplyMaKlaudeIsWaiting(t *testing.T) {
	req := testRequest()
	pending := approvedPending(req)
	pending.PreviewedResourceVersion = "999"

	auto := RefusalComment(req, pending, ReasonDrift, autonomousPolicy())
	if !strings.Contains(auto, "not waiting for you") {
		t.Errorf("the refusal implies MaKlaude is waiting for a decision it will not wait for: %s", auto)
	}
	if !strings.Contains(auto, "`"+RejectedLabel+"`") {
		t.Errorf("the refusal does not say how to actually stop the action: %s", auto)
	}

	gated := RefusalComment(req, pending, ReasonDrift, DefaultPolicy())
	if !strings.Contains(gated, "Re-add the `"+ApprovedLabel+"` label") {
		t.Errorf("the human-gated refusal lost its call to action: %s", gated)
	}
	if strings.Contains(gated, "not waiting for you") {
		t.Errorf("the human-gated refusal claims MaKlaude is not waiting: %s", gated)
	}
}

func TestApprovalSummaryTellsAChatReaderWhichPostureIsInForce(t *testing.T) {
	req := testRequest()

	gated := ApprovalSummary(req, DefaultPolicy())
	if !strings.Contains(gated, "Nothing runs until a human") {
		t.Errorf("the human-gated chat summary lost its central promise: %s", gated)
	}

	auto := ApprovalSummary(req, autonomousPolicy())
	if strings.Contains(auto, "Nothing runs until a human") {
		t.Errorf("the autonomous chat summary repeats a promise the bypass breaks: %s", auto)
	}
	for _, want := range []string{"AUTONOMOUS MODE", "NO human review", "`" + RejectedLabel + "`"} {
		if !strings.Contains(auto, want) {
			t.Errorf("the autonomous chat summary is missing %q: %s", want, auto)
		}
	}
}

func TestAuthorityStringsAreStableAndDistinct(t *testing.T) {
	seen := map[string]Authority{}
	for _, a := range []Authority{AuthorityNone, AuthorityHuman, AuthorityPolicy} {
		got := a.String()
		if strings.HasPrefix(got, "authority(") {
			t.Errorf("Authority(%d) has no rendering", int(a))
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("Authority(%d) and Authority(%d) both render as %q", int(prev), int(a), got)
		}
		seen[got] = a
	}
	if got := Authority(99).String(); got != "authority(99)" {
		t.Errorf("Authority(99).String() = %q, want the fallback rendering", got)
	}

	// Only the human authority reports human review. An authority nobody has thought
	// of yet must not, because that is the direction that launders an unreviewed action.
	for _, a := range []Authority{AuthorityNone, AuthorityPolicy, Authority(99)} {
		if a.HumanReviewed() {
			t.Errorf("Authority(%d).HumanReviewed() = true", int(a))
		}
	}
	if !AuthorityHuman.HumanReviewed() {
		t.Error("AuthorityHuman.HumanReviewed() = false")
	}
}

// TestInvalidAuthorizationReportsNoAuthority extends the unforgeability guarantee to
// the new accessor: a struct literal another package writes must not be able to claim
// an authority any more than it can claim to be valid.
func TestInvalidAuthorizationReportsNoAuthority(t *testing.T) {
	forged := &Authorization{authority: AuthorityHuman, approver: "definitely-a-human"}
	if got := forged.Authority(); got != AuthorityNone {
		t.Errorf("Authority() = %s on an ungranted authorization, want %s", got, AuthorityNone)
	}
	var nilAuth *Authorization
	if got := nilAuth.Authority(); got != AuthorityNone {
		t.Errorf("Authority() = %s on a nil authorization, want %s", got, AuthorityNone)
	}
	if got := AuthorizationComment(nilAuth); !strings.Contains(got, "did not issue") {
		t.Errorf("AuthorizationComment(nil) = %q, want it to flag the bug", got)
	}
}

// TestBypassDoesNotChangeWhichReasonIsReportedWhenSeveralApply pins that the surviving
// checks keep their documented order. A refactor that moved the waived block could
// silently reorder them, and the reason token is what an operator reads first on an
// artifact that is wrong in more than one way.
func TestBypassDoesNotChangeWhichReasonIsReportedWhenSeveralApply(t *testing.T) {
	req := testRequest()
	req.Preview.Error = "the server rejected the patch"
	req.Proposal.Target.ResourceVersion = "1001" // also drifted

	for name, policy := range map[string]Policy{"gated": DefaultPolicy(), "autonomous": autonomousPolicy()} {
		t.Run(name, func(t *testing.T) {
			got := Decide(req, approvedPending(testRequest()), policy, passAt)
			if got.Reason != ReasonPreviewFailed {
				t.Fatalf("reason = %s, want %s — an action the server refuses is not worth discussing the freshness of",
					got.Reason, ReasonPreviewFailed)
			}
		})
	}
}

// TestGatekeeperReconcileIsUnchangedForTheHumanPath is a belt-and-braces check that
// nothing in this change altered the default posture, driven through the full gate
// rather than through Decide.
func TestGatekeeperReconcileIsUnchangedForTheHumanPath(t *testing.T) {
	h := newHarness(t)
	req := testRequest()

	// Several passes with no decision authorize nothing, forever.
	for i := 0; i < 3; i++ {
		res := h.pass(req)
		if len(res.Authorized) != 0 {
			t.Fatalf("pass %d authorized without a human decision", i+1)
		}
		h.at = h.at.Add(time.Minute)
	}
	if strings.Contains(h.logs.String(), "AUTO-APPROVED") {
		t.Errorf("the human-gated gate warned about an auto-approval: %s", h.logs.String())
	}

	artifact := h.only()
	if err := h.sink.Decide(artifact.Ref, ApprovedLabel, "the-gigi", h.at); err != nil {
		t.Fatalf("recording the decision: %v", err)
	}
	h.at = h.at.Add(time.Second)

	res := h.pass(req)
	if len(res.Authorized) != 1 {
		t.Fatalf("authorized = %d, want 1", len(res.Authorized))
	}
	if got := res.Authorized[0].Approver(); got != "the-gigi" {
		t.Errorf("Approver() = %q, want the human's login", got)
	}
	if got := res.Authorized[0].Authority(); got != AuthorityHuman {
		t.Errorf("Authority() = %s, want %s", got, AuthorityHuman)
	}
}
