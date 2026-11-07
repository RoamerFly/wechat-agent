//go:build windows

package station

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/google/uuid"

	"wechat-bot/internal/apphost"
)

func TestLocalLiveInvalidEncoderLeavesTargetsUnavailable(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "station")
	if err := os.MkdirAll(filepath.Join(data, "managed-obs", "studio"), 0700); err != nil {
		t.Fatal(err)
	}
	f := newLocalHomeFixture(t, data, filepath.Join(base, "workspace"))
	f.login()
	if code, out := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize: %d %#v", code, out)
	}
	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK ||
		out["encoder"] != "invalid" || out["available"] != false {
		t.Fatalf("invalid portable runtime should not expose targets: %d %#v", code, out)
	}
}

func realOBSFixture(t *testing.T) string {
	t.Helper()
	data := os.Getenv("AHS_TEST_PORTABLE_OBS_FIXTURE")
	if data == "" {
		t.Skip("set AHS_TEST_PORTABLE_OBS_FIXTURE to an isolated installed station fixture")
	}
	if !filepath.IsAbs(data) || filepath.Base(data) != "real-obs-fixture" {
		t.Fatal("expected a dedicated fixture directory named real-obs-fixture")
	}
	return data
}

func TestLocalLiveVerifiedEncoderDoesNotImplyAPlatformGrant(t *testing.T) {
	data := realOBSFixture(t)
	f := newLocalHomeFixture(t, data, t.TempDir())
	f.login()
	if code, out := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize: %d %#v", code, out)
	}
	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK ||
		out["encoder"] != "ready" || out["available"] != false || out["active"] != nil {
		t.Fatalf("verified local encoder must still wait for a platform grant: %d %#v", code, out)
	}
}

type localPortableOBSProvider struct {
	localLiveRouteProvider
	target *url.URL
	starts atomic.Int32
}

func (p *localPortableOBSProvider) StartLive(_ context.Context, ref apphost.LiveReference) (apphost.LiveStartResult, error) {
	p.starts.Add(1)
	return apphost.LiveStartResult{
		Receipt: apphost.LiveReceipt{Reference: ref, State: "live", RoomID: "room-1"},
		Ingest:  &apphost.LiveIngest{Destination: p.target, DialContext: (&net.Dialer{}).DialContext},
	}, nil
}

func TestLocalLivePortableOBSProgramReachesLoopbackThroughOwnerRoute(t *testing.T) {
	data := realOBSFixture(t)
	sink, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	received := make(chan error, 1)
	go receivePortableProgram(sink, received)

	provider := &localPortableOBSProvider{
		localLiveRouteProvider: localLiveRouteProvider{connection: apphost.Connection{
			ID: "broadcast", AccountID: "synthetic-owner", Label: "合成直播接收端",
		}},
		target: &url.URL{Scheme: "rtmp", Host: sink.Addr().String(), Path: "/fixture", Fragment: "receiver-test-key"},
	}
	registry, err := apphost.New(apphost.Registration{Application: apphost.Application{
		ID: "synthetic-live", Name: "合成平台", InstallationID: "synthetic-install", Registration: "synthetic-v1",
	}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	handler, err := NewLocalHomeHandlerWithApps(data, root, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	f := newLocalAppObservationFixtureWithHandler(t, handler, data, root)
	f.login()
	if code, out := f.request(http.MethodPost, "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize: %d %#v", code, out)
	}
	if code, out := f.request(http.MethodGet, "/api/local/live", nil, false); code != http.StatusOK ||
		out["encoder"] != "ready" || out["available"] != true {
		t.Fatalf("authorized synthetic target unavailable: %d %#v", code, out)
	}
	request := map[string]any{"action": "start", "app_id": "synthetic-live", "connection_id": "broadcast", "request_key": "portable-obs-fixture-" + uuid.NewString(), "duration_seconds": 60}
	code, started := f.request(http.MethodPost, "/api/local/live", request, true)
	if code != http.StatusOK || started["state"] != "live" || started["room_id"] != "room-1" {
		t.Fatalf("start: %d %#v", code, started)
	}
	id, _ := started["id"].(string)
	if id == "" {
		t.Fatalf("missing session: %#v", started)
	}
	select {
	case err := <-received:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("OBS did not publish local H.264 video")
	}
	for deadline := time.Now().Add(time.Second); ; {
		code, view := f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "status", "id": id}, true)
		output, _ := view["local_output"].(map[string]any)
		if code == http.StatusOK && output["state"] == "forwarding" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local media state: %d %#v", code, view)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, replay := f.request(http.MethodPost, "/api/local/live", request, true); code != http.StatusOK ||
		replay["id"] != id || provider.starts.Load() != 1 {
		t.Fatalf("replay created another broadcast: %d %#v", code, replay)
	}
	code, stopped := f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "stop", "id": id}, true)
	for deadline := time.Now().Add(3 * time.Second); code == http.StatusOK && stopped["state"] == "stopping" && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		code, stopped = f.request(http.MethodPost, "/api/local/live", map[string]string{"action": "reconcile", "id": id}, true)
	}
	if code != http.StatusOK || stopped["state"] != "ended" || stopped["local_stopped"] != true || stopped["remote_ended"] != true {
		t.Fatalf("stop: %d %#v", code, stopped)
	}
}

func receivePortableProgram(sink net.Listener, received chan<- error) {
	socket, err := sink.Accept()
	if err != nil {
		received <- err
		return
	}
	defer socket.Close()
	_ = socket.SetDeadline(time.Now().Add(30 * time.Second))
	receiver := &gortmplib.ServerConn{RW: socket}
	if err = receiver.Initialize(); err != nil {
		received <- err
		return
	}
	if err = receiver.AcceptConn(); err != nil {
		received <- err
		return
	}
	if !receiver.Publish || receiver.URL.Path != "/fixture/receiver-test-key" {
		received <- errors.New("wrong synthetic receiver stream")
		return
	}
	if err = receiver.AcceptAction(); err != nil {
		received <- err
		return
	}
	confirmed := false
	for {
		msg, err := receiver.Read()
		if err != nil {
			if !confirmed {
				received <- err
			}
			return
		}
		video, ok := msg.(*message.Video)
		if !confirmed && ok && video.Codec == message.CodecH264 && video.PacketType == message.VideoPacketTypeAU && len(video.AU) > 0 {
			confirmed = true
			received <- nil
		}
	}
}
