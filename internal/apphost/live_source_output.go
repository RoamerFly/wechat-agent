package apphost

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// LiveLocalSource owns a capture-ready local signal for one session. Start
// waits for actual signal readiness. Check verifies that signal, and Stop
// releases the source without changing the platform's room state.
type LiveLocalSource interface {
	Start(context.Context) error
	Check(context.Context) error
	Stop(context.Context) error
}

type ManagedLiveSource struct {
	launch func(context.Context, LiveReference) (LiveLocalSource, error)
	check  func(context.Context, LiveReference) error
	mu     sync.Mutex
	lease  *liveSourceLease
}

type liveSourceLease struct {
	ref      LiveReference
	ctx      context.Context
	cancel   context.CancelFunc
	ready    chan struct{}
	cleaning chan struct{}
	stopped  atomic.Bool
	source   LiveLocalSource
	startErr error
}

func NewManagedLiveSource(launch func(context.Context, LiveReference) (LiveLocalSource, error), check func(context.Context, LiveReference) error) *ManagedLiveSource {
	if launch == nil {
		return nil
	}
	return &ManagedLiveSource{launch: launch, check: check}
}

func (o *ManagedLiveSource) current(ref LiveReference) *liveSourceLease {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lease == nil || !sameLiveReference(o.lease.ref, ref) {
		return nil
	}
	return o.lease
}

func (o *ManagedLiveSource) PrepareLiveSource(ctx context.Context, ref LiveReference) (result error) {
	o.mu.Lock()
	if ref.ID == "" || ref.MediaMode != LiveMediaLocalSource || ctx.Err() != nil || !time.Now().Before(ref.Deadline) ||
		(o.lease != nil && (!o.lease.stopped.Load() || sameLiveReference(o.lease.ref, ref))) {
		o.mu.Unlock()
		return ErrLiveUnavailable
	}
	lease := &liveSourceLease{ref: ref, ready: make(chan struct{}), cleaning: make(chan struct{}, 1)}
	lease.ctx, lease.cancel = context.WithDeadline(context.Background(), ref.Deadline)
	o.lease = lease
	o.mu.Unlock()

	go func() {
		<-lease.ctx.Done()
		cleanup, cancel := context.WithTimeout(context.Background(), liveRemoteTimeout)
		defer cancel()
		_ = lease.stop(cleanup)
	}()
	setup, cancel := context.WithCancel(ctx)
	stopSetup := context.AfterFunc(lease.ctx, cancel)
	defer func() {
		lease.startErr = result
		close(lease.ready)
		stopSetup()
		cancel()
		if result != nil {
			lease.cancel()
		}
	}()
	if o.check != nil {
		if err := o.check(setup, ref); err != nil {
			return err
		}
	}
	lease.source, result = o.launch(setup, ref)
	if result != nil {
		return result
	}
	if lease.source == nil || setup.Err() != nil {
		return ErrLiveUnavailable
	}
	if err := lease.source.Start(setup); err != nil {
		return err
	}
	if o.check != nil {
		if err := o.check(setup, ref); err != nil {
			return err
		}
	}
	if err := lease.source.Check(setup); err != nil {
		return err
	}
	return setup.Err()
}

func (o *ManagedLiveSource) CheckLiveOutput(ctx context.Context, ref LiveReference) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.check != nil {
		if err := o.check(ctx, ref); err != nil {
			return err
		}
	}
	lease := o.current(ref)
	if lease == nil {
		return nil
	}
	select {
	case <-lease.ready:
		if lease.ctx.Err() != nil || lease.startErr != nil || lease.source == nil {
			return ErrLiveUnavailable
		}
		return lease.source.Check(ctx)
	default:
		return ErrLiveUnavailable
	}
}

func (o *ManagedLiveSource) LiveOutputStatus(ref LiveReference) *LiveOutputStatus {
	lease := o.current(ref)
	if lease == nil {
		return nil
	}
	state := "preparing"
	select {
	case <-lease.ready:
		state = "ready"
		if lease.startErr != nil {
			state = "stopping"
		}
	default:
	}
	if lease.ctx.Err() != nil {
		state = "stopping"
	}
	if lease.stopped.Load() {
		state = "stopped"
	}
	return &LiveOutputStatus{State: state}
}

func (o *ManagedLiveSource) StopLiveOutput(ctx context.Context, ref LiveReference) error {
	lease := o.current(ref)
	if lease == nil {
		return nil
	}
	lease.cancel()
	return lease.stop(ctx)
}

func (l *liveSourceLease) stop(ctx context.Context) error {
	if l.stopped.Load() {
		return nil
	}
	select {
	case <-l.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case l.cleaning <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l.cleaning }()
	if l.stopped.Load() {
		return nil
	}
	if l.source != nil {
		if err := l.source.Stop(ctx); err != nil {
			return err
		}
	}
	l.stopped.Store(true)
	return nil
}
