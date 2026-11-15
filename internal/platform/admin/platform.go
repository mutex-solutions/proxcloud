package admin

// platform.go — the managed-services platform (docs/adr/0009-managed-services-platform-lifecycle.md):
//
//	GET /admin/platform/bom   the platform bill of materials this release embeds, with its sha256 (admin:service:read)

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) routePlatform(r chi.Router) {
	r.Get("/platform/bom", h.platformBOM)
}

func (h *Handler) platformBOM(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, serviceReadPerm) {
		return
	}
	bom, err := platformlife.Current()
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, bom)
}
