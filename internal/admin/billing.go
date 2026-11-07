package admin

import (
	"errors"
	"net/http"
	"sync"
	"time"
	"wechat-bot/internal/billing"
	"wechat-bot/internal/deepseek"
)

type balanceCache struct {
	mu   sync.Mutex
	id   string
	at   time.Time
	view balanceView
}

func modelForBilling(c Config) billing.ModelConfig {
	return billing.ModelConfig{Provider: c.Provider, BaseURL: c.BaseURL, Model: c.Model, APIKey: c.APIKey}
}

type balanceView struct {
	State   string            `json:"state"`
	Checked time.Time         `json:"checked_at,omitempty"`
	Reason  string            `json:"reason,omitempty"`
	Data    *deepseek.Balance `json:"data,omitempty"`
}

func (s *Server) billingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/billing", s.protected(func(w http.ResponseWriter, r *http.Request) {
		m := s.manager
		m.mu.Lock()
		c := cloneConfig(m.config)
		m.mu.Unlock()
		respond(w, 200, m.billing.Snapshot(modelForBilling(c)))
	}))
	mux.HandleFunc("POST /api/billing/prices", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ActiveID string `json:"active_id"`
			Reset    bool   `json:"reset"`
			billing.Prices
		}
		if !decode(w, r, &in) {
			return
		}
		m := s.manager
		m.op.Lock()
		defer m.op.Unlock()
		m.mu.Lock()
		c := cloneConfig(m.config)
		m.mu.Unlock()
		model := modelForBilling(c)
		if in.ActiveID != billing.ModelID(model) {
			fail(w, 409, "服务商或模型已切换，请刷新单价再保存")
			return
		}
		price := &in.Prices
		if in.Reset {
			price = nil
		}
		err := m.billing.SetPrices(model, price)
		switch {
		case errors.Is(err, billing.ErrInvalidPrices), errors.Is(err, billing.ErrPriceLimit):
			fail(w, 400, err.Error())
		case err != nil:
			fail(w, 500, "单价保存失败")
		default:
			respond(w, 200, m.billing.Snapshot(model))
		}
	}))
	mux.HandleFunc("POST /api/billing/balance", s.protected(func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !decode(w, r, &in) {
			return
		}
		m := s.manager
		m.mu.Lock()
		c := cloneConfig(m.config)
		m.mu.Unlock()
		model := modelForBilling(c)
		if !billing.Official(model) {
			respond(w, 200, balanceView{State: "unsupported", Reason: "当前兼容服务商未提供统一余额接口"})
			return
		}
		if c.APIKey == "" {
			respond(w, 200, balanceView{State: "unconfigured", Reason: "请先保存服务商Key"})
			return
		}
		b := &m.balance
		b.mu.Lock()
		defer b.mu.Unlock()
		id := billing.ModelID(model)
		if b.id == id && time.Since(b.at) < 30*time.Second {
			respond(w, 200, b.view)
			return
		}
		provider, err := m.makeModel(c)
		view := balanceView{State: "error", Checked: time.Now().UTC(), Reason: "余额查询暂不可用"}
		if err == nil {
			var balance deepseek.Balance
			balance, err = provider.Balance(r.Context())
			if err == nil {
				view.State = "ok"
				view.Reason = ""
				view.Data = &balance
			} else {
				view.Reason = balanceReason(err)
			}
		}
		b.id = id
		b.at = time.Now()
		b.view = view
		respond(w, 200, view)
	}))
}
func balanceReason(err error) string {
	switch err.Error() {
	case "balance_http_401", "balance_http_403":
		return "余额查询鉴权失败，请检查Key"
	case "balance_http_429":
		return "余额查询被限流，请稍后刷新"
	case "balance_unsupported":
		return "当前地址不支持官方余额查询"
	case "balance_network_failed":
		return "余额查询连接失败，请稍后刷新"
	default:
		return "余额服务返回异常，未显示为零余额"
	}
}
