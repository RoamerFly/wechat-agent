package station

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalCreationToolsSupportDefaultDesktopWorkspace(t *testing.T) {
	t.Run("ComfyUI", func(t *testing.T) {
		data, _, registration := writeLocalComfyFixture(t)
		workspace := filepath.Join(data, "managed-workspaces")
		service := newLocalComfyServiceAt(data, workspace)
		if _, err := service.Info(t.Context(), "synthetic-fish"); err != nil {
			t.Fatal("desktop workspace rejected valid private ComfyUI deployment", err)
		}
		registration.Entry = filepath.ToSlash(registration.Entry)
		if err := registration.validate(data, workspace); err != nil {
			t.Fatal("documented slash-separated ComfyUI entry rejected", err)
		}
		var manifest localComfyManifest
		raw, err := os.ReadFile(registration.Manifest)
		if err != nil || json.Unmarshal(raw, &manifest) != nil {
			t.Fatal(err)
		}
		files := map[string]string{}
		for name, hash := range manifest.Files {
			files[filepath.ToSlash(name)] = hash
		}
		manifest.Files = files
		raw, _ = json.Marshal(manifest)
		if err := os.WriteFile(registration.Manifest, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := localComfyLoadManifest(registration); err != nil {
			t.Fatal("documented slash-separated ComfyUI manifest rejected", err)
		}
		if err := registration.validate(data, data); err == nil {
			t.Fatal("editable private registry accepted")
		}
		if err := registration.validate(data, filepath.Dir(data)); err == nil {
			t.Fatal("editable parent of private registry accepted")
		}
		content, err := os.ReadFile(registration.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		registration.Manifest = filepath.Join(workspace, "editable-manifest.json")
		if err := os.WriteFile(registration.Manifest, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := registration.validate(data, workspace); err == nil {
			t.Fatal("Agent-editable ComfyUI control manifest accepted")
		}
	})
	t.Run("YuE", func(t *testing.T) {
		data, _, registration := writeLocalYuEFixture(t)
		workspace := filepath.Join(data, "managed-workspaces")
		service := newLocalYuEServiceAt(data, workspace)
		if _, err := service.Info(t.Context(), "synthetic-fish"); err != nil {
			t.Fatal("desktop workspace rejected valid private YuE deployment", err)
		}
		registration.Entry = filepath.ToSlash(registration.Entry)
		if err := registration.validate(data, workspace); err != nil {
			t.Fatal("documented slash-separated YuE entry rejected", err)
		}
		if err := registration.validate(data, data); err == nil {
			t.Fatal("editable private registry accepted")
		}
		if err := registration.validate(data, filepath.Dir(data)); err == nil {
			t.Fatal("editable parent of private registry accepted")
		}
		content, err := os.ReadFile(registration.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(workspace, 0700); err != nil {
			t.Fatal(err)
		}
		registration.Manifest = filepath.Join(workspace, "editable-manifest.json")
		if err := os.WriteFile(registration.Manifest, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := registration.validate(data, workspace); err == nil {
			t.Fatal("Agent-editable YuE control manifest accepted")
		}
	})
}

func TestLocalCreationToolRelativePathsRejectDriveRelativeAndTraversal(t *testing.T) {
	for _, name := range []string{"../entry.py", "ComfyUI/../entry.py", ".", "..", "", "C:entry.py", "C:/entry.py"} {
		if localComfySafeRelative(name) {
			t.Fatalf("unsafe creation-tool path accepted: %q", name)
		}
	}
}
