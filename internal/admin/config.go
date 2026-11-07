// Package admin serves the local self-hosted control panel. It never returns
// provider credentials or native session material through its API.
package admin

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/bot"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/identity"
)

type GroupRule = wechatapp.LegacyGroupRule
type Config struct {
	PrivateAllFriends bool `json:"private_all_friends"`
	friendIDs         []string
	Behavior          bot.Options      `json:"behavior"`
	Provider          string           `json:"provider"`
	BaseURL           string           `json:"base_url"`
	Model             string           `json:"model"`
	SystemPrompt      string           `json:"system_prompt"`
	Prefix            string           `json:"prefix"`
	MaxAgeSeconds     int              `json:"max_age_seconds"`
	MaxTokens         int              `json:"max_tokens"`
	ThinkingEnabled   bool             `json:"thinking_enabled"`
	ReasoningEffort   string           `json:"reasoning_effort"`
	Enabled           bool             `json:"enabled"`
	ConnectionPaused  bool             `json:"connection_paused,omitempty"`
	PrivateEnabled    bool             `json:"private_enabled"`
	FilehelperEnabled bool             `json:"filehelper_enabled"`
	DirectIDs         []string         `json:"direct_ids"`
	GroupRules        []GroupRule      `json:"group_rules"`
	APIKey            string           `json:"api_key,omitempty"`
	Companion         companion.Config `json:"companion"`
}
type ConfigInput struct {
	Config
	ClearKey      bool `json:"clear_key"`
	KeyConfigured bool `json:"key_configured"`
}
type ConfigView struct {
	Config
	KeyConfigured bool `json:"key_configured"`
}

