package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func webPublicationFixture(t *testing.T) (*localHomeFixture, *localDouyinWebPublication, localDouyinWebAccess, apphost.ConnectionRef) {
	t.Helper()
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	_, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if code, out := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_publish", "revision": view["revision"], "allow_publish": true}, true); code != 200 || out["read_enabled"] != false {
		t.Fatalf("independent grant: %d %#v", code, out)
	}
	p := &localDouyinWebPublication{home: f.handler.home, installation: "built-in-douyin"}
	a, err := p.access(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ref := apphost.ConnectionRef{AppID: douyinapp.ID, InstallationID: p.installation, ConnectionID: a.connection.ID, AccountID: a.connection.AccountID}
	return f, p, a, ref
}

func TestLocalDouyinPublicationDiagnosticsRemainSafeAndRecoverable(t *testing.T) {
	if code := localDouyinCreatorErrorCode(errors.New("private-profile-path and synthetic-secret-value")); code != "creator_failed" {
		t.Fatal("raw provider error escaped", code)
	}
	if code := localDouyinCreatorErrorCode(stationbrowser.ErrDouyinWebLoginRequired); code != "creator_login_required" {
		t.Fatal(code)
	}
	_, p, _, ref := webPublicationFixture(t)
	reference := apphost.PublicationReference{Target: ref, TaskID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64)}
	checkpoint := localDouyinPublicationCheckpoint{Version: 1, TaskID: reference.TaskID, ManifestSHA256: reference.ManifestSHA256, ErrorCode: "creator_controls_unavailable"}
	progress, err := p.PublicationCheckpointProgress(mustLocalDouyinJSON(checkpoint))
	if err != nil || progress.Stage != "uploading" || progress.ErrorCode != checkpoint.ErrorCode {
		t.Fatalf("lost diagnostic %#v %v", progress, err)
	}
	receipt, err := p.ReconcilePublication(t.Context(), reference, mustLocalDouyinJSON(checkpoint), false)
	if err != nil || receipt.State != "unknown" || receipt.Progress.ErrorCode != checkpoint.ErrorCode {
		t.Fatalf("pre-submit diagnostic %#v %v", receipt, err)
	}
}

func TestLocalDouyinPublicationRecoveryRejectsChangedIdentityBeforeBrowser(t *testing.T) {
	_, p, _, ref := webPublicationFixture(t)
	reference := apphost.PublicationReference{Target: ref, TaskID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64)}
	checkpoint := localDouyinPublicationCheckpoint{Version: 1, TaskID: reference.TaskID, ManifestSHA256: reference.ManifestSHA256, Submitted: true}
	req := apphost.PublicationRecoveryRequest{Reference: reference, Checkpoint: mustLocalDouyinJSON(checkpoint), CreatedAt: "2026-11-01T00:00:00Z", Draft: apphost.PublicationDraft{Target: ref, Title: "合成标题", Body: "合成正文", Options: json.RawMessage(`{"visibility":"private"}`), Artifacts: []apphost.PublicationArtifact{{ID: "synthetic"}}}}
	changed := req
	changed.Draft.Target.AccountID = "other-account"
	if _, err := p.RecoverPublication(t.Context(), changed); !errors.Is(err, apphost.ErrPublicationConflict) {
		t.Fatal("changed draft target", err)
	}
	changed = req
	changed.CreatedAt = "not-a-host-time"
	if _, err := p.RecoverPublication(t.Context(), changed); !errors.Is(err, apphost.ErrPublicationConflict) {
		t.Fatal("invented time", err)
	}
	changed = req
	changed.Reference.Target.AccountID = "other-account"
	changed.Draft.Target = changed.Reference.Target
	if _, err := p.RecoverPublication(t.Context(), changed); !errors.Is(err, apphost.ErrPublicationDenied) {
		t.Fatal("wrong saved account", err)
	}
	checkpoint.RemoteID = "123"
	req.Reference.RemoteID = "456"
	a, _ := p.access(t.Context())
	if _, err := p.reconcile(t.Context(), a, req.Reference, checkpoint); !errors.Is(err, apphost.ErrPublicationConflict) {
		t.Fatal("conflicting recovered ID", err)
	}
}

