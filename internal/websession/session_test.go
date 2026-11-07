package websession

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExpiredSessionCannotAuthorizeFollowupOrProtectedRoute(t *testing.T) {
	const origin = "https://owner.example.test"
	store, err := New("synthetic-password", origin, "fixture_session")
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, origin+"/api/login", strings.NewReader(`{"password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", origin)
	res := httptest.NewRecorder()
	store.Login(res, login)
	cookies := res.Result().Cookies()
	if res.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login failed: %d", res.Code)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != int(lifetime/time.Second) {
		t.Fatal("unexpected session cookie attributes")
	}
	next := httptest.NewRequest(http.MethodPost, origin+"/api/apps/fixture/connections", nil)
	next.AddCookie(cookie)
	id, session, ok := store.Lookup(next)
	if !ok || session.CSRF == "" {
		t.Fatal("initial session lookup failed")
	}
	next.Header.Set("Origin", origin)
	next.Header.Set("X-CSRF-Token", session.CSRF)
	session.expires = time.Now().Add(-time.Second)
	store.sessions[id] = session
	res = httptest.NewRecorder()
	store.Protect(func(http.ResponseWriter, *http.Request) { t.Fatal("expired session reached the operation") })(res, next)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("expired session returned %d", res.Code)
	}
	if _, _, ok := store.Lookup(next); ok || len(store.sessions) != 0 {
		t.Fatal("expired session survived a follow-up lookup")
	}
}
