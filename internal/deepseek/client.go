// Package deepseek implements bounded, non-streaming text completion calls.
package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const LegacySystemPrompt = "你是微信群中的 AI 助手。用简洁、清楚的中文回答当前问题，尽量控制在500字以内。你只能生成回复文字，不能声称执行了外部操作。"
const DefaultSystemPrompt = "你是微信群中的 AI 伙伴。用简短自然的中文回应当前话题，优先遵循配置的角色与会话权限。只有程序明确提供的能力才可使用；对联网、生成媒体和外部操作，必须以真实返回结果为依据，不能编造已经完成。"

type Config struct {
	BaseURL, KeyFile, Model string
	SystemPrompt            string
	MaxTokens               int
	ThinkingEnabled         *bool
	ReasoningEffort         string
	ToolMaxTokens           int
	Compatible              bool
	OnUsage                 func(Usage)
}
type Client struct {
	endpoint, key, model string
	systemPrompt         string
	maxTokens            int
	toolMaxTokens        int
	compatible           bool
	responseLimitBytes   int64
	thinkingConfigured   bool
	thinkingEnabled      bool
	reasoningEffort      string
	rejectToolCalls      bool
	http                 *http.Client
	onUsage              func(Usage)
}

// CloseIdleConnections releases a short-lived caller's transport without
// cancelling an already running request. Long-lived hosts may keep the client.
func (c *Client) CloseIdleConnections() { c.http.CloseIdleConnections() }

func New(config Config) (*Client, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(config.KeyFile)
	if err != nil {
		return nil, errors.New("DeepSeek key file unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	key := strings.TrimSpace(string(raw))
	if err != nil || len(raw) > 4096 || !validAPIKey(key) {
		return nil, errors.New("invalid DeepSeek key file")
	}
	return newConfiguredClient(config, key)
}

// NewWithAPIKey creates a client from an in-memory credential.  It is useful
// to hosts that keep provider credentials in their own private configuration
// rather than creating a temporary key file for each model client.
func NewWithAPIKey(config Config, key string) (*Client, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if !validAPIKey(key) {
		return nil, errors.New("invalid DeepSeek API key")
	}
	return newConfiguredClient(config, key)
}

func normalizeConfig(config Config) (Config, error) {
	if config.BaseURL == "" {
		config.BaseURL = "https://api.deepseek.com"
	}
	if config.Model == "" {
		config.Model = "deepseek-flash"
	}
	if config.SystemPrompt == "" {
		config.SystemPrompt = DefaultSystemPrompt
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = 768
	}
	if config.ToolMaxTokens == 0 {
		config.ToolMaxTokens = config.MaxTokens
	}
	if config.ToolMaxTokens < 64 || config.ToolMaxTokens > 393216 {
		return Config{}, errors.New("invalid tool output limit")
	}
	if len(config.SystemPrompt) > 16384 || !utf8.ValidString(config.SystemPrompt) || config.MaxTokens < 64 || config.MaxTokens > 393216 {
		return Config{}, errors.New("invalid model role or output limit")
	}
	if config.ReasoningEffort == "" {
		config.ReasoningEffort = "low"
	}
	if config.ReasoningEffort != "low" && config.ReasoningEffort != "high" && config.ReasoningEffort != "max" {
		return Config{}, errors.New("invalid reasoning effort")
	}
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Config{}, errors.New("DeepSeek base URL must be HTTPS")
	}
	if len(config.Model) > 128 || strings.ContainsAny(config.Model, "\r\n\x00") {
		return Config{}, errors.New("invalid model")
	}
	return config, nil
}

func validAPIKey(key string) bool {
	// Keep New's historical key-file acceptance boundary: whitespace around the
	// value is trimmed before this check, while embedded tabs were accepted.
	return len(key) >= 16 && len(key) <= 4096 && utf8.ValidString(key) && !strings.ContainsAny(key, "\r\n\x00 ")
}

func newConfiguredClient(config Config, key string) (*Client, error) {
	u, err := url.Parse(config.BaseURL)
	if err != nil { // normalizeConfig has already checked this; retain a safe guard.
		return nil, errors.New("DeepSeek base URL must be HTTPS")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/chat/completions"
	enabled := config.ThinkingEnabled != nil && *config.ThinkingEnabled
	return &Client{thinkingConfigured: config.ThinkingEnabled != nil, thinkingEnabled: enabled, reasoningEffort: config.ReasoningEffort, endpoint: u.String(), key: key, model: config.Model, systemPrompt: config.SystemPrompt, maxTokens: config.MaxTokens, toolMaxTokens: config.ToolMaxTokens, compatible: config.Compatible, onUsage: config.OnUsage, http: &http.Client{
		Timeout:       90 * time.Second,
		Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second, MaxIdleConns: 8, MaxIdleConnsPerHost: 2},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect denied") },
	}}, nil
}

func (c *Client) Complete(ctx context.Context, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" || len(prompt) > 4096 || !utf8.ValidString(prompt) {
		return "", errors.New("invalid model input")
	}
	return c.Chat(ctx, c.systemPrompt, []map[string]string{{"role": "user", "content": prompt}})
}

