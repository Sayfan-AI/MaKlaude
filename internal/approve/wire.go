package approve

import (
	"fmt"
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// SinkFromEnv selects the approval sink for the running process from the
// MAKLAUDE_GITHUB_* environment (see [escalate.GitHubConfig], [SelfLoginEnv], and
// [AutoApproveEnv]).
//
//   - Configured: a live [GitHubSink] and live=true.
//   - Not configured: a [MemorySink] and live=false, so a credential-less
//     deployment degrades to a side-effect-free dry run.
//
// The unconfigured case is more than a convenience here. The gate's output is
// permission to mutate a cluster, and a process with no comms credentials cannot ask
// a human anything — so the correct degraded behavior is one where no artifact is
// ever created, therefore no artifact is ever labelled, therefore no
// [Authorization] is ever issued. Falling back to a memory sink produces exactly
// that: the pipeline runs end to end and authorizes nothing. Returning a nil sink
// would instead force every caller to nil-check, and a missed check would panic in
// the one code path that must never fail open.
//
// # It can now refuse to build one at all
//
// Two misconfigurations are errors rather than defaults, and both are errors because
// the alternative is silent: an unreadable [AutoApproveEnv] value (see
// [ErrAmbiguousAutoApprove]), and a live trail with no self-identity while the approval
// requirement is still in force (see [ErrSelfIdentityUnknown] and [GateConfig.Check]).
//
// The second is the one worth the signature change. Before it, a MaKlaude that could
// not recognize its own label events did not behave like a broken gate — it behaved
// like a working one that happened to approve everything MaKlaude asked for, and
// nothing anywhere said otherwise. An error at startup is the only place that failure
// can be made visible, because by the time it matters the artifact already reads like
// a human decision.
func SinkFromEnv() (sink ApprovalSink, live bool, err error) {
	gate, err := GateConfigFromEnv(os.Getenv)
	if err != nil {
		return nil, false, err
	}

	cfg := escalate.GitHubConfigFromEnv(os.Getenv)
	if cfg.Configured() {
		if cerr := gate.Check(); cerr != nil {
			return nil, false, fmt.Errorf("refusing to build a live approval gate: %w", cerr)
		}
		if gh, ok := NewGitHubSink(cfg, gate.SelfLogin); ok {
			return gh, true, nil
		}
	}

	// The memory sink's own login is set too, so the self-approval refusal behaves
	// the same in the degraded path as in the live one. The identity is not REQUIRED
	// here, for the reason given in [GateConfig.Check]: nothing outside this process
	// can label an in-memory artifact, so there is no labeler to mistake for a human.
	ms := NewMemorySink()
	ms.SelfLogin = gate.SelfLogin
	return ms, false, nil
}

// GatekeeperFromEnv returns a ready [Gatekeeper] plus whether it is backed by a live
// GitHub trail. A monitor calls it once at startup and reuses the gatekeeper across
// reconciliation cycles, exactly as it does with [escalate.EscalatorFromEnv].
//
// Chat is wired the same way the escalation trail wires it, and for the same reason:
// this is the layer that can see both the artifact store (to persist and recover the
// thread handle) and the notifier. The returned live reflects the GITHUB trail only —
// the auditable source of truth and the only place a decision can be recorded. An
// unconfigured Slack backend yields a [notify.NopNotifier] and costs nothing but the
// mirror, because a chat message is never the approval signal.
//
// policy is normalized by [NewGatekeeper], so a zero value takes the shipped
// defaults. [AutoApproveEnv] can only turn [Policy.AutoApprove] ON, never off: a
// caller that hard-coded it has already made the decision in code, and an environment
// variable quietly re-arming a gate that a program deliberately disabled would be a
// surprise in the harmless direction, while the reverse would be a surprise in the
// other one. Both spellings are deliberate acts; neither overrides the other toward
// less safety.
//
// An error means no gatekeeper was built. See [SinkFromEnv] for the two
// misconfigurations that produce one and why each is fatal rather than defaulted.
func GatekeeperFromEnv(policy Policy) (gk *Gatekeeper, live bool, err error) {
	auto, err := AutoApproveFromEnv(os.Getenv)
	if err != nil {
		return nil, false, err
	}
	if auto {
		policy.AutoApprove = true
	}

	sink, live, err := SinkFromEnv()
	if err != nil {
		return nil, false, err
	}
	issueBaseURL := escalate.GitHubConfigFromEnv(os.Getenv).IssueBaseURL()
	notifier, _ := notify.NotifierFromEnvWithIssueBaseURL(issueBaseURL)
	return NewGatekeeper(sink, notifier, policy), live, nil
}
