package station

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/pluginhost"
)

const (
	localRVCPackage    = "rvc"
	localRVCCapability = "rvc.convert"
	localRVCVoicesTool = "project_rvc_voices"
	localRVCStartTool  = "project_rvc_convert"
	localRVCJobTool    = "project_rvc_job"
	localRVCInputLimit = 25 << 20
)

var localRVCGrants = []string{"artifact.read", "artifact.write", "gpu"}
var errLocalRVCFollowupDeployment = errors.New("原 RVC 部署已变化或不可用")

type localRVCVoicePin struct {
	Label      string `json:"profile"`
	ConfigHash string `json:"config_sha256"`
	ModelHash  string `json:"model_sha256"`
	IndexHash  string `json:"index_sha256"`
	Device     string `json:"requested_device"`
}

func (p localRVCVoicePin) valid() bool {
	return safeRVCDisplay(p.Label, 128) && validLocalRVCHex(p.ConfigHash, 64) &&
		validLocalRVCHex(p.ModelHash, 64) && validLocalRVCHex(p.IndexHash, 64) &&
		(p.Device == "auto" || p.Device == "cpu" || p.Device == "cuda:0")
}

type localRVCInput struct {
	Artifact localArtifactLink    `json:"artifact"`
	Origin   *companion.SourceRef `json:"origin"`
	Trace    string               `json:"trace"`
	Binding  string               `json:"binding"`
	VoiceID  string               `json:"voice_profile_id"`
	Pin      localRVCVoicePin     `json:"pin"`
	Title    string               `json:"title"`
	Seconds  float64              `json:"seconds"`
}

type localRVCRemoteJob struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Engine  string `json:"engine"`
	VoiceID string `json:"voice_profile_id"`
	Recipe  struct {
		Title         string  `json:"title"`
		VoiceID       string  `json:"voice_profile_id"`
		ReferenceHash string  `json:"reference_sha256"`
		SourceHash    string  `json:"reference_source_sha256"`
		Start         float64 `json:"reference_start_seconds"`
		Seconds       float64 `json:"reference_seconds"`
	} `json:"recipe"`
	Pin          localRVCVoicePin `json:"singing_voice"`
	Hash         string           `json:"sha256"`
	Bytes        int64            `json:"bytes"`
	Duration     float64          `json:"duration_seconds"`
	Frames       uint32           `json:"frames"`
	SampleRate   uint32           `json:"sample_rate"`
	Channels     uint16           `json:"channels"`
	Bits         uint16           `json:"bits_per_sample"`
	Label        string           `json:"profile"`
	ModelHash    string           `json:"model_sha256"`
	IndexHash    string           `json:"index_sha256"`
	Device       string           `json:"requested_device"`
	ActualDevice string           `json:"rvc_device"`
}

func safeRVCDisplay(value string, limit int) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

func validLocalRVCHex(value string, size int) bool {
	return len(value) == size && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9')
	}) < 0
}

func localRVCSetupReason(err error) string {
	if err == nil {
		return "尚未登记获准使用的 RVC 声线。"
	}
	switch err.Error() {
	case "station_rvc_not_installed_or_authorized":
		return "请先在宿主私有目录登记并启用独立 RVC 后端。"
	case "station_rvc_workspace_overlaps_agent_home", "station_rvc_workspace_invalid":
		return "RVC 资源目录须独立于大肥鱼可编辑的工作空间。"
	case "station_rvc_control_unavailable", "station_rvc_control_invalid":
		return "Windows RVC 控制包缺失或完整性校验失败。"
	case "station_rvc_profile_registration_invalid":
		return "RVC 声线清单无效，请核对精确声线编号。"
	}
	return "请检查 Windows RVC 控制包、宿主部署登记和声线清单。"
}

