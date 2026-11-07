package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
)

type memoryModelConfig struct {
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
	Provider string `json:"provider"`
}
type memoryServices struct {
	Settings    companion.MemorySettings  `json:"settings"`
	Embedding   companion.EmbeddingConfig `json:"embedding"`
	Maintenance memoryModelConfig         `json:"maintenance"`
}

func (m *Manager) loadMemoryServices() (memoryServices, error) {
	v := memoryServices{Settings: m.Config().Companion.MemoryConfig()}
	b, e := os.ReadFile(filepath.Join(m.dir, "memory-services.json"))
	if errors.Is(e, os.ErrNotExist) {
		return v, nil
	}
	if e != nil || len(b) > 64<<10 {
		return v, errors.New("记忆服务配置不可读")
	}
	if json.Unmarshal(b, &v) != nil {
		return v, errors.New("记忆服务配置无效")
	}
	return v, nil
}
func memoryServicesView(c memoryServices) map[string]any {
	eKey, mKey := c.Embedding.APIKey != "", c.Maintenance.APIKey != ""
	c.Embedding.APIKey = ""
	c.Maintenance.APIKey = ""
	return map[string]any{"settings": c.Settings, "embedding": c.Embedding, "maintenance": c.Maintenance, "embedding_key_configured": eKey, "maintenance_key_configured": mKey, "semantic_configured": c.Embedding.Configured()}
}
func preserveMemoryKey(oldURL, newURL, oldKey, newKey string, clear bool) (string, error) {
	if clear {
		return "", nil
	}
	if newKey != "" {
		return newKey, nil
	}
	a, ae := url.Parse(oldURL)
	b, be := url.Parse(newURL)
	if oldKey != "" && (ae != nil || be != nil || a.Scheme != b.Scheme || !strings.EqualFold(a.Host, b.Host)) {
		return "", errors.New("更换记忆服务地址需重新填写对应密钥")
	}
	return oldKey, nil
}
func (m *Manager) saveMemoryServices(c memoryServices) error {
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	return writePrivate(filepath.Join(m.dir, "memory-services.json"), b)
}
func (s *Server) memoryServicesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/memory/services", s.protected(func(w http.ResponseWriter, r *http.Request) {
		c, e := s.manager.loadMemoryServices()
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		respond(w, 200, memoryServicesView(c))
	}))
	mux.HandleFunc("POST /api/memory/services", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			memoryServices
			ClearEmbeddingKey   bool `json:"clear_embedding_key"`
			ClearMaintenanceKey bool `json:"clear_maintenance_key"`
		}
		if !decode(w, r, &in) {
			return
		}
		m := s.manager
		m.memoryServiceMu.Lock()
		defer m.memoryServiceMu.Unlock()
		old, e := m.loadMemoryServices()
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		in.Embedding.APIKey, e = preserveMemoryKey(old.Embedding.BaseURL, in.Embedding.BaseURL, old.Embedding.APIKey, in.Embedding.APIKey, in.ClearEmbeddingKey)
		if e == nil {
			in.Maintenance.APIKey, e = preserveMemoryKey(old.Maintenance.BaseURL, in.Maintenance.BaseURL, old.Maintenance.APIKey, in.Maintenance.APIKey, in.ClearMaintenanceKey)
		}
		if e == nil {
			e = in.Settings.Validate()
		}
		if e == nil {
			e = in.Embedding.Validate()
		}
		if e == nil && (in.Maintenance.BaseURL != "" || in.Maintenance.Model != "") {
			test := defaults()
			test.BaseURL = in.Maintenance.BaseURL
			test.Model = in.Maintenance.Model
			test.Provider = in.Maintenance.Provider
			test.APIKey = in.Maintenance.APIKey
			e = validate(test)
		}
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		if e = m.saveMemoryServices(in.memoryServices); e != nil {
			fail(w, 500, e.Error())
			return
		}
		respond(w, 200, memoryServicesView(in.memoryServices))
	}))
	mux.HandleFunc("POST /api/memory/services/automatic", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Enabled == nil {
			fail(w, http.StatusBadRequest, "enabled is required")
			return
		}
		m := s.manager
		m.memoryServiceMu.Lock()
		defer m.memoryServiceMu.Unlock()
		current, e := m.loadMemoryServices()
		if e != nil {
			fail(w, http.StatusInternalServerError, e.Error())
			return
		}
		current.Settings.Enabled = *in.Enabled
		if e = m.saveMemoryServices(current); e != nil {
			fail(w, http.StatusInternalServerError, e.Error())
			return
		}
		respond(w, http.StatusOK, memoryServicesView(current))
	}))
	mux.HandleFunc("POST /api/memory/embedding/test", s.protected(func(w http.ResponseWriter, r *http.Request) {
		c, e := s.manager.loadMemoryServices()
		if e != nil {
			fail(w, 500, e.Error())
			return
		}
		result, e := c.Embedding.Embed(r.Context(), []string{"合成测试：喜欢阅读。"})
		if e != nil {
			fail(w, 502, e.Error())
			return
		}
		_ = s.manager.memory.RecordMaintenanceUsage(r.Context(), "embedding_test", result.Tokens, result.Reported)
		respond(w, 200, map[string]any{"ok": true, "dimensions": len(result.Vectors[0]), "reported": result.Reported, "tokens": result.Tokens})
	}))
}

