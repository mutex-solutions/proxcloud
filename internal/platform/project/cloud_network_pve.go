package project

// cloud_network_pve.go — the Proxmox NETWORK write leg: user-facing L3
// self-service (name + CIDR-or-auto + DHCP + SNAT), the sdn-apply serialization
// lock, CIDR auto-carving, and the public-NIC attach/detach server actions.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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

// SDNApplyLockName serializes PUT /cluster/sdn platform-wide (it is global +
// async on the PVE side and PVE itself does not serialize it).
const SDNApplyLockName = "sdn-apply"

// SDNApplyMaxFor bounds a crashed holder: the next apply may steal the lock
// after this long.
const SDNApplyMaxFor = 3 * time.Minute

// lockSDN takes the sdn-apply lock and returns its release; busy → 409.
func (h *Handler) lockSDN(ctx context.Context) (func(), error) {
	if h.sdnLock == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "network service not configured")
	}
	now := time.Now().UTC()
	ok, err := h.sdnLock.Lock(ctx, SDNApplyLockName, SDNApplyMaxFor, now)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Another network change is being applied — retry in a moment")
	}
	return func() { _ = h.sdnLock.Unlock(ctx, SDNApplyLockName, 0, now, time.Now().UTC()) }, nil
}

// withSDNLock runs fn under the sdn-apply lock; busy → 409.
func (h *Handler) withSDNLock(ctx context.Context, w http.ResponseWriter, fn func() error) bool {
	unlock, err := h.lockSDN(ctx)
	if err != nil {
		h.fail(w, err)
		return false
	}
	defer unlock()
	if err := fn(); err != nil {
		// Validation done under the lock (CIDR checks) keeps its own status.
		var he *httpx.HTTPError
		if !errors.As(err, &he) {
			he = httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error())
		}
		h.fail(w, he)
		return false
	}
	return true
}

// pveCreateNetwork handles POST /project/{id}/cloud {type:NETWORK} on a proxmox
// provider. Body: {name, cidr?, enableDhcp?, snat?} — cidr blank auto-carves a
// /24 from the provider's cidrPool; a given cidr must sit inside the pool and
// overlap no subnet on the cluster (any project).
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
	if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{Networks: 1}); herr != nil {
		h.fail(w, herr)
		return
	}
	ps, ok2 := h.pveService(w, es)
	if !ok2 {
		return
	}
	requested := strAny(data["cidr"])
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
		// CIDR pick under the lock: the live subnet list then includes every staged
		// network, so two concurrent creates cannot land on the same range.
		used, err := ps.SubnetCIDRs(ctx)
		if err != nil {
			return err
		}
		cidr, herr := pvePickCIDR(requested, netCfg.CIDRPool, used)
		if herr != nil {
			return herr
		}
		if netCfg.Isolation == pve.IsolationEVPN {
			if zone, err = ps.EnsureProjectZone(ctx, proj.ID, zone, netCfg.EVPNController); err != nil {
				return fmt.Errorf("ensure project zone: %w", err)
			}
		} else if err := ps.EnsureZone(ctx, zone); err != nil {
			return fmt.Errorf("ensure zone: %w", err)
		}
		res, err := ps.CreateNetwork(ctx, pve.CreateNetworkReq{
			ProjectID: proj.ID, Name: name, Zone: zone, CIDR: cidr, DHCP: dhcp, SNAT: snat,
		})
		if err != nil {
			return err
		}
		// A new network's subnet is allowed to its project before it goes live.
		if err := ps.ReconcileIsolation(ctx, proj.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs); err != nil {
			_ = ps.DeleteNetwork(ctx, proj.ID, res.Vnet)
			return fmt.Errorf("isolation: %w", err)
		}
		result = res
		if err := ps.ApplyAndWait(ctx); err != nil {
			// The apply failed with the vnet staged — unstage and re-apply so the SDN
			// config never sits pending (risk #2 in the plan).
			_ = ps.DeleteNetwork(ctx, proj.ID, res.Vnet)
			_ = ps.ReconcileIsolation(ctx, proj.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs)
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
	ctx = context.WithoutCancel(ctx) // see pveCreate
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
	netCfg := es.PVENetworkConfig()
	return h.withSDNLock(ctx, w, func() error {
		// A freed CIDR may be carved for another tenant next, so no export may still list it.
		return h.withShareAccessRevoked(ctx, proj.ID, networkCIDRs(cr), func() error {
			if err := ps.DeleteNetwork(ctx, proj.ID, cr.ExternalID); err != nil {
				return err
			}
			// Shrink the project's allow-list (a freed CIDR may be carved for another
			// tenant next). No pool = nothing was ever isolated.
			if netCfg.CIDRPool != "" {
				if err := ps.ReconcileIsolation(ctx, proj.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs); err != nil {
					return fmt.Errorf("isolation: %w", err)
				}
			}
			// Lab-verified (PVE 9.2): a zone and its last vnet can go in one apply.
			if netCfg.Isolation == pve.IsolationEVPN {
				if _, err := ps.DeleteProjectZoneIfEmpty(ctx, proj.ID); err != nil {
					return err
				}
			}
			if err := ps.ApplyAndWait(ctx); err != nil {
				return err
			}
			// Under the lock: a refresh right after must not read the deleted network's CIDR off its row.
			return h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC())
		})
	})
}

