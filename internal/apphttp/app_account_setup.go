package apphttp

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/websession"
)

var appSetupRequestID = regexp.MustCompile("^[a-f0-9]{32}$")

func registerAccountSetup(mux *http.ServeMux, auth *websession.Store, registry apphost.Registry) {
	route := func(pattern string, run func(http.ResponseWriter, *http.Request, apphost.AccountSetup)) {
		mux.HandleFunc(pattern, auth.Protect(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			app, ok := registry.Lookup(r.PathValue("app"))
			setup, supported := app.Provider.(apphost.AccountSetup)
			if !ok || !supported {
				websession.Fail(w, http.StatusNotFound, "应用未提供账号接入")
				return
			}
			run(w, r, setup)
		}))
	}
	route("POST /api/apps/{app}/connections", func(w http.ResponseWriter, r *http.Request, setup apphost.AccountSetup) {
		key := readAppSetupRequest(w, r)
		if key == "" {
			return
		}
		connection, err := setup.ProvisionConnection(r.Context(), key)
		appSetupResponse(w, connection, err)
	})
	route("POST /api/apps/{app}/connections/{connection}/login", func(w http.ResponseWriter, r *http.Request, setup apphost.AccountSetup) {
		key := readAppSetupRequest(w, r)
		if key == "" {
			return
		}
		op, err := setup.StartConnectionLogin(r.Context(), r.PathValue("connection"), key)
		appSetupResponse(w, op, err)
	})
	route("GET /api/apps/{app}/connections/{connection}/login/{operation}", func(w http.ResponseWriter, r *http.Request, setup apphost.AccountSetup) {
		op, err := setup.ReadConnectionLogin(r.Context(), r.PathValue("connection"), r.PathValue("operation"))
		appSetupResponse(w, op, err)
	})
	route("GET /api/apps/{app}/connections/{connection}/login/{operation}/qr", func(w http.ResponseWriter, r *http.Request, setup apphost.AccountSetup) {
		generation, err := strconv.Atoi(r.URL.Query().Get("generation"))
		if err != nil || generation < 0 || generation > 100 {
			websession.Fail(w, http.StatusBadRequest, "二维码代次无效")
			return
		}
		data, err := setup.ReadConnectionQR(r.Context(), r.PathValue("connection"), r.PathValue("operation"), generation)
		if err != nil {
			appSetupResponse(w, nil, err)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	})
}

func readAppSetupRequest(w http.ResponseWriter, r *http.Request) string {
	var in struct {
		RequestID string `json:"request_id"`
	}
	if !websession.Decode(w, r, &in) {
		return ""
	}
	if !appSetupRequestID.MatchString(in.RequestID) {
		websession.Fail(w, http.StatusBadRequest, "账号操作缺少有效请求编号")
		return ""
	}
	return in.RequestID
}

func appSetupResponse(w http.ResponseWriter, value any, err error) {
	switch {
	case errors.Is(err, apphost.ErrNotFound):
		websession.Fail(w, http.StatusNotFound, "未找到对应的账号、登录操作或二维码")
	case errors.Is(err, apphost.ErrLifecycleConflict):
		websession.Fail(w, http.StatusConflict, "此账号已登录、正在恢复，或已有扫码请求")
	case err != nil:
		websession.Fail(w, http.StatusServiceUnavailable, "暂时无法核对账号操作，请稍后重试当前请求")
	default:
		websession.Respond(w, http.StatusOK, value)
	}
}
