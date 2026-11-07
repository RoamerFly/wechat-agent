//go:build !windows

package station

import (
	"net/http"
	"wechat-bot/internal/websession"
)

func (*localChatService) obsDeploymentRoutes(*websession.Store, *http.ServeMux) {}
