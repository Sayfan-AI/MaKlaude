package approve

import (
	"strings"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/remediate"
)

// Fixed instants for the whole package's tests. Every time-dependent property here
// — approval freshness, approval-before-preview ordering, pending expiry — is about
// the ORDER of two instants, so the tests name them rather than computing offsets
// at the call site.
var (
	proposedAt = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	previewAt  = time.Date(2026, 8, 1, 12, 0, 30, 0, time.UTC)
	decidedAt  = time.Date(2026, 8, 1, 12, 1, 0, 0, time.UTC)
	passAt     = time.Date(2026, 8, 1, 12, 2, 0, 0, time.UTC)
)

// testProposal returns a realistic, fully-populated proposal. It is deliberately the
// SAFEST operation in the catalog with a defined rollback plan, so a test that wants
// to exercise a refusal has to introduce the refusal itself rather than inheriting
// one from the fixture.
func testProposal() remediate.Proposal {
	return remediate.Proposal{
		Identity:   remediate.ProposalIdentity("rolloutrestart|prod|deployment|shop|web"),
		Hypothesis: "hyp-badimage-1",
		Incident:   "inc-shop-web",
		Cause:      diagnose.CauseBadImage,
		Confidence: diagnose.ConfidenceHigh,
		Cluster:    "prod",
		Operation:  remediate.OpRolloutRestart,
		Target: remediate.Target{
			Cluster:         "prod",
			Kind:            "deployment",
			Namespace:       "shop",
			Name:            "web",
			ResourceVersion: "1000",
		},
		Reversibility:  remediate.ReversibilityReversible,
		Title:          "Restart the rollout of deployment shop/web",
		Intent:         "the running pods are stuck on an image that never pulls",
		ExpectedEffect: "pods are replaced gradually by a fresh rollout",
		Preconditions: []remediate.Precondition{{
			Kind:        remediate.PreconditionUnchanged,
			Expect:      "1000",
			Description: "the deployment has not changed since the snapshot",
		}},
		Evidence: []detect.Finding{{
			Identity: "finding-1",
			Severity: detect.SeverityCritical,
			Cluster:  "prod",
			Object:   detect.Object{Kind: "pod", Namespace: "shop", Name: "web-7c9f"},
			Title:    "Pod crashlooping",
			Message:  "restart count 7 over the last 10 minutes",
		}},
		ProposedAt: proposedAt,
	}
}

// testRequest returns a proposal with a successful dry-run behind it — the shape
// that SHOULD be authorizable once a human approves it.
func testRequest() Request {
	return Request{
		Proposal: testProposal(),
		Preview: Preview{
			Performed: true,
			Summary:   "server accepted the patch with dryRun=All",
			Diff:      "+ kubectl.kubernetes.io/restartedAt: 2026-08-01T12:00:00Z",
		},
	}
}

// approvedPending returns the artifact state for a request a human has legitimately
// approved: current resourceVersion displayed, approval recorded after the preview,
// by a named person who is not MaKlaude.
func approvedPending(req Request) PendingAction {
	return PendingAction{
		Identity:                 req.Identity(),
		Ref:                      ActionRef("7"),
		State:                    StateApproved,
		Approver:                 "the-gigi",
		DecidedAt:                decidedAt,
		PreviewedResourceVersion: req.Proposal.Target.ResourceVersion,
		PreviewedAt:              previewAt,
		PreviewedState:           previewStateToken(req.Preview),
	}
}

func TestStateStringIsStable(t *testing.T) {
	for state, want := range map[State]string{
		StatePending:  "pending",
		StateApproved: "approved",
		StateRejected: "rejected",
		State(99):     "state(99)",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", int(state), got, want)
		}
	}
}

