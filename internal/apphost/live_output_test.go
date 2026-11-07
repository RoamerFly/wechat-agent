package apphost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// relayLiveProvider adds the private, transient ingest route to the ordinary
// durable live-provider test double. It deliberately does not store it in the
// provider's state: the controller must not persist this route either.
type relayLiveProvider struct {
	*liveTestProvider
	ingest LiveIngest
}

func (p *relayLiveProvider) StartLive(ctx context.Context, ref LiveReference) (LiveStartResult, error) {
	result, err := p.liveTestProvider.StartLive(ctx, ref)
	if err != nil {
		return result, err
	}
	ingest := p.ingest
	result.Ingest = &ingest
	return result, nil
}

// relayTarget is a minimal in-process RTMP publish endpoint. It is intentionally
// loopback-only: the integration test proves that the relay forwards actual
// media, but cannot reach a platform or other network destination.
type relayTarget struct {
	listener net.Listener
	received chan *message.Audio
	errs     chan error
	done     chan struct{}
	once     sync.Once
}

func newRelayTarget(t *testing.T) *relayTarget {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := &relayTarget{
		listener: listener,
		received: make(chan *message.Audio, 4),
		errs:     make(chan error, 1),
		done:     make(chan struct{}),
	}
	go target.serve()
	t.Cleanup(target.Close)
	return target
}

func (t *relayTarget) serve() {
	defer close(t.done)
	conn, err := t.listener.Accept()
	if err != nil {
		if !errors.Is(err, net.ErrClosed) {
			t.fail(err)
		}
		return
	}
	defer conn.Close()
	server := &gortmplib.ServerConn{RW: conn}
	if err := server.Initialize(); err != nil {
		t.fail(err)
		return
	}
	if err := server.AcceptConn(); err != nil {
		t.fail(err)
		return
	}
	if !server.Publish {
		t.fail(errors.New("relay target expected a publishing client"))
		return
	}
	if err := server.AcceptAction(); err != nil {
		t.fail(err)
		return
	}
	for {
		msg, err := server.Read()
		if err != nil {
			return
		}
		if audio, ok := msg.(*message.Audio); ok {
			select {
			case t.received <- audio:
			default:
			}
		}
	}
}

func (t *relayTarget) fail(err error) {
	select {
	case t.errs <- err:
	default:
	}
}

func (t *relayTarget) destination() *url.URL {
	// The fragment is an intentionally private stream key. Neither a LiveView
	// nor the durable session payload may contain it.
	u, _ := url.Parse("rtmp://" + t.listener.Addr().String() + "/platform#private-stream-key")
	return u
}

func (t *relayTarget) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if address != t.listener.Addr().String() {
		return nil, fmt.Errorf("unexpected relay destination %q", address)
	}
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func (t *relayTarget) Close() {
	t.once.Do(func() {
		_ = t.listener.Close()
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
		}
	})
}

type syntheticRelayEncoder struct {
	mu sync.Mutex

	payload  []byte
	client   *gortmplib.Client
	starts   int
	stops    int
	verified int
	launched LiveReference
}

func (e *syntheticRelayEncoder) VerifyPublisher(context.Context, *net.TCPConn) error {
	e.mu.Lock()
	e.verified++
	e.mu.Unlock()
	return nil
}

