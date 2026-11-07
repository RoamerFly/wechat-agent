//go:build windows

package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/stationbrowser"
)

func syntheticDouyinAudioMedia(t *testing.T, withAudio bool) []byte {
	t.Helper()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable")
		}
	}
	file := filepath.Join(t.TempDir(), "synthetic.mp4")
	args := []string{"-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x48:r=5:d=3"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=f=800:r=16000:d=3")
	}
	args = append(args, "-c:v", "mpeg4", "-threads", "1")
	if withAudio {
		args = append(args, "-c:a", "aac")
	}
	args = append(args, file)
	cmd := exec.Command("ffmpeg", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthetic media: %v %.200s", err, out)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func setSyntheticDouyinWebAudio(t *testing.T, f *localHomeFixture, view map[string]any, allow bool) map[string]any {
	t.Helper()
	code, next := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_audio": allow}, true)
	if code != 200 || next["audio_enabled"] != allow {
		t.Fatalf("audio permission: %d %v", code, next)
	}
	return next
}

func syntheticDouyinWebAudioInput(id string, limit int) map[string]any {
	in := syntheticDouyinWebPlaybackInput(id, limit)
	in["field"] = "audio"
	return in
}

func TestLocalDouyinWebAudioNormalEntryDecodesPagesAndInvalidatesASR(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	if view["audio_enabled"] != false {
		t.Fatal("login granted audio")
	}
	version := "synthetic-asr-v1"
	reads, recognitions := 0, 0
	m.audio = stationLocalASR{version: func(context.Context) (string, error) { return version, nil }, transcribe: func(_ context.Context, wav []byte, check func() error) (string, error) {
		recognitions++
		if len(wav) < 44 || string(wav[:4]) != "RIFF" {
			t.Error("ASR did not receive decoded WAV")
		}
		if err := check(); err != nil {
			return "", err
		}
		return "本机合成转写内容。", nil
	}}
	view = setSyntheticDouyinWebScope(t, f, view, "home_and_videos")
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	input := syntheticDouyinWebAudioInput(id, 1)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 {
		t.Fatal("text granted audio", code)
	}
	view = setSyntheticDouyinWebAudio(t, f, view, true)
	if view["playback_enabled"] != false {
		t.Fatal("audio silently enabled playback")
	}
	id = syntheticDouyinWebPlaybackTarget(t, f, m)
	input["content_id"] = id
	media := syntheticDouyinAudioMedia(t, true)
	m.readMedia = func(ctx context.Context, _, videoID string, check func() error) (stationbrowser.DouyinWebMedia, error) {
		reads++
		if videoID != "7000000000000000011" {
			t.Error("read another source")
		}
		if err := check(); err != nil {
			return stationbrowser.DouyinWebMedia{}, err
		}
		return stationbrowser.DouyinWebMedia{Data: media, ContentType: "video/mp4"}, nil
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebAudioInput(id, 241), true); code != 400 || reads != 0 {
		t.Fatal("oversized page downloaded media", code, reads)
	}
	code, first := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || first["complete"] != false || first["audio_proof"].(map[string]any)["start_seconds"] != float64(0) || reads != 1 || recognitions != 1 {
		t.Fatalf("first: %d %v %d/%d", code, first, reads, recognitions)
	}
	input["limit"] = 3
	code, last := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || last["complete"] != true || last["resumed"] != true || last["audio_proof"].(map[string]any)["start_seconds"] != float64(1) || reads != 1 || recognitions != 2 {
		t.Fatalf("continuation: %d %v %d/%d", code, last, reads, recognitions)
	}
	if last["playback_proof"] != nil {
		t.Fatal("transcription fabricated viewing")
	}
	raw, _ := json.Marshal(last)
	if strings.Contains(string(raw), f.dir) || strings.Contains(string(raw), "Cookies") {
		t.Fatal("private path leaked")
	}
	_, cached := f.request("POST", "/api/local/apps/observation", input, true)
	if cached["cached"] != true || recognitions != 2 {
		t.Fatal("completed recognition unnecessarily repeated")
	}
	version = "synthetic-asr-v2"
	code, newModel := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || newModel["cached"] != false || newModel["audio_proof"].(map[string]any)["start_seconds"] != float64(0) || reads != 1 || recognitions != 3 {
		t.Fatalf("new ASR reused old transcript or redownloaded: %d %v", code, newModel)
	}
	input["refresh"] = true
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 200 || reads != 2 {
		t.Fatal("explicit refresh did not fetch new media", code, reads)
	}
	view = setSyntheticDouyinWebAudio(t, f, view, false)
	delete(input, "refresh")
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 {
		t.Fatal("revoked audio reused old transcript", code)
	}
}

