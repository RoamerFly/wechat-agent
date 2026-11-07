//go:build windows

package station

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

func liveProgramArtifact(t *testing.T, f *localHomeFixture, data []byte, name string) localArtifact {
	t.Helper()
	_, home := f.request("GET", "/api/local/home", nil, false)
	if err := os.WriteFile(filepath.Join(home["directory"].(string), "works", name), data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := f.handler.home.artifacts.publish(t.Context(), localArtifactInput{Path: "works/" + name, Key: name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func assertLiveProgramClean(t *testing.T, f *localHomeFixture) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(f.dir, "live-program-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("private program snapshot leaked: %v %v", files, err)
	}
}

func TestLocalLiveProgramSnapshotProbeAndRevocation(t *testing.T) {
	f := localChatFixture(t)
	data := syntheticDouyinAudioMedia(t, true)
	a := liveProgramArtifact(t, f, data, "program.mp4")
	ref := apphost.LiveReference{ID: "synthetic-session", ProgramArtifactID: a.ID, ProgramSHA256: a.SHA256}
	programs := f.handler.chat.livePrograms
	if err := programs.check(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	assertLiveProgramClean(t, f)
	// An edited home original does not change the selected immutable artifact.
	_, home := f.request("GET", "/api/local/home", nil, false)
	if err := os.WriteFile(filepath.Join(home["directory"].(string), "works/program.mp4"), []byte("edited home file"), 0600); err != nil {
		t.Fatal(err)
	}
	media, err := programs.open(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(media.Source)
	if err != nil || !bytes.Equal(actual, data) || media.Width != 64 || media.Height != 48 || media.SHA256 != a.SHA256 || media.Bytes != int64(len(data)) {
		t.Fatal("wrong immutable program", err, media)
	}
	if err := media.Source.Close(); err != nil {
		t.Fatal(err)
	}
	assertLiveProgramClean(t, f)
	bad := ref
	bad.ProgramSHA256 = strings.Repeat("0", 64)
	if !errors.Is(programs.check(t.Context(), bad), errLiveProgramSource) {
		t.Fatal("wrong hash accepted")
	}
	late, err := programs.open(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.request("POST", "/api/local/home", map[string]any{"action": "pause", "revision": home["revision"]}, true); code != 200 {
		t.Fatal("pause", code)
	}
	if _, err := io.ReadAll(late.Source); !errors.Is(err, errLiveProgramSource) {
		t.Fatal("copy completed after source grant was revoked", err)
	}
	if err := late.Source.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(programs.check(t.Context(), ref), errLiveProgramSource) {
		t.Fatal("cached program bypassed revoked home")
	}
	assertLiveProgramClean(t, f)
}

func TestLocalLiveProgramRejectsFakeContainerAndChangedOriginal(t *testing.T) {
	f := localChatFixture(t)
	a := liveProgramArtifact(t, f, []byte("not an actual video"), "fake.mp4")
	ref := apphost.LiveReference{ProgramArtifactID: a.ID, ProgramSHA256: a.SHA256}
	if !errors.Is(f.handler.chat.livePrograms.check(t.Context(), ref), errLiveProgramFormat) {
		t.Fatal("extension accepted as video evidence")
	}
	assertLiveProgramClean(t, f)
	if err := os.WriteFile(filepath.Join(f.dir, "artifacts", a.ID, "original"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(f.handler.chat.livePrograms.check(t.Context(), ref), errLiveProgramSource) {
		t.Fatal("tampered immutable snapshot accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(f.handler.chat.livePrograms.check(ctx, ref), context.Canceled) {
		t.Fatal("cancelled preflight continued")
	}
	assertLiveProgramClean(t, f)
}

type localProgramTestOutput struct{ programs *localLivePrograms }

func (o localProgramTestOutput) CheckLiveOutput(ctx context.Context, ref apphost.LiveReference) error {
	return o.programs.check(ctx, ref)
}
func (o localProgramTestOutput) StopLiveOutput(context.Context, apphost.LiveReference) error {
	return nil
}

func TestLocalLiveProgramNormalChatAndForgottenSource(t *testing.T) {
	f := localChatFixture(t)
	p := installLocalLiveToolFixture(t, f)
	identity, _ := f.handler.home.identity.Read()
	host, _ := f.handler.chat.projectTools.forFish(identity.FishID)
	f.handler.chat.live = apphost.NewLiveController(f.handler.chat.applications, host.store, localProgramTestOutput{f.handler.chat.livePrograms})
	_, home := f.request("GET", "/api/local/home", nil, false)
	data := syntheticDouyinAudioMedia(t, true)
	if err := os.WriteFile(filepath.Join(home["directory"].(string), "works/chat-program.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var artifact string
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, _ []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall(localArtifactPublishTool, map[string]any{"path": "works/chat-program.mp4"}), nil
			case 2:
				ids := localPublicationWorkIDs(localModelHistory(turns))
				if len(ids) != 1 {
					t.Fatalf("missing program artifact: %v", ids)
				}
				artifact = ids[0]
				return localToolCall("discover_tools", map[string]any{"query": "app_live"}), nil
			case 3, 4:
				args := localLiveTestStart()
				args["program_artifact_id"], args["program_sha256"], args["program_loop"] = artifact, sha256Hex(data), true
				return localToolCall(localLiveToolName, args), nil
			default:
				if !strings.Contains(localModelHistory(turns), `\"program_artifact_id\":\"`+artifact) {
					t.Fatal("program reference missing from receipt")
				}
				return `{"action":"reply","messages":["合成节目任务已启动。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "live-program-chat", "把这份成品循环播放60秒"); code != 200 || out["status"] != "completed" || p.starts.Load() != 1 || p.ref.ProgramArtifactID != artifact || !p.ref.ProgramLoop {
		t.Fatalf("program chat %d %#v starts=%d ref=%+v", code, out, p.starts.Load(), p.ref)
	}
	if code, _ := localChatSend(f, "live-program-chat", "把这份成品循环播放60秒"); code != 200 || calls != 5 || p.starts.Load() != 1 {
		t.Fatal("program chat replay duplicated start", code, calls)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(companion.ScopeID("local", in.Account, in.Thread)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(f.handler.chat.livePrograms.check(t.Context(), p.ref), errLiveProgramSource) {
		t.Fatal("forgotten program remained available")
	}
	view, err := f.handler.chat.live.Reconcile(t.Context(), identity.LocalOwner.ID, p.ref.ID)
	if err != nil || view.StopReason != "output-unavailable" || (view.State != "stopping" && view.State != "ended") {
		t.Fatal("forgotten source did not stop original programme", view, err)
	}
	if _, err := f.handler.chat.live.Stop(t.Context(), identity.LocalOwner.ID, p.ref.ID); err != nil {
		t.Fatal(err)
	}
	assertLiveProgramClean(t, f)
}
