package project

// cloud_lb_pair_pve.go — a load balancer on two appliances (field ha, the default for a new balancer). The address
// is a VIP owned by the balancer (a pvePublicIp row of owner "lb:<id>", net1), which keepalived moves between two
// VMs built from the stock OS template; both run the same HAProxy config. Placement: the soft spread group
// "lb-<id>", and both on the first one's node when the tenant network is a simple zone, whose traffic stays on its
// node. What HA means here is active-standby: one VM carries the traffic, and a switch cuts open connections. A
// balancer without ha is the single appliance of cloud_lb_pve.go, unchanged.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	lbStatusDegraded = "DEGRADED"

	// Appliance states, as the last apply saw them.
	lbAppUp       = "up"
	lbAppDown     = "down"
	lbAppBuilding = "building"
	lbAppGone     = "gone"

	// lbReplaceMaxFor bounds a replace: two builds and their configs. A mark older than this is from a portal that
	// died meanwhile, and no longer holds anything.
	lbReplaceMaxFor = 3 * lbBuildTimeout
)

// lbAppliance is one VM of a pair.
type lbAppliance struct {
	ServerID  string `json:"serverId"`
	Node      string `json:"node,omitempty"`
	PrivateIP string `json:"privateIp,omitempty"`
	// The guest's names of net0 and net1, read once through the guest agent.
	TenantIface string `json:"tenantIface,omitempty"`
	PublicIface string `json:"publicIface,omitempty"`
	// What the appliance runs, by hash of the rendered file.
	HAProxyHash    string `json:"haproxyHash,omitempty"`
	KeepalivedHash string `json:"keepalivedHash,omitempty"`
	Role           string `json:"role,omitempty"` // keepalived's state: MASTER, BACKUP, FAULT, STOPPED, UNKNOWN
	HoldsVIP       bool   `json:"holdsVip,omitempty"`
	State          string `json:"state,omitempty"` // up, down, building, gone
	Reason         string `json:"reason,omitempty"`
}

// lbPairDemand is what a pair asks of the quota at create: two servers of the flavor and the address.
func lbPairDemand(fl *pveflavor.Flavor) pveQuotaDemand {
	return pveQuotaDemand{VCPUs: 2 * fl.VCPUs, RAMMb: 2 * fl.RAMMb, Servers: 2, DiskGb: 2 * fl.DiskGb, FloatingIPs: 1}
}

// lbRouterID is the pair's virtual_router_id. Adverts go unicast to the peer only, so two pairs on one network do not
// meet; the id only has to be in range and stable.
func lbRouterID(id string) int {
	n, _ := strconv.ParseUint(id, 16, 64)
	return int(n%255) + 1
}

// lbReplacing: a replace of this pair is running.
func lbReplacing(lb *loadBalancer, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, lb.Replacing)
	return err == nil && now.Sub(t) < lbReplaceMaxFor
}

