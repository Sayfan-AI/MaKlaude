package aidiagnose

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sayfan-AI/MaKlaude/internal/correlate"
	"github.com/Sayfan-AI/MaKlaude/internal/detect"
	"github.com/Sayfan-AI/MaKlaude/internal/diagnose"
	"github.com/Sayfan-AI/MaKlaude/internal/health"
)

// fixedTime pins the collection time so fixtures are deterministic.
var fixedTime = time.Date(2026, time.July, 5, 12, 0, 0, 0, time.UTC)

// fakeProvider is the test [Provider]: it captures the exact request it was handed
// (so a test can assert on what would egress) and returns a canned response or
// error, optionally after a delay to exercise timeouts. It is safe for concurrent
// use.
type fakeProvider struct {
	resp  Response
	err   error
	delay time.Duration

	mu      sync.Mutex
	calls   int
	lastReq Request
}

func (f *fakeProvider) Suggest(ctx context.Context, req Request) (Response, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
	return f.resp, f.err
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) captured() Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

// captureAuditor records every [AuditRecord] for assertions.
type captureAuditor struct {
	mu      sync.Mutex
	records []AuditRecord
}

func (c *captureAuditor) Record(_ context.Context, rec AuditRecord) {
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
}

func (c *captureAuditor) all() []AuditRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]AuditRecord, len(c.records))
	copy(out, c.records)
	return out
}

// testConfig returns an active config with generous bounds so a test opts into
// tighter ones explicitly.
func testConfig() Config {
	return Config{Enabled: true, APIKey: "test-key"}
}

// badImageFixture builds a reachable snapshot with a single bad-image cascade
// (deployment → replicaset → pod in ImagePullBackOff), running it through the real
// detect→correlate→diagnose pipeline so tests operate on genuine incidents and
// deterministic hypotheses. waitingMsg is placed on the pod's container waiting
// message, letting a test seed secrets into a field the evidence builder always
// includes. eventMsg, when non-empty, is attached as a warning event on the pod.
func badImageFixture(t *testing.T, waitingMsg, eventMsg string) (health.Snapshot, correlate.Incident, []diagnose.Hypothesis) {
	t.Helper()
	snap := health.Snapshot{
		Cluster:      "prod",
		CollectedAt:  fixedTime,
		Reachability: health.Reachability{Reachable: true, ServerVersion: "v1.30.0"},
		Deployments: []health.DeploymentSignal{
			{Namespace: "shop", Name: "api", DesiredReplicas: 3, AvailableReplicas: 0},
		},
		ReplicaSets: []health.ReplicaSetSignal{
			{Namespace: "shop", Name: "api-6f7d", DesiredReplicas: 3, AvailableReplicas: 0},
		},
		Pods: []health.PodSignal{
			{
				Namespace: "shop", Name: "api-6f7d-aaaa", Phase: "Pending", Pending: true,
				Owners: []health.OwnerRef{{Kind: "ReplicaSet", Name: "api-6f7d", Controller: true}},
				Containers: []health.ContainerSignal{
					{Name: "api", WaitingReason: "ImagePullBackOff", WaitingMessage: waitingMsg},
				},
			},
		},
	}
	if eventMsg != "" {
		snap.WarningEvents = []health.EventSignal{
			{
				Namespace: "shop", Name: "api-6f7d-aaaa.abc", Reason: "Failed",
				Message: eventMsg, Count: 3, InvolvedObject: "Pod/shop/api-6f7d-aaaa", LastSeen: fixedTime,
			},
		}
	}

	incidents := correlate.Correlate(snap, detect.Analyze(snap))
	if len(incidents) == 0 {
		t.Fatalf("fixture produced no incidents")
	}
	inc := incidents[0]
	base := diagnose.Diagnose(snap, inc)
	if len(base) == 0 {
		t.Fatalf("fixture produced no base hypotheses")
	}
	return snap, inc, base
}

