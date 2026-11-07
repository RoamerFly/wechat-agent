package admin

import (
	"context"
	"encoding/json"
	"slices"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

func (p *permittedTools) callDelivery(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return wechatapp.Delivery(ctx, name, args, &wechatDeliveryHost{p: p, name: name, raw: slices.Clone(args)})
}