func (h *Handler) pveCreateLBPair(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any, pn *externalservice.PVEPublicNetwork, bridge string) {
	ctx := r.Context()
	name := strAny(data["name"])
	if h.pveFor == nil || h.pveFlavors == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Load balancers are not configured")
		return
	}
	ps, err := h.pveFor(es)
	if err != nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
		return
	}
	// The VIP must be in both appliances' ip filter sets. On a vnet the platform writes that set from the vnet's
	// subnets, and an EVPN zone suppresses ARP for an address that moves (C10).
	if _, vnet, err := ps.SDNZoneOf(ctx, pn.ID); err != nil {
		h.fail(w, upstreamErr(computeBackend, err))
		return
	} else if vnet {
		h.fail(w, httpx.BadRequest("Public network "+pn.ID+" is an SDN vnet, and two appliances cannot share an address there. Ask your operator for a public network on a plain bridge, or create the load balancer without high availability"))
		return
	}
	fl, err := h.pveFlavors.Get(ctx, strAny(data["flavorId"]))
	if err != nil || fl == nil || !fl.Active {
		h.fail(w, httpx.BadRequest("Unknown flavor"))
		return
	}
	if herr := h.enforcePVEQuota(ctx, proj, es, lbPairDemand(fl)); herr != nil {
		h.fail(w, herr)
		return
	}
	id, err := randomHex(6)
	if err != nil {
		h.fail(w, err)
		return
	}
	now := time.Now().UTC()
	row := &cloud.CloudResource{
		ExternalID: "lb-" + id, ServiceID: es.ID, ProjectID: proj.ID, Region: h.pveRegion(proj, es),
		Type: cloud.TypeLoadBalancer, CreatedAt: &now, Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}
	lb := &loadBalancer{ID: id, Name: name, Status: lbStatusBuild, NetworkID: bridge, NetworkRef: strAny(data["networkId"]),
		PublicNetworkID: pn.ID, HA: true, FlavorID: fl.ID}
	if err := setLBRow(row, lb); err != nil {
		h.fail(w, err)
		return
	}
	if row, err = h.cloud.Insert(ctx, row); err != nil {
		h.fail(w, err)
		return
	}
	vip, err := h.publicIPs.Allocate(ctx, es.ID, proj.ID, LBAddressOwner(id), publicNIC, *pn)
	if err != nil {
		if derr := h.cloud.DeleteAndArchive(ctx, row, time.Now().UTC()); derr != nil {
			slog.Error("load balancer: drop row after a failed address", "project", proj.ID, "err", derr)
		}
		h.fail(w, err)
		return
	}
	lb.VIPs, lb.PublicIP = []string{vip.IP}, vip.IP
	if err := h.lbSave(ctx, row, lb); err != nil {
		h.fail(w, err)
		return
	}
	for range 2 {
		app, berr := h.lbBuildAppliance(ctx, u, proj, es, ps, lb)
		if berr == nil {
			lb.Appliances = append(lb.Appliances, *app)
			if lb.ServerID == "" { // what the legacy fields name: the subnet members must be on, and "created"
				lb.ServerID, lb.PrivateIP = app.ServerID, app.PrivateIP
			}
			berr = h.lbSave(ctx, row, lb)
		}
		if berr != nil {
			h.lbAbandon(ctx, es, proj, row, lb)
			h.fail(w, berr)
			return
		}
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", row)
	go h.lbConverge(context.WithoutCancel(ctx), es, proj, row.ExternalID)
	httpx.OK(w, *row)
}

// lbBuildAppliance builds one VM of a pair. The second of a pair on a simple zone goes to the first one's node: a
// simple zone's traffic never leaves its node (C9).
func (h *Handler) lbBuildAppliance(ctx context.Context, u *user.User, proj *Project, es *externalservice.ExternalService, ps *pve.Service, lb *loadBalancer) (*lbAppliance, error) {
	pn := pveAllowedPublicNetwork(es, proj, lb.PublicNetworkID)
	if pn == nil {
		return nil, httpx.BadRequest("Public network is not available")
	}
	pin := ""
	if len(lb.Appliances) > 0 {
		zone, _, err := ps.SDNZoneOf(ctx, lb.NetworkID)
		if err != nil {
			return nil, upstreamErr(computeBackend, err)
		}
		if zone == "simple" {
			pin = lb.Appliances[0].Node
		}
	}
	app := &lbAppliance{State: lbAppBuilding}
	ap := &pveAppliance{
		loadBalancerID: lb.ID,
		node:           pin,
		beforeBoot: func(ctx context.Context, ps *pve.Service, node string, vmid int) error {
			app.ServerID, app.Node = strconv.Itoa(vmid), node
			return h.lbPlugPairPublic(ctx, ps, es, proj, pn, lb, app, node, vmid)
		},
	}
	if _, err := h.pveBuildServer(ctx, u, proj, es, map[string]any{
		"name":        "lb-" + lb.Name,
		"flavorId":    lb.FlavorID,
		"imageId":     es.PVELoadBalancerImage(),
		"networkIds":  []any{lb.NetworkRef},
		"spreadGroup": "lb-" + lb.ID,
	}, ap); err != nil {
		return nil, err
	}
	return app, nil
}