func (m *Manager) memoryProvider(c memoryServices) (companion.LanguageModel, error) {
	identity := sha256.Sum256([]byte(c.Maintenance.BaseURL + "\x00" + c.Maintenance.APIKey))
	keyPath := filepath.Join(m.dir, fmt.Sprintf("memory-model-key-%x", identity[:12]))
	if e := writePrivate(keyPath, []byte(c.Maintenance.APIKey)); e != nil {
		return nil, e
	}
	shared := m.Config().Config
	return deepseek.New(deepseek.Config{ThinkingEnabled: &shared.ThinkingEnabled, ReasoningEffort: shared.ReasoningEffort, BaseURL: c.Maintenance.BaseURL, KeyFile: keyPath, Model: c.Maintenance.Model, MaxTokens: 4096, ToolMaxTokens: 4096, Compatible: c.Maintenance.Provider == "openai-compatible", OnUsage: func(u deepseek.Usage) {
		if m.billing != nil {
			cfg := m.Config().Config
			cfg.BaseURL = c.Maintenance.BaseURL
			cfg.Model = c.Maintenance.Model
			cfg.APIKey = c.Maintenance.APIKey
			cfg.Provider = c.Maintenance.Provider
			_ = m.billing.Record(modelForBilling(cfg), u)
		}
	}})
}
func (m *Manager) runMemoryMaintenance(ctx context.Context, engine *companion.Engine) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if !m.draining() {
			services, err := m.loadMemoryServices()
			if err == nil && services.Settings.Enabled && m.Config().Companion.Permissions.Remember {
				worker := *engine
				worker.Config = m.Config().Companion
				worker.MemoryAllowed = func() bool { return m.Config().Companion.Permissions.Remember }
				worker.Config.Memory = &services.Settings
				worker.MemoryOptions = nil
				if services.Maintenance.Model != "" {
					worker.MemoryModel, err = m.memoryProvider(services)
				}
				if err == nil {
					for _, scope := range m.memory.Summaries(worker.Config, time.Now()) {
						if ctx.Err() != nil || m.draining() {
							break
						}
						stored, e := m.memory.Read(scope.ID, worker.Config, time.Now())
						if e != nil || stored.Account != m.native.Account() || stored.Channel != "wechat" {
							continue
						}
						_, e = m.memory.IndexArchives(ctx, scope.ID, 5)
						if e == nil {
							e = worker.MaintainScope(ctx, scope.ID, false)
						}
						if e == nil {
							_, e = m.memory.RebuildEmbeddingsBudget(ctx, scope.ID, services.Embedding, 32, services.Settings.DailyTokens)
						}
						m.memory.MemoryError(ctx, scope.ID, e)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