func (t *localToolSet) localRVCTools(ctx context.Context) []mcptools.Tool {
	if t.projectTools == nil || t.projectTools.rvc == nil || t.run == nil || !t.run.Tracked {
		return nil
	}
	grant, err := t.access.resolve()
	if err != nil || !t.projectTools.rvcRegistered(grant.FishID) {
		return nil
	}
	host, err := t.projectTools.forFish(grant.FishID)
	if err != nil {
		return nil
	}
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localRVCPackage, capability: localRVCCapability}); err != nil {
		return nil
	}
	deployment, err := t.projectTools.rvc.Info(ctx, grant.FishID)
	if err != nil || len(deployment.Profiles) == 0 {
		return nil
	}
	return []mcptools.Tool{
		mcptools.NewTool(localRVCVoicesTool, "列出本机主人明确登记的 RVC 声线编号，以及接续声线的旧编号。开始换声前选择精确编号。", `{"type":"object","additionalProperties":false}`),
		mcptools.NewTool(localRVCStartTool, "为已登记的 PCM16 WAV 成品启动本地 RVC 换声。明确指定声线编号；返回持久任务编号。单段上限为 600 秒及 25 MiB，必要时先用音频工坊截取。", `{"type":"object","properties":{"artifact_id":{"type":"string"},"voice_profile_id":{"type":"string"},"title":{"type":"string","maxLength":100}},"required":["artifact_id","voice_profile_id"],"additionalProperties":false}`),
		mcptools.NewTool(localRVCJobTool, "查看原 RVC 任务并取得已校验成品，或按主人要求取消、续办原任务。使用提交时返回的 job_id；执行中可以稍后再查。", `{"type":"object","properties":{"job_id":{"type":"string"},"action":{"type":"string","enum":["status","cancel","resume"]}},"required":["job_id","action"],"additionalProperties":false}`),
	}
}

func (s *localProjectTools) rvcRegistered(fish string) bool {
	stat, err := os.Stat(filepath.Join(s.home.dataDir, "project-tools", fish))
	return err == nil && stat.IsDir()
}

func (t *localToolSet) callLocalRVC(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	return t.callLocalRVCMode(ctx, name, raw, false)
}

func (t *localToolSet) callLocalRVCMode(ctx context.Context, name string, raw json.RawMessage, existingOnly bool) (string, error) {
	s := t.projectTools
	if s == nil || s.rvc == nil || t.run == nil || !t.run.Tracked {
		return "", errors.New("当前对话没有本机 RVC 执行授权")
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	s.update.Lock()
	defer s.update.Unlock()
	grant, err := t.access.resolve()
	if err != nil {
		return "", err
	}
	host, err := s.forFish(grant.FishID)
	if err != nil {
		return "", err
	}
	installed, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localRVCPackage, capability: localRVCCapability})
	if err != nil {
		return "", err
	}
	deployment, err := s.rvc.Info(ctx, grant.FishID)
	if err != nil || deployment.Binding == "" {
		if existingOnly {
			return "", errLocalRVCFollowupDeployment
		}
		return "", errors.New(localRVCSetupReason(err))
	}
	cfg, err := host.store.Configuration(ctx, localRVCPackage)
	if err != nil {
		return "", err
	}
	env := localRVCInvocation{tool: t, host: host, grant: grant, installed: installed, deployment: deployment,
		configRevision: strconv.Itoa(cfg.Revision), existingOnly: existingOnly}
	switch name {
	case localRVCVoicesTool:
		if strictLocalMemoryDecode(raw, &struct{}{}) != nil {
			return "", errors.New("声线查询不需要参数")
		}
		return localProcessJSON(map[string]any{"voices": deployment.Profiles})
	case localRVCStartTool:
		return env.start(ctx, raw)
	default:
		return env.jobAction(ctx, raw)
	}
}

type localRVCInvocation struct {
	tool           *localToolSet
	host           *localProjectToolHost
	grant          localWorkspaceGrant
	installed      pluginhost.InstalledPackage
	deployment     localRVCDeployment
	configRevision string
	existingOnly   bool
}

func (e localRVCInvocation) voiceAvailable(id string) bool {
	for _, voice := range e.deployment.Profiles {
		if voice.ID == id {
			return true
		}
	}
	return false
}

func (e localRVCInvocation) profileID(id string) string {
	for _, voice := range e.deployment.Profiles {
		if voice.ID == id || (voice.LegacyVoiceID != "" && voice.LegacyVoiceID == id) {
			return voice.ID
		}
	}
	return ""
}

