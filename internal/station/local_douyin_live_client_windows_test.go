//go:build windows

package station

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalDouyinLiveClientInspectionIsOwnerOnlyAndDoesNotConnect(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"executable": exe}
	if code, _ := f.request("POST", douyinLiveClientInspectRoute, input, false); code != http.StatusForbidden {
		t.Fatalf("missing CSRF accepted: %d", code)
	}
	_, before := f.request("GET", localDouyinWebRoute, nil, false)
	code, view := f.request("POST", douyinLiveClientInspectRoute, input, true)
	if code != http.StatusOK || view["state"] != "metadata_missing" || view["control_available"] != false {
		t.Fatalf("Go test executable must stay an unverified file, not a usable client: %d %v", code, view)
	}
	file, ok := view["file"].(map[string]any)
	if !ok || len(file["sha256"].(string)) != 64 || file["architecture"] != "x64" || file["bytes"].(float64) <= 0 {
		t.Fatal("missing actual file identity", view)
	}
	raw, _ := json.Marshal(view)
	if strings.Contains(string(raw), filepath.Base(exe)) || strings.Contains(string(raw), filepath.Dir(exe)) {
		t.Fatal("response exposed local path", string(raw))
	}
	_, after := f.request("GET", localDouyinWebRoute, nil, false)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("inspection changed browser login or grants")
	}
}

func TestLocalDouyinLiveClientInspectionErrorsStaySpecificAndPrivate(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	bad := filepath.Join(t.TempDir(), "synthetic-client.exe")
	if err := os.WriteFile(bad, []byte("not an executable"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		code int
	}{
		{"", http.StatusBadRequest},
		{`\\server\private\client.exe`, http.StatusBadRequest},
		{bad + ":private", http.StatusBadRequest},
		{bad, http.StatusUnprocessableEntity},
		{filepath.Join(t.TempDir(), "missing-private-client.exe"), http.StatusServiceUnavailable},
	} {
		code, out := f.request("POST", douyinLiveClientInspectRoute, map[string]any{"executable": tc.path}, true)
		if code != tc.code {
			t.Fatalf("got %d, want %d: %v", code, tc.code, out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "private-client") || strings.Contains(string(raw), "synthetic-client") {
			t.Fatal("private path leaked", string(raw))
		}
	}
}
