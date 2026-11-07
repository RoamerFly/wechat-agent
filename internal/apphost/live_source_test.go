package apphost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type liveSourceTestProvider struct {
	*liveTestProvider
	beforeStart func(context.Context, LiveReference)
	startMode   *string
	queried     chan LiveReference
	stopped     chan LiveReference
}

func (*liveSourceTestProvider) LiveMediaMode(ConnectionRef) string { return LiveMediaLocalSource }

func (p *liveSourceTestProvider) StartLive(ctx context.Context, ref LiveReference) (LiveStartResult, error) {
	if p.beforeStart != nil {
		p.beforeStart(ctx, ref)
	}
	result, err := p.liveTestProvider.StartLive(ctx, ref)
	if p.startMode != nil {
		result.Receipt.Reference.MediaMode = *p.startMode
	}
	return result, err
}

func (p *liveSourceTestProvider) QueryLive(ctx context.Context, ref LiveReference) (LiveReceipt, error) {
	p.queried <- ref
	return p.liveTestProvider.QueryLive(ctx, ref)
}

func (p *liveSourceTestProvider) StopLive(ctx context.Context, ref LiveReference) (LiveReceipt, error) {
	p.stopped <- ref
	return p.liveTestProvider.StopLive(ctx, ref)
}

type liveSourceTestOutput struct {
	*liveTestOutput
	prepare func(context.Context, LiveReference) error
}

func (o *liveSourceTestOutput) PrepareLiveSource(ctx context.Context, ref LiveReference) error {
	return o.prepare(ctx, ref)
}

func newLiveSourceTestFixture(t *testing.T, prepare func(context.Context, LiveReference) error) (*liveTestFixture, *liveSourceTestProvider, *liveSourceTestOutput) {
	t.Helper()
	f := newLiveTestFixture(t)
	provider := &liveSourceTestProvider{liveTestProvider: f.provider, queried: make(chan LiveReference, 8), stopped: make(chan LiveReference, 8)}
	entry, _ := f.control.registry.Lookup(liveTestApp)
	entry.Provider = provider
	registry, err := New(entry)
	if err != nil {
		t.Fatal(err)
	}
	output := &liveSourceTestOutput{liveTestOutput: &liveTestOutput{stopped: make(chan LiveReference, 8)}, prepare: prepare}
	f.control = NewLiveControllerWithSource(registry, f.store, f.output, output)
	f.control.now = f.time
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.control.Close(ctx)
	})
	return f, provider, output
}

func liveSourceTestInput(key string) LiveStart {
	return LiveStart{AppID: liveTestApp, ConnectionID: liveTestConn, RequestKey: key, DurationSeconds: 60,
		ProgramArtifactID: "work-local-source", ProgramSHA256: strings.Repeat("a", 64), ProgramLoop: true}
}

func awaitLiveSourceTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the synthetic live operation")
		var zero T
		return zero
	}
}

func startLiveSourceTestAsync(t *testing.T, f *liveTestFixture, key string) <-chan LiveView {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan LiveView, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})
	go func() {
		defer close(done)
		v, err := f.control.Start(ctx, liveTestOwner, liveSourceTestInput(key))
		if err != nil {
			t.Error(err)
		}
		result <- v
	}()
	return result
}

