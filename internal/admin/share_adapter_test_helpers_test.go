package admin

import (
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// Test-only compatibility helpers keep the admin integration tests focused on
// host policy while the source-bound route itself lives with the WeChat app.
type shareRouteBinding = wechatapp.BoundChannelsRoute

func newShareRouteBinding(message linuxnative.Message, source linuxnative.ChannelsSource, ref companion.SourceRef, verifiedShare string) (*shareRouteBinding, error) {
	return wechatapp.NewBoundChannelsRoute(message, source, wechatapp.ChannelsEvent{Account: ref.Account, Thread: ref.Thread, Message: ref.MessageID, EventID: ref.EventID, Revision: ref.Revision}, verifiedShare)
}
