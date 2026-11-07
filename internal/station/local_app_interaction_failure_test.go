package station

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/pluginhost"
)

func TestLocalInteractionFailureClassificationAndRedaction(t *testing.T) {
	// Synthetic provider details must never become user or model facing errors.
	const secret = `synthetic-token-84 path=E:\synthetic-private\session.json`
	cases := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"denied", apphost.ErrInteractionDenied, 403, "应用操作许可、原消息来源或所用资料版本已变化，请按原任务编号核对结果"},
		{"ownership", apphost.ErrInteractionOwnership, 403, "这条应用操作不属于当前本机主人"},
		{"invalid", apphost.ErrInteractionInvalid, 400, "应用目标、评论来源或操作参数无法核对"},
		{"request_conflict", pluginhost.ErrRequestConflict, 409, "同一请求已有操作记录；请从list找到原job_id核对，不能改写旧任务重提"},
		{"not_found", apphost.ErrNotFound, 404, "未找到原应用操作，请先查看list中的实际任务编号"},
		{"state_conflict", apphost.ErrInteractionConflict, 409, "任务状态已变化，请查询原任务继续"},
		{"revision", pluginhost.ErrRevision, 409, "任务状态已变化，请查询原任务继续"},
		{"receipt", apphost.ErrInteractionReceipt, 409, "操作回执与原任务不一致，原任务保留待核对"},
		{"timeout", context.DeadlineExceeded, 504, "应用操作请求超时，原任务保留待核对，请按原编号查询结果"},
		{"canceled", context.Canceled, 408, "本次应用操作请求已中断，原任务保留待核对，请按原编号查询结果"},
		{"unavailable", apphost.ErrInteractionUnavailable, 503, "应用连接暂不可用，原任务保留，可按原编号核对"},
		{"unknown", errors.New(secret), 503, "应用操作暂时不可用，原任务保留，可按原编号核对"},
	}
	for _, tc := range cases {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/wrapped=%t", tc.name, wrapped), func(t *testing.T) {
				err := tc.err
				if wrapped {
					err = fmt.Errorf("%s: %w", secret, err)
				}
				status, message := localInteractionFailure(err)
				if status != tc.status || message != tc.message {
					t.Fatalf("got (%d, %q), want (%d, %q)", status, message, tc.status, tc.message)
				}
				safe := &localInteractionToolError{status: status, message: message}
				code, toolMessage := safe.SafeToolFailure()
				if code != "application_interaction_"+strconv.Itoa(tc.status) || toolMessage != tc.message || safe.Error() != tc.message {
					t.Fatalf("safe tool failure changed: %q %q", code, toolMessage)
				}
				for _, output := range []string{message, code, toolMessage, safe.Error()} {
					if strings.Contains(output, "synthetic-token-84") || strings.Contains(output, `E:\synthetic-private`) {
						t.Fatal("provider details leaked into safe error")
					}
				}
			})
		}
	}
}
