package admin

import (
	"strings"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/mcptools"
)

// WeChat discovery belongs to the connected application. No optional project
// service is enumerated while listing an application's communication tools.
func (p *permittedTools) wechatCommunicationTools() []mcptools.Tool {
	if !p.agentToolsAllowed() {
		return nil
	}
	c := p.manager.agent.config()
	return wechatapp.Tools(wechatapp.Policy{
		Owner: p.privileged(), ReadConversations: c.ReadConversations && (!strings.HasSuffix(p.trace.Thread, "@chatroom") || p.manager.Config().Companion.Permissions.ObserveGroups),
		CrossConversation: c.CrossConversation, AccountActions: c.AccountActions,
	})
}

func workspaceAgentToolName(name string) bool {
	return agentToolName(name) && !wechatapp.ToolName(name)
}
