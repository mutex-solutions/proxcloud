package project

// cloud_network_pve.go — the Proxmox NETWORK write leg: user-facing L3
// self-service (name + CIDR-or-auto + DHCP + SNAT), the sdn-apply serialization
// lock, CIDR auto-carving, and the public-NIC attach/detach server actions.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SetSDNLock wires the platform-wide sdn-apply lock (nil = proxmox network
// writes are unavailable — apply MUST be serialized).
func (h *Handler) SetSDNLock(l *lock.ShedLock) { h.sdnLock = l }

// sdnApplyLockName serializes PUT /cluster/sdn platform-wide (it is global +
// async on the PVE side and PVE itself does not serialize it).
const sdnApplyLockName = "sdn-apply"

// sdnApplyMaxFor bounds a crashed holder: the next apply may steal the lock
// after this long.
const sdnApplyMaxFor = 3 * time.Minute

// withSDNLock runs fn under the sdn-apply lock; busy → 409.
func (h *Handler) withSDNLock(ctx context.Context, w http.ResponseWriter, fn func() error) bool {
	if h.sdnLock == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "network service not configured")
		return false
	}
	now := time.Now().UTC()
	ok, err := h.sdnLock.Lock(ctx, sdnApplyLockName, sdnApplyMaxFor, now)
	if err != nil {
		h.fail(w, err)
		return false
	}
	if !ok {
		httpx.Err(w, http.StatusConflict, http.StatusConflict, "Another network change is being applied — retry in a moment")
		return false
	}
	defer func() { _ = h.sdnLock.Unlock(ctx, sdnApplyLockName, 0, now, time.Now().UTC()) }()
	if err := fn(); err != nil {
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error()))
		return false
	}
	return true
}

// pveCreateNetwork handles POST /project/{id}/cloud {type:NETWORK} on a proxmox
// provider. Body: {name, cidr?, enableDhcp?, snat?} — cidr blank auto-carves a
// /24 from the provider's cidrPool.
func (h *Handler) pveCreateNetwork(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx := r.Context()
	name := strAny(data["name"])
	if name == "" {
		h.fail(w, httpx.BadRequest("name is required"))
		return
	}
	if strings.ContainsAny(name, ":;") {
		h.fail(w, httpx.BadRequest("name must not contain ':' or ';'"))
		return
	}
	netCfg := es.PVENetworkConfig()
	zone, ok := netCfg.Zones[netCfg.DefaultZone]
	if !ok {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "No network zone is configured on this provider")
		return
	}
	ps, ok2 := h.pveService(w, es)
	if !ok2 {
		return
	}
	cidr := strAny(data["cidr"])
	if cidr == "" {
		carved, cerr := h.pveCarveCIDR(ctx, es.ID, netCfg.CIDRPool)
		if cerr != nil {
			h.fail(w, cerr)
			return
		}
		cidr = carved
	} else if _, _, err := net.ParseCIDR(cidr); err != nil {
		h.fail(w, httpx.BadRequest("Invalid CIDR"))
		return
	}
	dhcp := true
	if v, ok := data["enableDhcp"].(bool); ok {
		dhcp = v
	}
	snat := true
	if v, ok := data["snat"].(bool); ok {
		snat = v
	}
	var result *pve.NetworkResult
	okRun := h.withSDNLock(ctx, w, func() error {
		if err := ps.EnsureZone(ctx, zone); err != nil {
			return fmt.Errorf("ensure zone: %w", err)
		}
		res, err := ps.CreateNetwork(ctx, pve.CreateNetworkReq{
			ProjectID: proj.ID, Name: name, Zone: zone, CIDR: cidr, DHCP: dhcp, SNAT: snat,
		})
		if err != nil {
			return err
		}
		result = res
		if err := ps.ApplyAndWait(ctx); err != nil {
			// The apply failed with the vnet staged — unstage and re-apply so the SDN
			// config never sits pending (risk #2 in the plan).
			_ = ps.DeleteNetwork(ctx, proj.ID, res.Vnet)
			_ = ps.ApplyAndWait(ctx)
			return fmt.Errorf("apply: %w", err)
		}
		return nil
	})
	if !okRun {
		return
	}
	now := time.Now().UTC()
	cr := &cloud.CloudResource{
		ServiceID: es.ID, Region: h.pveRegion(proj, es), ProjectID: proj.ID,
		Type: cloud.TypeNetwork, ExternalID: result.Vnet,
		Data: pve.NetworkData(result.Vnet, name, zone.Key, map[string]any{
			"cidr": result.CIDR, "gateway": result.Gateway, "snat": snat, "dhcp": dhcp,
			"subnets": []any{result.CIDR},
		}),
		CreatedAt: &now, UpdatedAt: &now,
		Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}
	saved, err := h.cloud.Insert(ctx, cr)
	if err != nil {
		saved = cr
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

// pveDeleteNetwork removes a vnet (guarded: no pool VM NIC may sit on it).
func (h *Handler) pveDeleteNetwork(ctx context.Context, w http.ResponseWriter, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) bool {
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return false
	}
	inUse, uerr := ps.VnetInUse(ctx, proj.ID, cr.ExternalID)
	if uerr != nil {
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, uerr.Error()))
		return false
	}
	if inUse {
		httpx.Err(w, http.StatusConflict, http.StatusConflict, "Network is in use by a server")
		return false
	}
	return h.withSDNLock(ctx, w, func() error {
		if err := ps.DeleteNetwork(ctx, proj.ID, cr.ExternalID); err != nil {
			return err
		}
		return ps.ApplyAndWait(ctx)
	})
}

