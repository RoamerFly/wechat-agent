package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"path/filepath"
	"testing"

	wechat "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/pluginhost"
)

func TestOfficialAppSource(t *testing.T) {
	contract, err := Lookup("wechat")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pluginhost.VerifyDescriptor(filepath.Join("wechat"), "app.json")
	if err != nil || p.ID != contract.Package.ID || p.Status != "migration-pending" || p.Runtime.Transport != "unbound" {
		t.Fatalf("invalid source bundle: %#v %v", p, err)
	}
	if _, err := Lookup("../wechat"); err == nil {
		t.Fatal("untrusted catalog path accepted")
	}
	if _, err := pluginhost.VerifyDescriptor("wechat", "../app.json"); err == nil {
		t.Fatal("invalid descriptor accepted")
	}
	// Existing connection keys must survive moving their implementation.
	if wechat.ConnectionID("synthetic-account") != "wechat-50a5ee17cb005388e7f31e1ed28043dda745bb3d2b65d8042afaead6fd4af575" {
		t.Fatal("connection identity changed")
	}
	if wechat.ConnectionRef("synthetic-account").InstallationID != "wechat-legacy" {
		t.Fatal("legacy installation changed")
	}
}

func TestBilibiliSourceSeparatesPublicReadsAndPublication(t *testing.T) {
	contract, err := Lookup("bilibili")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pluginhost.VerifyDescriptor("bilibili", "app.json")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != contract.Package.ID || p.Status != "ready" || p.Runtime.Transport != "http-adapter" ||
		len(p.HTTPRoutes) != 12 || p.HTTPRoutes[0].Endpoint != "https://api.bilibili.com/x/web-interface/view" ||
		!reflect.DeepEqual(p.HTTPRoutes[0].Capabilities, []string{"observation.title", "observation.body"}) ||
		p.HTTPRoutes[1].Endpoint != "https://api.bilibili.com/x/player/playurl" ||
		!reflect.DeepEqual(p.HTTPRoutes[1].Capabilities, []string{"observation.playback", "observation.audio"}) ||
		p.HTTPRoutes[2].Endpoint != "https://api.bilibili.com/x/v2/reply/main" ||
		!reflect.DeepEqual(p.HTTPRoutes[2].Capabilities, []string{"observation.comments"}) {
		t.Fatalf("unexpected Bilibili package capabilities: %v", p.HTTPRoutes)
	}
	wantEndpoints := []string{
		"https://member.bilibili.com/arcopen/fn/archive/video/init",
		"https://openupos.bilivideo.com/video/v2/part/upload",
		"https://member.bilibili.com/arcopen/fn/archive/video/complete",
		"https://member.bilibili.com/arcopen/fn/archive/add-by-utoken",
	}
	for i, endpoint := range wantEndpoints {
		route := p.HTTPRoutes[3+i]
		if route.Endpoint != endpoint || !reflect.DeepEqual(route.Capabilities, []string{"publication.video"}) {
			t.Fatalf("unexpected publication route: %#v", route)
		}
	}
	view, list := p.HTTPRoutes[7], p.HTTPRoutes[8]
	if view.Endpoint != "https://member.bilibili.com/arcopen/fn/archive/view" ||
		!reflect.DeepEqual(view.Capabilities, []string{"publication.video", "publication.reconciliation"}) ||
		list.Endpoint != "https://member.bilibili.com/arcopen/fn/archive/viewlist" ||
		!reflect.DeepEqual(list.Capabilities, []string{"publication.reconciliation"}) {
		t.Fatalf("unexpected publication reconciliation routes: %#v %#v", view, list)
	}
	account := p.HTTPRoutes[9]
	if account.Endpoint != "https://member.bilibili.com/arcopen/fn/user/account/info" || !reflect.DeepEqual(account.Capabilities, []string{"authorization.verify"}) {
		t.Fatalf("account verification route was mixed with observation or posting: %#v", account)
	}
	for i, endpoint := range []string{"https://api.bilibili.com/x/account-oauth2/v1/token", "https://api.bilibili.com/x/account-oauth2/v1/refresh_token"} {
		route := p.HTTPRoutes[10+i]
		if route.Endpoint != endpoint || !reflect.DeepEqual(route.Capabilities, []string{"authorization.oauth"}) {
			t.Fatalf("unexpected OAuth route: %#v", route)
		}
	}
}

type catalogReaderFunc func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error)

func (f catalogReaderFunc) Catalog(c context.Context, k, t, s, p string) (linuxnative.CatalogPage, error) {
	return f(c, k, t, s, p)
}

