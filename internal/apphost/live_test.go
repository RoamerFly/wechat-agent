package apphost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"wechat-bot/internal/pluginhost"
)

const (
	liveTestOwner = "owner-a"
	liveTestApp   = "synthetic-live"
	liveTestConn  = "broadcast"
)

// These tests deliberately use the real SQLite host store.  The provider and
// encoder output are synthetic only: a session must never be able to pass a
// coordinator transition merely because a mock forgot its durable state.
type liveTestProvider struct {
	mu sync.Mutex

	connection Connection
	grant      LiveGrant
	grantErr   error

	startCalls int
	queryCalls int
	stopCalls  int
	startState string
	queryState string
	stopState  string
	startErr   error
	queryErr   error
	stopErr    error
	startWait  bool
	entered    chan LiveReference
}

func (p *liveTestProvider) Connections(context.Context) ([]Connection, error) {
	return []Connection{p.connection}, nil
}

func (p *liveTestProvider) Inspect(context.Context, string) (ConnectionStatus, error) {
	return ConnectionStatus{Connection: p.connection, Login: "online"}, nil
}

func (p *liveTestProvider) LiveGrant(context.Context, ConnectionRef) (LiveGrant, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.grant, p.grantErr
}

func (p *liveTestProvider) StartLive(ctx context.Context, ref LiveReference) (LiveStartResult, error) {
	p.mu.Lock()
	p.startCalls++
	wait, entered, state, err := p.startWait, p.entered, p.startState, p.startErr
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- ref:
		default:
		}
	}
	if wait {
		<-ctx.Done()
	}
	if state == "" {
		state = "live"
	}
	return LiveStartResult{Receipt: liveTestReceipt(ref, state)}, err
}

func (p *liveTestProvider) QueryLive(_ context.Context, ref LiveReference) (LiveReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queryCalls++
	state := p.queryState
	if state == "" {
		state = "live"
	}
	return liveTestReceipt(ref, state), p.queryErr
}

func (p *liveTestProvider) StopLive(_ context.Context, ref LiveReference) (LiveReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopCalls++
	state := p.stopState
	if state == "" {
		state = "ended"
	}
	return liveTestReceipt(ref, state), p.stopErr
}

func (p *liveTestProvider) calls() (start, query, stop int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startCalls, p.queryCalls, p.stopCalls
}

func liveTestReceipt(ref LiveReference, state string) LiveReceipt {
	r := LiveReceipt{Reference: ref, State: state, RoomID: "room-1"}
	if state == "unknown" {
		r.RoomID = ""
	}
	return r
}

type liveTestOutput struct {
	mu        sync.Mutex
	checkErr  error
	stopErr   error
	stopCalls int
	stopped   chan LiveReference
}

func (o *liveTestOutput) CheckLiveOutput(context.Context, LiveReference) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.checkErr
}

func (o *liveTestOutput) StopLiveOutput(_ context.Context, ref LiveReference) error {
	o.mu.Lock()
	o.stopCalls++
	err, stopped := o.stopErr, o.stopped
	o.mu.Unlock()
	if stopped != nil {
		select {
		case stopped <- ref:
		default:
		}
	}
	return err
}

func (o *liveTestOutput) setCheck(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.checkErr = err
}

func (o *liveTestOutput) stops() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stopCalls
}

type liveTestFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *pluginhost.Store
	provider *liveTestProvider
	output   *liveTestOutput
	control  *LiveController
	now      time.Time
	clockMu  sync.RWMutex
}

