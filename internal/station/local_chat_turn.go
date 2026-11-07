package station

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"wechat-bot/internal/billing"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/deepseek"
	"wechat-bot/internal/websession"
)

type localTurnResponse struct {
	RequestID string   `json:"request_id"`
	Messages  []string `json:"messages"`
	Status    string   `json:"status"`
	Reason    string   `json:"reason,omitempty"`
	Warning   string   `json:"warning,omitempty"`
}

// These messages are selected by the host. Underlying model, store and tool
// errors must not leak credentials or local paths to HTTP or task consumers.
type localTurnError struct {
	status  int
	code    string
	stage   string
	message string
}

func (e *localTurnError) Error() string { return e.message }
func (e *localTurnError) SafeToolFailure() (string, string) {
	code := e.code
	if code == "" {
		code = "local_turn_failed"
	}
	return code, e.message
}

func writeLocalTurnResponse(w http.ResponseWriter, response localTurnResponse, err error) {
	if err != nil {
		var failure *localTurnError
		if !errors.As(err, &failure) {
			failure = &localTurnError{status: 502, code: "local_turn_failed", stage: "conversation", message: "本轮聊天处理未完成，原消息保留，可查看后重试"}
		}
		if failure.code == "" {
			websession.Fail(w, failure.status, failure.message)
		} else {
			websession.Respond(w, failure.status, map[string]string{"error": failure.message, "code": failure.code, "stage": failure.stage})
		}
		return
	}
	// Retain the original response shape, including an empty warning on a
	// completed turn and an empty reason on a no-reply decision.
	out := map[string]any{"request_id": response.RequestID, "messages": response.Messages, "status": response.Status}
	if response.Status == "no_reply" {
		out["reason"] = response.Reason
	} else {
		out["warning"] = response.Warning
	}
	websession.Respond(w, http.StatusOK, out)
}

// generateLocalTurn is the single model, billing, validation and acceptance
// pipeline for an admitted user turn and a host-verified continuation. Callers
// retain turn serialization and must supply the actual original source/run;
// this helper never observes a fabricated user message.
func (h *localChatService) generateLocalTurn(ctx context.Context, store *companion.Store, in companion.Input, run *companion.LocalToolRun, continuationID string) (localTurnResponse, error) {
	response := localTurnResponse{RequestID: in.TraceID}
	fail := func(status int, message string) (localTurnResponse, error) {
		return response, &localTurnError{status: status, message: message}
	}
	if run == nil || store == nil {
		return fail(503, "工具检查点无法核验，未执行本轮工具")
	}
	if continuationID != "" {
		if err := h.checkLocalTaskContinuation(ctx, store, in, continuationID); err != nil {
			return fail(409, "后台任务状态或授权已变化，未继续处理")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	settings, err := h.models.View()
	if err != nil {
		return fail(409, "本机模型配置不可读，原文件保留")
	}
	var model companion.LanguageModel
	if h.modelFactory != nil {
		model, err = h.modelFactory()
	} else {
		model, settings, err = h.models.ClientSnapshot()
	}
	if err != nil {
		return fail(503, "请先保存有效的本机模型配置和 Key；没有调用模型")
	}
	if client, ok := model.(interface{ CloseIdleConnections() }); ok {
		defer client.CloseIdleConnections()
	}
	c := localConversationConfig(settings.Persona)
	engine, err := companion.New(c, store, model, "你是大肥鱼，正在 Agent-home-station 软件中与本机主人交谈。按本轮工具目录和主人的授权使用本地记忆、家目录、已授权的项目工具和已连接的应用。生成本机成品后，把工具实际返回的 artifact_id 告诉主人，界面会显示下载入口。软件界面展示已写入本地聊天记录的气泡，不代表微信发送或主人已经读过。不要声称已使用未提供的应用或执行能力。")
	if err != nil {
		return fail(503, "本地聊天引擎未能准备，原始记录保留")
	}
	tools := h.tools(store, in)
	tools.run = run
	tools.inspectOnly = !run.Tracked || continuationID == "" && len(run.Operations) > 0
	if continuationID != "" {
		// A verified new process result admits the next stage of the same
		// authorized task. Existing checkpoints still deduplicate old effects.
		tools.continuationGuard = func(ctx context.Context) error { return h.checkLocalTaskContinuation(ctx, store, in, continuationID) }
	}
	engine.Tools = tools
	purpose := "local_conversation"
	if continuationID != "" {
		purpose = "local_task_continuation"
	}
	var usageErr error
	ctx = deepseek.WithUsageObserver(deepseek.WithPurpose(ctx, purpose), func(u deepseek.Usage) {
		dir := filepath.Join(h.home.dataDir, "local-billing")
		err := os.MkdirAll(dir, 0700)
		var ledger *billing.Store
		if err == nil {
			ledger, err = billing.Open(dir)
		}
		if err == nil {
			err = ledger.Record(billing.ModelConfig{Provider: settings.Provider, BaseURL: settings.BaseURL, Model: settings.Model}, u)
		}
		usageErr = errors.Join(usageErr, err)
	})
	decision, err := engine.Respond(ctx, in)
	if err != nil {
		message, code := "本轮聊天处理未完成，原消息保留，可查看后重试", "local_turn_failed"
		if diagnostic, ok := deepseek.ErrorDiagnostics(err); ok {
			message, code = deepseek.UserMessage(err)+"；原消息保留，可查看后重试", diagnostic.Code
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			message, code = "本轮生成已取消或超时，原消息保留", "local_turn_cancelled"
		}
		return response, &localTurnError{status: 502, code: code, stage: "conversation", message: message}
	}
	if decision.Action != "reply" || len(decision.Messages) == 0 || len(decision.MediaIDs) != 0 || decision.ImageID != "" {
		response.Messages, response.Status, response.Reason = []string{}, "no_reply", decision.Reason
		return response, nil
	}
	_, current, err := h.conversation()
	if err != nil || current.Account != in.Account || current.Sender != in.Sender {
		return fail(409, "本机身份已变更，本次结果未写入聊天")
	}
	if proof, ok := decision.GenerationEvidence(); ok {
		if err = engine.ValidateGenerationEvidence(ctx, proof); err != nil {
			return fail(409, "对话来源已更正或遗忘，本次结果未写入聊天")
		}
	}
	if err = tools.checkAppObservations(ctx); err != nil {
		return fail(409, "应用读取授权已变化，本次回复未写入聊天")
	}
	if continuationID != "" {
		if err = h.checkLocalTaskContinuation(ctx, store, in, continuationID); err != nil {
			return fail(409, "后台任务状态或授权已变化，本次结果未写入聊天")
		}
		err = store.AcceptLocalContinuation(ctx, in, continuationID, decision.Messages, c, time.Now().UTC())
	} else {
		err = store.AcceptLocalConversation(ctx, in, decision.Messages, c, time.Now().UTC())
	}
	if err != nil {
		return fail(503, "回复未能写入本地记录，请先刷新核对，原消息保留")
	}
	response.Messages, response.Status = decision.Messages, "completed"
	if usageErr != nil {
		response.Warning = "回复已保存，但本次用量记录写入失败"
	}
	return response, nil
}
