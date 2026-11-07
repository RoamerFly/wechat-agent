package apphost

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"wechat-bot/internal/rtmprelay"
)

// LiveEncoder is a dedicated host-owned process for exactly one live lease.
type LiveEncoder interface {
	VerifyPublisher(context.Context, *net.TCPConn) error
	Start(context.Context, int, string) error
	Stop(context.Context) error
}

type ManagedLiveOutput struct {
	launch func(context.Context, LiveReference) (LiveEncoder, error)
	check  func(context.Context, LiveReference) error
	mu     sync.Mutex
	lease  *liveOutputLease
}

type liveOutputLease struct {
	ref       LiveReference
	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	cleaning  chan struct{}
	stopped   atomic.Bool
	readyTime time.Time
	encoder   LiveEncoder
	relay     *rtmprelay.Relay
}

const liveOutputMediaTimeout = 20 * time.Second

func NewManagedLiveOutput(launch func(context.Context, LiveReference) (LiveEncoder, error)) *ManagedLiveOutput {
	return NewManagedLiveOutputWithPreflight(launch, nil)
}

// NewManagedLiveOutputWithPreflight rechecks host-owned program authorization
// before platform dispatch and encoder launch, and during an active lease.
// The callback must not start an encoder or perform an external action.
func NewManagedLiveOutputWithPreflight(launch func(context.Context, LiveReference) (LiveEncoder, error), check func(context.Context, LiveReference) error) *ManagedLiveOutput {
	if launch == nil {
		return nil
	}
	return &ManagedLiveOutput{launch: launch, check: check}
}

func (o *ManagedLiveOutput) StartLiveOutput(ctx context.Context, ref LiveReference, ingest LiveIngest) (err error) {
	o.mu.Lock()
	if ref.ID == "" || ctx.Err() != nil || !time.Now().Before(ref.Deadline) ||
		(o.lease != nil && (!o.lease.stopped.Load() || sameLiveReference(o.lease.ref, ref))) {
		o.mu.Unlock()
		return ErrLiveUnavailable
	}
	lease := &liveOutputLease{ref: ref, ready: make(chan struct{}), cleaning: make(chan struct{}, 1)}
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
		lease.readyTime = time.Now()
		close(lease.ready)
		stopSetup()
		cancel()
		if err != nil {
			lease.cancel()
		}
	}()
	if o.check != nil {
		if err := o.check(setup, ref); err != nil || setup.Err() != nil {
			return ErrLiveUnavailable
		}
	}
	lease.encoder, err = o.launch(setup, ref)
	if err != nil || lease.encoder == nil || setup.Err() != nil {
		return ErrLiveUnavailable
	}
	lease.relay, err = rtmprelay.Open(lease.ctx, rtmprelay.Config{
		Destination: ingest.Destination, DialContext: ingest.DialContext, VerifyPeer: lease.encoder.VerifyPublisher,
	})
	if err != nil {
		return ErrLiveUnavailable
	}
	if err := lease.encoder.Start(setup, lease.relay.Port(), lease.relay.Ticket()); err != nil || setup.Err() != nil {
		return ErrLiveUnavailable
	}
	return nil
}

func (o *ManagedLiveOutput) current(ref LiveReference) *liveOutputLease {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lease == nil || !sameLiveReference(o.lease.ref, ref) {
		return nil
	}
	return o.lease
}

// CheckLiveOutput is also called before remote start. At that point no media
// can exist; the selected platform must first return its private ingest route.
func (o *ManagedLiveOutput) CheckLiveOutput(ctx context.Context, ref LiveReference) error {
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
	if lease.ctx.Err() != nil {
		return ErrLiveUnavailable
	}
	select {
	case <-lease.ready:
		if lease.relay == nil {
			return ErrLiveUnavailable
		}
		status := lease.relay.Status()
		if status.State == "disconnected" || status.State == "stopped" ||
			(status.MediaMessages == 0 && time.Since(lease.readyTime) >= liveOutputMediaTimeout) {
			return ErrLiveUnavailable
		}
	default:
	}
	return nil
}

func (o *ManagedLiveOutput) LiveOutputStatus(ref LiveReference) *LiveOutputStatus {
	lease := o.current(ref)
	if lease == nil {
		return nil
	}
	status := &LiveOutputStatus{State: "preparing"}
	select {
	case <-lease.ready:
		if lease.relay != nil {
			current := lease.relay.Status()
			status.State, status.MediaMessages = current.State, current.MediaMessages
		}
	default:
	}
	if lease.stopped.Load() {
		status.State = "stopped"
	} else if lease.ctx.Err() != nil {
		status.State = "stopping"
	}
	return status
}

func (o *ManagedLiveOutput) StopLiveOutput(ctx context.Context, ref LiveReference) error {
	lease := o.current(ref)
	if lease == nil {
		// On restart an owned Windows job and its in-process relay have already
		// lost their host handles. Never create a new process to verify an old one.
		return nil
	}
	lease.cancel()
	return lease.stop(ctx)
}

func (l *liveOutputLease) stop(ctx context.Context) error {
	select {
	case <-l.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case l.cleaning <- struct{}{}:
		defer func() { <-l.cleaning }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if l.stopped.Load() {
		return nil
	}
	var err error
	if l.relay != nil {
		err = l.relay.Close(ctx)
	}
	if l.encoder != nil {
		err = errors.Join(err, l.encoder.Stop(ctx))
	}
	if err == nil {
		l.stopped.Store(true)
	}
	return err
}
