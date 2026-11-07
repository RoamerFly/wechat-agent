package station

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

type legacyGroupSettingView struct {
	Status                   string                                `json:"status"`
	Digest                   string                                `json:"digest,omitempty"`
	Note                     string                                `json:"note,omitempty"`
	CanEnableChannelsSummary bool                                  `json:"can_enable_channels_summary,omitempty"`
	ReplySettings            *apphost.ConnectionGroupReplySettings `json:"reply_settings,omitempty"`
	Reason                   string                                `json:"reason,omitempty"`
}

func (s *stationWechat) legacyGroupSettingRoutes(server *websession.Store, mux *http.ServeMux, path string, replyRule bool) {
	handle := server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		thread := r.PathValue("thread")
		if !validStationGroup(thread) {
			websession.Fail(w, http.StatusBadRequest, "请选择明确的群聊")
			return
		}
		var in struct {
			Digest                string `json:"digest"`
			SameIdentity          bool   `json:"same_identity"`
			EnableChannelsSummary bool   `json:"enable_channels_summary"`
		}
		if r.Method == http.MethodPost {
			if !websession.Decode(w, r, &in) {
				return
			}
			if !legacyMemoryDigestValid(in.Digest) || !in.SameIdentity {
				websession.Fail(w, http.StatusBadRequest, "请预演并确认新旧连接使用同一个实际微信账号")
				return
			}
			if replyRule && in.EnableChannelsSummary {
				websession.Fail(w, http.StatusBadRequest, "请通过旧群备注接续开启视频号自动总结")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		view, err := s.legacyGroupSetting(ctx, r.PathValue("connection"), thread, in.Digest, replyRule, in.EnableChannelsSummary)
		switch {
		case err != nil:
			legacyImportFailure(w, err)
		case in.Digest != "" && view.Status != "applied":
			websession.Fail(w, http.StatusConflict, view.Reason)
		default:
			websession.Respond(w, http.StatusOK, view)
		}
	})
	mux.HandleFunc("GET "+path, handle)
	mux.HandleFunc("POST "+path, handle)
}

