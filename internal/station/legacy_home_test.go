package station

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/fishidentity"
)

type legacyHomeFixture struct {
	data, workspace, source, target string
	registry                        *fishidentity.Registry
}

func newLegacyHomeFixture(t *testing.T) legacyHomeFixture {
	t.Helper()
	base := t.TempDir()
	f := legacyHomeFixture{data: filepath.Join(base, "station"), workspace: filepath.Join(base, "workspace"), source: filepath.Join(base, "old-project"), target: "projects/selected"}
	for _, name := range []string{f.data, f.source} {
		if err := os.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f.registry = fishidentity.New(f.data)
	if _, err := f.registry.InitializeLocal(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.source, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"tool.py": "print('synthetic persistent tool')\n", "config.json": `{"project":"synthetic settings"}`} {
		if err := os.WriteFile(filepath.Join(f.source, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f legacyHomeFixture) preview(ctx context.Context, maxBytes int64) (LegacyHomePlan, error) {
	return PreviewLegacyHome(ctx, f.data, f.workspace, f.source, f.target, maxBytes)
}

func (f legacyHomeFixture) apply(ctx context.Context, digest string) (LegacyHomePlan, error) {
	return ImportLegacyHome(ctx, f.data, f.workspace, f.source, f.target, 4096, digest)
}

func TestLegacyHomeCopiesExactSelectedFilesAndRecoversReceipt(t *testing.T) {
	f := newLegacyHomeFixture(t)
	plan, err := f.preview(t.Context(), 4096)
	if err != nil || plan.Files != 2 || plan.Bytes == 0 {
		t.Fatal("preflight failed", plan, err)
	}
	raw, _ := json.Marshal(plan)
	if bytes.Contains(raw, []byte("persistent tool")) {
		t.Fatal("preview contains file contents")
	}
	if _, err := os.Stat(plan.Target); !os.IsNotExist(err) {
		t.Fatal("preview wrote the target", err)
	}
	// Simulate the private receipt destination failing after directory publish.
	receipts := filepath.Join(f.data, "home-imports")
	if err := os.WriteFile(receipts, []byte("synthetic disk obstruction"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), plan.Digest); err == nil {
		t.Fatal("receipt write failure was hidden")
	}
	if err := os.Remove(receipts); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.apply(t.Context(), plan.Digest); err != nil {
			t.Fatal("original operation did not recover", err)
		}
	}
	for _, name := range []string{"tool.py", "config.json"} {
		original, _ := os.ReadFile(filepath.Join(f.source, name))
		copied, err := os.ReadFile(filepath.Join(plan.Target, name))
		if err != nil || !bytes.Equal(original, copied) {
			t.Fatal("copied content changed", name, err)
		}
	}
	if info, err := os.Stat(filepath.Join(plan.Target, "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty project directory lost", err)
	}
	if _, err := os.Stat(filepath.Join(receipts, plan.Digest+".json")); err != nil {
		t.Fatal("private receipt was not repaired", err)
	}
	if err := os.WriteFile(filepath.Join(plan.Target, "tool.py"), []byte("user edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.apply(t.Context(), plan.Digest); err == nil {
		t.Fatal("import overwrote an edited target")
	}
}

func TestLegacyHomeRejectsStaleSourceAndRevokedOwner(t *testing.T) {
	for _, change := range []string{"source", "owner", "cancel"} {
		t.Run(change, func(t *testing.T) {
			f := newLegacyHomeFixture(t)
			plan, err := f.preview(t.Context(), 4096)
			if err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			switch change {
			case "source":
				err = os.WriteFile(filepath.Join(f.source, "new.txt"), []byte("new source"), 0600)
			case "owner":
				_, err = f.registry.SetHomeAccess(plan.Revision, false)
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.apply(ctx, plan.Digest); err == nil {
				t.Fatal("stale or cancelled migration was accepted")
			}
			if _, err := os.Stat(plan.Target); !os.IsNotExist(err) {
				t.Fatal("rejected migration published a target", err)
			}
		})
	}
}

func TestLegacyHomeRejectsPrivateSourcesTraversalAndBudget(t *testing.T) {
	f := newLegacyHomeFixture(t)
	if _, err := f.preview(t.Context(), 1); err == nil || !strings.Contains(err.Error(), "--legacy-home-max-bytes") {
		t.Fatal("budget failure did not explain continuation", err)
	}
	for _, target := range []string{"../outside", "projects", "Connected Apps/wechat", "projects/../outside"} {
		if _, err := PreviewLegacyHome(t.Context(), f.data, f.workspace, f.source, target, 4096); err == nil {
			t.Fatal("invalid target accepted", target)
		}
	}
	if err := os.WriteFile(filepath.Join(f.source, "provider-key"), []byte("synthetic private key marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.preview(t.Context(), 4096); err == nil {
		t.Fatal("private host source accepted")
	}
}

func TestLegacyHomeChecksScopeOfSelectedSubdirectory(t *testing.T) {
	f := newLegacyHomeFixture(t)
	allowed, err := f.preview(t.Context(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"managed-workspaces/pri_workspace/old-account/Conversations/old-group/output",
		"managed-workspaces/pri_workspace/old-account/voice-library/voice-files",
		"admin/media-assets/inbound/2026",
		"admin/media-history/old-account",
		"admin/manual-uploads/staging",
		"secrets",
		"managed-workspaces/pri_workspace/old-account/projects/selected",
		"managed-workspaces/public_workspace/projects/selected",
	} {
		t.Run(relative, func(t *testing.T) {
			source := filepath.Join(filepath.Dir(f.source), filepath.FromSlash(relative))
			if err := os.MkdirAll(source, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "ordinary.txt"), []byte("synthetic content"), 0600); err != nil {
				t.Fatal(err)
			}
			plan, err := PreviewLegacyHome(t.Context(), f.data, f.workspace, source, f.target, 4096)
			if strings.Contains(relative, "/projects/") {
				if err != nil || plan.Files != 1 {
					t.Fatalf("explicitly selected project blocked: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "受限素材范围") {
				t.Fatalf("protected source scope accepted: %v", err)
			}
			if _, err := ImportLegacyHome(t.Context(), f.data, f.workspace, source, f.target, 4096, allowed.Digest); err == nil ||
				!strings.Contains(err.Error(), "受限素材范围") {
				t.Fatalf("confirmation accepted a protected source scope: %v", err)
			}
			if _, err := os.Stat(allowed.Target); !os.IsNotExist(err) {
				t.Fatalf("rejected source created a target: %v", err)
			}
		})
	}
}

func TestLegacyHomeConcurrentImportsKeepOneVerifiedTarget(t *testing.T) {
	f := newLegacyHomeFixture(t)
	plan, err := f.preview(t.Context(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := f.apply(t.Context(), plan.Digest)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("concurrent continuation failed", err)
		}
	}
	if _, err := f.apply(t.Context(), plan.Digest); err != nil {
		t.Fatal("concurrent continuation damaged the target", err)
	}
}
