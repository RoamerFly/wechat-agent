package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
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

func TestStationWechatGroupDirectoryAndMuteReceipt(t *testing.T) {
	base := t.TempDir()
	host, workspace := filepath.Join(base, "host"), filepath.Join(base, "home")
	nativeDir := filepath.Join(host, "apps", "wechat", "native")
	if err := os.MkdirAll(nativeDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeDir, "api-token"), []byte("synthetic-group-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	const group = "room-one@chatroom"
	const lostGroup = "room-lost@chatroom"
	const agentGroup = "room-agent@chatroom"
	const racingGroup = "room-race@chatroom"
	const renameGroup = "room-rename@chatroom"
	var submissions, receiptReads atomic.Int32
	var ready, wrongTarget, nativeOffline atomic.Bool
	var unsupported, renameUnsupported, renameVisible atomic.Bool
	var renameSubmissions, renameLookups atomic.Int32
	var renameKey atomic.Value
	var renameReceiptMode atomic.Int32
	var nicknameUnsupported, nicknameReceiptReady atomic.Bool
	var nicknameSubmissions, nicknameCache atomic.Int32
	var nicknameKey atomic.Value
	preflight, resumePreflight := make(chan struct{}, 1), make(chan struct{})
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		if r.Header.Get("Authorization") != "Bearer synthetic-group-fixture" {
			t.Error("native credentials not bound")
			w.WriteHeader(401)
			return
		}
		if nativeOffline.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			out(map[string]any{"pid": 8008, "dataDir": nativeDir, "serverId": "synthetic-group", "ownershipGeneration": "first"})
		case "GET /v1/accounts":
			out(map[string]any{"items": []map[string]any{
				{"id": "one", "authenticated": true, "connection": "online"},
				{"id": "two", "authenticated": true, "connection": "online"},
			}})
		case "GET /v1/accounts/one/capabilities", "GET /v1/accounts/two/capabilities":
			targets := []string{}
			if !unsupported.Load() {
				targets = append(targets, "group")
			}
			mutations := []string{}
			if !renameUnsupported.Load() {
				mutations = append(mutations, "rename")
			}
			if !nicknameUnsupported.Load() {
				mutations = append(mutations, "my_name")
			}
			out(map[string]any{"threadNotificationTargets": targets, "groupMutations": mutations})
		case "GET /v1/accounts/one/groups":
			if q := r.URL.Query(); q.Get("limit") != "200" || q.Get("search") != "测试" {
				t.Error("incorrect native directory query", q)
			}
			out(map[string]any{"items": []map[string]any{{"id": group, "roomId": group, "displayName": "测试群", "announcement": "synthetic-private-announcement"}},
				"coverage": map[string]any{"partial": true, "databases": []string{"synthetic-private-db"}}, "nextCursor": "synthetic-native-group-cursor"})
		case "GET /v1/accounts/one/groups/" + group:
			out(map[string]any{"id": group, "roomId": group, "muted": ready.Load(), "isInGroup": true})
		case "GET /v1/accounts/one/groups/" + lostGroup:
			out(map[string]any{"id": lostGroup, "roomId": lostGroup})
		case "GET /v1/accounts/one/groups/" + agentGroup:
			out(map[string]any{"id": agentGroup, "roomId": agentGroup, "isInGroup": true})
		case "GET /v1/accounts/one/groups/" + racingGroup:
			preflight <- struct{}{}
			select {
			case <-resumePreflight:
				out(map[string]any{"id": racingGroup, "roomId": racingGroup, "isInGroup": true})
			case <-r.Context().Done():
			}
		case "GET /v1/accounts/one/groups/" + renameGroup:
			name := "合成旧群名"
			if renameVisible.Load() {
				name = "合成新群名"
			}
			profile := map[string]any{"id": renameGroup, "roomId": renameGroup, "name": name, "isInGroup": true, "announcement": "synthetic-private-announcement"}
			switch nicknameCache.Load() {
			case 1:
				profile["myDisplayName"] = " 合成群昵称 "
			case 2:
				profile["myDisplayName"] = ""
			}
			out(profile)
		case "GET /v1/accounts/two/groups/" + group, "GET /v1/accounts/two/groups/" + renameGroup:
			w.WriteHeader(404)
		case "PUT /v1/accounts/one/threads/" + group + "/notifications":
			var in struct{ Muted *bool }
			if json.NewDecoder(r.Body).Decode(&in) != nil || in.Muted == nil || !strings.HasPrefix(r.Header.Get("Idempotency-Key"), "station-mute-") {
				t.Error("mute submission is not bound to the requested value and idempotency key")
			}
			submissions.Add(1)
			receipt := syntheticStationMuteReceipt("queued", "not_attempted", group)
			if in.Muted != nil && !*in.Muted {
				receipt["id"] = "native-unmute"
			}
			out(receipt)
		case "PUT /v1/accounts/one/threads/" + lostGroup + "/notifications":
			submissions.Add(1)
			w.WriteHeader(503)
		case "PUT /v1/accounts/one/threads/" + agentGroup + "/notifications":
			submissions.Add(1)
			receipt := syntheticStationMuteReceipt("queued", "not_attempted", agentGroup)
			receipt["id"] = "native-agent"
			out(receipt)
		case "PATCH /v1/accounts/one/groups/" + renameGroup:
			var body struct{ Name string }
			key := r.Header.Get("Idempotency-Key")
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Name != "合成新群名" || !strings.HasPrefix(key, "station-rename-") {
				t.Error("rename did not carry the exact name and host idempotency key")
			}
			renameKey.Store(key)
			renameSubmissions.Add(1)
			// Native recorded the operation, but the submit response was lost.
			w.WriteHeader(503)
		case "PUT /v1/accounts/one/groups/" + renameGroup + "/my-name":
			var body map[string]string
			key := r.Header.Get("Idempotency-Key")
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || !strings.HasPrefix(key, "station-my_name-") {
				t.Error("nickname submission must carry only the display name and its bound key")
			}
			displayName, present := body["displayName"]
			switch nicknameSubmissions.Add(1) {
			case 1:
				if !present || displayName != " 合成群昵称 " {
					t.Error("nickname text was changed", body)
				}
				nicknameKey.Store(key)
				w.WriteHeader(503)
			case 2:
				if !present || displayName != "" {
					t.Error("clear must submit an explicit empty nickname", body)
				}
				out(syntheticStationNicknameReceipt("native-nickname-clear", "succeeded", "accepted", renameGroup))
			default:
				t.Error("unexpected duplicate nickname submit")
			}
		case "GET /v1/accounts/one/operations/native-nickname-set":
			receipt := syntheticStationNicknameReceipt("native-nickname-set", "succeeded", "accepted", renameGroup)
			if !nicknameReceiptReady.Load() {
				receipt["result"].(map[string]string)["action"] = "rename"
			}
			out(receipt)
		case "GET /v1/accounts/one/group-mutations/by-idempotency-key":
			keyParam := r.URL.Query().Get("key")
			if key, _ := nicknameKey.Load().(string); key != "" && key == keyParam {
				out(syntheticStationNicknameReceipt("native-nickname-set", "queued", "not_attempted", renameGroup))
				return
			}
			key, _ := renameKey.Load().(string)
			if key == "" || r.URL.Query().Get("key") != key {
				w.WriteHeader(404)
				return
			}
			renameLookups.Add(1)
			out(syntheticStationRenameReceipt("queued", "not_attempted", renameGroup, "rename"))
		case "GET /v1/accounts/one/operations/native-rename":
			switch renameReceiptMode.Load() {
			case 0:
				out(syntheticStationRenameReceipt("succeeded", "accepted", lostGroup, "rename"))
			case 1:
				out(syntheticStationRenameReceipt("succeeded", "accepted", renameGroup, "my_name"))
			case 2:
				out(syntheticStationRenameReceipt("succeeded", "unknown", renameGroup, "rename"))
			default:
				out(syntheticStationRenameReceipt("succeeded", "accepted", renameGroup, "rename"))
			}
		case "GET /v1/accounts/one/operations/native-agent":
			receipt := syntheticStationMuteReceipt("succeeded", "accepted", agentGroup)
			receipt["id"] = "native-agent"
			out(receipt)
		case "GET /v1/accounts/one/operations/native-mute":
			receiptReads.Add(1)
			target := group
			if wrongTarget.Load() {
				target = lostGroup
			}
			state, outcome := "running", "not_attempted"
			if ready.Load() {
				state, outcome = "succeeded", "accepted"
			}
			out(syntheticStationMuteReceipt(state, outcome, target))
		default:
			t.Error("unexpected native call", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(daemon.Close)
	open := func() *localHomeFixture {
		t.Helper()
		entry, err := wechatapp.StationRegistration(t.Context(), daemon.URL, 8008, host)
		if err != nil {
			t.Fatal(err)
		}
		registry, err := apphost.New(entry)
		if err != nil {
			t.Fatal(err)
		}
		h, err := NewLocalHomeHandlerWithApps(host, workspace, "synthetic-local-password", "", registry)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(h)
		t.Cleanup(func() { _ = h.Close() })
		t.Cleanup(server.Close)
		jar, _ := cookiejar.New(nil)
		return &localHomeFixture{t: t, server: server, client: &http.Client{Jar: jar}, handler: h}
	}
	f := open()
	basePath := func(account string) string { return "/api/apps/wechat/connections/" + wechatapp.ConnectionID(account) }
	one, two := basePath("one"), basePath("two")
	path := one + "/group-actions"
	notify := one + "/groups/" + group + "/notification"
	request := func(method, path string, body any, csrf bool, expected int) map[string]any {
		t.Helper()
		status, out := f.request(method, path, body, csrf)
		if status != expected {
			t.Fatalf("%s %s = %d want %d, %v", method, path, status, expected, out)
		}
		return out
	}
	mute := func(target, key string, enable bool) map[string]any {
		return map[string]any{"group_id": target, "request_key": key, "muted": enable}
	}
	request("GET", one+"/groups", nil, false, 401)
	f.login()
	request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true, 200)
	page := request("GET", one+"/groups?search=%E6%B5%8B%E8%AF%95&limit=500", nil, false, 200)
	raw, _ := json.Marshal(page)
	if !strings.Contains(string(raw), "测试群") || strings.Contains(string(raw), "synthetic-private") || strings.Contains(string(raw), "synthetic-native-group-cursor") ||
		page["effective_limit"] != float64(200) || page["partial"] != true {
		t.Fatal("directory exposed private fields or lost its boundaries", page)
	}
	request("GET", one+"/contacts?search=%E6%B5%8B%E8%AF%95&cursor="+page["next_cursor"].(string), nil, false, 400)
	if state := request("GET", notify, nil, false, 200); state["muted"] != false || state["operation"] != nil {
		t.Fatal("initial notification did not report the cached state", state)
	}
	request("POST", path, mute(group, "first", true), false, 403)
	request("POST", path, mute("not-a-group", "first", true), true, 400)
	request("POST", two+"/group-actions", mute(group, "first", true), true, 404)
	unsupported.Store(true)
	if state := request("GET", notify, nil, false, 200); state["can_change"] != false {
		t.Fatal("unsupported runtime advertised an action", state)
	}
	request("POST", path, mute(group, "first", true), true, 409)
	unsupported.Store(false)
	if submissions.Load() != 0 {
		t.Fatal("unauthorized mute reached native")
	}
	first := request("POST", path, mute(group, "first", true), true, 202)
	operation := first["operation_id"].(string)
	if first["state"] != "submitted_pending" {
		t.Fatal("queued result claimed as confirmed", first)
	}
	request("POST", path, mute(group, "first", true), true, 202)
	request("POST", path, mute(group, "first", false), true, 409)
	request("POST", path, mute(group, "another-key", false), true, 409)
	request("GET", two+"/operations/"+operation, nil, false, 404)
	wrongTarget.Store(true)
	ready.Store(true)
	if r := request("GET", one+"/operations/"+operation, nil, false, 200); r["state"] != "submitted_pending" || r["lookup"] != "identity_mismatch" {
		t.Fatal("foreign target receipt accepted", r)
	}
	wrongTarget.Store(false)
	if r := request("GET", notify, nil, false, 200); r["operation"].(map[string]any)["state"] != "succeeded" || r["muted"] != true {
		t.Fatal("late native receipt was not reconciled", r)
	}
	if r := request("POST", path, mute(group, "unmute", false), true, 202); r["state"] != "submitted_pending" || r["platform_operation_id"] != "native-unmute" {
		t.Fatal("confirmed mute blocked an explicit request to unmute", r)
	}
	unknown := request("POST", path, mute(lostGroup, "lost", true), true, 202)
	if unknown["state"] != "submission_unknown" || unknown["platform_operation_id"] != nil {
		t.Fatal("lost operation was misreported", unknown)
	}
	request("POST", path, mute(lostGroup, "lost", true), true, 202)
	request("POST", path, mute(lostGroup, "lost-new-key", false), true, 409)
	if submissions.Load() != 3 || receiptReads.Load() != 2 {
		t.Fatal("duplicate submission or unexpected lookup", submissions.Load(), receiptReads.Load())
	}
	renameProfile := one + "/groups/" + renameGroup + "/profile"
	var renameOperation string
	t.Run("OwnerRename", func(t *testing.T) {
		rename := func(key, name string) map[string]any {
			return map[string]any{"action": "rename", "group_id": renameGroup, "request_key": key, "name": name}
		}
		renameUnsupported.Store(true)
		raw, _ := json.Marshal(request("GET", renameProfile, nil, false, 200))
		if strings.Contains(string(raw), "synthetic-private-announcement") || !strings.Contains(string(raw), "合成旧群名") {
			t.Fatal("profile missing the cached name or exposing unrelated fields", string(raw))
		}
		if r := request("GET", renameProfile, nil, false, 200); r["can_rename"] != false {
			t.Fatal("unsupported rename capability displayed", r)
		}
		request("POST", path, rename("rename-first", "合成新群名"), true, 409)
		renameUnsupported.Store(false)
		request("POST", path, rename("rename-first", "合成新群名"), false, 403)
		request("POST", two+"/group-actions", rename("rename-first", "合成新群名"), true, 404)
		for _, name := range []string{" ", "新\n名称", strings.Repeat("鱼", 171)} {
			request("POST", path, rename("rename-first", name), true, 400)
		}
		if r := request("GET", renameProfile, nil, false, 200); r["can_rename"] != true || r["operation"] != nil {
			t.Fatal("rename profile before submit", r)
		}
		if renameSubmissions.Load() != 0 {
			t.Fatal("denied rename reached native")
		}
		first := request("POST", path, rename("rename-first", "合成新群名"), true, 202)
		renameOperation = first["operation_id"].(string)
		if first["state"] != "submission_unknown" || first["platform_operation_id"] != nil {
			t.Fatal("missing response claimed as confirmation", first)
		}
		request("POST", path, rename("rename-first", "合成新群名"), true, 202)
		request("POST", path, rename("rename-first", "另一个群名"), true, 409)
		request("POST", path, rename("rename-replacement", "合成新群名"), true, 409)
		request("GET", two+"/operations/"+renameOperation, nil, false, 404)
		if r := request("GET", renameProfile, nil, false, 200); r["can_rename"] != false ||
			r["operation"].(map[string]any)["state"] != "submitted_pending" {
			t.Fatal("key lookup did not recover the queued operation", r)
		}
		for mode := int32(0); mode <= 2; mode++ {
			renameReceiptMode.Store(mode)
			r := request("GET", one+"/operations/"+renameOperation, nil, false, 200)
			if r["state"] != "submitted_pending" || r["lookup"] != "identity_mismatch" {
				t.Fatal("mismatched receipt accepted", r)
			}
		}
		renameReceiptMode.Store(3)
		if r := request("GET", renameProfile, nil, false, 200); r["operation"].(map[string]any)["state"] != "succeeded" ||
			r["name"] != "合成旧群名" {
			t.Fatal("cached name was conflated with the platform receipt", r)
		}
		renameVisible.Store(true)
		if r := request("GET", renameProfile, nil, false, 200); r["name"] != "合成新群名" || r["can_rename"] != true {
			t.Fatal("new cached name was not reported", r)
		}
		if renameSubmissions.Load() != 1 || renameLookups.Load() != 1 {
			t.Fatal("rename was resubmitted or durable lookup binding lost", renameSubmissions.Load(), renameLookups.Load())
		}
	})
	var clearedNicknameOperation string
	t.Run("OwnerGroupNickname", func(t *testing.T) {
		input := func(key, name string) map[string]any {
			return map[string]any{"action": "my_name", "group_id": renameGroup, "request_key": key, "name": name}
		}
		nicknameUnsupported.Store(true)
		if r := request("GET", renameProfile, nil, false, 200); r["can_set_my_name"] != false || r["can_rename"] != true {
			t.Fatal("nickname capability must be independently declared", r)
		}
		request("POST", path, input("nickname-set", " 合成群昵称 "), true, 409)
		nicknameUnsupported.Store(false)
		request("POST", path, input("nickname-set", " 合成群昵称 "), false, 403)
		request("POST", two+"/group-actions", input("nickname-set", " 合成群昵称 "), true, 404)
		for _, extra := range []map[string]any{
			{"name": nil}, {"name": "bad\nname"}, {"name": strings.Repeat("鱼", 171)}, {"member_id": "other"},
		} {
			invalid := input("invalid", "合成群昵称")
			for k, v := range extra {
				invalid[k] = v
			}
			request("POST", path, invalid, true, 400)
		}
		missing := input("missing-name", "")
		delete(missing, "name")
		request("POST", path, missing, true, 400)
		if nicknameSubmissions.Load() != 0 {
			t.Fatal("nickname validation did not stop native execution")
		}
		first := request("POST", path, input("nickname-set", " 合成群昵称 "), true, 202)
		if first["state"] != "submission_unknown" || first["action"] != "my_name" {
			t.Fatal("lost nickname response reported as known", first)
		}
		request("POST", path, input("nickname-set", " 合成群昵称 "), true, 202)
		request("POST", path, input("nickname-set", ""), true, 409)
		request("POST", path, input("nickname-new-key", ""), true, 409)
		request("GET", two+"/operations/"+first["operation_id"].(string), nil, false, 404)
		r := request("GET", renameProfile, nil, false, 200)
		if r["my_name_operation"].(map[string]any)["state"] != "submitted_pending" ||
			r["can_set_my_name"] != false || r["can_rename"] != true ||
			r["operation"].(map[string]any)["operation_id"] != renameOperation {
			t.Fatal("original nickname receipt was not recovered independently", r)
		}
		if _, present := r["my_display_name"]; present {
			t.Fatal("missing cache was displayed as an empty nickname", r)
		}
		r = request("GET", one+"/operations/"+first["operation_id"].(string), nil, false, 200)
		if r["state"] != "submitted_pending" || r["lookup"] != "identity_mismatch" {
			t.Fatal("group-rename receipt satisfied nickname lookup", r)
		}
		nicknameReceiptReady.Store(true)
		nicknameCache.Store(1)
		r = request("GET", renameProfile, nil, false, 200)
		if r["my_display_name"] != " 合成群昵称 " || r["my_name_operation"].(map[string]any)["state"] != "succeeded" {
			t.Fatal("verified nickname and cached readback missing", r)
		}
		clear := request("POST", path, input("nickname-clear", ""), true, 202)
		clearedNicknameOperation = clear["operation_id"].(string)
		if clear["state"] != "succeeded" {
			t.Fatal("explicit clear failed", clear)
		}
		nicknameCache.Store(2)
		r = request("GET", renameProfile, nil, false, 200)
		if r["my_display_name"] != "" || r["name"] != "合成新群名" || nicknameSubmissions.Load() != 2 {
			t.Fatal("cached cleared nickname, rename or submit count incorrect", r)
		}
	})
	t.Run("AgentGrant", func(t *testing.T) {
		connection := wechatapp.ConnectionID("one")
		other := wechatapp.ConnectionID("two")
		grant := func(groups ...string) {
			result := request("PUT", one+"/policy", map[string]any{"reply_threads": []string{}, "group_mute_threads": append([]string{}, groups...)}, true, 200)
			policy := result["connection"].(map[string]any)["configured_policy"].(map[string]any)
			rows, _ := policy["group_mute_threads"].([]any)
			if len(rows) != len(groups) {
				t.Fatal(policy)
			}
		}
		request("PUT", one+"/policy", map[string]any{"reply_threads": []string{}, "group_mute_threads": []string{"private-chat"}}, true, 400)
		newRun := func(trace string) *localToolSet {
			t.Helper()
			store, in, err := f.handler.chat.conversation()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			in.TraceID, in.Text, in.MessageTime = trace, "synthetic mute request", now.Format(time.RFC3339Nano)
			in.OriginalText = &in.Text
			if err := store.Observe(in, localConversationConfig(""), now); err != nil {
				t.Fatal(err)
			}
			run, err := store.OpenLocalToolRun(t.Context(), in, true)
			if err != nil {
				t.Fatal(err)
			}
			tools := f.handler.chat.tools(store, in)
			tools.run = run
			return tools
		}
		set := func(connection, group string) map[string]any {
			return map[string]any{"action": "set", "connection_id": connection, "group_id": group, "muted": true}
		}
		call := func(tools *localToolSet, args map[string]any) error {
			raw, _ := json.Marshal(args)
			_, err := tools.Call(t.Context(), wechatapp.StationGroupMuteToolName, raw)
			return err
		}
		before := newRun("agent-before-grant")
		if err := call(before, set(connection, agentGroup)); !errors.Is(err, errStationMuteGrant) {
			t.Fatal("mute allowed by default", err)
		}
		grant(agentGroup, racingGroup)
		fresh := newRun("agent-fresh-direct")
		for _, args := range []map[string]any{set(other, agentGroup), set(connection, group)} {
			if err := call(fresh, args); !errors.Is(err, errStationMuteGrant) {
				t.Fatal("scope escaped", err)
			}
		}
		if err := call(before, set(connection, agentGroup)); !errors.Is(err, errStationMuteGrant) {
			t.Fatal("grant authorized an earlier request", err)
		}
		f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
			step := 0
			return localModelFunc(func(_ context.Context, _ string, history []map[string]string) (string, error) {
				step++
				last := localModelHistory(history)
				switch step {
				case 1:
					return localToolCall(wechatapp.StationGroupMuteToolName, map[string]any{"action": "list"}), nil
				case 2:
					if !strings.Contains(last, agentGroup) || strings.Contains(last, other) {
						t.Error("model did not receive exact authorized scopes")
					}
					return localToolCall(wechatapp.StationGroupMuteToolName, set(connection, agentGroup)), nil
				case 3, 4:
					if !strings.Contains(last, "submitted_pending") {
						t.Error("expected queued receipt")
					}
					if step == 3 {
						return localToolCall(wechatapp.StationGroupMuteToolName, set(connection, agentGroup)), nil
					}
					return localToolCall(wechatapp.StationGroupMuteToolName, map[string]any{"action": "status", "connection_id": connection, "group_id": agentGroup}), nil
				default:
					if !strings.Contains(last, "succeeded") || strings.Contains(last, "native-agent") {
						t.Error("model did not receive the projected accepted result")
					}
					return `{"action":"reply","messages":["合成测试操作已确认。"]}`, nil
				}
			}), nil
		}
		if code, body := localChatSend(f, "agent-mute-chat", "请给获准的合成测试群开启免打扰"); code != 200 || body["status"] != "completed" || submissions.Load() != 4 {
			t.Fatal("normal owner chat did not complete exactly one submission", code, body, submissions.Load())
		}
		result := make(chan error, 1)
		go func() { result <- call(fresh, set(connection, racingGroup)) }()
		select {
		case <-preflight:
		case <-time.After(5 * time.Second):
			t.Fatal("expected native preflight")
		}
		grant()
		grant(agentGroup, racingGroup)
		close(resumePreflight)
		if err := <-result; !errors.Is(err, errStationMuteGrant) || submissions.Load() != 4 {
			t.Fatal("revoked and regranted scope allowed the old request", err, submissions.Load())
		}
	})
	// A native import receipt means this account may be an unactivated legacy
	// handover. A malformed receipt is enough to fence every platform write;
	// it must not leave a local action reservation that prevents the same owner
	// request from being retried after the receipt is removed.
	importReceipt := filepath.Join(host, "apps", "wechat", "native-import-"+wechatapp.ConnectionID("one")+".json")
	if err := os.WriteFile(importReceipt, []byte(`{"id":"wrong-account"}`), 0600); err != nil {
		t.Fatal(err)
	}
	blockedRename := map[string]any{"action": "rename", "group_id": renameGroup, "request_key": "cutover-blocked", "name": "合成新群名"}
	beforeCutoverWrite := renameSubmissions.Load()
	blocked := request("POST", path, blockedRename, true, 409)
	if message, _ := blocked["error"].(string); !strings.Contains(message, "接续未获启用") {
		t.Fatal("cutover denial was not explicit", blocked)
	}
	if renameSubmissions.Load() != beforeCutoverWrite {
		t.Fatal("unactivated import receipt reached the native group mutation", renameSubmissions.Load(), beforeCutoverWrite)
	}
	if err := os.Remove(importReceipt); err != nil {
		t.Fatal(err)
	}
	retried := request("POST", path, blockedRename, true, 202)
	if retried["state"] != "submission_unknown" || renameSubmissions.Load() != beforeCutoverWrite+1 {
		t.Fatal("same owner request could not retry after the cutover receipt was removed", retried, renameSubmissions.Load())
	}
	request("POST", one+"/lifecycle", map[string]string{"action": "stop"}, true, 200)
	request("POST", path, mute(group, "paused-new-key", false), true, 409)
	f.server.Close()
	if err := f.handler.Close(); err != nil {
		t.Fatal(err)
	}
	f = open()
	f.login()
	nativeOffline.Store(true)
	if r := request("GET", one+"/operations/"+operation, nil, false, 200); r["state"] != "succeeded" || r["lookup"] != "stored" {
		t.Fatal("confirmed receipt lost after restart", r)
	}
	if r := request("GET", one+"/operations/"+renameOperation, nil, false, 200); r["action"] != "rename" || r["state"] != "succeeded" || r["lookup"] != "stored" {
		t.Fatal("confirmed rename lost after restart", r)
	}
	if r := request("GET", one+"/operations/"+clearedNicknameOperation, nil, false, 200); r["action"] != "my_name" || r["state"] != "succeeded" || r["lookup"] != "stored" {
		t.Fatal("nickname confirmation lost after restart", r)
	}
	if r := request("GET", one+"/groups/"+lostGroup+"/notification", nil, false, 200); r["operation"].(map[string]any)["state"] != "submission_unknown" || r["can_change"] != false {
		t.Fatal("offline recovery lost unknown reservation", r)
	}
}

func syntheticStationMuteReceipt(state, outcome, target string) map[string]any {
	return map[string]any{"id": "native-mute", "accountId": "one", "kind": "group_mutation", "state": state,
		"result": map[string]string{"action": "mute", "threadId": target, "targetId": target, "remoteOutcome": outcome}}
}

func syntheticStationRenameReceipt(state, outcome, target, action string) map[string]any {
	return map[string]any{"id": "native-rename", "accountId": "one", "kind": "group_mutation", "state": state,
		"result": map[string]string{"action": action, "groupId": target, "targetId": target, "remoteOutcome": outcome}}
}

func syntheticStationNicknameReceipt(id, state, outcome, target string) map[string]any {
	receipt := syntheticStationRenameReceipt(state, outcome, target, "my_name")
	receipt["id"] = id
	return receipt
}
