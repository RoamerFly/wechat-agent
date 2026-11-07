package rtmprelay

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// rtmpSink is a deliberately tiny RTMP publisher endpoint. It exercises the
// relay over real loopback TCP without depending on an RTMP server binary.
type rtmpSink struct {
	ln        net.Listener
	published chan *gortmplib.ServerConn
	messages  chan message.Message
	done      chan struct{}

	closeAfterMedia bool
	closeOnce       sync.Once
}

func newRTMPSink(t *testing.T, closeAfterMedia bool) *rtmpSink {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &rtmpSink{
		ln:              ln,
		published:       make(chan *gortmplib.ServerConn, 1),
		messages:        make(chan message.Message, 8),
		done:            make(chan struct{}),
		closeAfterMedia: closeAfterMedia,
	}
	go s.serve()
	t.Cleanup(s.Close)
	return s
}

func (s *rtmpSink) serve() {
	defer close(s.done)
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	sc := &gortmplib.ServerConn{RW: conn}
	if err := sc.Initialize(); err != nil {
		return
	}
	if err := sc.AcceptConn(); err != nil {
		return
	}
	if !sc.Publish || sc.AcceptAction() != nil {
		return
	}
	s.published <- sc

	for {
		msg, err := sc.Read()
		if err != nil {
			return
		}
		s.messages <- msg
		if s.closeAfterMedia {
			s.closeOnce.Do(func() { _ = conn.Close() })
			return
		}
	}
}

func (s *rtmpSink) Close() {
	s.closeOnce.Do(func() { _ = s.ln.Close() })
	select {
	case <-s.done:
	case <-time.After(time.Second):
	}
}

func (s *rtmpSink) destinationURL(t *testing.T, path, key string) *url.URL {
	t.Helper()
	u, err := url.Parse("rtmp://" + s.ln.Addr().String() + path + "#" + key)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func openRelay(t *testing.T, destination *url.URL, configure func(*Config)) *Relay {
	t.Helper()
	cfg := Config{
		Destination: destination,
		DialContext: (&net.Dialer{}).DialContext,
		VerifyPeer: func(context.Context, *net.TCPConn) error {
			return nil
		},
	}
	if configure != nil {
		configure(&cfg)
	}
	relay, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = relay.Close(ctx)
	})
	return relay
}

