package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationruntime"
)

type localToolSet struct {
	memory            companion.ToolAccess
	files             *localWorkspaceFileTools
	process           *localProcessTools
	processError      error
	projectTools      *localProjectTools
	access            *localWorkspaceAccess
	python            string
	run               *companion.LocalToolRun
	traceID           string
	inspectOnly       bool
	store             *companion.Store
	wechat            *stationWechat
	observations      *apphost.Observer
	appReads          []apphost.ObservationResult
	publications      *localChatService
	requestTime       time.Time
	continuationGuard func(context.Context) error
}

// Local owner actions already pass the actual tool's grant checks. Reuse the
// runner's existing autonomous loop instead of a zero/default tool-call quota.
func (*localToolSet) AutonomousActions() bool { return true }

func (h *localChatService) tools(store *companion.Store, in companion.Input) *localToolSet {
	access := &localWorkspaceAccess{home: h.home, fishID: in.Account, ownerID: in.Sender}
	h.mu.Lock()
	var err error
	if h.processes == nil && !h.closed && !h.stopping.Load() {
		h.processes, err = newLocalProcessTools(access, h.home.dataDir, h.windows)
	}
	process := h.processes
	python := ""
	if h.windows != nil {
		python = filepath.Join(filepath.Dir(h.windows.Pythonw), "python.exe")
	}
	h.mu.Unlock()
	tools := &localToolSet{
		memory: newLocalMemoryTools(store, in), files: newLocalFileTools(access), process: process,
		access: access, processError: err, python: python, traceID: in.TraceID,
		store: store,
	}
	if in.Channel == "local" && in.Mode == "direct" && in.Directed && in.Thread == in.Sender && !in.Preview && !in.Surf && !in.ReportOnly {
		tools.wechat = h.wechat
		tools.observations = h.observations
		tools.publications = h
		tools.projectTools = h.projectTools
		tools.requestTime, _ = time.Parse(time.RFC3339Nano, in.MessageTime)
	}
	return tools
}

func (t *localToolSet) List(ctx context.Context) ([]mcptools.Tool, error) {
	if t.continuationGuard != nil {
		if err := t.continuationGuard(ctx); err != nil {
			return nil, err
		}
	}
	all := []mcptools.Tool{}
	if t.publications != nil && t.run != nil {
		all = append(all, localPublicationTool(), localInteractionTool(), localLiveTool(), localFollowupTool())
	}
	if t.observations.HasDiscoverable(ctx) {
		all = append(all, localAppObservationTool())
	}
	if t.wechat != nil && t.wechat.hasContactGrant() {
		all = append(all, wechatapp.StationContactsTool())
	}
	if t.wechat != nil && t.run != nil && t.run.Tracked && len(t.wechat.stationMuteTargets(t.requestTime)) > 0 {
		all = append(all, wechatapp.StationGroupMuteTool())
	}
	if t.access.home.artifacts != nil && t.run != nil {
		all = append(all, mcptools.NewTool(localArtifactPublishTool, "把家目录中已完成的文件登记为不可变成品快照。path 为家目录相对路径，宿主自动计算并返回实际 SHA-256；已知哈希时可选 expected_sha256 核对指定版本，无需仅为登记另起进程算哈希。返回 artifact_id，不会向应用发送。", `{"type":"object","properties":{"path":{"type":"string"},"expected_sha256":{"type":"string","minLength":64,"maxLength":64}},"required":["path"],"additionalProperties":false}`))
	}
	all = append(all, t.localDocumentTools(ctx)...)
	all = append(all, t.localAudioTools(ctx)...)
	all = append(all, t.localRVCTools(ctx)...)
	all = append(all, t.localComfyTools(ctx)...)
	all = append(all, t.localYuETools(ctx)...)
	all = append(all, t.localTTSTools(ctx)...)
	var failures error
	accesses := []companion.ToolAccess{t.memory, t.files}
	if t.process != nil {
		accesses = append(accesses, t.process)
	}
	for _, access := range accesses {
		tools, err := access.List(ctx)
		if t.run != nil && access == t.process && len(tools) > 0 {
			tools[0].Description = "在大肥鱼家目录中启动、查看或取消 Windows 隔离进程。启动只接受绝对 exe 路径与 argv。聊天宿主按原请求和实际命令绑定启动编号，旧参数 request_key 可省略。跨请求查看或取消旧程序请用已有 id。"
		}
		all = append(all, tools...)
		failures = errors.Join(failures, err)
	}
	return all, errors.Join(failures, t.processError)
}

