package admin

import (
	"encoding/json"
	"testing"

	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

func TestIncomingVoiceRequiresMediaPluginBeforeDownloading(t *testing.T) {
	f := setup(t)
	native := &observationNative{fakeNative: f.native, data: []byte("must not be read")}
	f.manager.native = native
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = true
		legacySpeechFixture.ASRBackend = "local_then_bailian"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	if _, err := (mediaUnderstanding{manager: f.manager}).recognizeVoice(t.Context(), linuxnative.Message{ID: "incoming-voice", Type: 34}); err == nil || err.Error() != "media_transcription_plugin_migration_required" {
		t.Fatal("missing plugin did not stop before download", err)
	}
	if native.calls != 0 {
		t.Fatal("missing plugin downloaded incoming audio", native.calls)
	}
}

func TestLocalASRStatusRequiresInstalledConfiguredRegisteredPlugin(t *testing.T) {
	f := setup(t)
	// Old speech flags and a legacy model name no longer make local ASR
	// available. The plugin registry is the only source of that claim.
	{
		legacySpeechFixture := readLegacySpeechTestConfig(t, f.manager)
		legacySpeechFixture.ASREnabled = true
		legacySpeechFixture.ASRBackend = "local_then_bailian"
		legacySpeechFixture.ASRLocalModel = "sensevoice-small-int8"
		writeLegacySpeechMigrationFixture(t, f.manager, legacySpeechFixture)
	}
	if status := f.manager.localASRStatus(); status["available"] != false || status["message"] != "媒体理解工具未安装或未启用" {
		t.Fatal("legacy speech state was treated as local plugin readiness", status)
	}

	fixture, _, policy, ctx := mediaPolicyFixture(t, false)
	installed, err := policy.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, false); err != nil {
		t.Fatal(err)
	}
	configuration, err := policy.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]json.RawMessage{}
	for key, value := range configuration.Values {
		values[key] = append(json.RawMessage(nil), value...)
	}
	values["asr_backend"] = json.RawMessage(`"local"`)
	values["local_asr_profile_id"] = json.RawMessage(`"sensevoice-small-int8"`)
	if _, err = policy.store.SaveConfiguration(ctx, localasr.PluginID, pluginhost.ConfigUpdate{Revision: configuration.Revision, Values: values, ClearSecrets: []string{"asr_key"}}); err != nil {
		t.Fatal(err)
	}
	installed, err = policy.store.Installed(ctx, localasr.PluginID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = policy.store.SetEnabled(ctx, localasr.PluginID, installed.Revision, true); err != nil {
		t.Fatal(err)
	}
	if status := fixture.manager.localASRStatus(); status["available"] != false || status["message"] != "本地语音识别资源尚未登记或已失效" {
		t.Fatal("unregistered local resources were accepted", status)
	}
	_, _, expected := writeRegisteredLocalASRProfile(t, fixture.manager)
	status := fixture.manager.localASRStatus()
	if status["available"] != true || status["id"] != expected.Initializer.ProfileID || status["resource_version"] != expected.ResourceVersion {
		t.Fatal("registered local profile status changed", status)
	}
}
