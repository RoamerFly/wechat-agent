package admin

import (
	"context"
	"encoding/json"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
)

// localASRStatus reports only the currently installed media plugin's local
// profile. The former speech configuration and public manifest runner are no
// longer an execution path, so they must not make the desktop UI claim that
// local ASR is usable.
func (m *Manager) localASRStatus() map[string]any {
	state := map[string]any{
		"available": false,
		"message":   "本地语音识别尚未通过媒体理解工具登记",
	}
	ctx := context.Background()
	store, err := m.packageStore()
	if err != nil {
		state["message"] = "媒体工具存储不可用"
		return state
	}
	installed, err := store.Installed(ctx, localasr.PluginID)
	if err != nil || installed.Removed || !installed.Enabled {
		state["message"] = "媒体理解工具未安装或未启用"
		return state
	}
	configuration, err := store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		state["message"] = "媒体理解工具配置不可读取"
		return state
	}
	var backend string
	if json.Unmarshal(configuration.Values["asr_backend"], &backend) != nil || backend != "local" {
		state["message"] = "媒体理解工具当前未选择本地语音识别"
		return state
	}
	profile, ok := m.registeredLocalASRProfile(ctx, installed.Package, pluginhost.Grant{
		Account:     m.native.Account(),
		Plugin:      installed.Package.ID,
		Version:     installed.Package.Version,
		Permissions: installed.Permissions,
	})
	if !ok {
		state["message"] = "本地语音识别资源尚未登记或已失效"
		return state
	}
	state["available"] = true
	state["id"] = profile.Metadata().ProfileID
	state["resource_version"] = profile.Metadata().ResourceVersion
	state["message"] = "本地语音识别资源已登记；实际识别仍会核验来源、授权和输入音频"
	return state
}
