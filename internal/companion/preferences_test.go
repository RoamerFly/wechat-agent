package companion

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPreferencePolicyReachesSocialAndDirectWithoutExtraCalls(t *testing.T) {
	for _, mode := range []string{"social", "direct"} {
		t.Run(mode, func(t *testing.T) {
			s, c, in := memoryFixture(t)
			c.Enabled = true
			c.QuietStart, c.QuietEnd = "00:00", "00:00"
			in.Mode = mode
			in.IsGroup = true
			in.Text = "synthetic shared content"
			in = syntheticNativeInput(in, "preference-fixture")
			c.Permissions.ObserveGroups = true
			c.Permissions.AutonomousGroups = true
			if err := s.Observe(in, c, time.Now()); err != nil {
				t.Fatal(err)
			}
			pref := "收到分享默认总结并发送；昨天已经总结过的不要重复。"
			if err := s.SetPreferences(ScopeID(in.Channel, in.Account, in.Thread), pref, 0); err != nil {
				t.Fatal(err)
			}
			m := &fakeModel{answer: `{"action":"silent","messages":[]}`}
			e := &Engine{Config: c, Store: s, model: m}
			if _, err := e.Respond(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if m.calls != 1 || !strings.Contains(m.system, conversationPreferencePolicy) {
				t.Fatal("preference priority absent or added model call", m.calls)
			}
			found := false
			for _, turn := range m.turns {
				var p map[string]any
				if json.Unmarshal([]byte(turn["content"]), &p) == nil && p["kind"] == "current_message" {
					found = p["conversation_preferences"] == pref
					if p["must_reply"] == true {
						t.Fatal("preference fabricated mandatory reply")
					}
				}
			}
			if !found {
				t.Fatal("saved preference missing from model input")
			}
		})
	}
}

func TestConversationPreferencesVersionScopeAndInvalidation(t *testing.T) {
	s, c, in := memoryFixture(t)
	if err := s.Observe(in, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	id := ScopeID(in.Channel, in.Account, in.Thread)
	before := recallEpochForTest(t, s)
	text := "本群收到视频分享后默认总结主要内容；不要仅凭标题回答。"
	if err := s.SetPreferences(id, text, 0); err != nil {
		t.Fatal(err)
	}
	if recallEpochForTest(t, s) == before {
		t.Fatal("old generation evidence still valid")
	}
	if got, rev := s.Preferences(id); got != text || rev != 1 {
		t.Fatal(got, rev)
	}
	if err := s.SetPreferences(id, "stale", 0); err == nil {
		t.Fatal("stale save accepted")
	}
	if got, _ := s.Preferences(ScopeID(in.Channel, "other", in.Thread)); got != "" {
		t.Fatal("account leak")
	}
	if err := s.Clear(id); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Preferences(id); got != text {
		t.Fatal("memory clear deleted administrator settings")
	}
	if err := s.SetPreferences(id, "", 1); err != nil {
		t.Fatal(err)
	}
	if got, rev := s.Preferences(id); got != "" || rev != 2 {
		t.Fatal("clear preference failed")
	}
}

func TestHostedGroupNoteAndSharePolicyReachModelWithinOneTurn(t *testing.T) {
	s, c, in := memoryFixture(t)
	c.Enabled = true
	c.QuietStart, c.QuietEnd = "00:00", "00:00"
	c.Permissions.ObserveGroups = true
	in.Mode = "direct"
	in.IsGroup = true
	in.GroupShareSummary = true
	in.Text = "synthetic share"
	in = syntheticNativeInput(in, "host-preference-fixture")
	model := &fakeModel{answer: `{"action":"silent","messages":[]}`}
	const note = "Keep group summaries concise."
	e := &Engine{Config: c, Store: s, model: model, ConversationPreference: func(got Input) (string, bool) {
		return note, got.Channel == in.Channel && got.Account == in.Account && got.Thread == in.Thread
	}}
	if _, err := e.Respond(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || !strings.Contains(model.system, "这是主人为当前群启用的分享总结") {
		t.Fatalf("hosted share guidance absent or extra calls: %d", model.calls)
	}
	found := false
	for _, turn := range model.turns {
		var payload map[string]any
		if json.Unmarshal([]byte(turn["content"]), &payload) != nil || payload["kind"] != "current_message" {
			continue
		}
		found = payload["conversation_preferences"] == note && payload["group_share_summary"] == true
		if payload["directed"] == true || payload["must_reply"] == true {
			t.Fatal("automatic summary fabricated a mention or forced reply")
		}
	}
	if !found {
		t.Fatal("hosted note or share marker missing from current message")
	}
	if text, _ := s.Preferences(ScopeID(in.Channel, in.Account, in.Thread)); text != "" {
		t.Fatal("host policy was written to memory")
	}
	other := in
	other.Account = "other-account"
	if got := e.conversationScope(other, c, time.Now()).Preferences; got != "" {
		t.Fatalf("host note leaked across accounts: %q", got)
	}
}
