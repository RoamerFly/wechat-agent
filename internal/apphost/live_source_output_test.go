package apphost

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type sourceSignalFixture struct {
	entered chan struct{}
	start   func(context.Context) error
	ready   atomic.Bool
	stops   atomic.Int32
}

func (s *sourceSignalFixture) Start(ctx context.Context) error {
	if s.entered != nil {
		close(s.entered)
	}
	if s.start != nil {
		if err := s.start(ctx); err != nil {
			return err
		}
	}
	s.ready.Store(true)
	return nil
}

func (s *sourceSignalFixture) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.ready.Load() {
		return errors.New("synthetic signal unavailable")
	}
	return nil
}

func (s *sourceSignalFixture) Stop(context.Context) error {
	s.stops.Add(1)
	s.ready.Store(false)
	return nil
}

func TestManagedLiveSourceUsesOnlyItsSessionAndActualSignal(t *testing.T) {
	source := &sourceSignalFixture{}
	var launches atomic.Int32
	output := NewManagedLiveSource(func(context.Context, LiveReference) (LiveLocalSource, error) {
		launches.Add(1)
		return source, nil
	}, nil)
	ref := LiveReference{ID: "source-fixture", MediaMode: LiveMediaLocalSource, Deadline: time.Now().Add(time.Minute)}
	if err := output.PrepareLiveSource(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := output.CheckLiveOutput(t.Context(), ref); err != nil || output.LiveOutputStatus(ref).State != "ready" {
		t.Fatal("missing actual signal", err)
	}
	wrong := ref
	wrong.ProgramLoop = true
	if err := output.StopLiveOutput(t.Context(), wrong); err != nil || source.stops.Load() != 0 {
		t.Fatal("another reference stopped this signal", err)
	}
	source.ready.Store(false)
	if err := output.CheckLiveOutput(t.Context(), ref); err == nil {
		t.Fatal("a dead signal passed readiness")
	}
	if err := output.StopLiveOutput(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := output.PrepareLiveSource(t.Context(), ref); !errors.Is(err, ErrLiveUnavailable) {
		t.Fatal("cancelled source replayed", err)
	}
	if launches.Load() != 1 || source.stops.Load() != 1 || output.LiveOutputStatus(ref).State != "stopped" {
		t.Fatal("unexpected launch or cleanup", launches.Load(), source.stops.Load())
	}
}

func TestManagedLiveSourceCancelsPartialPreparationAndCleansOnce(t *testing.T) {
	source := &sourceSignalFixture{entered: make(chan struct{}), start: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	output := NewManagedLiveSource(func(context.Context, LiveReference) (LiveLocalSource, error) { return source, nil }, nil)
	ref := LiveReference{ID: "partial-source", MediaMode: LiveMediaLocalSource, Deadline: time.Now().Add(time.Minute)}
	finished := make(chan error, 1)
	go func() { finished <- output.PrepareLiveSource(t.Context(), ref) }()
	awaitLiveSourceTestValue(t, source.entered)
	if err := output.StopLiveOutput(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if err := awaitLiveSourceTestValue(t, finished); !errors.Is(err, context.Canceled) {
		t.Fatal("partial preparation ignored stop", err)
	}
	if err := output.StopLiveOutput(t.Context(), ref); err != nil || source.stops.Load() != 1 {
		t.Fatal("repeated cancellation repeated cleanup", err, source.stops.Load())
	}
}

func TestManagedLiveSourceRechecksAuthorizationAfterLaunch(t *testing.T) {
	source := &sourceSignalFixture{}
	var checks atomic.Int32
	output := NewManagedLiveSource(func(context.Context, LiveReference) (LiveLocalSource, error) { return source, nil },
		func(context.Context, LiveReference) error {
			if checks.Add(1) > 1 {
				return ErrLiveDenied
			}
			return nil
		})
	ref := LiveReference{ID: "revoked-source", MediaMode: LiveMediaLocalSource, Deadline: time.Now().Add(time.Minute)}
	if err := output.PrepareLiveSource(t.Context(), ref); !errors.Is(err, ErrLiveDenied) {
		t.Fatal("revoked program became ready", err)
	}
	if err := output.StopLiveOutput(t.Context(), ref); err != nil || source.stops.Load() != 1 {
		t.Fatal("failed prepare was not reclaimed", err)
	}
}