func (e localRVCInvocation) request(ctx context.Context, method, route, key string, data any) ([]byte, int, error) {
	var body []byte
	if data != nil {
		var err error
		body, err = json.Marshal(data)
		if err != nil {
			return nil, 0, err
		}
	}
	if e.existingOnly {
		return e.tool.projectTools.rvc.CallExisting(ctx, e.grant.FishID, e.deployment.Binding, method, route, key, body)
	}
	return e.tool.projectTools.rvc.Call(ctx, e.grant.FishID, e.deployment.Binding, method, route, key, body)
}

func (e localRVCInvocation) health(ctx context.Context, voiceID string) (localRVCVoicePin, error) {
	data, code, err := e.request(ctx, http.MethodGet, "/rvc-health?voice_profile_id="+url.QueryEscape(voiceID), "", nil)
	var out struct {
		localRVCVoicePin
		Ready   bool   `json:"ready"`
		Engine  string `json:"engine"`
		VoiceID string `json:"voice_profile_id"`
	}
	if err != nil || code != http.StatusOK || json.Unmarshal(data, &out) != nil ||
		!out.Ready || out.Engine != "rvc" || out.VoiceID != voiceID || !out.localRVCVoicePin.valid() {
		return localRVCVoicePin{}, errors.New("指定 RVC 声线未就绪，未提交换声")
	}
	return out.localRVCVoicePin, nil
}

func (e localRVCInvocation) start(ctx context.Context, raw json.RawMessage) (string, error) {
	var in struct {
		Artifact string `json:"artifact_id"`
		VoiceID  string `json:"voice_profile_id"`
		Title    string `json:"title"`
	}
	if strictLocalMemoryDecode(raw, &in) != nil || !artifactID(in.Artifact, "work-") {
		return "", errors.New("请选用本机成品编号和已授权的精确 RVC 声线编号")
	}
	in.VoiceID = e.profileID(in.VoiceID)
	if in.VoiceID == "" {
		return "", errors.New("请选用本机成品编号和已授权的精确 RVC 声线编号")
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		in.Title = "本机 RVC 换声"
	}
	if !safeRVCDisplay(in.Title, 100) {
		return "", errors.New("RVC 标题须为不超过 100 字的单行文字")
	}
	t, g := e.tool, e.grant
	source, ok := t.store.SourceForInput(companion.Input{Channel: "local", Account: g.FishID, Thread: g.OwnerID, Sender: g.OwnerID, TraceID: t.traceID})
	if !ok {
		return "", errors.New("本轮来源已变化，未提交换声")
	}
	op := companion.LocalToolCheckpoint{Tool: localRVCStartTool, Grant: localCheckpointKey(g), Target: in.Artifact,
		Key: localCheckpointKey(localRVCStartTool, source.EventID, source.Revision, in)}
	for _, previous := range t.run.Operations {
		if previous.Key == op.Key {
			return localProcessJSON(t.rvcCheckpointView(previous, g))
		}
	}
	if t.inspectOnly {
		return "", errors.New("续办只可核对原 RVC 任务")
	}
	bound, err := t.access.home.artifacts.bindProjectArtifact(ctx, in.Artifact, t.access, g)
	if err != nil {
		return "", err
	}
	audio, evidence, err := readRVCReference(ctx, bound)
	if err != nil {
		return "", err
	}
	seconds := evidence.seconds()
	if seconds > 600 {
		return "", errors.New("RVC 单段最长 600 秒，请先用音频工坊截取所需片段")
	}
	pin, err := e.health(ctx, in.VoiceID)
	if err != nil {
		return "", err
	}
	args := localRVCInput{Artifact: localArtifactLink{ID: in.Artifact, SHA256: bound.artifact.SHA256},
		Origin: source, Trace: t.traceID, Binding: e.deployment.Binding, VoiceID: in.VoiceID, Pin: pin, Title: in.Title, Seconds: seconds}
	arguments, _ := json.Marshal(args)
	versions := map[string]int{source.EventID: source.Revision}
	if parent := bound.artifact.Source; parent != nil {
		versions[parent.EventID] = parent.Revision
	}
	call := pluginhost.Invocation{Account: g.FishID, Thread: g.OwnerID, Requester: g.OwnerID, Purpose: "local RVC conversion",
		RequestKey: op.Key, Authorization: op.Grant, SourceVersions: versions}
	if err := pluginhost.Authorize(e.installed.Package, pluginhost.Grant{Account: g.FishID, Plugin: localRVCPackage,
		Version: e.installed.Package.Version, Permissions: e.installed.Permissions}, call, localRVCGrants); err != nil {
		return "", err
	}
	return t.dispatchCheckpointed(ctx, op, g, func() (string, string, error) {
		if err := e.checkSource(ctx, args, bound); err != nil {
			return "", "", err
		}
		job, created, err := e.host.store.CreateJob(ctx, pluginhost.Job{
			Plugin: localRVCPackage, Version: e.installed.Package.Version, PackageDigest: e.installed.Digest,
			ConfigRevision: e.configRevision, Context: call, Tool: localRVCCapability, Arguments: arguments})
		if err != nil {
			return "", "", err
		}
		if !created {
			view, err := localProcessJSON(rvcJobView(job))
			return view, "registered", err
		}
		job, err = e.host.store.ClaimJob(ctx, job.ID, job.Revision)
		if err != nil {
			return "", "", err
		}
		if err := e.checkSource(ctx, args, bound); err != nil {
			return "", "", err
		}
		payload := map[string]any{"title": args.Title, "voice_profile_id": args.VoiceID, "reference_base64": audio,
			"reference_start_seconds": 0, "reference_seconds": args.Seconds, "reference_pretrimmed": true,
			"reference_source_sha256": args.Artifact.SHA256, "profile_config_sha256": args.Pin.ConfigHash}
		result, code, requestErr := e.request(ctx, http.MethodPost, "/rvc-jobs", op.Key, payload)
		view := rvcJobView(job)
		if requestErr == nil {
			var rejection struct {
				Reason string `json:"error"`
			}
			if json.Unmarshal(result, &rejection) == nil && knownRVCRejection(code, rejection.Reason) {
				receipt, _ := json.Marshal(map[string]string{"reason": rejection.Reason})
				next, err := e.host.store.FailJob(ctx, job.ID, job.Revision, receipt)
				if err == nil {
					view = rvcJobView(next)
					view["reason"] = rejection.Reason
				}
			}
		}
		if requestErr == nil && (code == http.StatusAccepted || code == http.StatusOK) {
			var remote localRVCRemoteJob
			if json.Unmarshal(result, &remote) == nil && validLocalRVCHex(remote.ID, 32) && remote.Engine == "rvc" &&
				(code == http.StatusAccepted && remote.State == "queued" && remote.VoiceID == args.VoiceID ||
					code == http.StatusOK && remote.matches(args)) {
				save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				next, err := e.host.store.AcknowledgeJob(save, job.ID, job.Revision, remote.ID)
				cancel()
				if err == nil {
					view = rvcJobView(next)
					view["backend_state"] = "queued"
				}
			}
		}
		if err := e.checkSource(ctx, args, bound); err != nil {
			return "", "", err
		}
		value, err := localProcessJSON(view)
		return value, "registered", err
	})
}

