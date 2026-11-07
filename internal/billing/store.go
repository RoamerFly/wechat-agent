// Package billing owns the local provider-usage ledger shared by host entry points.
package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/hostfile"
)

// ModelConfig identifies the provider and credential scope for usage totals.
type ModelConfig struct {
	Provider, BaseURL, Model string
	APIKey                   string `json:"-"`
}

var ErrInvalidPrices = errors.New("请输入有效币种及每百万Token单价")
var ErrPriceLimit = errors.New("单价设置达到上限")

type Totals struct {
	Requests int64              `json:"requests"`
	Reported int64              `json:"reported_requests"`
	Input    int64              `json:"input_tokens"`
	Output   int64              `json:"output_tokens"`
	CacheHit int64              `json:"cache_hit_tokens"`
	Unpriced int64              `json:"unpriced_requests"`
	Costs    map[string]float64 `json:"estimated_costs"`
}
type modelUsage struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Total    Totals `json:"total"`
}
type Prices struct {
	Currency string  `json:"currency"`
	Input    float64 `json:"input_per_million"`
	CacheHit float64 `json:"cache_hit_per_million"`
	Output   float64 `json:"output_per_million"`
}
type usageData struct {
	Purposes map[string]Totals     `json:"purposes,omitempty"`
	Version  int                   `json:"version"`
	Started  time.Time             `json:"started_at"`
	Total    Totals                `json:"total"`
	Days     map[string]Totals     `json:"days"`
	Models   map[string]modelUsage `json:"models"`
	Prices   map[string]Prices     `json:"prices"`
}
type Store struct {
	mu   sync.Mutex
	path string
	data usageData
}

// Open loads the existing ledger, creating one only when it does not exist.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "usage.json"), data: usageData{Version: 1, Started: time.Now().UTC(), Days: map[string]Totals{}, Models: map[string]modelUsage{}, Prices: map[string]Prices{}}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		initial, _ := json.Marshal(s.data)
		if err := hostfile.WritePrivate(s.path, initial); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil || len(b) > 2<<20 || json.Unmarshal(b, &s.data) != nil || s.data.Version != 1 || s.data.Days == nil || s.data.Models == nil || s.data.Prices == nil || len(s.data.Days) > 370 || len(s.data.Models) > 128 || len(s.data.Prices) > 128 {
		return nil, errors.New("用量账本不可读，未自动重置")
	}
	return s, nil
}

