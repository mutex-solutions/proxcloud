package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) SetSeedArtifacts(repo *pve.SeedArtifactRepo) { h.seedArtifacts = repo }

// seedCleanupStatus exposes durable, payload-free cleanup state to an operator,
// not a tenant. It never fetches a CD, prints a config, or returns credentials.
func (h *Handler) seedCleanupStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
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
	if h.seedArtifacts == nil {
		httpx.WriteError(w, httpx.NewError(503, 503, "Seed artifact journal is unavailable"))
		return
	}
	report, err := h.seedArtifacts.Report(r.Context(), es.ID)
	if err != nil {
		httpx.WriteError(w, httpx.NewError(503, 503, "Seed cleanup status is unavailable"))
		return
	}
	cfg := es.PVEConfig()
	report["inventoryReaderConfigured"] = cfg.SeedInventoryTokenID != "" && cfg.SeedInventorySecret != ""
	httpx.OK(w, report)
}
