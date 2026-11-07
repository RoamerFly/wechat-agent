package admin

import (
	"context"
	"encoding/json"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

func (p *permittedTools) recallMessage(ctx context.Context, raw json.RawMessage) (string, error) {
	return wechatapp.Recall(ctx, raw, &wechatActionHost{p: p, tool: "wechat_recall", raw: raw})
}
