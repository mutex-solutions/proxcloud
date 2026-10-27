package admin

// pveflavors.go — admin CRUD for the Proxmox flavor catalog (pgdoc `flavor`).
// GET/POST /admin/flavors, PUT/DELETE /admin/flavors/{slug}. The client wizard
// consumes the catalog through the LIST_FLAVORS bulk action; this is the operator
// side. Gate: admin:service:manage (flavors are provider-shaping config).

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SetPVEFlavors wires the flavor catalog repo (nil = the surface 503s).
func (h *Handler) SetPVEFlavors(r *pveflavor.Repo) { h.pveFlavors = r }

func (h *Handler) routePVEFlavors(r chi.Router) {
	r.Get("/flavors", h.pveFlavorList)
	r.Post("/flavors", h.pveFlavorUpsert)
	r.Put("/flavors/{slug}", h.pveFlavorUpsert)
	r.Delete("/flavors/{slug}", h.pveFlavorDelete)
}

func (h *Handler) pveFlavorsReady(w http.ResponseWriter) bool {
	if h.pveFlavors == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "flavor catalog not configured")
		return false
	}
	return true
}

func (h *Handler) pveFlavorList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") || !h.pveFlavorsReady(w) {
		return
	}
	fls, err := h.pveFlavors.List(r.Context(), false)
	if httpx.WriteError(w, err) {
		return
	}
	out := make([]any, len(fls))
	for i := range fls {
		out[i] = fls[i]
	}
	httpx.List(w, out)
}

func (h *Handler) pveFlavorUpsert(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:manage") || !h.pveFlavorsReady(w) {
		return
	}
	var f pveflavor.Flavor
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	// PUT names the slug in the path; a mismatched body slug is a client bug.
	if slug := chi.URLParam(r, "slug"); slug != "" {
		if f.Slug == "" {
			f.Slug = slug
		} else if f.Slug != slug {
			httpx.WriteError(w, httpx.BadRequest("slug in body does not match the path"))
			return
		}
	}
	if err := h.pveFlavors.Save(r.Context(), &f); err != nil {
		httpx.WriteError(w, httpx.BadRequest(err.Error()))
		return
	}
	httpx.OK(w, f)
}

func (h *Handler) pveFlavorDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:manage") || !h.pveFlavorsReady(w) {
		return
	}
	if httpx.WriteError(w, h.pveFlavors.Delete(r.Context(), chi.URLParam(r, "slug"))) {
		return
	}
	httpx.Accepted(w)
}
