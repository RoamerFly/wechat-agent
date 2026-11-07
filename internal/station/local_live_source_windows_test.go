//go:build windows

package station

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

type localSourceTestProvider struct {
	*localLiveToolProvider
	signal *localSourceSignal
}

func (*localSourceTestProvider) LiveMediaMode(apphost.ConnectionRef) string {
	return apphost.LiveMediaLocalSource
}

func (p *localSourceTestProvider) StartLive(ctx context.Context, ref apphost.LiveReference) (apphost.LiveStartResult, error) {
	if !p.signal.ready.Load() {
		return apphost.LiveStartResult{}, errors.New("synthetic platform cannot capture an unprepared signal")
	}
	return p.localLiveToolProvider.StartLive(ctx, ref)
}

type localSourceSignal struct {
	ready atomic.Bool
	stops atomic.Int32
}

func (s *localSourceSignal) Start(context.Context) error { s.ready.Store(true); return nil }
func (s *localSourceSignal) Check(context.Context) error {
	if !s.ready.Load() {
		return errors.New("synthetic signal stopped")
	}
	return nil
}
func (s *localSourceSignal) Stop(context.Context) error {
	s.stops.Add(1)
	s.ready.Store(false)
	return nil
}

func TestLocalLiveSourceNormalChatUsesOriginalProgramAndSettles(t *testing.T) {
	f := localChatFixture(t)
	p := installLocalLiveToolFixture(t, f)
	signal := &localSourceSignal{}
	entry, _ := f.handler.chat.applications.Lookup("synthetic-live")
	entry.Provider = &localSourceTestProvider{localLiveToolProvider: p, signal: signal}
	registry, err := apphost.New(entry)
	if err != nil {
		t.Fatal(err)
	}
	data := syntheticDouyinAudioMedia(t, true)
	artifact := liveProgramArtifact(t, f, data, "source-program.mp4")
	programs := f.handler.chat.livePrograms
	var launched atomic.Int32
	output := apphost.NewManagedLiveSource(func(ctx context.Context, ref apphost.LiveReference) (apphost.LiveLocalSource, error) {
		media, err := programs.open(ctx, ref)
		if err != nil {
			return nil, err
		}
		if media == nil {
			return nil, errLiveProgramSource
		}
		actual, readErr := io.ReadAll(media.Source)
		closeErr := media.Source.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(actual, data) {
			return nil, errors.New("program snapshot differs from the original request")
		}
		launched.Add(1)
		return signal, nil
	}, programs.check)
	h := f.handler.chat
	identity, _ := h.home.identity.Read()
	host, _ := h.projectTools.forFish(identity.FishID)
	h.applications = registry
	h.live = apphost.NewLiveControllerWithSource(registry, host.store, nil, output)
	calls := 0
	h.modelFactory = func() (companion.LanguageModel, error) {
		return &syntheticObservationModel{call: func(_ context.Context, _ string, turns []map[string]string, _ []mcptools.Tool) (string, error) {
			calls++
			switch calls {
			case 1:
				return localToolCall("discover_tools", map[string]string{"query": "app_live"}), nil
			case 2, 3:
				args := localLiveTestStart()
				args["program_artifact_id"], args["program_sha256"] = artifact.ID, artifact.SHA256
				return localToolCall(localLiveToolName, args), nil
			case 4:
				return localToolCall(localLiveToolName, map[string]string{"action": "stop", "id": p.ref.ID}), nil
			default:
				if !strings.Contains(localModelHistory(turns), `\"remote_ended\":true`) {
					t.Fatal("normal entry did not settle the original platform receipt")
				}
				return `{"action":"reply","messages":["合成节目已停止。"]}`, nil
			}
		}}, nil
	}
	if code, out := localChatSend(f, "source-normal", "使用指定节目完成合成开播后停止"); code != 200 || out["status"] != "completed" {
		t.Fatal(code, out)
	}
	if launched.Load() != 1 || p.starts.Load() != 1 || signal.stops.Load() != 1 || p.ref.MediaMode != apphost.LiveMediaLocalSource {
		t.Fatal("source or platform replayed", launched.Load(), p.starts.Load(), signal.stops.Load(), p.ref.MediaMode)
	}
	assertLiveProgramClean(t, f)
}