func knownRVCRejection(status int, reason string) bool {
	switch reason {
	case "station_rvc_profile_changed", "job_history_limit":
		return status == http.StatusConflict
	case "queue_full":
		return status == http.StatusTooManyRequests
	case "rvc_profile_unavailable", "rvc_profile_required", "station_rvc_stopping":
		return status == http.StatusServiceUnavailable
	case "station_rvc_pcm16_reference_required", "station_rvc_profile_pin_required", "station_rvc_source_hash_required":
		return status == http.StatusBadRequest
	}
	return false
}

func (e localRVCInvocation) checkSource(ctx context.Context, args localRVCInput, bound *localProjectArtifactInput) error {
	t, g := e.tool, e.grant
	if args.Origin == nil || args.Binding != e.deployment.Binding || !e.voiceAvailable(args.VoiceID) ||
		t.run.CheckSource(ctx) != nil || !t.access.current(g) {
		return errors.New("RVC 原任务来源、授权或部署已变化")
	}
	artifactStore := t.access.home.artifacts
	if !artifactStore.sourceValid(localArtifact{Fish: g.FishID, Owner: g.OwnerID, Request: args.Trace, Source: args.Origin}) {
		return errors.New("RVC 原请求已变更或遗忘")
	}
	if bound.artifact.SHA256 != args.Artifact.SHA256 || bound.check(ctx) != nil {
		return errors.New("RVC 原成品已变更或遗忘")
	}
	return nil
}

