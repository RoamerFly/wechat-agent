package station

import (
	"context"
	"net/http"

	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/websession"
)

func (h *localHomeService) douyinRoutes(owner *websession.Store, mux *http.ServeMux, registry apphost.Registry, serviceContext context.Context) {
	if _, ok := registry.Lookup(douyinapp.ID); ok {
		h.douyinWebLoginRoutes(owner, mux, serviceContext)
		h.douyinLiveClientRoutes(owner, mux, serviceContext)
	}
}