func TestLocalDouyinPublicationGrantAndSubmittedRecovery(t *testing.T) {
	f, p, _, ref := webPublicationFixture(t)
	grant, err := p.PublicationGrant(t.Context(), ref)
	if err != nil || !grant.Allowed {
		t.Fatalf("publish without read grant: %#v %v", grant, err)
	}
	reference := apphost.PublicationReference{Target: ref, TaskID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64)}
	checkpoint := localDouyinPublicationCheckpoint{Version: 1, TaskID: reference.TaskID, ManifestSHA256: reference.ManifestSHA256, Submitted: true}
	request := apphost.PublicationRequest{Reference: reference, Draft: apphost.PublicationDraft{Target: ref, Artifacts: []apphost.PublicationArtifact{{ID: "synthetic"}}}, Checkpoint: mustLocalDouyinJSON(checkpoint), GrantVersion: grant.Version,
		CheckActive: func(context.Context) error { return nil },
		OpenArtifact: func(context.Context, apphost.PublicationArtifact) (io.ReadCloser, error) {
			t.Fatal("submitted task reopened original")
			return nil, nil
		},
		SaveCheckpoint: func(context.Context, json.RawMessage) error { return nil },
	}
	receipt, err := p.ContinuePublication(t.Context(), request)
	if err != nil || receipt.State != "unknown" || receipt.Progress.Stage != "posting" {
		t.Fatalf("unknown recovery: %#v %v", receipt, err)
	}
	_, view := f.request("GET", localDouyinWebRoute, nil, true)
	if code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin_creator", "revision": view["revision"], "approve_profile": true}, true); code != 200 {
		t.Fatal(code)
	}
	if grant, err := p.PublicationGrant(t.Context(), ref); err == nil && grant.Allowed {
		t.Fatal("old profile generation still authorized")
	}
}

func TestLocalDouyinPublicationStageAndPreSubmitRecovery(t *testing.T) {
	_, p, access, ref := webPublicationFixture(t)
	data := "synthetic private movie payload"
	hash := sha256.Sum256([]byte(data))
	artifact := apphost.PublicationArtifact{ID: "synthetic", Name: "movie.mp4", Bytes: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
	reference := apphost.PublicationReference{Target: ref, TaskID: strings.Repeat("a", 64), ManifestSHA256: strings.Repeat("b", 64)}
	reads := 0
	request := apphost.PublicationRequest{Reference: reference, Draft: apphost.PublicationDraft{Artifacts: []apphost.PublicationArtifact{artifact}}, CheckActive: func(context.Context) error { return nil },
		OpenArtifact: func(context.Context, apphost.PublicationArtifact) (io.ReadCloser, error) {
			reads++
			return io.NopCloser(strings.NewReader(data)), nil
		}}
	checkpoint := localDouyinPublicationCheckpoint{Version: 1, TaskID: reference.TaskID, ManifestSHA256: reference.ManifestSHA256}
	path, err := p.stage(t.Context(), access, request, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(path) > 240 || strings.Contains(path, reference.TaskID+string(os.PathSeparator)+reference.ManifestSHA256) {
		t.Fatal("upload staging nests IDs beyond native file-picker range", len(path))
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != data {
		t.Fatal("staging did not preserve exact artifact", err)
	}
	if _, err := p.stage(t.Context(), access, request, checkpoint); err != nil || reads != 1 {
		t.Fatal("verified cache re-downloaded", reads, err)
	}
	request.Reference.TaskID = "../outside"
	if _, err := p.stage(t.Context(), access, request, checkpoint); !errors.Is(err, apphost.ErrPublicationInvalidDraft) {
		t.Fatal("unsafe task path", err)
	}
	for _, cancel := range []bool{false, true} {
		receipt, err := p.ReconcilePublication(t.Context(), reference, mustLocalDouyinJSON(checkpoint), cancel)
		wanted := "unknown"
		if cancel {
			wanted = "cancelled"
		}
		if err != nil || receipt.State != wanted {
			t.Fatalf("pre-submit cancel=%v: %#v %v", cancel, receipt, err)
		}
	}
}