func TestLocalDouyinWebAudioEmptyTrackNoSpeechAndFailureAreDifferent(t *testing.T) {
	for _, test := range []struct {
		name     string
		audio    bool
		asrError error
		state    string
		status   int
	}{
		{"without-track", false, nil, "no_audio_track", 200},
		{"without-speech", true, &mediaspec.Failure{Code: "ASR_RESPONSE_HAVE_NO_WORDS"}, "no_recognizable_speech", 200},
		{"service-failed", true, errors.New("synthetic-private-service-path"), "", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, m := newLocalDouyinWebFixture(t)
			view := enableSyntheticDouyinWebRead(t, f)
			m.audio = stationLocalASR{version: func(context.Context) (string, error) { return "synthetic", nil }, transcribe: func(context.Context, []byte, func() error) (string, error) {
				if !test.audio {
					t.Error("no-track video was submitted to ASR")
				}
				return "", test.asrError
			}}
			setSyntheticDouyinWebAudio(t, f, view, true)
			id := syntheticDouyinWebPlaybackTarget(t, f, m)
			media := syntheticDouyinAudioMedia(t, test.audio)
			m.readMedia = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error) {
				return stationbrowser.DouyinWebMedia{Data: media, ContentType: "video/mp4"}, nil
			}
			code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebAudioInput(id, 60), true)
			if code != test.status {
				t.Fatalf("wrong stage %d %v", code, result)
			}
			if code == 200 && result["audio_proof"].(map[string]any)["state"] != test.state {
				t.Fatal("silence became error", result)
			}
			raw, _ := json.Marshal(result)
			if strings.Contains(string(raw), "synthetic-private-service-path") {
				t.Fatal("private error leaked")
			}
		})
	}
}

func TestLocalDouyinWebAudioUnavailableDoesNotSaveGrantAndLateResultsDoNotEscape(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	code, _ := f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true, "read_scope": "home_and_videos", "allow_audio": true}, true)
	if code != 503 {
		t.Fatal("missing local ASR accepted", code)
	}
	_, unchanged := f.request("GET", localDouyinWebRoute, nil, false)
	if unchanged["revision"] != view["revision"] || unchanged["audio_enabled"] != false {
		t.Fatal("failed ASR admission changed grant")
	}
	m.audio = stationLocalASR{version: func(context.Context) (string, error) { return "synthetic", nil }, transcribe: func(context.Context, []byte, func() error) (string, error) { return "late successful words", nil }}
	view = setSyntheticDouyinWebAudio(t, f, view, true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	m.readMedia = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error) {
		setSyntheticDouyinWebAudio(t, f, view, false)
		return stationbrowser.DouyinWebMedia{Data: []byte("must not persist"), ContentType: "video/mp4"}, nil
	}
	code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebAudioInput(id, 1), true)
	if code != 403 || result["audio_proof"] != nil {
		t.Fatalf("revoked late media: %d %v", code, result)
	}
	if localDouyinWebFieldAllowed(localDouyinWebRecord{ReadScope: "home_and_videos", AudioEnabled: true}, "home", apphost.ObservationAudio) {
		t.Fatal("home granted audio")
	}
}

