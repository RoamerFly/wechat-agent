package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	biliapp "wechat-bot/connected-apps/bilibili/adapter"
	"wechat-bot/internal/apphost"

	"wechat-bot/internal/websession"
)

func (h *localHomeService) bilibiliRoutes(s *websession.Store, mux *http.ServeMux, registry apphost.Registry) {
	entry, ok := registry.Lookup(biliapp.ID)
	bili, supported := entry.Provider.(*biliapp.PublicApp)
	if !ok || !supported {
		return
	}
	handler := s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		record, err := h.identity.Read()
		if err != nil || record.LocalOwner == nil {
			websession.Fail(w, http.StatusConflict, "请先准备本机主人身份")
			return
		}
		var view biliapp.Settings
		if r.Method == http.MethodGet {
			view, err = bili.View()
		} else {
			var in struct {
				Revision        *uint64  `json:"revision"`
				Enabled         *bool    `json:"enabled"`
				Videos          []string `json:"videos"`
				BrowserEnabled  bool     `json:"browser_enabled"`
				PlaybackEnabled bool     `json:"playback_enabled"`
				CommentsEnabled bool     `json:"comments_enabled"`
				AudioEnabled    bool     `json:"audio_enabled"`
			}
			if !websession.Decode(w, r, &in) {
				return
			}
			if in.Revision == nil || in.Enabled == nil || in.Videos == nil {
				websession.Fail(w, http.StatusBadRequest, "请提交读取开关、公开 BV 号列表和当前设置版本")
				return
			}
			if in.BrowserEnabled && !*in.Enabled {
				websession.Fail(w, http.StatusBadRequest, "启用独立浏览器前，请先开启公开读取并填写 BV 号")
				return
			}
			if in.PlaybackEnabled && !in.BrowserEnabled {
				websession.Fail(w, http.StatusBadRequest, "启用播放采样前，请先开启独立浏览器")
				return
			}
			if in.AudioEnabled && !*in.Enabled {
				websession.Fail(w, http.StatusBadRequest, "启用音轨读取前，请先开启公开读取并填写 BV 号")
				return
			}
			if in.CommentsEnabled && !*in.Enabled {
				websession.Fail(w, http.StatusBadRequest, "启用评论读取前，请先开启公开读取并填写 BV 号")
				return
			}
			view, err = bili.Save(*in.Revision, biliapp.Settings{
				Enabled: *in.Enabled, Videos: in.Videos, BrowserEnabled: in.BrowserEnabled,
				PlaybackEnabled: in.PlaybackEnabled, CommentsEnabled: in.CommentsEnabled, AudioEnabled: in.AudioEnabled,
			})
		}
		switch {
		case errors.Is(err, biliapp.ErrConflict):
			websession.Fail(w, http.StatusConflict, "B 站读取设置已变化，请重新读取后保存")
		case errors.Is(err, biliapp.ErrInvalid):
			websession.Fail(w, http.StatusBadRequest, "请提供有效的 BV 号，最多 100 条且不重复")
		case errors.Is(err, apphost.ErrObservationBrowserUnavailable):
			websession.Fail(w, http.StatusServiceUnavailable, apphost.ErrObservationBrowserUnavailable.Error())
		case err != nil:
			websession.Fail(w, http.StatusServiceUnavailable, "B 站读取设置暂不可用，已保留原配置")
		default:
			websession.Respond(w, http.StatusOK, struct {
				biliapp.Settings
				BrowserAvailable bool `json:"browser_available"`
				AudioAvailable   bool `json:"audio_available"`
			}{view, bili.BrowserAvailable(), bili.AudioAvailable(r.Context())})
		}
	})
	mux.HandleFunc("GET /api/local/apps/bilibili/public", handler)
	mux.HandleFunc("PUT /api/local/apps/bilibili/public", handler)
	authorization := s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		record, err := h.identity.Read()
		if err != nil || record.LocalOwner == nil {
			websession.Fail(w, http.StatusConflict, "请先准备本机主人身份")
			return
		}
		var view biliapp.PublicationAuthorizationView
		if r.Method == http.MethodGet {
			view, err = bili.PublicationAuthorization()
		} else {
			r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
			var input biliapp.PublicationAuthorizationInput
			if !websession.Decode(w, r, &input) {
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			view, err = bili.SavePublicationAuthorization(ctx, input)
		}
		switch {
		case errors.Is(err, biliapp.ErrPublicationAuthorizationConflict):
			websession.Fail(w, http.StatusConflict, "发布授权已变化，请刷新后重试")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationLegacy):
			websession.Fail(w, http.StatusConflict, "请先用现有凭据核验账号，再更换令牌")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationIdentity):
			websession.Fail(w, http.StatusConflict, "新授权与已绑定的 B 站应用或账号不符")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationVerify):
			websession.Fail(w, http.StatusBadGateway, "B 站账号核验失败，请检查令牌和 USER_INFO 授权")
		case errors.Is(err, biliapp.ErrInvalid):
			websession.Fail(w, http.StatusBadRequest, "请核对 B 站发布凭据及到期时间")
		case err != nil:
			websession.Fail(w, http.StatusServiceUnavailable, "B 站发布授权暂不可用，原配置已保留")
		default:
			websession.Respond(w, http.StatusOK, view)
		}
	})
	mux.HandleFunc("GET /api/local/apps/bilibili/publication-authorization", authorization)
	mux.HandleFunc("POST /api/local/apps/bilibili/publication-authorization", authorization)
	mux.HandleFunc("POST /api/local/apps/bilibili/publication-oauth", s.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		record, err := h.identity.Read()
		sessionID, _, authenticated := s.Lookup(r)
		if err != nil || record.LocalOwner == nil || !authenticated {
			websession.Fail(w, http.StatusConflict, "请先准备本机主人身份并登录")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		var input biliapp.PublicationOAuthInput
		if !websession.Decode(w, r, &input) {
			return
		}
		// The opaque key ties the pending attempt to this local owner and
		// authenticated management session without storing the session cookie.
		binding := sha256.Sum256([]byte(record.FishID + "\x00" + record.LocalOwner.ID + "\x00" + sessionID))
		ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		result, err := bili.PublicationOAuth(ctx, hex.EncodeToString(binding[:]), input)
		switch {
		case errors.Is(err, biliapp.ErrPublicationAuthorizationConflict):
			websession.Fail(w, http.StatusConflict, "发布授权已变化，请刷新后重新开始")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationLegacy):
			websession.Fail(w, http.StatusConflict, "请先用旧版原凭据核验账号，再进行网页授权")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationIdentity):
			websession.Fail(w, http.StatusConflict, "网页授权与已绑定的 B 站应用或账号不符")
		case errors.Is(err, biliapp.ErrPublicationOAuthCallback):
			websession.Fail(w, http.StatusBadRequest, "回跳地址与本次授权不匹配、已用过或已过期")
		case errors.Is(err, biliapp.ErrPublicationOAuthScope):
			websession.Fail(w, http.StatusForbidden, "请在 B 站确认已授权 USER_INFO 和 ARC_BASE")
		case errors.Is(err, biliapp.ErrPublicationOAuthRefreshUnavailable):
			websession.Fail(w, http.StatusConflict, "当前凭据无法续期，请重新网页授权")
		case errors.Is(err, biliapp.ErrPublicationOAuthRefresh):
			websession.Fail(w, http.StatusBadGateway, "续期结果未确认，请重新网页授权")
		case errors.Is(err, biliapp.ErrPublicationOAuthExchange):
			websession.Fail(w, http.StatusBadGateway, "B 站授权码兑换失败，请重新发起网页授权")
		case errors.Is(err, biliapp.ErrPublicationAuthorizationVerify):
			websession.Fail(w, http.StatusBadGateway, "B 站账号核验失败，请重新发起网页授权")
		case errors.Is(err, biliapp.ErrInvalid):
			websession.Fail(w, http.StatusBadRequest, "请核对授权操作、已登记回调地址及应用凭据")
		case err != nil:
			websession.Fail(w, http.StatusServiceUnavailable, "B 站网页授权暂不可用，请刷新后核对状态")
		default:
			websession.Respond(w, http.StatusOK, result)
		}
	}))
}