func (e *syntheticRelayEncoder) Start(ctx context.Context, port int, ticket string) error {
	u, err := url.Parse(fmt.Sprintf("rtmp://127.0.0.1:%d/live/%s", port, ticket))
	if err != nil {
		return err
	}
	client := &gortmplib.Client{URL: u, Publish: true}
	if err := client.Initialize(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	e.client = client
	e.starts++
	e.mu.Unlock()
	if err := client.Write(&message.Audio{
		ChunkStreamID:   4,
		MessageStreamID: 1,
		Codec:           message.CodecMPEG1Audio,
		Rate:            message.AudioRate44100,
		Depth:           message.AudioDepth16,
		IsStereo:        true,
		AU:              append([]byte(nil), e.payload...),
	}); err != nil {
		client.Close()
		return err
	}
	return nil
}

func (e *syntheticRelayEncoder) Stop(context.Context) error {
	e.mu.Lock()
	e.stops++
	client := e.client
	e.client = nil
	e.mu.Unlock()
	if client != nil {
		client.Close()
	}
	return nil
}

func (e *syntheticRelayEncoder) counts() (starts, stops, verified int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts, e.stops, e.verified
}

func newRelayController(t *testing.T, f *liveTestFixture, output *ManagedLiveOutput, ingest LiveIngest) *relayLiveProvider {
	t.Helper()
	provider := &relayLiveProvider{liveTestProvider: f.provider, ingest: ingest}
	registry, err := New(Registration{
		Application: Application{ID: liveTestApp, Name: "合成直播", InstallationID: "synthetic-install", Registration: "synthetic-v1"},
		Provider:    provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.control = NewLiveController(registry, f.store, output)
	f.control.now = f.time
	return provider
}

func TestManagedLiveOutputRelaysMediaAndKeepsPrivateRouteTransient(t *testing.T) {
	f := newLiveTestFixture(t)
	target := newRelayTarget(t)
	encoder := &syntheticRelayEncoder{payload: []byte{0x51, 0x52, 0x53}}
	output := NewManagedLiveOutput(func(_ context.Context, ref LiveReference) (LiveEncoder, error) {
		encoder.mu.Lock()
		encoder.launched = ref
		encoder.mu.Unlock()
		return encoder, nil
	})
	newRelayController(t, f, output, LiveIngest{Destination: target.destination(), DialContext: target.dial})

	started := f.start("relay-normal", 60)
	if started.State != "live" || started.RoomID != "room-1" {
		t.Fatalf("unexpected platform receipt: %#v", started)
	}
	if started.LocalOutput == nil {
		t.Fatal("live view did not expose local output evidence")
	}

	var forwarded *message.Audio
	select {
	case forwarded = <-target.received:
	case err := <-target.errs:
		t.Fatalf("target handshake failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not forward synthetic audio")
	}
	if !bytes.Equal(forwarded.AU, encoder.payload) {
		t.Fatalf("forwarded payload = %x, want %x", forwarded.AU, encoder.payload)
	}

	current := awaitForwardingLive(t, f, started.ID)
	viewJSON, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, privatePart := range []string{"private-stream-key", target.listener.Addr().String(), "platform"} {
		if bytes.Contains(viewJSON, []byte(privatePart)) {
			t.Fatalf("private ingest route leaked into live view: %s", viewJSON)
		}
	}
	record, err := f.store.LiveSession(f.ctx, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, privatePart := range []string{"private-stream-key", target.listener.Addr().String(), "platform"} {
		if bytes.Contains(record.Payload, []byte(privatePart)) {
			t.Fatalf("private ingest route leaked into durable live session: %s", record.Payload)
		}
	}

	// A different reference cannot stop the running relay, even if it happens
	// to name the same application and connection.
	wrongRef := LiveReference{ID: "other-live", Target: currentLiveReference(t, f, started.ID).Target,
		Registration: "synthetic-v1", GrantVersion: "grant-v1", Deadline: time.Now().Add(time.Minute)}
	if err := output.StopLiveOutput(f.ctx, wrongRef); err != nil {
		t.Fatalf("unowned stop should be a no-op: %v", err)
	}
	stillLive, err := f.control.Status(f.ctx, liveTestOwner, started.ID)
	if err != nil || stillLive.LocalOutput == nil || stillLive.LocalOutput.State != "forwarding" {
		t.Fatalf("unowned stop affected live session: %#v %v", stillLive, err)
	}

	stopped, err := f.control.Stop(f.ctx, liveTestOwner, started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "ended" || !stopped.LocalStopped || !stopped.RemoteEnded {
		t.Fatalf("owner stop did not settle both transports: %#v", stopped)
	}
	if starts, stops, verified := encoder.counts(); starts != 1 || stops != 1 || verified != 1 {
		t.Fatalf("encoder lifecycle = starts:%d stops:%d verified:%d, want 1:1:1", starts, stops, verified)
	}
	if starts, _, _ := f.provider.calls(); starts != 1 {
		t.Fatalf("platform StartLive calls = %d, want exactly one", starts)
	}
	if replay := f.start("relay-normal", 60); replay.ID != started.ID {
		t.Fatalf("request replay created another session: %#v", replay)
	}
	if starts, _, _ := f.provider.calls(); starts != 1 {
		t.Fatalf("request replay called platform StartLive again: %d", starts)
	}
}

func currentLiveReference(t *testing.T, f *liveTestFixture, id string) LiveReference {
	t.Helper()
	record, err := f.store.LiveSession(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeLive(record)
	if err != nil {
		t.Fatal(err)
	}
	return state.Reference
}

func awaitForwardingLive(t *testing.T, f *liveTestFixture, id string) LiveView {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		current, err := f.control.Status(f.ctx, liveTestOwner, id)
		if err == nil && current.LocalOutput != nil && current.LocalOutput.State == "forwarding" && current.LocalOutput.MediaMessages > 0 {
			return current
		}
		select {
		case <-deadline.C:
			t.Fatalf("relay never reached forwarding state, last=%#v err=%v", current, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func awaitEndedLive(t *testing.T, f *liveTestFixture, id string) LiveView {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		current, err := f.control.Reconcile(f.ctx, liveTestOwner, id)
		if err == nil && current.State == "ended" && current.LocalStopped && current.RemoteEnded {
			return current
		}
		select {
		case <-deadline.C:
			t.Fatalf("live session did not settle, last=%#v err=%v", current, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestManagedLiveOutputCancelsWhileEncoderIsLaunching(t *testing.T) {
	f := newLiveTestFixture(t)
	entered := make(chan LiveReference, 1)
	output := NewManagedLiveOutput(func(ctx context.Context, ref LiveReference) (LiveEncoder, error) {
		select {
		case entered <- ref:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	// The launch deliberately never reaches the relay. A valid private route
	// still exercises the normal LiveController dispatch ordering.
	destination, _ := url.Parse("rtmp://127.0.0.1:1935/platform#private-stream-key")
	newRelayController(t, f, output, LiveIngest{
		Destination: destination,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("encoder launch should be canceled before dial")
		},
	})

	result := make(chan struct {
		view LiveView
		err  error
	}, 1)
	go func() {
		view, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "launch-cancel", DurationSeconds: 60})
		result <- struct {
			view LiveView
			err  error
		}{view, err}
	}()

	var ref LiveReference
	select {
	case ref = <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("encoder launch was never entered")
	}
	if ref.ID == "" || ref.Target.ConnectionID != liveTestConn || ref.Target.AccountID != "account-exact" {
		t.Fatalf("launcher received wrong owned reference: %#v", ref)
	}

	stopped, err := f.control.Stop(f.ctx, liveTestOwner, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.StopReason != "owner-stop" || !stopped.LocalStopped {
		t.Fatalf("owner stop did not fence the launching encoder: %#v", stopped)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("concurrent Start returned an error instead of its durable result: %v", got.err)
		}
		if got.view.ID != ref.ID || (got.view.State != "stopping" && got.view.State != "ended") {
			t.Fatalf("concurrent Start returned wrong durable session: %#v", got.view)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled encoder launch did not return")
	}
	awaitEndedLive(t, f, ref.ID)
	if start, _, _ := f.provider.calls(); start != 1 {
		t.Fatalf("platform StartLive calls = %d, want 1", start)
	}
}

func TestManagedLiveOutputRestartNeverRepublishesOwnedSession(t *testing.T) {
	f := newLiveTestFixture(t)
	target := newRelayTarget(t)
	encoder := &syntheticRelayEncoder{payload: []byte{0x61}}
	originalOutput := NewManagedLiveOutput(func(_ context.Context, ref LiveReference) (LiveEncoder, error) {
		encoder.mu.Lock()
		encoder.launched = ref
		encoder.mu.Unlock()
		return encoder, nil
	})
	newRelayController(t, f, originalOutput, LiveIngest{Destination: target.destination(), DialContext: target.dial})
	started := f.start("relay-restart", 60)
	select {
	case <-target.received:
	case err := <-target.errs:
		t.Fatalf("target handshake failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("initial relay did not forward media")
	}
	awaitForwardingLive(t, f, started.ID)

	// A fresh host has no lease and therefore cannot republish an old durable
	// session. Its only permitted recovery action is to settle that session.
	var freshLaunches int
	freshOutput := NewManagedLiveOutput(func(context.Context, LiveReference) (LiveEncoder, error) {
		freshLaunches++
		return nil, errors.New("restart must not launch a replacement encoder")
	})
	restarted := NewLiveController(f.control.registry, f.store, freshOutput)
	restarted.now = f.time
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		restarted.Run(runCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		// In production the prior process dies on restart. The synthetic process
		// is still in this test process, so explicitly release it after recovery.
		_ = originalOutput.StopLiveOutput(context.Background(), currentLiveReference(t, f, started.ID))
	})

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		view, err := restarted.Status(f.ctx, liveTestOwner, started.ID)
		if err == nil && view.State == "ended" && view.LocalStopped && view.RemoteEnded {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("restart did not settle old durable session, last=%#v err=%v", view, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if freshLaunches != 0 {
		t.Fatalf("restart launched a replacement encoder %d times", freshLaunches)
	}
	if starts, _, _ := f.provider.calls(); starts != 1 {
		t.Fatalf("restart replayed platform StartLive: %d", starts)
	}
}

func TestManagedLiveOutputPreflightPreventsPlatformAndEncoderStart(t *testing.T) {
	f := newLiveTestFixture(t)
	launches, checks := 0, 0
	denied := errors.New("synthetic program authorization revoked")
	output := NewManagedLiveOutputWithPreflight(func(context.Context, LiveReference) (LiveEncoder, error) {
		launches++
		return nil, errors.New("must not launch")
	}, func(context.Context, LiveReference) error { checks++; return denied })
	f.control = NewLiveController(f.control.registry, f.store, output)
	f.control.now = f.time
	started := f.start("preflight-denied", 60)
	if checks == 0 || launches != 0 {
		t.Fatalf("preflight checks=%d launches=%d", checks, launches)
	}
	if starts, _, _ := f.provider.calls(); starts != 0 {
		t.Fatalf("preflight denial dispatched %d platform starts", starts)
	}
	if started.StopReason != "output-unavailable" {
		t.Fatalf("missing preflight failure: %#v", started)
	}
	// A direct output call must not bypass the same check.
	ref := LiveReference{ID: "direct", Deadline: time.Now().Add(time.Minute)}
	if err := output.StartLiveOutput(f.ctx, ref, LiveIngest{}); !errors.Is(err, ErrLiveUnavailable) {
		t.Fatalf("direct preflight result: %v", err)
	}
	if launches != 0 {
		t.Fatalf("direct call launched %d encoders", launches)
	}
	if err := output.StopLiveOutput(f.ctx, ref); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLiveOutputPreflightRechecksActiveLease(t *testing.T) {
	checks := 0
	var denied error
	output := NewManagedLiveOutputWithPreflight(func(context.Context, LiveReference) (LiveEncoder, error) { return nil, errors.New("not used") }, func(context.Context, LiveReference) error { checks++; return denied })
	ref := LiveReference{ID: "active", Deadline: time.Now().Add(time.Minute)}
	if err := output.CheckLiveOutput(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output.lease = &liveOutputLease{ref: ref, ctx: leaseCtx, ready: make(chan struct{})}
	denied = errors.New("source forgotten")
	if err := output.CheckLiveOutput(context.Background(), ref); !errors.Is(err, denied) {
		t.Fatalf("active lease authorization not rechecked: %v", err)
	}
	if checks != 2 {
		t.Fatalf("checks=%d, want 2", checks)
	}
}
