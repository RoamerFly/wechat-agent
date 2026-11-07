package station

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestLocalApplicationRegistryStartsEmptyAndRequiresOwnerSession(t *testing.T) {
	root := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(root, "host"), filepath.Join(root, "workspace"))
	if code, _ := f.request("GET", "/api/apps", nil, false); code != http.StatusUnauthorized {
		t.Fatal("application registry exposed before login", code)
	}
	f.login()
	code, apps := f.request("GET", "/api/apps", nil, true)
	if items, ok := apps["items"].([]any); code != http.StatusOK || !ok || len(items) != 0 {
		t.Fatalf("local station fabricated a connected app: %d %#v", code, apps)
	}
	if code, _ := f.request("GET", "/api/apps/wechat/connections", nil, true); code != http.StatusNotFound {
		t.Fatal("local station exposed an unregistered WeChat connection", code)
	}
}