// Chat accepts only text user/assistant turns. The caller supplies a separate
// trusted system policy; historical group messages cannot become system roles.
func (c *Client) Chat(ctx context.Context, system string, messages []map[string]string) (string, error) {
	return c.chat(ctx, system, messages, false)
}

// ChatCompression has its own output budget; a short conversational reply
// limit must not truncate the structured memory summary.
func (c *Client) ChatCompression(ctx context.Context, system string, messages []map[string]string) (string, error) {
	if ctx.Value(purposeKey{}) == nil {
		ctx = WithPurpose(ctx, "compression")
	}
	copy := *c
	copy.maxTokens = 4096
	structured := true
	answer, err := copy.chat(ctx, system, messages, structured)
	if d, ok := ErrorDiagnostics(err); ok && d.Code == "model_invalid_parameters" && c.compatible {
		// Some compatible services do not implement response_format.
		structured = false
		answer, err = copy.chat(ctx, system, messages, structured)
	}
	if d, ok := ErrorDiagnostics(err); ok && d.Code == "model_output_token_limit" && ctx.Err() == nil {
		copy.maxTokens = 8192
		answer, retryErr := copy.chat(ctx, system, messages, structured)
		var failure *RequestError
		if errors.As(retryErr, &failure) {
			failure.Attempts += d.Attempts
			failure.MaxAttempts += d.Attempts
			failure.DurationMS += d.DurationMS
		}
		return answer, retryErr
	}
	return answer, err
}

func (c *Client) chat(ctx context.Context, system string, messages []map[string]string, structured bool) (string, error) {
	return c.chatWithGrounding(ctx, system, messages, structured, nil)
}

