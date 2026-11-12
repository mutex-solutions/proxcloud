package project

// gpucapacity.go — GET /project/{id}/gpu-capacity: per-model GPU capacity of the
// project's Proxmox provider (cluster PCI mappings vs the platform-tagged GPU
// VMs), operator-gated per project (GpuCapacityVisible). Short cache bounds the
// PVE round-trips; cloud failures degrade to a warnings-partial 200.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const gpuCapacityTTL = 30 * time.Second

type gpuCapacityEntry struct {
	Model     string `json:"model"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

type gpuCapacityResponse struct {
	Visible  bool               `json:"visible"`
	Region   string             `json:"region,omitempty"`
	Capacity []gpuCapacityEntry `json:"capacity"`
	Warnings []string           `json:"warnings"`
}

type gpuCapacityCached struct {
	at   time.Time
	caps []gpuCapacityEntry
}

var gpuCapacityCache sync.Map // serviceID → gpuCapacityCached

// projectGPUCapacity handles GET /project/{id}/gpu-capacity.
func (h *Handler) projectGPUCapacity(w http.ResponseWriter, r *http.Request) {
	_, proj, ok := h.resolveForMember(w, r)
	if !ok {
		return
	}
	resp := gpuCapacityResponse{Visible: proj.GpuCapacityVisible, Capacity: []gpuCapacityEntry{}, Warnings: []string{}}
	if !proj.GpuCapacityVisible {
		httpx.OK(w, resp)
		return
	}
	// An explicit service the project is not attached to is refused (#215), not answered with
	// another provider's capacity.
	serviceID, err := h.resolveServiceID(r, proj)
	if err != nil {
		h.fail(w, err)
		return
	}
	if serviceID == "" || h.esSvc == nil {
		resp.Warnings = append(resp.Warnings, "GPU capacity is unavailable for this project.")
		httpx.OK(w, resp)
		return
	}
	es, err := h.esSvc.Get(r.Context(), serviceID)
	if err != nil || es == nil || !es.IsProxmox() {
		resp.Warnings = append(resp.Warnings, "GPU capacity is unavailable because the cloud service is not ready.")
		httpx.OK(w, resp)
		return
	}
	resp.Region = h.pveRegion(proj, es)
	caps, err := h.gpuCapacityForService(r.Context(), es)
	if err != nil {
		resp.Warnings = append(resp.Warnings, "GPU capacity is currently unavailable.")
		httpx.OK(w, resp)
		return
	}
	resp.Capacity = caps
	httpx.OK(w, resp)
}

// gpuCapacityForService sums each configured GPU mapping's devices vs the tagged
// VMs' usage across the cluster, cached briefly.
func (h *Handler) gpuCapacityForService(ctx context.Context, es *externalservice.ExternalService) ([]gpuCapacityEntry, error) {
	key := es.ID
	if v, ok := gpuCapacityCache.Load(key); ok {
		if entry := v.(gpuCapacityCached); time.Since(entry.at) < gpuCapacityTTL {
			return entry.caps, nil
		}
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return nil, err
	}
	aliases := es.PVEGPUAliases()
	caps := make([]gpuCapacityEntry, 0, len(aliases))
	for alias, mappings := range aliases {
		inv, err := ps.GPUInventoryFor(ctx, cloud.NormalizeGPUAlias(alias), mappings)
		if err != nil {
			continue // one bad alias must not hide the others
		}
		caps = append(caps, gpuCapacityEntry{
			Model: inv.Alias, Available: len(inv.Free), Total: len(inv.All),
		})
	}
	gpuCapacityCache.Store(key, gpuCapacityCached{at: time.Now(), caps: caps})
	return caps, nil
}
