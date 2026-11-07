package admin

import (
	"context"
	"encoding/json"
	"strings"
	"wechat-bot/internal/localasr"
	"wechat-bot/internal/mcptools"
)

func diagnosticsTool() mcptools.Tool {
	return agentTool("agent_diagnostics", "Trusted owner only: read structured operational diagnostics of THIS account, without host-service directory access or credentials. Default current conversation; thread_id may select another authorized conversation with cross-conversation access. Filter by message_id for audio/video/tool failures. Returns original time, event, status and provider HTTP/code; paginate with next_offset. No raw prompts, keys, error bodies or log files. No WeChat send. Input {thread_id?:string,message_id?:string,offset?:integer,limit?:integer}.", `{"type":"object","properties":{"thread_id":{"type":"string"},"message_id":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`)
}

func diagnosticCode(value string) string {
	if len(value) > 128 || strings.Trim(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" {
		return ""
	}
	for _, prefix := range []string{"sk-", "ghp_", "gho_", "github_pat_"} {
		if strings.HasPrefix(strings.ToLower(value), prefix) {
			return ""
		}
	}
	return value
}

func (p *permittedTools) callDiagnostics(raw json.RawMessage) (string, error) {
	var in struct {
		Thread  string `json:"thread_id"`
		Message string `json:"message_id"`
		Offset  int
		Limit   int
	}
	if json.Unmarshal(raw, &in) != nil || in.Offset < 0 || in.Limit < 0 {
		return "", toolInputError("invalid diagnostic query")
	}
	if !p.privileged() {
		return "", toolInputError("只有当前账号可信管理员可读取运行诊断")
	}
	if in.Thread == "" {
		in.Thread = p.trace.Thread
	}
	if !p.inspectionThreadAllowed(in.Thread) {
		return "", toolInputError("诊断来源会话未授权")
	}
	logs := p.manager.Logs()
	rows := []map[string]any{}
	for i := len(logs) - 1; i >= 0; i-- {
		item := logs[i]
		if item.ThreadID != in.Thread || in.Message != "" && item.MessageID != in.Message {
			continue
		}
		rows = append(rows, map[string]any{"at": item.Time, "event": diagnosticCode(item.Event), "status": diagnosticCode(item.Status), "message_id": item.MessageID, "tool": diagnosticCode(item.Tool), "provider_code": diagnosticCode(item.ToolCode), "http_status": item.ToolHTTPStatus, "stage": diagnosticCode(item.ToolStage)})
	}
	if in.Offset > len(rows) {
		return "", toolInputError("offset out of range")
	}
	if in.Limit == 0 {
		in.Limit = 60
	}
	end := len(rows)
	if in.Limit < end-in.Offset {
		end = in.Offset + in.Limit
	}
	backend, model, profile := "", "", ""
	if store, err := p.manager.packageStore(); err == nil {
		if config, err := store.Configuration(context.Background(), localasr.PluginID); err == nil {
			_ = json.Unmarshal(config.Values["asr_backend"], &backend)
			_ = json.Unmarshal(config.Values["asr_model"], &model)
			_ = json.Unmarshal(config.Values["local_asr_profile_id"], &profile)
			if backend == "" {
				backend = "remote"
			}
		}
	}
	b, e := json.Marshal(map[string]any{"account": p.trace.Account, "thread_id": in.Thread, "items": rows[in.Offset:end], "next_offset": end, "has_more": end < len(rows), "total": len(rows), "read_only": true, "asr_plugin": localasr.PluginID, "asr_enabled": p.manager.mediaTranscriptionConfigured(context.Background()), "asr_model": model, "asr_backend": backend, "asr_local_model": profile})
	return string(b), e
}
