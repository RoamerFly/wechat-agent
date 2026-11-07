package station

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"wechat-bot/internal/fishidentity"
	"wechat-bot/internal/hostfile"
	"wechat-bot/internal/stationbrowser"
	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

// Web login is an owner-confirmed browser profile, not an OAuth grant or a
// claim that the platform has verified an account. Never expose its contents
// to capability discovery, the editable fish home or model input.
type localDouyinWebView struct {
	Revision        uint64 `json:"revision"`
	State           string `json:"state"`
	Active          bool   `json:"active"`
	AttemptID       string `json:"attempt_id,omitempty"`
	ConfirmedAt     string `json:"confirmed_at,omitempty"`
	Available       bool   `json:"available"`
	Reason          string `json:"reason,omitempty"`
	ReadEnabled     bool   `json:"read_enabled"`
	ReadScope       string `json:"read_scope"`
	PlaybackEnabled bool   `json:"playback_enabled"`
	AudioEnabled    bool   `json:"audio_enabled"`
	VisualEnabled   bool   `json:"visual_enabled"`
	CommentsEnabled bool   `json:"comments_enabled"`
	LikesEnabled    bool   `json:"likes_enabled"`
	RepliesEnabled  bool   `json:"replies_enabled"`
	VisualReady     bool   `json:"visual_ready"`
	VisualReason    string `json:"visual_reason,omitempty"`
	PublishEnabled  bool   `json:"publish_enabled"`
}

type localDouyinWebInput struct {
	Action         string  `json:"action"`
	Revision       uint64  `json:"revision"`
	AttemptID      string  `json:"attempt_id,omitempty"`
	ApproveProfile bool    `json:"approve_profile"`
	AllowRead      *bool   `json:"allow_read,omitempty"`
	ReadScope      *string `json:"read_scope,omitempty"`
	AllowPlayback  *bool   `json:"allow_playback,omitempty"`
	AllowAudio     *bool   `json:"allow_audio,omitempty"`
	AllowVisual    *bool   `json:"allow_visual,omitempty"`
	AllowComments  *bool   `json:"allow_comments,omitempty"`
	AllowPublish   *bool   `json:"allow_publish,omitempty"`
	AllowLikes     *bool   `json:"allow_likes,omitempty"`
	AllowReplies   *bool   `json:"allow_replies,omitempty"`
}

type localDouyinWebRecord struct {
	Version  int    `json:"schema_version"`
	FishID   string `json:"fish_id"`
	Revision uint64 `json:"revision"`
	Profile  string `json:"profile,omitempty"`
	// ProfileEpoch changes before any browser login/creator re-login. It is
	// part of the host connection identity so a possibly changed platform
	// account cannot inherit an earlier task's target reference.
	ProfileEpoch    uint64 `json:"profile_epoch,omitempty"`
	ConfirmedAt     string `json:"confirmed_at,omitempty"`
	ReadEnabled     bool   `json:"read_enabled,omitempty"`
	ReadScope       string `json:"read_scope,omitempty"`
	PlaybackEnabled bool   `json:"playback_enabled,omitempty"`
	AudioEnabled    bool   `json:"audio_enabled,omitempty"`
	VisualEnabled   bool   `json:"visual_enabled,omitempty"`
	CommentsEnabled bool   `json:"comments_enabled,omitempty"`
	LikesEnabled    bool   `json:"likes_enabled,omitempty"`
	RepliesEnabled  bool   `json:"replies_enabled,omitempty"`
	PublishEnabled  bool   `json:"publish_enabled,omitempty"`
}

type localDouyinLoginWindow interface {
	Active() bool
	Close(context.Context) error
}

type localDouyinWebLogin struct {
	mu            sync.Mutex
	home          *localHomeService
	ctx           context.Context
	open          func(context.Context, string) (localDouyinLoginWindow, error)
	openCreator   func(context.Context, string) (localDouyinLoginWindow, error)
	available     func() bool
	readPage      func(context.Context, string, func() error) (stationbrowser.DouyinWebPage, error)
	readVideo     func(context.Context, string, string, func() error) (stationbrowser.DouyinWebPage, error)
	readComments  func(context.Context, string, stationbrowser.DouyinCommentsRequest, func() error) (stationbrowser.DouyinCommentPage, error)
	interact      func(context.Context, string, stationbrowser.DouyinInteractionRequest, func() error) (stationbrowser.DouyinInteractionResult, error)
	sampleVideo   func(context.Context, string, string, time.Duration, func() error) (stationbrowser.PlaybackSample, error)
	readMedia     func(context.Context, string, string, func() error) (stationbrowser.DouyinWebMedia, error)
	audio         stationLocalASR
	visual        stationLocalVision
	window        localDouyinLoginWindow
	cancel        context.CancelFunc
	attempt, fish string
}

