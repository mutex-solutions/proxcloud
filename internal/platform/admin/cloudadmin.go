package admin

// cloudadmin.go — the cloud-admin live reads: answered from the Proxmox provider
// config + live PVE reads (templates, flavor catalog, storage profiles, GPUs,
// public networks, regions).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const cloudServiceCollection = "externalService"

// routeCloudAdmin registers the cloud-admin live reads.
func (h *Handler) routeCloudAdmin(r chi.Router) {
	r.Post("/service/proxmox/auth", h.proxmoxAuth)
	r.Get("/service/os-images", h.imagesAll)
	r.Get("/service/regions", h.serviceRegionsList)
	r.Get("/service/{id}/os-images", h.imagesByService)
	r.Get("/service/{id}/os-flavors", h.osFlavorsByService)
	// Kamaji chart-pin surface: list every managed cluster's pin; re-pin one or all onto the
	// provider's current chartVersion (the operator-side "platform update").
	r.Get("/service/{id}/k8s-clusters", h.kamajiClusterPins)
	r.Post("/service/{id}/k8s-clusters/bump-chart", h.kamajiClusterBumpAll)
	r.Post("/service/{id}/k8s-clusters/{clusterId}/bump-chart", h.kamajiClusterBump)
	// Dbaas chart-pin surface — same shape for managed databases.
	r.Get("/service/{id}/db-clusters", h.dbaasClusterPins)
	r.Post("/service/{id}/db-clusters/bump-chart", h.dbaasClusterBumpAll)
	r.Post("/service/{id}/db-clusters/{clusterId}/bump-chart", h.dbaasClusterBump)
	r.Get("/service/{id}/gpu-info", h.gpuInfo)
	r.Get("/service/{id}/unpriced-flavors", h.unpricedFlavors)
	r.Get("/service/{id}/volume/types", h.volumeTypes)
	r.Get("/cloud-resource/public-networks/{id}", h.publicNetworks)
	// listFlavors — the curated flavor catalog (Proxmox has no live flavors).
	r.Get("/flavor-categories/flavors", h.flavorsAll)
}

// ── helpers ────────────────────────────────────────────────────────────────

func (h *Handler) serviceRegions(es *externalservice.ExternalService) []string {
	rs := es.RegionNames()
	if len(rs) == 0 {
		if h.defaultRegion != "" {
			return []string{h.defaultRegion}
		}
		return []string{""}
	}
	return rs
}

// regionMeta reads config.regions[region] → (displayName, country); blanks when absent.
func regionMeta(es *externalservice.ExternalService, region string) (displayName, country string) {
	regions, ok := es.Config["regions"].(map[string]any)
	if !ok {
		return "", ""
	}
	m, ok := regions[region].(map[string]any)
	if !ok {
		return "", ""
	}
	return str(m["displayName"]), str(m["country"])
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// loadServiceOr writes the not-found (HTTP 500 "Service not found: %s") and returns false when
// the externalService is absent.
func (h *Handler) loadServiceOr(w http.ResponseWriter, r *http.Request, id string) (*externalservice.ExternalService, bool) {
	if h.esSvc == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return nil, false
	}
	es, err := h.esSvc.Get(r.Context(), id)
	if err != nil || es == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return nil, false
	}
	return es, true
}

// ── DTOs ───────────────────────────────────────────────────────────────────

// ImagesByLocationDto is the admin FE's images-by-location shape;
// images are the provider's OS templates rendered image-like.
type ImagesByLocationDto struct {
	ServiceID         string           `json:"serviceId"`
	ServiceName       string           `json:"serviceName"`
	Region            string           `json:"region"`
	RegionDisplayName string           `json:"regionDisplayName"`
	Images            []map[string]any `json:"images"`
}

// VolumeTypes is the volume-types response (storage-profile slugs on this fork).
type VolumeTypes struct {
	Region      string   `json:"region"`
	VolumeTypes []string `json:"volumeTypes"`
}

