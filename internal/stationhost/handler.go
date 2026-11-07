// Package stationhost assembles the local owner's HTTP and shutdown boundary.
package stationhost

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/apphttp"
	"wechat-bot/internal/stationui"
	"wechat-bot/internal/websession"
)

// Lifecycle first seals admission and cancels work, then waits for resources
// to close. CloseContext may be called again after a timeout.
type Lifecycle interface {
	BeginShutdown()
	CloseContext(context.Context) error
}

// Attachment is supplied by trusted in-process application wiring. Mount
// registers owner-protected routes, and Restore starts only after mounting.
// A stateless application can omit Restore and Lifecycle.
type Attachment struct {
	Mount     func(*websession.Store, *http.ServeMux)
	Restore   func()
	Lifecycle Lifecycle
}

type Handler struct {
	http.Handler
	components []Lifecycle
	begin      sync.Once
}

func New(mux *http.ServeMux, owner *websession.Store, registry apphost.Registry, privateDir string, core Lifecycle, apps ...Attachment) *Handler {
	h := &Handler{Handler: mux, components: []Lifecycle{core}}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		websession.Respond(w, http.StatusOK, map[string]any{"ok": true, "mode": "local", "wechat_required": false})
	})
	mux.HandleFunc("POST /api/login", owner.Login)
	mux.HandleFunc("GET /api/session", owner.Protect(owner.Status))
	// The inherited desktop stop pipe is separate from this identity check.
	mux.HandleFunc("GET /api/local/runtime", owner.Protect(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		websession.Respond(w, http.StatusOK, map[string]any{"kind": "local-station", "pid": os.Getpid(), "data_directory": privateDir})
	}))
	apphttp.Register(mux, owner, registry)
	stationui.Register(mux)
	for _, app := range apps {
		app.Mount(owner, mux)
		if app.Lifecycle != nil {
			h.components = append(h.components, app.Lifecycle)
		}
	}
	for _, app := range apps {
		if app.Restore != nil {
			app.Restore()
		}
	}
	return h
}

func (h *Handler) BeginShutdown() {
	h.begin.Do(func() {
		for _, component := range h.components {
			component.BeginShutdown()
		}
	})
}

func (h *Handler) Close() error {
	return h.CloseContext(context.Background())
}

func (h *Handler) CloseContext(ctx context.Context) error {
	h.BeginShutdown()
	var err error
	for _, component := range h.components {
		err = errors.Join(err, component.CloseContext(ctx))
	}
	return err
}