func (h *localHomeService) douyinWebLoginRoutes(owner *websession.Store, mux *http.ServeMux, ctx context.Context) {
	m := &localDouyinWebLogin{home: h, ctx: ctx, available: stationbrowser.Available, readPage: stationbrowser.ReadDouyinHome, readVideo: stationbrowser.ReadDouyinVideo, readComments: stationbrowser.ReadDouyinComments, interact: stationbrowser.RunDouyinInteraction, sampleVideo: stationbrowser.SampleDouyinWebVideo,
		readMedia: stationbrowser.ReadDouyinWebMedia, audio: stationLocalASR{version: h.localDouyinAudioVersion, transcribe: h.transcribeLocalStationAudio},
		visual: stationLocalVision{version: h.localDouyinVisualVersion, observe: h.observeLocalStationImages},
		open: func(ctx context.Context, profile string) (localDouyinLoginWindow, error) {
			return stationbrowser.OpenDouyinLogin(ctx, profile)
		},
		openCreator: func(ctx context.Context, profile string) (localDouyinLoginWindow, error) {
			return stationbrowser.OpenDouyinCreatorLogin(ctx, profile)
		}}
	h.douyinWeb = m
	handler := owner.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		identity, err := h.identity.Read()
		if err != nil || identity.LocalOwner == nil || !fishidentity.ValidFishID(identity.FishID) {
			websession.Fail(w, http.StatusConflict, "请先准备本机主人身份")
			return
		}
		var input *localDouyinWebInput
		if r.Method == http.MethodPost {
			r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
			input = new(localDouyinWebInput)
			if !websession.Decode(w, r, input) {
				return
			}
		}
		view, status, err := m.apply(r.Context(), identity.FishID, input)
		if err != nil {
			websession.Fail(w, status, err.Error())
			return
		}
		view = m.withVisualReadiness(r.Context(), view)
		websession.Respond(w, http.StatusOK, view)
	})
	mux.HandleFunc("GET /api/local/apps/douyin/web-login", handler)
	mux.HandleFunc("POST /api/local/apps/douyin/web-login", handler)
}

func localDouyinWebID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validLocalDouyinWebID(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == s
}

func lockLocalDouyinWebProfile(profile string) (func(), error) {
	path, err := stationbrowser.DouyinLoginProfileLockPath(profile)
	if err != nil {
		return nil, err
	}
	return hostfile.TryLock(path)
}

func readLocalDouyinWebRecord(root *os.Root, fish string) (localDouyinWebRecord, error) {
	record := localDouyinWebRecord{Version: 1, FishID: fish}
	info, err := root.Lstat("login.json")
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return record, errors.New("网页登录登记无法读取，原资料保留")
	}
	f, err := workspacefs.OpenRegular(root, "login.json")
	if err != nil {
		return record, errors.New("网页登录登记无法读取，原资料保留")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.FishID != fish ||
		(record.Profile != "" && !validLocalDouyinWebID(record.Profile)) || (record.Profile == "" && record.ConfirmedAt != "") {
		return record, errors.New("网页登录登记无效，原资料保留")
	}
	if record.ReadEnabled && (record.Profile == "" || record.ConfirmedAt == "") {
		return record, errors.New("网页读取授权没有已确认的登录资料，原资料保留")
	}
	if record.ReadScope != "" && record.ReadScope != "home" && record.ReadScope != "home_and_videos" {
		return record, errors.New("网页读取范围无效，原资料保留")
	}
	if record.ReadScope == "home_and_videos" && !record.ReadEnabled {
		return record, errors.New("网页读取范围缺少有效许可，原资料保留")
	}
	if record.PlaybackEnabled && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("网页播放许可缺少有效的视频页读取范围，原资料保留")
	}
	if record.AudioEnabled && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("音轨读取许可缺少有效的视频页读取范围，原资料保留")
	}
	if record.VisualEnabled && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("画面分析许可缺少有效的视频页读取范围，原资料保留")
	}
	if record.CommentsEnabled && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("评论读取许可缺少有效的视频页读取范围，原资料保留")
	}
	if record.LikesEnabled && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("点赞许可缺少有效的视频页读取范围，原资料保留")
	}
	if record.RepliesEnabled && (!record.CommentsEnabled || !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
		return record, errors.New("评论回复许可缺少有效的评论读取范围，原资料保留")
	}
	if record.PublishEnabled && (record.Profile == "" || record.ConfirmedAt == "") {
		return record, errors.New("网页发布授权没有已确认的登录资料，原资料保留")
	}
	if record.ConfirmedAt != "" {
		if _, err := time.Parse(time.RFC3339, record.ConfirmedAt); err != nil {
			return record, errors.New("网页登录登记时间无效，原资料保留")
		}
	}
	return record, nil
}

