package station

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/hostfile"
)

// LocalModelSettings is the non-secret configuration for the independent
// local conversation.  API credentials are intentionally not part of this
// value: it is safe to return from the local management API.
type LocalModelSettings struct {
	Revision        uint64 `json:"revision"`
	Provider        string `json:"provider"`
	BaseURL         string `json:"base_url"`
	Model           string `json:"model"`
	MaxTokens       int    `json:"max_tokens"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
	ReasoningEffort string `json:"reasoning_effort"`
	Persona         string `json:"persona"`
	KeyConfigured   bool   `json:"key_configured"`
}

// LocalModelInput uses Revision as a compare-and-swap precondition.  An empty
// APIKey keeps the already saved key; ClearKey explicitly removes it.
type LocalModelInput struct {
	LocalModelSettings
	APIKey   string `json:"api_key,omitempty"`
	ClearKey bool   `json:"clear_key"`
}

type localModelRecord struct {
	LocalModelSettings
	APIKey string `json:"api_key,omitempty"`
}

type localModelStore struct {
	path string
	mu   sync.Mutex
}

var errLocalModelRevision = errors.New("本机聊天模型设置已更新，请刷新后再保存")

func newLocalModelStore(dir string) *localModelStore {
	return &localModelStore{path: filepath.Join(dir, "local-model.json")}
}

func defaultLocalModelSettings() LocalModelSettings {
	return LocalModelSettings{
		Provider:        "deepseek",
		BaseURL:         "https://api.deepseek.com",
		Model:           "deepseek-flash",
		MaxTokens:       4096,
		ThinkingEnabled: true,
		ReasoningEffort: "low",
		Persona:         companion.FriendlyPersona,
	}
}

func (s *localModelStore) View() (LocalModelSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.readLocked()
	if err != nil {
		return LocalModelSettings{}, err
	}
	return localModelView(record), nil
}

// Save atomically replaces the private record only after validating the new
// settings and its Revision.  Validation constructs no network request.
func (s *localModelStore) Save(input LocalModelInput) (LocalModelSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Atomic replacement alone does not protect a CAS across two instances.
	release, err := hostfile.Lock(s.path + ".lock")
	if err != nil {
		return LocalModelSettings{}, errors.New("本机聊天模型设置暂不可写")
	}
	defer release()
	if input.ClearKey && strings.TrimSpace(input.APIKey) != "" {
		return LocalModelSettings{}, errors.New("清除密钥时不能同时填写 API Key")
	}
	current, err := s.readLocked()
	if err != nil {
		return LocalModelSettings{}, err
	}
	if input.Revision != current.Revision {
		return LocalModelSettings{}, errLocalModelRevision
	}
	if current.Revision == ^uint64(0) {
		return LocalModelSettings{}, errors.New("本机聊天模型设置版本已达上限，原配置未修改")
	}
	key := current.APIKey
	if input.ClearKey {
		key = ""
	} else if input.APIKey != "" {
		key = input.APIKey
	}
	next := localModelRecord{LocalModelSettings: input.LocalModelSettings, APIKey: key}
	next.Revision = current.Revision + 1
	next.KeyConfigured = key != ""
	if err := validateLocalModelRecord(next); err != nil {
		return LocalModelSettings{}, err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return LocalModelSettings{}, errors.New("无法编码本机聊天模型设置")
	}
	if err := hostfile.WritePrivate(s.path, data); err != nil {
		return LocalModelSettings{}, err
	}
	return localModelView(next), nil
}

// Client creates the configured language model without writing an additional
// credential file.  The optional callback is forwarded to the host's billing
// store and receives only provider-reported usage.
func (s *localModelStore) Client(onUsage ...func(deepseek.Usage)) (companion.LanguageModel, error) {
	model, _, err := s.ClientSnapshot(onUsage...)
	return model, err
}

// ClientSnapshot returns the exact non-secret settings used to construct the
// client.  Callers can safely use the snapshot for request auditing and
// billing even if a concurrent save happens immediately afterwards.
func (s *localModelStore) ClientSnapshot(onUsage ...func(deepseek.Usage)) (companion.LanguageModel, LocalModelSettings, error) {
	s.mu.Lock()
	record, err := s.readLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, LocalModelSettings{}, err
	}
	settings := localModelView(record)
	if record.APIKey == "" {
		return nil, settings, errors.New("本机聊天尚未配置模型密钥")
	}
	var usage func(deepseek.Usage)
	if len(onUsage) > 0 {
		usage = onUsage[0]
	}
	thinking := record.ThinkingEnabled
	model, err := deepseek.NewWithAPIKey(deepseek.Config{
		BaseURL:         record.BaseURL,
		Model:           record.Model,
		SystemPrompt:    record.Persona,
		MaxTokens:       record.MaxTokens,
		ThinkingEnabled: &thinking,
		ReasoningEffort: record.ReasoningEffort,
		Compatible:      record.Provider == "openai-compatible",
		OnUsage:         usage,
	}, record.APIKey)
	if err != nil {
		return nil, settings, err
	}
	return model, settings, nil
}

func (s *localModelStore) readLocked() (localModelRecord, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return localModelRecord{LocalModelSettings: defaultLocalModelSettings()}, nil
	}
	if err != nil || len(data) > 128<<10 {
		return localModelRecord{}, errors.New("无法读取本机聊天模型设置")
	}
	var record localModelRecord
	if json.Unmarshal(data, &record) != nil || record.Revision == 0 || validateLocalModelRecord(record) != nil {
		return localModelRecord{}, errors.New("本机聊天模型设置损坏，原文件未被覆盖")
	}
	return record, nil
}

func localModelView(record localModelRecord) LocalModelSettings {
	view := record.LocalModelSettings
	view.KeyConfigured = record.APIKey != ""
	return view
}

func validateLocalModelRecord(record localModelRecord) error {
	if record.Provider != "deepseek" && record.Provider != "openai-compatible" {
		return errors.New("请选择支持的本机聊天 API 服务商类型")
	}
	if strings.TrimSpace(record.Persona) == "" || len(record.Persona) > 16384 || !utf8.ValidString(record.Persona) {
		return errors.New("本机聊天角色提示词不能为空，且不能超过 16 KiB")
	}
	// Reuse the provider constructor's exact endpoint, model, token, reasoning
	// and credential checks. Construction is local and never makes a request.
	thinking := record.ThinkingEnabled
	key := localValidationAPIKey(record.APIKey)
	_, err := deepseek.NewWithAPIKey(deepseek.Config{
		BaseURL:         record.BaseURL,
		Model:           record.Model,
		SystemPrompt:    record.Persona,
		MaxTokens:       record.MaxTokens,
		ThinkingEnabled: &thinking,
		ReasoningEffort: record.ReasoningEffort,
		Compatible:      record.Provider == "openai-compatible",
	}, key)
	if err != nil {
		return errors.New("请检查本机聊天模型地址、名称、输出和思考设置")
	}
	if record.KeyConfigured != (record.APIKey != "") {
		return errors.New("本机聊天模型设置中的密钥状态不一致")
	}
	return nil
}

func localValidationAPIKey(key string) string {
	if key != "" {
		return key
	}
	return "local-validation-key-000000000000"
}
