package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

func enableSyntheticDouyinWebRead(t *testing.T, f *localHomeFixture) map[string]any {
	t.Helper()
	initializeDouyinWeb(t, f)
	code, view := f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": 0, "approve_profile": true}, true)
	if code != 200 {
		t.Fatalf("begin: %d %v", code, view)
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "confirm", "revision": view["revision"], "attempt_id": view["attempt_id"]}, true)
	if code != 200 {
		t.Fatalf("confirm: %d %v", code, view)
	}
	if view["read_enabled"] != false {
		t.Fatal("login granted webpage access")
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true}, true)
	if code != 200 || view["read_enabled"] != true {
		t.Fatalf("opt in: %d %v", code, view)
	}
	return view
}

func syntheticDouyinWebReadInput(limit int) map[string]any {
	return map[string]any{"app_id": "douyin", "connection_id": douyinapp.WebReadConnectionID, "content_id": douyinapp.WebReadContentID, "field": "page_text", "limit": limit}
}

func TestLocalDouyinWebReadRequiresIndependentPermission(t *testing.T) {
	f, _ := newLocalDouyinWebFixture(t)
	initializeDouyinWeb(t, f)
	input := map[string]any{"action": "set_read", "revision": 0, "allow_read": true}
	if code, _ := f.request("POST", localDouyinWebRoute, input, false); code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if code, _ := f.request("POST", localDouyinWebRoute, input, true); code != 409 {
		t.Fatal("unconfirmed profile obtained read grant")
	}
	delete(input, "allow_read")
	if code, _ := f.request("POST", localDouyinWebRoute, input, true); code != 400 {
		t.Fatal("missing read decision accepted")
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2), true); code != 403 {
		t.Fatalf("unapproved read = %d", code)
	}
}

func TestLocalDouyinWebReadThroughObserverPaginatesRefreshesAndRevokes(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	reads := 0
	body := "甲乙丙丁🙂戊"
	m.readPage = func(ctx context.Context, profile string, authorize func() error) (stationbrowser.DouyinWebPage, error) {
		reads++
		if !strings.HasPrefix(profile, filepath.Join(f.dir, "apps", "douyin", "web-login")) || strings.HasPrefix(profile, f.root) {
			t.Error("browser left its private profile scope")
		}
		if err := authorize(); err != nil {
			return stationbrowser.DouyinWebPage{}, err
		}
		return stationbrowser.DouyinWebPage{Title: "合成首页", Text: body}, nil
	}
	code, targets := f.request("GET", "/api/local/apps/observation", nil, false)
	items := localAppObservationItems(targets)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["connection_id"] != douyinapp.WebReadConnectionID {
		t.Fatalf("discovery: %d %v", code, targets)
	}
	input := syntheticDouyinWebReadInput(len([]rune("页面标题：合成首页\n")) + 2)
	if code, _ := f.request("POST", "/api/local/apps/observation", input, false); code != 403 {
		t.Fatal("read CSRF bypass")
	}
	code, first := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || first["complete"] != false || reads != 1 {
		t.Fatalf("first: %d %v reads=%d", code, first, reads)
	}
	chunks := first["chunks"].([]any)
	var firstText string
	for _, chunk := range chunks {
		firstText += chunk.(map[string]any)["text"].(string)
	}
	if !strings.HasSuffix(firstText, "甲乙") {
		t.Fatal(first)
	}
	input["limit"] = 100000 // explicit request is not silently clamped to 2048
	code, last := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || last["complete"] != true || last["resumed"] != true || reads != 1 {
		t.Fatalf("resume: %d %v reads=%d", code, last, reads)
	}
	if got := last["chunks"].([]any)[0].(map[string]any)["text"]; got != "丙丁🙂戊" {
		t.Fatal("Unicode continuation changed:", got)
	}
	_, cached := f.request("POST", "/api/local/apps/observation", input, true)
	if cached["cached"] != true || reads != 1 {
		t.Fatal("completed read did not reuse verified snapshot")
	}
	body = "刷新后的实际页面"
	input["refresh"] = true
	code, fresh := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || fresh["cached"] != false || reads != 2 {
		t.Fatalf("refresh: %d %v reads=%d", code, fresh, reads)
	}
	raw, _ := json.Marshal(fresh)
	if !strings.Contains(string(raw), body) || strings.Contains(string(raw), f.dir) || strings.Contains(string(raw), "Cookies") {
		t.Fatal("invalid page result or private path leakage")
	}
	code, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": false}, true)
	if code != 200 || view["read_enabled"] != false {
		t.Fatal(view)
	}
	delete(input, "refresh")
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 || reads != 2 {
		t.Fatal("revoked cached material escaped")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "set_read", "revision": view["revision"], "allow_read": true}, true)
	code, again := f.request("POST", "/api/local/apps/observation", input, true)
	if code != 200 || again["cached"] != false || reads != 3 {
		t.Fatal("new grant reused the old cache")
	}
	_, view = f.request("POST", localDouyinWebRoute, map[string]any{"action": "begin", "revision": view["revision"], "approve_profile": true}, true)
	if view["read_enabled"] != false {
		t.Fatal("relogin retained old account access")
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", input, true); code != 403 {
		t.Fatal("relogin reused the former account grant")
	}
}

