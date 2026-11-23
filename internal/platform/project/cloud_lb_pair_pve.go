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
	// KeepalivedConfHash is keepalived.conf alone, as the appliance sums the file (KeepalivedHash is the config with
	// its support files, which no read answers). A pass compares it with the file's sum to tell a config changed
	// behind the product from one the product is about to change itself.
	KeepalivedConfHash string `json:"keepalivedConfHash,omitempty"`
	// LVSHash: the UDP listeners' files (pve.LVSHash). Empty on a pair that never had a UDP listener, whose
	// appliances hold nothing of them.
	LVSHash  string `json:"lvsHash,omitempty"`
	Role     string `json:"role,omitempty"` // keepalived's state: MASTER, BACKUP, FAULT, STOPPED, UNKNOWN
	HoldsVIP bool   `json:"holdsVip,omitempty"`
	State    string `json:"state,omitempty"` // up, down, building, gone
	Reason   string `json:"reason,omitempty"`
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
		if ap.left != nil { // a VM the failed build could not remove: no pair has a use for it
			if derr := h.lbDestroyVM(ctx, es, proj, strconv.Itoa(ap.left.VMID)); derr != nil {
				slog.Error("load balancer: remove an appliance whose build failed", "lb", lb.ID, "vm", ap.left.VMID, "err", derr)
			}
		}
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

// lbReasonPaused is an appliance's reason while its VM is paused.
const lbReasonPaused = "its VM is paused"

