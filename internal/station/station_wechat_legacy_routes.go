package station

import (
	"encoding/hex"
	"errors"
	"net/http"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/websession"
)

func (s *stationWechat) legacyImportRoutes(server *websession.Store, mux *http.ServeMux) {
	s.legacyMemoryRoute(server, mux)
	const path = "/api/apps/wechat/connections/{connection}/legacy-import"
	s.legacyGroupSettingRoutes(server, mux, path+"/memory/{thread}/preference", false)
	s.legacyGroupSettingRoutes(server, mux, path+"/groups/{thread}/rule", true)
	s.legacyReceiptRoutes(server, mux, path+"/receipts")
	s.legacyActivationRoutes(server, mux, path+"/activation")
	s.legacyRollbackRoutes(server, mux, path+"/rollback")
	s.legacyRollbackAuditRoutes(server, mux, path+"/rollback/audit")
	mux.HandleFunc("GET "+path, server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("connection")
		prior, found, err := s.savedLegacyImport(r.Context(), id, true)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		if found {
			websession.Respond(w, http.StatusOK, prior)
			return
		}
		target, err := s.worker(id, false)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		if target != nil {
			legacyImportFailure(w, errLegacyImportConflict)
			return
		}
		archive, err := s.previewLegacyImport(r.Context(), id)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		websession.Respond(w, http.StatusOK, archive.Plan)
	}))
	mux.HandleFunc("POST "+path, server.Protect(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var input struct {
			Digest string `json:"digest"`
		}
		if !websession.Decode(w, r, &input) {
			return
		}
		digest, err := hex.DecodeString(input.Digest)
		if err != nil || len(digest) != 32 {
			websession.Fail(w, http.StatusBadRequest, "请提交旧版预演的摘要")
			return
		}
		result, err := s.applyLegacyImport(r.Context(), r.PathValue("connection"), input.Digest)
		if err != nil {
			legacyImportFailure(w, err)
			return
		}
		websession.Respond(w, http.StatusOK, result)
	}))
}

func legacyImportFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, apphost.ErrNotFound):
		websession.Fail(w, http.StatusNotFound, "找不到匹配的在册账号")
	case errors.Is(err, errLegacyImportNotConfigured):
		websession.Fail(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errLegacySnapshotIntegrity):
		websession.Fail(w, http.StatusUnprocessableEntity, errLegacySnapshotIntegrity.Error())
	case errors.Is(err, errLegacyImportInvalid):
		websession.Fail(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, errLegacyImportChanged), errors.Is(err, errLegacyImportConflict):
		websession.Fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, apphost.ErrLifecycleConflict):
		websession.Fail(w, http.StatusConflict, "成长站正在停止，稍后可重新预演")
	default:
		websession.Fail(w, http.StatusServiceUnavailable, "暂时无法核对或保存旧版接入记录")
	}
}
