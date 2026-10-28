package admin

// pveadmin.go — Proxmox provider admin surface: the connection test.
// POST /api/v1/admin/service/proxmox/auth: authenticate with
// creds from the body — or the stored service via ?externalServiceId, so the FE
// re-tests without resending the token secret — and return what the token can see
// (nodes, storages, SDN zones, PCI mappings) to populate the provider form and
// drive the GPU/SDN preflight badges. Fails while the datacenter firewall is off,
// a node runs the iptables (not nftables) firewall, or a stored provider's public
// network sits on a node's management bridge.
// Missing ProxCloudPlatform privileges come back as warnings, not failures.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
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
	// Warnings are non-fatal findings, chiefly privileges the token lacks
	// against the ProxCloudPlatform role (docs/proxmox-permissions.md).
	Warnings []string `json:"warnings"`
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
		// connection test → 400, not 500 (a bad credential is a client error)
		httpx.WriteError(w, httpx.BadRequest("Proxmox authentication failed: "+err.Error()))
		return
	}
	// Preflight: with the datacenter firewall off, security groups, VM anti-spoof
	// and the vnet tenant-isolation rules are all silently unenforced.
	fwOn, err := cc.DatacenterFirewallEnabled(ctx)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox preflight failed: cannot read the datacenter firewall options: "+err.Error()))
		return
	}
	if !fwOn {
		httpx.WriteError(w, httpx.BadRequest("Proxmox preflight failed: the datacenter firewall is disabled, so security groups and tenant network isolation are not enforced. Enable it under Datacenter > Firewall > Options."))
		return
	}
	if herr := checkNodeFirewalls(ctx, cc); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	// Preflight: a stored provider's public networks must not sit on a node's
	// management bridge.
	if id := r.URL.Query().Get("externalServiceId"); id != "" && h.esSvc != nil {
		if es, err := h.esSvc.Get(ctx, id); err == nil && es != nil && es.IsProxmox() {
			if herr := checkPublicNetworks(ctx, cc, es.PVENetworkConfig().PublicNetworks); herr != nil {
				httpx.WriteError(w, herr)
				return
			}
		}
	}
	out := PVEAuthResponse{Nodes: []pve.Node{}, Storages: []pve.StorageDef{}, SDNZones: []pve.SDNZone{}, PCIMappings: []pve.PCIMapping{}, Warnings: tokenWarnings(ctx, cc, cfg.TokenID)}
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

// tokenWarnings reports a root@pam token and every ProxCloudPlatform privilege
// the token lacks. Warnings only: an operator may grant less on purpose (no GPUs,
// no EVPN) and the test must still pass.
func tokenWarnings(ctx context.Context, cc *pve.Client, tokenID string) []string {
	out := []string{}
	if strings.HasPrefix(tokenID, "root@pam!") {
		out = append(out, "The token belongs to root@pam and has full rights on the cluster. Use a dedicated proxcloud@pve token with the ProxCloudPlatform role (see docs/proxmox-permissions.md).")
	}
	missing, err := cc.MissingPrivileges(ctx, pve.PlatformPrivileges)
	if err != nil {
		return append(out, "Could not read the token's permissions: "+err.Error())
	}
	for _, m := range missing {
		out = append(out, "Token lacks "+m+" (see docs/proxmox-permissions.md)")
	}
	return out
}

// checkNodeFirewalls rejects a cluster where an online node still runs the stock
// iptables firewall: it ignores the SDN vnet rules, so tenant network isolation
// would be silently off on that node.
func checkNodeFirewalls(ctx context.Context, cc *pve.Client) *httpx.HTTPError {
	legacy, err := cc.NodesWithoutNFTables(ctx)
	if err != nil {
		return httpx.BadRequest("Proxmox preflight failed: cannot read the node firewall options: " + err.Error())
	}
	if len(legacy) > 0 {
		return httpx.BadRequest("Proxmox preflight failed: node " + strings.Join(legacy, ", ") + " runs the legacy iptables firewall, which ignores the SDN vnet rules that isolate tenant networks. Set nftables: 1 under Node > Firewall > Options on every node, then restart its guests.")
	}
	return nil
}

// checkPublicNetworks rejects a public network on a bridge that carries a node's
// own IP or default gateway: a public NIC there gives tenant VMs L2 next to the
// hypervisor's management address.
func checkPublicNetworks(ctx context.Context, cc *pve.Client, pns []externalservice.PVEPublicNetwork) *httpx.HTTPError {
	if len(pns) == 0 {
		return nil
	}
	mgmt, err := cc.ManagementBridges(ctx)
	if err != nil {
		return httpx.BadRequest("Proxmox preflight failed: cannot read the node networks: " + err.Error())
	}
	for _, pn := range pns {
		if node, ok := mgmt[pn.ID]; ok {
			return httpx.BadRequest(fmt.Sprintf("Proxmox preflight failed: public network %q is the management bridge on node %s. Use a dedicated bridge for public NICs.", pn.ID, node))
		}
	}
	return nil
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
