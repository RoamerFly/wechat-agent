package station

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"wechat-bot/internal/obspreview"
	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/websession"
	catalog "wechat-bot/plugins"
)

const (
	localOBSPackage       = "obs-preview"
	localOBSDefaultPort   = 4455
	localOBSPreviewIdle   = time.Minute
	localOBSFrameInterval = time.Second
)

type localOBSPreview struct {
	mu       sync.Mutex
	session  *localOBSPreviewSession
	inFlight *localOBSPreviewSession
	epoch    uint64
	closed   bool
	capture  func(context.Context, int, string) (obspreview.Frame, error)
}

type localOBSPreviewSession struct {
	ctx        context.Context
	cancel     context.CancelFunc
	timer      *time.Timer
	expires    time.Time
	nextFrame  time.Time
	scene      string
	observedAt string
}

type localOBSView struct {
	Configured     bool   `json:"configured"`
	ConfigRevision int    `json:"config_revision"`
	Port           int    `json:"port"`
	Enabled        bool   `json:"enabled"`
	Active         bool   `json:"active"`
	Scene          string `json:"scene,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
	ImageData      string `json:"image_data,omitempty"`
}

var errOBSPreviewBusy = errors.New("OBS preview frame still in flight")
var errOBSPreviewInactive = errors.New("OBS preview stopped")
var errOBSPreviewFast = errors.New("OBS preview frame rate exceeded")
var errOBSPreviewSession = errors.New("OBS preview owner session expired")

func (p *localOBSPreview) status(view localOBSView) localOBSView {
	p.mu.Lock()
	defer p.mu.Unlock()
	if view.Enabled && p.session != nil {
		view.Active = true
		view.Scene, view.ObservedAt = p.session.scene, p.session.observedAt
	}
	return view
}

func (p *localOBSPreview) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *localOBSPreview) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.stopLocked()
}

func (p *localOBSPreview) stopLocked() {
	p.epoch++
	if p.session != nil {
		p.session.cancel()
		p.session.timer.Stop()
		p.session = nil
	}
}

func (p *localOBSPreview) ticket() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch
}

func (p *localOBSPreview) claim(start bool, ticket uint64) (*localOBSPreviewSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || ticket != p.epoch {
		return nil, errOBSPreviewInactive
	}
	if p.inFlight != nil {
		return nil, errOBSPreviewBusy
	}
	if start {
		if p.session != nil {
			return nil, errOBSPreviewBusy
		}
		ctx, cancel := context.WithCancel(context.Background())
		p.session = &localOBSPreviewSession{ctx: ctx, cancel: cancel}
	} else if p.session == nil {
		return nil, errOBSPreviewInactive
	}
	now := time.Now()
	session := p.session
	if now.Before(session.nextFrame) {
		return nil, errOBSPreviewFast
	}
	session.nextFrame = now.Add(localOBSFrameInterval)
	session.expires = now.Add(localOBSPreviewIdle)
	expires := session.expires
	if session.timer != nil {
		session.timer.Stop()
	}
	session.timer = time.AfterFunc(localOBSPreviewIdle, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.session == session && session.expires == expires {
			p.stopLocked()
		}
	})
	p.inFlight = session
	return session, nil
}

func (p *localOBSPreview) read(ctx context.Context, session *localOBSPreviewSession, port int, password string) (obspreview.Frame, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	unbind := context.AfterFunc(session.ctx, cancel)
	defer unbind()
	if err := session.ctx.Err(); err != nil {
		return obspreview.Frame{}, err
	}
	if p.capture != nil {
		return p.capture(ctx, port, password)
	}
	return obspreview.Capture(ctx, port, password)
}

func (p *localOBSPreview) finish(session *localOBSPreviewSession, view localOBSView, frame obspreview.Frame, err error) (localOBSView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inFlight = nil
	if p.session != session {
		return localOBSView{}, errOBSPreviewInactive
	}
	if err != nil {
		p.stopLocked()
		return localOBSView{}, err
	}
	session.scene, session.observedAt = frame.Scene, time.Now().UTC().Format(time.RFC3339)
	view.Active, view.Scene, view.ObservedAt, view.ImageData = true, session.scene, session.observedAt, frame.ImageData
	return view, nil
}

func localOBSConfiguration(ctx context.Context, host *localProjectToolHost) (localOBSView, error) {
	view := localOBSView{Port: localOBSDefaultPort}
	item, err := host.store.Installed(ctx, localOBSPackage)
	if err != nil {
		return view, err
	}
	contract, err := catalog.ContractFor(localOBSPackage)
	if err != nil || item.Removed || item.Digest != contract.ManifestSHA256 {
		return view, pluginhost.ErrRevision
	}
	config, err := host.store.Configuration(ctx, localOBSPackage)
	if err != nil {
		return view, err
	}
	if raw, ok := config.Values["port"]; ok {
		if err := json.Unmarshal(raw, &view.Port); err != nil || view.Port < 1 || view.Port > 65535 {
			return view, pluginhost.ErrRevision
		}
	}
	view.ConfigRevision, view.Enabled = config.Revision, item.Enabled
	view.Configured = slices.Contains(config.ConfiguredSecrets, "password")
	return view, nil
}

type localOBSInput struct {
	Action         string `json:"action"`
	Port           int    `json:"port,omitempty"`
	Password       string `json:"password,omitempty"`
	ConfigRevision int    `json:"config_revision,omitempty"`
}

// prepareOBS runs under the same lock as enabling, disabling and shutdown. The
// socket read happens after releasing it so stop can cancel an in-flight read.
func (s *localProjectTools) prepareOBS(ctx context.Context, in localOBSInput, sessionOK func() bool) (localOBSView, *localOBSPreviewSession, string, error) {
	ticket := s.obs.ticket()
	if !sessionOK() {
		return localOBSView{}, nil, "", errOBSPreviewSession
	}
	if in.Action == "stop" {
		s.obs.stop()
	}
	if in.Action != "status" && in.Action != "stop" {
		s.update.Lock()
		defer s.update.Unlock()
	}
	record, err := s.home.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return localOBSView{}, nil, "", pluginhost.ErrRevision
	}
	host, err := s.forFish(record.FishID)
	if err != nil {
		return localOBSView{}, nil, "", err
	}
	view, err := localOBSConfiguration(ctx, host)
	if err != nil {
		return view, nil, "", err
	}
	switch in.Action {
	case "status", "stop":
		return s.obs.status(view), nil, "", nil
	case "configure":
		if view.Enabled || in.ConfigRevision != view.ConfigRevision {
			return view, nil, "", pluginhost.ErrRevision
		}
		config, err := host.store.SaveConfiguration(ctx, localOBSPackage, pluginhost.ConfigUpdate{
			Revision: in.ConfigRevision,
			Values:   map[string]json.RawMessage{"port": json.RawMessage(strconv.Itoa(in.Port))},
			Secrets:  map[string]string{"password": in.Password},
		})
		if err != nil {
			return view, nil, "", err
		}
		view.Port, view.ConfigRevision, view.Configured = in.Port, config.Revision, true
		return view, nil, "", nil
	default:
		if !view.Configured || !view.Enabled {
			return view, nil, "", pluginhost.ErrRevision
		}
		config, err := host.store.ConfigurationForExecution(ctx, localOBSPackage)
		var password string
		if err != nil || config.Revision != view.ConfigRevision || json.Unmarshal(config.Values["password"], &password) != nil || password == "" {
			return view, nil, "", pluginhost.ErrRevision
		}
		session, err := s.obs.claim(in.Action == "start", ticket)
		return view, session, password, err
	}
}

func (s *localProjectTools) obsPreview(owner *websession.Store, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	in := localOBSInput{Action: "status"}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if !websession.Decode(w, r, &in) {
			return
		}
		if in.Action == "configure" {
			if in.Port < 1 || in.Port > 65535 || in.Password == "" || len(in.Password) > 1024 || in.ConfigRevision < 0 {
				websession.Fail(w, 400, "请填写有效的 OBS 本机端口及 WebSocket 密码")
				return
			}
		} else if (in.Action != "start" && in.Action != "frame" && in.Action != "stop") || in.Port != 0 || in.Password != "" || in.ConfigRevision != 0 {
			websession.Fail(w, 400, "OBS 预览操作无效")
			return
		}
	}
	view, session, password, err := s.prepareOBS(r.Context(), in, func() bool {
		_, _, ok := owner.Lookup(r)
		return ok
	})
	if err == nil && session != nil {
		frame, readErr := s.obs.read(r.Context(), session, view.Port, password)
		view, err = s.obs.finish(session, view, frame, readErr)
	}
	if err != nil {
		status, message := localOBSFailure(err)
		websession.Fail(w, status, message)
		return
	}
	websession.Respond(w, 200, view)
}

func localOBSFailure(err error) (int, string) {
	switch {
	case errors.Is(err, errOBSPreviewSession):
		return 401, "请先登录管理页面"
	case errors.Is(err, errOBSPreviewInactive):
		return 409, "本地预览已停止"
	case errors.Is(err, errOBSPreviewBusy):
		return 409, "本地预览已打开或上一帧仍在结束"
	case errors.Is(err, errOBSPreviewFast):
		return 429, "请等待一秒后再取下一帧"
	case errors.Is(err, pluginhost.ErrRevision):
		return 409, "请刷新 OBS 项目工具，确认已安装、配置并启用；修改配置前请先停用"
	case errors.Is(err, obspreview.ErrAuthentication):
		return 502, "OBS WebSocket 认证失败，请核对服务器已启用密码认证及保存的密码"
	case errors.Is(err, obspreview.ErrUnavailable):
		return 502, "无法连接本机 OBS WebSocket，请核对 OBS 是否运行、服务是否启用及端口"
	case errors.Is(err, obspreview.ErrProtocol):
		return 502, "本机端口未返回有效的 OBS WebSocket v5 响应"
	case errors.Is(err, obspreview.ErrRequestRejected):
		return 502, "OBS 无法读取当前节目场景，请检查场景和来源是否正常"
	case errors.Is(err, obspreview.ErrInvalidFrame):
		return 502, "OBS 返回的预览图无效"
	case errors.Is(err, context.DeadlineExceeded):
		return 504, "OBS 获取预览超时，请稍后重新开启预览"
	default:
		return 502, "OBS 本地预览失败，请核对 OBS 是否正在运行及 WebSocket 配置"
	}
}
