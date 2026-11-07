package admin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
)

var errWechatConnectionPaused = errors.New("wechat_connection_paused")

func (m *Manager) connectionPaused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config.ConnectionPaused
}

func (m *Manager) checkWechatSubmission(ctx context.Context) error {
	if m.connectionPaused() {
		return rejectOutboundEvidence(errWechatConnectionPaused)
	}
	return ctx.Err()
}

func (m *Manager) submitWechat(ctx context.Context, native nativeTools, request linuxnative.ToolSend) (linuxnative.ToolStatus, error) {
	if err := m.checkWechatSubmission(ctx); err != nil {
		return linuxnative.ToolStatus{}, err
	}
	return native.Submit(ctx, request)
}

func (w wechatCompatibility) SetConnectionRunning(ctx context.Context, connectionID string, running bool) (apphost.ConnectionLifecycle, error) {
	// The global updater uses the same lifecycle lock when installing its
	// drain ticket. Neither transition may clear the other's barrier.
	w.accounts.lifecycle.Lock()
	defer w.accounts.lifecycle.Unlock()
	item, release, err := w.acquire(connectionID)
	if err != nil {
		return apphost.ConnectionLifecycle{}, err
	}
	defer release()
	m := item.manager
	if m == nil || m.native.Account() != item.record.ID {
		return apphost.ConnectionLifecycle{}, apphost.ErrUnavailable
	}
	if err := m.setConnectionRunning(ctx, running); err != nil {
		return apphost.ConnectionLifecycle{}, err
	}
	cfg := m.runtimeConfig()
	return apphost.ConnectionLifecycle{
		ConnectionRef: wechatConnectionRef(item.record.ID), Worker: m.BotStatus().State,
		WorkerEnabled: cfg.Enabled, SubmissionsPaused: cfg.ConnectionPaused,
	}, nil
}

func (m *Manager) setConnectionRunning(ctx context.Context, running bool) error {
	m.op.Lock()
	defer m.op.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.draining() {
		return apphost.ErrLifecycleConflict
	}
	current := m.runtimeConfig()
	next := current
	next.ConnectionPaused = !running
	if running {
		next.Enabled = true
		if err := validate(next); err != nil {
			return apphost.ErrLifecycleNotReady
		}
		switch m.BotStatus().State {
		case "running", "waiting_login":
			if current.Enabled && !current.ConnectionPaused {
				return nil
			}
		}
	}
	if !running {
		// Close the admission gate before waiting on the worker. Persist the
		// pause first so a process crash cannot resume this account on boot.
		m.mu.Lock()
		m.config.ConnectionPaused = true
		m.mu.Unlock()
	}
	if running && m.stop() != nil {
		return apphost.ErrLifecycleConflict
	}
	if current.ConnectionPaused != next.ConnectionPaused || current.Enabled != next.Enabled {
		data, err := json.MarshalIndent(next, "", "  ")
		if err == nil {
			err = writePrivate(filepath.Join(m.dir, "config.json"), data)
		}
		if err != nil {
			m.mu.Lock()
			m.config.ConnectionPaused = current.ConnectionPaused
			m.mu.Unlock()
			return err
		}
		m.mu.Lock()
		m.config = next
		m.mu.Unlock()
	}
	if running {
		m.start(next)
		return nil
	}
	if err := m.stop(); err != nil {
		return apphost.ErrLifecycleConflict
	}
	return nil
}
