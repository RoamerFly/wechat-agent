package apphost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"wechat-bot/internal/pluginhost"
)

func (l *LiveController) update(ctx context.Context, id string, change func(*liveState) error) (liveState, error) {
	for range 8 {
		r, err := l.store.LiveSession(ctx, id)
		s, err := readLive(r, err)
		if err != nil {
			return s, err
		}
		if !r.Active {
			return s, nil
		}
		before := s
		if err = change(&s); err != nil {
			return before, err
		}
		if s == before {
			return s, nil
		}
		if s.State == "stopping" && s.RemoteEnded && s.LocalStopped {
			s.State = "ended"
		}
		raw, _ := json.Marshal(s)
		_, err = l.store.UpdateLiveSession(ctx, id, r.Revision, s.State != "ended", raw)
		if !errors.Is(err, pluginhost.ErrRevision) {
			return s, err
		}
	}
	return liveState{}, ErrLiveConflict
}

func (l *LiveController) provider(ref LiveReference) (LiveProvider, error) {
	entry, ok := l.registry.Lookup(ref.Target.AppID)
	if !ok || entry.InstallationID != ref.Target.InstallationID || entry.Registration != ref.Registration {
		return nil, ErrLiveUnavailable
	}
	provider, ok := entry.Provider.(LiveProvider)
	if !ok {
		return nil, ErrLiveUnavailable
	}
	return provider, nil
}

func (l *LiveController) checkGrant(ctx context.Context, ref LiveReference) string {
	if !l.now().Before(ref.Deadline) {
		return "deadline"
	}
	entry, target, provider, err := l.resolve(ctx, ref.Target.AppID, ref.Target.ConnectionID)
	if err != nil || target != ref.Target || entry.Registration != ref.Registration || liveMediaMode(provider, target) != ref.MediaMode {
		return "authorization-changed"
	}
	grant, err := provider.LiveGrant(ctx, target)
	if err != nil || grant.Version != ref.GrantVersion || !grantCovers(grant, l.now(), ref.Deadline) {
		return "authorization-changed"
	}
	return ""
}

func (l *LiveController) check(ctx context.Context, ref LiveReference) string {
	if reason := l.checkGrant(ctx, ref); reason != "" {
		return reason
	}
	output := l.outputFor(ref.MediaMode)
	if output == nil || output.CheckLiveOutput(ctx, ref) != nil {
		return "output-unavailable"
	}
	return ""
}

func (l *LiveController) prepareSource(ctx context.Context, ref LiveReference) string {
	// Use the same ownership gate as StopLiveOutput. A cancelled prepare
	// may still hold local resources until it returns, so recovery cannot
	// mark those resources stopped while prepare is in flight.
	select {
	case l.local <- struct{}{}:
	case <-ctx.Done():
		return "start-interrupted"
	}
	defer func() { <-l.local }()
	record, err := readLive(l.store.LiveSession(ctx, ref.ID))
	if err != nil || !sameLiveReference(record.Reference, ref) || record.State != "starting" || record.DispatchClaimed || ctx.Err() != nil {
		return "start-interrupted"
	}
	if l.source == nil || l.source.PrepareLiveSource(ctx, ref) != nil {
		return "output-unavailable"
	}
	return ""
}

func (l *LiveController) dispatch(ctx context.Context, s liveState, provider LiveProvider) (string, LiveReceipt) {
	check := l.check
	if s.Reference.MediaMode == LiveMediaLocalSource {
		check = l.checkGrant
	}
	if reason := check(ctx, s.Reference); reason != "" {
		return reason, LiveReceipt{}
	}
	if s.Reference.MediaMode == LiveMediaLocalSource {
		if reason := l.prepareSource(ctx, s.Reference); reason != "" {
			return reason, LiveReceipt{}
		}
	}
	if !l.remote.TryLock() {
		return "start-interrupted", LiveReceipt{}
	}
	defer l.remote.Unlock()
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return "shutdown", LiveReceipt{}
	}
	if s.Reference.MediaMode == LiveMediaLocalSource {
		if reason := l.check(ctx, s.Reference); reason != "" {
			return reason, LiveReceipt{}
		}
	}
	current, err := l.update(ctx, s.Reference.ID, func(state *liveState) error {
		if state.State != "starting" || state.DispatchClaimed || ctx.Err() != nil || !l.now().Before(state.Reference.Deadline) {
			return ErrLiveConflict
		}
		state.DispatchClaimed = true
		return nil
	})
	if err != nil || current.State != "starting" || ctx.Err() != nil {
		return "start-interrupted", LiveReceipt{}
	}
	platformCtx, cancel := context.WithTimeout(ctx, liveRemoteTimeout)
	result, err := provider.StartLive(platformCtx, s.Reference)
	platformErr := platformCtx.Err()
	cancel()
	receipt := result.Receipt
	if err != nil || !validLiveReceipt(s, receipt) || receipt.State == "unknown" {
		return "start-unconfirmed", LiveReceipt{}
	}
	if receipt.State == "ended" {
		return "platform-ended", receipt
	}
	if platformErr != nil || ctx.Err() != nil || !l.now().Before(s.Reference.Deadline) {
		return "start-interrupted", receipt
	}
	if s.Reference.MediaMode == LiveMediaLocalSource {
		if result.Ingest != nil {
			return "output-unavailable", receipt
		}
	} else if output, ok := l.output.(liveStreamOutput); ok {
		if result.Ingest == nil {
			return "output-unavailable", receipt
		}
		if reason := l.checkGrant(ctx, s.Reference); reason != "" {
			return reason, receipt
		}
		if err := output.StartLiveOutput(ctx, s.Reference, *result.Ingest); err != nil {
			return "output-unavailable", receipt
		}
	} else if result.Ingest != nil {
		return "output-unavailable", receipt
	}
	return "", receipt
}

