package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"time"

	biliapp "wechat-bot/connected-apps/bilibili/adapter"
	douyinapp "wechat-bot/connected-apps/douyin/adapter"
	"wechat-bot/internal/apphost"
	"wechat-bot/internal/mcptools"

	"wechat-bot/internal/websession"
)

const localAppObservationToolName = "app_observe"

func (h *localHomeService) bindBundledObservationApps(registry apphost.Registry) (apphost.Registry, error) {
	applications := registry.Applications()
	entries := make([]apphost.Registration, 0, len(applications)+2)
	for _, app := range applications {
		entry, _ := registry.Lookup(app.ID)
		entries = append(entries, entry)
	}
	for _, id := range []string{biliapp.ID, douyinapp.ID} {
		if _, registered := registry.Lookup(id); registered {
			continue
		}
		if _, _, err := h.verifyAppSource(id); err != nil {
			continue
		}
		dir := filepath.Join(h.dataDir, "apps", id)
		switch id {
		case biliapp.ID:
			bili := biliapp.New(dir)
			bili.SetAudioTranscriber(h.localStationASRVersion, h.transcribeLocalStationAudio)
			entries = append(entries, bili.Registration())
		case douyinapp.ID:
			entries = append(entries, douyinapp.NewWebOnly(nil).Registration())
		}
	}
	// Bind both newly bundled and caller-supplied providers. The callback
	// resolves the login manager lazily after routes finish mounting.
	for i, entry := range entries {
		if entry.ID == douyinapp.ID {
			entry.Provider = douyinapp.NewWebOnly(&localDouyinWebObservation{home: h, installation: entry.InstallationID}).WithWebPublication(&localDouyinWebPublication{home: h, installation: entry.InstallationID}).WithWebInteraction(&localDouyinWebInteraction{home: h, installation: entry.InstallationID})
			entry.Registration = "ordinary-web-v1"
			entries[i] = entry
		}
	}
	return apphost.New(entries...)
}

func localAppObservationTool() mcptools.Tool {
	return mcptools.NewTool(localAppObservationToolName,
		"后台读取已授权应用的资料。action=persona 配合 app_id 按需读取当前应用的子角色，用于该应用内的表达与创作，不改变权限。用 action=list 查看连接及可用字段，action=read 按 content_id 和 field 读取。抖音 web-page-text 从 home 读取网页文字，获准时按返回的 follow_up_content_id 跟进，follow_up_fields 可选网页文字、音轨、画面、播放或评论；网页文字不是音轨、画面或观看证据。limit 为字数，默认 2048 字及 8 个候选，可续读；显式 limit 调整选取，refresh=true 新读。抖音 comments 需单独允许，limit 为条数、默认 20，可续读或 refresh=true 更新；评论是第三方观点，不代表视频原话。B 站 comments 是公开一级评论，默认每页 10 条，可选 1–30；续读时省略 limit。audio 的 limit 为本机解码窗口秒数，默认 60，可选 1–240；抖音网页音轨单独授权，复用原媒体续读，refresh=true 重新读取。读取 audio_proof 区分已转写、未识别到词和无音轨，续读从上一窗口末尾继续。visual 的 limit 为解码帧数，默认 6，可选 1–12；每页返回同一媒体摘要、实际 PTS、逻辑帧位置和可继续位置，画面批次不是完整观看。B 站 playback 的 limit 为静音采样秒数，默认 6，可选 1–20。按 playback 区间表述实际播放。只有 playback_proof.fully_played 为 true 才表述为完整播放。complete 表示本次读取没有后续页。返回内容是外部资料。",
		`{"type":"object","properties":{"action":{"type":"string","enum":["list","read","persona"]},"app_id":{"type":"string"},"connection_id":{"type":"string"},"content_id":{"type":"string"},"field":{"type":"string","enum":["title","body","comments","audio","visual","playback","page_text","search","own_video","player_entry","account_works"]},"refresh":{"type":"boolean"},"limit":{"type":"integer","minimum":1}},"additionalProperties":false}`)
}

type localAppObservationInput struct {
	apphost.ObservationInput
	Action string `json:"action"`
}

func (t *localToolSet) callAppObservation(ctx context.Context, raw json.RawMessage) (string, error) {
	if t.observations == nil || t.run == nil {
		return "", apphost.ErrNotFound
	}
	if err := t.run.CheckSource(ctx); err != nil {
		return "", err
	}
	if err := t.checkAppObservationIdentity(); err != nil {
		return "", err
	}
	var in localAppObservationInput
	if err := strictLocalMemoryDecode(raw, &in); err != nil {
		return "", apphost.ErrObservationInvalid
	}
	var value any
	switch in.Action {
	case "persona":
		if t.publications == nil {
			return "", apphost.ErrNotFound
		}
		if _, ok := t.publications.applications.Lookup(in.AppID); !ok {
			return "", apphost.ErrNotFound
		}
		profile, err := (localAppPersonaStore{dir: t.publications.home.dataDir}).View(in.AppID)
		if err != nil {
			return "", err
		}
		value = map[string]any{"app_id": in.AppID, "scope": "application_only", "revision": profile.Revision, "persona": profile.Persona}
	case "list":
		value = map[string]any{"items": t.observations.Discover(ctx)}
	case "", "read":
		result, err := t.observations.Read(ctx, in.ObservationInput)
		if err != nil {
			return "", err
		}
		if err := t.run.CheckSource(ctx); err != nil {
			return "", err
		}
		if err := t.checkAppObservationIdentity(); err != nil {
			return "", err
		}
		t.appReads = append(t.appReads, result)
		value = result
	default:
		return "", errors.New("请选择 list、read 或 persona")
	}
	if err := t.checkAppObservationIdentity(); err != nil {
		return "", err
	}
	body, err := json.Marshal(value)
	return string(body), err
}