// lbPlugPairPublic gives an appliance of a pair its public NIC before first boot: no address and no cloud-init
// config of its own (keepalived adds the VIPs), the VIPs in its ip filter set, and net0 keeps the tenant network's
// gateway for the appliance's own traffic.
func (h *Handler) lbPlugPairPublic(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, proj *Project, pn *externalservice.PVEPublicNetwork, lb *loadBalancer, app *lbAppliance, node string, vmid int) error {
	mgmt, err := ps.C.ManagementBridges(ctx, node)
	if err != nil {
		return err
	}
	if _, bad := mgmt[pn.ID]; bad {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Public network "+pn.ID+" is the Proxmox management bridge")
	}
	held, err := h.publicIPs.ForServer(ctx, es.ID, strconv.Itoa(vmid))
	if err != nil {
		return err
	}
	for _, row := range held {
		if row.NIC == "net0" && row.ProjectID == proj.ID {
			app.PrivateIP, lb.PrivatePrefix = row.IP, row.Prefix
		}
	}
	if app.PrivateIP == "" {
		return fmt.Errorf("load balancer: vm %d has no address on its network", vmid)
	}
	if err := ps.SetIPFilterEntries(ctx, node, vmid, publicNIC, lb.VIPs); err != nil {
		return err
	}
	up, err := ps.C.SetVMConfig(ctx, node, vmid, map[string]string{publicNIC: "virtio,bridge=" + pn.ID + ",firewall=1"})
	if err == nil {
		err = ps.C.WaitTask(ctx, up)
	}
	return err
}

// lbAbandon takes back a pair whose create failed: its VMs and its address. If that fails too, the row stays in
// ERROR, as the tenant's way to remove what is left.
func (h *Handler) lbAbandon(ctx context.Context, es *externalservice.ExternalService, proj *Project, row *cloud.CloudResource, lb *loadBalancer) {
	ps, err := h.pveFor(es)
	if err == nil {
		err = h.lbDestroy(ctx, ps, es, proj, lb)
	}
	if err != nil {
		slog.Error("load balancer: remove a pair that could not be built", "project", proj.ID, "lb", lb.ID, "err", err)
		lb.Status, lb.StatusMessage = lbStatusError, "The load balancer could not be built and its VMs could not be removed. Delete this load balancer and create it again."
		if serr := h.lbSave(ctx, row, lb); serr != nil {
			slog.Error("load balancer: save row after a failed build", "project", proj.ID, "err", serr)
		}
		return
	}
	if err := h.cloud.DeleteAndArchive(ctx, row, time.Now().UTC()); err != nil {
		slog.Error("load balancer: drop row after a failed build", "project", proj.ID, "err", err)
	}
}

// lbPushOrder is the order configs reach a pair: the standby first, the one holding the address last, so a config
// that breaks HAProxy shows on the standby before it can reach the traffic.
func lbPushOrder(apps []*lbAppliance) []*lbAppliance {
	out := slices.Clone(apps)
	slices.SortStableFunc(out, func(a, b *lbAppliance) int {
		switch {
		case a.HoldsVIP == b.HoldsVIP:
			return 0
		case a.HoldsVIP:
			return 1
		}
		return -1
	})
	return out
}

// lbPeerOf is the other appliance of the pair, nil when it has none with a known address.
func lbPeerOf(lb *loadBalancer, a *lbAppliance) *lbAppliance {
	for i := range lb.Appliances {
		if p := &lb.Appliances[i]; p.ServerID != a.ServerID && p.PrivateIP != "" {
			return p
		}
	}
	return nil
}