func TestLocalDouyinWebReadRevocationDuringReadDiscardsEvidence(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	view := enableSyntheticDouyinWebRead(t, f)
	identity, err := m.home.identity.Read()
	if err != nil {
		t.Fatal(err)
	}
	m.readPage = func(ctx context.Context, _ string, authorize func() error) (stationbrowser.DouyinWebPage, error) {
		if err := authorize(); err != nil {
			return stationbrowser.DouyinWebPage{}, err
		}
		allow := false
		_, code, err := m.apply(ctx, identity.FishID, &localDouyinWebInput{Action: "set_read", Revision: uint64(view["revision"].(float64)), AllowRead: &allow})
		if err != nil || code != 200 {
			t.Errorf("revoke: %d %v", code, err)
		}
		// Simulate a browser that finished at the same time; the host must repeat
		// the grant check even when the callback returns apparent success.
		return stationbrowser.DouyinWebPage{Title: "不应返回", Text: "撤权之后的私有文字"}, nil
	}
	code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true)
	raw, _ := json.Marshal(result)
	if code != 403 || strings.Contains(string(raw), "撤权之后") {
		t.Fatalf("revoked evidence: %d %s", code, raw)
	}
}

func TestLocalDouyinWebReadErrorsRemainActionable(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	enableSyntheticDouyinWebRead(t, f)
	for _, test := range []struct {
		err    error
		status int
	}{
		{stationbrowser.ErrDouyinLoginActive, 409}, {stationbrowser.ErrDouyinWebLoginRequired, 409},
		{stationbrowser.ErrDouyinWebTextSize, 413}, {stationbrowser.ErrPageUnavailable, 404},
		{context.DeadlineExceeded, 504},
		{errors.New("synthetic internal private-path failure"), 503},
	} {
		m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
			return stationbrowser.DouyinWebPage{}, test.err
		}
		code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true)
		raw, _ := json.Marshal(result)
		if code != test.status || strings.Contains(string(raw), "private-path") {
			t.Fatalf("error mapping: %v -> %d %s", test.err, code, raw)
		}
	}
}

func TestLocalDouyinWebReadUsesOriginalGrantAfterRestart(t *testing.T) {
	f, m := newLocalDouyinWebFixture(t)
	enableSyntheticDouyinWebRead(t, f)
	m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
		return stationbrowser.DouyinWebPage{Text: "已经取得的完整合成正文"}, nil
	}
	if code, _ := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true); code != 200 {
		t.Fatal(code)
	}
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err := apphost.New(douyinapp.New(filepath.Join(f.dir, "apps", "douyin")).Registration())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewLocalHomeHandlerWithApps(f.dir, f.root, "synthetic-local-password", "", registry)
	if err != nil {
		t.Fatal(err)
	}
	next := newLocalAppObservationFixtureWithHandler(t, handler, f.dir, f.root)
	handler.home.douyinWeb.available = func() bool { return true }
	handler.home.douyinWeb.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
		t.Error("restart unnecessarily reread the site")
		return stationbrowser.DouyinWebPage{}, nil
	}
	next.login()
	code, result := next.request(http.MethodPost, "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true)
	if code != 200 || result["cached"] != true {
		t.Fatalf("restart: %d %v", code, result)
	}
}

func TestLocalDouyinWebSharedRegistrationCannotRetargetAnotherHome(t *testing.T) {
	base := t.TempDir()
	provider := douyinapp.New(filepath.Join(base, "unused-official-settings"))
	registry, err := apphost.New(provider.Registration())
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []*localHomeFixture
	for _, name := range []string{"first", "second"} {
		data, workspace := filepath.Join(base, name, "host"), filepath.Join(base, name, "workspace")
		handler, err := NewLocalHomeHandlerWithApps(data, workspace, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		f := newLocalAppObservationFixtureWithHandler(t, handler, data, workspace)
		m := handler.home.douyinWeb
		m.available = func() bool { return true }
		m.open = func(ctx context.Context, _ string) (localDouyinLoginWindow, error) {
			w := new(syntheticDouyinLoginWindow)
			w.active.Store(true)
			context.AfterFunc(ctx, func() { w.active.Store(false) })
			return w, nil
		}
		m.readPage = func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error) {
			return stationbrowser.DouyinWebPage{Text: name + " private visible page"}, nil
		}
		enableSyntheticDouyinWebRead(t, f)
		fixtures = append(fixtures, f)
	}
	for i, f := range fixtures {
		code, result := f.request("POST", "/api/local/apps/observation", syntheticDouyinWebReadInput(2048), true)
		if code != 200 {
			t.Fatalf("home %d: %d %v", i, code, result)
		}
		text := result["chunks"].([]any)[0].(map[string]any)["text"]
		want := []string{"first private visible page", "second private visible page"}[i]
		if text != want {
			t.Fatalf("home %d read another scope: %v", i, text)
		}
	}
}
