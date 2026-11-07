package station

import (
	"net/http"

	"wechat-bot/internal/stationhost"
	"wechat-bot/internal/stationservices"
	"wechat-bot/internal/websession"
)

func (s *stationWechat) attachment(identity stationservices.OwnerReader) stationhost.Attachment {
	return stationhost.Attachment{
		Mount: func(owner *websession.Store, mux *http.ServeMux) {
			s.contactRoutes(owner, mux)
			s.groupActionRoutes(owner, mux, identity)
			s.recallRoutes(owner, mux, identity)
			s.legacyImportRoutes(owner, mux)
			s.attachmentRoutes(owner, mux)
			s.shareRoutes(owner, mux)
			s.artifactRoutes(owner, mux)
			s.deliveryControlRoutes(owner, mux)
		},
		Restore:   s.restore,
		Lifecycle: s,
	}
}