func publisherURL(t *testing.T, relay *Relay, key string) *url.URL {
	t.Helper()
	u, err := url.Parse("rtmp://127.0.0.1:" + itoa(relay.Port()) + "/live#" + key)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func itoa(v int) string {
	// Avoid widening production dependencies merely for test URL formatting.
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	b := make([]byte, 0, 6)
	for v > 0 {
		b = append([]byte{digits[v%10]}, b...)
		v /= 10
	}
	return string(b)
}

func openPublisher(t *testing.T, u *url.URL) *gortmplib.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := &gortmplib.Client{URL: u, Publish: true}
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func waitStatus(t *testing.T, relay *Relay, want string, minMedia uint64) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status := relay.Status()
		if status.State == want && status.MediaMessages >= minMedia {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("relay did not reach state %q with at least %d media messages (last: %+v)", want, minMedia, relay.Status())
	return Status{}
}

func TestRelayForwardsMediaAndSeparatesKeys(t *testing.T) {
	sink := newRTMPSink(t, false)
	const destinationKey = "destination-only-key"
	relay := openRelay(t, sink.destinationURL(t, "/ingest", destinationKey), nil)
	if status := relay.Status(); status.State != "waiting" || status.MediaMessages != 0 {
		t.Fatalf("unexpected initial status: %+v", status)
	}

	publisher := openPublisher(t, publisherURL(t, relay, relay.Ticket()))
	if err := publisher.Write(&message.DataAMF0{
		ChunkStreamID: 5, MessageStreamID: 0x1000000, Payload: []any{"onMetaData"},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-sink.messages:
		if _, ok := msg.(*message.DataAMF0); !ok {
			t.Fatalf("expected metadata, got %T", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata did not reach the receiver")
	}
	if status := relay.Status(); status.State != "connecting" || status.MediaMessages != 0 {
		t.Fatalf("metadata alone counted as media: %+v", status)
	}
	if err := publisher.Write(&message.Audio{
		ChunkStreamID: message.AudioChunkStreamID, MessageStreamID: 1,
		Codec: message.CodecMPEG4Audio, Rate: message.AudioRate44100, Depth: message.AudioDepth16,
		IsStereo: true, AACType: message.AudioAACTypeAU, AU: []byte("synthetic-aac-payload"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Write(&message.Video{
		ChunkStreamID: message.VideoChunkStreamID, MessageStreamID: 1, DTS: 40 * time.Millisecond,
		FrameType: message.VideoFrameTypeKeyFrame, Codec: message.CodecH264,
		PacketType: message.VideoPacketTypeAU, AU: []byte{0, 0, 0, 1, 0x65, 0x88, 0x84},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case destination := <-sink.published:
		if got, want := destination.URL.Path, "/ingest/"+destinationKey; got != want {
			t.Fatalf("destination stream key was not used independently: got path %q, want %q", got, want)
		}
		if strings.Contains(destination.URL.String(), relay.Ticket()) {
			t.Fatalf("publisher ticket leaked to destination URL: %q", destination.URL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("destination did not receive an RTMP publish session")
	}

	seenAudio, seenVideo := false, false
	deadline := time.After(2 * time.Second)
	for !(seenAudio && seenVideo) {
		select {
		case msg := <-sink.messages:
			switch msg := msg.(type) {
			case *message.Audio:
				seenAudio = string(msg.AU) == "synthetic-aac-payload"
			case *message.Video:
				seenVideo = len(msg.AU) > 0 && msg.AU[len(msg.AU)-1] == 0x84
			}
		case <-deadline:
			t.Fatalf("did not receive both synthetic audio and video (audio=%v video=%v)", seenAudio, seenVideo)
		}
	}
	// The receiver may run before the relay's Write returns, and forwarding
	// starts with the first media write. Wait for both completion records.
	if status := waitStatus(t, relay, "forwarding", 2); status.MediaMessages != 2 {
		t.Fatalf("forwarding status counted unexpected media messages: %+v", status)
	}
}

func TestRelayRejectsWrongOrUnverifiedPublishBeforeOutboundDial(t *testing.T) {
	for _, tc := range []struct {
		name      string
		publisher string
		verify    func(context.Context, *net.TCPConn) error
	}{
		{name: "wrong key", publisher: "not-the-ticket"},
		{name: "unverified peer", publisher: "unused", verify: func(context.Context, *net.TCPConn) error {
			return errors.New("synthetic peer verification rejection")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials atomic.Uint32
			destination, err := url.Parse("rtmp://127.0.0.1:1/never#dialed")
			if err != nil {
				t.Fatal(err)
			}
			relay := openRelay(t, destination, func(cfg *Config) {
				cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("outbound dial must not happen")
				}
				if tc.verify != nil {
					cfg.VerifyPeer = tc.verify
				}
			})
			key := tc.publisher
			if tc.name == "wrong key" {
				key = tc.publisher
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client := &gortmplib.Client{URL: publisherURL(t, relay, key), Publish: true}
			if err := client.Initialize(ctx); err == nil {
				client.Close()
				t.Fatal("invalid publisher was accepted")
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("invalid publisher caused %d outbound dial(s)", got)
			}
			if status := relay.Status(); status.State != "waiting" {
				t.Fatalf("rejected publisher changed terminal relay state: %+v", status)
			}
		})
	}
}

func TestRelayReceiverDisconnectStopsWithoutReconnect(t *testing.T) {
	sink := newRTMPSink(t, true)
	var dials atomic.Uint32
	relay := openRelay(t, sink.destinationURL(t, "/out", "receiver-key"), func(cfg *Config) {
		base := (&net.Dialer{}).DialContext
		cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return base(ctx, network, address)
		}
	})
	publisher := openPublisher(t, publisherURL(t, relay, relay.Ticket()))
	if err := publisher.Write(&message.Audio{
		ChunkStreamID: message.AudioChunkStreamID, MessageStreamID: 1,
		Codec: message.CodecPCMU, Rate: message.AudioRate44100, Depth: message.AudioDepth16,
		AU: []byte("close-the-receiver"),
	}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, relay, "disconnected", 0)
	got := dials.Load()
	time.Sleep(150 * time.Millisecond)
	if later := dials.Load(); later != got || got != 1 {
		t.Fatalf("receiver disconnect must not reconnect (initial=%d later=%d)", got, later)
	}
}

func TestRelayCloseInterruptsStalledHandshakeWithoutLateDial(t *testing.T) {
	var dials atomic.Uint32
	destination, err := url.Parse("rtmp://127.0.0.1:1/out#never")
	if err != nil {
		t.Fatal(err)
	}
	relay := openRelay(t, destination, func(cfg *Config) {
		cfg.DialContext = func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("stalled publisher must not create destination dial")
		}
	})
	stalled, err := net.Dial("tcp", "127.0.0.1:"+itoa(relay.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := relay.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if status := relay.Status(); status.State != "stopped" {
		t.Fatalf("close did not reach stopped state: %+v", status)
	}
	time.Sleep(150 * time.Millisecond)
	if got := dials.Load(); got != 0 {
		t.Fatalf("close allowed a late outbound dial (%d)", got)
	}
}
