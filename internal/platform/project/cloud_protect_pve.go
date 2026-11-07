package project

// cloud_protect_pve.go = the "Protected" server add-on on a Proxmox provider. See pve/protect.go.

import (
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveProtectionAction handles GET_PROTECTION, ENABLE_PROTECTION and DISABLE_PROTECTION on a server.
func (h *Handler) pveProtectionAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string) bool {
	if action != "GET_PROTECTION" && action != "ENABLE_PROTECTION" && action != "DISABLE_PROTECTION" {
		return false
	}
	ctx := r.Context()
	ps, ok := h.pveService(w, es)
	if !ok {
		return true
	}
	var err error
	switch action {
	case "ENABLE_PROTECTION":
		err = ps.EnableProtection(ctx, proj.ID, cr.ExternalID, es.PVEStorageHeadroomPct(), es.PVEOvercommitCPU())
	case "DISABLE_PROTECTION":
		err = ps.DisableProtection(ctx, proj.ID, cr.ExternalID)
	}
	if err != nil {
		h.fail(w, pveVolumeErr(err))
		return true
	}
	if action != "GET_PROTECTION" {
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	}
	vm, verr := ps.ResolveVM(ctx, proj.ID, cr.ExternalID)
	if verr != nil || vm == nil {
		h.fail(w, httpx.NotFound("Server not found"))
		return true
	}
	out := map[string]any{"available": false}
	if ready, reason, err := ps.ProtectionReady(ctx); err == nil {
		out["available"], out["reason"] = ready, reason
	}
	state, err := ps.ReconcileProtection(ctx, *vm)
	if err != nil {
		h.fail(w, upstreamErr(computeBackend, err))
		return true
	}
	out["protection"] = state
	// The row carries the state for billing and lists; the sync keeps it fresh from here on.
	h.pveRefreshServerRow(ctx, ps, proj, cr)
	httpx.OK(w, map[string]any{"result": out})
	return true
}