func newLiveTestFixture(t *testing.T) *liveTestFixture {
	t.Helper()
	store, err := pluginhost.OpenStore(t.TempDir(), "live-owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// The relay enforces its lease against the wall clock, even when the
	// controller tests advance their own captured clock.
	now := time.Now().UTC()
	provider := &liveTestProvider{
		connection: Connection{ID: liveTestConn, AccountID: "account-exact", Label: "合成直播账号", Configuration: "ready"},
		grant:      LiveGrant{Allowed: true, Version: "grant-v1", OpensAt: now.Add(-time.Hour), ClosesAt: now.Add(time.Hour), MaxDuration: 30 * time.Minute},
	}
	registry, err := New(Registration{Application: Application{ID: liveTestApp, Name: "合成直播", InstallationID: "synthetic-install", Registration: "synthetic-v1"}, Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	output := &liveTestOutput{stopped: make(chan LiveReference, 8)}
	control := NewLiveController(registry, store, output)
	fixture := &liveTestFixture{t: t, ctx: context.Background(), store: store, provider: provider, output: output, control: control, now: now}
	control.now = fixture.time
	return fixture
}

func (f *liveTestFixture) time() time.Time {
	f.clockMu.RLock()
	defer f.clockMu.RUnlock()
	return f.now
}

func (f *liveTestFixture) advance(d time.Duration) {
	f.clockMu.Lock()
	defer f.clockMu.Unlock()
	f.now = f.now.Add(d)
}

func (f *liveTestFixture) start(key string, seconds int64) LiveView {
	f.t.Helper()
	v, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: key, DurationSeconds: seconds})
	if err != nil {
		f.t.Fatalf("Start(%q): %v", key, err)
	}
	return v
}

func (f *liveTestFixture) requireEnded(id string) LiveView {
	f.t.Helper()
	v, err := f.control.Status(f.ctx, liveTestOwner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	if v.State != "ended" || !v.LocalStopped || !v.RemoteEnded {
		f.t.Fatalf("want fully ended live session, got %#v", v)
	}
	return v
}

func TestLiveStartUsesExactConnectionAndReplaysRequest(t *testing.T) {
	f := newLiveTestFixture(t)
	targets := f.control.Targets(f.ctx)
	if len(targets) != 1 || targets[0].AppID != liveTestApp || targets[0].ConnectionID != liveTestConn {
		t.Fatalf("unexpected live targets: %#v", targets)
	}

	first := f.start("request-a", 60)
	if first.State != "live" || first.RoomID != "room-1" || first.AppID != liveTestApp || first.ConnectionID != liveTestConn {
		t.Fatalf("unexpected started view: %#v", first)
	}
	replay := f.start("request-a", 60)
	if replay != first {
		t.Fatalf("request replay changed durable view: got %#v want %#v", replay, first)
	}
	if got, _, _ := f.provider.calls(); got != 1 {
		t.Fatalf("request replay submitted another live start: %d", got)
	}
	if _, err := f.control.Status(f.ctx, "another-owner", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner isolation error = %v, want ErrNotFound", err)
	}
	if _, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "request-a", DurationSeconds: 61}); !errors.Is(err, ErrLiveConflict) {
		t.Fatalf("changed replay error = %v, want ErrLiveConflict", err)
	}
	if _, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "too-long", DurationSeconds: int64((31 * time.Minute) / time.Second)}); !errors.Is(err, ErrLiveDenied) {
		t.Fatalf("duration beyond grant error = %v, want ErrLiveDenied", err)
	}
	if _, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: "bad id!", RequestKey: "bad-input", DurationSeconds: 1}); !errors.Is(err, ErrLiveInput) {
		t.Fatalf("bad input error = %v, want ErrLiveInput", err)
	}
	if _, err := f.control.Stop(f.ctx, liveTestOwner, first.ID); err != nil {
		t.Fatal(err)
	}
	f.requireEnded(first.ID)
}