// Location is one region location entry.
type Location struct {
	Name        string `json:"name"`
	ServiceID   string `json:"serviceId"`
	ServiceName string `json:"serviceName"`
	Region      string `json:"region"`
	Country     string `json:"country,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// GPUModelCapacity is one GPU model's device counts.
type GPUModelCapacity struct {
	Name  string `json:"name"`
	Total int    `json:"total"`
	InUse int    `json:"inUse"`
}

// GPURegionCapacity is the gpu-info response entry.
type GPURegionCapacity struct {
	Region string             `json:"region"`
	Gpus   []GPUModelCapacity `json:"gpus"`
}

// ── handlers ───────────────────────────────────────────────────────────────

// pveTemplateImages renders a proxmox provider's template catalog image-like.
func pveTemplateImages(es *externalservice.ExternalService) []map[string]any {
	out := []map[string]any{}
	for _, t := range es.PVETemplates() {
		out = append(out, map[string]any{
			"id": t.Slug, "name": t.Name, "status": "active", "visibility": "public",
			"os_distro": t.OSDistro, "min_disk": t.MinDiskGb,
		})
	}
	return out
}

// imagesByService: the provider's OS templates per region. ADMIN_SERVICE_READ.
func (h *Handler) imagesByService(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	out := []ImagesByLocationDto{}
	if es.IsProxmox() {
		for _, region := range h.serviceRegions(es) {
			display, _ := regionMeta(es, region)
			out = append(out, ImagesByLocationDto{
				ServiceID: es.ID, ServiceName: es.Name, Region: region,
				RegionDisplayName: display, Images: pveTemplateImages(es),
			})
		}
	}
	httpx.List(w, out)
}

// imagesAll: templates across every provider. ADMIN_SERVICE_READ.
func (h *Handler) imagesAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	out := []ImagesByLocationDto{}
	if h.esSvc != nil {
		services, _ := h.esSvc.ListByType(r.Context(), externalservice.TypeCloud)
		for i := range services {
			es := &services[i]
			if es.IsDisabled() || !es.IsProxmox() {
				continue
			}
			for _, region := range h.serviceRegions(es) {
				display, _ := regionMeta(es, region)
				out = append(out, ImagesByLocationDto{
					ServiceID: es.ID, ServiceName: es.Name, Region: region,
					RegionDisplayName: display, Images: pveTemplateImages(es),
				})
			}
		}
	}
	httpx.List(w, out)
}

// osFlavorsByService: the curated flavor catalog in live-flavor shape. ADMIN_SERVICE_READ.
func (h *Handler) osFlavorsByService(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	out := []map[string]any{}
	if h.pveFlavors != nil && es.IsProxmox() {
		region := es.PVERegion()
		if fls, err := h.pveFlavors.List(r.Context(), false); err == nil {
			for i := range fls {
				out = append(out, fls[i].LiveShape(es.ID, region))
			}
		}
	}
	httpx.List(w, out)
}

// flavorsAll: the curated flavor catalog flattened (feeds the flavor-category form).
// ADMIN_FLAVOR_CATEGORY_MANAGE.
func (h *Handler) flavorsAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:flavor_category:manage") {
		return
	}
	out := []map[string]any{}
	if h.pveFlavors != nil {
		if fls, err := h.pveFlavors.List(r.Context(), false); err == nil {
			for i := range fls {
				name := fls[i].Name
				if name == "" {
					name = fls[i].Slug
				}
				out = append(out, map[string]any{
					"id": fls[i].Slug, "name": name, "vcpus": fls[i].VCPUs,
					"ram": fls[i].RAMMb, "disk": fls[i].DiskGb,
					"extraSpecs": fls[i].ExtraSpecs(),
				})
			}
		}
	}
	httpx.List(w, out)
}

// gpuInfo: per-model GPU capacity off the provider's PCI mappings vs tagged VMs.
// ADMIN_SERVICE_READ.
func (h *Handler) gpuInfo(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	out := []GPURegionCapacity{}
	if es.IsProxmox() && h.pveFor != nil {
		if ps, err := h.pveFor(es); err == nil {
			models := []GPUModelCapacity{}
			for alias, mappings := range es.PVEGPUAliases() {
				inv, ierr := ps.GPUInventoryFor(r.Context(), cloud.NormalizeGPUAlias(alias), mappings)
				if ierr != nil {
					continue
				}
				models = append(models, GPUModelCapacity{
					Name: inv.Alias, Total: len(inv.All), InUse: len(inv.All) - len(inv.Free),
				})
			}
			out = append(out, GPURegionCapacity{Region: es.PVERegion(), Gpus: models})
		}
	}
	httpx.List(w, out)
}

// volumeTypes: the provider's storage-profile slugs per region. ADMIN_SERVICE_READ.
func (h *Handler) volumeTypes(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	out := []VolumeTypes{}
	if es.IsProxmox() {
		names := []string{}
		for slug := range es.PVEStorageProfiles() {
			names = append(names, slug)
		}
		out = append(out, VolumeTypes{Region: es.PVERegion(), VolumeTypes: names})
	}
	httpx.List(w, out)
}

// serviceRegionsList: every provider's configured regions. ADMIN_SERVICE_READ.
func (h *Handler) serviceRegionsList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	out := []Location{}
	if h.esSvc != nil {
		services, _ := h.esSvc.List(r.Context())
		for i := range services {
			es := &services[i]
			for _, region := range es.RegionNames() {
				display, country := regionMeta(es, region)
				out = append(out, Location{
					Name: region, ServiceID: es.ID, ServiceName: es.Name,
					Region: region, Country: country, DisplayName: display,
				})
			}
		}
	}
	httpx.List(w, out)
}

// publicNetworks: the provider's admin-designated public networks (the per-project
// allow-list picker source). ADMIN_CLOUD_RESOURCE_READ.
func (h *Handler) publicNetworks(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:cloud_resource:read") {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	out := []map[string]any{}
	if es.IsProxmox() {
		for _, pn := range es.PVENetworkConfig().PublicNetworks {
			out = append(out, map[string]any{"id": pn.ID, "name": pn.Name})
		}
	}
	httpx.List(w, out)
}
