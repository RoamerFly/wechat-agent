package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"wechat-bot/internal/companion"
)

func TestLocalTurnResponsePreservesHTTPShapeAndRedactsUnexpectedError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response localTurnResponse
		err      error
		status   int
		body     map[string]any
	}{
		{"completed", localTurnResponse{RequestID: "request", Messages: []string{"已保存"}, Status: "completed"}, nil, 200, map[string]any{"request_id": "request", "messages": []any{"已保存"}, "status": "completed", "warning": ""}},
		{"no_reply", localTurnResponse{RequestID: "request", Messages: []string{}, Status: "no_reply"}, nil, 200, map[string]any{"request_id": "request", "messages": []any{}, "status": "no_reply", "reason": ""}},
		{"configuration", localTurnResponse{}, &localTurnError{status: 503, message: "请先保存有效的本机模型配置和 Key；没有调用模型"}, 503, map[string]any{"error": "请先保存有效的本机模型配置和 Key；没有调用模型"}},
		{"model", localTurnResponse{}, &localTurnError{status: 502, code: "local_turn_cancelled", stage: "conversation", message: "本轮生成已取消或超时，原消息保留"}, 502, map[string]any{"error": "本轮生成已取消或超时，原消息保留", "code": "local_turn_cancelled", "stage": "conversation"}},
		{"unexpected", localTurnResponse{}, errors.New("synthetic-private-token E:/private/local-model.json"), 502, map[string]any{"error": "本轮聊天处理未完成，原消息保留，可查看后重试", "code": "local_turn_failed", "stage": "conversation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeLocalTurnResponse(w, tc.response, tc.err)
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || !reflect.DeepEqual(got, tc.body) {
				t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "synthetic-private-token") {
				t.Fatal("unsafe error exposed")
			}
		})
	}
	code, message := (&localTurnError{status: 503, message: "固定宿主错误"}).SafeToolFailure()
	if code != "local_turn_failed" || message != "固定宿主错误" {
		t.Fatal(code, message)
	}
}

func TestLocalTurnSharedPipelineAcceptsOriginalRequestWithoutNewUserEvent(t *testing.T) {
	f := localChatFixture(t)
	const request, text = "shared-pipeline", "请在普通对话中核对这条合成消息"
	if code, _ := localChatSend(f, request, text); code != 503 {
		t.Fatal("expected unfinished request", code)
	}
	store, in, err := f.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	in.TraceID, in.OriginalText = request, new(string)
	*in.OriginalText = text
	prior, err := store.LocalConversationRequest(t.Context(), in, request)
	if err != nil || prior == nil {
		t.Fatal(prior, err)
	}
	in.Text, in.MessageTime = text, prior.OccurredAt
	in.Source = &companion.SourceRef{MessageID: request}
	run, err := store.OpenLocalToolRun(t.Context(), in, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(context.Context, string, []map[string]string) (string, error) {
			calls++
			return `{"action":"reply","messages":["共享生成流程已核对。"]}`, nil
		}), nil
	}
	response, err := f.handler.chat.generateLocalTurn(t.Context(), store, in, run, "")
	if err != nil || response.RequestID != request || response.Status != "completed" || calls != 1 {
		t.Fatal(response, err, calls)
	}
	accepted, err := store.LocalConversationRequest(t.Context(), in, request)
	if err != nil || !accepted.Completed || !reflect.DeepEqual(accepted.Messages, response.Messages) {
		t.Fatal(accepted, err)
	}
	page, err := store.LocalConversationPage(t.Context(), in, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, item := range page.Items {
		if item.Role == "user" {
			users++
			if item.Text != text {
				t.Fatal("user source changed", item)
			}
		}
	}
	if users != 1 {
		t.Fatal("generation added duplicate user events", users)
	}
}
