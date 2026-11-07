package station

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
)

type stationMuteTarget struct {
	Connection string   `json:"connection_id"`
	Groups     []string `json:"group_ids"`
}

func (s *stationWechat) stationMuteTargets(when time.Time) []stationMuteTarget {
	out := []stationMuteTarget{}
	if when.IsZero() || s.ctx.Err() != nil {
		return out
	}
	s.mu.Lock()
	workers := maps.Clone(s.workers)
	s.mu.Unlock()
	for _, connection := range slices.Sorted(maps.Keys(workers)) {
		w := workers[connection]
		w.mu.Lock()
		groups := []string{}
		for id, grant := range w.policy.GroupMuteGrants {
			if !grant.IsZero() && !when.Before(grant) {
				groups = append(groups, id)
			}
		}
		w.mu.Unlock()
		if len(groups) > 0 {
			slices.Sort(groups)
			out = append(out, stationMuteTarget{Connection: connection, Groups: groups})
		}
	}
	return out
}

func (s *stationWechat) stationAgentMuteGrant(connection, group string, when time.Time) time.Time {
	w, err := s.worker(connection, false)
	if err != nil || w == nil || when.IsZero() {
		return time.Time{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	grant := w.policy.GroupMuteGrants[group]
	if when.Before(grant) {
		return time.Time{}
	}
	return grant
}

func (t *localToolSet) localWechatGroupMute(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		Action     string `json:"action"`
		Connection string `json:"connection_id"`
		Group      string `json:"group_id"`
		Muted      *bool  `json:"muted"`
	}
	invalid := &stationContactError{"wechat_group_mute_input_invalid", "查询授权用 list。设置或核对请提供准确的连接和群编号，设置时需提供目标免打扰状态。"}
	if strictLocalMemoryDecode(raw, &in) != nil {
		return "", invalid
	}
	if t.run == nil || !t.run.Tracked || !identity.ValidPathID(t.traceID) || t.checkWechatOwner(ctx) != nil {
		return "", errStationMuteGrant
	}
	if in.Action == "list" {
		if in.Connection != "" || in.Group != "" || in.Muted != nil {
			return "", invalid
		}
		return localProcessJSON(map[string]any{"targets": t.wechat.stationMuteTargets(t.requestTime)})
	}
	if !artifactID(in.Connection, "wechat-") || !validStationGroup(in.Group) ||
		!(in.Action == "set" && in.Muted != nil || in.Action == "status" && in.Muted == nil) {
		return "", invalid
	}
	grant := t.wechat.stationAgentMuteGrant(in.Connection, in.Group, t.requestTime)
	if grant.IsZero() {
		return "", errStationMuteGrant
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	stop := context.AfterFunc(t.wechat.ctx, cancel)
	defer stop()
	var receipt apphost.ActionReceipt
	var err error
	if in.Action == "status" {
		ledger, _, readErr := t.wechat.stationActionLedger(in.Connection)
		if readErr != nil {
			return "", stationMuteToolFailure(readErr)
		}
		receipt.State = "not_submitted"
		if ledger != nil {
			if previous, ok := ledger.FindConnected(t.wechat.installation, in.Connection, "", in.Group, "mute"); ok {
				receipt = t.wechat.lookupStationAction(ctx, ledger, previous)
			}
		}
	} else {
		receipt, err = t.wechat.submitStationGroupAction(ctx, in.Connection, linuxnative.AdminAction{Kind: "mute", ThreadID: in.Group, Muted: *in.Muted}, "",
			stationGroupActionAuthority{requester: t.access.ownerID, trace: t.traceID, grant: grant, active: func(ctx context.Context) bool {
				return t.checkWechatOwner(ctx) == nil
			}})
		if err != nil {
			return "", stationMuteToolFailure(err)
		}
	}
	if t.checkWechatOwner(ctx) != nil || !t.wechat.stationAgentMuteGrant(in.Connection, in.Group, t.requestTime).Equal(grant) {
		return "", errStationMuteGrant
	}
	return localProcessJSON(map[string]any{"connection_id": in.Connection, "group_id": in.Group, "operation_id": receipt.OperationID, "state": receipt.State, "lookup": receipt.Lookup})
}

func stationMuteToolFailure(err error) error {
	switch {
	case errors.Is(err, errStationMuteGrant):
		return errStationMuteGrant
	case errors.Is(err, errStationGroupActionPending):
		return &stationContactError{"wechat_group_mute_pending", "该群已有结果待核对的操作，请查询原操作状态。"}
	case errors.Is(err, errStationGroupActionUnsupported):
		return &stationContactError{"wechat_group_mute_unsupported", "当前微信后台未声明群免打扰能力。"}
	case errors.Is(err, apphost.ErrLifecycleCutover):
		return &stationContactError{"wechat_cutover_not_ready", "旧微信接续未获启用或凭证已失效，群操作未提交。"}
	case errors.Is(err, errStationGroupActionTarget), errors.Is(err, linuxnative.ErrToolNotFound), errors.Is(err, apphost.ErrNotFound):
		return &stationContactError{"wechat_group_mute_target_invalid", "未能在该连接核对所选群。"}
	case errors.Is(err, apphost.ErrLifecycleConflict), errors.Is(err, errContactLogin):
		return &stationContactError{"wechat_group_mute_conflict", "连接已暂停、未登录或本轮已改变目标状态，请查询原操作。"}
	default:
		return &stationContactError{"wechat_group_mute_unavailable", "暂时无法核对群免打扰操作，请查询原操作记录。"}
	}
}
