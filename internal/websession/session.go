// Package websession handles browser sessions without depending on application
// accounts or the legacy management server.
package websession

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const lifetime = 8 * time.Hour

var (
	ErrPassword = errors.New("invalid password")
	ErrOrigin   = errors.New("invalid public origin")
	ErrSession  = errors.New("session expired")
	ErrCurrent  = errors.New("incorrect current password")
)

type Session struct {
	CSRF    string
	expires time.Time
}

type Store struct {
	cookieName string
	origin     string

	mu            sync.Mutex
	passwordHash  [32]byte
	sessions      map[string]Session
	loginWindow   time.Time
	loginAttempts int
}

func ValidPassword(value string) bool {
	return len(value) >= 5 && len(value) <= 128 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func New(password, origin, cookieName string) (*Store, error) {
	if !ValidPassword(password) {
		return nil, ErrPassword
	}
	if origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return nil, ErrOrigin
		}
	}
	if (&http.Cookie{Name: cookieName, Value: "session"}).Valid() != nil {
		return nil, errors.New("invalid session cookie name")
	}
	return &Store{cookieName: cookieName, origin: origin, passwordHash: sha256.Sum256([]byte(password)), sessions: make(map[string]Session)}, nil
}

func (s *Store) sameOrigin(r *http.Request) bool {
	want := s.origin
	if want == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		want = scheme + "://" + r.Host
	}
	return r.Header.Get("Origin") == want
}

func (s *Store) cookie(w http.ResponseWriter, r *http.Request, value string, age int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || strings.HasPrefix(s.origin, "https://"), MaxAge: age,
	})
}

func (s *Store) Lookup(r *http.Request) (string, Session, bool) {
	cookie, err := r.Cookie(s.cookieName)
	if err != nil {
		return "", Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[cookie.Value]
	if !ok || !time.Now().Before(value.expires) {
		delete(s.sessions, cookie.Value)
		return "", Session{}, false
	}
	return cookie.Value, value, true
}

func (s *Store) Protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, value, ok := s.Lookup(r)
		if !ok {
			Fail(w, http.StatusUnauthorized, "请先登录管理页面")
			return
		}
		if r.Method != http.MethodGet {
			header := r.Header.Get("X-CSRF-Token")
			if !s.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(header), []byte(value.CSRF)) != 1 {
				Fail(w, http.StatusForbidden, "页面验证失效，请刷新后重试")
				return
			}
		}
		next(w, r)
	}
}

func (s *Store) Status(w http.ResponseWriter, r *http.Request) {
	_, value, ok := s.Lookup(r)
	if !ok {
		Fail(w, http.StatusUnauthorized, "请先登录管理页面")
		return
	}
	Respond(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": value.CSRF})
}

func (s *Store) Login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		Fail(w, http.StatusForbidden, "不接受跨站登录请求")
		return
	}
	s.mu.Lock()
	if time.Since(s.loginWindow) > time.Minute {
		s.loginWindow = time.Now()
		s.loginAttempts = 0
	}
	s.loginAttempts++
	limited := s.loginAttempts > 10
	s.mu.Unlock()
	if limited {
		Fail(w, http.StatusTooManyRequests, "登录尝试过多，请一分钟后再试")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !Decode(w, r, &input) {
		return
	}
	hash := sha256.Sum256([]byte(input.Password))
	s.mu.Lock()
	if subtle.ConstantTimeCompare(hash[:], s.passwordHash[:]) != 1 {
		s.mu.Unlock()
		Fail(w, http.StatusUnauthorized, "管理密码不正确")
		return
	}
	id, csrf := randomToken(), randomToken()
	now := time.Now()
	for key, value := range s.sessions {
		if !now.Before(value.expires) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= 32 {
		for key := range s.sessions {
			delete(s.sessions, key)
			break
		}
	}
	s.sessions[id] = Session{CSRF: csrf, expires: now.Add(lifetime)}
	s.mu.Unlock()
	s.cookie(w, r, id, int(lifetime/time.Second))
	Respond(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": csrf})
}

func (s *Store) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, cookie.Value)
		s.mu.Unlock()
	}
	s.ClearCookie(w, r)
	Respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Store) ClearCookie(w http.ResponseWriter, r *http.Request) {
	s.cookie(w, r, "", -1)
}

// ChangePassword is called from a CSRF-protected route. It rechecks the session,
// and replaces the password and clears sessions only after persist succeeds.
func (s *Store) ChangePassword(r *http.Request, current, next string, persist func() error) error {
	if !ValidPassword(next) {
		return ErrPassword
	}
	cookie, err := r.Cookie(s.cookieName)
	if err != nil {
		return ErrSession
	}
	oldHash, newHash := sha256.Sum256([]byte(current)), sha256.Sum256([]byte(next))
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.sessions[cookie.Value]
	if !ok || !time.Now().Before(value.expires) {
		return ErrSession
	}
	if subtle.ConstantTimeCompare(oldHash[:], s.passwordHash[:]) != 1 {
		return ErrCurrent
	}
	if err := persist(); err != nil {
		return err
	}
	s.passwordHash = newHash
	clear(s.sessions)
	return nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("system random source unavailable")
	}
	return hex.EncodeToString(b[:])
}
