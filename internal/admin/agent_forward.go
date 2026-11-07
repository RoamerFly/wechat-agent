package admin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	wechatapp "wechat-bot/connected-apps/wechat/adapter"
)

type forwardFileInput = wechatapp.ForwardRequest

func forwardReceiptDescription(op agentOperation, current string) (string, string) {
	var in forwardFileInput
	_ = json.Unmarshal(op.Arguments, &in)
	if in.Target == "" {
		in.Target = current
	}
	var result struct {
		Filename string `json:"filename"`
	}
	_ = json.Unmarshal([]byte(op.Result), &result)
	if result.Filename == "" {
		result.Filename = "转发附件"
	}
	return in.Target, "[文件] " + result.Filename + "（转自 " + in.Source + "）"
}

func (p *permittedTools) forwardFile(ctx context.Context, args json.RawMessage) (string, error) {
	var request forwardFileInput
	if json.Unmarshal(args, &request) != nil {
		return "", errors.New("转发需要真实消息编号和稳定请求编号")
	}
	if request.Target == "" {
		request.Target = p.trace.Thread
	}
	return wechatapp.ForwardFile(ctx, args, &wechatForwardHost{p: p, raw: slices.Clone(args), request: request})
}

type wechatForwardHost struct {
	p       *permittedTools
	raw     json.RawMessage
	request forwardFileInput
}

func (h *wechatForwardHost) CurrentThread() string { return h.p.trace.Thread }
func (h *wechatForwardHost) Allowed(source, target string) bool {
	return source == h.request.Source && target == h.request.Target && h.p.crossAllowed(source, target)
}
func (h *wechatForwardHost) LegacyResult(ctx context.Context) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if !h.Allowed(h.request.Source, h.request.Target) {
		return "", false, errors.New("cross_forward_denied")
	}
	id := h.p.operationID("wechat_forward_file", h.raw)
	h.p.manager.agent.mu.Lock()
	old, ok := h.p.manager.agent.data.Operations[id]
	h.p.manager.agent.mu.Unlock()
	return old.Result, ok, nil
}
func (h *wechatForwardHost) QueueFile(ctx context.Context, in wechatapp.MediaRequest) (string, error) {
	expected := wechatapp.MediaRequest{Source: h.request.Source, Target: h.request.Target, Message: h.request.Message, Kind: "file", Key: h.request.Key}
	if !reflect.DeepEqual(in, expected) || !h.Allowed(in.Source, in.Target) {
		return "", errors.New("cross_forward_denied")
	}
	next, _ := json.Marshal(in)
	return h.p.sendMedia(ctx, next)
}