func (m *localDouyinWebLogin) view(record localDouyinWebRecord) localDouyinWebView {
	v := localDouyinWebView{Revision: record.Revision, State: "unconfigured", Available: m.available(), ConfirmedAt: record.ConfirmedAt, ReadScope: localDouyinWebScope(record)}
	if record.Profile != "" {
		v.State = "unconfirmed"
	}
	if record.ConfirmedAt != "" {
		v.State = "saved"
	}
	if record.Profile != "" {
		profile, err := workspacefs.OpenPath(filepath.Join(m.home.dataDir, "apps", "douyin", "web-login", record.FishID, record.Profile))
		if err != nil {
			v.State, v.ConfirmedAt = "unconfirmed", ""
			v.Reason = "网页登录资料目录缺失或不可用，请重新登录或移除登记"
		} else {
			profile.Close()
		}
	}
	if m.window != nil && m.fish == record.FishID {
		v.AttemptID = m.attempt
		if m.window.Active() {
			v.State, v.Active = "login_open", true
		}
	}
	if !v.Available {
		v.Reason = "未找到可用的 Microsoft Edge，已有网页登录资料保留"
	}
	v.ReadEnabled = record.ReadEnabled && v.State == "saved" && v.AttemptID == ""
	v.PlaybackEnabled = v.ReadEnabled && record.PlaybackEnabled
	v.AudioEnabled = v.ReadEnabled && record.AudioEnabled
	v.VisualEnabled = v.ReadEnabled && record.VisualEnabled
	v.CommentsEnabled = v.ReadEnabled && v.ReadScope == "home_and_videos" && record.CommentsEnabled
	v.LikesEnabled = v.ReadEnabled && v.ReadScope == "home_and_videos" && record.LikesEnabled
	v.RepliesEnabled = v.CommentsEnabled && record.RepliesEnabled
	v.PublishEnabled = record.PublishEnabled && v.State == "saved" && v.AttemptID == ""
	return v
}

// withVisualReadiness is deliberately only used while preparing the owner UI
// response. Runtime grants independently call visual.ready immediately before
// an observation, so a stale UI state can never authorize a visual request.
func (m *localDouyinWebLogin) withVisualReadiness(ctx context.Context, view localDouyinWebView) localDouyinWebView {
	if !view.ReadEnabled || view.ReadScope != "home_and_videos" {
		return view
	}
	if _, err := m.visual.ready(ctx); err != nil {
		view.VisualReason = "本机视觉工具尚未就绪，请在项目工具中配置媒体理解。"
		return view
	}
	view.VisualReady = true
	return view
}

// localDouyinWebRevokesOnly permits a durable withdrawal even while the
// browser service is stopping. No enable or scope expansion may use this
// exception.
func localDouyinWebRevokesOnly(in *localDouyinWebInput) bool {
	if in == nil {
		return false
	}
	switch in.Action {
	case "clear":
		return in.ApproveProfile
	case "set_publish":
		return in.AllowPublish != nil && !*in.AllowPublish
	case "set_interactions":
		return (in.AllowLikes != nil || in.AllowReplies != nil) &&
			(in.AllowLikes == nil || !*in.AllowLikes) &&
			(in.AllowReplies == nil || !*in.AllowReplies)
	case "set_read":
		return in.AllowRead != nil && !*in.AllowRead &&
			(in.AllowPlayback == nil || !*in.AllowPlayback) &&
			(in.AllowAudio == nil || !*in.AllowAudio) &&
			(in.AllowVisual == nil || !*in.AllowVisual) &&
			(in.AllowComments == nil || !*in.AllowComments)
	default:
		return false
	}
}