func (r localRVCRemoteJob) matches(args localRVCInput) bool {
	switch r.State {
	case "queued", "running", "completed", "failed", "needs_review", "cancelling", "cancelled":
	default:
		return false
	}
	return validLocalRVCHex(r.ID, 32) && r.Engine == "rvc" && r.Recipe.Title == args.Title &&
		r.Recipe.VoiceID == args.VoiceID && r.Recipe.SourceHash == args.Artifact.SHA256 &&
		r.Recipe.ReferenceHash == args.Artifact.SHA256 && r.Recipe.Start == 0 && math.Abs(r.Recipe.Seconds-args.Seconds) < 1e-6 && r.Pin == args.Pin
}

func rvcJobView(job pluginhost.Job) map[string]any {
	state := job.State
	if state == "submitting" || state == "needs_review" {
		state = "needs_review"
	}
	if job.CancelRequested && job.Recovery() != "terminal" {
		state = "cancelling"
	}
	return map[string]any{"job_id": job.ID, "state": state}
}

func (t *localToolSet) rvcCheckpointView(op companion.LocalToolCheckpoint, g localWorkspaceGrant) map[string]any {
	view := map[string]any{"tool": op.Tool, "state": "needs_review", "recovered": true}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if t.projectTools == nil || t.run == nil || t.run.CheckSource(ctx) != nil {
		return view
	}
	host, err := t.projectTools.forFish(g.FishID)
	if err != nil {
		return view
	}
	if _, err := installedLocalProject(ctx, host, localProjectOperation{pkg: localRVCPackage, capability: localRVCCapability}); err != nil {
		return view
	}
	var job pluginhost.Job
	if op.Tool == localRVCJobTool {
		job, err = host.store.Job(ctx, op.Target)
	} else {
		job, err = host.store.JobByRequest(ctx, localRVCPackage, g.OwnerID, op.Key)
	}
	if err != nil || job.Plugin != localRVCPackage || job.Context.Authorization != op.Grant ||
		job.Context.Account != g.FishID || job.Context.Requester != g.OwnerID {
		return view
	}
	view = rvcJobView(job)
	view["tool"], view["recovered"] = op.Tool, true
	return view
}

// PCM16 is the independent bridge's actual input format. Scan chunks and
// reject truncated samples instead of reading a duration from an unchecked header.
type localRVCWAV struct {
	Frames, Rate uint32
	Channels     uint16
}

func (w localRVCWAV) seconds() float64 { return float64(w.Frames) / float64(w.Rate) }

func inspectRVCWAV(data []byte) (localRVCWAV, error) {
	invalid := errors.New("RVC 需要完整的单声道或双声道 PCM16 WAV")
	var result localRVCWAV
	u16, u32 := binary.LittleEndian.Uint16, binary.LittleEndian.Uint32
	if len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" || int64(u32(data[4:8]))+8 != int64(len(data)) {
		return result, invalid
	}
	var audioBytes uint32
	for i := 12; i < len(data); {
		if len(data)-i < 8 {
			return result, invalid
		}
		n := u32(data[i+4 : i+8])
		padded := int64(n) + int64(n&1)
		if padded > int64(len(data)-i-8) {
			return result, invalid
		}
		chunk := data[i+8 : i+8+int(n)]
		switch string(data[i : i+4]) {
		case "fmt ":
			if result.Rate != 0 || len(chunk) < 16 || u16(chunk[:2]) != 1 || u16(chunk[14:16]) != 16 {
				return result, invalid
			}
			result.Channels, result.Rate = u16(chunk[2:4]), u32(chunk[4:8])
			if result.Channels < 1 || result.Channels > 2 || result.Rate == 0 || u16(chunk[12:14]) != result.Channels*2 ||
				uint64(u32(chunk[8:12])) != uint64(result.Rate)*uint64(result.Channels)*2 {
				return result, invalid
			}
		case "data":
			if audioBytes != 0 || n == 0 {
				return result, invalid
			}
			audioBytes = n
		}
		i += 8 + int(padded)
	}
	if result.Rate == 0 || audioBytes == 0 || audioBytes%uint32(result.Channels*2) != 0 {
		return result, invalid
	}
	result.Frames = audioBytes / uint32(result.Channels*2)
	return result, nil
}