func (s *stationWechat) legacyGroupSetting(ctx context.Context, id, thread, digest string, replyRule, enableChannelsSummary bool) (legacyGroupSettingView, error) {
	unavailable := func(status, reason string) (legacyGroupSettingView, error) {
		return legacyGroupSettingView{Status: status, Reason: reason}, nil
	}
	native, err := s.WorkerSender(ctx, id)
	if err != nil {
		return legacyGroupSettingView{}, err
	}
	worker, err := s.worker(id, false)
	if err != nil {
		return legacyGroupSettingView{}, err
	}
	if worker == nil {
		return unavailable("not_ready", "请先保存旧版接入摘要并授权该群的回复范围")
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	reply := worker.policy.Grants[thread]
	if reply.IsZero() {
		return unavailable("not_ready", "请先授权该群的回复范围")
	}
	imports := worker.policy.LegacyGroupNoteImports
	history := time.Time{}
	if replyRule {
		imports = worker.policy.LegacyGroupReplyImports
	} else if history = worker.policy.HistoryGrants[thread]; history.IsZero() {
		return unavailable("not_ready", "请先授权该群的回复范围和历史读取")
	}
	if prior := imports[thread]; prior != "" {
		if digest != "" && prior != legacyGroupSettingReceipt(digest, enableChannelsSummary) {
			return unavailable("conflict", "此设置已有接续记录")
		}
		return legacyGroupSettingView{Status: "applied"}, nil
	}
	if worker.run != nil || (worker.policy.Enabled && !worker.policy.Paused) {
		return unavailable("not_ready", "请先暂停连接并等候工作器退出")
	}
	current := worker.policy.GroupRules[thread]
	attachment := worker.policy.AttachmentGrants[thread]
	canEnableChannelsSummary := !attachment.IsZero() && current.SummarizeSince.IsZero()
	if replyRule && current.hasReplySettings() {
		return unavailable("conflict", "新站已有该群回复规则，保留现有内容")
	}
	if !replyRule && current.Note != "" {
		return unavailable("conflict", "新站已有该群备注，保留现有内容")
	}
	if enableChannelsSummary && !canEnableChannelsSummary {
		return unavailable("conflict", "只有已授权附件读取且尚未开启自动总结的群才可同时开启")
	}
	target, err := workspacefs.OpenPath(worker.dir)
	if err != nil {
		return legacyGroupSettingView{}, apphost.ErrUnavailable
	}
	defer target.Close()
	account := native.Account()
	archive, err := readLegacyImportArchive(target)
	if err != nil || archive.Plan.Account != account {
		return unavailable("not_ready", "请先保存此连接的旧版接入摘要")
	}
	snapshot, source, err := s.openLegacyImport()
	if err != nil {
		return legacyGroupSettingView{}, err
	}
	defer snapshot.Close()
	if !source.matchesArchive(archive) {
		return legacyGroupSettingView{}, errLegacySnapshotIntegrity
	}
	if source.guard != nil {
		if err := source.guard.verify(ctx, snapshot); err != nil {
			return legacyGroupSettingView{}, err
		}
	}
	config, _, err := legacyImportAccountPaths(snapshot, source, account)
	if err != nil {
		return legacyGroupSettingView{}, err
	}
	cutover, evidence := s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
	if !legacyMemoryCutoverReady(cutover) {
		return unavailable("not_ready", "请先核对旧宿主停机记录、原生档案导入回执与目标微信登录")
	}
	daemon, err := native.DaemonStatus(ctx)
	if err != nil {
		return unavailable("not_ready", "目标微信原生服务暂时不可核对")
	}
	var view legacyGroupSettingView
	var verifySource func() bool
	var bindingFields []any
	if replyRule {
		setting, version := readLegacyGroupReplySettings(snapshot, config, thread, source.guard)
		if setting.Status != "ready" {
			return setting, nil
		}
		view = setting
		bindingFields = []any{"legacy-group-reply-v1", version}
		verifySource = func() bool {
			freshConfig, _, err := legacyImportAccountPaths(snapshot, source, account)
			if err != nil || freshConfig != config {
				return false
			}
			_, freshVersion := readLegacyGroupReplySettings(snapshot, config, thread, source.guard)
			return version == freshVersion
		}
	} else {
		dir, err := workspacefs.OpenExistingDirectory(snapshot, filepath.Join(filepath.Dir(config), "companion"))
		if err != nil {
			return unavailable("not_ready", "未找到该账号的旧 SQLite 记忆副本")
		}
		defer dir.Close()
		memoryCheck := legacyMemorySnapshotCheck{
			guard: source.guard, relative: filepath.ToSlash(filepath.Join(filepath.Dir(config), "companion", legacyMemoryDB)),
		}
		note, file, err := readStationLegacyScope(ctx, dir, account, thread, companion.ReadLegacyConversationPreference, memoryCheck)
		if err != nil {
			return unavailable("not_ready", "旧库不完整或无法核对该群身份")
		}
		if note == "" {
			return unavailable("empty", "该群没有保存过可接续的文本备注")
		}
		view = legacyGroupSettingView{Status: "ready", Note: note, CanEnableChannelsSummary: canEnableChannelsSummary}
		bindingFields = []any{"legacy-group-note-v2", legacyMemoryVersion{Size: file.Size(), Modified: file.ModTime().UTC()}, note, attachment}
		verifySource = func() bool {
			fresh, err := dir.Lstat(legacyMemoryDB)
			if err != nil || !sameLegacyMemoryFile(file, fresh) || !standaloneLegacyMemory(dir) {
				return false
			}
			return source.guard == nil || checkLegacyMemorySnapshotFile(ctx, memoryCheck, dir) == nil
		}
	}
	if source.guard != nil {
		bindingFields = append(bindingFields, source.guard.digest())
	}
	binding, err := json.Marshal(append(bindingFields, s.installation, id, account, thread, archive.Plan.Digest,
		source.Root, daemon.ServerID, daemon.OwnershipGeneration, evidence, reply, history, current))
	if err != nil {
		return legacyGroupSettingView{}, err
	}
	want := legacySettingsHash(binding)
	if digest == "" {
		view.Digest = want
		return view, nil
	}
	if digest != want {
		return unavailable("conflict", "旧资料或目标权限已变化，请重新预演")
	}
	latest, latestEvidence := s.checkLegacyCutover(ctx, id, native, snapshot, source, archive.LegacyServer)
	status, err := native.DaemonStatus(ctx)
	if err != nil || status != daemon || !legacyMemoryCutoverReady(latest) || latestEvidence != evidence {
		return unavailable("conflict", "切换前置条件已变化，请重新预演")
	}
	if !verifySource() {
		return unavailable("conflict", "旧资料在确认期间变化，请重新预演")
	}
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return legacyGroupSettingView{}, apphost.ErrLifecycleConflict
	}
	next := worker.policy
	next.GroupRules = maps.Clone(next.GroupRules)
	if next.GroupRules == nil {
		next.GroupRules = make(map[string]stationWechatGroupRule)
	}
	imports = maps.Clone(imports)
	if imports == nil {
		imports = make(map[string]string)
	}
	imports[thread] = legacyGroupSettingReceipt(want, enableChannelsSummary)
	if replyRule {
		if err := current.setReplySettings(*view.ReplySettings, time.Now().UTC()); err != nil {
			return legacyGroupSettingView{}, err
		}
		next.LegacyGroupReplyImports = imports
	} else {
		current.Note = view.Note
		if enableChannelsSummary {
			current.SummarizeSince = time.Now().UTC()
			if !validStationGroupRule(thread, current, next.Grants, next.AttachmentGrants) {
				return unavailable("conflict", "无法核对视频号自动总结的授权时间，请检查系统时间后重试")
			}
		}
		next.LegacyGroupNoteImports = imports
	}
	next.GroupRules[thread] = current
	if err := worker.save(next); err != nil {
		return legacyGroupSettingView{}, err
	}
	return legacyGroupSettingView{Status: "applied"}, nil
}