func TestWeChatApplicationToolPolicyAndProtocol(t *testing.T) {
	for _, tc := range []struct {
		name               string
		policy             wechat.Policy
		count              int
		read, send, recall bool
	}{
		{"member", wechat.Policy{}, 2, false, false, false},
		{"member-read", wechat.Policy{ReadConversations: true}, 3, true, false, false},
		{"owner", wechat.Policy{Owner: true, ReadConversations: true}, 6, true, false, false},
		{"owner-cross", wechat.Policy{Owner: true, ReadConversations: true, CrossConversation: true}, 9, true, true, false},
		{"owner-recall", wechat.Policy{Owner: true, ReadConversations: true, CrossConversation: true, AccountActions: []string{"recall"}}, 11, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := wechat.Tools(tc.policy)
			names := map[string]bool{}
			for _, tool := range tools {
				if names[tool.Name] || !wechat.ToolName(tool.Name) || !json.Valid(tool.InputSchema) {
					t.Fatal("invalid/duplicate tool", tool.Name)
				}
				names[tool.Name] = true
			}
			if len(tools) != tc.count || names["wechat_read"] != tc.read || names["wechat_send_text"] != tc.send || names["wechat_recall"] != tc.recall {
				t.Fatal(names)
			}
		})
	}
	if wechat.ToolName("wechat_unknown") || wechat.ToolName("server_list") || wechat.SubmissionTool("wechat_read") || !wechat.SubmissionTool("wechat_send_text") {
		t.Fatal("invalid tool dispatch boundary")
	}
}

func TestWeChatApplicationReadPreservesCursorAndTypedErrors(t *testing.T) {
	calls := 0
	allowed := true
	read := catalogReaderFunc(func(ctx context.Context, kind, thread, search, cursor string) (linuxnative.CatalogPage, error) {
		calls++
		if kind != "messages" || thread != "room" || search != "" || cursor != "opaque-page" {
			t.Fatal("read arguments changed")
		}
		next := "exact-next-page"
		return linuxnative.CatalogPage{Items: []map[string]any{{"id": "message-1", "threadId": "room"}}, NextCursor: &next, Coverage: map[string]any{"partial": true}}, nil
	})
	auth := func(thread string) bool { return allowed && thread == "room" }
	result, err := wechat.ReadCatalog(t.Context(), read, "room", json.RawMessage(`{"kind":"messages","cursor":"opaque-page"}`), auth)
	if err != nil || calls != 1 || result.Thread != "room" || *result.Page.NextCursor != "exact-next-page" {
		t.Fatal(result, err)
	}
	if _, err := wechat.ReadCatalog(t.Context(), read, "room", json.RawMessage(`{"kind":"messages","thread_id":"other"}`), auth); !errors.Is(err, wechat.ErrReadDenied) || calls != 1 {
		t.Fatal("unauthorized native read", err, calls)
	}
	typed := &linuxnative.CatalogError{Code: "catalog_cursor_invalid"}
	failing := catalogReaderFunc(func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error) {
		return linuxnative.CatalogPage{}, typed
	})
	if _, err := wechat.ReadCatalog(t.Context(), failing, "room", json.RawMessage(`{"kind":"messages"}`), auth); err != typed {
		t.Fatal("typed diagnostic lost", err)
	}
	revoked := catalogReaderFunc(func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error) {
		allowed = false
		return linuxnative.CatalogPage{Items: []map[string]any{{"id": "late"}}}, nil
	})
	if out, err := wechat.ReadCatalog(t.Context(), revoked, "room", json.RawMessage(`{"kind":"messages"}`), auth); !errors.Is(err, wechat.ErrReadDenied) || len(out.Page.Items) > 0 {
		t.Fatal("late revoked page escaped", out, err)
	}
	allowed = true
	mismatched := catalogReaderFunc(func(context.Context, string, string, string, string) (linuxnative.CatalogPage, error) {
		return linuxnative.CatalogPage{Items: []map[string]any{{"threadId": "other"}}}, nil
	})
	if _, err := wechat.ReadCatalog(t.Context(), mismatched, "room", json.RawMessage(`{"kind":"messages"}`), auth); err == nil {
		t.Fatal("wrong conversation page escaped")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := wechat.ReadCatalog(ctx, read, "room", json.RawMessage(`{"kind":"messages"}`), auth); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancelled read ran", err, calls)
	}
}