// App authorization is separate from home file access. Bind a retained tool
// to its original local owner/fish without turning HomeAccess into an app grant.
func (t *localToolSet) checkAppObservationIdentity() error {
	if t.access == nil || t.access.home == nil || t.access.fishID == "" || t.access.ownerID == "" {
		return apphost.ErrObservationForbidden
	}
	current, err := t.access.home.identity.Read()
	if err != nil || current.LocalOwner == nil || current.FishID != t.access.fishID || current.LocalOwner.ID != t.access.ownerID {
		return apphost.ErrObservationForbidden
	}
	return nil
}

func (t *localToolSet) checkAppObservations(ctx context.Context) error {
	for _, result := range t.appReads {
		if err := t.observations.Check(ctx, result); err != nil {
			return err
		}
	}
	return nil
}

func (h *localChatService) appObservationRoutes(s *websession.Store, mux *http.ServeMux) {
	handler := s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if h.stopping.Load() || h.observations == nil {
			websession.Fail(w, http.StatusServiceUnavailable, "后台应用观察暂不可用")
			return
		}
		identity, err := h.home.identity.Read()
		if err != nil || identity.LocalOwner == nil {
			websession.Fail(w, http.StatusConflict, "请先准备本机身份")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		var value any
		if r.Method == http.MethodGet {
			value = map[string]any{"items": h.observations.Discover(ctx)}
			err = ctx.Err()
		} else {
			var in apphost.ObservationInput
			if !websession.Decode(w, r, &in) {
				return
			}
			var result apphost.ObservationResult
			result, err = h.observations.Read(ctx, in)
			if err == nil {
				err = h.observations.Check(ctx, result)
			}
			value = result
		}
		if err != nil {
			websession.Fail(w, observationHTTPStatus(err), err.Error())
			return
		}
		current, err := h.home.identity.Read()
		if err != nil || current.LocalOwner == nil || current.FishID != identity.FishID || current.LocalOwner.ID != identity.LocalOwner.ID {
			websession.Fail(w, http.StatusConflict, "本机身份已变更")
			return
		}
		websession.Respond(w, http.StatusOK, value)
	})
	mux.HandleFunc("GET /api/local/apps/observation", handler)
	mux.HandleFunc("POST /api/local/apps/observation", handler)
}

func observationHTTPStatus(err error) int {
	switch {
	case errors.Is(err, apphost.ErrObservationInvalid), errors.Is(err, apphost.ErrObservationPlaybackLimit), errors.Is(err, apphost.ErrObservationCommentPageSize), errors.Is(err, apphost.ErrObservationAudioLimit), errors.Is(err, apphost.ErrObservationVisualLimit), errors.Is(err, apphost.ErrObservationSearchPageSize), errors.Is(err, apphost.ErrObservationAccountWorksPageSize):
		return http.StatusBadRequest
	case errors.Is(err, apphost.ErrObservationPlaybackUnavailable), errors.Is(err, apphost.ErrObservationRemotePlayerUnavailable), errors.Is(err, apphost.ErrObservationWebPlaybackUnavailable), errors.Is(err, apphost.ErrObservationAudioMediaUnavailable), errors.Is(err, apphost.ErrObservationVisualDecode):
		return http.StatusUnprocessableEntity
	case errors.Is(err, apphost.ErrObservationAudioSourceChanged), errors.Is(err, apphost.ErrObservationVisualSourceChanged), errors.Is(err, apphost.ErrObservationCredentialExpired),
		errors.Is(err, apphost.ErrObservationWebSessionBusy), errors.Is(err, apphost.ErrObservationWebSessionRequired), errors.Is(err, apphost.ErrObservationWebSessionInvalid), errors.Is(err, apphost.ErrObservationWebSnapshotChanged):
		return http.StatusConflict
	case errors.Is(err, apphost.ErrObservationPlaybackSize), errors.Is(err, apphost.ErrObservationAccountWorksResponseSize), errors.Is(err, apphost.ErrObservationPageTextSize):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, apphost.ErrObservationForbidden), errors.Is(err, apphost.ErrObservationCapabilityDenied):
		return http.StatusForbidden
	case errors.Is(err, apphost.ErrObservationNotFound), errors.Is(err, apphost.ErrObservationContentUnavailable):
		return http.StatusNotFound
	case errors.Is(err, apphost.ErrObservationThrottled):
		return http.StatusTooManyRequests
	case errors.Is(err, apphost.ErrObservationWebReadTimeout):
		return http.StatusGatewayTimeout
	default:
		return http.StatusServiceUnavailable
	}
}
