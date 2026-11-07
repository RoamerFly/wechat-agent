//go:build windows

package obspreview

import (
	"bytes"
	"context"
	"flag"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/message"

	"wechat-bot/internal/rtmprelay"
)

var syntheticPublisherURL = flag.String("test-owned-publisher-url", "", "synthetic local relay URL")
var syntheticVideo = []byte{0, 0, 0, 3, 0x65, 0x88, 0x84}

func TestOwnedPublisherFixture(t *testing.T) {
	if *syntheticPublisherURL == "" {
		return
	}
	target, err := url.Parse(*syntheticPublisherURL)
	if err != nil || target.Scheme != "rtmp" || target.Hostname() != "127.0.0.1" {
		t.Fatal("fixture can only publish to the local test relay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	client := &gortmplib.Client{URL: target, Publish: true}
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Write(&message.Video{ChunkStreamID: message.VideoChunkStreamID,
		MessageStreamID: 0x1000000, FrameType: message.VideoFrameTypeKeyFrame,
		Codec: message.CodecH264, PacketType: message.VideoPacketTypeAU, AU: syntheticVideo}); err != nil {
		t.Fatal(err)
	}
	<-ctx.Done()
}

func TestOwnedWindowsEncoderCanPublishThroughRTMPRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	sink, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	result := make(chan []byte, 1)
	sinkDone := make(chan struct{})
	go func() {
		defer close(sinkDone)
		conn, err := sink.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
		server := &gortmplib.ServerConn{RW: conn}
		if server.Initialize() != nil || server.AcceptConn() != nil || !server.Publish ||
			server.URL.Path != "/test/fixture-sink-key" || server.AcceptAction() != nil {
			return
		}
		for {
			msg, err := server.Read()
			if err != nil {
				return
			}
			if video, ok := msg.(*message.Video); ok {
				result <- video.AU
				return
			}
		}
	}()

	ready := make(chan struct{})
	var encoder *EncoderProcess
	relay, err := rtmprelay.Open(ctx, rtmprelay.Config{
		Destination: &url.URL{Scheme: "rtmp", Host: sink.Addr().String(), Path: "/test", Fragment: "fixture-sink-key"},
		DialContext: (&net.Dialer{}).DialContext,
		VerifyPeer: func(ctx context.Context, conn *net.TCPConn) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ready:
				return encoder.VerifyPublisher(ctx, conn)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = relay.Close(closeCtx)
	}()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := encoderDigest(exe)
	if err != nil {
		t.Fatal(err)
	}
	ingress := &url.URL{Scheme: "rtmp", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(relay.Port())), Path: "/live", Fragment: relay.Ticket()}
	encoder, err = LaunchEncoder(ctx, EncoderLaunch{
		Executable: exe, SHA256: digest, Directory: t.TempDir(), ControlPort: reserveEncoderPort(t), ControlPassword: "fixture-password",
		Arguments: []string{"-test.run=^TestOwnedPublisherFixture$", "-test-owned-publisher-url=" + ingress.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	defer encoder.Stop(context.Background())

	select {
	case <-sinkDone:
		select {
		case payload := <-result:
			if !bytes.Equal(payload, syntheticVideo) {
				t.Fatalf("unexpected media payload %x", payload)
			}
		default:
			t.Fatal("approved receiver disconnected without receiving synthetic media")
		}
	case <-ctx.Done():
		t.Fatal("owned Windows publisher did not reach the approved receiver")
	}
}