// lbApplyPair brings a pair in line with its row. Order (C14): firewall and new VIPs in the ip filter sets on every
// running appliance first, so a VIP is in both sets before keepalived can take it; then per appliance, standby first,
// keepalived and HAProxy; then a VIP no longer served leaves the sets, once keepalived let go of it; then each one's
// state is read back. Rules and sets are compared on every pass, so one changed by hand is put back.
func (h *Handler) lbApplyPair(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource, lb *loadBalancer) error {
	ps, err := h.pveFor(es)
	if err != nil {
		return err
	}
	vms, err := lbVMs(ctx, ps)
	if err != nil {
		return err // nothing is known, so nothing changes
	}
	pool := ps.PoolFor(proj.ID)
	var running []*lbAppliance
	for i := range lb.Appliances {
		a := &lb.Appliances[i]
		a.Reason = ""
		vm, listed := vms[a.ServerID]
		switch {
		case !listed || vm.Pool != pool || (!vm.NodeDown && pve.LBOfTags(vm.Tags) != lb.ID):
			a.State, a.Role, a.HoldsVIP, a.Reason = lbAppGone, "", false, "its VM is gone"
			continue
		case vm.NodeDown:
			a.State, a.HoldsVIP, a.Reason = lbAppDown, false, "its node is down"
			continue
		}
		a.Node = vm.Node
		st, err := ps.C.GetVMStatus(ctx, vm.Node, vm.VMID)
		if err != nil {
			a.State, a.Reason = lbAppDown, "its VM cannot be read"
			continue
		}
		switch {
		case st.QMPStatus == "paused":
			a.State, a.Reason = lbAppDown, "its VM is paused"
			continue
		case st.Status == "stopped":
			a.State, a.HoldsVIP, a.Reason = lbAppDown, false, "its VM is stopped; starting it"
			if up, serr := ps.C.StartVM(ctx, vm.Node, vm.VMID); serr != nil {
				slog.Warn("load balancer: start appliance", "lb", lb.ID, "vm", vm.VMID, "err", serr)
			} else if up != "" {
				_ = ps.C.WaitTask(ctx, up)
			}
			continue
		case st.Status != "running":
			a.State, a.Reason = lbAppDown, "its VM is "+st.Status
			continue
		}
		if a.TenantIface == "" || a.PublicIface == "" {
			t, p, ierr := ps.LBIfaces(ctx, vm.Node, vm.VMID)
			if ierr != nil {
				a.State, a.Reason = lbAppBuilding, "it is booting"
				continue
			}
			a.TenantIface, a.PublicIface = t, p
		}
		running = append(running, a)
	}
	// A member whose server left drops out of the config but stays in the row, marked, for the tenant.
	for i := range lb.Pools {
		for j := range lb.Pools[i].Members {
			m := &lb.Pools[i].Members[j]
			addr, _, aerr := h.lbMemberAddress(ctx, es, proj, lb, pool, vms, m.ServerID, m.Stamp)
			if aerr != nil {
				return aerr
			}
			m.Missing = addr == ""
			if addr != "" {
				m.Address = addr
			}
		}
	}
	haproxy := pve.RenderHAProxyOn(lb.LBSpec, lb.VIPs)
	hhash := lbHash(haproxy)
	pn := pveAllowedPublicNetwork(es, proj, lb.PublicNetworkID)
	var refused error
	leftover := map[*lbAppliance][]string{} // set entries of VIPs no longer served, removed after keepalived let go
	for _, a := range running {
		vmid, _ := strconv.Atoi(a.ServerID)
		rules := pve.LBListenerRules(lb.Listeners, lb.VIPs)
		if peer := lbPeerOf(lb, a); peer != nil {
			rules = append(rules, pve.LBVRRPRule(peer.PrivateIP))
		}
		if err := ps.SetLBRules(ctx, a.Node, vmid, rules); err != nil {
			return err
		}
		extra, err := ps.AddIPFilterEntries(ctx, a.Node, vmid, publicNIC, lb.VIPs)
		if err != nil {
			return err
		}
		leftover[a] = extra
	}
	keepalivedCurrent := map[*lbAppliance]bool{}
	for _, a := range lbPushOrder(running) {
		vmid, _ := strconv.Atoi(a.ServerID)
		// keepalived needs the peer; an appliance whose peer left keeps the config it has, and the address.
		if peer := lbPeerOf(lb, a); peer != nil && pn != nil {
			conf, kerr := pve.RenderKeepalived(pve.LBKeepalived{RouterID: lbRouterID(lb.ID), Self: a.PrivateIP, Peer: peer.PrivateIP,
				TenantIface: a.TenantIface, PublicIface: a.PublicIface, VIPs: lb.VIPs, Prefix: pveNetworkPrefix(pn), Gateway: pn.Gateway})
			if kerr != nil {
				return kerr
			}
			if khash := lbHash(conf + pve.LBPairFilesDigest(a.PublicIface)); khash != a.KeepalivedHash {
				if err := ps.PushKeepalived(ctx, a.Node, vmid, conf, a.PublicIface); err != nil {
					if errors.Is(err, pve.ErrLBNotReady) {
						a.State, a.Reason = lbAppBuilding, "it is installing HAProxy and keepalived"
						continue
					}
					refused = err
					continue
				}
				a.KeepalivedHash = khash
			}
			keepalivedCurrent[a] = true
		}
		if hhash != a.HAProxyHash {
			if err := ps.PushPairHAProxy(ctx, a.Node, vmid, haproxy); err != nil {
				if errors.Is(err, pve.ErrLBNotReady) {
					a.State, a.Reason = lbAppBuilding, "it is installing HAProxy and keepalived"
					continue
				}
				refused = err
				continue
			}
			a.HAProxyHash = hhash
		}
	}
	for a, extra := range leftover {
		if len(extra) > 0 && keepalivedCurrent[a] {
			vmid, _ := strconv.Atoi(a.ServerID)
			if err := ps.RemoveIPFilterEntries(ctx, a.Node, vmid, publicNIC, extra); err != nil {
				return err
			}
		}
	}
	vip := ""
	if len(lb.VIPs) > 0 {
		vip = lb.VIPs[0]
	}
	for _, a := range running {
		if a.State == lbAppBuilding && a.HAProxyHash == "" {
			continue
		}
		vmid, _ := strconv.Atoi(a.ServerID)
		st, err := ps.ReadVRRPState(ctx, a.Node, vmid, vip)
		if err != nil {
			a.State, a.Reason = lbAppDown, "it does not answer"
			continue
		}
		a.Role, a.HoldsVIP = st.State, st.HoldsVIP
		switch {
		case !st.HAProxy:
			a.State, a.Reason = lbAppDown, "HAProxy is not running"
		case st.State != "MASTER" && st.State != "BACKUP":
			a.State, a.Reason = lbAppDown, "keepalived is "+strings.ToLower(st.State)
		case a.HAProxyHash != hhash:
			a.State, a.Reason = lbAppDown, "it runs an older configuration"
		default:
			a.State, a.Reason = lbAppUp, ""
		}
	}
	lb.Status, lb.StatusMessage = lbPairStatus(lb.Appliances, lbStuck(cr))
	if refused != nil {
		lb.Status, lb.StatusMessage = lbStatusError, refused.Error()
	}
	if lb.Status == lbStatusActive || lb.Status == lbStatusDegraded {
		lb.AppliedHash = hhash
	}
	return nil
}

