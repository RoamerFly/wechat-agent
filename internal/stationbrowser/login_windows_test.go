//go:build windows

package stationbrowser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// This test deliberately uses a local page through the unexported opener. It
// verifies profile persistence without requesting the real Douyin site or
// creating a visible desktop window.
func TestLocalLoginProfilePersistsAndIsExclusive(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/set" {
			_, _ = w.Write([]byte(`<!doctype html><script>document.cookie='station_login=ok; Max-Age=3600; path=/'; localStorage.setItem('station-login','ok')</script>`))
			return
		}
		_, _ = w.Write([]byte(`<!doctype html><script>void 0</script>`))
	}))
	defer server.Close()
	profile := t.TempDir() + "\\login-profile"
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	first, err := openDouyinLogin(ctx, profile, server.URL+"/set", false)
	if err != nil {
		t.Fatalf("open local login: %v", err)
	}
	if _, err := openDouyinLogin(ctx, profile, server.URL+"/read", false); !errorsIs(err, ErrDouyinLoginActive) {
		t.Fatalf("second open = %v, want exclusive-profile error", err)
	}
	if got := loginStorage(t, ctx, first); got != "ok|ok" {
		t.Fatalf("first persistent storage = %q", got)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatalf("confirmed login close: %v", err)
	}
	second, err := openDouyinLogin(ctx, profile, server.URL+"/read", false)
	if err != nil {
		t.Fatalf("reopen private profile: %v", err)
	}
	defer second.Close(context.Background())
	if got := loginStorage(t, ctx, second); got != "ok|ok" {
		t.Fatalf("reopened storage = %q, want persisted cookie/localStorage", got)
	}
	other, err := openDouyinLogin(ctx, t.TempDir()+"\\other", server.URL+"/read", false)
	if err != nil {
		t.Fatalf("open isolated profile: %v", err)
	}
	defer other.Close(context.Background())
	if got := loginStorage(t, ctx, other); got != "" && got != "|" {
		t.Fatalf("other profile leaked stored login: %q", got)
	}
}

func TestLoginCancellationClosesAndReleasesProfile(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	profile := t.TempDir() + "\\cancel-profile"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	w, err := openDouyinLogin(ctx, profile, server.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for w.Active() {
		select {
		case <-deadline:
			t.Fatal("cancel did not close login job")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := w.Close(context.Background()); !errorsIs(err, ErrDouyinLoginCloseUnconfirmed) {
		t.Fatalf("close after cancelled job = %v, want unconfirmed", err)
	}
}

// Calling Browser.close directly simulates the CDP pipe EOF produced when the
// visible app is closed by its user. It deliberately does not call
// LoginWindow.Close first, so it exercises the login-only bounded EOF grace.
func TestLoginEOFGraceKeepsConfirmedClose(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed in Program Files")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w, err := openDouyinLogin(ctx, t.TempDir()+"\\eof-profile", server.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.session.Call(ctx, "", "Browser.close", map[string]any{}, nil)
	deadline := time.After(5 * time.Second)
	for w.Active() {
		select {
		case <-deadline:
			t.Fatal("EOF close did not end login browser")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("confirmed EOF close: %v", err)
	}
}

func loginStorage(t *testing.T, ctx context.Context, w *LoginWindow) string {
	t.Helper()
	for until := time.Now().Add(4 * time.Second); time.Now().Before(until); time.Sleep(50 * time.Millisecond) {
		var targets struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		if err := w.session.Call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
			t.Fatal(err)
		}
		for _, target := range targets.TargetInfos {
			if !strings.HasPrefix(target.URL, "http://") {
				continue
			}
			var attached struct {
				SessionID string `json:"sessionId"`
			}
			if err := w.session.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); err != nil {
				t.Fatal(err)
			}
			for until := time.Now().Add(4 * time.Second); time.Now().Before(until); time.Sleep(50 * time.Millisecond) {
				var out struct {
					Result struct {
						Value string `json:"value"`
					} `json:"result"`
				}
				err := w.session.Call(ctx, attached.SessionID, "Runtime.evaluate", map[string]any{"expression": "localStorage.getItem('station-login')+'|'+(document.cookie.includes('station_login=ok')?'ok':'')", "returnByValue": true}, &out)
				if err == nil && out.Result.Value == "ok|ok" {
					return out.Result.Value
				}
			}
			return ""
		}
	}
	t.Fatal("local login target not found")
	return ""
}

// Keep the test independent from the spelling of a wrapped Windows error.
func errorsIs(err, want error) bool {
	return err != nil && (err == want || strings.Contains(err.Error(), want.Error()))
}
