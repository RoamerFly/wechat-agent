package linuxnative

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestNativeChangeGuardKeepsReadsAndCancellation(t *testing.T) {
	var admitted atomic.Bool
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("synthetic-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{URL: server.URL, Account: "one", TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	s.GuardChanges(func(_ context.Context) (func(), error) {
		if !admitted.Load() {
			return nil, errors.New("synthetic cutover conflict")
		}
		return nil, nil
	})
	for _, operation := range []struct {
		method, path string
		allowed      bool
	}{
		{"GET", "/sends/original", true},
		{"POST", "/jobs/original/cancel", true},
		{"POST", "/messages/msg-one/media-transfers", true},
		{"POST", "/messages/msg-one/channels-share-url", true},
		{"POST", "/sends", false},
		{"POST", "/uploads", false},
		{"POST", "/jobs/original/retry", false},
		{"POST", "/tools/media/original/resume-missing", false},
		{"PATCH", "/groups/room", false},
		{"POST", "/messages/msg-one/recall", false},
		{"POST", "/jobs/original/cancel/extra", false},
		{"PUT", "/jobs/original/cancel", false},
	} {
		_, err := s.toolJSON(t.Context(), operation.method, s.accountPath()+operation.path, "", nil, "")
		if operation.allowed && err != nil || !operation.allowed && !errors.Is(err, ErrWriteNotAdmitted) {
			t.Errorf("%s %s: allowed=%v, got %v", operation.method, operation.path, operation.allowed, err)
		}
	}
	if requests.Load() != 4 {
		t.Fatalf("blocked requests reached the server: %d total", requests.Load())
	}
	admitted.Store(true)
	if _, err := s.toolJSON(t.Context(), "POST", s.sendsPath(), "", nil, ""); err != nil || requests.Load() != 5 {
		t.Fatalf("admitted send did not reach native: %v", err)
	}
}