// TestRefine_RedactsSecretsBeforeEgress is the crux safety test: seed known
// secrets into evidence the builder includes, then assert none of them appear in
// the payload actually handed to the provider, while the redaction placeholder and
// non-secret context survive.
func TestRefine_RedactsSecretsBeforeEgress(t *testing.T) {
	const (
		secretPassword = "hunter2SuperSecret"
		secretToken    = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		secretEmail    = "alice@example.com"
		secretAWSKey   = "AKIAIOSFODNN7EXAMPLE"
		secretBearer   = "sk-ant-abcdef0123456789ABCDEF"
	)
	waitingMsg := "Back-off pulling image; password=" + secretPassword + " token=" + secretToken
	eventMsg := "auth failed for " + secretEmail + " using key " + secretAWSKey + " and " + secretBearer

	snap, inc, base := badImageFixture(t, waitingMsg, eventMsg)

	provider := &fakeProvider{}
	r := NewRefinerForTest(context.Background(), testConfig(), provider, nil)
	r.Refine(snap, inc, base)

	if provider.callCount() != 1 {
		t.Fatalf("provider called %d times, want 1", provider.callCount())
	}
	payload := provider.captured().System + "\n" + provider.captured().Evidence

	for _, secret := range []string{secretPassword, secretToken, secretEmail, secretAWSKey, secretBearer} {
		if strings.Contains(payload, secret) {
			t.Errorf("payload leaked secret %q:\n%s", secret, payload)
		}
	}
	if !strings.Contains(payload, redactionPlaceholder) {
		t.Errorf("payload contains no redaction placeholder, so redaction may not have run:\n%s", payload)
	}
	// Non-secret context must survive so the model still gets a useful prompt.
	if !strings.Contains(payload, "ImagePullBackOff") {
		t.Errorf("payload dropped non-secret context (ImagePullBackOff):\n%s", payload)
	}
}

// TestRedact_DirectSeededSecrets checks the redactor in isolation against a spread
// of secret shapes, independent of any evidence assembly.
func TestRedact_DirectSeededSecrets(t *testing.T) {
	cases := map[string]string{
		"password=swordfish":                             "swordfish",
		"api_key: 9f8e7d6c5b4a3928abcdef0123456789":      "9f8e7d6c5b4a3928abcdef0123456789",
		"Authorization: Bearer abc.def.ghijklmnop":       "abc.def.ghijklmnop",
		"contact ops@corp.io now":                        "ops@corp.io",
		"key AKIAIOSFODNN7EXAMPLE leaked":                "AKIAIOSFODNN7EXAMPLE",
		"token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789": "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
	}
	for input, secret := range cases {
		got := Redact(input)
		if strings.Contains(got, secret) {
			t.Errorf("Redact(%q) = %q, still contains secret %q", input, got, secret)
		}
		if !strings.Contains(got, redactionPlaceholder) {
			t.Errorf("Redact(%q) = %q, expected a redaction placeholder", input, got)
		}
	}
}

// TestRedact_KeepsOrdinaryText proves redaction does not shred normal diagnostic
// prose (a safety layer that destroyed all context would be useless).
func TestRedact_KeepsOrdinaryText(t *testing.T) {
	in := "pod api-6f7d-aaaa in namespace shop is ImagePullBackOff on node worker-2"
	if got := Redact(in); got != in {
		t.Errorf("Redact mangled ordinary text:\n in: %q\nout: %q", in, got)
	}
}

// TestRefine_NilProviderReturnsBase proves the unconfigured/absent path degrades
// to the deterministic hypotheses with no change and no panic.
func TestRefine_NilProviderReturnsBase(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	r := NewRefinerForTest(context.Background(), testConfig(), nil, nil)
	got := r.Refine(snap, inc, base)
	assertSameHypotheses(t, base, got)
}

// TestRefine_ProviderErrorDegrades proves a provider error falls back to the base
// and is audited as an error.
func TestRefine_ProviderErrorDegrades(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	provider := &fakeProvider{err: context.DeadlineExceeded}
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), testConfig(), provider, auditor)

	got := r.Refine(snap, inc, base)
	assertSameHypotheses(t, base, got)

	recs := auditor.all()
	if len(recs) != 1 || recs[0].Outcome != OutcomeError {
		t.Fatalf("audit = %+v, want one error record", recs)
	}
	if recs[0].Purpose != refinePurpose {
		t.Errorf("audit purpose = %q, want %q", recs[0].Purpose, refinePurpose)
	}
}

