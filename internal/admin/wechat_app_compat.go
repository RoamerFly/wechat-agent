package admin

import (
	"context"
	"sort"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
)

type wechatCompatibility struct {
	accounts *accountRegistry
}

func (w wechatCompatibility) registration() apphost.Registration {
	return apphost.Registration{
		Application: wechatapp.Application(),
		Provider:    w,
	}
}

func wechatConnectionID(accountID string) string {
	return wechatapp.ConnectionID(accountID)
}

func (w wechatCompatibility) connection(item *accountRuntime) apphost.Connection {
	config := "loaded"
	if item.manager == nil {
		config = "unavailable"
	}
	return apphost.Connection{
		ID: wechatConnectionID(item.record.ID), AccountID: item.record.ID, Label: item.record.Label,
		Primary: item.record.ID == w.accounts.root.manager.native.Account(), Configuration: config,
	}
}

func registeredWechatAccount(item *accountRuntime) bool {
	return wechatapp.LegacyAccountRegistered(item.record)
}

// Unlike the legacy directory refresh, this reads the host's existing account
// set. Listing an app must not discover, import or start another account.
func (w wechatCompatibility) Connections(_ context.Context) ([]apphost.Connection, error) {
	w.accounts.mu.Lock()
	defer w.accounts.mu.Unlock()
	if w.accounts.closed {
		return nil, apphost.ErrUnavailable
	}
	items := make([]apphost.Connection, 0, len(w.accounts.items))
	for _, item := range w.accounts.items {
		if registeredWechatAccount(item) {
			items = append(items, w.connection(item))
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Primary != items[j].Primary {
			return items[i].Primary
		}
		return items[i].AccountID < items[j].AccountID
	})
	return items, nil
}

func (w wechatCompatibility) lookup(connectionID string) (accountRuntime, error) {
	w.accounts.mu.Lock()
	defer w.accounts.mu.Unlock()
	if w.accounts.closed {
		return accountRuntime{}, apphost.ErrUnavailable
	}
	for _, item := range w.accounts.items {
		if registeredWechatAccount(item) && wechatConnectionID(item.record.ID) == connectionID {
			return *item, nil
		}
	}
	return accountRuntime{}, apphost.ErrNotFound
}

func (w wechatCompatibility) Inspect(ctx context.Context, connectionID string) (apphost.ConnectionStatus, error) {
	item, release, err := w.acquire(connectionID)
	if err != nil {
		return apphost.ConnectionStatus{}, err
	}
	defer release()
	result := apphost.ConnectionStatus{
		Connection: w.connection(&item), Login: "unknown", Runtime: "unavailable", Worker: "unavailable",
		CapabilityCheck: "not_checked", SupportedActions: []string{},
	}
	if item.manager == nil {
		return result, nil
	}
	m := item.manager
	result.Worker = m.BotStatus().State
	result.SubmissionsPaused = m.connectionPaused()
	result.Policy = configuredWechatPolicy(m)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	state, err := m.native.Status(ctx)
	if err != nil {
		return result, nil
	}
	result.Login = "signed_out"
	if state.Authenticated {
		result.Login = "authenticated"
	}
	switch state.Connection {
	case "online", "connected":
		result.Runtime = "online"
	case "offline", "disconnected":
		result.Runtime = "offline"
	case "starting", "connecting":
		result.Runtime = "starting"
	default:
		result.Runtime = "unknown"
	}
	if !state.Authenticated || result.Runtime != "online" {
		return result, nil
	}
	result.SupportedActions = []string{"events.read", "groups.list", "messages.read", "send.text"}
	result.CapabilityCheck = "basic"
	native, ok := m.native.(nativeTools)
	if !ok {
		return result, nil
	}
	caps, err := native.Capabilities(ctx)
	if err != nil {
		result.CapabilityCheck = "partial"
		return result, nil
	}
	result.CapabilityCheck = "checked"
	if caps.QuotedTextSend {
		result.SupportedActions = append(result.SupportedActions, "send.quoted_text")
	}
	for _, kind := range []string{"image", "voice", "video", "file"} {
		if contains(caps.MediaKinds, kind) && caps.MediaSendMaxBytes[kind] > 0 {
			result.SupportedActions = append(result.SupportedActions, "send."+kind)
		}
	}
	if _, ok := m.native.(nativeActions); ok {
		for _, kind := range strings.Fields("rename my_name mute clear_announcement recall add_members remove_members leave create") {
			if contains(caps.GroupMutations, kind) {
				result.SupportedActions = append(result.SupportedActions, "account."+kind)
			}
		}
	}
	return result, nil
}

// Match legacy account routing's exclusion against removal and logout.
func (w wechatCompatibility) acquire(connectionID string) (accountRuntime, func(), error) {
	item, err := w.lookup(connectionID)
	if err != nil {
		return accountRuntime{}, nil, err
	}
	if item.access == nil {
		return item, func() {}, nil
	}
	item.access.mu.RLock()
	if item.access.removed {
		item.access.mu.RUnlock()
		return accountRuntime{}, nil, apphost.ErrNotFound
	}
	return item, item.access.mu.RUnlock, nil
}

func configuredWechatPolicy(m *Manager) *apphost.ConfiguredPolicy {
	c := m.Config()
	p := c.Companion.Permissions
	policy := &apphost.ConfiguredPolicy{
		WorkerEnabled: c.Enabled, ConversationEnabled: c.Companion.Enabled, Grants: []string{},
	}
	for _, rule := range []struct {
		id      string
		granted bool
	}{
		{"send.text", p.ReplyText}, {"send.image", p.SendImage}, {"send.voice", p.SendVoice},
		{"send.video", p.SendVideo}, {"send.file", p.SendFile}, {"observe.groups", p.ObserveGroups},
	} {
		if rule.granted {
			policy.Grants = append(policy.Grants, rule.id)
		}
	}
	if m.agent != nil {
		agent := m.agent.config()
		policy.ToolsEnabled = agent.Enabled
		if agent.ReadConversations {
			policy.Grants = append(policy.Grants, "read.conversations")
		}
		if agent.CrossConversation {
			policy.Grants = append(policy.Grants, "cross_conversation")
		}
		for _, kind := range agent.AccountActions {
			policy.Grants = append(policy.Grants, "account."+kind)
		}
	}
	return policy
}
