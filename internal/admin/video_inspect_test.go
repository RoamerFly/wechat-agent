package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaobserve"
)

func TestVideoInspectionFailuresExplainNextStepWithoutRawSecrets(t *testing.T) {
	if text := mcptools.FailureDescription(videoInspectionError(errors.New("video_inspect_probe_failed"))); !strings.Contains(text, "ffprobe") {
		t.Fatal(text)
	}
	if text := mcptools.FailureDescription(videoInspectionError(errors.New("private-path-and-token"))); strings.Contains(text, "private-path") {
		t.Fatal("raw failure leaked")
	}
}

func TestPluginAudioExtractionRangeIsNotClampedTo180Seconds(t *testing.T) {
	data := observationFixture(t, "240")
	f := setup(t)
	calls := 0
	a := mediaUnderstanding{manager: f.manager}
	text, seconds, err := a.videoAudioRangeTimedWith(context.Background(), data, 0, 240, 60, func(_ context.Context, wav []byte) (string, error) {
		calls++
		if len(wav) < 44 || string(wav[:4]) != "RIFF" {
			t.Fatal("plugin would receive invalid extracted WAV")
		}
		return "synthetic transcription", nil
	})
	if err != nil || calls != 1 || text != "synthetic transcription" || seconds < 239.9 || seconds > 240.1 {
		t.Fatal("plugin extraction range was shortened", calls, text, seconds, err)
	}
}

func inspectionFixture(t *testing.T) []byte {
	t.Helper()
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg unavailable")
	}
	path := filepath.Join(t.TempDir(), "synthetic.mp4")
	if b, e := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc2=s=96x64:r=12:d=2", "-c:v", "libx264", "-threads", "1", path).CombinedOutput(); e != nil {
		t.Fatalf("synthetic encoding %v %.200s", e, b)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return data
}

func TestVideoInspectionDecodesEveryFrameAcrossPages(t *testing.T) {
	data := inspectionFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "input.mp4")
	os.WriteFile(path, data, 0600)
	ctx := context.Background()
	meta, e := probeInspectionVideo(ctx, path)
	if e != nil || meta.Frames != 24 {
		t.Fatal(meta, e)
	}
	last := -1.0
	total := 0
	for _, page := range []struct{ start, count int }{{0, 13}, {13, 11}} {
		out := t.TempDir()
		images, times, e := mediaobserve.ExtractFrames(ctx, path, out, page.start, page.count, 1, 960)
		if e != nil || len(images) != page.count || len(times) != page.count {
			t.Fatal("frame page incomplete", len(images), e)
		}
		for _, at := range times {
			if at <= last {
				t.Fatal("frame timestamp repeated", at, last)
			}
			last = at
			total++
		}
	}
	if total != meta.Frames {
		t.Fatal("frames skipped", total)
	}
	for _, step := range []int{1, 7, int(^uint(0) >> 1)} {
		if n := mediaobserve.RemainingFrames(24, 0, step); n < 1 || n > 24 {
			t.Fatal("overflow", n)
		}
	}
}