func sameLiveReference(a, b LiveReference) bool {
	return a.ID == b.ID && a.Target == b.Target && a.Registration == b.Registration &&
		a.GrantVersion == b.GrantVersion && a.Deadline.Equal(b.Deadline) &&
		a.ProgramArtifactID == b.ProgramArtifactID && a.ProgramSHA256 == b.ProgramSHA256 && a.ProgramLoop == b.ProgramLoop &&
		a.MediaMode == b.MediaMode
}

func validLiveReceipt(s liveState, r LiveReceipt) bool {
	ref := s.Reference
	if !sameLiveReference(ref, r.Reference) {
		return false
	}
	if s.RoomID != "" && r.RoomID != s.RoomID || r.RoomID != "" && !liveIdentifier.MatchString(r.RoomID) {
		return false
	}
	switch r.State {
	case "live":
		return r.RoomID != ""
	case "ended":
		return true
	case "unknown":
		return s.RoomID == r.RoomID
	default:
		return false
	}
}

func (l *LiveController) applyLiveReceipt(ctx context.Context, s liveState, r LiveReceipt) (liveState, error) {
	return l.update(ctx, s.Reference.ID, func(current *liveState) error {
		if !current.DispatchClaimed || !validLiveReceipt(*current, r) {
			return ErrLiveConflict
		}
		switch r.State {
		case "live":
			if current.RemoteEnded {
				return ErrLiveConflict
			}
			current.RoomID = r.RoomID
			if current.StopReason == "" {
				current.State = "live"
			}
		case "ended":
			current.RoomID = r.RoomID
			current.RemoteEnded = true
			markLiveStop(current, "platform-ended")
		}
		return nil
	})
}

func markLiveStop(current *liveState, reason string) {
	current.State = "stopping"
	if current.StopReason == "" {
		current.StopReason = reason
	}
	if !current.DispatchClaimed {
		current.RemoteEnded = true
		// A local source can already exist before the platform dispatch
		// claim. It still needs an idempotent StopLiveOutput.
		if current.Reference.MediaMode != LiveMediaLocalSource {
			current.LocalStopped = true
		}
	}
}

func (l *LiveController) requestStop(ctx context.Context, s liveState, reason string) (liveState, error) {
	current, err := l.update(ctx, s.Reference.ID, func(current *liveState) error {
		markLiveStop(current, reason)
		return nil
	})
	l.mu.Lock()
	if l.startID == s.Reference.ID {
		l.cancel()
	}
	if l.managed.ID == s.Reference.ID && l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.mu.Unlock()
	if err != nil {
		// Persistence may be unavailable. Still stop only this owned output,
		// and leave the original durable operation for startup reconciliation.
		l.stopOutput(context.WithoutCancel(ctx), s.Reference, false)
	}
	return current, err
}

func (l *LiveController) stopOutput(ctx context.Context, ref LiveReference, wait bool) bool {
	output := l.outputFor(ref.MediaMode)
	if output == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, liveRemoteTimeout)
	defer cancel()
	if wait {
		select {
		case l.local <- struct{}{}:
		case <-ctx.Done():
			return false
		}
	} else {
		select {
		case l.local <- struct{}{}:
		default:
			return false
		}
	}
	defer func() { <-l.local }()
	return output.StopLiveOutput(ctx, ref) == nil
}

func (l *LiveController) finishStop(ctx context.Context, s liveState, waitLocal bool) (liveState, error) {
	if s.State != "stopping" {
		return s, nil
	}
	var err error
	if !s.LocalStopped && l.stopOutput(ctx, s.Reference, waitLocal) {
		s, err = l.update(ctx, s.Reference.ID, func(current *liveState) error {
			current.LocalStopped = true
			return nil
		})
		if err != nil {
			return s, err
		}
	}
	if s.RemoteEnded || !l.remote.TryLock() {
		return s, nil
	}
	defer l.remote.Unlock()
	s, err = readLive(l.store.LiveSession(ctx, s.Reference.ID))
	if err != nil || s.State != "stopping" || s.RemoteEnded {
		return s, err
	}
	provider, err := l.provider(s.Reference)
	if err != nil {
		return s, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, liveRemoteTimeout)
	defer cancel()
	receipt, err := provider.StopLive(callCtx, s.Reference)
	if err == nil && validLiveReceipt(s, receipt) && receipt.State != "unknown" {
		s, err = l.applyLiveReceipt(ctx, s, receipt)
		if err != nil || s.RemoteEnded {
			return s, err
		}
	}
	receipt, err = provider.QueryLive(callCtx, s.Reference)
	if err != nil || !validLiveReceipt(s, receipt) {
		return s, nil
	}
	return l.applyLiveReceipt(ctx, s, receipt)
}

