package admin

// cloudresourcemut.go — STUB. The admin per-resource live sync/delete ran through
// the OpenStack tenant client; on this fork resources reconcile through the
// project sync (POST /admin/project/{id}/sync → syncjob) and delete through the
// client surface. 501 keeps the admin FE honest.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) routeCloudResourceMut(r chi.Router) {
	r.Post("/cloud-resource/{id}/sync", h.cloudResourceSync)
	r.Delete("/cloud-resource/{id}", h.cloudResourceDelete)
}

func (h *Handler) cloudResourceSync(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:manage") {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented,
		"Per-resource sync is not available — run the project sync instead")
}

func (h *Handler) cloudResourceDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:manage") {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented,
		"Admin resource delete is not available — delete through the client surface")
}