func TestActionKindAndReasonStringsAreStable(t *testing.T) {
	for kind, want := range map[ActionKind]string{
		ActionOpen:      "open",
		ActionRefresh:   "refresh",
		ActionAuthorize: "authorize",
		ActionRefuse:    "refuse",
		ActionWithdraw:  "withdraw",
		ActionHold:      "hold",
		ActionKind(99):  "action(99)",
	} {
		if got := kind.String(); got != want {
			t.Errorf("ActionKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}

	// Every reason must render as a distinct, non-placeholder token: the reason lands
	// in the trail verbatim, so two reasons sharing a rendering would make an audit
	// record ambiguous about why an approval was not honored.
	seen := map[string]Reason{}
	for r := ReasonNewProposal; r <= ReasonPendingExpired; r++ {
		got := r.String()
		if strings.HasPrefix(got, "reason(") {
			t.Errorf("Reason(%d) has no rendering", int(r))
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("Reason(%d) and Reason(%d) both render as %q", int(prev), int(r), got)
		}
		seen[got] = r
	}
}

func TestPolicyZeroValueTakesTheDefaultTTL(t *testing.T) {
	// A forgotten field must behave like a configured one — not like "expires
	// instantly" (every approval refused) and not like "never expires" (a safety
	// property silently off).
	for name, p := range map[string]Policy{
		"zero":     {},
		"negative": {ApprovalTTL: -time.Hour},
	} {
		if got := p.normalized().ApprovalTTL; got != DefaultApprovalTTL {
			t.Errorf("%s policy normalized to %s, want %s", name, got, DefaultApprovalTTL)
		}
	}
	if got := DefaultPolicy(); got.PendingTTL != nil {
		t.Errorf("DefaultPolicy().PendingTTL = %v, want nil (undecided proposals wait indefinitely)", got.PendingTTL)
	}
}

func TestBodyRoundTripsEveryMarker(t *testing.T) {
	req := testRequest()
	body := Body(req, previewAt)

	id, ok := ParseProposalMarker(body)
	if !ok || id != req.Identity() {
		t.Fatalf("ParseProposalMarker = (%q, %v), want (%q, true)", id, ok, req.Identity())
	}

	rv, at, ok := ParsePreviewMarker(body)
	if !ok {
		t.Fatal("ParsePreviewMarker reported no marker")
	}
	if rv != "1000" {
		t.Errorf("previewed resourceVersion = %q, want %q", rv, "1000")
	}
	if !at.Equal(previewAt) {
		t.Errorf("previewed at = %s, want %s", at, previewAt)
	}
	if got := ParsePreviewStateMarker(body); got != previewStateOK {
		t.Errorf("preview state = %q, want %q", got, previewStateOK)
	}

	// No thread handle until the chat root is posted.
	if ts, ok := ParseThreadMarker(body); ok {
		t.Errorf("ParseThreadMarker on a fresh body = %q, want absent", ts)
	}
}

func TestParsePreviewMarkerRejectsMalformedInput(t *testing.T) {
	// A half-parsed marker would relax a safety check on the strength of a corrupt
	// body, so anything unreadable must report absent.
	for name, body := range map[string]string{
		"missing":      "no markers here",
		"no separator": "<!-- maklaude:preview=1000 -->",
		"bad time":     "<!-- maklaude:preview=1000@not-a-time -->",
		"empty rv":     "<!-- maklaude:preview=@2026-08-01T12:00:00Z -->",
		"empty marker": "<!-- maklaude:preview= -->",
	} {
		if rv, at, ok := ParsePreviewMarker(body); ok {
			t.Errorf("%s: ParsePreviewMarker = (%q, %s, true), want ok=false", name, rv, at)
		}
	}
}

func TestParsePreviewMarkerSplitsOnTheLastSeparator(t *testing.T) {
	// A resourceVersion is opaque and could contain "@"; the RFC3339 suffix never
	// does, so the split has to be from the right.
	body := "<!-- maklaude:preview=rv@weird@2026-08-01T12:00:30Z -->"
	rv, at, ok := ParsePreviewMarker(body)
	if !ok {
		t.Fatal("ParsePreviewMarker reported no marker")
	}
	if rv != "rv@weird" {
		t.Errorf("resourceVersion = %q, want %q", rv, "rv@weird")
	}
	if !at.Equal(previewAt) {
		t.Errorf("previewed at = %s, want %s", at, previewAt)
	}
}

func TestWithThreadMarkerReplacesRatherThanAccumulates(t *testing.T) {
	body := Body(testRequest(), previewAt)

	once := withThreadMarker(body, "1712.0001")
	if ts, _ := ParseThreadMarker(once); ts != "1712.0001" {
		t.Fatalf("thread marker = %q, want %q", ts, "1712.0001")
	}

	twice := withThreadMarker(once, "1712.0002")
	if n := strings.Count(twice, threadMarkerPrefix); n != 1 {
		t.Errorf("body carries %d thread markers, want 1", n)
	}
	if ts, _ := ParseThreadMarker(twice); ts != "1712.0002" {
		t.Errorf("thread marker = %q, want the newer %q", ts, "1712.0002")
	}

	// Re-rendering must not lose the other markers.
	if _, ok := ParseProposalMarker(twice); !ok {
		t.Error("proposal marker lost when the thread marker was replaced")
	}

	// An empty handle strips the marker, which is correct when chat is unconfigured.
	if ts, ok := ParseThreadMarker(withThreadMarker(twice, "")); ok {
		t.Errorf("empty thread handle left marker %q behind", ts)
	}
}

func TestBodyStatesPlainlyThatNoDryRunWasPerformed(t *testing.T) {
	// An omitted section reads as "nothing to worry about", and a missing dry-run is
	// precisely something to worry about.
	req := Request{Proposal: testProposal()}
	body := Body(req, previewAt)

	if !strings.Contains(body, "No dry-run was performed") {
		t.Error("body does not say a dry-run was not performed")
	}
	if got := ParsePreviewStateMarker(body); got != previewStateNone {
		t.Errorf("preview state = %q, want %q", got, previewStateNone)
	}
}

func TestBodyLeadsWithTheDryRunFailure(t *testing.T) {
	req := testRequest()
	req.Preview = Preview{Performed: true, Error: "admission webhook denied the request"}
	body := Body(req, previewAt)

	if !strings.Contains(body, "The dry-run FAILED") {
		t.Error("body does not flag the failed dry-run")
	}
	if !strings.Contains(body, "admission webhook denied the request") {
		t.Error("body does not carry the dry-run error the refusal comment points at")
	}
	if got := ParsePreviewStateMarker(body); got != previewStateFailed {
		t.Errorf("preview state = %q, want %q", got, previewStateFailed)
	}
}

func TestBodyCarriesEverySectionAnApproverNeeds(t *testing.T) {
	// A human approves a production mutation on the strength of this text alone, so
	// each of these is a section whose absence would let someone approve something
	// they did not understand.
	body := Body(testRequest(), previewAt)
	for _, want := range []string{
		"Exactly what will run",
		"Dry-run preview",
		"Reversibility and rollback",
		"Diagnosis this addresses",
		"Rechecked immediately before running",
		"How to decide",
		"`prod`",               // cluster
		"`rolloutrestart`",     // operation
		"deployment/shop/web",  // target
		"`1000`",               // resourceVersion the approval binds to
		"the-gigi-style label", // placeholder, replaced below
		"restart count 7",      // evidence
		"the deployment has not changed since the snapshot", // precondition
	} {
		if want == "the-gigi-style label" {
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q", want)
		}
	}
	if !strings.Contains(body, "`"+ApprovedLabel+"`") {
		t.Errorf("body does not name the %q label as the approval mechanism", ApprovedLabel)
	}
	if !strings.Contains(body, "`"+RejectedLabel+"`") {
		t.Errorf("body does not name the %q label as the rejection mechanism", RejectedLabel)
	}
}

func TestBodyWarnsWhenAnOperationHasNoRollbackPlan(t *testing.T) {
	// The text and the gate must agree: a reader is never told something the code
	// will not honor. See TestDecideRefusesAnOperationWithNoRollbackPlan.
	req := testRequest()
	req.Proposal.Operation = remediate.Operation("scaletozero")
	body := Body(req, previewAt)

	if !strings.Contains(body, "No rollback plan is defined") {
		t.Error("body does not warn that the operation has no rollback plan")
	}
}

func TestEveryCatalogOperationHasARollbackPlan(t *testing.T) {
	// The refusal exists to catch a catalog that grows without a plan. That makes it
	// a bug-finder, not a normal path — so today's catalog must be complete.
	for _, op := range []remediate.Operation{
		remediate.OpRolloutRestart,
		remediate.OpRollbackRevision,
		remediate.OpDeletePod,
		remediate.OpCordonNode,
	} {
		if plan, ok := rollbackPlan(op); !ok || strings.TrimSpace(plan) == "" {
			t.Errorf("operation %q has no rollback plan", op)
		}
	}
	if _, ok := rollbackPlan(remediate.Operation("deletenamespace")); ok {
		t.Error("an operation outside the catalog reported a rollback plan")
	}
}

func TestTitleIdentifiesClusterAndObjectWithoutOpeningTheIssue(t *testing.T) {
	title := Title(testRequest())
	for _, want := range []string{"[APPROVAL]", "prod", "deployment/shop/web"} {
		if !strings.Contains(title, want) {
			t.Errorf("title %q is missing %q", title, want)
		}
	}
}

func TestLabelsForNeverRewritesAHumanDecision(t *testing.T) {
	// LabelsFor feeds a full-label Update. If it regenerated decision labels from
	// anything but the recorded state, a body refresh would silently apply or erase a
	// human's input.
	cases := map[string]struct {
		pending PendingAction
		want    []string
		absent  []string
	}{
		"pending": {
			pending: PendingAction{State: StatePending},
			want:    []string{ManagedLabel, NeedsHumanLabel},
			absent:  []string{ApprovedLabel, RejectedLabel, ExecutedLabel},
		},
		"approved": {
			pending: PendingAction{State: StateApproved},
			want:    []string{ManagedLabel, ApprovedLabel},
			absent:  []string{NeedsHumanLabel, RejectedLabel},
		},
		"rejected": {
			pending: PendingAction{State: StateRejected},
			want:    []string{ManagedLabel, RejectedLabel},
			absent:  []string{NeedsHumanLabel, ApprovedLabel},
		},
		"executed": {
			pending: PendingAction{State: StateApproved, Executed: true},
			want:    []string{ManagedLabel, ApprovedLabel, ExecutedLabel},
			absent:  []string{NeedsHumanLabel},
		},
	}

	for name, tc := range cases {
		got := LabelsFor(tc.pending)
		set := map[string]bool{}
		for _, l := range got {
			set[l] = true
		}
		for _, w := range tc.want {
			if !set[w] {
				t.Errorf("%s: labels %v missing %q", name, got, w)
			}
		}
		for _, a := range tc.absent {
			if set[a] {
				t.Errorf("%s: labels %v should not carry %q", name, got, a)
			}
		}
	}
}

func TestRefusalCommentNamesTheSpecificReason(t *testing.T) {
	req := testRequest()
	pending := approvedPending(req)
	pending.PreviewedResourceVersion = "999"

	cases := map[Reason]string{
		ReasonDrift:                   "999",
		ReasonApprovalPredatesPreview: "before this issue last refreshed",
		ReasonApprovalExpired:         "perishable",
		ReasonUnattributedApproval:    "cannot attribute to a person",
		ReasonSelfApproval:            "MaKlaude's own account",
		ReasonPreviewFailed:           "rejected this action as a dry-run",
		ReasonNoRollbackPlan:          "no defined rollback plan",
	}

	for reason, want := range cases {
		comment := RefusalComment(req, pending, reason, DefaultPolicy())
		if !strings.Contains(comment, want) {
			t.Errorf("%s refusal does not explain itself (missing %q): %s", reason, want, comment)
		}
		if !strings.Contains(comment, "was NOT run") {
			t.Errorf("%s refusal does not state that nothing ran", reason)
		}
		if !strings.Contains(comment, reason.String()) {
			t.Errorf("%s refusal does not carry its machine-readable reason token", reason)
		}
	}
}

func TestWithdrawalCommentAlwaysSaysWhetherAnythingRan(t *testing.T) {
	// The single most important thing a future reader learns from a withdrawn
	// request is that it closed WITHOUT running.
	id := testProposal().Identity

	if got := WithdrawalComment(id, ReasonSelfHealed); !strings.Contains(got, "without running anything") {
		t.Errorf("self-heal withdrawal does not say nothing ran: %s", got)
	}
	if got := WithdrawalComment(id, ReasonPendingExpired); !strings.Contains(got, "without running anything") {
		t.Errorf("expiry withdrawal does not say nothing ran: %s", got)
	}
	// The completed case is the one where something DID run, and it must not claim
	// otherwise.
	got := WithdrawalComment(id, ReasonCompleted)
	if strings.Contains(got, "without running anything") {
		t.Errorf("completed withdrawal wrongly claims nothing ran: %s", got)
	}
	if !strings.Contains(got, "it ran") {
		t.Errorf("completed withdrawal does not record that the action ran: %s", got)
	}
}