// lbPairStatus sums the appliances up. ACTIVE: both answer and one holds the address. DEGRADED: one answers, or both
// hold the address, which happens when they cannot hear each other's adverts; nothing is changed for it, and it heals
// when they hear each other again (the one with the lower address yields, RFC 5798). DOWN: neither answers.
func lbPairStatus(apps []lbAppliance, stuck bool) (string, string) {
	up, holders, building, gone, applied := 0, 0, 0, 0, false
	var reasons []string
	for _, a := range apps {
		switch a.State {
		case lbAppUp:
			up++
		case lbAppBuilding:
			building++
		case lbAppGone:
			gone++
		}
		if a.HoldsVIP {
			holders++
		}
		if a.HAProxyHash != "" {
			applied = true
		}
		if a.State != lbAppUp {
			reasons = append(reasons, fmt.Sprintf("appliance %s: %s", a.ServerID, a.Reason))
		}
	}
	why := strings.Join(reasons, "; ")
	switch {
	case !applied && building > 0 && stuck:
		return lbStatusError, "The load balancer's VMs did not become ready. Their image needs the guest agent, and HAProxy, socat and keepalived installed or reachable from its repositories through the network's gateway."
	case !applied && building+up > 0:
		return lbStatusBuild, ""
	case holders > 1:
		return lbStatusDegraded, "Both appliances hold the address: they cannot hear each other. Traffic may reach either one until they do; nothing is changed automatically."
	case up == 2 && holders == 1 && len(apps) == 2:
		return lbStatusActive, ""
	case up == 2:
		return lbStatusDegraded, "Neither appliance holds the address yet: they are choosing which one does."
	case up == 1:
		return lbStatusDegraded, "One appliance carries the traffic, with no standby (" + why + "). Replace appliances builds the missing one again."
	case gone == len(apps):
		return lbStatusError, "Both appliances are gone. Replace appliances builds them again, or delete this load balancer."
	}
	return lbStatusDown, "No appliance answers (" + why + ")."
}

