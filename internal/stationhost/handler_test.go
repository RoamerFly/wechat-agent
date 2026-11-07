package stationhost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/websession"
)

type testLifecycle struct {
	begin func()
	close func(context.Context) error
}

func (l testLifecycle) BeginShutdown()                         { l.begin() }
func (l testLifecycle) CloseContext(ctx context.Context) error { return l.close(ctx) }

func TestHostMountsBeforeRestoreAndDrainsEveryComponent(t *testing.T) {
	owner, err := websession.New("synthetic-owner-password", "", "synthetic_station")
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	step := func(value string) func() { return func() { events = append(events, value) } }
	appError := errors.New("synthetic cleanup result")
	core := testLifecycle{step("stop chat"), func(ctx context.Context) error {
		step("close chat")()
		return ctx.Err()
	}}
	app := testLifecycle{step("stop app"), func(ctx context.Context) error {
		step("close app")()
		if ctx.Err() != nil {
			return appError
		}
		return nil
	}}
	h := New(http.NewServeMux(), owner, apphost.Registry{}, t.TempDir(), core,
		Attachment{
			Mount: func(auth *websession.Store, mux *http.ServeMux) {
				if auth != owner {
					t.Fatal("app did not receive the local owner session")
				}
				mux.HandleFunc("GET /synthetic-app", auth.Protect(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}))
				step("mount app")()
			},
			Restore:   step("restore app"),
			Lifecycle: app,
		},
		Attachment{Mount: func(*websession.Store, *http.ServeMux) { step("mount stateless app")() }},
	)
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/synthetic-app": http.StatusUnauthorized, "/api/local/runtime": http.StatusUnauthorized} {
		reply := httptest.NewRecorder()
		h.ServeHTTP(reply, httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil))
		if reply.Code != want {
			t.Fatalf("%s returned %d, want %d", path, reply.Code, want)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h.BeginShutdown()
	if err := h.CloseContext(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, appError) {
		t.Fatalf("drain lost component results: %v", err)
	}
	if err := h.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"mount app", "mount stateless app", "restore app", "stop chat", "stop app", "close chat", "close app", "close chat", "close app"}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %q, want %q", events, want)
	}
}