func defaults() Config {
	return Config{Behavior: bot.DefaultOptions(), Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", SystemPrompt: deepseek.DefaultSystemPrompt, Prefix: "/ai ", MaxAgeSeconds: 120, MaxTokens: 4096, ThinkingEnabled: false, ReasoningEffort: "low", FilehelperEnabled: true, DirectIDs: []string{}, GroupRules: []GroupRule{}, Companion: companion.Defaults()}
}
func (c Config) view() ConfigView {
	present := c.APIKey != ""
	c.APIKey = ""
	return ConfigView{c, present}
}

func validID(id string) bool { return identity.ValidPathID(id) }

func validate(c Config) error {
	if err := c.Behavior.Validate(); err != nil {
		return err
	}
	if err := companion.Validate(companion.Normalize(c.Companion)); err != nil {
		return err
	}
	if c.Provider != "deepseek" && c.Provider != "openai-compatible" {
		return errors.New("请选择支持的 API 服务商类型")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("API 地址需要使用 HTTPS，不能包含密码、查询参数或片段")
	}
	if len(c.BaseURL) > 2048 || strings.TrimSpace(c.Model) == "" || len(c.Model) > 128 || strings.ContainsAny(c.Model, "\r\n\x00") {
		return errors.New("请填写有效的 API 地址与模型名称")
	}
	if strings.TrimSpace(c.SystemPrompt) == "" || !utf8.ValidString(c.SystemPrompt) || len(c.SystemPrompt) > 16384 {
		return errors.New("角色提示词不能为空，且不能超过 16 KiB")
	}
	if strings.TrimSpace(c.Prefix) == "" || len(c.Prefix) > 64 || !utf8.ValidString(c.Prefix) || strings.ContainsAny(c.Prefix, "\r\n\x00") {
		return errors.New("私聊触发前缀需要为 1 至 64 字节的单行文字")
	}
	if c.MaxAgeSeconds < 30 || c.MaxAgeSeconds > 3600 || c.MaxTokens < 64 || c.MaxTokens > 393216 {
		return errors.New("请检查消息有效期或输出 Token 上限")
	}
	if c.ReasoningEffort != "" && c.ReasoningEffort != "low" && c.ReasoningEffort != "high" && c.ReasoningEffort != "max" {
		return errors.New("请选择 low、high 或 max 思考强度")
	}
	if c.APIKey != "" && (len(c.APIKey) < 16 || len(c.APIKey) > 4096 || !utf8.ValidString(c.APIKey) || strings.ContainsAny(c.APIKey, " \r\n\t\x00")) {
		return errors.New("API Key 格式无效，请只填入密钥本身")
	}
	if len(c.DirectIDs) > 100 || len(c.GroupRules) > 100 {
		return errors.New("单个实例最多配置 100 个私聊或群聊规则")
	}
	seen := map[string]bool{}
	for _, id := range c.DirectIDs {
		if !validID(id) || strings.HasSuffix(id, "@chatroom") || id == "filehelper" || seen[id] {
			return errors.New("私聊白名单需要有效且不重复的用户 ID")
		}
		seen[id] = true
	}
	for _, r := range c.GroupRules {
		if !validID(r.ID) || !strings.HasSuffix(r.ID, "@chatroom") || seen[r.ID] || (r.Mode != "mention" && r.Mode != "all" && r.Mode != "social") || len(r.Name) > 256 || !utf8.ValidString(r.Name) || len(r.Prefix) > 64 || !utf8.ValidString(r.Prefix) || strings.ContainsAny(r.Prefix, "\r\n\x00") || len(r.Persona) > 4000 || !utf8.ValidString(r.Persona) {
			return errors.New("请检查群 ID、群回复模式和触发前缀，群规则不能重复")
		}
		seen[r.ID] = true
		if r.Mode == "social" && c.Companion.Enabled && (!c.Companion.Permissions.ObserveGroups || !c.Companion.Permissions.AutonomousGroups) {
			return errors.New("自主接话模式需要开启角色，并授权群聊观察与自主接话")
		}
	}
	allowed := map[string]bool{}
	for _, id := range c.allowed() {
		allowed[id] = true
	}
	proactiveSeen := map[string]bool{}
	for _, id := range c.Companion.ProactiveThreads {
		if !validID(id) || !allowed[id] || proactiveSeen[id] {
			return errors.New("主动续聊只能选择已允许且不重复的会话 ID")
		}
		proactiveSeen[id] = true
	}
	if c.Enabled && (c.APIKey == "" || len(c.allowed()) == 0 && !(c.PrivateEnabled && c.PrivateAllFriends)) {
		return errors.New("启动前请先配置 API Key，并启用至少一个回复会话")
	}
	return nil
}
func (c Config) allowed() []string {
	ids := []string{}
	if c.FilehelperEnabled {
		ids = append(ids, "filehelper")
	}
	if c.PrivateEnabled {
		ids = append(ids, c.DirectIDs...)
		if c.PrivateAllFriends {
			ids = append(ids, c.friendIDs...)
		}
	}
	for _, r := range c.GroupRules {
		ids = append(ids, r.ID)
	}
	return ids
}

func writePrivate(path string, data []byte) error { return hostfile.WritePrivate(path, data) }
func loadConfig(dir string) (Config, error) {
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil || len(data) > 128<<10 {
		return Config{}, errors.New("无法读取管理配置")
	}
	c := defaults()
	if json.Unmarshal(data, &c) != nil {
		return Config{}, errors.New("管理配置损坏，已停止自动启动")
	}
	if c.SystemPrompt == deepseek.LegacySystemPrompt {
		c.SystemPrompt = deepseek.DefaultSystemPrompt
	}
	// New character abilities default on for older files, while an explicitly
	// saved false remains an operator choice.
	var presence struct {
		Companion map[string]json.RawMessage `json:"companion"`
	}
	if json.Unmarshal(data, &presence) == nil {
		var permissions map[string]json.RawMessage
		_ = json.Unmarshal(presence.Companion["permissions"], &permissions)
		if _, exists := permissions["send_image"]; !exists {
			c.Companion.Permissions.SendImage = true
		}
		if _, exists := permissions["send_voice"]; !exists {
			c.Companion.Permissions.SendVoice = true
		}
		if _, exists := permissions["send_video"]; !exists {
			c.Companion.Permissions.SendVideo = true
		}
		if _, exists := permissions["send_file"]; !exists {
			c.Companion.Permissions.SendFile = true
		}
	}
	c.Companion = companion.Normalize(c.Companion)
	if err := validate(c); err != nil {
		return Config{}, errors.New("管理配置无效，已停止自动启动")
	}
	return c, nil
}

// Review inherits the same model and thinking strength as ordinary conversation.
func (c Config) sharedCompanion() companion.Config {
	out := c.Companion
	settings := out.GroundingConfig()
	settings.Model = c.Model
	settings.MaxTokens = c.MaxTokens
	settings.ReasoningEffort = c.ReasoningEffort
	if settings.ReasoningEffort == "" {
		settings.ReasoningEffort = "low"
	}
	out.Grounding = &settings
	return out
}