// TestRefine_TimeoutDegrades proves a provider slower than the configured timeout
// degrades to the base rather than blocking or failing the scan.
func TestRefine_TimeoutDegrades(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	cfg := testConfig()
	cfg.Timeout = 5 * time.Millisecond
	provider := &fakeProvider{delay: 200 * time.Millisecond, resp: Response{
		Suggestions: []Suggestion{{Cause: "x", Title: "should never apply"}},
	}}
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), cfg, provider, auditor)

	start := time.Now()
	got := r.Refine(snap, inc, base)
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Errorf("Refine blocked %v, expected to abandon near the %v timeout", elapsed, cfg.Timeout)
	}
	assertSameHypotheses(t, base, got)
	if recs := auditor.all(); len(recs) != 1 || recs[0].Outcome != OutcomeError {
		t.Fatalf("audit = %+v, want one error record", recs)
	}
}

// TestRefine_CallBudgetEnforced proves the per-cycle budget caps provider calls
// and that over-budget incidents degrade to the base and are audited.
func TestRefine_CallBudgetEnforced(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	cfg := testConfig()
	cfg.CallBudget = 1
	provider := &fakeProvider{} // empty response: no change, but a real call
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), cfg, provider, auditor)

	r.Refine(snap, inc, base) // consumes the single call
	r.Refine(snap, inc, base) // over budget
	r.Refine(snap, inc, base) // over budget

	if provider.callCount() != 1 {
		t.Fatalf("provider called %d times, want 1 (budget)", provider.callCount())
	}
	recs := auditor.all()
	if len(recs) != 3 {
		t.Fatalf("audit records = %d, want 3", len(recs))
	}
	skipped := 0
	for _, rec := range recs {
		if rec.Outcome == OutcomeSkippedBudget {
			skipped++
		}
	}
	if skipped != 2 {
		t.Errorf("skipped-budget records = %d, want 2", skipped)
	}
}

// TestRefine_EvidenceSizeCapped proves the evidence handed to the provider honours
// the configured byte cap.
func TestRefine_EvidenceSizeCapped(t *testing.T) {
	longMsg := strings.Repeat("padding ", 5000) // ~40KB of harmless text
	snap, inc, base := badImageFixture(t, longMsg, "")
	cfg := testConfig()
	cfg.MaxEvidenceBytes = 500
	provider := &fakeProvider{}
	r := NewRefinerForTest(context.Background(), cfg, provider, nil)

	r.Refine(snap, inc, base)
	got := provider.captured()
	if runes := len([]rune(got.Evidence)); runes > 500+len([]rune(" …[truncated]")) {
		t.Errorf("evidence has %d runes, exceeds cap of 500 (+marker)", runes)
	}
	if got.MaxTokens != cfg.maxResponseTokens() {
		t.Errorf("request MaxTokens = %d, want %d", got.MaxTokens, cfg.maxResponseTokens())
	}
}

// TestRefine_AppliesSuggestions proves a successful call rewrites a matching
// deterministic hypothesis and adds a novel one, stamping both SourceRefined and
// preserving the deterministic hypothesis's identity on the rewrite.
func TestRefine_AppliesSuggestions(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	badImage := findByCause(t, base, diagnose.CauseBadImage)

	provider := &fakeProvider{resp: Response{Suggestions: []Suggestion{
		{Cause: "badimage", Title: "Refined bad image", Message: "sharper", Confidence: "high"},
		{Cause: "registry-auth", Title: "Registry auth failure", Message: "novel", Confidence: "medium"},
	}}}
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), testConfig(), provider, auditor)

	got := r.Refine(snap, inc, base)

	rewritten := findByCause(t, got, diagnose.CauseBadImage)
	if rewritten.Source != diagnose.SourceRefined {
		t.Errorf("rewritten hypothesis source = %q, want refined", rewritten.Source)
	}
	if rewritten.Title != "Refined bad image" {
		t.Errorf("rewritten title = %q, want refined title", rewritten.Title)
	}
	if rewritten.Identity != badImage.Identity {
		t.Errorf("rewrite changed identity: got %q want %q", rewritten.Identity, badImage.Identity)
	}
	added := findByCause(t, got, diagnose.Cause("registryauth"))
	if added.Source != diagnose.SourceRefined {
		t.Errorf("added hypothesis source = %q, want refined", added.Source)
	}
	if added.Cluster != inc.Cluster || !added.DetectedAt.Equal(inc.DetectedAt) {
		t.Errorf("added hypothesis did not inherit incident cluster/time")
	}
	if recs := auditor.all(); len(recs) != 1 || recs[0].Outcome != OutcomeRefined {
		t.Fatalf("audit = %+v, want one refined record", recs)
	}
}

