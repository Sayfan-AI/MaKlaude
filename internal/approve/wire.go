package approve

import (
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// SinkFromEnv selects the approval sink for the running process from the
// MAKLAUDE_GITHUB_* environment (see [escalate.GitHubConfig] and [SelfLoginEnv]).
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
func SinkFromEnv() (sink ApprovalSink, live bool) {
	cfg := escalate.GitHubConfigFromEnv(os.Getenv)
	if gh, ok := NewGitHubSink(cfg, SelfLoginFromEnv(os.Getenv)); ok {
		return gh, true
	}
	// The memory sink's own login is set too, so the self-approval refusal behaves
	// the same in the degraded path as in the live one.
	ms := NewMemorySink()
	ms.SelfLogin = SelfLoginFromEnv(os.Getenv)
	return ms, false
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
// defaults.
func GatekeeperFromEnv(policy Policy) (gk *Gatekeeper, live bool) {
	sink, live := SinkFromEnv()
	issueBaseURL := escalate.GitHubConfigFromEnv(os.Getenv).IssueBaseURL()
	notifier, _ := notify.NotifierFromEnvWithIssueBaseURL(issueBaseURL)
	return NewGatekeeper(sink, notifier, policy), live
}