// pveNetworkPrefix is the prefix length of a public network's CIDR (32 when it cannot be read).
func pveNetworkPrefix(pn *externalservice.PVEPublicNetwork) int {
	if i := strings.IndexByte(pn.CIDR, '/'); i >= 0 {
		if n, err := strconv.Atoi(pn.CIDR[i+1:]); err == nil && n > 0 && n <= 32 {
			return n
		}
	}
	return 32
}

// lbPairHealth answers GET_HEALTH for a pair: the members as the appliance holding the address sees them, and both
// appliances' state as just read.
func (h *Handler) lbPairHealth(ctx context.Context, es *externalservice.ExternalService, proj *Project, lb *loadBalancer) (map[string]any, *httpx.HTTPError) {
	ps, err := h.pveFor(es)
	if err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
	}
	vip := ""
	if len(lb.VIPs) > 0 {
		vip = lb.VIPs[0]
	}
	type seen struct {
		app lbAppliance
		vm  *pve.ClusterResource
	}
	var apps []seen
	for _, a := range lb.Appliances {
		vm, err := ps.ResolveVM(ctx, proj.ID, a.ServerID)
		if err != nil || vm == nil || vm.NodeDown || pve.LBOfTags(vm.Tags) != lb.ID {
			a.Role, a.HoldsVIP = "", false
			apps = append(apps, seen{app: a})
			continue
		}
		if st, err := ps.ReadVRRPState(ctx, vm.Node, vm.VMID, vip); err == nil {
			a.Role, a.HoldsVIP = st.State, st.HoldsVIP
		}
		apps = append(apps, seen{app: a, vm: vm})
	}
	out := map[string]any{"result": []pve.LBMemberHealth{}}
	var list []lbAppliance
	for _, s := range apps {
		list = append(list, s.app)
	}
	out["appliances"] = list
	for _, s := range apps {
		if s.vm != nil && s.app.HoldsVIP && s.app.HAProxyHash != "" {
			health, err := ps.LBHealth(ctx, s.vm.Node, s.vm.VMID, lb.LBSpec)
			if err != nil {
				slog.Warn("load balancer: health", "project", proj.ID, "lb", lb.ID, "err", err)
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "The load balancer's active appliance did not answer")
			}
			out["result"] = health
			break
		}
	}
	return out, nil
}