// TestRefine_DoesNotMutateInput proves the refiner never mutates the base slice or
// its elements — the deterministic result the caller keeps must be untouched.
func TestRefine_DoesNotMutateInput(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	// Snapshot the base for later comparison.
	before := make([]diagnose.Hypothesis, len(base))
	copy(before, base)

	provider := &fakeProvider{resp: Response{Suggestions: []Suggestion{
		{Cause: "badimage", Title: "Rewritten", Message: "m", Confidence: "high"},
	}}}
	r := NewRefinerForTest(context.Background(), testConfig(), provider, nil)
	_ = r.Refine(snap, inc, base)

	assertSameHypotheses(t, before, base)
}

// TestRefine_InvalidSuggestionsDropped proves malformed suggestions (empty title
// or cause) are ignored, degrading to no change rather than corrupt output.
func TestRefine_InvalidSuggestionsDropped(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	provider := &fakeProvider{resp: Response{Suggestions: []Suggestion{
		{Cause: "", Title: "no cause"},
		{Cause: "somecause", Title: ""},
		{Cause: "   ", Title: "   "},
	}}}
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), testConfig(), provider, auditor)

	got := r.Refine(snap, inc, base)
	assertSameHypotheses(t, base, got)
	if recs := auditor.all(); len(recs) != 1 || recs[0].Outcome != OutcomeNoChange {
		t.Fatalf("audit = %+v, want one no-change record", recs)
	}
}

// panicProvider panics on every call, exercising the refiner's panic-recovery
// guarantee.
type panicProvider struct{}

func (panicProvider) Suggest(context.Context, Request) (Response, error) {
	panic("provider blew up")
}

// TestRefine_RecoversFromProviderPanic proves a panicking provider can never take
// down a scan: the refiner recovers, returns the deterministic base, and audits
// the failure.
func TestRefine_RecoversFromProviderPanic(t *testing.T) {
	snap, inc, base := badImageFixture(t, "Back-off pulling image", "")
	auditor := &captureAuditor{}
	r := NewRefinerForTest(context.Background(), testConfig(), panicProvider{}, auditor)

	got := r.Refine(snap, inc, base) // must not panic
	assertSameHypotheses(t, base, got)

	recs := auditor.all()
	if len(recs) != 1 || recs[0].Outcome != OutcomeError {
		t.Fatalf("audit = %+v, want one error record", recs)
	}
	if !strings.Contains(recs[0].Detail, "panic") {
		t.Errorf("audit detail = %q, want it to mention the recovered panic", recs[0].Detail)
	}
}

// TestRefinerFromEnv_ActiveBuildsRefiner proves the env path mints a working
// per-cycle refiner when the feature is explicitly enabled with a key.
func TestRefinerFromEnv_ActiveBuildsRefiner(t *testing.T) {
	t.Setenv("MAKLAUDE_LLM_DIAGNOSIS", "true")
	t.Setenv("MAKLAUDE_LLM_API_KEY", "sk-ant-test")

	build, live := RefinerFromEnv()
	if !live || build == nil {
		t.Fatalf("expected an active layer: live=%v build!=nil=%v", live, build != nil)
	}
	if r := build(context.Background()); r == nil {
		t.Fatal("builder returned a nil refiner")
	}
}

// findByCause returns the single hypothesis with the given cause, failing if there
// is not exactly one.
func findByCause(t *testing.T, hyps []diagnose.Hypothesis, cause diagnose.Cause) diagnose.Hypothesis {
	t.Helper()
	var found []diagnose.Hypothesis
	for _, h := range hyps {
		if h.Cause == cause {
			found = append(found, h)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one hypothesis with cause %q, got %d", cause, len(found))
	}
	return found[0]
}

// assertSameHypotheses fails if the two slices differ in length or in the fields
// that matter for equality here.
func assertSameHypotheses(t *testing.T, want, got []diagnose.Hypothesis) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("hypothesis count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if want[i].Identity != got[i].Identity || want[i].Cause != got[i].Cause ||
			want[i].Confidence != got[i].Confidence || want[i].Title != got[i].Title ||
			want[i].Message != got[i].Message || want[i].Source != got[i].Source {
			t.Fatalf("hypothesis %d differs:\n want %+v\n  got %+v", i, want[i], got[i])
		}
	}
}
