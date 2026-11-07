package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/linuxnative"
)

type nativeCatalog = wechatapp.CatalogReader

func (s *Server) catalogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/catalog", s.protected(func(w http.ResponseWriter, r *http.Request) {
		native, ok := s.manager.native.(nativeCatalog)
		if !ok {
			fail(w, 501, "当前后台不支持会话目录")
			return
		}
		kind, thread := r.URL.Query().Get("kind"), r.URL.Query().Get("thread")
		if kind != "contacts" && kind != "groups" && kind != "threads" && kind != "messages" && kind != "members" {
			fail(w, 400, "目录类型无效")
			return
		}
		if kind == "messages" || kind == "members" {
			if !s.consoleConversationAllowed(thread) {
				fail(w, 403, "请先从当前账号目录选择会话，或检查管理员全局权限")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		page, err := native.Catalog(ctx, kind, thread, r.URL.Query().Get("search"), r.URL.Query().Get("cursor"))
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				fail(w, 504, "目录查询超时，可以重新查询；群成员未同步时请刷新当前群成员")
				return
			}
			var catalogErr *linuxnative.CatalogError
			if errors.As(err, &catalogErr) {
				fail(w, 502, catalogErr.Error())
				return
			}
			fail(w, 502, "目录读取失败；请检查登录与数据同步状态")
			return
		}
		if names := nativeConversationNames(kind, page); len(names) > 0 {
			if err := s.manager.rememberNativeConversations(names); err != nil {
				fail(w, 502, "目录已读取，但账号会话索引保存失败")
				return
			}
		}
		respond(w, 200, page)
	}))
}