// pveLBReplace starts REPLACE_APPLIANCES: new VMs from the current image, one at a time, the standby first, so the
// address keeps being served.
func (h *Handler) pveLBReplace(ctx context.Context, u *user.User, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) *httpx.HTTPError {
	release, ok, err := h.sdnLock.Wait(ctx, "lb-"+cr.ExternalID, lbLockMaxFor, lbLockWait)
	if err != nil {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Load balancers cannot be changed right now")
	}
	if !ok {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is being changed. Try again in a moment")
	}
	defer release()
	fresh, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, cr.ExternalID)
	if err != nil || fresh == nil {
		return httpx.NotFound("Resource not found")
	}
	lb, err := lbOfRow(fresh)
	if err != nil {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer cannot be read")
	}
	switch {
	case !lb.HA:
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "A load balancer on one VM has no appliances to replace. Create a new load balancer and delete this one")
	case lb.Status == lbStatusBuild:
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is still being created")
	case lbReplacing(lb, time.Now().UTC()):
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The appliances are already being replaced")
	}
	lb.Replacing = time.Now().UTC().Format(time.RFC3339)
	if err := h.lbSave(ctx, fresh, lb); err != nil {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Load balancers cannot be changed right now")
	}
	go h.lbReplace(context.WithoutCancel(ctx), u, es, proj, cr.ExternalID)
	return nil
}

// lbEdit changes a pair's row under its lock; edit answers false to leave it as it is.
func (h *Handler) lbEdit(ctx context.Context, es *externalservice.ExternalService, externalID string, edit func(*loadBalancer) bool) (*loadBalancer, error) {
	release, ok, err := h.sdnLock.Wait(ctx, "lb-"+externalID, lbLockMaxFor, lbLockMaxFor)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("load balancer %s stayed locked", externalID)
	}
	defer release()
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, externalID)
	if err != nil || cr == nil {
		return nil, fmt.Errorf("load balancer %s is gone", externalID)
	}
	lb, err := lbOfRow(cr)
	if err != nil {
		return nil, err
	}
	if edit(lb) {
		if err := h.lbSave(ctx, cr, lb); err != nil {
			return nil, err
		}
	}
	return lb, nil
}

// lbReplace is the replace: for each old appliance, the standby first, a planned switch away from it if it holds the
// address (keepalived stopped: the peer takes over in about 0.6 s), its VM destroyed, a new one built and waited for
// until it runs the current configs. A missing appliance is built first. It stops, with a message on the row, at the
// first step that fails; what was replaced stays replaced.
func (h *Handler) lbReplace(ctx context.Context, u *user.User, es *externalservice.ExternalService, proj *Project, externalID string) {
	ctx, cancel := context.WithTimeout(ctx, lbReplaceMaxFor)
	defer cancel()
	fail := ""
	defer func() {
		if _, err := h.lbEdit(context.WithoutCancel(ctx), es, externalID, func(lb *loadBalancer) bool {
			lb.Replacing = ""
			if fail != "" {
				lb.StatusMessage = "Replacing the appliances stopped: " + fail
			}
			return true
		}); err != nil {
			slog.Warn("load balancer: end replace", "lb", externalID, "err", err)
		}
	}()
	ps, err := h.pveFor(es)
	if err != nil {
		fail = err.Error()
		return
	}
	lb, err := h.lbEdit(ctx, es, externalID, func(*loadBalancer) bool { return false })
	if err != nil {
		fail = err.Error()
		return
	}
	var olds []string
	for _, a := range lbPushOrder(pointers(lb.Appliances)) {
		olds = append(olds, a.ServerID)
	}
	if len(lb.Appliances) < 2 {
		if fail = h.lbReplaceStep(ctx, u, es, proj, ps, externalID, ""); fail != "" {
			return
		}
	}
	for _, old := range olds {
		if fail = h.lbReplaceStep(ctx, u, es, proj, ps, externalID, old); fail != "" {
			return
		}
	}
}

func pointers(apps []lbAppliance) []*lbAppliance {
	out := make([]*lbAppliance, len(apps))
	for i := range apps {
		out[i] = &apps[i]
	}
	return out
}