// Bind the opt-in choice to its durable receipt. A retried note-only request
// cannot turn summaries on, and replay never changes the current setting.
func legacyGroupSettingReceipt(digest string, enableChannelsSummary bool) string {
	if enableChannelsSummary {
		return localCheckpointKey("legacy-group-note-channels-summary-v1", digest)
	}
	return digest
}

func readLegacyGroupReplySettings(root *os.Root, config, thread string, guard *legacySnapshotGuard) (legacyGroupSettingView, string) {
	invalid := legacyGroupSettingView{Status: "not_ready", Reason: "旧配置不完整或无法核对该群规则"}
	raw, err := guard.readFile(root, config, 128<<10)
	if err != nil || !utf8.Valid(raw) {
		return invalid, ""
	}
	policy, err := decodeLegacyImportPolicy(raw)
	if err != nil {
		return invalid, ""
	}
	index := -1
	for i, candidate := range policy.Groups {
		if candidate.ID != thread {
			continue
		}
		if index >= 0 {
			return invalid, ""
		}
		index = i
	}
	if index < 0 {
		return legacyGroupSettingView{Status: "empty", Reason: "旧版未设置此群的回复规则"}, ""
	}
	group := policy.Groups[index]
	switch {
	case policy.Companion.Enabled && !policy.Companion.Permissions.ReplyText:
		return legacyGroupSettingView{Status: "unsupported", Reason: "旧版没有开启文字回复，请在新站单独设置规则"}, ""
	case group.Mode != "mention" && group.Mode != "all" && group.Mode != "social":
		return invalid, ""
	case group.Prefix == "" && policy.Prefix != "/ai ":
		return legacyGroupSettingView{Status: "unsupported", Reason: "此群沿用旧版自定义的全局前缀，请在新站单独核对"}, ""
	case !policy.Companion.Enabled && (group.Persona != "" || group.ForceReplyWhenMentioned):
		return legacyGroupSettingView{Status: "unsupported", Reason: "此群的角色规则在旧版未启用，请在新站单独核对"}, ""
	case group.Mode == "mention" && group.Prefix == "" && group.Persona == "" && !group.ForceReplyWhenMentioned:
		return legacyGroupSettingView{Status: "empty", Reason: "此群使用新站已有的默认回复方式"}, ""
	}
	settings := apphost.ConnectionGroupReplySettings{Mode: group.Mode, Prefix: group.Prefix, Persona: group.Persona, ForceReplyWhenMentioned: group.ForceReplyWhenMentioned}
	if !validStationGroupReplySettings(settings) {
		return invalid, ""
	}
	// Only values affecting this group's reply behavior contribute to the
	// preview version. The rest of config.json may contain credentials.
	version, err := json.Marshal([]any{settings, policy.Companion.Enabled})
	if err != nil {
		return invalid, ""
	}
	return legacyGroupSettingView{Status: "ready", ReplySettings: &settings}, legacySettingsHash(version)
}