func (l *LiveController) stopSession(ctx context.Context, s liveState, reason string) (liveState, error) {
	current, err := l.requestStop(ctx, s, reason)
	if err != nil {
		return s, err
	}
	return l.finishStop(ctx, current, false)
}

func (l *LiveController) Stop(ctx context.Context, owner, id string) (LiveView, error) {
	s, err := readLive(l.store.LiveSession(ctx, id))
	s, err = ownLive(s, owner, err)
	if err != nil {
		return LiveView{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*liveRemoteTimeout)
	defer cancel()
	s, err = l.stopSession(ctx, s, "owner-stop")
	return l.view(s), err
}

func (l *LiveController) inspect(ctx context.Context, s liveState) (liveState, error) {
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if s.State == "stopping" {
		return l.finishStop(ctx, s, false)
	}
	if s.State == "ended" {
		return s, nil
	}
	check := l.check
	if s.State == "starting" && s.Reference.MediaMode == LiveMediaLocalSource {
		// Preparation owns the source readiness check. The periodic
		// supervisor can still revoke the grant without mistaking an
		// in-progress preparation for a failed local output.
		check = l.checkGrant
	}
	if reason := check(ctx, s.Reference); reason != "" {
		return l.stopSession(ctx, s, reason)
	}
	if s.State == "starting" {
		l.mu.Lock()
		starting := l.startID == s.Reference.ID
		l.mu.Unlock()
		if !starting {
			current, err := readLive(l.store.LiveSession(ctx, s.Reference.ID))
			if err != nil || current.State != "starting" {
				return current, err
			}
			return l.stopSession(ctx, current, "start-unconfirmed")
		}
		return s, nil
	}
	if !l.remote.TryLock() {
		return s, nil
	}
	provider, err := l.provider(s.Reference)
	if err != nil {
		l.remote.Unlock()
		return l.stopSession(ctx, s, "authorization-changed")
	}
	callCtx, cancel := context.WithTimeout(ctx, liveRemoteTimeout)
	receipt, err := provider.QueryLive(callCtx, s.Reference)
	cancel()
	l.remote.Unlock()
	if err != nil || !validLiveReceipt(s, receipt) || receipt.State == "unknown" {
		return l.stopSession(ctx, s, "platform-unreachable")
	}
	if receipt.State == "live" {
		if reason := l.checkGrant(ctx, s.Reference); reason != "" {
			s, err = l.requestStop(ctx, s, reason)
			if err != nil {
				return s, err
			}
		}
	}
	s, err = l.applyLiveReceipt(ctx, s, receipt)
	if err != nil || s.State != "stopping" {
		return s, err
	}
	return l.finishStop(ctx, s, false)
}

func (l *LiveController) Reconcile(ctx context.Context, owner, id string) (LiveView, error) {
	s, err := readLive(l.store.LiveSession(ctx, id))
	s, err = ownLive(s, owner, err)
	if err != nil {
		return LiveView{}, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*liveRemoteTimeout)
	defer cancel()
	s, err = l.inspect(ctx, s)
	return l.view(s), err
}

func (l *LiveController) active(ctx context.Context) (liveState, error) {
	s, err := readLive(l.store.ActiveLiveSession(ctx))
	if err != nil && !errors.Is(err, ErrNotFound) {
		l.mu.Lock()
		ref := l.managed
		l.mu.Unlock()
		if ref.ID != "" {
			l.stopOutput(context.WithoutCancel(ctx), ref, false)
		}
	}
	return s, err
}

// Run recovers the durable slot without replaying StartLive, then supervises
// the active session until the station's lifecycle context ends.
func (l *LiveController) Run(ctx context.Context) {
	recovery := true
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		checkCtx, cancel := context.WithTimeout(ctx, 3*liveRemoteTimeout)
		s, err := l.active(checkCtx)
		if err == nil {
			l.mu.Lock()
			managed := l.managed.ID == s.Reference.ID
			l.mu.Unlock()
			if recovery && !managed {
				_, err = l.stopSession(checkCtx, s, "restart")
			} else {
				_, err = l.inspect(checkCtx, s)
			}
		}
		idle := errors.Is(err, ErrNotFound)
		if err == nil || idle {
			recovery = false
		}
		cancel()
		if idle {
			select {
			case <-ctx.Done():
			case <-l.wake:
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// BeginShutdown seals admission without waiting for the ledger or a provider.
func (l *LiveController) BeginShutdown() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

func (l *LiveController) Close(ctx context.Context) error {
	l.BeginShutdown()
	l.mu.Lock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.mu.Unlock()
	s, err := l.active(ctx)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	current, err := l.requestStop(ctx, s, "shutdown")
	if err == nil {
		current, err = l.finishStop(ctx, current, true)
	}
	if err == nil && !current.LocalStopped {
		return ErrLiveConflict
	}
	return err
}