func (m *localDouyinWebLogin) apply(ctx context.Context, fish string, in *localDouyinWebInput) (localDouyinWebView, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fail := func(code int, message string) (localDouyinWebView, int, error) {
		return localDouyinWebView{}, code, errors.New(message)
	}
	dir := filepath.Join(m.home.dataDir, "apps", "douyin", "web-login", fish)
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return fail(503, "网页登录私有目录不可用")
	}
	defer root.Close()
	unlock, err := hostfile.TryLock(filepath.Join(dir, "login.lock"))
	if err != nil {
		return fail(409, "网页登录状态正在更新，请稍后重新读取")
	}
	defer unlock()
	record, err := readLocalDouyinWebRecord(root, fish)
	if err != nil {
		return fail(503, err.Error())
	}
	if in == nil {
		return m.view(record), 200, nil
	}
	if m.ctx.Err() != nil && !localDouyinWebRevokesOnly(in) {
		return fail(503, "服务正在停止，请重新启动后登录")
	}
	if in.Revision != record.Revision {
		return fail(409, "网页登录状态已变化，请重新读取后操作")
	}
	save := func() error {
		record.Revision++
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return hostfile.WritePrivate(filepath.Join(dir, "login.json"), raw)
	}
	switch in.Action {
	case "set_publish":
		if in.AllowPublish == nil {
			return fail(400, "请明确是否允许大肥鱼通过该网页登录资料发布作品")
		}
		view := m.view(record)
		if *in.AllowPublish && (view.State != "saved" || view.AttemptID != "" || !view.Available) {
			return fail(409, "请先完成网页登录并确认保存，再开启作品发布")
		}
		record.PublishEnabled = *in.AllowPublish
		if save() != nil {
			return fail(503, "网页发布授权未保存，请重新读取后核对")
		}
	case "begin_creator":
		view := m.view(record)
		if !record.PublishEnabled || view.State != "saved" || view.AttemptID != "" || !view.Available {
			return fail(409, "请先完成网页登录并开启作品发布")
		}
		if m.window != nil && m.window.Active() {
			return fail(409, "登录窗口已打开，请先完成或取消本次登录")
		}
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.window, m.attempt, m.fish = nil, "", ""
		profile := filepath.Join(dir, record.Profile)
		probe, err := lockLocalDouyinWebProfile(profile)
		if err != nil {
			return fail(409, "网页登录资料仍在使用，请先关闭原登录窗口")
		}
		probe()
		// Opening creator login can switch the account inside this profile.
		// Revoke every old side-effect/read grant before the browser opens; the
		// owner explicitly confirms and re-enables what they want afterwards.
		record.ProfileEpoch++
		record.ConfirmedAt = ""
		record.ReadEnabled = false
		record.ReadScope = ""
		record.PlaybackEnabled = false
		record.AudioEnabled = false
		record.VisualEnabled = false
		record.CommentsEnabled = false
		record.LikesEnabled = false
		record.RepliesEnabled = false
		record.PublishEnabled = false
		if save() != nil {
			return fail(503, "无法保存创作者中心登录前的授权撤销，未启动浏览器")
		}
		attempt, err := localDouyinWebID()
		if err != nil {
			return fail(503, "无法创建本次创作者中心编号")
		}
		browserCtx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		window, err := m.openCreator(browserCtx, profile)
		if err != nil {
			cancel()
			return fail(503, "创作者中心无法打开或网页登录资料正在使用")
		}
		if window == nil {
			cancel()
			return fail(503, "创作者中心无法打开或网页登录资料正在使用")
		}
		m.window, m.cancel, m.attempt, m.fish = window, cancel, attempt, fish
	case "set_read":
		if in.AllowRead == nil {
			return fail(400, "请明确是否允许大肥鱼读取抖音首页网页文字")
		}
		if in.ReadScope != nil && *in.ReadScope != "home" && *in.ReadScope != "home_and_videos" {
			return fail(400, "网页读取范围须为首页或首页及视频页文字")
		}
		view := m.view(record)
		if *in.AllowRead && (view.State != "saved" || view.AttemptID != "" || !view.Available) {
			return fail(409, "请先完成网页登录并确认保存，再开启网页文字读取")
		}
		record.ReadEnabled = *in.AllowRead
		if !record.ReadEnabled {
			record.ReadScope = ""
		} else if in.ReadScope != nil {
			record.ReadScope = *in.ReadScope
		}
		if in.AllowPlayback != nil && *in.AllowPlayback && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
			return fail(400, "请先允许首页及视频页文字读取，再单独允许静音播放观察")
		}
		if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
			record.PlaybackEnabled = false
		} else if in.AllowPlayback != nil {
			record.PlaybackEnabled = *in.AllowPlayback
		}
		if in.AllowAudio != nil && *in.AllowAudio {
			if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
				return fail(400, "请先允许首页及视频页文字读取，再单独允许本机音轨转写")
			}
			if _, err := m.audio.ready(ctx); err != nil {
				return fail(503, "本机音轨识别尚未就绪，请在项目工具中启用本地媒体识别并核对解码器与模型资源；许可未保存")
			}
		}
		if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
			record.AudioEnabled = false
		} else if in.AllowAudio != nil {
			record.AudioEnabled = *in.AllowAudio
		}
		if in.AllowVisual != nil && *in.AllowVisual {
			if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
				return fail(400, "请先允许首页及视频页文字读取，再单独允许画面分析")
			}
			if _, err := m.visual.ready(ctx); err != nil {
				return fail(503, "本机视觉工具尚未就绪，请在项目工具中启用媒体理解并配置视觉服务；许可未保存")
			}
		}
		if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
			record.VisualEnabled = false
		} else if in.AllowVisual != nil {
			record.VisualEnabled = *in.AllowVisual
		}
		if in.AllowComments != nil && *in.AllowComments && (!record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos") {
			return fail(400, "请先允许首页及视频页文字读取，再单独允许读取评论")
		}
		if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
			record.CommentsEnabled = false
		} else if in.AllowComments != nil {
			record.CommentsEnabled = *in.AllowComments
		}
		if !record.CommentsEnabled {
			record.RepliesEnabled = false
		}
		if !record.ReadEnabled || localDouyinWebScope(record) != "home_and_videos" {
			record.LikesEnabled = false
			record.RepliesEnabled = false
		}
		if save() != nil {
			return fail(503, "网页读取授权未保存，请重新读取后核对")
		}
	case "set_interactions":
		if in.AllowLikes == nil && in.AllowReplies == nil {
			return fail(400, "请明确是否允许点赞或回复评论")
		}
		view := m.view(record)
		if in.AllowLikes != nil && *in.AllowLikes && (view.State != "saved" || view.AttemptID != "" || !view.Available || !view.ReadEnabled || view.ReadScope != "home_and_videos") {
			return fail(409, "请先完成网页登录并允许读取首页及视频页文字，再开启点赞")
		}
		if in.AllowReplies != nil && *in.AllowReplies && (view.State != "saved" || view.AttemptID != "" || !view.Available || !view.CommentsEnabled) {
			return fail(409, "请先完成网页登录、允许读取首页及视频页文字和视频评论，再开启回复评论")
		}
		if in.AllowLikes != nil {
			record.LikesEnabled = *in.AllowLikes
		}
		if in.AllowReplies != nil {
			record.RepliesEnabled = *in.AllowReplies
		}
		if save() != nil {
			return fail(503, "抖音互动授权未保存，请重新读取后核对")
		}
	case "begin":
		if !in.ApproveProfile {
			return fail(400, "请确认创建或使用项目专属的抖音网页登录资料")
		}
		if !m.available() {
			return fail(503, "未找到可用的 Microsoft Edge")
		}
		if m.window != nil && m.window.Active() {
			return fail(409, "登录窗口已打开，请先完成或取消本次登录")
		}
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		m.window, m.attempt, m.fish = nil, "", ""
		if record.Profile == "" {
			record.Profile, err = localDouyinWebID()
			if err != nil {
				return fail(503, "无法创建网页登录资料编号")
			}
		}
		profile := filepath.Join(dir, record.Profile)
		p, err := workspacefs.EnsurePath(profile)
		if err != nil {
			return fail(503, "网页登录资料目录不可用")
		}
		p.Close()
		// Reject another live host before changing the durable revision used
		// by that host's confirmation. The opener takes this lock again for
		// the browser lifetime; the metadata lock serializes owner actions.
		probe, err := lockLocalDouyinWebProfile(profile)
		if err != nil {
			return fail(409, "网页登录资料仍在使用，请先关闭原登录窗口")
		}
		probe()
		// Beginning a new login never carries forward an old confirmation.
		record.ProfileEpoch++
		record.ConfirmedAt = ""
		record.ReadEnabled = false
		record.ReadScope = ""
		record.PlaybackEnabled = false
		record.AudioEnabled = false
		record.VisualEnabled = false
		record.CommentsEnabled = false
		record.LikesEnabled = false
		record.RepliesEnabled = false
		record.PublishEnabled = false
		if save() != nil {
			return fail(503, "无法保存网页登录登记，未启动浏览器")
		}
		attempt, err := localDouyinWebID()
		if err != nil {
			return fail(503, "无法创建本次登录编号")
		}
		browserCtx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		window, err := m.open(browserCtx, profile)
		if err != nil {
			cancel()
			return fail(503, "登录窗口启动失败或资料正被使用，请稍后重试")
		}
		m.window, m.cancel, m.attempt, m.fish = window, cancel, attempt, fish
	case "confirm", "cancel":
		if in.AttemptID == "" || in.AttemptID != m.attempt || m.window == nil || m.fish != fish {
			return fail(409, "本次登录已结束或服务已重启，请重新打开登录窗口")
		}
		closeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = m.window.Close(closeCtx)
		cancel()
		if m.cancel != nil {
			m.cancel()
		}
		m.window, m.cancel, m.attempt = nil, nil, ""
		if err != nil {
			return fail(503, "登录窗口未能确认正常退出，尚未确认保存；请重新打开后核对")
		}
		if in.Action == "confirm" {
			if m.ctx.Err() != nil {
				return fail(503, "服务已停止，尚未确认登录保存")
			}
			record.ConfirmedAt = time.Now().UTC().Format(time.RFC3339)
		}
		if save() != nil {
			return fail(503, "登录窗口已关闭，但状态未保存；请重新读取后核对")
		}
	case "clear":
		if !in.ApproveProfile {
			return fail(400, "请确认移除项目专属的网页登录资料")
		}
		// Revoke pending reads before trying to close/delete profile files.
		// Another browser may still own the profile lock during that cleanup.
		if record.ReadEnabled || record.PublishEnabled || record.CommentsEnabled || record.LikesEnabled || record.RepliesEnabled {
			record.ReadEnabled = false
			record.ReadScope = ""
			record.PlaybackEnabled = false
			record.AudioEnabled = false
			record.VisualEnabled = false
			record.CommentsEnabled = false
			record.LikesEnabled = false
			record.RepliesEnabled = false
			record.PublishEnabled = false
			if save() != nil {
				return fail(503, "网页读取授权未能撤销，尚未清理资料")
			}
		}
		if m.window != nil && m.fish == fish {
			closeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err = m.window.Close(closeCtx)
			cancel()
			if m.cancel != nil {
				m.cancel()
			}
			m.window, m.cancel, m.attempt = nil, nil, ""
			if err != nil {
				return fail(503, "登录窗口尚未确认退出，未删除网页登录资料")
			}
		}
		if record.Profile != "" {
			// The browser takes this same lock for its entire process lifetime.
			release, err := lockLocalDouyinWebProfile(filepath.Join(dir, record.Profile))
			if err != nil {
				return fail(409, "网页登录资料仍在使用，未删除")
			}
			defer release()
			if err = root.RemoveAll(record.Profile); err != nil {
				return fail(503, "网页登录资料未能全部删除，请重试")
			}
		}
		record.Profile, record.ConfirmedAt = "", ""
		if save() != nil {
			return fail(503, "网页登录资料已移除，但登记未更新；请重新读取后重试")
		}
	default:
		return fail(400, "不支持的网页登录操作")
	}
	return m.view(record), 200, nil
}

func localDouyinWebScope(record localDouyinWebRecord) string {
	if record.ReadScope == "home_and_videos" {
		return record.ReadScope
	}
	return "home"
}