// pveTeardownNetworks removes what a torn-down project leaves on the SDN and the
// cluster firewall once its VMs are gone: its vnets (+ isolation rules), its EVPN
// zone (evpn-per-project; a no-op in shared mode) and its security groups.
// Idempotent, so a failed teardown can simply be re-run.
func (h *Handler) pveTeardownNetworks(ctx context.Context, ps *pve.Service, projectID string) error {
	unlock, err := h.lockSDN(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	staged, err := ps.TeardownProjectNetworks(ctx, projectID)
	if err != nil {
		return err
	}
	gone, err := ps.DeleteProjectZoneIfEmpty(ctx, projectID)
	if err != nil {
		return err
	}
	if staged || gone {
		if err := ps.ApplyAndWait(ctx); err != nil {
			return err
		}
	}
	return ps.DeleteProjectSecurityGroups(ctx, projectID)
}

// pvePickCIDR returns the network's CIDR: the requested one (canonicalized)
// when it sits inside the provider's cidrPool and overlaps nothing in used, or
// — when requested is blank — the first free /24 of the pool. used is every
// subnet on the cluster (all projects): non-overlap is what the shared-zone
// isolation rules rely on.
func pvePickCIDR(requested, pool string, used []string) (string, *httpx.HTTPError) {
	if pool == "" {
		return "", httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "No network CIDR pool is configured on this provider")
	}
	_, poolNet, err := net.ParseCIDR(pool)
	if err != nil {
		return "", httpx.BadRequest("provider cidrPool is invalid")
	}
	ones, bits := poolNet.Mask.Size()
	if bits != 32 || ones > 24 {
		return "", httpx.BadRequest("cidrPool must be an IPv4 range of /24 or larger")
	}
	var usedNets []*net.IPNet
	for _, u := range used {
		if _, n, err := net.ParseCIDR(u); err == nil {
			usedNets = append(usedNets, n)
		}
	}
	overlaps := func(n *net.IPNet) bool {
		for _, u := range usedNets {
			if u.Contains(n.IP) || n.Contains(u.IP) {
				return true
			}
		}
		return false
	}
	if requested != "" {
		_, want, err := net.ParseCIDR(requested)
		if err != nil {
			return "", httpx.BadRequest("Invalid CIDR")
		}
		wantOnes, _ := want.Mask.Size()
		if !poolNet.Contains(want.IP) || wantOnes < ones {
			return "", httpx.BadRequest("CIDR must be inside the provider's network pool " + poolNet.String())
		}
		if wantOnes > 29 {
			return "", httpx.BadRequest("CIDR must be /29 or larger")
		}
		if overlaps(want) {
			return "", httpx.NewError(http.StatusConflict, http.StatusConflict, "CIDR overlaps an existing network")
		}
		return want.String(), nil
	}
	base := poolNet.IP.To4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	count := 1 << (24 - ones) // /24s in the pool
	for i := 0; i < count; i++ {
		c := n + uint32(i)<<8
		cand := &net.IPNet{IP: net.IPv4(byte(c>>24), byte(c>>16), byte(c>>8), 0).To4(), Mask: net.CIDRMask(24, 32)}
		if !overlaps(cand) {
			return cand.String(), nil
		}
	}
	return "", httpx.NewError(http.StatusConflict, http.StatusConflict, "The provider's network CIDR pool is exhausted")
}

