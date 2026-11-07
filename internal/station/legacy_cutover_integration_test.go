package station

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/fishidentity"
)

// TestLegacyCutoverKeepsOneFishAcrossSettingsHomeAndPrivateMedia is a small
// offline rehearsal of the first station cutover.  It deliberately uses the
// public migration and local-owner HTTP surfaces rather than constructing a
// second identity or writing target registrations by hand.
//
// It does not start a model, an Agent, a native application, or a real media
// service.  The speech/RVC fixtures are synthetic files only.
func TestLegacyCutoverKeepsOneFishAcrossSettingsHomeAndPrivateMedia(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	workspace := filepath.Join(data, "managed-workspaces")
	identity, voiceID := prepareLegacyCutover(t, base, data, workspace)
	assertCutoverLocalHome(t, data, workspace, identity, voiceID)
	// Reopen the real HTTP host over the same private data.
	assertCutoverLocalHome(t, data, workspace, identity, voiceID)
}

func prepareLegacyCutover(t *testing.T, base, data, workspace string) (fishidentity.Record, string) {
	t.Helper()
	ctx := t.Context()
	legacyAdmin := filepath.Join(base, "legacy-admin")
	legacyProject := filepath.Join(base, "old-project")
	if err := os.MkdirAll(legacyAdmin, 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacySettingsFixture(t, legacyAdmin, "synthetic-cutover-key-000000000000000", true)
	oldIdentity, err := fishidentity.New(legacyAdmin).Read()
	if err != nil || oldIdentity.LocalOwner == nil || !oldIdentity.LocalOwner.HomeAccess {
		t.Fatalf("synthetic legacy identity is not importable: %#v, %v", oldIdentity, err)
	}
	if err := os.MkdirAll(filepath.Join(legacyProject, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	projectFile := filepath.Join(legacyProject, "nested", "tool.py")
	projectBytes := []byte("print('synthetic cutover project')\n")
	if err := os.WriteFile(projectFile, projectBytes, 0600); err != nil {
		t.Fatal(err)
	}

	settings, err := PreviewLegacySettings(ctx, legacyAdmin, data)
	if err != nil || settings.FishID != oldIdentity.FishID || !settings.HasIdentity {
		t.Fatalf("settings preview: %#v, %v", settings, err)
	}
	if _, err = ImportLegacySettings(ctx, legacyAdmin, data, settings.Digest); err != nil {
		t.Fatalf("settings import: %v", err)
	}
	identity, err := fishidentity.New(data).Read()
	if err != nil || identity.FishID != oldIdentity.FishID || identity.LocalOwner == nil || !identity.LocalOwner.HomeAccess {
		t.Fatalf("settings import changed local identity: %#v, %v", identity, err)
	}

	home, err := PreviewLegacyHome(ctx, data, workspace, legacyProject, "projects/imported-project", 1<<20)
	if err != nil || home.FishID != identity.FishID || !bytes.HasPrefix([]byte(home.Target), []byte(filepath.Join(workspace, "fish_home"))) {
		t.Fatalf("home preview: %#v, %v", home, err)
	}
	if _, err = ImportLegacyHome(ctx, data, workspace, legacyProject, "projects/imported-project", 1<<20, home.Digest); err != nil {
		t.Fatalf("home import: %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(home.Target, "nested", "tool.py"))
	if err != nil || !bytes.Equal(copied, projectBytes) {
		t.Fatalf("project bytes changed during cutover: %q, %v", copied, err)
	}

	// Each legacy resource fixture owns a separate synthetic legacy library.
	// Only the target data/workspace are shared so both imports must bind to the
	// identity installed above rather than creating a parallel fish.
	speech := newLegacySpeechFixture(t)
	speech.data, speech.workspace = data, workspace
	speech.write(t)
	speechSource, err := os.ReadFile(speech.catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	speechPlan, err := PreviewLegacySpeech(ctx, data, workspace, speech.optionsFile)
	if err != nil || speechPlan.FishID != identity.FishID || len(speechPlan.Voices) != 1 {
		t.Fatalf("speech preview: %#v, %v", speechPlan, err)
	}
	if _, err = ImportLegacySpeech(ctx, data, workspace, speech.optionsFile, speechPlan.Digest); err != nil {
		t.Fatalf("speech import: %v", err)
	}
	if after, readErr := os.ReadFile(speech.catalogFile); readErr != nil || !bytes.Equal(speechSource, after) {
		t.Fatalf("speech import changed old catalog: %v", readErr)
	}
	speechInfo, err := newLocalTTSBackendAt(data, workspace, speech.executable).Info(ctx, identity.FishID)
	if err != nil || len(speechInfo.Voices) != 1 || speechInfo.Voices[0].ID != speech.entry.ID {
		t.Fatalf("imported speech is not owned by the installed fish: %#v, %v", speechInfo, err)
	}

	rvc := newLegacyRVCFixture(t)
	rvc.data, rvc.editable = data, workspace
	rvc.write(t)
	rvcSource, err := os.ReadFile(rvc.catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	rvcPlan, err := PreviewLegacyRVC(ctx, data, workspace, rvc.optionsFile)
	if err != nil || rvcPlan.FishID != identity.FishID || len(rvcPlan.Profiles) != 1 {
		t.Fatalf("RVC preview: %#v, %v", rvcPlan, err)
	}
	if _, err = ImportLegacyRVC(ctx, data, workspace, rvc.optionsFile, rvcPlan.Digest); err != nil {
		t.Fatalf("RVC import: %v", err)
	}
	if after, readErr := os.ReadFile(rvc.catalogFile); readErr != nil || !bytes.Equal(rvcSource, after) {
		t.Fatalf("RVC import changed old catalog: %v", readErr)
	}
	var rvcRegistration localRVCRegistration
	rawRVC, err := os.ReadFile(filepath.Join(data, localRVCRegistrationFile))
	if err != nil || json.Unmarshal(rawRVC, &rvcRegistration) != nil || rvcRegistration.FishID != identity.FishID || len(rvcRegistration.Profiles) != 1 {
		t.Fatalf("RVC registration lost installed identity or profile: %#v, %v", rvcRegistration, err)
	}
	for _, name := range []string{"synthetic-model.pth", "synthetic.index"} {
		source, readErr := os.ReadFile(filepath.Join(rvc.library, "weights", name))
		target, targetErr := os.ReadFile(filepath.Join(localRVCPrivateScope(rvc.workspace, identity.FishID), "voice-library", "resources", "weights", name))
		if readErr != nil || targetErr != nil || !bytes.Equal(source, target) {
			t.Fatalf("RVC resource bytes changed or escaped the private scope (%s): %v, %v", name, readErr, targetErr)
		}
	}

	return identity, speech.entry.ID
}

func assertCutoverLocalHome(t *testing.T, data, workspace string, identity fishidentity.Record, voiceID string) {
	t.Helper()
	handler, err := NewLocalHomeHandlerWithApps(data, workspace, "synthetic-local-password", "", apphost.Registry{})
	if err != nil {
		t.Fatalf("open local home: %v", err)
	}
	server := httptest.NewServer(handler)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, dir: data, root: workspace, handler: handler}
	defer func() {
		server.Close()
		if closeErr := handler.Close(); closeErr != nil {
			t.Errorf("close local home: %v", closeErr)
		}
	}()
	fixture.login()
	assertCutoverOwnerState(t, fixture, identity, voiceID)
}

func assertCutoverOwnerState(t *testing.T, fixture *localHomeFixture, identity fishidentity.Record, voiceID string) {
	t.Helper()
	if code, home := fixture.request(http.MethodGet, "/api/local/home", nil, true); code != http.StatusOK || home["fish_id"] != identity.FishID || home["home_access"] != true {
		t.Fatalf("HTTP home lost imported identity: %d %#v", code, home)
	}
	if code, model := fixture.request(http.MethodGet, "/api/local/model", nil, true); code != http.StatusOK ||
		model["model"] != "deepseek-flash" || model["key_configured"] != true ||
		model["thinking_enabled"] != true || model["reasoning_effort"] != "low" {
		t.Fatalf("HTTP model lost imported settings: %d %#v", code, model)
	}
	if code, tools := fixture.request(http.MethodGet, "/api/local/project-tools", nil, true); code != http.StatusOK || tools["tools"] == nil {
		t.Fatalf("HTTP tool catalog unavailable after cutover: %d %#v", code, tools)
	}
	// The HTTP catalogue is intentionally executable-directory agnostic.  The
	// registration itself is checked here without exposing its host paths.
	registered, err := newLocalTTSBackendAt(fixture.dir, fixture.root, "").loadRegistration(context.Background())
	if err != nil || len(registered.Voices) != 1 || registered.Voices[0].ID != voiceID {
		t.Fatalf("speech registration unavailable after HTTP restart: %#v, %v", registered, err)
	}
}