func TestLiveStopWinsWhenLateStartReturnsReceipt(t *testing.T) {
	f := newLiveTestFixture(t)
	f.provider.startWait = true
	f.provider.entered = make(chan LiveReference, 1)
	result := make(chan struct {
		v   LiveView
		err error
	}, 1)
	go func() {
		v, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "delayed", DurationSeconds: 60})
		result <- struct {
			v   LiveView
			err error
		}{v, err}
	}()
	select {
	case <-f.provider.entered:
	case <-time.After(time.Second):
		t.Fatal("StartLive was not entered")
	}
	active, err := f.control.Active(f.ctx, liveTestOwner)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := f.control.Stop(f.ctx, liveTestOwner, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopping" && stopped.State != "ended" {
		t.Fatalf("stop did not claim session before late receipt: %#v", stopped)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("late Start returned error: %v", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late Start did not observe Stop cancellation")
	}
	f.requireEnded(active.ID)
	if start, _, stop := f.provider.calls(); start != 1 || stop != 1 {
		t.Fatalf("late receipt restarted or skipped cleanup: starts=%d stops=%d", start, stop)
	}
}

func TestLiveUnknownKeepsDurableSlot(t *testing.T) {
	f := newLiveTestFixture(t)
	f.provider.startState = "unknown"
	f.provider.stopErr = errors.New("temporary platform outage")
	f.provider.queryState = "unknown"
	v := f.start("unknown-start", 60)
	if v.State != "stopping" || v.StopReason != "start-unconfirmed" || !v.LocalStopped || v.RemoteEnded {
		t.Fatalf("unknown start must retain a recoverable active slot, got %#v", v)
	}
	if _, err := f.control.Active(f.ctx, liveTestOwner); err != nil {
		t.Fatalf("unknown session disappeared from active slot: %v", err)
	}
	if _, err := f.control.Start(f.ctx, liveTestOwner, LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: "another", DurationSeconds: 60}); !errors.Is(err, ErrLiveConflict) {
		t.Fatalf("unknown active slot allowed a second broadcast: %v", err)
	}
	if _, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID); err != nil {
		t.Fatal(err)
	}
	if start, _, _ := f.provider.calls(); start != 1 {
		t.Fatalf("reconciliation replayed StartLive: %d", start)
	}
}

func TestLiveReconcileStopsForOutputGrantAndDeadline(t *testing.T) {
	t.Run("platform disconnected", func(t *testing.T) {
		f := newLiveTestFixture(t)
		v := f.start("platform-disconnected", 60)
		f.provider.mu.Lock()
		f.provider.queryErr = errors.New("synthetic connection lost")
		f.provider.stopErr = f.provider.queryErr
		f.provider.mu.Unlock()
		got, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID)
		if err != nil || got.StopReason != "platform-unreachable" || got.State != "stopping" || !got.LocalStopped || got.RemoteEnded {
			t.Fatalf("disconnect lost local stop or forged a remote end: %+v %v", got, err)
		}
		f.provider.mu.Lock()
		f.provider.queryErr, f.provider.stopErr = nil, nil
		f.provider.mu.Unlock()
		if _, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID); err != nil {
			t.Fatal(err)
		}
		f.requireEnded(v.ID)
	})
	t.Run("unsafe output", func(t *testing.T) {
		f := newLiveTestFixture(t)
		v := f.start("output-fails", 60)
		f.output.setCheck(errors.New("encoder exited"))
		got, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StopReason != "output-unavailable" {
			t.Fatalf("stop reason = %q, want output-unavailable", got.StopReason)
		}
		f.requireEnded(v.ID)
	})
	t.Run("grant revoked", func(t *testing.T) {
		f := newLiveTestFixture(t)
		v := f.start("grant-revoked", 60)
		f.provider.mu.Lock()
		f.provider.grant.Allowed = false
		f.provider.mu.Unlock()
		got, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StopReason != "authorization-changed" {
			t.Fatalf("stop reason = %q, want authorization-changed", got.StopReason)
		}
		f.requireEnded(v.ID)
	})
	t.Run("deadline", func(t *testing.T) {
		f := newLiveTestFixture(t)
		v := f.start("deadline", 2)
		f.advance(3 * time.Second)
		got, err := f.control.Reconcile(f.ctx, liveTestOwner, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.StopReason != "deadline" {
			t.Fatalf("stop reason = %q, want deadline", got.StopReason)
		}
		f.requireEnded(v.ID)
	})
}