// pvePublicNetworkAction handles ATTACH_PUBLIC_NETWORK / DETACH_PUBLIC_NETWORK on
// a SERVER: inbound reachability = a second NIC (net1) on an admin-designated
// bridge with one address from that network's pool (no floating IPs in the PVE
// model). Returns handled.
// pveDetachPublicNIC unplugs a VM's public NIC and frees its address. Empty net1's ipfilter first:
// should the unplug only go pending (no hotplug), the NIC can no longer use the address once it is
// reallocated. The address goes back to the pool only after the NIC is gone.
// Lab-verified (PVE 9.2): an empty ipfilter-net1 set drops all net1 egress, and deleting an absent
// net1/ipconfig1 key only warns.
func (h *Handler) pveDetachPublicNIC(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, proj *Project, vm *pve.ClusterResource, serverID string) error {
	if err := ps.SetIPFilter(ctx, vm.Node, vm.VMID, "net1", nil); err != nil {
		return err
	}
	// net0 takes the default route back (see the attach below).
	restore := map[string]string{}
	held, err := h.publicIPs.ForServer(ctx, es.ID, serverID)
	if err != nil {
		return err
	}
	for _, row := range held {
		if row.NIC == "net0" && row.ProjectID == proj.ID {
			_, gateway, _, _, _, gerr := ps.VnetStaticPoolFor(ctx, row.NetworkID, row.IP)
			if gerr != nil {
				return gerr
			}
			restore["ipconfig0"] = fmt.Sprintf("ip=%s/%d,gw=%s", row.IP, row.Prefix, gateway)
		}
	}
	up, err := ps.C.SetVMConfig(ctx, vm.Node, vm.VMID, restore, "net1", "ipconfig1")
	if err != nil {
		return err
	}
	if err := ps.C.WaitTask(ctx, up); err != nil {
		return err
	}
	return h.publicIPs.ReleaseNIC(ctx, es.ID, serverID, publicNIC)
}

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
	if h.publicIPs == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "public IP service not configured")
		return true
	}
	vm, verr := ps.ResolveVM(ctx, proj.ID, cr.ExternalID)
	if verr != nil || vm == nil {
		h.fail(w, httpx.NotFound("Server not found"))
		return true
	}
	if action == "DETACH_PUBLIC_NETWORK" {
		if derr := h.pveDetachPublicNIC(ctx, ps, es, proj, vm, cr.ExternalID); derr != nil {
			h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, derr.Error()))
			return true
		}
		h.pveReseed(ctx, ps, es, cr.ExternalID, vm.Node)
		h.pveRefreshServerRow(ctx, ps, proj, cr)
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
		httpx.OK(w, map[string]any{"result": map[string]any{"rebootRequired": true}})
		return true
	}
	netID := strAny(data["networkId"])
	var pn *externalservice.PVEPublicNetwork
	for _, n := range es.PVENetworkConfig().PublicNetworks {
		if n.ID == netID {
			pn = &n
			break
		}
	}
	// Per-project restriction rides the project's PublicNetworkIds allow-list.
	if pn == nil || !publicNetworkAllowed(proj, netID) {
		h.fail(w, httpx.BadRequest("Public network is not available"))
		return true
	}
	// Never the management bridge (test-connection also flags it, but the config
	// may have changed since).
	mgmt, merr := ps.C.ManagementBridges(ctx, vm.Node)
	if merr != nil {
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, merr.Error()))
		return true
	}
	if _, bad := mgmt[netID]; bad {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable,
			fmt.Sprintf("Public network %s is the Proxmox management bridge and cannot be attached", netID))
		return true
	}
	held, err := h.publicIPs.ForServer(ctx, es.ID, cr.ExternalID)
	if err != nil {
		h.fail(w, err)
		return true
	}
	var ip, net0IP *PublicIP
	for i := range held {
		// PVE reuses vmids: a row a failed release left behind for another
		// project's deleted VM is never handed to this one.
		if held[i].ProjectID != proj.ID {
			continue
		}
		switch held[i].NIC {
		case publicNIC:
			ip = &held[i]
		case "net0":
			net0IP = &held[i]
		}
	}
	if ip != nil && ip.NetworkID != netID {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "Server already has a public network — detach it first"))
		return true
	}
	fresh := ip == nil // a repeat attach to the same network keeps its address
	if fresh {
		if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{FloatingIPs: 1}); herr != nil {
			h.fail(w, herr)
			return true
		}
		if ip, err = h.publicIPs.Allocate(ctx, es.ID, proj.ID, cr.ExternalID, publicNIC, *pn); err != nil {
			h.fail(w, err)
			return true
		}
	}
	// Anti-spoof first (VMs made before the firewall default get it here), then
	// pin net1 to its /32, then plug the NIC: it never comes up unfiltered.
	// Lab-verified (PVE 9.2): the NIC hot-plugs, but ipconfig1 reaches the guest only when
	// cloud-init re-runs (next boot), hence rebootRequired. With a gateway on both NICs the guest
	// keeps net0's default route, and replies from the public address then leave through the
	// overlay, where its ipfilter and the exit node's SNAT do not cover them. So the public NIC
	// takes the default route and a static net0 gives up its gateway. A DHCP net0 cannot.
	ipcfg := "ip=" + ip.IP + "/" + strconv.Itoa(ip.Prefix)
	if pn.Gateway != "" {
		ipcfg += ",gw=" + pn.Gateway
	}
	cfgParams := map[string]string{
		"net1":      "virtio,bridge=" + netID + ",firewall=1",
		"ipconfig1": ipcfg,
	}
	if net0IP != nil && pn.Gateway != "" {
		cfgParams["ipconfig0"] = "ip=" + net0IP.IP + "/" + strconv.Itoa(net0IP.Prefix)
	}
	plugged := false
	serr := ps.SecureVM(ctx, vm.Node, vm.VMID)
	if serr == nil && net0IP != nil { // SecureVM widened net0 to its subnet
		serr = ps.SetIPFilter(ctx, vm.Node, vm.VMID, "net0", []string{net0IP.IP + "/32"})
	}
	if serr == nil {
		serr = ps.SetIPFilter(ctx, vm.Node, vm.VMID, "net1", []string{ip.IP + "/32"})
	}
	var up pve.UPID
	if serr == nil {
		plugged = true
		up, serr = ps.C.SetVMConfig(ctx, vm.Node, vm.VMID, cfgParams)
	}
	if serr == nil {
		serr = ps.C.WaitTask(ctx, up)
	}
	if serr != nil {
		// Only an address no NIC was ever plugged with goes straight back; past
		// that point it stays held until a detach.
		if fresh && !plugged {
			_ = h.publicIPs.ReleaseNIC(ctx, es.ID, cr.ExternalID, publicNIC)
		}
		h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, serr.Error()))
		return true
	}
	h.pveReseed(ctx, ps, es, cr.ExternalID, vm.Node)
	h.pveRefreshServerRow(ctx, ps, proj, cr)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{"ip": ip.IP, "networkId": netID, "rebootRequired": true}})
	return true
}