func (t *localToolSet) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if t.continuationGuard != nil {
		if err := t.continuationGuard(ctx); err != nil {
			return "", err
		}
	}
	if name == localFollowupToolName {
		return t.callFollowup(ctx, raw)
	}
	if name == localLiveToolName {
		return t.callLive(ctx, raw)
	}
	if name == localInteractionToolName {
		return t.callInteraction(ctx, raw)
	}
	if name == localPublicationToolName {
		return t.callPublication(ctx, raw)
	}
	if name == localAppObservationToolName {
		return t.callAppObservation(ctx, raw)
	}
	if name == wechatapp.StationContactsToolName {
		return t.readWechatContacts(ctx, raw)
	}
	if name == wechatapp.StationGroupMuteToolName {
		return t.localWechatGroupMute(ctx, raw)
	}
	if name == localDocumentToolName {
		return t.createLocalDocument(ctx, raw)
	}
	if name == localDocumentInspectTool || name == localDocumentExportTool {
		return t.useLocalDocumentArtifact(ctx, name, raw)
	}
	if name == localAudioToolName {
		return t.prepareLocalAudio(ctx, raw)
	}
	switch name {
	case localRVCVoicesTool, localRVCStartTool, localRVCJobTool:
		return t.callLocalRVC(ctx, name, raw)
	case localComfyListTool, localComfyStartTool, localComfyJobTool:
		return t.callLocalComfy(ctx, name, raw)
	case localYuEStartTool, localYuEJobTool:
		return t.callLocalYuE(ctx, name, raw)
	case localTTSVoicesTool, localTTSStartTool, localTTSJobTool:
		return t.callLocalTTS(ctx, name, raw)
	}
	if name == localArtifactPublishTool {
		return t.publishCheckpointed(ctx, raw)
	}
	if strings.HasPrefix(name, "memory_") {
		return t.memory.Call(ctx, name, raw)
	}
	if name == "server_windows_process" {
		if t.process == nil {
			return "", errors.New("本机进程记录不可用，程序未启动；请保留记录并检查私有目录")
		}
		if t.run != nil {
			var in localProcessInput
			if err := strictLocalMemoryDecode(raw, &in); err != nil {
				return "", errors.New("本机程序参数无效")
			}
			if in.Action == "start" {
				return t.startCheckpointed(ctx, in)
			}
		}
		return t.process.Call(ctx, name, raw)
	}
	if t.run != nil && (name == "server_write" || name == "server_edit") {
		return t.writeCheckpointed(ctx, name, raw)
	}
	result, err := t.files.Call(ctx, name, raw)
	if err == nil && name == "server_status" {
		var state map[string]any
		if json.Unmarshal([]byte(result), &state) == nil {
			state["windows_hidden_executor"] = t.python != "" && t.processError == nil
			state["execution_tool"] = "server_windows_process"
			data, _ := json.Marshal(state)
			result = string(data)
		}
	}
	return result, err
}

func (t *localToolSet) AgentContext(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}
	grant, err := t.access.resolve()
	var data map[string]any
	if err != nil {
		data = map[string]any{"kind": "local_workspace", "authorized": false, "note": "家目录操作已暂停或尚未授权；文字聊天和本机对话读取仍可用。"}
	} else {
		data = map[string]any{"kind": "local_workspace", "authorized": true, "home": grant.Directory, "python_executable": t.python, "tool_directory": "Project Tools", "note": "家目录内可保存、修改、读取自己的项目与工具。先按需发现准确工具schema；只有实际进程结果能证明程序已运行成功。指定Python仅在运行器已配置时可用；无应用接入，不代表具有平台外发能力。"}
		// Publish only host-selected, present command dependencies. Without this
		// small index the model has to guess executable locations, which produces
		// avoidable denied launches before it can write its own processing tool.
		if t.process != nil {
			programs := map[string]string{}
			for _, root := range stationruntime.PackagedCommandRuntimeRoots(t.process.cfg) {
				for _, name := range []string{"ffmpeg", "ffprobe"} {
					for _, relative := range []string{name + ".exe", filepath.Join("bin", name+".exe")} {
						path := filepath.Join(root, relative)
						if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
							programs[name] = path
							break
						}
					}
				}
			}
			if len(programs) > 0 {
				data["media_executables"] = programs
			}
		}
	}
	if t.run != nil && t.inspectOnly && t.run.CheckSource(ctx) == nil {
		const preview = 8
		ops := t.run.Operations
		items := make([]map[string]any, 0, min(preview, len(ops)))
		for _, op := range ops[max(0, len(ops)-preview):] {
			items = append(items, t.checkpointView(op, grant))
		}
		data["tool_recovery"] = map[string]any{
			"total": len(ops), "recent": items,
			"note": "正在续办原请求。先核对已有文件或按程序 id 查询、取消。结果未确认时如实说明；新增写入和启动需要主人发起新请求。",
		}
	}
	encoded, _ := json.Marshal(data)
	return string(encoded)
}
