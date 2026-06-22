package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/Sayfan-AI/MaKlaude/internal/detect"
)

// TestSlackConfig_MentionPrefix is the table-driven test for rendering the operator
// @-mention used to make a needs:human escalation mobile-push eligible.
func TestSlackConfig_MentionPrefix(t *testing.T) {
	cases := []struct {
		name     string
		operator string
		want     string
	}{
		{"empty operator omits mention", "", ""},
		{"user id becomes user mention", "U0123456789", "<@U0123456789>"},
		{"workspace user id becomes user mention", "W0123456789", "<@W0123456789>"},
		{"usergroup id becomes subteam mention", "S0123456789", "<!subteam^S0123456789>"},
		{"explicit mention token passes through", "<@U999>", "<@U999>"},
		{"explicit subteam token passes through", "<!subteam^S999>", "<!subteam^S999>"},
		{"bare @name wrapped best-effort", "@oncall", "<@oncall>"},
		{"whitespace-only operator omits mention", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := SlackConfig{Operator: c.operator}
			if got := cfg.mentionPrefix(); got != c.want {
				t.Errorf("mentionPrefix() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestEscalationText_MentionOnNeedsHuman proves the rendered escalation root carries
// the operator @-mention only when a mention is supplied (the needs:human case),
// and never otherwise.
func TestEscalationText_MentionOnNeedsHuman(t *testing.T) {
	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	withMention := escalationText(id, "Pod crashlooping", "42", "<@U42>")
	if !strings.Contains(withMention, "<@U42>") {
		t.Errorf("needs:human escalation should @-mention the operator: %q", withMention)
	}
	if !strings.Contains(withMention, "needs:human") {
		t.Errorf("needs:human escalation should label itself: %q", withMention)
	}

	noMention := escalationText(id, "Pod crashlooping", "42", "")
	if strings.Contains(noMention, "<@") || strings.Contains(noMention, "needs:human") {
		t.Errorf("non-needs:human escalation must not @-mention: %q", noMention)
	}
	// Both still carry the summary and backing issue.
	for _, txt := range []string{withMention, noMention} {
		if !strings.Contains(txt, "Pod crashlooping") || !strings.Contains(txt, "#42") {
			t.Errorf("escalation text lost summary/ref: %q", txt)
		}
	}
}

// TestSlackNotifier_NeedsHumanMentionsOperator is the mobile-push done-criterion at
// the notifier level: a needs:human escalation posts the configured operator's
// @-mention into the root; a non-needs:human one does not. Exercised with the fake
// transport (zero network).
func TestSlackNotifier_NeedsHumanMentionsOperator(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Operator = "U0OPERATOR"

	fake := &fakeSlack{tsQueue: []string{"100.1", "100.2"}}
	sn, ok := NewSlackNotifier(cfg, fake)
	if !ok {
		t.Fatal("configured notifier expected")
	}
	id := detect.Identity("prod|pod.crashloop|pod/team/api")

	// needs:human => mention present.
	if _, err := sn.NotifyEscalation(ctx, id, "Pod crashlooping", "42", true); err != nil {
		t.Fatalf("NotifyEscalation(needsHuman): %v", err)
	}
	rootText, _ := fake.requests[0].body["text"].(string)
	if !strings.Contains(rootText, "<@U0OPERATOR>") {
		t.Errorf("needs:human root should @-mention the operator: %q", rootText)
	}

	// non-needs:human => no mention.
	other := detect.Identity("prod|event.warning|event/team/x")
	if _, err := sn.NotifyEscalation(ctx, other, "Noteworthy event", "43", false); err != nil {
		t.Fatalf("NotifyEscalation(info): %v", err)
	}
	infoText, _ := fake.requests[1].body["text"].(string)
	if strings.Contains(infoText, "<@U0OPERATOR>") {
		t.Errorf("info escalation must not @-mention: %q", infoText)
	}
}

// TestSlackNotifier_NoOperatorNoMention proves that with no operator configured a
// needs:human escalation simply posts without a mention (no behavior change),
// degrading gracefully.
func TestSlackNotifier_NoOperatorNoMention(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSlack{tsQueue: []string{"100.1"}}
	sn, _ := NewSlackNotifier(testConfig(), fake) // no Operator set

	id := detect.Identity("prod|pod.crashloop|pod/team/api")
	if _, err := sn.NotifyEscalation(ctx, id, "Pod crashlooping", "42", true); err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	rootText, _ := fake.requests[0].body["text"].(string)
	if strings.Contains(rootText, "<@") {
		t.Errorf("no operator configured: root must carry no mention: %q", rootText)
	}
}
