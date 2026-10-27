package project

// publicnetworks.go — GET /project/{id}/public-networks: the admin-designated
// inbound bridges/vnets of the project's Proxmox provider (config.network.
// publicNetworks), filtered through the project's publicNetworkIds allow-list
// (nil = all allowed, empty = none; admin-managed via PUT /admin/project/{id}/
// public-networks). There are no floating IPs on this fork — a "public network"
// is what a server attaches its second NIC to (ATTACH_PUBLIC_NETWORK).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// publicNetworks handles GET /api/v1/project/{id}/public-networks.
func (h *Handler) publicNetworks(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, err := h.svc.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	out := []map[string]any{}
	svcID := h.resolveServiceID(r, proj)
	if es, eerr := h.esSvc.Get(r.Context(), svcID); eerr == nil && es != nil && es.IsProxmox() {
		for _, pn := range es.PVENetworkConfig().PublicNetworks {
			if publicNetworkAllowed(proj, pn.ID) {
				out = append(out, map[string]any{"id": pn.ID, "name": pn.Name})
			}
		}
	}
	httpx.List(w, out)
}

// publicNetworkAllowed reports whether a public network id is enabled for the project:
// nil PublicNetworkIds = all allowed (the default), empty slice = none.
func publicNetworkAllowed(proj *Project, netID string) bool {
	if proj.PublicNetworkIds == nil {
		return true
	}
	for _, id := range proj.PublicNetworkIds {
		if id == netID {
			return true
		}
	}
	return false
}
