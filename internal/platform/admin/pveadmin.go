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
// ?checks=all answers 200 with one row per check instead of stopping at the first failure.
//
// Storage capabilities: POST /service/{id}/storage/detect re-detects and persists
// config.capabilities; PUT /service/{id}/storage-classes sets the per-storage class overrides;
// a storageProfiles save is validated against a fresh detection (validatePVEStorageProfiles).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveAuthReq is the connection-test request body. All fields optional when
// ?externalServiceId resolves a stored provider. A field the body leaves out is nil, so that with
// a stored provider and a blank secret only the fields it does carry replace the stored ones.
type pveAuthReq struct {
	APIURL      *string  `json:"apiUrl"`
	APIURLs     []string `json:"apiUrls"`
	CACert      *string  `json:"caCert"`
	TokenID     *string  `json:"tokenId"`
	TokenSecret string   `json:"tokenSecret"`
	Fingerprint *string  `json:"fingerprint"`
	InsecureTLS *bool    `json:"insecureTls"`
}

// PVECheck is one row of the connection test, returned with ?checks=all. The ids are part of the API (docs/api.md).
type PVECheck struct {
	ID      string `json:"id"`
	Status  string `json:"status"` // checkOK | checkWarn | checkFail
	Message string `json:"message"`
}

// checkWarn works but wants the operator's attention; checkFail does not work.
const (
	checkOK   = "ok"
	checkWarn = "warn"
	checkFail = "fail"
)

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
	// Checks is every check that ran, one row each; only with ?checks=all.
	Checks []PVECheck `json:"checks,omitempty"`
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
	ctx := r.Context()
	var es *externalservice.ExternalService
	if id := r.URL.Query().Get("externalServiceId"); id != "" && h.esSvc != nil {
		if got, err := h.esSvc.Get(ctx, id); err == nil && got != nil && got.IsProxmox() {
			es = got
		}
	}
	_, granted, _ := h.adminContext(r)
	cfg, ok := pveAuthConfig(req, es, rbac.Matches(granted, externalServiceManagePerm))
	if !ok {
		httpx.WriteError(w, httpx.BadRequest("apiUrl, tokenId and tokenSecret are required"))
		return
	}
	out, herr := pveConnectionTest(ctx, cfg, es, r.URL.Query().Get("checks") == "all")
	if herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	httpx.OK(w, out)
}