// pveSubnetAction handles ADD_SUBNET / REMOVE_SUBNET / SET_SNAT on a NETWORK: a vnet holds several
// subnets. Routing between them, and between the project's networks, needs nothing of a tenant: the
// zone's VRF is the router, and the project's isolation ACCEPT covers all its subnets.
func (h *Handler) pveSubnetAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if cr.Type != cloud.TypeNetwork || (action != "ADD_SUBNET" && action != "REMOVE_SUBNET" && action != "SET_SNAT") {
		return false
	}
	ctx := r.Context()
	ps, ok := h.pveService(w, es)
	if !ok {
		return true
	}
	netCfg := es.PVENetworkConfig()
	cidr := strAny(data["cidr"])
	if action == "REMOVE_SUBNET" {
		held, err := h.publicIPs.InNetwork(ctx, es.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, err)
			return true
		}
		for _, row := range held {
			if pve.SubnetHolds(cidr, row.IP) {
				httpx.Err(w, http.StatusConflict, http.StatusConflict, "Subnet "+cidr+" is in use by server "+row.ServerID)
				return true
			}
		}
	}
	snat := true
	if v, ok := data["snat"].(bool); ok {
		snat = v
	}
	if !h.withSDNLock(ctx, w, func() error {
		switch action {
		case "ADD_SUBNET":
			used, err := ps.SubnetCIDRs(ctx)
			if err != nil {
				return err
			}
			picked, herr := pvePickCIDR(cidr, netCfg.CIDRPool, used)
			if herr != nil {
				return herr
			}
			if _, err := ps.AddSubnet(ctx, proj.ID, cr.ExternalID, picked, snat); err != nil {
				return pveVolumeErr(err)
			}
		case "REMOVE_SUBNET":
			// Off the exports first, and the row follows inside the lock: a freed CIDR may be carved for
			// another tenant next.
			return h.withShareAccessRevoked(ctx, proj.ID, []string{cidr}, func() error {
				if err := ps.RemoveSubnet(ctx, proj.ID, cr.ExternalID, cidr); err != nil {
					return pveVolumeErr(err)
				}
				if err := ps.ReconcileIsolation(ctx, proj.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs); err != nil {
					return fmt.Errorf("isolation: %w", err)
				}
				if err := ps.ApplyAndWait(ctx); err != nil {
					return err
				}
				return h.pveRefreshSubnetRow(ctx, ps, cr)
			})
		case "SET_SNAT":
			if err := ps.SetSubnetSNAT(ctx, proj.ID, cr.ExternalID, cidr, snat); err != nil {
				return pveVolumeErr(err)
			}
		}
		if err := ps.ReconcileIsolation(ctx, proj.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs); err != nil {
			return fmt.Errorf("isolation: %w", err)
		}
		return ps.ApplyAndWait(ctx)
	}) {
		return true
	}
	if action != "REMOVE_SUBNET" {
		if err := h.pveRefreshSubnetRow(ctx, ps, cr); err != nil {
			slog.Error("pve network: refresh subnets", "network", cr.ExternalID, "err", err)
		}
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": *cr})
	return true
}

// pveRefreshSubnetRow rereads a NETWORK row's subnets off the SDN.
func (h *Handler) pveRefreshSubnetRow(ctx context.Context, ps *pve.Service, cr *cloud.CloudResource) error {
	net, _ := cr.Data["network"].(map[string]any)
	if net == nil {
		return nil
	}
	subs, err := ps.C.ListSDNSubnets(ctx, cr.ExternalID)
	if err != nil {
		return err
	}
	for k, v := range pve.SubnetDetail(subs) {
		net[k] = v
	}
	saved, err := h.cloud.Insert(ctx, cr)
	if err != nil {
		return err
	}
	*cr = *saved
	return nil
}
