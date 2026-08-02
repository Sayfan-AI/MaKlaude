package disclose

import (
	"os"

	"github.com/Sayfan-AI/MaKlaude/internal/escalate"
	"github.com/Sayfan-AI/MaKlaude/internal/notify"
)

// SinkFromEnv builds the disclosure sink from the environment, mirroring
// [escalate.SinkFromEnv]: a real GitHub sink when the MAKLAUDE_GITHUB_* variables are
// present, an in-memory one otherwise.
//
// live reports which. It is returned rather than logged because "the disclosure reached
// nobody" is a materially different posture from "the disclosure is waiting to be read",
// and the state summary states it: an unattended action whose only record died with the
// process is the exact thing this trail exists to prevent, so a deployment that has
// enabled autonomy without a live trail needs to be told, not left to infer it.
func SinkFromEnv() (sink Sink, live bool) {
	if gh, ok := NewGitHubSink(escalate.GitHubConfigFromEnv(os.Getenv)); ok {
		return gh, true
	}
	return NewMemorySink(), false
}

// TrailFromEnv builds the production disclosure trail: the environment's sink and the
// environment's chat notifier.
//
// It never fails. Both halves degrade to their in-memory or no-op forms when
// unconfigured, and live reports whether the artifact half reaches anybody. Chat is
// deliberately not part of live: it is a second copy of a record whose durable home is
// the artifact, so a deployment with issues and no Slack is fully disclosed, while one
// with Slack and no issues is not.
func TrailFromEnv() (trail *Trail, live bool) {
	sink, live := SinkFromEnv()
	notifier, _ := notify.NotifierFromEnvWithIssueBaseURL(escalate.GitHubConfigFromEnv(os.Getenv).IssueBaseURL())
	t, err := NewTrail(sink, notifier)
	if err != nil {
		// Unreachable: SinkFromEnv never returns nil. Kept as a total function rather than
		// a panic, because a construction error here would take down a cycle that has not
		// yet been asked to do anything unattended.
		return nil, false
	}
	return t, live
}
