package station

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/mediaspec"
)

// legacyRVCFixture is deliberately small, but has the same ownership and
// runtime boundaries as a real private RVC library.  In particular, the old
// library is outside both station storage and the fish home.
type legacyRVCFixture struct {
	data, editable, workspace, media, library, optionsFile, catalogFile string
	options                                                             legacyRVCOptions
	entry                                                               mediaspec.VoiceEntry
	registry                                                            *fishidentity.Registry
}

func newLegacyRVCFixture(t *testing.T) legacyRVCFixture {
	t.Helper()
	root := t.TempDir()
	f := legacyRVCFixture{
		data:        filepath.Join(root, "data"),
		editable:    filepath.Join(root, "data", "managed-workspaces"),
		workspace:   filepath.Join(root, "rvc-workspace"),
		media:       filepath.Join(root, "media"),
		library:     filepath.Join(root, "legacy-voice-library"),
		optionsFile: filepath.Join(root, "rvc-import.json"),
	}
	public := filepath.Join(f.workspace, "public_workspace")
	for _, path := range []string{
		f.data, f.editable, f.media, f.library, filepath.Join(f.library, "manifests"), filepath.Join(f.library, "profiles"), filepath.Join(f.library, "weights"),
		filepath.Join(f.workspace, "pri_workspace"), filepath.Join(public, "runtimes", "runtime"),
		filepath.Join(public, "projects", "rvc", "assets", "hubert_base"), filepath.Join(public, "projects", "rvc", "assets", "rmvpe"),
		filepath.Join(public, "projects", "rvc", "infer", "vc"),
	} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join(f.media, "ffmpeg.exe"), filepath.Join(f.media, "ffprobe.exe"),
		filepath.Join(public, "runtimes", "python.exe"), filepath.Join(public, "runtimes", "rvc.exe"),
		filepath.Join(public, "projects", "rvc", "assets", "hubert_base", "pytorch_model.bin"),
		filepath.Join(public, "projects", "rvc", "assets", "rmvpe", "rmvpe.pt"),
		filepath.Join(public, "projects", "rvc", "infer", "vc", "modules.py"),
	} {
		if err := os.WriteFile(path, []byte("synthetic fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	model := filepath.Join(f.library, "weights", "synthetic-model.pth")
	index := filepath.Join(f.library, "weights", "synthetic.index")
	if err := os.WriteFile(model, []byte("synthetic rvc model"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(index, []byte("synthetic rvc index"), 0600); err != nil {
		t.Fatal(err)
	}
	profileID, legacyID := "private-synthetic-rvc", "legacy-synthetic-rvc"
	config := filepath.Join(f.library, "profiles", profileID+".config")
	preset := legacyRVCPreset{
		Enabled: true, Engine: "rvc", Profile: profileID,
		Python: filepath.Join(public, "runtimes", "python.exe"), RVCPython: filepath.Join(public, "runtimes", "rvc.exe"),
		Runtime: filepath.Join(public, "runtimes", "runtime"), Project: filepath.Join(public, "projects", "rvc"),
		Model: model, Index: index, ModelSHA256: localTTSFileSHA(model), IndexSHA256: localTTSFileSHA(index),
		Device: "cpu", Precision: "auto",
	}
	raw, err := json.Marshal(preset)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(f.library, "manifests", profileID+".json")
	raw, err = json.Marshal(legacyRVCManifest{Configuration: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f.entry = mediaspec.VoiceEntry{ID: legacyID, Name: "合成 RVC 声线", Owner: "synthetic-original-owner", Engine: "rvc", Status: "ready", Configuration: config, ProfileID: profileID}
	f.options = legacyRVCOptions{
		Version: 1, Library: f.library, SourceOwner: f.entry.Owner, VoiceIDs: []string{legacyID},
		Deployment: localRVCRegistration{SchemaVersion: 1, Enabled: true, Workspace: f.workspace, Media: f.media, Port: 32491},
	}
	f.catalogFile = filepath.Join(f.library, "catalog.json")
	f.registry = fishidentity.New(f.data)
	if _, err := f.registry.InitializeLocal(); err != nil {
		t.Fatal(err)
	}
	f.write(t)
	return f
}

func (f legacyRVCFixture) write(t *testing.T) {
	t.Helper()
	for path, value := range map[string]any{f.optionsFile: f.options, f.catalogFile: map[string]any{"items": []mediaspec.VoiceEntry{f.entry}}} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyRVCPreviewDoesNotWriteAndImportPreservesPrivateSource(t *testing.T) {
	f := newLegacyRVCFixture(t)
	beforeCatalog, err := os.ReadFile(f.catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, err := os.ReadFile(f.entry.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
	if err != nil || len(plan.Profiles) != 1 || plan.Profiles[0].ID != f.entry.ProfileID || plan.Profiles[0].LegacyVoiceID != f.entry.ID || plan.Bytes <= 0 || len(plan.Digest) != 64 {
		t.Fatalf("legacy RVC preview failed: %#v, %v", plan, err)
	}
	target := filepath.Join(localRVCPrivateScope(f.workspace, plan.FishID), "voice-library")
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("preview wrote private library: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.data, localRVCRegistrationFile)); !os.IsNotExist(err) {
		t.Fatalf("preview wrote registration: %v", err)
	}

	for range 2 {
		if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err != nil {
			t.Fatalf("import: %v", err)
		}
	}
	afterCatalog, _ := os.ReadFile(f.catalogFile)
	afterConfig, _ := os.ReadFile(f.entry.Configuration)
	if !bytes.Equal(beforeCatalog, afterCatalog) || !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatal("import modified the legacy source")
	}
	var registration localRVCRegistration
	raw, err := os.ReadFile(filepath.Join(f.data, localRVCRegistrationFile))
	if err != nil || json.Unmarshal(raw, &registration) != nil || registration.FishID != plan.FishID || len(registration.Profiles) != 1 || registration.Profiles[0] != plan.Profiles[0] {
		t.Fatalf("registration missing mapped private profile: %#v, %v", registration, err)
	}
	manifestPath := filepath.Join(target, "manifests", f.entry.ProfileID+".json")
	var manifest legacyRVCManifest
	raw, err = os.ReadFile(manifestPath)
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		t.Fatalf("private manifest: %v", err)
	}
	var preset legacyRVCPreset
	raw, err = os.ReadFile(manifest.Configuration)
	if err != nil || json.Unmarshal(raw, &preset) != nil || preset.ModelSHA256 != localTTSFileSHA(filepath.Join(target, "resources", "weights", "synthetic-model.pth")) || preset.IndexSHA256 != localTTSFileSHA(filepath.Join(target, "resources", "weights", "synthetic.index")) {
		t.Fatalf("private preset did not preserve hashes: %#v, %v", preset, err)
	}
	for _, source := range []string{preset.Model, preset.Index} {
		if !localRVCInside(source, target) || !localRVCSafeRegular(source) {
			t.Fatalf("resource escaped target private library: %q", source)
		}
	}
}

func TestLegacyRVCRejectsChangedOrUnauthorizedSources(t *testing.T) {
	t.Run("unowned catalog requires explicit confirmation", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		f.entry.Owner = ""
		f.options.SourceOwner = ""
		f.write(t)
		if _, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile); err == nil {
			t.Fatal("unowned source was accepted without confirmation")
		}
		f.options.AllowUnowned = true
		f.write(t)
		plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
		if err != nil || !plan.UnownedSource {
			t.Fatalf("explicit unowned source was not planned: %#v, %v", plan, err)
		}
	})
	t.Run("stale catalog configuration uses checked manifest", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		f.entry.Configuration = filepath.Join(f.library, "display-only", "old-location.config")
		f.write(t)
		plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
		if err != nil || plan.CatalogPathDifferences != 1 {
			t.Fatalf("stale catalog path did not retain checked manifest: %#v, %v", plan, err)
		}
	})
	t.Run("budget", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		f.options.MaxBytes = 1
		f.write(t)
		if _, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile); err == nil {
			t.Fatal("budget overflow was accepted")
		}
	})
	t.Run("source resource changed after preview", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.library, "weights", "synthetic-model.pth"), []byte("tampered synthetic rvc"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err == nil {
			t.Fatal("changed source was imported")
		}
	})
	t.Run("owner grant changed after preview", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.registry.SetHomeAccess(plan.OwnerRevision, false); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err == nil {
			t.Fatal("revoked home access was accepted")
		}
	})
	t.Run("target conflict", func(t *testing.T) {
		f := newLegacyRVCFixture(t)
		plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(localRVCPrivateScope(f.workspace, plan.FishID), "voice-library")
		if err := os.MkdirAll(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "other.txt"), []byte("other import"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err == nil {
			t.Fatal("conflicting target was overwritten")
		}
	})
}

func TestLegacyRVCImportSerializesSamePlan(t *testing.T) {
	f := newLegacyRVCFixture(t)
	plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errors := make(chan error, 3)
	for range 3 {
		group.Go(func() {
			_, err := ImportLegacyRVC(context.Background(), f.data, f.editable, f.optionsFile, plan.Digest)
			errors <- err
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("parallel retry failed: %v", err)
		}
	}
}

func TestLegacyRVCPackagedBridgeChecksMigratedProfileAfterRestart(t *testing.T) {
	preview := os.Getenv("AHS_STATION_RVC_PREVIEW")
	if runtime.GOOS != "windows" || preview == "" {
		t.Skip("requires isolated Windows package with RVC")
	}
	f := newLegacyRVCFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.options.Deployment.Port = listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	f.write(t)
	plan, err := PreviewLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err != nil {
		t.Fatal(err)
	}
	// Replay the window after the library committed but before registration.
	if err := os.Remove(filepath.Join(f.data, localRVCRegistrationFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLegacyRVC(t.Context(), f.data, f.editable, f.optionsFile, plan.Digest); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	service := newLocalRVCServiceAt(f.data, f.editable, preview)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := service.Info(ctx, "fish-"+strings.Repeat("c", 48)); err == nil {
		t.Fatal("migrated private profile was visible to a different fish")
	}
	for range 2 {
		info, err := service.Info(ctx, plan.FishID)
		if err != nil || info.Running || len(info.Profiles) != 1 || info.Profiles[0] != plan.Profiles[0] {
			t.Fatalf("unexpected migrated registration: %+v, %v", info, err)
		}
		body, code, err := service.Call(ctx, plan.FishID, info.Binding, http.MethodGet,
			"/rvc-health?voice_profile_id="+url.QueryEscape(f.entry.ProfileID), "", nil)
		var response struct {
			localRVCVoicePin
			Ready bool   `json:"ready"`
			ID    string `json:"voice_profile_id"`
		}
		if err != nil || code != http.StatusOK || json.Unmarshal(body, &response) != nil || !response.Ready ||
			response.ID != f.entry.ProfileID || !response.localRVCVoicePin.valid() ||
			response.ModelHash != localTTSFileSHA(filepath.Join(f.library, "weights", "synthetic-model.pth")) ||
			response.IndexHash != localTTSFileSHA(filepath.Join(f.library, "weights", "synthetic.index")) {
			t.Fatalf("migrated profile not verified by packaged bridge: HTTP %d, %s, %v", code, body, err)
		}
		if err := service.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
