package admin

// pveadmin.go — Proxmox provider admin surface: the connection test.
// POST /api/v1/admin/service/proxmox/auth (keystoneAuth twin): authenticate with
// creds from the body — or the stored service via ?externalServiceId, so the FE
// re-tests without resending the token secret — and return what the token can see
// (nodes, storages, SDN zones, PCI mappings) to populate the provider form and
// drive the GPU/SDN preflight badges.

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveAuthReq is the connection-test request body. All fields optional when
// ?externalServiceId resolves a stored provider.
type pveAuthReq struct {
	APIURL      string `json:"apiUrl"`
	TokenID     string `json:"tokenId"`
	TokenSecret string `json:"tokenSecret"`
	Fingerprint string `json:"fingerprint"`
	InsecureTLS bool   `json:"insecureTls"`
}

// PVEAuthResponse feeds the admin provider form.
type PVEAuthResponse struct {
	Version     string           `json:"version"`
	Nodes       []pve.Node       `json:"nodes"`
	Storages    []pve.StorageDef `json:"storages"`
	SDNZones    []pve.SDNZone    `json:"sdnZones"`
	PCIMappings []pve.PCIMapping `json:"pciMappings"`
}

func (h *Handler) proxmoxAuth(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	var req pveAuthReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	cfg, ok := h.pveConfig(r.Context(), req, r.URL.Query().Get("externalServiceId"))
	if !ok {
		httpx.WriteError(w, httpx.BadRequest("apiUrl, tokenId and tokenSecret are required"))
		return
	}
	cc, err := pve.New(cfg)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox connection failed: "+err.Error()))
		return
	}
	ctx := r.Context()
	ver, err := cc.Version(ctx)
	if err != nil {
		// connection test → 400, not 500 (keystoneAuth precedent)
		httpx.WriteError(w, httpx.BadRequest("Proxmox authentication failed: "+err.Error()))
		return
	}
	out := PVEAuthResponse{Nodes: []pve.Node{}, Storages: []pve.StorageDef{}, SDNZones: []pve.SDNZone{}, PCIMappings: []pve.PCIMapping{}}
	if v, ok := ver["version"].(string); ok {
		out.Version = v
	}
	// Best-effort enrichment: a token narrow enough to miss one of these should not
	// fail the whole test — the form renders what it can.
	if ns, err := cc.Nodes(ctx); err == nil {
		out.Nodes = ns
	}
	if ss, err := cc.ListStorage(ctx); err == nil {
		out.Storages = ss
	}
	if zs, err := cc.ListSDNZones(ctx); err == nil {
		out.SDNZones = zs
	}
	if ms, err := cc.ListPCIMappings(ctx); err == nil {
		out.PCIMappings = ms
	}
	httpx.OK(w, out)
}

// pveConfig builds the pve.Config from the request, or from the stored service
// when externalServiceId is given and the body carries no secret.
func (h *Handler) pveConfig(ctx context.Context, req pveAuthReq, externalServiceID string) (pve.Config, bool) {
	if externalServiceID != "" && req.TokenSecret == "" && h.esSvc != nil {
		if es, err := h.esSvc.Get(ctx, externalServiceID); err == nil && es != nil && es.IsProxmox() {
			return es.PVEConfig(), true
		}
	}
	if req.APIURL == "" || req.TokenID == "" || req.TokenSecret == "" {
		return pve.Config{}, false
	}
	return pve.Config{
		APIURL:      req.APIURL,
		TokenID:     req.TokenID,
		Secret:      req.TokenSecret,
		Fingerprint: req.Fingerprint,
		InsecureTLS: req.InsecureTLS,
	}, true
}