func TestWeChatApplicationSourceKeepsExactQuoteMediaAndScope(t *testing.T) {
	src := wechat.SourceMetadata{EventID: "event-1", InstanceID: "dataset-1", Account: "account-1", Thread: "room", MessageID: "message-1", ServerID: "101", Sender: "user-1", Revision: 2, Sequence: 12}
	quote := src
	quote.EventID = "event-2"
	quote.MessageID = "message-2"
	quote.ServerID = "102"
	quote.Sender = "user-2"
	record := wechat.SourceRecord{Source: src, StoredSequence: 23, AddresseeIDs: []string{"user-2"}, QuotedSource: &quote, Media: []wechat.SourceMedia{
		{EventID: src.EventID, Revision: 2, AssetID: "image-exact", Kind: "image"},
		{EventID: src.EventID, Revision: 1, AssetID: "image-old", Kind: "image"},
		{EventID: "other", Revision: 2, AssetID: "image-foreign", Kind: "image"},
	}}
	allowed := true
	quoteMissing := false
	read := func(ctx context.Context, thread, message string) (wechat.SourceRecord, error) {
		if thread != "room" {
			t.Fatal("cross-scope source query")
		}
		if message == src.MessageID {
			return record, nil
		}
		if quoteMissing {
			return wechat.SourceRecord{}, apphost.ErrNotFound
		}
		return wechat.SourceRecord{Source: quote}, nil
	}
	auth := func(context.Context, string) (bool, error) { return allowed, nil }
	out, err := wechat.ReadEvent(t.Context(), src.Account, src.Thread, src.MessageID, auth, read)
	if err != nil || out.ConnectionRef != wechat.ConnectionRef(src.Account) || out.Quote == nil || out.Quote.MessageID != quote.MessageID || out.StoredSequence != 23 || out.SourceSequence != 12 || !reflect.DeepEqual(out.Media, []apphost.MediaRef{{AssetID: "image-exact", Kind: "image"}}) {
		t.Fatal(out, err)
	}
	quoteMissing = true
	if out, err := wechat.ReadEvent(t.Context(), src.Account, src.Thread, src.MessageID, auth, read); err != nil || out.Quote != nil {
		t.Fatal("forgotten quote revived", out, err)
	}
	if _, err := wechat.ReadEvent(t.Context(), "account-2", src.Thread, src.MessageID, auth, read); !errors.Is(err, apphost.ErrNotFound) {
		t.Fatal("cross-account source exposed", err)
	}
	late := func(ctx context.Context, thread, message string) (wechat.SourceRecord, error) {
		allowed = false
		return record, nil
	}
	if out, err := wechat.ReadEvent(t.Context(), src.Account, src.Thread, src.MessageID, auth, late); !errors.Is(err, apphost.ErrNotFound) || out.EventID != "" {
		t.Fatal("revoked source returned", out, err)
	}
	allowed = true
	reads := 0
	lateQuote := func(ctx context.Context, thread, message string) (wechat.SourceRecord, error) {
		reads++
		allowed = false
		return record, nil
	}
	if _, err := wechat.ReadEvent(t.Context(), src.Account, src.Thread, src.MessageID, auth, lateQuote); !errors.Is(err, apphost.ErrNotFound) || reads != 1 {
		t.Fatal("quote source was read after revocation", reads, err)
	}
}

func TestDouyinSourceDeclaresOnlyOrdinaryAccountRoutes(t *testing.T) {
	contract, err := Lookup("douyin")
	if err != nil {
		t.Fatal(err)
	}
	p, err := pluginhost.VerifyDescriptor("douyin", "app.json")
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != contract.Package.ID || p.Status != "ready" || len(p.HTTPRoutes) != 0 {
		t.Fatalf("retired API routes still declared: %+v %v", p.HTTPRoutes, err)
	}
	var browser struct {
		Routes []struct {
			Capability  string `json:"capability"`
			URLTemplate string `json:"url_template"`
			OwnerOptIn  string `json:"owner_opt_in"`
		} `json:"browser_routes"`
	}
	raw, err := descriptors.ReadFile("douyin/app.json")
	if err != nil || json.Unmarshal(raw, &browser) != nil || len(browser.Routes) != 8 {
		t.Fatal("ordinary browser routes missing", err)
	}
	for i, capability := range []string{"observation.page-text", "observation.page-text", "observation.web-playback", "observation.web-audio", "observation.web-visual", "observation.web-comments", "interaction.web-like", "interaction.web-reply-comment"} {
		route := browser.Routes[i]
		want := "https://www.douyin.com/video/{video_id}"
		if i == 0 {
			want = "https://www.douyin.com/"
		}
		if route.Capability != capability || route.URLTemplate != want || route.OwnerOptIn == "" {
			t.Fatalf("route %d: %+v", i, route)
		}
	}
}
