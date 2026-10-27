package admin

// projectimport.go — STUB. Importing pre-existing keystone tenants was an
// OpenStack surface; a Proxmox project bootstraps its own pool. 501 keeps the
// admin FE honest.

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) routeProjectImport(r chi.Router) {
	r.Get("/service/{id}/project-imports", h.projectImportFetch)
	r.Post("/service/{id}/project-imports", h.projectImportBulk)
}

func (h *Handler) projectImportFetch(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:read") {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "Project import is not available on this fork")
}

func (h *Handler) projectImportBulk(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:project:manage") {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented, "Project import is not available on this fork")
}