func TestLocalDouyinWebAudioContinuationRejectsChangedPrivateMedia(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	m.audio = stationLocalASR{version: func(context.Context) (string, error) { return "synthetic", nil }, transcribe: func(context.Context, []byte, func() error) (string, error) { return "正确原件的转写。", nil }}
	setSyntheticDouyinWebAudio(t, f, view, true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	media := syntheticDouyinAudioMedia(t, true)
	reads := 0
	m.readMedia = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error) {
		reads++
		return stationbrowser.DouyinWebMedia{Data: media, ContentType: "video/mp4"}, nil
	}
	input := syntheticDouyinWebAudioInput(id, 1)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 200 {
		t.Fatal(code)
	}
	paths, err := filepath.Glob(filepath.Join(f.dir, "apps", "douyin", "web-snapshots", "*", "*", "audio-*.media"))
	if err != nil || len(paths) != 1 {
		t.Fatal("missing scoped media", paths, err)
	}
	if err := os.WriteFile(paths[0], []byte("different media"), 0600); err != nil {
		t.Fatal(err)
	}
	code, result := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 503 || result["audio_proof"] != nil || reads != 1 {
		t.Fatalf("changed source escaped or silently redownloaded: %d %v reads=%d", code, result, reads)
	}
	if err := os.WriteFile(paths[0], media, 0600); err != nil {
		t.Fatal(err)
	}
	code, result = f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || result["audio_proof"].(map[string]any)["start_seconds"] != float64(1) || reads != 1 {
		t.Fatalf("failed read advanced original cursor: %d %v", code, result)
	}
}

func TestLocalDouyinWebAudioMediaHasHostDeadlineAndReleasesLockOnTimeout(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	m.audio = stationLocalASR{version: func(context.Context) (string, error) { return "synthetic", nil }, transcribe: func(context.Context, []byte, func() error) (string, error) {
		t.Fatal("timed out media reached ASR")
		return "", nil
	}}
	setSyntheticDouyinWebAudio(t, f, view, true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	m.readMedia = func(ctx context.Context, _, _ string, _ func() error) (stationbrowser.DouyinWebMedia, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 95*time.Second || time.Until(deadline) <= 0 {
			t.Error("host did not bound no-deadline caller")
		}
		return stationbrowser.DouyinWebMedia{}, context.DeadlineExceeded
	}
	for attempt := 0; attempt < 2; attempt++ {
		code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebAudioInput(id, 1), true)
		if code != 504 || result["audio_proof"] != nil {
			t.Fatalf("timeout retained lock or emitted evidence: %d %v", code, result)
		}
	}
	files, err := filepath.Glob(filepath.Join(f.dir, "apps", "douyin", "web-snapshots", "*", "*", "audio-*.media"))
	if err != nil || len(files) != 0 {
		t.Fatal("timeout committed partial original", files, err)
	}
}

func TestLocalDouyinWebAudioRestartContinuesSameOriginalWithoutDownload(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	asr := stationLocalASR{version: func(context.Context) (string, error) { return "synthetic-asr-v1", nil }, transcribe: func(context.Context, []byte, func() error) (string, error) {
		return "同一原件的合成转写。", nil
	}}
	m.audio = asr
	setSyntheticDouyinWebAudio(t, f, view, true)
	id := syntheticDouyinWebPlaybackTarget(t, f, m)
	media := syntheticDouyinAudioMedia(t, true)
	m.readMedia = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error) {
		return stationbrowser.DouyinWebMedia{Data: media, ContentType: "video/mp4"}, nil
	}
	input := syntheticDouyinWebAudioInput(id, 1)
	code, first := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || first["complete"] != false {
		t.Fatalf("first %d %v", code, first)
	}
	hash := first["audio_proof"].(map[string]any)["media_sha256"]
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(douyinapp.New(filepath.Join(f.dir, "apps", "douyin")).Registration())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(f.dir, f.root, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	next := newLocalAppObservationFixtureWithHandler(t, handler, f.dir, f.root)
	next.login()
	nextManager := handler.home.douyinWeb
	nextManager.available = func() bool { return true }
	nextManager.audio = asr
	nextManager.readMedia = func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error) {
		t.Error("restart downloaded a different original")
		return stationbrowser.DouyinWebMedia{}, errors.New("must use original")
	}
	input["limit"] = 3
	code, last := next.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || last["complete"] != true || last["resumed"] != true || last["audio_proof"].(map[string]any)["start_seconds"] != float64(1) || last["audio_proof"].(map[string]any)["media_sha256"] != hash {
		t.Fatalf("restart: %d %v", code, last)
	}
}
