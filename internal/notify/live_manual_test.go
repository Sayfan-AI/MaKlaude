package notify

import (
	"context"
	"os"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// TestLiveSlackManual drives the real SlackNotifier against a live workspace to
// prove threading + the needs:human @-mention end-to-end. It is skipped unless
// MAKLAUDE_SLACK_LIVE=1, so it never runs in CI or a normal `task test`.
//
//	set -a; source .env.slack.local; set +a
//	MAKLAUDE_SLACK_LIVE=1 go test -run TestLiveSlackManual ./internal/notify -v -count=1
func TestLiveSlackManual(t *testing.T) {
	if os.Getenv("MAKLAUDE_SLACK_LIVE") != "1" {
		t.Skip("set MAKLAUDE_SLACK_LIVE=1 to run the live Slack test")
	}

	cfg := SlackConfigFromEnv(os.Getenv)
	if !cfg.Configured() {
		t.Fatalf("slack not configured: %s", cfg)
	}
	t.Logf("config: %s", cfg) // String() redacts secrets

	sn, ok := NewSlackNotifier(cfg, nil)
	if !ok {
		t.Fatal("NewSlackNotifier returned ok=false for a configured env")
	}

	ctx := context.Background()
	id := detect.Identity("live-test/maklaude/demo-crashloop")

	ts, err := sn.NotifyEscalation(ctx, id,
		"demo: pod web-7c9 CrashLoopBackOff in ns demo (4 restarts)",
		"https://github.com/Sayfan-AI/MaKlaude/issues/50", true /* needsHuman → @-mention */)
	if err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	if ts == "" {
		t.Fatal("NotifyEscalation returned empty thread_ts")
	}
	t.Logf("posted escalation root, thread_ts=%s", ts)

	if err := sn.NotifyUpdate(ctx, id, ts, "still crashlooping — restart count now 7"); err != nil {
		t.Fatalf("NotifyUpdate: %v", err)
	}
	if err := sn.NotifyResolution(ctx, id, ts, "pod recovered — 0 restarts over the last 5m, resolved"); err != nil {
		t.Fatalf("NotifyResolution: %v", err)
	}
	t.Logf("update + resolution replied into thread %s", ts)
}
