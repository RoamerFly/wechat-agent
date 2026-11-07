package billing

import (
	"bytes"
	"math"
	"os"
	"testing"
	"time"
	"wechat-bot/internal/deepseek"
)

func defaultTestModel() ModelConfig {
	return ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash"}
}
func TestUsageLedgerCachesCostOnceAndPreservesOldPriceAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := defaultTestModel()
	c.APIKey = "private-key-not-in-ledger"
	// A pre-upgrade external-currency price must survive later CNY pricing.
	if err = s.SetPrices(c, &Prices{Currency: "USD", Input: .15, CacheHit: .003, Output: .6}); err != nil {
		t.Fatal(err)
	}
	u := deepseek.Usage{Time: time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), Reported: true, Input: 100, Output: 20, CacheHit: 60}
	if err = s.Record(c, u); err != nil {
		t.Fatal(err)
	}
	want := (40*.15 + 60*.003 + 20*.6) / 1_000_000
	if got := s.data.Total.Costs["USD"]; math.Abs(got-want) > 1e-12 {
		t.Fatal("cache tokens charged twice", got, want)
	}
	if err = s.SetPrices(c, &Prices{Currency: "CNY", Input: 2, CacheHit: 1, Output: 3}); err != nil {
		t.Fatal(err)
	}
	if err = s.Record(c, u); err != nil {
		t.Fatal(err)
	}
	if err = s.Record(c, deepseek.Usage{Time: u.Time}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	total := loaded.data.Total
	if total.Input != 200 || total.Output != 40 || total.CacheHit != 120 || total.Requests != 3 || total.Reported != 2 || math.Abs(total.Costs["USD"]-want) > 1e-12 || total.Costs["CNY"] <= 0 {
		t.Fatal(total)
	}
	b, _ := os.ReadFile(loaded.path)
	if bytes.Contains(b, []byte(c.APIKey)) {
		t.Fatal("key leaked to usage ledger")
	}
	// A failed reset must preserve the active override in memory and on disk.
	path := loaded.path
	loaded.path = t.TempDir() // a file cannot replace this existing directory
	if err := loaded.SetPrices(c, nil); err == nil {
		t.Fatal("invalid ledger path unexpectedly accepted the reset")
	}
	loaded.path = path
	if prices := loaded.data.Prices[ModelID(c)]; prices.Currency != "CNY" || prices.Input != 2 {
		t.Fatal("failed reset changed active prices", prices)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, b) {
		t.Fatal("failed reset changed the saved ledger", err)
	}
	c.APIKey = "different-account-key"
	if ModelID(c) == ModelID(defaultTestModel()) {
		t.Fatal("identities not isolated")
	}
}
func TestReferencePricePeakAndWeekendAndUnknownModel(t *testing.T) {
	c := defaultTestModel()
	weekday := time.Date(2026, 10, 26, 2, 0, 0, 0, time.UTC)
	p, ok := referencePrices(c, weekday)
	weekend, _ := referencePrices(c, weekday.AddDate(0, 0, -1))
	if !ok || p.Input != 2*weekend.Input {
		t.Fatal(p, weekend)
	}
	if p.Currency != "CNY" || weekend.Input != 1 || weekend.CacheHit != .02 || weekend.Output != 4 {
		t.Fatal("published yuan reference prices missing", p, weekend)
	}
	c.Model = "unverified-model"
	if _, ok = referencePrices(c, weekday); ok {
		t.Fatal("unknown model assigned a guessed price")
	}
}

func TestReviewerUsageKeepsActualModelAndIndependentPrices(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := defaultTestModel()
	c.APIKey = "synthetic-review-usage-key"
	c.Model = "deepseek-flash"
	s.data.Prices[ModelID(c)] = Prices{Currency: "CNY", Input: 99, CacheHit: 99, Output: 99}
	review := c
	review.Model = "deepseek-v4-pro"
	usage := deepseek.Usage{Model: review.Model, Purpose: "reply_grounding_check", Time: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), Reported: true, Input: 1000, CacheHit: 200, Output: 300}
	if err := s.Record(c, usage); err != nil {
		t.Fatal(err)
	}
	entry := s.data.Models[ModelID(review)]
	prices, known := referencePrices(review, usage.Time)
	want := addUsage(Totals{}, usage, prices, known)
	if entry.Model != review.Model || entry.Total.Input != 1000 || entry.Total.Output != 300 || math.Abs(entry.Total.Costs["CNY"]-want.Costs["CNY"]) > 1e-12 {
		t.Fatalf("review usage used the wrong model or price: %+v", entry)
	}
	if _, exists := s.data.Models[ModelID(c)]; exists || c.Model != "deepseek-flash" {
		t.Fatal("review usage changed the configured conversational identity")
	}
	usage.Model = "unpriced-review-model"
	if err := s.Record(c, usage); err != nil {
		t.Fatal(err)
	}
	unpriced := c
	unpriced.Model = usage.Model
	if s.data.Models[ModelID(unpriced)].Total.Unpriced != 1 {
		t.Fatal("unknown review model inherited conversational prices")
	}
}
