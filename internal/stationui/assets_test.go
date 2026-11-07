package stationui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStandaloneAllowsOnlyStationAssets(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux)
	for _, path := range []string{"/", "/assets/local-home.js", "/assets/local-home.css", "/assets/whale-girl-cutout.png", "/assets/night-sea.webp"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusOK || res.Body.Len() == 0 {
			t.Fatalf("station asset %s: status=%d bytes=%d", path, res.Code, res.Body.Len())
		}
		if !strings.Contains(res.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("station asset %s missed its CSP", path)
		}
	}
	for _, path := range []string{"/local-home", "/assets/app.js"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		if res.Code != http.StatusNotFound {
			t.Fatalf("standalone exposed legacy asset %s: %d", path, res.Code)
		}
	}
}

func TestLegacyAliasIsStillAvailable(t *testing.T) {
	res := httptest.NewRecorder()
	if ServeLegacy(res, "/") || res.Body.Len() != 0 {
		t.Fatal("station shell claimed the legacy management homepage")
	}
	if !ServeLegacy(res, "/local-home") || res.Code != http.StatusOK || res.Body.Len() == 0 {
		t.Fatalf("legacy alias was not served: status=%d bytes=%d", res.Code, res.Body.Len())
	}
}