// pveConnectionTest runs the connection test against cfg; es is the stored provider when the caller named one. It
// answers the first failed check as a 400, as it always has: installer stage 60 relies on that. With all it runs every
// check it still can and reports each one in Checks, so a cluster with its firewall off still gets its privilege gaps
// listed. Only a cluster that cannot be reached, or refuses the token, ends the list early.
func pveConnectionTest(ctx context.Context, cfg pve.Config, es *externalservice.ExternalService, all bool) (PVEAuthResponse, *httpx.HTTPError) {
	out := PVEAuthResponse{Nodes: []pve.Node{}, Storages: []pve.StorageDef{}, SDNZones: []pve.SDNZone{}, PCIMappings: []pve.PCIMapping{}, Warnings: []string{}}
	add := func(id, status, msg string) {
		if all {
			out.Checks = append(out.Checks, PVECheck{ID: id, Status: status, Message: msg})
		}
	}
	// check records one check that fails the test: herr is its failure, nil when it passed. What it returns ends the test.
	check := func(id string, herr *httpx.HTTPError, passed string) *httpx.HTTPError {
		if herr == nil {
			add(id, checkOK, passed)
			return nil
		}
		add(id, checkFail, herr.Msg)
		if all {
			return nil
		}
		return herr
	}
	// warn records a check that only ever warns, with the warnings it found.
	warn := func(id, passed string, found []string) {
		out.Warnings = append(out.Warnings, found...)
		if len(found) == 0 {
			add(id, checkOK, passed)
		} else {
			add(id, checkWarn, strings.Join(found, "\n"))
		}
	}
	cc, err := pve.New(cfg)
	if err != nil {
		return out, check("connect", httpx.BadRequest("Proxmox connection failed: "+err.Error()), "")
	}
	ver, err := cc.Version(ctx)
	if err != nil {
		// connection test → 400, not 500 (a bad credential is a client error)
		id := "connect"
		var ae *pve.APIError
		if errors.As(err, &ae) { // PVE answered: the address and its certificate are fine, the token is not
			add("connect", checkOK, "Proxmox answers at this address.")
			id = "token"
		}
		return out, check(id, httpx.BadRequest("Proxmox authentication failed: "+err.Error()), "")
	}
	out.Version, _ = ver["version"].(string)
	add("connect", checkOK, "Reached Proxmox VE "+out.Version+".")
	// Preflight: with the datacenter firewall off, security groups, VM anti-spoof
	// and the vnet tenant-isolation rules are all silently unenforced.
	if herr := check("firewall-datacenter", checkDatacenterFirewall(ctx, cc), "The datacenter firewall is on."); herr != nil {
		return out, herr
	}
	if herr := check("firewall-nodes", checkNodeFirewalls(ctx, cc), "Every online node runs the nftables firewall."); herr != nil {
		return out, herr
	}
	// Preflight: a stored provider's public networks must not sit on a node's
	// management bridge.
	if es != nil {
		if herr := check("public-networks", checkPublicNetworks(ctx, cc, es.PVENetworkConfig().PublicNetworks), "No public network is on a management bridge."); herr != nil {
			return out, herr
		}
	}
	for _, c := range tokenChecks(ctx, cc, cfg.TokenID) {
		add(c.ID, c.Status, c.Message)
		if c.Status != checkOK {
			out.Warnings = append(out.Warnings, c.Message)
		}
	}
	if es != nil {
		if es.PVEImportStorage() != "" {
			warn("import-store", "The token can delete files on the import store.", importStoreWarnings(ctx, cc, es.PVEImportStorage()))
		}
		warn("store-reach", "Tenant networks cannot reach the network stores.", storeReachWarnings(ctx, cc, es.PVENetworkConfig().BlockedCIDRs))
		warn("boot-iso", "The Kubernetes bootstrap ISO store is node-local and takes ISOs.", bootISOWarnings(ctx, cc, es.PVEBootISOStorage()))
		out.Warnings = append(out.Warnings, "Ordinary server seed CDs retain plaintext user-data. Check GET /admin/service/"+es.ID+"/seed-cleanup for pending cleanup; a separate complete global read-only inventory credential is required (docs/server-seed-safety.md).")
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
	return out, nil
}

// checkDatacenterFirewall rejects a cluster whose datacenter firewall is off or cannot be read.
func checkDatacenterFirewall(ctx context.Context, cc *pve.Client) *httpx.HTTPError {
	fwOn, err := cc.DatacenterFirewallEnabled(ctx)
	if err != nil {
		return httpx.BadRequest("Proxmox preflight failed: cannot read the datacenter firewall options: " + err.Error())
	}
	if !fwOn {
		return httpx.BadRequest("Proxmox preflight failed: the datacenter firewall is disabled, so security groups and tenant network isolation are not enforced. Enable it under Datacenter > Firewall > Options.")
	}
	return nil
}

// tokenWarnings is what tokenChecks found wrong, as the warnings the route has always returned.
func tokenWarnings(ctx context.Context, cc *pve.Client, tokenID string) []string {
	out := []string{}
	for _, c := range tokenChecks(ctx, cc, tokenID) {
		if c.Status != checkOK {
			out = append(out, c.Message)
		}
	}
	return out
}

// tokenChecks reports a root@pam token and, per ACL path, every ProxCloudPlatform privilege
// the token lacks. Warnings only: an operator may grant less on purpose (no GPUs,
// no EVPN) and the test must still pass.
func tokenChecks(ctx context.Context, cc *pve.Client, tokenID string) []PVECheck {
	out := []PVECheck{{ID: "token", Status: checkOK, Message: "Proxmox accepts the token " + tokenID + "."}}
	if strings.HasPrefix(tokenID, "root@pam!") {
		out[0] = PVECheck{ID: "token", Status: checkWarn, Message: "The token belongs to root@pam and has full rights on the cluster. Use a dedicated proxcloud@pve token with the ProxCloudPlatform role (see docs/proxmox-permissions.md)."}
	}
	for _, chk := range pve.PlatformPrivileges {
		missing, err := cc.MissingPrivileges(ctx, []pve.PrivCheck{chk})
		if err != nil {
			return append(out[:1], PVECheck{ID: "privileges", Status: checkWarn, Message: "Could not read the token's permissions: " + err.Error()})
		}
		c := PVECheck{ID: "privileges:" + chk.Path, Status: checkOK, Message: "The token holds what the portal needs on " + chk.Path + "."}
		if len(missing) > 0 {
			c.Status, c.Message = checkWarn, "Token lacks "+missing[0]+" (see docs/proxmox-permissions.md)"
		}
		out = append(out, c)
	}
	return out
}

// importStoreWarnings reports what image import needs on its store beyond the role: deleting the downloaded
// file takes Datastore.Allocate, which the role holds nowhere.
func importStoreWarnings(ctx context.Context, cc *pve.Client, store string) []string {
	missing, err := cc.MissingPrivileges(ctx, []pve.PrivCheck{{Path: "/storage/" + store, Privs: []string{"Datastore.Allocate", "Datastore.AllocateTemplate"}}})
	if err != nil {
		return []string{"Could not read the token's permissions on the import store: " + err.Error()}
	}
	out := []string{}
	for _, m := range missing {
		out = append(out, "Token lacks "+m+": image import needs it, or uploaded files stay on the store (see docs/proxmox-permissions.md)")
	}
	return out
}

// bootISOWarnings says when the provider's Kubernetes bootstrap ISO store is unfit. A create refuses it, but clusters made
// earlier stay on it without a word: their move only ever goes to this store (docs/managed-k8s.md, "Bootstrap ISOs").
func bootISOWarnings(ctx context.Context, cc *pve.Client, store string) []string {
	caps, err := cc.DetectStorage(ctx)
	if err != nil {
		return []string{"Could not read the storages to check the Kubernetes bootstrap ISO store: " + err.Error()}
	}
	if p := pve.BootISOProblem(caps, store, store); p != "" {
		return []string{"Kubernetes bootstrap ISO store (setting isoStorage): " + p + ". Cluster creates on this provider answer 409, and existing clusters keep their join tokens there."}
	}
	return nil
}

// storeReachWarnings names each network store outside the networks tenants are dropped from. A tenant network
// leaves through its exit node's NAT under the node's address, which an export to the nodes admits, and these stores
// hold seed CDs with user-data, templates and disks.
func storeReachWarnings(ctx context.Context, cc *pve.Client, blocked []string) []string {
	defs, err := cc.ListStorage(ctx)
	if err != nil {
		return []string{"Could not read the storages to check that tenants cannot reach them: " + err.Error()}
	}
	mgmt, _, err := cc.ManagementCIDRs(ctx)
	if err != nil {
		return []string{"Could not read the node networks to check that tenants cannot reach the storages: " + err.Error()}
	}
	return unblockedStores(defs, append(mgmt, blocked...), func(host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip4", host)
	})
}

func unblockedStores(defs []pve.StorageDef, blocked []string, lookup func(string) ([]net.IP, error)) []string {
	var nets []netip.Prefix
	for _, b := range blocked {
		if p, err := netip.ParsePrefix(b); err == nil {
			nets = append(nets, p.Masked())
		} else if a, err := netip.ParseAddr(b); err == nil {
			nets = append(nets, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	out := []string{}
	for _, d := range defs {
		if d.Disable == 1 {
			continue
		}
	hosts:
		for _, host := range storeHosts(d) {
			ips := []net.IP{net.ParseIP(host)}
			if ips[0] == nil {
				var err error
				if ips, err = lookup(host); err != nil || len(ips) == 0 {
					out = append(out, fmt.Sprintf("Could not resolve %s, a server of storage %s, to check that tenants cannot reach it", host, d.Storage))
					continue
				}
			}
			for _, ip := range ips {
				a, _ := netip.AddrFromSlice(ip)
				if !slices.ContainsFunc(nets, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) }) {
					out = append(out, fmt.Sprintf("Storage %s (%s at %s) is outside blockedCidrs and the nodes' management networks: a tenant network reaches it through the nodes' NAT, under a node's address, and only the store's own access control then keeps seed CDs and disks there from it. Add %s to blockedCidrs and export the store to the node addresses only (docs/cloud-integration.md).", d.Storage, d.Type, host, a.Unmap()))
					break hosts
				}
			}
		}
	}
	return out
}

// storeHosts lists the addresses a network store is reached at: the file server, an external Ceph's monitors
// (`10.0.0.1 10.0.0.2`, `a:6789;b`, `[v2:a:3300/0,v1:a:6789/0]`), the iSCSI portal.
// ponytail: rbd and cephfs without monhost use the nodes' own Ceph, whose monitors this does not look up.
func storeHosts(d pve.StorageDef) []string {
	var raw string
	switch d.Type {
	case "nfs", "cifs", "glusterfs":
		raw = d.Server
	case "rbd", "cephfs":
		raw = d.Monhost
	case "iscsi", "iscsidirect":
		raw = d.Portal
	}
	var out []string
	for _, f := range strings.FieldsFunc(raw, func(r rune) bool { return r == ' ' || r == ',' || r == ';' }) {
		f = strings.Trim(f, "[]")
		f = strings.TrimPrefix(strings.TrimPrefix(f, "v1:"), "v2:")
		f, _, _ = strings.Cut(f, "/")
		if h, _, err := net.SplitHostPort(f); err == nil {
			f = h
		}
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
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

// pveAuthConfig builds the pve.Config to test: the body's, or the stored provider's when the body has no secret. With
// overlay the fields the body does carry replace the stored ones, so an edited form is tested with the stored secret
// before it is saved. overlay is for callers who may change the provider: sending the stored secret to an address of
// the caller's choosing hands it over, so a caller who can only read gets the stored config as it is.
func pveAuthConfig(req pveAuthReq, es *externalservice.ExternalService, overlay bool) (pve.Config, bool) {
	var cfg pve.Config
	stored := es != nil && req.TokenSecret == ""
	if stored {
		if cfg = es.PVEConfig(); !overlay {
			return cfg, true
		}
	} else {
		cfg.Secret = req.TokenSecret
	}
	cfg.APIURL = valueOr(req.APIURL, cfg.APIURL)
	cfg.CACert = valueOr(req.CACert, cfg.CACert)
	cfg.TokenID = valueOr(req.TokenID, cfg.TokenID)
	cfg.Fingerprint = valueOr(req.Fingerprint, cfg.Fingerprint)
	cfg.InsecureTLS = valueOr(req.InsecureTLS, cfg.InsecureTLS)
	if req.APIURLs != nil {
		cfg.APIURLs = req.APIURLs
	}
	return cfg, stored || (cfg.APIURL != "" && cfg.TokenID != "" && cfg.Secret != "")
}

func valueOr[T any](p *T, fallback T) T {
	if p != nil {
		return *p
	}
	return fallback
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