// lbReplaceStep takes appliance old out of the pair (none: nothing to take out) and builds one in its place. It
// answers what went wrong, "" when the new appliance runs the current configs.
func (h *Handler) lbReplaceStep(ctx context.Context, u *user.User, es *externalservice.ExternalService, proj *Project, ps *pve.Service, externalID, old string) string {
	if cur, err := h.svc.GetProjectByID(ctx, proj.ID); err != nil || cur.Deleting() || cur.Status != StatusEnabled {
		return "the project is not enabled"
	}
	if old != "" {
		vms, err := lbVMs(ctx, ps)
		if err != nil {
			return err.Error()
		}
		lb, err := h.lbEdit(ctx, es, externalID, func(*loadBalancer) bool { return false })
		if err != nil {
			return err.Error()
		}
		if vm, ok := vms[old]; ok && !vm.NodeDown && pve.LBOfTags(vm.Tags) == lb.ID && vm.Status == "running" {
			if peer := slices.IndexFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID != old && a.State == lbAppUp }); peer >= 0 {
				// The planned switch: the peer takes the address before this VM goes.
				if err := ps.StopKeepalived(ctx, vm.Node, vm.VMID); err != nil {
					slog.Warn("load balancer: planned switch", "lb", externalID, "vm", old, "err", err)
				}
				time.Sleep(2 * time.Second)
			}
		}
		if _, err := h.lbEdit(ctx, es, externalID, func(lb *loadBalancer) bool {
			lb.Appliances = slices.DeleteFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == old })
			return true
		}); err != nil {
			return err.Error()
		}
		if vm, ok := vms[old]; ok && pve.LBOfTags(vm.Tags) == lb.ID && vm.Pool == ps.PoolFor(proj.ID) {
			if err := h.lbDestroyVM(ctx, es, proj, old); err != nil {
				return "the old appliance " + old + " could not be removed: " + err.Error()
			}
		}
	}
	lb, err := h.lbEdit(ctx, es, externalID, func(*loadBalancer) bool { return false })
	if err != nil {
		return err.Error()
	}
	app, err := h.lbBuildAppliance(ctx, u, proj, es, ps, lb)
	if err != nil {
		return "a new appliance could not be built: " + err.Error()
	}
	if _, err := h.lbEdit(ctx, es, externalID, func(lb *loadBalancer) bool {
		lb.Appliances = append(lb.Appliances, *app)
		return true
	}); err != nil {
		// The balancer went meanwhile: the new VM is nobody's.
		if derr := h.lbDestroyVM(ctx, es, proj, app.ServerID); derr != nil {
			slog.Error("load balancer: remove an appliance built for a balancer that went", "lb", externalID, "vm", app.ServerID, "err", derr)
		}
		return err.Error()
	}
	for {
		status, err := h.pveLBApply(ctx, es, proj, externalID)
		if err != nil {
			slog.Warn("load balancer: apply during replace", "lb", externalID, "err", err)
		}
		if status == "" {
			return "the load balancer was deleted"
		}
		lb, err := h.lbEdit(ctx, es, externalID, func(*loadBalancer) bool { return false })
		if err == nil {
			if i := slices.IndexFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == app.ServerID }); i >= 0 && lb.Appliances[i].State == lbAppUp {
				return ""
			}
		}
		select {
		case <-ctx.Done():
			return "the new appliance " + app.ServerID + " did not become ready"
		case <-time.After(lbConvergeEvery):
		}
	}
}

// lbDestroyVM destroys one appliance VM and its rows, as a server delete does.
func (h *Handler) lbDestroyVM(ctx context.Context, es *externalservice.ExternalService, proj *Project, vmid string) error {
	row, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, vmid)
	if err != nil {
		return err
	}
	owned := row != nil && row.ProjectID == proj.ID && row.Type == cloud.TypeServer
	if !owned { // built, and no row yet
		row = &cloud.CloudResource{ExternalID: vmid, ProjectID: proj.ID, ServiceID: es.ID, Type: cloud.TypeServer}
	}
	if err := h.pveDeleteResource(ctx, es, proj, row, true); err != nil {
		return err
	}
	if owned {
		return h.cloud.DeleteAndArchive(ctx, row, time.Now().UTC())
	}
	return nil
}