func TestLiveReceiptRejectsOtherAccountOrRoom(t *testing.T) {
	f := newLiveTestFixture(t)
	v := f.start("foreign-receipt", 60)
	s, err := readLive(f.store.LiveSession(f.ctx, v.ID))
	if err != nil {
		t.Fatal(err)
	}
	foreignAccount := liveTestReceipt(s.Reference, "ended")
	foreignAccount.Reference.Target.AccountID = "unrelated-account"
	foreignRoom := liveTestReceipt(s.Reference, "ended")
	foreignRoom.RoomID = "unrelated-room"
	for _, receipt := range []LiveReceipt{foreignAccount, foreignRoom} {
		if _, err := f.control.applyLiveReceipt(f.ctx, s, receipt); !errors.Is(err, ErrLiveConflict) {
			t.Fatalf("foreign receipt was accepted: %v", err)
		}
	}
	if current, err := f.control.Status(f.ctx, liveTestOwner, v.ID); err != nil || current.State != "live" || current.RemoteEnded {
		t.Fatalf("foreign receipt changed authoritative state: %+v %v", current, err)
	}
}

func TestLiveShutdownWaitsForOwnedOutputStop(t *testing.T) {
	f := newLiveTestFixture(t)
	v := f.start("shutdown-waits", 60)
	f.control.local <- struct{}{}
	held := true
	defer func() {
		if held {
			<-f.control.local
		}
	}()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.control.Close(ctx) }()
	for {
		current, err := f.control.Status(ctx, liveTestOwner, v.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == "stopping" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("shutdown did not record stop intent")
		case <-time.After(5 * time.Millisecond):
		}
	}
	select {
	case err := <-done:
		t.Fatalf("shutdown finished before confirming local output: %v", err)
	default:
	}
	<-f.control.local
	held = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.requireEnded(v.ID)
}

func TestLiveDeadlineTimerStopsWithoutSupervisor(t *testing.T) {
	f := newLiveTestFixture(t)
	realNow := time.Now
	f.control.now = realNow
	f.provider.mu.Lock()
	f.provider.grant.OpensAt = realNow().Add(-time.Minute)
	f.provider.grant.ClosesAt = realNow().Add(time.Minute)
	f.provider.mu.Unlock()
	v := f.start("timer-deadline", 1)

	until := time.NewTimer(3 * time.Second)
	defer until.Stop()
	for {
		got, err := f.control.Status(f.ctx, liveTestOwner, v.ID)
		if err == nil && got.State == "ended" {
			if got.StopReason != "deadline" {
				t.Fatalf("timer stop reason = %q, want deadline", got.StopReason)
			}
			return
		}
		select {
		case <-until.C:
			t.Fatalf("deadline timer did not settle without Run, last=%#v err=%v", got, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestLiveRunRestartsByStoppingDurableSessionOnly(t *testing.T) {
	f := newLiveTestFixture(t)
	v := f.start("restart", 60)
	if start, _, _ := f.provider.calls(); start != 1 {
		t.Fatalf("initial StartLive calls = %d, want 1", start)
	}
	restarted := NewLiveController(f.control.registry, f.store, f.output)
	restarted.now = f.time
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); restarted.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		got, err := restarted.Status(f.ctx, liveTestOwner, v.ID)
		if err == nil && got.State == "ended" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("restart recovery did not settle session, last=%#v err=%v", got, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit")
	}
	if start, _, _ := f.provider.calls(); start != 1 {
		t.Fatalf("restart recovery replayed StartLive: %d", start)
	}
	if f.output.stops() == 0 {
		t.Fatal("restart recovery did not locally stop the owned output")
	}
}
