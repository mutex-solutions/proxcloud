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
//
// Storage capabilities: POST /service/{id}/storage/detect re-detects and persists
// config.capabilities; PUT /service/{id}/storage-classes sets the per-storage class overrides;
// a storageProfiles save is validated against a fresh detection (validatePVEStorageProfiles).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveAuthReq is the connection-test request body. All fields optional when
// ?externalServiceId resolves a stored provider.
type pveAuthReq struct {
	APIURL      string `json:"apiUrl"`
	CACert      string `json:"caCert"`
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
// iptables firewall: it ignores FORWARD rules, so tenant network isolation
// would be silently off on that node.
func checkNodeFirewalls(ctx context.Context, cc *pve.Client) *httpx.HTTPError {
	legacy, err := cc.NodesWithoutNFTables(ctx)
	if err != nil {
		return httpx.BadRequest("Proxmox preflight failed: cannot read the node firewall options: " + err.Error())
	}
	if len(legacy) > 0 {
		return httpx.BadRequest("Proxmox preflight failed: node " + strings.Join(legacy, ", ") + " runs the legacy iptables firewall, which ignores the FORWARD rules that isolate tenant networks. Set nftables: 1 under Node > Firewall > Options on every node, then restart its guests.")
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
		CACert:      req.CACert,
		TokenID:     req.TokenID,
		Secret:      req.TokenSecret,
		Fingerprint: req.Fingerprint,
		InsecureTLS: req.InsecureTLS,
	}, true
}

// proxmoxMigrateServer moves a VM to another node: POST /service/{id}/server/{vmid}/migrate {target}. Operator only,
// for maintenance. Waits for the migration; a large disk without a replica takes as long as its copy.
func (h *Handler) proxmoxMigrateServer(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if !es.IsProxmox() || h.pveFor == nil {
		httpx.WriteError(w, httpx.BadRequest("Migration needs a Proxmox VE provider"))
		return
	}
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	var req struct {
		Target string `json:"target"`
	}
	if err != nil || json.NewDecoder(r.Body).Decode(&req) != nil || req.Target == "" {
		httpx.WriteError(w, httpx.BadRequest("A numeric vmid and a target node are required"))
		return
	}
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox connection failed: "+err.Error()))
		return
	}
	if err := ps.MigrateServer(context.WithoutCancel(r.Context()), vmid, req.Target); err != nil {
		var ce *pve.ConflictError
		if errors.As(err, &ce) {
			httpx.WriteError(w, httpx.NewError(http.StatusConflict, http.StatusConflict, ce.Msg))
			return
		}
		httpx.WriteError(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error()))
		return
	}
	httpx.OK(w, map[string]any{"target": req.Target})
}

// proxmoxDetectStorage is the admin "Re-detect capabilities" action: detect every cluster
// storage now and persist config.capabilities.
func (h *Handler) proxmoxDetectStorage(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if !es.IsProxmox() || h.pveFor == nil {
		httpx.WriteError(w, httpx.BadRequest("Storage detection needs a Proxmox VE provider"))
		return
	}
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox connection failed: "+err.Error()))
		return
	}
	caps, err := h.esSvc.DetectPVECapabilities(r.Context(), es, ps.C)
	if err != nil {
		httpx.WriteError(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, "Storage detection failed: "+err.Error()))
		return
	}
	httpx.OK(w, caps)
}

// proxmoxStorageClasses handles PUT /service/{id}/storage-classes: body {"<storage>": class}
// sets config.storageClasses.<storage>.class (the admin override, wins over the detected
// class); "" clears it. The installer's suggestedClass is kept as-is.
func (h *Handler) proxmoxStorageClasses(w http.ResponseWriter, r *http.Request) {
	h.serviceFieldSet(w, r, applyStorageClasses)
}

func applyStorageClasses(req *http.Request, doc pgdoc.M) *httpx.HTTPError {
	var body map[string]string
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return httpx.BadRequest("Invalid request body")
	}
	classes := ensureMap(ensureConfig(doc), "storageClasses")
	for id, class := range body {
		if class != "" && !pve.ValidClass(class) {
			return httpx.BadRequest(fmt.Sprintf("class for %q must be one of: performance, standard, capacity", id))
		}
		entry := ensureMap(classes, id)
		if class == "" {
			delete(entry, "class")
		} else {
			entry["class"] = class
		}
	}
	return nil
}

// validatePVEStorageProfiles checks a storageProfiles save against a fresh storage detection
// of the stored provider and, when it passes, refreshes config.capabilities with it. No pve
// factory wired (unit tests) = no live cluster to check against → skipped.
func (h *Handler) validatePVEStorageProfiles(req *http.Request, cfg pgdoc.M, raw any) *httpx.HTTPError {
	if h.pveFor == nil || h.esSvc == nil {
		return nil
	}
	es, err := h.esSvc.Get(req.Context(), chi.URLParam(req, "id"))
	if err != nil {
		return httpx.BadRequest("Cannot validate storage profiles: " + err.Error())
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return httpx.BadRequest("Cannot validate storage profiles: " + err.Error())
	}
	caps, err := ps.C.DetectStorage(req.Context())
	if err != nil {
		return httpx.BadRequest("Cannot validate storage profiles: storage detection failed: " + err.Error())
	}
	if err := externalservice.ValidatePVEStorageProfiles(raw, caps, cfg["storageClasses"]); err != nil {
		return httpx.BadRequest(err.Error())
	}
	cfg["capabilities"] = externalservice.PVECapabilities{DetectedAt: time.Now().UTC(), Storages: caps}
	return nil
}
