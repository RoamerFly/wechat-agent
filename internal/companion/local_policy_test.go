package companion

import (
	"strings"
	"testing"
)

func TestLocalConversationPolicyKeepsSourcesWithoutApplicationProtocols(t *testing.T) {
	c := Defaults()
	e := &Engine{Config: c, system: "与本机主人交流"}
	local := e.policy(Input{Channel: LocalChannel})
	connected := e.policy(Input{Channel: "wechat"})
	for _, needed := range []string{c.Persona, c.Style, c.Relationship, "conversation_context", "来源", "版本", `"messages"`} {
		if !strings.Contains(local, needed) {
			t.Errorf("missing local policy component %q", needed)
		}
	}
	for _, unavailable := range []string{"wechat_recall", "media_ids", "wake_after_seconds", "reply_contexts"} {
		if strings.Contains(local, unavailable) {
			t.Errorf("unconnected protocol entered local policy: %s", unavailable)
		}
	}
	if EstimateTokens(local)*100 >= EstimateTokens(connected)*60 {
		t.Fatalf("local policy did not remove unused application protocols: local=%d connected=%d", EstimateTokens(local), EstimateTokens(connected))
	}
	t.Logf("policy conservative estimate: local=%d connected=%d", EstimateTokens(local), EstimateTokens(connected))
}