// pveCarveCIDR finds the first free /24 inside the provider's cidrPool, checked
// against every cached NETWORK row of the SERVICE (all projects — the pool is
// shared cluster-wide).
func (h *Handler) pveCarveCIDR(ctx context.Context, svcID, pool string) (string, *httpx.HTTPError) {
	if pool == "" {
		return "", httpx.BadRequest("cidr is required (no cidrPool configured)")
	}
	_, poolNet, err := net.ParseCIDR(pool)
	if err != nil {
		return "", httpx.BadRequest("provider cidrPool is invalid")
	}
	ones, bits := poolNet.Mask.Size()
	if bits != 32 || ones > 24 {
		return "", httpx.BadRequest("cidrPool must be an IPv4 range of /24 or larger")
	}
	rows, err := h.cloud.FindByServiceAndType(ctx, svcID, cloud.TypeNetwork)
	if err != nil {
		return "", httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
	}
	used := map[string]bool{}
	for i := range rows {
		if netMap, ok := rows[i].Data["network"].(map[string]any); ok {
			if c := strAny(netMap["cidr"]); c != "" {
				used[c] = true
			}
		}
	}
	base := poolNet.IP.To4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	count := 1 << (24 - ones) // /24s in the pool
	for i := 0; i < count; i++ {
		c := n + uint32(i)<<8
		cidr := fmt.Sprintf("%d.%d.%d.0/24", byte(c>>24), byte(c>>16), byte(c>>8))
		if !used[cidr] {
			return cidr, nil
		}
	}
	return "", httpx.NewError(http.StatusConflict, http.StatusConflict, "The provider's network CIDR pool is exhausted")
}

// pvePublicNetworkAction handles ATTACH_PUBLIC_NETWORK / DETACH_PUBLIC_NETWORK on
// a SERVER: inbound reachability = a second NIC on an admin-designated bridge
// (no floating IPs in the PVE model). Returns handled.
func (h *Handler) pvePublicNetworkAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if action != "ATTACH_PUBLIC_NETWORK" && action != "DETACH_PUBLIC_NETWORK" {
		return false
	}
	ctx := r.Context()
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return true
	}
	vm, verr := ps.ResolveVM(ctx, proj.ID, cr.ExternalID)
	if verr != nil || vm == nil {
		h.fail(w, httpx.NotFound("Server not found"))
		return true
	}
	if action == "DETACH_PUBLIC_NETWORK" {
		if up, err := ps.C.SetVMConfig(ctx, vm.Node, vm.VMID, nil, "net1"); err == nil {
			_ = ps.C.WaitTask(ctx, up)
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{}})
		return true
	}
	netID := strAny(data["networkId"])
	allowed := false
	for _, pn := range es.PVENetworkConfig().PublicNetworks {
		if pn.ID == netID {
			allowed = true
			break
		}
	}
	// Per-project restriction rides the same PublicNetworkIds field as OpenStack.
	if !allowed || !publicNetworkAllowed(proj, netID) {
		h.fail(w, httpx.BadRequest("Public network is not available"))
		return true
	}
	up, serr := ps.C.SetVMConfig(ctx, vm.Node, vm.VMID, map[string]string{
		"net1": "virtio,bridge=" + netID,
	})
	if serr == nil {
		serr = ps.C.WaitTask(ctx, up)
	}
	if serr != nil {
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, serr.Error()))
		return true
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{}})
	return true
}