// ModelID is the historical billing scope, including the credential hash.
func ModelID(c ModelConfig) string {
	sum := sha256.Sum256([]byte(c.BaseURL + "\x00" + c.APIKey + "\x00" + c.Model))
	return hex.EncodeToString(sum[:])[:20]
}
func Official(c ModelConfig) bool {
	u, e := url.Parse(c.BaseURL)
	return e == nil && c.Provider == "deepseek" && u.Scheme == "https" && u.Host == "api.deepseek.com" && (u.Path == "" || u.Path == "/" || u.Path == "/v1" || u.Path == "/v1/")
}
func referencePrices(c ModelConfig, stamp time.Time) (Prices, bool) {
	if !Official(c) {
		return Prices{}, false
	}
	// Published CNY reference prices; existing USD ledger entries are retained.
	p := Prices{Currency: "CNY"}
	switch c.Model {
	case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
		p.Input = 1
		p.CacheHit = .02
		p.Output = 4
	case "deepseek-v4-pro":
		p.Input = 4.5
		p.CacheHit = .15
		p.Output = 13.5
	default:
		return Prices{}, false
	}
	u := stamp.UTC()
	hour := u.Hour()
	if u.Weekday() != time.Saturday && u.Weekday() != time.Sunday && ((hour >= 1 && hour < 4) || (hour >= 6 && hour < 10)) {
		p.Input *= 2
		p.CacheHit *= 2
		p.Output *= 2
	}
	return p, true
}
func (s *Store) pricesLocked(c ModelConfig, stamp time.Time) (Prices, bool, bool) {
	if p, ok := s.data.Prices[ModelID(c)]; ok {
		return p, true, true
	}
	p, ok := referencePrices(c, stamp)
	return p, ok, false
}
func addUsage(t Totals, u deepseek.Usage, p Prices, priced bool) Totals {
	t.Requests++
	if u.Reported {
		t.Reported++
		t.Input += u.Input
		t.Output += u.Output
		t.CacheHit += u.CacheHit
		if priced {
			if t.Costs == nil {
				t.Costs = map[string]float64{}
			}
			t.Costs[p.Currency] += (float64(u.Input-u.CacheHit)*p.Input + float64(u.CacheHit)*p.CacheHit + float64(u.Output)*p.Output) / 1_000_000
		} else {
			t.Unpriced++
		}
	}
	return t
}
func (s *Store) Record(c ModelConfig, u deepseek.Usage) error {
	// An independent reviewer shares credentials and endpoint, but its actual
	// model and price identity must not inherit the conversational model.
	if u.Model != "" {
		c.Model = u.Model
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, _ := json.Marshal(s.data)
	id := ModelID(c)
	entry, exists := s.data.Models[id]
	if !exists && len(s.data.Models) >= 128 {
		return errors.New("用量模型上限")
	}
	uurl, _ := url.Parse(c.BaseURL)
	label := c.Provider
	if uurl != nil && uurl.Hostname() != "" {
		label = uurl.Hostname()
	}
	entry.ID = id
	entry.Provider = label
	entry.Model = c.Model
	p, ok, _ := s.pricesLocked(c, u.Time)
	s.data.Total = addUsage(s.data.Total, u, p, ok)
	if s.data.Purposes == nil {
		s.data.Purposes = map[string]Totals{}
	}
	purpose := u.Purpose
	if purpose == "" {
		purpose = "conversation"
	}
	s.data.Purposes[purpose] = addUsage(s.data.Purposes[purpose], u, p, ok)
	location, _ := time.LoadLocation("Asia/Shanghai")
	day := u.Time.In(location).Format("2006-01-02")
	s.data.Days[day] = addUsage(s.data.Days[day], u, p, ok)
	entry.Total = addUsage(entry.Total, u, p, ok)
	s.data.Models[id] = entry
	for old := range s.data.Days {
		if old < u.Time.AddDate(0, 0, -365).In(location).Format("2006-01-02") {
			delete(s.data.Days, old)
		}
	}
	b, err := json.Marshal(s.data)
	if err == nil {
		err = hostfile.WritePrivate(s.path, b)
	}
	if err != nil {
		_ = json.Unmarshal(prior, &s.data)
	}
	return err
}
func (s *Store) Snapshot(c ModelConfig) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Deep copy maps before releasing the lock and serializing in HTTP handlers.
	b, _ := json.Marshal(s.data)
	var d usageData
	_ = json.Unmarshal(b, &d)
	loc, _ := time.LoadLocation("Asia/Shanghai")
	day := time.Now().In(loc).Format("2006-01-02")
	models := []modelUsage{}
	for _, m := range d.Models {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	p, known, custom := s.pricesLocked(c, time.Now())
	return map[string]any{"purposes": d.Purposes, "started_at": d.Started, "today": d.Days[day], "total": d.Total, "days": d.Days, "models": models, "active_id": ModelID(c), "active_model": c.Model, "prices": p, "priced": known, "custom_prices": custom, "price_reference_date": "2026-10-30", "price_source": "https://api-docs.deepseek.com/zh-cn/quick_start/pricing/", "cost_is_estimate": true, "timezone": "Asia/Shanghai"}
}

func (s *Store) Totals() Totals {
	s.mu.Lock()
	defer s.mu.Unlock()
	totals := s.data.Total
	totals.Costs = maps.Clone(totals.Costs)
	return totals
}

func (s *Store) PurposeTotals(purpose string) Totals {
	s.mu.Lock()
	defer s.mu.Unlock()
	totals := s.data.Purposes[purpose]
	totals.Costs = maps.Clone(totals.Costs)
	return totals
}

// SetPrices updates the override for the selected model. A nil price resets it.
func (s *Store) SetPrices(c ModelConfig, price *Prices) error {
	if price != nil && !validPrices(*price) {
		return ErrInvalidPrices
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := ModelID(c)
	prior, existed := s.data.Prices[id]
	if price == nil {
		delete(s.data.Prices, id)
	} else {
		if len(s.data.Prices) >= 128 {
			return ErrPriceLimit
		}
		s.data.Prices[id] = *price
	}
	out, err := json.Marshal(s.data)
	if err == nil {
		err = hostfile.WritePrivate(s.path, out)
	}
	if err != nil {
		if existed {
			s.data.Prices[id] = prior
		} else {
			delete(s.data.Prices, id)
		}
	}
	return err
}

func validPrices(p Prices) bool {
	if p.Currency != "USD" && p.Currency != "CNY" {
		return false
	}
	for _, v := range []float64{p.Input, p.CacheHit, p.Output} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1_000_000 {
			return false
		}
	}
	return true
}
