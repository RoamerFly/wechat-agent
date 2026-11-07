package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/localasr"
)

func TestASRDiagnosticsReportsPluginConfigurationAndLiveReadiness(t *testing.T) {
	f, native, policy, ctx := transcriptionFixture(t, false)
	config := f.manager.agent.config()
	config.Enabled = true
	config.OwnerIDs = []string{native.msg.SenderID}
	config.PrivilegedThreads = []string{native.msg.ThreadID}
	if err := f.manager.agent.save(config); err != nil {
		t.Fatal(err)
	}
	p := &permittedTools{manager: f.manager, trace: companion.Input{Channel: "wechat", Account: "test", Thread: native.msg.ThreadID, Sender: native.msg.SenderID}}
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = false
		legacySpeechFixture.ASRModel = "stale-legacy-model"
		legacySpeechFixture.ASRBackend = "local_then_bailian"
		legacySpeechFixture.ASRLocalModel = "stale-legacy-local-profile"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	assert := func(ready bool) {
		t.Helper()
		raw, err := p.callDiagnostics(json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Plugin  string `json:"asr_plugin"`
			Ready   bool   `json:"asr_enabled"`
			Backend string `json:"asr_backend"`
			Model   string `json:"asr_model"`
		}
		if json.Unmarshal([]byte(raw), &result) != nil || result.Plugin != "media-understanding" || result.Ready != ready || result.Backend != "remote" || result.Model != "synthetic-asr" || strings.Contains(raw, "synthetic-asr-key") || strings.Contains(raw, "stale-legacy") {
			t.Fatal("diagnostics used stale settings or exposed credentials", raw)
		}
	}
	assert(true)
	installed, err := policy.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	assert(false)
}
