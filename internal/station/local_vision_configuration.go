package station

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"wechat-bot/internal/localasr"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/websession"
)

// This is the fish's own media-tool configuration. It deliberately never
// consults a WeChat connection or exposes the host-only vision credential.
type localVisionConfiguration struct {
	Endpoint        string `json:"endpoint"`
	Model           string `json:"model"`
	Backend         string `json:"backend"`
	MaxTokens       int    `json:"max_tokens"`
	Thinking        bool   `json:"thinking"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type localVisionView struct {
	Revision      int                      `json:"revision"`
	Configuration localVisionConfiguration `json:"configuration"`
	KeyConfigured bool                     `json:"key_configured"`
	Available     bool                     `json:"available"`
	Ready         bool                     `json:"ready"`
	Reason        string                   `json:"reason,omitempty"`
}

type localVisionInput struct {
	Revision      int                       `json:"revision"`
	Configuration *localVisionConfiguration `json:"configuration"`
	APIKey        string                    `json:"api_key"`
	ClearKey      bool                      `json:"clear_key"`
	ApproveModel  bool                      `json:"approve_model"`
}

const localVisionConfigurationRoute = "/api/local/project-tools/media-understanding/vision"

func defaultLocalVisionConfiguration() localVisionConfiguration {
	return localVisionConfiguration{Backend: "openai-compatible", MaxTokens: 768, ReasoningEffort: "low"}
}

func (s *localProjectTools) visionConfigurationRoutes(owner *websession.Store, mux *http.ServeMux) {
	for _, method := range []string{"GET", "PUT"} {
		mux.HandleFunc(method+" "+localVisionConfigurationRoute, owner.Protect(s.visionConfiguration))
	}
}

func localVisionText(value string, maximum int) bool {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maximum {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}

func validateLocalVisionConfiguration(c localVisionConfiguration) error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || len(c.Endpoint) > 4096 || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" ||
		!strings.HasSuffix(u.Path, "/chat/completions") || strings.ContainsAny(c.Endpoint, "\\\r\n\x00") {
		return errors.New("视觉接口须为完整 HTTPS chat/completions 地址，不能包含账号、查询参数或片段")
	}
	if strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.Model) != c.Model || !localVisionText(c.Model, 128) {
		return errors.New("请填写有效的视觉模型名称")
	}
	if c.Backend != "openai-compatible" && c.Backend != "deepseek" {
		return errors.New("请选择已支持的视觉接口协议")
	}
	if c.MaxTokens < 1 {
		return errors.New("视觉输出额度须为正整数")
	}
	if c.ReasoningEffort != "low" && c.ReasoningEffort != "high" && c.ReasoningEffort != "max" {
		return errors.New("请选择已支持的视觉思考强度")
	}
	if c.Thinking && c.Backend != "deepseek" {
		return errors.New("当前兼容协议未声明思考参数，请关闭思考或选择支持的接口协议")
	}
	return nil
}

func localVisionConfigurationFrom(config pluginhost.Configuration) localVisionConfiguration {
	c := defaultLocalVisionConfiguration()
	_ = json.Unmarshal(config.Values["vision_endpoint"], &c.Endpoint)
	_ = json.Unmarshal(config.Values["vision_model"], &c.Model)
	_ = json.Unmarshal(config.Values["vision_backend"], &c.Backend)
	_ = json.Unmarshal(config.Values["max_tokens"], &c.MaxTokens)
	_ = json.Unmarshal(config.Values["thinking"], &c.Thinking)
	_ = json.Unmarshal(config.Values["reasoning_effort"], &c.ReasoningEffort)
	return c
}

func (s *localProjectTools) localVisionView(ctx context.Context, host *localProjectToolHost) (localVisionView, error) {
	view := localVisionView{Configuration: defaultLocalVisionConfiguration()}
	installed, err := host.store.Installed(ctx, localasr.PluginID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && installed.Removed {
		view.Reason = "请先安装本地媒体识别工具，再配置本机视觉接口。"
		return view, nil
	}
	if err != nil {
		return view, err
	}
	config, err := host.store.Configuration(ctx, localasr.PluginID)
	if err != nil {
		return view, err
	}
	view.Available = true
	view.Revision, view.Configuration = config.Revision, localVisionConfigurationFrom(config)
	view.KeyConfigured = slices.Contains(config.ConfiguredSecrets, "vision_key")
	if validateLocalVisionConfiguration(view.Configuration) != nil || !view.KeyConfigured {
		view.Reason = "请保存视觉接口、模型和密钥；保存配置不会调用模型。"
		return view, nil
	}
	if !slices.Contains(installed.Permissions, "artifact.read") || !slices.Contains(installed.Permissions, "model.invoke") {
		view.Reason = "媒体工具尚未取得素材读取和模型调用权限。"
		return view, nil
	}
	if !installed.Enabled {
		view.Reason = "视觉配置已保存，请启用本地媒体识别工具后使用。"
		return view, nil
	}
	if !s.runtimeReady() {
		view.Reason = "视觉配置已保存，本机隔离运行环境尚未就绪。"
		return view, nil
	}
	view.Ready = true
	view.Reason = "已配置并获准调用；尚不代表服务连接或实际识别质量已验证。"
	return view, nil
}

func (s *localProjectTools) visionConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input localVisionInput
	if r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		if !websession.Decode(w, r, &input) {
			return
		}
		if input.Revision < 0 || input.Configuration == nil || !input.ApproveModel {
			websession.Fail(w, 400, "请填写完整视觉配置并确认允许该接口处理获准的画面")
			return
		}
		if !localVisionText(input.APIKey, 8192) || input.ClearKey && input.APIKey != "" {
			websession.Fail(w, 400, "密钥格式无效，或同时选择了填写和清除密钥")
			return
		}
		if err := validateLocalVisionConfiguration(*input.Configuration); err != nil {
			websession.Fail(w, 400, err.Error())
			return
		}
	}
	s.update.Lock()
	defer s.update.Unlock()
	identity, err := s.home.identity.Read()
	if err != nil || identity.LocalOwner == nil {
		websession.Fail(w, 409, "请先准备本机主人身份")
		return
	}
	host, err := s.forFish(identity.FishID)
	if err != nil {
		websession.Fail(w, 503, "本机媒体工具配置暂不可读取")
		return
	}
	view, err := s.localVisionView(r.Context(), host)
	if err != nil {
		websession.Fail(w, 503, "本机视觉配置暂不可读取，原设置保留")
		return
	}
	if r.Method == http.MethodGet {
		websession.Respond(w, 200, view)
		return
	}
	if !view.Available {
		websession.Fail(w, 409, view.Reason)
		return
	}
	if view.Revision != input.Revision {
		websession.Fail(w, 409, "视觉或语音识别配置已更新。当前草稿保留，请点击顶部刷新后核对再保存。")
		return
	}
	pending, err := host.store.HasUnfinishedJobs(r.Context(), localasr.PluginID)
	if err != nil || pending || s.isActive(localasr.PluginID) || host.controller == nil || host.controller.Status(localasr.PluginID).ActiveCalls > 0 {
		websession.Fail(w, 409, "媒体工具仍有未完成任务，请等待结束或核对任务后再保存视觉配置。")
		return
	}
	config, err := host.store.Configuration(r.Context(), localasr.PluginID)
	if err != nil {
		websession.Fail(w, 503, "媒体工具原配置暂不可读取，未修改设置")
		return
	}
	values := make(map[string]json.RawMessage, len(config.Values)+6)
	for name, raw := range config.Values {
		values[name] = append(json.RawMessage(nil), raw...)
	}
	c := input.Configuration
	for name, value := range map[string]any{"vision_endpoint": c.Endpoint, "vision_model": c.Model, "vision_backend": c.Backend, "max_tokens": c.MaxTokens, "thinking": c.Thinking, "reasoning_effort": c.ReasoningEffort} {
		values[name], _ = json.Marshal(value)
	}
	update := pluginhost.ConfigUpdate{Revision: input.Revision, Values: values, Secrets: map[string]string{"vision_key": input.APIKey}}
	if input.ClearKey {
		update.ClearSecrets = []string{"vision_key"}
	}
	if _, err = host.controller.ConfigureIdle(r.Context(), localasr.PluginID, update); err != nil {
		websession.Fail(w, 409, "视觉设置未保存，请核对版本、运行中的媒体任务和接口配置；原设置保留。")
		return
	}
	current, err := s.home.identity.Read()
	if err != nil || current.LocalOwner == nil || current.FishID != identity.FishID || current.LocalOwner.ID != identity.LocalOwner.ID {
		websession.Fail(w, 409, "本机身份已变化，请重新打开项目工具核对配置。")
		return
	}
	view, err = s.localVisionView(r.Context(), host)
	if err != nil {
		websession.Fail(w, 503, "设置已保存但状态暂不可读，请刷新后核对")
		return
	}
	websession.Respond(w, 200, view)
}