func (c *Client) chatWithGrounding(ctx context.Context, system string, messages []map[string]string, structured bool, grounding *GroundingOptions) (string, error) {
	messageLimit, totalLimit := 96<<10, 192<<10
	if grounding != nil {
		// Structured evidence is one message, unlike paginated tool history.
		// This is a transport guard, not a claim about any model's token window.
		messageLimit, totalLimit = 1<<20, 2<<20
	}
	sizeFailure := func(size, limit int) error {
		err := requestFailure("model_input_transport_limit", "input_validation", 0, false)
		err.InputBytes, err.InputByteLimit = size, limit
		return err
	}
	if strings.TrimSpace(system) == "" || len(system) > 96<<10 || !utf8.ValidString(system) || len(messages) == 0 || len(messages) > 101 {
		return "", errors.New("invalid conversation")
	}
	all := []map[string]string{{"role": "system", "content": system}}
	total := len(system)
	for _, message := range messages {
		role, text := message["role"], message["content"]
		if (role != "user" && role != "assistant") || text == "" || !utf8.ValidString(text) {
			return "", errors.New("invalid conversation turn")
		}
		if len(text) > messageLimit {
			return "", sizeFailure(len(text), messageLimit)
		}
		total += len(text)
		all = append(all, map[string]string{"role": role, "content": text})
	}
	if total > totalLimit {
		return "", sizeFailure(total, totalLimit)
	}
	input := struct {
		Model           string              `json:"model"`
		Messages        []map[string]string `json:"messages"`
		Stream          bool                `json:"stream"`
		MaxTokens       int                 `json:"max_tokens"`
		Thinking        map[string]string   `json:"thinking,omitempty"`
		ReasoningEffort string              `json:"reasoning_effort,omitempty"`
		ResponseFormat  map[string]string   `json:"response_format,omitempty"`
	}{Model: c.model, Messages: all, MaxTokens: c.maxTokens}
	if structured || strings.Contains(system, "【可自主使用的工具】") {
		input.ResponseFormat = map[string]string{"type": "json_object"}
	}
	if !c.compatible {
		input.Thinking = map[string]string{"type": "disabled"}
		if c.thinkingEnabled {
			input.Thinking["type"] = "enabled"
			input.ReasoningEffort = c.reasoningEffort
		}
		if grounding != nil && !c.thinkingConfigured {
			input.Thinking = map[string]string{"type": "enabled"}
			input.ReasoningEffort = grounding.ReasoningEffort
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return "", errors.New("model input encoding failed")
	}
	return c.request(ctx, body)
}
func (c *Client) requestOnce(ctx context.Context, body []byte) (answer string, failure *RequestError) {
	usage := Usage{Time: time.Now().UTC()}
	metadata := Diagnostics{}
	defer func() {
		if failure != nil {
			failure.FinishReason, failure.ContentBytes, failure.JSONValid = metadata.FinishReason, metadata.ContentBytes, metadata.JSONValid
			failure.FormatAttempt, _ = ctx.Value(groundingFormatAttemptKey{}).(int)
			if failure.FormatAttempt != 0 {
				failure.MaxFormatAttempts = 2
			}
		}
		if c.rejectToolCalls {
			metadata.Stage = "decode"
			metadata.Attempts, _ = ctx.Value(requestAttemptKey{}).(int)
			if metadata.Attempts == 0 {
				metadata.Attempts = 1
			}
			metadata.MaxAttempts = 2
			metadata.FormatAttempt, _ = ctx.Value(groundingFormatAttemptKey{}).(int)
			metadata.MaxFormatAttempts = 2
			metadata.DurationMS = time.Since(usage.Time).Milliseconds()
			if failure != nil {
				metadata.Code, metadata.Stage, metadata.Retryable = failure.Code, failure.Stage, failure.Retryable
			} else if !metadata.JSONValid {
				metadata.Code = "model_invalid_response"
			}
			observeRequest(ctx, "model_grounding_response", metadata)
		}
		usage.Model = c.model
		usage.Purpose, _ = ctx.Value(purposeKey{}).(string)
		if usage.Purpose == "" {
			usage.Purpose = "conversation"
		}
		if observer, ok := ctx.Value(usageObserverKey{}).(func(Usage)); ok {
			observer(usage)
		}
		if c.onUsage != nil {
			c.onUsage(usage)
		}
	}()
	var transientReasoning string
	defer func() {
		if failure == nil && !c.rejectToolCalls {
			rememberThinking(ctx, answer, transientReasoning)
		}
	}()
	var err error
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", requestFailure("model_invalid_request", "request", 0, false)
	}
	request.GetBody = nil // Transport replay is disabled; recovery.go owns the bounded retry.
	request.Header.Set("Authorization", "Bearer "+c.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		if response != nil && response.StatusCode >= 300 && response.StatusCode < 400 {
			return "", requestFailure("model_redirect_denied", "http_status", response.StatusCode, false)
		}
		return "", transportFailure(ctx, err, "connection_or_headers")
	}
	defer response.Body.Close()
	metadata.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		failure := statusFailure(response.StatusCode)
		failure.retryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
		return "", failure
	}
	limit := c.responseLimitBytes
	if limit == 0 {
		limit = 256 << 10
		if c.thinkingEnabled {
			limit = 16 << 20
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		failure := transportFailure(ctx, err, "response_body")
		failure.HTTPStatus = response.StatusCode
		return "", failure
	}
	if int64(len(data)) > limit {
		return "", requestFailure("model_response_too_large", "response_body", 200, false)
	}
	usage = parseUsage(data, usage.Time)
	var result struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Choices) != 1 {
		return "", requestFailure("model_invalid_response", "decode", 200, false)
	}
	choice := result.Choices[0]
	transientReasoning = choice.Message.Reasoning
	metadata.FinishReason = safeFinishReason(choice.FinishReason)
	metadata.ContentBytes = len(choice.Message.Content)
	metadata.JSONValid = json.Valid(bytes.TrimSpace([]byte(choice.Message.Content)))
	if c.rejectToolCalls && (choice.FinishReason == "tool_calls" || len(choice.Message.ToolCalls) != 0) {
		return "", requestFailure("model_invalid_response", "decode", 200, false)
	}
	if choice.FinishReason == "tool_calls" {
		if len(choice.Message.ToolCalls) == 0 || len(choice.Message.ToolCalls) > 8 {
			return "", requestFailure("model_tool_call_count_invalid", "decode", 200, false)
		}
		calls := []json.RawMessage{}
		for index, item := range choice.Message.ToolCalls {
			call := item.Function
			if len(call.Arguments) > 16<<10 {
				return "", requestFailure("model_tool_arguments_too_large", "decode", 200, false)
			}
			args := bytes.TrimSpace([]byte(call.Arguments))
			if !functionNameAllowed(call.Name) || len(call.Name) >= 130 || len(args) == 0 || args[0] != '{' || !json.Valid(args) {
				return "", requestFailure("model_tool_arguments_invalid", "decode", 200, false)
			}
			value := map[string]any{"action": "tool", "tool": call.Name, "arguments": json.RawMessage(args)}
			if index == 0 && strings.TrimSpace(choice.Message.Content) != "" && len(choice.Message.Content) <= 300 {
				value["progress"] = strings.TrimSpace(choice.Message.Content)
			}
			out, _ := json.Marshal(value)
			calls = append(calls, out)
			if c.thinkingEnabled && len(choice.Message.ToolCalls) > 1 {
				rememberThinking(ctx, string(out), transientReasoning)
			}
		}
		if len(calls) == 1 {
			return string(calls[0]), nil
		}
		out, _ := json.Marshal(map[string]any{"action": "tool_batch", "calls": calls})
		if len(out) > 32<<10 {
			return "", requestFailure("model_tool_arguments_too_large", "decode", 200, false)
		}
		return string(out), nil
	}
	if choice.FinishReason == "length" {
		return "", requestFailure("model_output_token_limit", "decode", 200, false)
	}
	if choice.FinishReason == "content_filter" {
		return "", requestFailure("model_content_filtered", "decode", 200, false)
	}
	if choice.FinishReason != "stop" {
		return "", requestFailure("model_response_incomplete", "decode", 200, false)
	}
	answer = strings.TrimSpace(result.Choices[0].Message.Content)
	if answer == "" || !utf8.ValidString(answer) {
		return "", requestFailure("model_response_empty", "decode", 200, answer == "")
	}
	return answer, nil
}

func safeFinishReason(reason string) string {
	switch reason {
	case "stop", "length", "content_filter", "tool_calls":
		return reason
	default:
		return "unknown"
	}
}