func TestLiveSourceRequiresPreparationAndMatchingReceipt(t *testing.T) {
	prepared := make(chan LiveReference, 1)
	f, provider, _ := newLiveSourceTestFixture(t, func(_ context.Context, ref LiveReference) error {
		prepared <- ref
		return nil
	})
	targets := f.control.Targets(f.ctx)
	if len(targets) != 1 || targets[0].MediaMode != LiveMediaLocalSource || targets[0].ConnectionID != liveTestConn {
		t.Fatalf("unexpected local-source targets: %#v", targets)
	}
	entered, release := make(chan LiveReference, 1), make(chan struct{})
	provider.beforeStart = func(ctx context.Context, ref LiveReference) {
		entered <- ref
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	result := startLiveSourceTestAsync(t, f, "local-source-ready")
	ref := awaitLiveSourceTestValue(t, entered)
	select {
	case localRef := <-prepared:
		if !sameLiveReference(localRef, ref) || localRef.MediaMode != LiveMediaLocalSource {
			t.Fatalf("prepared reference %#v differs from provider reference %#v", localRef, ref)
		}
	default:
		t.Fatal("provider start entered before local source preparation")
	}
	current, err := f.control.Status(f.ctx, liveTestOwner, ref.ID)
	if err != nil || current.State != "starting" || current.RoomID != "" {
		t.Fatalf("local source alone changed the live state: %#v %v", current, err)
	}
	close(release)
	live := awaitLiveSourceTestValue(t, result)
	if live.State != "live" || live.RoomID != "room-1" || live.MediaMode != LiveMediaLocalSource || live.ID != ref.ID {
		t.Fatalf("matching provider receipt did not confirm live state: %#v", live)
	}
	if durable := currentLiveReference(t, f, live.ID); !sameLiveReference(durable, ref) || durable.MediaMode != LiveMediaLocalSource {
		t.Fatalf("durable operation lost its source identity: %#v", durable)
	}
}

func TestLiveSourceChecksReadinessAfterPreparation(t *testing.T) {
	var prepared bool
	f, _, output := newLiveSourceTestFixture(t, func(context.Context, LiveReference) error {
		prepared = true
		return nil
	})
	output.liveTestOutput.setCheck(errors.New("synthetic source has not been prepared"))
	output.prepare = func(context.Context, LiveReference) error {
		prepared = true
		output.liveTestOutput.setCheck(nil)
		return nil
	}
	view, err := f.control.Start(f.ctx, liveTestOwner, liveSourceTestInput("prepare-before-ready"))
	if err != nil || !prepared || view.State != "live" {
		t.Fatalf("readiness prevented source preparation: %#v %v", view, err)
	}
}

func TestLiveSourcePreparationCannotDispatchAfterAbort(t *testing.T) {
	for _, reason := range []string{"owner-stop", "authorization-changed", "output-unavailable"} {
		t.Run(reason, func(t *testing.T) {
			entered, release := make(chan LiveReference, 1), make(chan struct{})
			f, _, output := newLiveSourceTestFixture(t, func(ctx context.Context, ref LiveReference) error {
				entered <- ref
				select {
				case <-ctx.Done():
					// Simulate readiness being returned just as the stop cancels setup.
					return nil
				case <-release:
					if reason == "output-unavailable" {
						return errors.New("synthetic local source preparation failed after claiming output")
					}
					return nil
				}
			})
			result := startLiveSourceTestAsync(t, f, reason)
			ref := awaitLiveSourceTestValue(t, entered)
			if start, _, _ := f.provider.calls(); start != 0 {
				t.Fatalf("platform start called while source preparation is blocked: %d", start)
			}
			switch reason {
			case "owner-stop":
				if _, err := f.control.Stop(f.ctx, liveTestOwner, ref.ID); err != nil {
					t.Fatal(err)
				}
			case "authorization-changed":
				f.provider.mu.Lock()
				f.provider.grant.Allowed = false
				f.provider.mu.Unlock()
				close(release)
			case "output-unavailable":
				close(release)
			}
			_ = awaitLiveSourceTestValue(t, result)
			stoppedRef := awaitLiveSourceTestValue(t, output.stopped)
			if !sameLiveReference(stoppedRef, ref) {
				t.Fatalf("stopped another local source: got %#v, want %#v", stoppedRef, ref)
			}
			if ended := f.requireEnded(ref.ID); ended.StopReason != reason || ended.RoomID != "" {
				t.Fatalf("unexpected final status for %s: %#v", reason, ended)
			}
			if start, query, stop := f.provider.calls(); start != 0 || query != 0 || stop != 0 {
				t.Fatalf("aborted source contacted the platform: start=%d query=%d stop=%d", start, query, stop)
			}
		})
	}
}

func TestLiveSourceStopsOnRejectedOrEndedReceipt(t *testing.T) {
	for _, reason := range []string{"start-unconfirmed", "platform-ended"} {
		t.Run(reason, func(t *testing.T) {
			f, provider, output := newLiveSourceTestFixture(t, func(context.Context, LiveReference) error { return nil })
			if reason == "start-unconfirmed" {
				wrongMode := ""
				provider.startMode = &wrongMode
			} else {
				f.provider.startState = "ended"
			}
			v, err := f.control.Start(f.ctx, liveTestOwner, liveSourceTestInput(reason))
			if err != nil || v.State != "ended" || v.StopReason != reason || !v.LocalStopped || !v.RemoteEnded {
				t.Fatalf("receipt did not settle local source: %#v %v", v, err)
			}
			if got := awaitLiveSourceTestValue(t, output.stopped); got.ID != v.ID || got.MediaMode != LiveMediaLocalSource {
				t.Fatalf("cleaned a different local operation: %#v", got)
			}
			if f.output.stops() != 0 {
				t.Fatal("local-source cleanup used the legacy ingest output")
			}
		})
	}
}

func TestLiveSourceRequiresConfiguredSourceOutput(t *testing.T) {
	f, _, _ := newLiveSourceTestFixture(t, nil)
	control := NewLiveController(f.control.registry, f.store, f.output)
	control.now = f.time
	if targets := control.Targets(f.ctx); len(targets) != 0 {
		t.Fatalf("local-source target advertised without a source output: %#v", targets)
	}
	if _, err := control.Start(f.ctx, liveTestOwner, liveSourceTestInput("missing-source")); !errors.Is(err, ErrLiveUnavailable) {
		t.Fatalf("local-source start without an output error = %v, want ErrLiveUnavailable", err)
	}
	if start, _, _ := f.provider.calls(); start != 0 {
		t.Fatalf("missing source dispatched %d platform starts", start)
	}
}

func TestLiveSourceRestartSettlesOriginalOperation(t *testing.T) {
	prepared := make(chan LiveReference, 2)
	f, provider, output := newLiveSourceTestFixture(t, func(_ context.Context, ref LiveReference) error {
		prepared <- ref
		return nil
	})
	live, err := f.control.Start(f.ctx, liveTestOwner, liveSourceTestInput("local-source-restart"))
	if err != nil || live.State != "live" {
		t.Fatalf("initial live start = %#v %v", live, err)
	}
	ref := awaitLiveSourceTestValue(t, prepared)
	f.provider.mu.Lock()
	f.provider.stopErr = errors.New("synthetic stop receipt lost")
	f.provider.queryState = "ended"
	f.provider.mu.Unlock()
	restarted := NewLiveControllerWithSource(f.control.registry, f.store, f.output, output)
	restarted.now = f.time
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); restarted.Run(ctx) }()
	for _, observed := range []LiveReference{awaitLiveSourceTestValue(t, output.stopped), awaitLiveSourceTestValue(t, provider.stopped), awaitLiveSourceTestValue(t, provider.queried)} {
		if !sameLiveReference(observed, ref) || observed.MediaMode != LiveMediaLocalSource {
			t.Fatalf("restart used a different operation: got %#v, want %#v", observed, ref)
		}
	}
	deadline := time.After(3 * time.Second)
	for {
		current, err := restarted.Status(f.ctx, liveTestOwner, ref.ID)
		if err == nil && current.State == "ended" && current.StopReason == "restart" && current.LocalStopped && current.RemoteEnded {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("restart did not settle the original operation: %#v %v", current, err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	awaitLiveSourceTestValue(t, done)
	if start, _, _ := f.provider.calls(); start != 1 || len(prepared) != 0 {
		t.Fatalf("restart replayed preparation or dispatch: extra preparations=%d starts=%d", len(prepared), start)
	}
}