// lbPushOrder is the order configs reach a pair: the standby first, the one holding the address last, so a config
// that breaks HAProxy shows on the standby before it can reach the traffic.
//
// An appliance keepalived never ran on comes after one it runs on. The one that served alone (the first of a pair,
// or the one a replace left) is told of its new peer first and sends it its adverts from then on, so the new one
// starts as a standby that hears a master. The other way round the new one would hear nothing, take the addresses
// too, and the election that follows could move them (drill Q0b-1 of v0.11.1).
func lbPushOrder(apps []*lbAppliance) []*lbAppliance {
	rank := func(a *lbAppliance) int {
		n := 0
		if a.KeepalivedHash == "" {
			n = 2
		}
		if a.HoldsVIP {
			n++
		}
		return n
	}
	out := slices.Clone(apps)
	slices.SortStableFunc(out, func(a, b *lbAppliance) int { return rank(a) - rank(b) })
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

// LBPushOrder is lbPushOrder for a caller that holds a pair's state reads and no row: the indexes of states in the
// order a pass's pushes reach their appliances. The real-guest test asks it with a real appliance's own read
// (test/integration/lb_udp_test.go): the order is only as good as that read (drill Q1-2 of v0.11.1).
func LBPushOrder(states []pve.VRRPState) []int {
	apps := make([]*lbAppliance, len(states))
	for i, st := range states {
		apps[i] = &lbAppliance{ServerID: strconv.Itoa(i), KeepalivedHash: st.KeepalivedSum, HoldsVIP: st.HoldsVIP}
	}
	out := make([]int, len(apps))
	for i, a := range lbPushOrder(apps) {
		out[i], _ = strconv.Atoi(a.ServerID)
	}
	return out
}

// lbApplyPair brings a pair in line with its row. Order (C14): firewall and new VIPs in the ip filter sets on every
// running appliance first, so a VIP is in both sets before keepalived can take it; then per appliance, standby first,
// keepalived and HAProxy; then a VIP no longer served leaves the sets, once keepalived let go of it; then each one's
// state is read back. Rules and sets are compared on every pass, so one changed by hand is put back.
//
// A recorded hash is the product's word that the appliance runs that config, so each pass asks the appliance first:
// one whose HAProxy does not serve its config (stopped, or running another one, as after the package's own first
// start) or whose keepalived stopped is pushed again, which starts the service with the current config. They are the
// product's own services, and nobody else starts them. The same read says what the config files on the appliance hash
// to: a file that is not the one recorded (edited by hand, or lost) is pushed again too.
//
// UDP listeners are a third file and service of their own (pve.RenderLVS), after HAProxy on each appliance. A pair
// without one is asked and given nothing of it.
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
		case !listed && lbVMUnseen(ctx, ps, a.ServerID):
			a.State, a.Reason = lbAppDown, "the platform cannot see its VM"
			continue
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
			// A paused VM sends no adverts and its peer has taken the address: what the row says it held is of
			// before the pause (drill N3-6: the row read "both appliances hold the address").
			a.State, a.HoldsVIP, a.Reason = lbAppDown, false, lbReasonPaused
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
	// A member whose server left drops out of the config but stays in the row, marked, for the tenant. A cluster's
	// worker is checked against the cluster's addresses before it gets here (cloud_lb_cluster_pve.go).
	for i := range lb.Pools {
		for j := range lb.Pools[i].Members {
			m := &lb.Pools[i].Members[j]
			if m.Node {
				continue
			}
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
	// The certificates of the tls and https listeners: each appliance gets their files with its config, before it
	// (PushPairHAProxy), the standby first as the config itself.
	certs, ok := h.lbApplyCerts(ctx, proj, lb)
	if !ok {
		return nil
	}
	haproxy := lbRender(lb)
	hhash := lbHash(haproxy)
	lvs := pve.RenderLVS(lb.LBSpec, lb.VIPs) // "" without a UDP listener
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
	vip := ""
	if len(lb.VIPs) > 0 {
		vip = lb.VIPs[0]
	}
	seen := map[*lbAppliance]pve.VRRPState{} // as read before the pushes
	started := map[*lbAppliance]string{}     // what this pass found not running and pushed again
	for _, a := range running {
		if a.HAProxyHash == "" && a.KeepalivedHash == "" {
			continue
		}
		vmid, _ := strconv.Atoi(a.ServerID)
		if st, err := ps.ReadVRRPState(ctx, a.Node, vmid, vip); err == nil {
			seen[a] = st
			// The pushes below go to the standby first by what the appliance says now, not by what the row said at
			// the pass before: the address may have moved since, and a row v0.11.1-rc.1 wrote for a pair with a UDP
			// listener says that neither appliance holds it (drill Q1-2).
			a.Role, a.HoldsVIP = st.State, st.HoldsVIP || (vip == "" && st.State == "MASTER")
		}
	}
	// differs: the appliance said what its file hashes to, and it is not what the row records for it.
	differs := func(sum, recorded string) bool { return sum != "" && recorded != "" && sum != recorded }
	lvsWant := func(a *lbAppliance) string {
		if lvs == "" {
			return ""
		}
		return pve.LVSHash(lvs, a.TenantIface)
	}
	pushed := map[*lbAppliance]bool{}
	keepalivedCurrent := map[*lbAppliance]bool{}
	// refuse records a push an appliance did not take. The reason is the guest's own words, which may quote a line
	// of a config: it goes into the row's message for the tenant and not into the log.
	refuse := func(a *lbAppliance, what string, err error) {
		refused = err
		slog.Warn("load balancer: an appliance did not take a push; the reason is in the load balancer's status message", "lb", lb.ID, "vm", a.ServerID, "push", what)
	}
	for _, a := range lbPushOrder(running) {
		vmid, _ := strconv.Atoi(a.ServerID)
		st, known := seen[a]
		// keepalived stopped by a replace's planned switch may be started again here before its VM goes: it comes
		// back as BACKUP and stays so (nopreempt), the address has moved already.
		restartKeepalived := known && a.KeepalivedHash != "" && st.State == "STOPPED"
		reloadHAProxy := known && a.HAProxyHash != "" && (!st.Serving || differs(st.HAProxySum, a.HAProxyHash))
		reloadLVS := known && a.LVSHash != "" && lvs != "" && (!st.LVS || differs(st.LVSSum, a.LVSHash))
		if restartKeepalived || reloadHAProxy || reloadLVS {
			slog.Warn("load balancer: an appliance does not run what its row records; pushing it again", "lb", lb.ID, "vm", a.ServerID,
				"keepalived", st.State, "haproxyServing", st.Serving, "haproxyFile", !differs(st.HAProxySum, a.HAProxyHash),
				"udpServing", st.LVS || a.LVSHash == "", "udpFiles", !differs(st.LVSSum, a.LVSHash))
		}
		// keepalived on every appliance, with a peer or without: it is what holds the addresses and what lets HAProxy
		// bind them (ip_nonlocal_bind is one of its files). An appliance with no peer in the row, the first of a
		// pair until the second is built or the one a replace left, is its own master and serves (pve's lbNoPeer).
		if pn != nil {
			k := pve.LBKeepalived{RouterID: lbRouterID(lb.ID), Self: a.PrivateIP, TenantIface: a.TenantIface, PublicIface: a.PublicIface,
				VIPs: lb.VIPs, Prefix: pveNetworkPrefix(pn), Gateway: pn.Gateway}
			if peer := lbPeerOf(lb, a); peer != nil {
				k.Peer = peer.PrivateIP
			}
			conf, kerr := pve.RenderKeepalived(k)
			if kerr != nil {
				return kerr
			}
			khash := lbHash(conf + pve.LBPairFilesDigest(a.PublicIface))
			// A row of before KeepalivedConfHash whose keepalived is current: the config rendered now is the one it
			// records.
			if a.KeepalivedConfHash == "" && khash == a.KeepalivedHash {
				a.KeepalivedConfHash = lbHash(conf)
			}
			// The file on the appliance against the config the row records, not against the one this pass renders:
			// compared with that one, every planned change of the address list (a Service that came or went) read as
			// a config changed by hand, 16 times in 18 minutes on the lab (drill Q1-4 of v0.11.1).
			editedKeepalived := known && differs(st.KeepalivedSum, a.KeepalivedConfHash)
			if editedKeepalived {
				slog.Warn("load balancer: an appliance's keepalived config is not the one its row records; pushing it again", "lb", lb.ID, "vm", a.ServerID)
			}
			if khash != a.KeepalivedHash || restartKeepalived || editedKeepalived {
				pushed[a] = true
				if err := ps.PushKeepalived(ctx, a.Node, vmid, conf, a.PublicIface); err != nil {
					if errors.Is(err, pve.ErrLBNotReady) {
						a.State, a.Reason = lbAppBuilding, "it is installing HAProxy and keepalived"
						continue
					}
					refuse(a, "keepalived", err)
					continue
				}
				a.KeepalivedHash, a.KeepalivedConfHash = khash, lbHash(conf)
				if restartKeepalived {
					started[a] = "keepalived"
				}
			}
			keepalivedCurrent[a] = true
		}
		// HAProxy binds the addresses, and without keepalived's files it cannot bind one the appliance does not hold:
		// it would refuse its config (drill Q0b-1). keepalived is there on every pass but one whose public network
		// this project may no longer use.
		if a.KeepalivedHash == "" {
			a.State, a.Reason = lbAppBuilding, "its public network is not available to this project"
			continue
		}
		// A config an appliance refused goes to no other one in this pass: the standby comes first (lbPushOrder), so
		// what HAProxy does not take there never reaches the appliance that holds the address. One that does not
		// serve at all has nothing to lose and is pushed.
		if (hhash != a.HAProxyHash && refused == nil) || reloadHAProxy {
			pushed[a] = true
			if err := ps.PushPairHAProxy(ctx, a.Node, vmid, haproxy, certs...); err != nil {
				if errors.Is(err, pve.ErrLBNotReady) {
					a.State, a.Reason = lbAppBuilding, "it is installing HAProxy and keepalived"
					continue
				}
				refuse(a, "haproxy", err)
				continue
			}
			a.HAProxyHash = hhash
			if reloadHAProxy {
				started[a] = strings.TrimPrefix(started[a]+" and HAProxy", " and ")
			}
		}
		// The UDP listeners, last: their firewall rules are in, and a member change reloads this service alone.
		// stray: the appliance holds an LVS config the row records nothing of (a push whose answer was lost, and the
		// listener gone before the next pass). It would keep forwarding on with nothing looking after its table.
		stray := known && a.LVSHash == "" && st.LVSSum != "" && st.LVSSum != "-"
		switch want := lvsWant(a); {
		case want == "" && a.LVSHash == "" && !stray: // never had a UDP listener: nothing of it is on the appliance
		case want == "":
			pushed[a] = true
			if err := ps.RemoveLVS(ctx, a.Node, vmid); err != nil {
				refuse(a, "udp", err)
				continue
			}
			a.LVSHash = ""
		case (want != a.LVSHash && refused == nil) || reloadLVS:
			pushed[a] = true
			if err := ps.PushLVS(ctx, a.Node, vmid, lvs, a.TenantIface); err != nil {
				if errors.Is(err, pve.ErrLBNotReady) {
					a.State, a.Reason = lbAppBuilding, "it is installing what its UDP listeners need"
					continue
				}
				refuse(a, "udp", err)
				continue
			}
			a.LVSHash = want
			if reloadLVS {
				started[a] = strings.TrimPrefix(started[a]+" and the UDP service", " and ")
			}
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
	// Settled: every appliance that still exists runs the current keepalived and has dropped every VIP it no longer
	// serves from its set, so an address the row gave up may go back to the pool.
	lb.settled = refused == nil
	for i := range lb.Appliances {
		if a := &lb.Appliances[i]; a.State != lbAppGone && !keepalivedCurrent[a] {
			lb.settled = false
		}
	}
	for _, a := range running {
		if a.State == lbAppBuilding && a.HAProxyHash == "" {
			continue
		}
		// What the appliance says now: read again after a push, which changed it.
		st, known := seen[a]
		if !known || pushed[a] {
			vmid, _ := strconv.Atoi(a.ServerID)
			var err error
			if st, err = ps.ReadVRRPState(ctx, a.Node, vmid, vip); err != nil {
				a.State, a.Reason = lbAppDown, "it does not answer"
				continue
			}
		}
		a.Role, a.HoldsVIP = st.State, st.HoldsVIP
		if vip == "" { // a cluster's pair before its first Service, or after its last: the master holds the placeholder
			a.HoldsVIP = st.State == "MASTER"
		}
		switch {
		case !st.HAProxy:
			a.State, a.Reason = lbAppDown, "HAProxy is not running"
		case !st.Serving:
			a.State, a.Reason = lbAppDown, "HAProxy does not serve its configuration"
		case st.State != "MASTER" && st.State != "BACKUP":
			a.State, a.Reason = lbAppDown, "keepalived is "+strings.ToLower(st.State)
		case a.HAProxyHash != hhash:
			a.State, a.Reason = lbAppDown, "it runs an older configuration"
		case a.LVSHash != lvsWant(a):
			a.State, a.Reason = lbAppDown, "its UDP listeners are not loaded yet"
		case lvs != "" && !st.LVS:
			a.State, a.Reason = lbAppDown, "its UDP listeners are not served"
		default:
			a.State, a.Reason = lbAppUp, ""
		}
		if what := started[a]; what != "" && a.State != lbAppUp {
			a.Reason += "; " + what + " did not run its configuration and the platform loaded it again"
		}
	}
	lb.Status, lb.StatusMessage = lbPairStatus(lb.Appliances, lbStuck(cr), lbReplacing(lb, time.Now().UTC()))
	if refused != nil {
		lb.Status, lb.StatusMessage = lbStatusError, refused.Error()
	}
	// The row's hash says that an appliance serves this config (a cluster's Services are ready by it). The status
	// alone does not: both appliances holding the address is DEGRADED whatever they run.
	serves := slices.ContainsFunc(lb.Appliances, func(a lbAppliance) bool { return a.State == lbAppUp })
	if serves && (lb.Status == lbStatusActive || lb.Status == lbStatusDegraded) {
		lb.AppliedHash = lbAppliedHash(lb)
	}
	return nil
}

// lbPairStatus sums the appliances up. ACTIVE: both answer and one holds the address. DEGRADED: one answers, or both
// hold the address, which happens when they cannot hear each other's adverts; nothing is changed for it, and it heals
// when they hear each other again (the one with the lower address yields, RFC 5798). DOWN: neither answers.
//
// The message names what is wrong with the other appliance and what puts it right: Replace appliances only when one
// is missing, since everything else the platform's next pass repairs by itself.
func lbPairStatus(apps []lbAppliance, stuck, replacing bool) (string, string) {
	up, holders, building, gone, applied := 0, 0, 0, 0, false
	paused, holderUp := false, false
	var reasons []string
	for _, a := range apps {
		paused = paused || a.Reason == lbReasonPaused
		switch a.State {
		case lbAppUp:
			up++
			holderUp = holderUp || a.HoldsVIP
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
		if a.State != lbAppUp && a.Reason != "" {
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
		if why != "" {
			why = " (" + why + ")"
		}
		switch {
		case holders == 1 && !holderUp:
			// The one that holds the address is the one that is not right (it runs the config of before a change
			// it could not take yet, and serves that), and the standby is: "no standby" would be the wrong way
			// round (drill N3-4).
			return lbStatusDegraded, "The appliance that holds the address is the one that is not in order" + why + "; the other one is ready, as its standby. The platform puts it right at its next pass."
		case replacing:
			return lbStatusDegraded, "One appliance carries the traffic while the other is being replaced" + why + "."
		case paused:
			return lbStatusDegraded, "One appliance carries the traffic, with no standby" + why + ". A paused VM serves again when it is resumed in Proxmox; Replace appliances builds a new one."
		case gone > 0 || len(apps) < 2:
			return lbStatusDegraded, "One appliance carries the traffic, with no standby" + why + ". Replace appliances builds the missing one again."
		case building > 0:
			return lbStatusDegraded, "One appliance carries the traffic, with no standby" + why + ". The other one is still being prepared."
		}
		return lbStatusDegraded, "One appliance carries the traffic, with no standby" + why + ". The platform puts the other one right at its next pass; if this stays, Replace appliances builds a new one."
	case gone == len(apps):
		return lbStatusError, "Both appliances are gone. Replace appliances builds them again, or delete this load balancer."
	}
	return lbStatusDown, "No appliance answers (" + why + ")."
}

// lbAppliedHash is what a pair's row records when an appliance serves its whole config: the HAProxy config's hash,
// as ever, for a balancer without a UDP listener, and both files' with one.
func lbAppliedHash(lb *loadBalancer) string {
	haproxy := lbRender(lb)
	if lvs := pve.RenderLVS(lb.LBSpec, lb.VIPs); lvs != "" {
		return lbHash(haproxy + lvs)
	}
	return lbHash(haproxy)
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
			if a.State == lbAppUp && !st.Serving { // the row's word is older than this read
				a.State, a.Reason = lbAppDown, "HAProxy does not serve its configuration"
			}
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
			health, notLoaded, err := ps.LBHealth(ctx, s.vm.Node, s.vm.VMID, lb.LBSpec)
			if errors.Is(err, pve.ErrLBNoHAProxy) {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, lbNoHAProxyMsg)
			}
			if err != nil {
				slog.Warn("load balancer: health", "project", proj.ID, "lb", lb.ID, "err", err)
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "The load balancer's active appliance did not answer")
			}
			if lb.HasUDP() { // the UDP pools' members are in the kernel's table, not in HAProxy
				udp, udpNotLoaded, err := ps.LVSHealth(ctx, s.vm.Node, s.vm.VMID, lb.LBSpec, lb.VIPs)
				if err != nil {
					slog.Warn("load balancer: udp health", "project", proj.ID, "lb", lb.ID, "err", err)
					return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "The load balancer's active appliance did not answer")
				}
				health, notLoaded = append(health, udp...), append(notLoaded, udpNotLoaded...)
			}
			out["result"] = health
			lbHealthNotLoaded(out, notLoaded)
			break
		}
	}
	return out, nil
}

// lbHealthNotLoaded adds to a GET_HEALTH answer the listeners the appliance does not run, and says so: member states
// alone would read as nothing wrong.
func lbHealthNotLoaded(out map[string]any, notLoaded []string) {
	if len(notLoaded) == 0 {
		return
	}
	out["notLoaded"] = notLoaded
	out["message"] = "The appliance does not run this load balancer's configuration: listener " + strings.Join(notLoaded, ", ") +
		" is not loaded. The platform loads it again at its next pass."
}

// pveLBReplace starts REPLACE_APPLIANCES: new VMs from the current image, one at a time, the standby first, so the
// address keeps being served.
func (h *Handler) pveLBReplace(ctx context.Context, u *user.User, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) *httpx.HTTPError {
	release, ok, err := h.lbLock(ctx, cr.ExternalID, lbLockWait)
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

// lbEdit changes a pair's row under its lock; edit nil only reads it.
func (h *Handler) lbEdit(ctx context.Context, es *externalservice.ExternalService, externalID string, edit func(*loadBalancer)) (*loadBalancer, error) {
	if edit != nil {
		release, ok, err := h.lbLock(ctx, externalID, lbLockMaxFor)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("load balancer %s stayed locked", externalID)
		}
		defer release()
	}
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, externalID)
	if err != nil || cr == nil {
		return nil, fmt.Errorf("load balancer %s is gone", externalID)
	}
	lb, err := lbOfRow(cr)
	if err != nil {
		return nil, err
	}
	if edit != nil {
		edit(lb)
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
		if _, err := h.lbEdit(context.WithoutCancel(ctx), es, externalID, func(lb *loadBalancer) {
			lb.Replacing = ""
			if fail != "" {
				lb.StatusMessage = "Replacing the appliances stopped: " + fail
			}
		}); err != nil {
			slog.Warn("load balancer: end replace", "lb", externalID, "err", err)
		}
	}()
	ps, err := h.pveFor(es)
	if err != nil {
		fail = err.Error()
		return
	}
	lb, err := h.lbEdit(ctx, es, externalID, nil)
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
		lb, err := h.lbEdit(ctx, es, externalID, nil)
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
		if _, err := h.lbEdit(ctx, es, externalID, func(lb *loadBalancer) { lbTakeOut(lb, old) }); err != nil {
			return err.Error()
		}
		if vm, ok := vms[old]; ok && pve.LBOfTags(vm.Tags) == lb.ID && vm.Pool == ps.PoolFor(proj.ID) {
			if err := h.lbDestroyVM(ctx, es, proj, old); err != nil {
				return "the old appliance " + old + " could not be removed: " + err.Error()
			}
		}
		// The one that is left is read now: the build that follows takes minutes, and the row would show its role
		// of before the switch for all of them (drill N3-1: "BACKUP, holds no address" while it served).
		if _, err := h.pveLBApply(ctx, es, proj, externalID); err != nil {
			slog.Warn("load balancer: apply during replace", "lb", externalID, "err", err)
		}
	}
	lb, err := h.lbEdit(ctx, es, externalID, nil)
	if err != nil {
		return err.Error()
	}
	app, err := h.lbBuildAppliance(ctx, u, proj, es, ps, lb)
	if err != nil {
		return "a new appliance could not be built: " + err.Error()
	}
	if _, err := h.lbEdit(ctx, es, externalID, func(lb *loadBalancer) { lbPutIn(lb, *app) }); err != nil {
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
		lb, err := h.lbEdit(ctx, es, externalID, nil)
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

// lbTakeOut removes an appliance from the row of a pair being replaced, and lbPutIn adds the one built in its
// place. The row's status is of the appliances it lists, so both state it again: a row that said ACTIVE of two
// appliances does not keep saying it with one (drill N2-7).
func lbTakeOut(lb *loadBalancer, serverID string) {
	lb.Appliances = slices.DeleteFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == serverID })
	lbPairLegacyFields(lb)
	lb.Status, lb.StatusMessage = lbPairStatus(lb.Appliances, false, true)
}

func lbPutIn(lb *loadBalancer, app lbAppliance) {
	app.Reason = "it is booting"
	lb.Appliances = append(lb.Appliances, app)
	lbPairLegacyFields(lb)
	lb.Status, lb.StatusMessage = lbPairStatus(lb.Appliances, false, true)
}

// lbPairLegacyFields keeps the single appliance's fields (ServerID, PrivateIP) on an appliance of the pair: a replace
// destroys the VM they named, PVE gives its vmid to the next VM, and a VM the balancer names is never a member and,
// before its row exists, refused to its tenant (lbOwnerOf).
func lbPairLegacyFields(lb *loadBalancer) {
	if len(lb.Appliances) == 0 || slices.ContainsFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == lb.ServerID }) {
		return
	}
	lb.ServerID, lb.PrivateIP = lb.Appliances[0].ServerID, lb.Appliances[0].PrivateIP
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
