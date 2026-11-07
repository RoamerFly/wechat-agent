package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
)

func TestStationWechatContactsOwnerAndLocalAgentIsolation(t *testing.T) {
	base := t.TempDir()
	host, workspace := filepath.Join(base, "host"), filepath.Join(base, "workspace")
	nativeRoot := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(nativeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeRoot, "api-token"), []byte("synthetic-directory-token"), 0600); err != nil {
		t.Fatal(err)
	}
	const nativeCursor = "native-private-directory-cursor"
	var requests atomic.Int32
	var oneSignedOut atomic.Bool
	started, release := make(chan struct{}, 1), make(chan struct{})
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-directory-token" {
			t.Error("unexpected unauthenticated request or mutation")
			http.Error(w, "unexpected request", http.StatusForbidden)
			return
		}
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.URL.Path {
		case "/v1/status":
			out(map[string]any{"pid": 7098, "dataDir": nativeRoot, "serverId": "synthetic-contacts", "ownershipGeneration": "first"})
		case "/v1/accounts":
			out(map[string]any{"items": []map[string]any{
				{"id": "one", "alias": "synthetic-one", "authenticated": !oneSignedOut.Load(), "connection": "online"},
				{"id": "two", "alias": "synthetic-two", "authenticated": true, "connection": "online"},
			}})
		case "/v1/accounts/one/contacts", "/v1/accounts/two/contacts":
			requests.Add(1)
			q := r.URL.Query()
			account := strings.Split(r.URL.Path, "/")[3]
			if q.Get("search") == "blocked" {
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			var next any = nativeCursor
			id := "contact-" + account
			if q.Get("cursor") != "" {
				if q.Get("cursor") != nativeCursor {
					t.Error("raw host cursor reached native")
				}
				if q.Get("limit") != "2" {
					t.Error("continuation did not honor adjusted limit")
				}
				next, id = nil, id+"-next"
			} else if q.Get("search") == "大鱼" && q.Get("limit") != "200" || q.Get("search") != "大鱼" && q.Get("limit") != "50" {
				t.Error("native effective page size did not match request", q)
			}
			out(map[string]any{
				"items":      []map[string]any{{"id": id, "username": id, "remark": "合成联系人-" + account, "displayName": "同名昵称", "alias": "synthetic-hidden-alias"}},
				"nextCursor": next, "coverage": map[string]any{"partial": true, "databases": []string{"synthetic-private-profile-path"}},
			})
		default:
			t.Error("unexpected native route", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(native.Close)
	open := func() *localHomeFixture {
		entry, err := wechatapp.StationRegistration(t.Context(), native.URL, 7098, host)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := apphost.New(entry)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := NewLocalHomeHandlerWithApps(host, workspace, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(func() { _ = handler.Close() })
		t.Cleanup(server.Close)
		jar, _ := cookiejar.New(nil)
		return &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, handler: handler}
	}
	f := open()
	one, two := wechatapp.ConnectionID("one"), wechatapp.ConnectionID("two")
	path := func(id string) string { return "/api/apps/wechat/connections/" + id }
	request := func(f *localHomeFixture, method, path string, body any, csrf bool, status int) map[string]any {
		t.Helper()
		code, out := f.request(method, path, body, csrf)
		if code != status {
			t.Fatalf("%s %s: %d want %d, %#v", method, path, code, status, out)
		}
		return out
	}
	request(f, "GET", path(one)+"/contacts", nil, false, 401)
	if requests.Load() != 0 {
		t.Fatal("unauthenticated directory access")
	}
	f.login()
	request(f, "POST", "/api/local/home", map[string]string{"action": "initialize"}, true, 200)
	grant := func(enable bool) {
		f.t.Helper()
		out := request(f, "PUT", path(one)+"/policy", map[string]any{"reply_threads": []string{}, "contact_read": enable}, true, 200)
		conn := out["connection"].(map[string]any)
		value, _ := conn["configured_policy"].(map[string]any)["contact_read"].(bool)
		if conn["worker"] != "disabled" || value != enable {
			t.Fatal("directory permission changed the worker or was lost", conn)
		}
	}
	twoPath := path(two) + "/contacts"
	onePath := path(one) + "/contacts"
	first := request(f, "GET", onePath+"?search="+url.QueryEscape("大鱼")+"&limit=500", nil, false, 200)
	token := first["next_cursor"].(string)
	data, _ := json.Marshal(first)
	if first["requested_limit"] != float64(500) || first["effective_limit"] != float64(200) || first["partial"] != true || !strings.Contains(string(data), "合成联系人-one") ||
		strings.Contains(string(data), "synthetic-private") || strings.Contains(string(data), "hidden-alias") || strings.Contains(token, nativeCursor) {
		t.Fatal("incorrect directory projection", first)
	}
	before := requests.Load()
	request(f, "GET", twoPath+"?search="+url.QueryEscape("大鱼")+"&limit=2&cursor="+token, nil, false, 400)
	request(f, "GET", onePath+"?search=changed&limit=2&cursor="+token, nil, false, 400)
	request(f, "GET", onePath+"?account_id=two", nil, false, 400)
	if before != requests.Load() {
		t.Fatal("invalid scope reached native directory")
	}
	second := request(f, "GET", onePath+"?search="+url.QueryEscape("大鱼")+"&limit=2&cursor="+token, nil, false, 200)
	if second["has_more"] != false || second["items"].([]any)[0].(map[string]any)["id"] != "contact-one-next" {
		t.Fatal(second)
	}
	if other := request(f, "GET", twoPath, nil, false, 200); other["items"].([]any)[0].(map[string]any)["id"] != "contact-two" {
		t.Fatal("owner page selected a different connection", other)
	}
	request(f, "PUT", path(one)+"/policy", map[string]any{"reply_threads": []string{}, "contact_read": true}, false, 403)
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID = "synthetic-direct-tool-turn"
	localTools := f.handler.chat.tools(store, in)
	call := func(tgt, query, cursor string) (string, error) {
		raw, _ := json.Marshal(stationContactQuery{Connection: tgt, Search: query, Cursor: cursor, Limit: 50})
		return localTools.Call(t.Context(), wechatapp.StationContactsToolName, raw)
	}
	if _, err := call(one, "", ""); !errors.Is(err, errContactDenied) {
		t.Fatal("default allowed Agent directory access", err)
	}
	grant(true)
	unchanged := request(f, "PUT", path(one)+"/policy", map[string]any{"reply_threads": []string{}}, true, 200)
	if unchanged["connection"].(map[string]any)["configured_policy"].(map[string]any)["contact_read"] != true {
		t.Fatal("old policy client unexpectedly revoked contact access", unchanged)
	}
	if _, err := call(two, "", ""); !errors.Is(err, errContactDenied) {
		t.Fatal("grant leaked to second connection", err)
	}
	if _, err := call(one, "大鱼", token); !errors.Is(err, errContactCursor) {
		t.Fatal("owner cursor was usable from Agent", err)
	}
	in.Channel = "wechat"
	if _, err := f.handler.chat.tools(store, in).Call(t.Context(), wechatapp.StationContactsToolName, json.RawMessage(`{}`)); !errors.Is(err, errContactDenied) {
		t.Fatal("inbound application context admitted to local tool", err)
	}
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		var step int
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			step++
			switch step {
			case 1:
				return localToolCall(wechatapp.StationContactsToolName, map[string]any{}), nil
			case 2:
				history := localModelHistory(turns)
				if !strings.Contains(history, one) || strings.Contains(history, two) {
					t.Error("local model did not receive exactly the authorized connection")
				}
				return localToolCall(wechatapp.StationContactsToolName, map[string]any{"connection_id": one}), nil
			default:
				history := localModelHistory(turns)
				if !strings.Contains(history, "合成联系人-one") || strings.Contains(history, nativeCursor) {
					t.Error("local model result missing or excessive")
				}
				return `{"action":"reply","messages":["查到了合成联系人。"]}`, nil
			}
		}), nil
	}
	if status, out := localChatSend(f, "synthetic-contact-turn", "查看已授权的通讯录"); status != 200 || out["status"] != "completed" {
		t.Fatal("owner chat tool did not complete", status, out)
	}
	result, err := call(one, "", "")
	if err != nil {
		t.Fatal(err)
	}
	var observed stationContactPage
	if json.Unmarshal([]byte(result), &observed) != nil || observed.Next == "" {
		t.Fatal(result)
	}
	oneSignedOut.Store(true)
	request(f, "GET", onePath, nil, false, 409)
	oneSignedOut.Store(false)
	pending := make(chan error, 1)
	go func() {
		out, err := call(one, "blocked", "")
		if out != "" {
			err = fmt.Errorf("revoked result escaped")
		}
		pending <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("directory read did not start")
	}
	grant(false)
	grant(true)
	close(release)
	if err := <-pending; !errors.Is(err, errContactDenied) {
		t.Fatal("old directory call survived revoke/regrant", err)
	}
	if _, err := call(one, "", observed.Next); !errors.Is(err, errContactCursor) {
		t.Fatal("old Agent cursor survived revoke/regrant", err)
	}
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	f.login()
	status := request(f, "GET", path(one), nil, false, 200)["connection"].(map[string]any)
	if status["configured_policy"].(map[string]any)["contact_read"] != true || status["worker"] != "disabled" {
		t.Fatal("restart lost durable contact grant", status)
	}
	request(f, "GET", onePath+"?search="+url.QueryEscape("大鱼")+"&limit=2&cursor="+token, nil, false, 400)
}