func TestVideoInspectionUsesScopedCacheAndAdvertisesContinuation(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	f.manager.config.Behavior.ReadVideos = true
	f.manager.config.Companion.Permissions.Remember = true
	a, e := f.manager.rememberAssetFormats(p.trace, assetOriginal{Data: inspectionFixture(t), MIME: "video/mp4", Filename: "synthetic.mp4"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"action":"info","asset_id":%q}`, a.ID))
	value, e := p.Call(context.Background(), "video_inspect", raw)
	var out struct{ Metadata videoMetadata }
	if e != nil || json.Unmarshal([]byte(value), &out) != nil || out.Metadata.Frames != 24 {
		t.Fatal(value, e)
	}
	// Omitted action must pass the advertised schema and execute the safe
	// metadata default, not fail before reaching the tool on its first call.
	value, e = p.Call(context.Background(), "video_inspect", json.RawMessage(fmt.Sprintf(`{"asset_id":%q}`, a.ID)))
	if e != nil || !strings.Contains(value, `"action":"info"`) || !strings.Contains(value, `"total_frames":24`) {
		t.Fatal("default info call failed", value, e)
	}
	if _, e = p.Call(context.Background(), "video_inspect", json.RawMessage(fmt.Sprintf(`{"action":"guess","asset_id":%q}`, a.ID))); e == nil {
		t.Fatal("explicit invalid action accepted")
	}
	p.trace.Thread = "filehelper"
	if _, e = p.Call(context.Background(), "video_inspect", raw); e == nil {
		t.Fatal("cross-conversation video exposed")
	}
	f.manager.config.Behavior.ReadVideos = false
	if p.enabled("video_inspect") {
		t.Fatal("closed video permission ignored")
	}
}

// Frame inspection is a visual-media operation.  A chat-model key must never
// revive the removed direct VisionWithBudget path when the media package is not
// installed and authorized.
func TestVideoInspectionFramesRequireInstalledMediaPlugin(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	f.manager.config.Behavior.ReadVideos = true
	f.manager.config.Companion.Permissions.Remember = true
	f.manager.config.APIKey = "would-have-enabled-the-legacy-chat-model"
	asset, err := f.manager.rememberAssetFormats(p.trace, assetOriginal{Data: inspectionFixture(t), MIME: "video/mp4", Filename: "plugin-only.mp4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "plugin-only.mp4")
	if err = os.WriteFile(path, inspectionFixture(t), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = p.inspectVideoFrameRange(t.Context(), path, dir, videoInspectionInput{SourceThread: p.trace.Thread, Start: 0, Step: 1, BatchSize: 1, MaxDimension: 96, Tokens: 128}, 1, 24, asset.ID, inspectionFixture(t))
	if err == nil || !strings.Contains(err.Error(), "media_plugin_migration_required") {
		t.Fatalf("frame inspection used a legacy model path or hid the package boundary: %v", err)
	}
}

func TestVideoInspectionPublicURLRequiresInstalledSharePlugin(t *testing.T) {
	f, p := trustedAgent(t)
	defer p.hub.Close()
	f.manager.config.Behavior.ReadVideos = true
	p.settings.Enabled, p.settings.WebFetchEnabled = true, true
	in := videoInspectionInput{URL: "https://public.example/video"}
	if _, _, err := p.inspectionSource(t.Context(), &in); err == nil || !strings.Contains(err.Error(), "share_plugin_migration_required") {
		t.Fatal("public URL used a retired network reader", err)
	}
}

func TestInspectionPluginAudioUsesCanonicalReceivedAssetSource(t *testing.T) {
	f, p, n := forwardFixture(t)
	f.manager.config.Behavior.ReadVideos = true
	n.message = linuxnative.Message{ID: "canonical-video", AccountID: "test", ThreadID: "source@chatroom", SenderID: "source-member", ServerID: "video-server", CreatedAt: "2026-10-30T12:00:00Z", Direction: "incoming", Type: 43}
	trace := companion.Input{Channel: "wechat", Account: "test", Thread: "source@chatroom", TraceID: n.message.ID, Sender: n.message.SenderID, MessageTime: n.message.CreatedAt}
	asset, err := f.manager.rememberAssetFormats(trace, assetOriginal{InboundMessage: &n.message, Data: inspectionFixture(t), MIME: "video/mp4", Filename: "canonical.mp4"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := p.inspectionTranscriptionMessage(context.Background(), videoInspectionInput{Asset: asset.ID, SourceThread: "source@chatroom"})
	if err != nil || message.ID != n.message.ID || message.ThreadID != "source@chatroom" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	// An asset is not enough if its original native source is no longer a
	// received video/share. The adapter must not manufacture a new source.
	n.message.Direction = "outgoing"
	if _, err = p.inspectionTranscriptionMessage(context.Background(), videoInspectionInput{Asset: asset.ID, SourceThread: "source@chatroom"}); err == nil {
		t.Fatal("outgoing source accepted for plugin ASR")
	}
	n.message.Direction = "incoming"
	if _, err = p.inspectionTranscriptionMessage(context.Background(), videoInspectionInput{Asset: asset.ID}); err == nil {
		t.Fatal("missing canonical source scope accepted")
	}
}
