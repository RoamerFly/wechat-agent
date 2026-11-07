//go:build windows

package stationbrowser

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func TestReaderUsesPrivateEdgeAndBlocksSubresources(t *testing.T) {
	if !Available() {
		t.Skip("Microsoft Edge is not installed")
	}
	var (
		mu   sync.Mutex
		hits []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/video":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><head><meta name="description" content="本地公开简介"></head><body><h1>本地公开标题</h1><script>document.querySelector('h1').textContent='脚本已改写标题';fetch('/must-not-run')</script><img src="/must-not-load"></body></html>`))
		default:
			http.Error(w, "unexpected subresource", http.StatusTeapot)
		}
	}))
	defer server.Close()
	target := server.URL + "/video"
	reader := New(filepath.Join(t.TempDir(), "edge-profile"))
	reader.allow = func(candidate string) bool { return candidate == target }
	page, err := reader.Read(t.Context(), target, func() error { return nil })
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if page != (Page{Title: "本地公开标题", Description: "本地公开简介"}) {
		t.Fatalf("Read() page = %#v", page)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 1 || hits[0] != "/video" {
		t.Fatalf("HTTP requests = %#v, want only the top-level document", hits)
	}
}
