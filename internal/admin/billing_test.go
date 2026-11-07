package admin

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestBillingAPIAuthPriceConflictAndUnsupportedBalance(t *testing.T) {
	f := setup(t)
	if code, _, _ := f.request("GET", "/api/billing", nil, false, false); code != 401 {
		t.Fatal(code)
	}
	f.login()
	code, b, _ := f.request("GET", "/api/billing", nil, false, false)
	if code != 200 {
		t.Fatal(code)
	}
	var summary struct {
		ActiveID string `json:"active_id"`
	}
	_ = json.Unmarshal(b, &summary)
	input := map[string]any{"active_id": summary.ActiveID, "currency": "CNY", "input_per_million": 1, "cache_hit_per_million": .1, "output_per_million": 2}
	if code, _, _ := f.request("POST", "/api/billing/prices", input, true, false); code != 403 {
		t.Fatal("no CSRF", code)
	}
	if code, _, _ := f.request("POST", "/api/billing/prices", input, true, true); code != 200 {
		t.Fatal(code)
	}
	input["active_id"] = "previous-provider"
	if code, _, _ := f.request("POST", "/api/billing/prices", input, true, true); code != 409 {
		t.Fatal("stale pricing overwrote another provider", code)
	}
	f.manager.config.Provider = "openai-compatible"
	f.manager.config.BaseURL = "https://example.org/v1"
	if code, b, _ := f.request("POST", "/api/billing/balance", map[string]any{}, true, true); code != 200 || !bytes.Contains(b, []byte("unsupported")) {
		t.Fatal(code, string(b))
	}
}
func TestBillingScriptServedWithExecutableMIME(t *testing.T) {
	f := setup(t)
	code, _, response := f.request("GET", "/assets/billing.js", nil, false, false)
	if code != 200 || response.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatal("browser would refuse billing script", code, response.Header.Get("Content-Type"))
	}
}
