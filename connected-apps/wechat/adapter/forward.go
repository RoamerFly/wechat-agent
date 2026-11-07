package adapter

import (
	"context"
	"encoding/json"
	"errors"
)

type ForwardRequest struct {
	Source  string `json:"source_thread_id"`
	Message string `json:"message_id"`
	Target  string `json:"target_thread_id"`
	Key     string `json:"request_key"`
}

type ForwardHost interface {
	CurrentThread() string
	Allowed(string, string) bool
	LegacyResult(context.Context) (string, bool, error)
	QueueFile(context.Context, MediaRequest) (string, error)
}

// Legacy file operations are looked up before creating the canonical media
// batch. This preserves pre-migration operation IDs and never replays them.
func ForwardFile(ctx context.Context, raw json.RawMessage, host ForwardHost) (string, error) {
	var in ForwardRequest
	if json.Unmarshal(raw, &in) != nil || in.Key == "" || in.Message == "" {
		return "", errors.New("转发需要真实消息编号和稳定请求编号")
	}
	if in.Target == "" {
		in.Target = host.CurrentThread()
	}
	if !host.Allowed(in.Source, in.Target) {
		return "", errors.New("cross_forward_denied")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if result, exists, err := host.LegacyResult(ctx); err != nil || exists {
		return result, err
	}
	return host.QueueFile(ctx, MediaRequest{Source: in.Source, Target: in.Target, Message: in.Message, Kind: "file", Key: in.Key})
}
