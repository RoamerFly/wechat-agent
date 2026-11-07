//go:build !windows

package station

import (
	"context"
	"net/http"

	"wechat-bot/internal/websession"
)

func (*localHomeService) douyinLiveClientRoutes(*websession.Store, *http.ServeMux, context.Context) {}
