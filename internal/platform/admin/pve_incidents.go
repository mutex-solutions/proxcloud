package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveincident"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// PVEIncidentRoutes is intentionally NOT mounted yet. The inventory adapter and
// startup Ensure must be explicitly wired after its verified snapshot contract lands.
// Mount under /service/{id}/incidents; nil reader fails closed. No mutation routes.
func (h *Handler) PVEIncidentRoutes(repo *pveincident.Repo) chi.Router {
	router := chi.NewRouter()
	serve := func(affected bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !h.require(w, r, "admin:service:read") || !h.require(w, r, "admin:cloud_resource:read") {
				return
			}
			if repo == nil {
				httpx.Err(w, 503, 503, "Incident observations unavailable")
				return
			}
			es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
			if !ok {
				return
			}
			if !es.IsProxmox() {
				httpx.WriteError(w, httpx.BadRequest("Not a Proxmox provider"))
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			q := r.URL.Query()
			limit := 50
			if v := q.Get("limit"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 || n > 100 {
					httpx.WriteError(w, httpx.BadRequest("Invalid incident limit"))
					return
				}
				limit = n
			}
			var data any
			var err error
			if affected {
				data, err = repo.Affected(ctx, es.ID, chi.URLParam(r, "incidentId"), q.Get("projectId"), q.Get("cursor"), limit)
			} else {
				data, err = repo.List(ctx, es.ID, pveincident.Filter{State: q.Get("state"), Kind: q.Get("kind"), Cursor: q.Get("cursor"), Limit: limit})
			}
			switch {
			case errors.Is(err, pveincident.ErrInvalid):
				httpx.WriteError(w, httpx.BadRequest("Invalid incident query"))
			case errors.Is(err, pveincident.ErrNotFound):
				httpx.WriteError(w, httpx.NotFound("Incident not found"))
			case err != nil:
				httpx.Err(w, 503, 503, "Incident observations unavailable")
			default:
				httpx.OK(w, data)
			}
		}
	}
	router.Get("/", serve(false))
	router.Get("/{incidentId}/affected", serve(true))
	return router
}
