package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionUsesOnlyCurrentPromptAndBoundsTransientRetries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantErr    bool
	}{
		{"success", `{"choices":[{"message":{"content":"回答"},"finish_reason":"stop"}]}`, 200, false},
		{"truncated", `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`, 200, true},
		{"authentication", `{"error":{"message":"private diagnostic"}}`, 401, true},
		{"balance", `{"error":{"message":"private diagnostic"}}`, 402, true},
		{"parameters", `{"error":{"message":"private diagnostic"}}`, 422, true},
		{"rate limit", `{}`, 429, true},
		{"null", `{"choices":[{"message":{"content":null},"finish_reason":"stop"}]}`, 200, true},
		{"invalid JSON", `broken`, 200, true},
		{"too large", strings.Repeat("x", (256<<10)+1), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer local-test-key-value" {
					t.Error("unexpected request")
				}
				var body struct {
					Model    string              `json:"model"`
					Messages []map[string]string `json:"messages"`
					Stream   bool                `json:"stream"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "deepseek-flash" || len(body.Messages) != 2 || body.Messages[1]["content"] != "current prompt" || body.Stream {
					t.Error("invalid completion contract")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			key := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(key, []byte("local-test-key-value\n"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := New(Config{BaseURL: server.URL, KeyFile: key})
			if err != nil {
				t.Fatal(err)
			}
			client.http = server.Client()
			answer, err := client.Complete(context.Background(), "current prompt")
			expectedCalls := 1
			if tc.status == 429 || tc.name == "null" {
				expectedCalls = 2
			}
			if (err != nil) != tc.wantErr || calls != expectedCalls {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "private diagnostic") {
				t.Fatal("upstream error leaked")
			}
			if err == nil && answer != "回答" {
				t.Fatalf("answer=%q", answer)
			}
		})
	}
}

func TestEndpointAndKeyValidation(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("local-test-key-value"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://api.deepseek.com", "https://user:secret@api.deepseek.com", "https://api.deepseek.com?secret=x", "https://api.deepseek.com/#fragment"} {
		if _, err := New(Config{BaseURL: endpoint, KeyFile: key}); err == nil {
			t.Fatalf("unsafe endpoint accepted: %s", endpoint)
		}
	}
	if err := os.WriteFile(key, []byte("key\nsecond-line"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{KeyFile: key}); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestNewWithAPIKeyDoesNotReadKeyFile(t *testing.T) {
	client, err := NewWithAPIKey(Config{
		BaseURL:   "https://api.deepseek.com",
		KeyFile:   filepath.Join(t.TempDir(), "does-not-exist"),
		Model:     "deepseek-flash",
		MaxTokens: 256,
	}, "local-test-key-000000000000000000")
	if err != nil || client == nil {
		t.Fatalf("in-memory key construction failed: client=%v err=%v", client, err)
	}
	if _, err := NewWithAPIKey(Config{}, "short"); err == nil || strings.Contains(err.Error(), "short") {
		t.Fatalf("invalid key error leaked or accepted key: %v", err)
	}
}

func TestRedirectDoesNotForwardCredential(t *testing.T) {
	hits := 0
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer destination.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer server.Close()
	key := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(key, []byte("local-test-key-value"), 0600)
	client, err := New(Config{BaseURL: server.URL, KeyFile: key})
	if err != nil {
		t.Fatal(err)
	}
	// Trust only the synthetic source certificate; retain the production redirect policy.
	client.http.Transport = server.Client().Transport
	if _, err = client.Complete(context.Background(), "current prompt"); err == nil || hits != 0 {
		t.Fatal("redirect allowed")
	}
}

func TestCompatibleProviderUsesConfiguredRoleWithoutDeepSeekExtension(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Fatal("invalid request")
		}
		if _, present := body["thinking"]; present {
			t.Error("DeepSeek-specific field leaked to compatible provider")
		}
		var messages []map[string]string
		_ = json.Unmarshal(body["messages"], &messages)
		if len(messages) != 2 || messages[0]["content"] != "custom role" {
			t.Error("custom role missing")
		}
		write := `{"choices":[{"message":{"content":"answer"},"finish_reason":"stop"}]}`
		_, _ = w.Write([]byte(write))
	}))
	defer server.Close()
	key := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(key, []byte("local-test-key-value"), 0600)
	client, err := New(Config{BaseURL: server.URL, KeyFile: key, Model: "custom-model", SystemPrompt: "custom role", Compatible: true, MaxTokens: 256})
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	if _, err := client.Complete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
}
