package project

// cloud_lb_pve.go — load balancers on a Proxmox provider. Each is a platform-owned HAProxy VM in the
// project's pool, with one NIC on a project network and one on a public network. The VM, its disk and
// its address are ordinary SERVER, VOLUME and FLOATING_IP rows, so the existing metering bills them
// and the project's quota counts them; the LOAD_BALANCER row only holds what the tenant configured.
// Config reaches the VM through the guest agent, whenever the row and what was last applied differ.
//
// A tenant names members by server only: the address comes from the platform's allocation table and
// must be on the balancer's own subnet, so no request can point a balancer at another project.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	lbDataKey = "loadbalancer"

	lbStatusBuild  = "BUILD"
	lbStatusActive = "ACTIVE"
	lbStatusDown   = "DOWN"
	lbStatusError  = "ERROR"

	// lbLockMaxFor is the longest anyone waits for a balancer's lock (lbEdit: a replace or a build that has to
	// write its step down).
	lbLockMaxFor = 3 * time.Minute
	lbLockWait   = 30 * time.Second
	// lbBuildTimeout is how long a new appliance gets to boot and install HAProxy.
	lbBuildTimeout = 20 * time.Minute
	// lbConvergeEvery paces the first config push after a create.
	lbConvergeEvery = 10 * time.Second
)

// lbLease is how long a balancer's lock (lb-<externalId>) lasts without its holder, who extends it while it works
// (lock.ShedLock.Hold). It was taken for three minutes whatever happened to the holder: a change the API was applying
// when it stopped went live 190 to 209 s later (v0.11.0-rc.2 drill, N3-9). With the cluster's converge lease
// (lbcLease) the same minute, both are free a minute after the stop.
var lbLease = time.Minute

// lbLock takes a balancer's lock, waiting up to wait for it.
func (h *Handler) lbLock(ctx context.Context, externalID string, wait time.Duration) (release func(), acquired bool, err error) {
	return h.sdnLock.Hold(ctx, "lb-"+externalID, lbLease, wait)
}

// loadBalancer is the row's data.loadbalancer.
type loadBalancer struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusMessage   string `json:"statusMessage,omitempty"`
	ServerID        string `json:"serverId,omitempty"`
	NetworkID       string `json:"networkId"` // the vnet
	PublicNetworkID string `json:"publicNetworkId"`
	PublicIP        string `json:"publicIp,omitempty"`
	PrivateIP       string `json:"privateIp,omitempty"`
	PrivatePrefix   int    `json:"privatePrefix,omitempty"`
	AppliedHash     string `json:"appliedHash,omitempty"`
	// HA: the balancer runs on two appliances (cloud_lb_pair_pve.go). A row without it is a single appliance.
	HA         bool          `json:"ha,omitempty"`
	NetworkRef string        `json:"networkRef,omitempty"` // the NETWORK the tenant picked, for a rebuild
	FlavorID   string        `json:"flavorId,omitempty"`
	VIPs       []string      `json:"vips,omitempty"` // public addresses, owned by the balancer (LBAddressOwner)
	Appliances []lbAppliance `json:"appliances,omitempty"`
	Replacing  string        `json:"replacing,omitempty"` // when REPLACE_APPLIANCES started, RFC 3339
	// Cluster: the balancer of a Kubernetes cluster's Services (cloud_lb_cluster_pve.go). Its spec is the platform's,
	// built from the state the cluster's controller sends; the tenant cannot change or delete it.
	Cluster *lbCluster `json:"cluster,omitempty"`
	pve.LBSpec
	// HeldSince is when a pass first found an appliance that could not take a change, RFC 3339; "" when none waits.
	// The appliances after it in the push order wait with it for lbHoldWindow from then (lbApplyPair).
	HeldSince string `json:"heldSince,omitempty"`
	// settled is the last pair apply's word that no appliance holds a VIP the row no longer lists.
	settled bool
	// waiting is the last apply's word that a change is still to reach an appliance that could not take it now: the
	// platform tries again by itself (lbRetry).
	waiting bool
}

// lbRender is the HAProxy config of the balancer: a cluster's gets the cluster's idle timeout (C5).
func lbRender(lb *loadBalancer) string {
	if lb.Cluster != nil {
		return pve.RenderClusterHAProxy(lb.LBSpec, lb.VIPs)
	}
	return pve.RenderHAProxyOn(lb.LBSpec, lb.VIPs)
}

// lbOfRow refuses a row with no id: an untagged VM's tag reads "" too, so such a row would match every one of them,
// as the owner of a tenant's server and as what a delete destroys.
func lbOfRow(cr *cloud.CloudResource) (*loadBalancer, error) {
	raw, err := json.Marshal(cr.Data[lbDataKey])
	if err != nil {
		return nil, err
	}
	var lb loadBalancer
	if err := json.Unmarshal(raw, &lb); err != nil {
		return nil, err
	}
	if lb.ID == "" {
		return nil, fmt.Errorf("load balancer row %s has no id", cr.ExternalID)
	}
	return &lb, nil
}

// lbsOfRows skips a row that cannot be read: it owns nothing.
func lbsOfRows(rows []cloud.CloudResource) []loadBalancer {
	var out []loadBalancer
	for i := range rows {
		if lb, err := lbOfRow(&rows[i]); err == nil {
			out = append(out, *lb)
		}
	}
	return out
}

// setLBRow stores lb on the row in the JSON-stable shape the cache needs.
func setLBRow(cr *cloud.CloudResource, lb *loadBalancer) error {
	raw, err := json.Marshal(lb)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	now := time.Now().UTC()
	cr.Data = map[string]any{lbDataKey: m}
	cr.UpdatedAt = &now
	return nil
}

// lbSave writes a row that must still exist: a delete that won the race stays deleted.
func (h *Handler) lbSave(ctx context.Context, cr *cloud.CloudResource, lb *loadBalancer) error {
	if err := setLBRow(cr, lb); err != nil {
		return err
	}
	_, err := h.cloud.Update(ctx, cr)
	return err
}

// lbSiteWarnings says, before a load balancer is asked for, what this site lacks for one: each line is something
// the create refuses, and only the operator can change. The page shows them instead of letting the user find out one
// refusal at a time (UX 3 of the production acceptance). A catalog that cannot be read warns of nothing.
func (h *Handler) lbSiteWarnings(ctx context.Context, proj *Project, es *externalservice.ExternalService) []string {
	out := []string{}
	hasPublic := slices.ContainsFunc(es.PVENetworkConfig().PublicNetworks, func(pn externalservice.PVEPublicNetwork) bool {
		return publicNetworkAllowed(proj, pn.ID)
	})
	if !hasPublic {
		out = append(out, "No public network is available to this project, and a load balancer takes its public address from one. Ask your operator to add a public network to the provider and to allow it for this project.")
	} else if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{FloatingIPs: 1}); herr != nil {
		out = append(out, "A load balancer takes one public address, and this project cannot take another ("+herr.Msg+"). Release an address or ask your operator to raise the public address quota.")
	}
	if h.pveFor == nil {
		return out
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return out
	}
	found, err := ps.C.Templates(ctx)
	if err != nil {
		return out
	}
	image := es.PVELoadBalancerImage()
	if !slices.ContainsFunc(es.PVEImageCatalog(found), func(t externalservice.PVETemplate) bool { return t.Slug == image }) {
		out = append(out, "The image a load balancer runs on, "+image+", is not in this site's image catalog. Ask your operator to add it, or to name another Debian family image in the provider setting lbImage.")
	}
	return out
}

func (h *Handler) pveCreateLoadBalancer(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, data map[string]any) {
	ctx := r.Context()
	name := strAny(data["name"])
	if !pve.LBNameOK(name) {
		h.fail(w, httpx.BadRequest("Load balancer names "+pve.LBNameRule))
		return
	}
	if h.publicIPs == nil || h.sdnLock == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Load balancers are not configured")
		return
	}
	pn := pveAllowedPublicNetwork(es, proj, strAny(data["publicNetworkId"]))
	if pn == nil {
		h.fail(w, httpx.BadRequest("Public network is not available"))
		return
	}
	if pn.Gateway == "" { // the public NIC carries the default route, see pvePublicNetworkAction
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "Public network "+pn.ID+" has no gateway"))
		return
	}
	bridge := h.pveResolveBridge(ctx, es, proj, map[string]any{"networkIds": []any{strAny(data["networkId"])}})
	if bridge == "" {
		h.fail(w, httpx.BadRequest("Select one of this project's networks for the load balancer"))
		return
	}
	if ha, set := data["ha"].(bool); ha || !set { // a new balancer gets the pair unless the tenant says no
		h.pveCreateLBPair(w, r, u, proj, es, data, pn, bridge)
		return
	}
	if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{FloatingIPs: 1}); herr != nil {
		h.fail(w, herr)
		return
	}
	id, err := randomHex(6)
	if err != nil {
		h.fail(w, err)
		return
	}
	// The row comes first: it is what a crash during the build leaves to find and delete the VM by.
	now := time.Now().UTC()
	row := &cloud.CloudResource{
		ExternalID: "lb-" + id, ServiceID: es.ID, ProjectID: proj.ID, Region: h.pveRegion(proj, es),
		Type: cloud.TypeLoadBalancer, CreatedAt: &now, Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now},
	}
	lb := &loadBalancer{ID: id, Name: name, Status: lbStatusBuild, NetworkID: bridge, PublicNetworkID: pn.ID}
	if err := setLBRow(row, lb); err != nil {
		h.fail(w, err)
		return
	}
	if row, err = h.cloud.Insert(ctx, row); err != nil {
		h.fail(w, err)
		return
	}
	appliance := &pveAppliance{
		loadBalancerID: id,
		beforeBoot: func(ctx context.Context, ps *pve.Service, node string, vmid int) error {
			return h.lbPlugPublic(ctx, ps, es, proj, pn, lb, node, vmid)
		},
	}
	saved, berr := h.pveBuildServer(ctx, u, proj, es, map[string]any{
		"name":       "lb-" + name,
		"flavorId":   strAny(data["flavorId"]),
		"imageId":    es.PVELoadBalancerImage(),
		"networkIds": []any{strAny(data["networkId"])},
	}, appliance)
	if berr != nil {
		if left := appliance.left; left != nil {
			// It may be up with these addresses, so they stay held. The row stays too: it is how the tenant gets rid
			// of the VM, whose tag hides it from Servers, and the sync brings it in line if it is healthy after all.
			lb.ServerID = strconv.Itoa(left.VMID)
			lb.Status, lb.StatusMessage = lbStatusError, "The load balancer could not be built and its VM could not be removed. Delete this load balancer and create it again."
			if err := h.lbSave(ctx, row, lb); err != nil {
				slog.Error("load balancer: save row after a failed build", "project", proj.ID, "err", err)
			}
			h.fail(w, berr)
			return
		}
		if lb.ServerID != "" {
			if err := h.publicIPs.ReleaseServer(ctx, es.ID, lb.ServerID); err != nil {
				slog.Error("load balancer: release address after a failed build", "project", proj.ID, "err", err)
			}
		}
		if err := h.cloud.DeleteAndArchive(ctx, row, time.Now().UTC()); err != nil {
			slog.Error("load balancer: drop row after a failed build", "project", proj.ID, "err", err)
		}
		h.fail(w, berr)
		return
	}
	lb.ServerID = saved.ExternalID
	if err := h.lbSave(ctx, row, lb); err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", row)
	go h.lbConverge(context.WithoutCancel(ctx), es, proj, row.ExternalID)
	httpx.OK(w, *row)
}

// lbPlugPublic gives the appliance its public NIC before first boot, so it needs no reboot, which a
// server's public NIC does. Same rules as pvePublicNetworkAction: pinned before it is plugged, and the
// public side takes the default route.
func (h *Handler) lbPlugPublic(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, proj *Project, pn *externalservice.PVEPublicNetwork, lb *loadBalancer, node string, vmid int) error {
	mgmt, err := ps.C.ManagementBridges(ctx, node)
	if err != nil {
		return err
	}
	if _, bad := mgmt[pn.ID]; bad {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Public network "+pn.ID+" is the Proxmox management bridge")
	}
	id := strconv.Itoa(vmid)
	lb.ServerID = id
	ip, err := h.publicIPs.AllocateBeforeRow(ctx, es.ID, proj.ID, id, publicNIC, *pn)
	if err != nil {
		return err
	}
	held, err := h.publicIPs.ForServer(ctx, es.ID, id)
	if err != nil {
		return err
	}
	for _, row := range held {
		if row.NIC == "net0" && row.ProjectID == proj.ID {
			lb.PrivateIP, lb.PrivatePrefix = row.IP, row.Prefix
		}
	}
	if lb.PrivateIP == "" {
		return fmt.Errorf("load balancer: vm %d has no address on its network", vmid)
	}
	if err := ps.SetIPFilter(ctx, node, vmid, "net1", []string{ip.IP + "/32"}); err != nil {
		return err
	}
	up, err := ps.C.SetVMConfig(ctx, node, vmid, map[string]string{
		"net1":      "virtio,bridge=" + pn.ID + ",firewall=1",
		"ipconfig1": "ip=" + ip.IP + "/" + strconv.Itoa(ip.Prefix) + ",gw=" + pn.Gateway,
		"ipconfig0": "ip=" + lb.PrivateIP + "/" + strconv.Itoa(lb.PrivatePrefix),
	})
	if err == nil {
		err = ps.C.WaitTask(ctx, up)
	}
	if err != nil {
		return err
	}
	lb.PublicIP = ip.IP
	return nil
}

// lbConverge pushes the first config as soon as the appliance can take it. The sync's reconcile is the
// backstop if this process dies first.
func (h *Handler) lbConverge(ctx context.Context, es *externalservice.ExternalService, proj *Project, externalID string) {
	ctx, cancel := context.WithTimeout(ctx, lbBuildTimeout)
	defer cancel()
	for {
		// A project suspended meanwhile: the sync's reconcile takes over after the resume.
		if cur, err := h.svc.GetProjectByID(ctx, proj.ID); err == nil && (cur.Deleting() || cur.Status != StatusEnabled) {
			return
		}
		status, err := h.pveLBApply(ctx, es, proj, externalID)
		if err != nil {
			slog.Warn("load balancer: apply", "project", proj.ID, "lb", externalID, "err", err)
		}
		if status == lbStatusActive || status == "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(lbConvergeEvery):
		}
	}
}

// ReconcileLoadBalancers is the sync's pass over a provider's balancers: a member's server that left,
// a config not yet pushed, an appliance someone stopped.
func (h *Handler) ReconcileLoadBalancers(ctx context.Context, _ *pve.Service, es *externalservice.ExternalService) error {
	if h.sdnLock == nil {
		return nil
	}
	rows, err := h.cloud.FindByServiceAndType(ctx, es.ID, cloud.TypeLoadBalancer)
	if err != nil {
		return err
	}
	for i := range rows {
		// A cluster's balancer converges from its cluster's state, which checks the project itself. In the background:
		// a pass may build an appliance (up to lbBuildTimeout), which must not hold up the rest of the sync.
		if lb, err := lbOfRow(&rows[i]); err == nil && lb.Cluster != nil && h.lbGW != nil {
			go h.lbClusterConverge(context.WithoutCancel(ctx), lb.Cluster.ID, true)
			continue
		}
		proj, err := h.svc.GetProjectByID(ctx, rows[i].ProjectID)
		if err != nil {
			slog.Warn("load balancer: reconcile project", "project", rows[i].ProjectID, "err", err)
			continue
		}
		if proj.Deleting() || proj.Status != StatusEnabled {
			continue
		}
		if _, err := h.pveLBApply(ctx, es, proj, rows[i].ExternalID); err != nil {
			slog.Warn("load balancer: reconcile", "project", proj.ID, "lb", rows[i].ExternalID, "err", err)
		}
	}
	return nil
}

// lbVMs indexes the cluster's VMs by vmid.
func lbVMs(ctx context.Context, ps *pve.Service) (map[string]pve.ClusterResource, error) {
	rows, err := ps.C.ClusterResources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	out := make(map[string]pve.ClusterResource, len(rows))
	for _, r := range rows {
		if r.Type == "qemu" {
			out[strconv.Itoa(r.VMID)] = r
		}
	}
	return out, nil
}

// lbVMUnseen says that an appliance missing from the VM list is not known to be gone: a guest the platform's token
// does not see still holds its VMID (the token lost sight of the pool, or the VM was moved out of it), or Proxmox
// did not say. Only a free VMID proves a VM gone, as for a SERVER row (docs/vm-inventory-safety.md): the list is
// filtered by what the token may see, and a pass that read a filtered list as an absence would tell the tenant to
// delete, or to replace, a balancer that is serving. The services sync runs this pass whether or not it could prove
// the project's inventory, so the pass proves its one absence itself.
func lbVMUnseen(ctx context.Context, ps *pve.Service, serverID string) bool {
	id, err := strconv.Atoi(serverID)
	if err != nil {
		return false
	}
	free, err := ps.C.VMIDFree(ctx, id)
	return err != nil || !free
}

// lbMemberAddress returns the address of a member's server if it may serve this balancer, "" if not.
// stamp "" accepts any VM, and is what a new member passes. It also returns the VM's current stamp.
func (h *Handler) lbMemberAddress(ctx context.Context, es *externalservice.ExternalService, proj *Project, lb *loadBalancer, pool string, vms map[string]pve.ClusterResource, serverID, stamp string) (string, string, error) {
	vm, listed := vms[serverID]
	cur, ok := lbMemberVM(vm, listed, pool, serverID, lb.ServerID, stamp)
	if !ok {
		return "", "", nil
	}
	held, err := h.publicIPs.ForServer(ctx, es.ID, serverID)
	if err != nil {
		return "", "", err
	}
	return lbMemberIP(held, proj.ID, lb), cur, nil
}

// lbMemberVM: a VM of this project's pool, not a template, not any balancer's appliance, and the very VM it was
// added as, since PVE reuses vmids.
func lbMemberVM(vm pve.ClusterResource, listed bool, pool, serverID, applianceID, stamp string) (string, bool) {
	if !listed || vm.Pool != pool || vm.Template == 1 || serverID == applianceID || pve.LBOfTags(vm.Tags) != "" {
		return "", false
	}
	cur := pve.CreatedStamp(vm.Tags)
	if vm.NodeDown { // a dead node's row has no tags left: not gone, and not to be judged
		cur = stamp
	}
	return cur, stamp == "" || cur == stamp
}

// lbMemberIP is the server's platform-allocated address on the balancer's network and subnet. Never one
// the guest reports: a tenant controls that.
func lbMemberIP(held []PublicIP, projectID string, lb *loadBalancer) string {
	subnet, err := netip.ParsePrefix(lb.PrivateIP + "/" + strconv.Itoa(lb.PrivatePrefix))
	if err != nil {
		return ""
	}
	for _, row := range held {
		if row.NIC != "net0" || row.ProjectID != projectID || row.NetworkID != lb.NetworkID {
			continue
		}
		if addr, err := netip.ParseAddr(row.IP); err == nil && subnet.Masked().Contains(addr) {
			return row.IP
		}
	}
	return ""
}

// lbNoHAProxyMsg answers GET_HEALTH when the appliance runs and its HAProxy does not.
const lbNoHAProxyMsg = "HAProxy is not running on the load balancer's VM. The platform starts it at its next pass"

// lbStuck: older than any create can take.
func lbStuck(cr *cloud.CloudResource) bool {
	return cr.CreatedAt != nil && time.Since(*cr.CreatedAt) > lbBuildTimeout
}

func lbHash(config string) string {
	sum := sha256.Sum256([]byte(config))
	return hex.EncodeToString(sum[:])
}

// pveLBApply brings one balancer in line with its row, and returns the status it ends on ("" = no such
// row). It is the only writer of what an appliance runs, one at a time per balancer. A change an appliance could not
// take now is tried again by the platform itself, every 20 s for two minutes (lbRetry), and after that at the next
// services sync or change: the caller is answered at once, and the row says what waits and when it is tried again
// (lbRetrySays).
func (h *Handler) pveLBApply(ctx context.Context, es *externalservice.ExternalService, proj *Project, externalID string) (string, error) {
	status, waiting, err := h.lbApplyOnce(ctx, es, proj, externalID)
	if waiting {
		h.lbRetry(externalID, func(ctx context.Context) bool {
			// It builds nothing, so it is bound as an apply's lock is.
			ctx, cancel := context.WithTimeout(ctx, lbLockMaxFor)
			defer cancel()
			// A project suspended or deleted meanwhile: the sync's reconcile takes over after the resume.
			if h.svc != nil {
				if cur, err := h.svc.GetProjectByID(ctx, proj.ID); err != nil || cur.Deleting() || cur.Status != StatusEnabled {
					return false
				}
			}
			_, waiting, err := h.lbApplyOnce(ctx, es, proj, externalID)
			if err != nil {
				slog.Warn("load balancer: retry of a change that waits", "project", proj.ID, "lb", externalID, "err", err)
			}
			return waiting
		})
	}
	return status, err
}

// lbApplyOnce is one apply under the balancer's lock. waiting: it left a change an appliance could not take now.
func (h *Handler) lbApplyOnce(ctx context.Context, es *externalservice.ExternalService, proj *Project, externalID string) (status string, waiting bool, err error) {
	release, ok, err := h.lbLock(ctx, externalID, lbLockWait)
	if err != nil {
		return lbStatusBuild, false, err
	}
	if !ok {
		return lbStatusBuild, false, nil // another apply is on it
	}
	defer release()
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, externalID)
	if err != nil {
		return lbStatusBuild, false, err
	}
	if cr == nil || cr.Type != cloud.TypeLoadBalancer || cr.ProjectID != proj.ID {
		return "", false, nil
	}
	lb, err := lbOfRow(cr)
	if err != nil {
		return lbStatusError, false, err
	}
	before, _ := json.Marshal(lb)
	err = h.lbApplyLocked(ctx, es, proj, cr, lb)
	if after, _ := json.Marshal(lb); string(after) != string(before) {
		if serr := h.lbSave(ctx, cr, lb); serr != nil && err == nil {
			err = serr
		}
	}
	return lb.Status, lb.waiting, err
}

func (h *Handler) lbApplyLocked(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource, lb *loadBalancer) error {
	if lb.HA {
		return h.lbApplyPair(ctx, es, proj, cr, lb)
	}
	lb.waiting = false
	ps, err := h.pveFor(es)
	if err != nil {
		return err
	}
	vms, err := lbVMs(ctx, ps)
	if err != nil {
		return err // nothing is known, so nothing changes
	}
	vm, listed := vms[lb.ServerID]
	if lb.ServerID != "" && !listed && lbVMUnseen(ctx, ps, lb.ServerID) {
		lb.Status, lb.StatusMessage = lbStatusDown, "The platform cannot see the load balancer's VM, and changes nothing until it can."
		return nil
	}
	if lb.ServerID == "" || !listed || vm.Pool != ps.PoolFor(proj.ID) || (!vm.NodeDown && pve.LBOfTags(vm.Tags) != lb.ID) {
		if lb.ServerID != "" || lbStuck(cr) { // no server yet: the create is still on it
			lb.Status, lb.StatusMessage = lbStatusError, "The load balancer's VM is gone. Delete this load balancer and create it again."
		}
		return nil
	}
	if vm.NodeDown {
		lb.Status, lb.StatusMessage = lbStatusDown, "The host of this load balancer is down"
		return nil
	}
	// cluster/resources lags a just-started VM by seconds; what to do here depends on the state itself.
	if st, err := ps.C.GetVMStatus(ctx, vm.Node, vm.VMID); err != nil {
		return err
	} else if vm.Status = st.Status; st.QMPStatus == "paused" {
		vm.Status = "paused"
	}
	switch vm.Status {
	case "running":
	case "stopped":
		lb.Status, lb.StatusMessage = lbStatusDown, "The load balancer's VM is stopped; starting it"
		if up, serr := ps.C.StartVM(ctx, vm.Node, vm.VMID); serr != nil {
			return serr
		} else if up != "" {
			return ps.C.WaitTask(ctx, up)
		}
		return nil
	default: // paused with a suspended project, or in transition
		lb.Status, lb.StatusMessage = lbStatusDown, "The load balancer's VM is "+vm.Status
		return nil
	}
	// A member whose server left drops out of the config but stays in the row, marked, for the tenant.
	pool := ps.PoolFor(proj.ID)
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
	certs, ok := h.lbApplyCerts(ctx, proj, lb)
	if !ok {
		return nil
	}
	config := pve.RenderHAProxy(lb.LBSpec)
	hash := lbHash(config)
	if hash == lb.AppliedHash {
		// The recorded hash is the product's word that the appliance serves this config, so ask it. One that does
		// not (HAProxy stopped, or running the package's own config) is pushed again below, which starts HAProxy
		// with the config: it is the product's own service. No answer says nothing new. The same question covers
		// the certificate files the config names: HAProxy serves from memory while one is damaged or gone and would
		// not start again, so the push below writes it again while nothing is down yet.
		serving, why, err := ps.HAProxyServing(ctx, vm.Node, vm.VMID, hash, certs...)
		switch {
		case err != nil:
			slog.Warn("load balancer: state", "project", proj.ID, "lb", lb.ID, "err", err)
			return nil
		case serving:
			lb.Status, lb.StatusMessage = lbStatusActive, ""
			return nil
		}
		slog.Warn("load balancer: the appliance does not serve what its row records; pushing it again", "project", proj.ID, "lb", lb.ID, "why", why)
		lb.Status, lb.StatusMessage = lbStatusDown, why+". The platform is loading the configuration again."
	}
	if err := ps.SetLBRules(ctx, vm.Node, vm.VMID, pve.LBListenerRules(lb.Listeners, nil)); err != nil {
		return err
	}
	// The log has one line for the push: taken, or held or refused with the class of the reason. The reason itself
	// is the guest's own words, which may quote a config: it goes into the row's message and not into the log.
	wrote, err := ps.PushHAProxy(ctx, vm.Node, vm.VMID, config, certs...)
	if wrote > 0 {
		slog.Info("load balancer: push taken", "lb", lb.ID, "vm", lb.ServerID, "push", "certificates", "role", "single", "position", 1, "of", 1)
	}
	if err != nil {
		held := errors.Is(err, pve.ErrLBNotReady)
		if held {
			slog.Warn("load balancer: push held", "lb", lb.ID, "vm", lb.ServerID, "push", "haproxy", "role", "single", "class", pve.LBHoldClass(err))
		} else {
			slog.Warn("load balancer: an appliance did not take a push; the reason is in the load balancer's status message", "lb", lb.ID, "vm", lb.ServerID,
				"push", "haproxy", "role", "single", "class", pve.LBHoldClass(err))
		}
		switch {
		case !held:
			lb.Status, lb.StatusMessage = lbStatusError, err.Error()
		case lbStuck(cr) && lb.AppliedHash == "":
			lb.Status, lb.StatusMessage = lbStatusError, "The load balancer's VM did not become ready. Its image needs the guest agent, and HAProxy and socat installed or reachable from its repositories."
		case lb.AppliedHash == "":
			lb.Status, lb.StatusMessage = lbStatusBuild, ""
		case hash != lb.AppliedHash: // the last config is served meanwhile; one found not served keeps its message
			lb.StatusMessage = "A change waits for the load balancer's VM: " + lbHeldWhy(err) + ". The configuration of before the change is served meanwhile; " + lbRetrySays() + "."
			lb.waiting = true
		default: // the config was found not served and cannot be loaded now: tried again too
			lb.waiting = true
		}
		return nil
	}
	slog.Info("load balancer: push taken", "lb", lb.ID, "vm", lb.ServerID, "push", "haproxy", "role", "single", "position", 1, "of", 1)
	lb.AppliedHash, lb.Status, lb.StatusMessage = hash, lbStatusActive, ""
	return nil
}

// pveLoadBalancerAction handles POST /cloud/{id}/action on a load balancer.
func (h *Handler) pveLoadBalancerAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	ctx := r.Context()
	switch action {
	case "UPDATE_CONFIG":
		if herr := lbClusterGuard(cr); herr != nil {
			h.fail(w, herr)
			return true
		}
		if herr := h.pveLBUpdateConfig(ctx, es, proj, cr, data); herr != nil {
			h.fail(w, herr)
			return true
		}
		if _, err := h.pveLBApply(ctx, es, proj, cr.ExternalID); err != nil {
			slog.Warn("load balancer: apply", "project", proj.ID, "lb", cr.ExternalID, "err", err)
		}
		fresh, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, cr.ExternalID)
		if err != nil || fresh == nil {
			h.fail(w, httpx.NotFound("Resource not found"))
			return true
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, fresh)
		httpx.OK(w, *fresh)
		return true
	case "REPLACE_APPLIANCES":
		if herr := h.pveLBReplace(ctx, u, es, proj, cr); herr != nil {
			h.fail(w, herr)
			return true
		}
		fresh, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, cr.ExternalID)
		if err != nil || fresh == nil {
			h.fail(w, httpx.NotFound("Resource not found"))
			return true
		}
		h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, fresh)
		httpx.OK(w, *fresh)
		return true
	case "GET_HEALTH":
		lb, err := lbOfRow(cr)
		if err != nil {
			h.fail(w, err)
			return true
		}
		if lb.HA {
			out, herr := h.lbPairHealth(ctx, es, proj, lb)
			if herr != nil {
				h.fail(w, herr)
				return true
			}
			httpx.OK(w, out)
			return true
		}
		if lb.AppliedHash == "" {
			httpx.OK(w, map[string]any{"result": []pve.LBMemberHealth{}})
			return true
		}
		ps, err := h.pveFor(es)
		if err != nil {
			httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
			return true
		}
		vm, err := ps.ResolveVM(ctx, proj.ID, lb.ServerID)
		if err != nil || vm == nil || vm.NodeDown || pve.LBOfTags(vm.Tags) != lb.ID {
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "The load balancer's VM cannot be reached"))
			return true
		}
		health, notLoaded, err := ps.LBHealth(ctx, vm.Node, vm.VMID, lb.LBSpec)
		if errors.Is(err, pve.ErrLBNoHAProxy) { // the VM answered, HAProxy did not (drill N3-2)
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, lbNoHAProxyMsg))
			return true
		}
		if err != nil {
			slog.Warn("load balancer: health", "project", proj.ID, "lb", cr.ExternalID, "err", err)
			h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "The load balancer's VM did not answer"))
			return true
		}
		out := map[string]any{"result": health}
		lbHealthNotLoaded(out, notLoaded)
		httpx.OK(w, out)
		return true
	}
	return false
}

// lbUDPNeedsPair is why a balancer cannot take spec: UDP listeners are served by keepalived and LVS, which only an
// appliance pair runs. "" when it can.
func lbUDPNeedsPair(spec pve.LBSpec, ha bool) string {
	if spec.HasUDP() && !ha {
		return "UDP needs a high-availability load balancer, on two appliances: this one runs on one VM. Create a load balancer with high availability for UDP listeners"
	}
	return ""
}

// lbSpecOfRequest reads the body of UPDATE_CONFIG: listeners and pools with every option of pve.LBSpec. A body from
// before v0.11.1, with the protocol on the pool alone, reads as it did. What the balancer cannot do is a 400 with
// the reason, a protocol the model knows and no appliance serves yet among them.
func lbSpecOfRequest(data map[string]any) (pve.LBSpec, error) {
	var spec pve.LBSpec
	raw, err := json.Marshal(data)
	if err != nil {
		return spec, err
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return spec, httpx.BadRequest("Listeners and pools do not have the expected shape")
	}
	if msg := spec.Validate(); msg != "" {
		return spec, httpx.BadRequest(msg)
	}
	return spec, nil
}

// pveLBUpdateConfig replaces a balancer's listeners, pools and members after checking every member.
func (h *Handler) pveLBUpdateConfig(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource, data map[string]any) error {
	spec, err := lbSpecOfRequest(data)
	if err != nil {
		return err
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox client not ready")
	}
	release, ok, err := h.lbLock(ctx, cr.ExternalID, lbLockWait)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is being changed. Try again in a moment")
	}
	defer release()
	fresh, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, cr.ExternalID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return httpx.NotFound("Resource not found")
	}
	lb, err := lbOfRow(fresh)
	if err != nil {
		return err
	}
	if lb.PrivateIP == "" {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is still being created")
	}
	vms, err := lbVMs(ctx, ps)
	if err != nil {
		return upstreamErr(computeBackend, err)
	}
	pool := ps.PoolFor(proj.ID)
	for i := range spec.Pools {
		for j := range spec.Pools[i].Members {
			m := &spec.Pools[i].Members[j]
			addr, stamp, err := h.lbMemberAddress(ctx, es, proj, lb, pool, vms, m.ServerID, "")
			if err != nil {
				return err
			}
			if addr == "" {
				// One answer for "not yours" and "does not exist": a tenant learns nothing about the vmid.
				return httpx.BadRequest("Server " + m.ServerID + " is not a server on this load balancer's network and subnet")
			}
			m.Address, m.Stamp = addr, stamp
		}
	}
	if why := lbUDPNeedsPair(spec, lb.HA); why != "" {
		return httpx.BadRequest(why)
	}
	// The certificates a listener names must be the project's; their files' names go into the spec for the size check.
	if _, why, err := h.lbResolveCerts(ctx, proj.ID, &spec, false); err != nil {
		return err
	} else if why == lbCertNoKeyMsg {
		return httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, why)
	} else if why != "" {
		return httpx.BadRequest(why)
	}
	if len(pve.RenderHAProxyOn(spec, lb.VIPs)) > pve.LBMaxConfig || len(pve.RenderLVS(spec, lb.VIPs)) > pve.LBMaxConfig {
		return httpx.BadRequest("This configuration is too large for a load balancer. Use fewer members")
	}
	lb.LBSpec = spec
	return h.lbSave(ctx, fresh, lb)
}

// pveDeleteLoadBalancer destroys the appliance and what hangs off it; the caller archives the row. Only
// a VM carrying this balancer's tag goes: the stored vmid alone could name another VM since.
func (h *Handler) pveDeleteLoadBalancer(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	lb, err := lbOfRow(cr)
	if err != nil {
		return err
	}
	// No server saved yet: the create is still running, and a delete now would find no VM or miss it, then leave the
	// create to finish a VM with no balancer. A create that died frees the row once it cannot be running any more. A
	// pair's create is running until it has both appliances.
	if lb.Status == lbStatusBuild && (lb.ServerID == "" || (lb.HA && len(lb.Appliances) < 2)) && !lbStuck(cr) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is still being created. Try again when it has its VM")
	}
	if lb.HA && lbReplacing(lb, time.Now().UTC()) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The appliances of this load balancer are being replaced. Try again when that is done")
	}
	if h.pveFor == nil {
		return fmt.Errorf("proxmox service not configured")
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return err
	}
	release, ok, err := h.lbLock(ctx, cr.ExternalID, lbLockWait)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is being changed. Try again in a moment")
	}
	defer release()
	return h.lbDestroy(ctx, ps, es, proj, lb)
}

// lbDestroy destroys every VM carrying the balancer's tag in the project's pool, then gives its addresses back and
// drops their rows. The caller holds the balancer's lock, or is the create that failed.
func (h *Handler) lbDestroy(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService, proj *Project, lb *loadBalancer) error {
	vms, err := lbVMs(ctx, ps)
	if err != nil {
		return err
	}
	pool := ps.PoolFor(proj.ID)
	if vm, ok := vms[lb.ServerID]; ok && vm.Pool == pool && vm.NodeDown {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The host of this load balancer is down. Try again when it is back")
	}
	for _, a := range lb.Appliances {
		if vm, ok := vms[a.ServerID]; ok && vm.Pool == pool && vm.NodeDown && pve.LBOfTags(vm.Tags) == lb.ID {
			return httpx.NewError(http.StatusConflict, http.StatusConflict, "The host of an appliance of this load balancer is down. Try again when it is back")
		}
	}
	for id, vm := range vms {
		if vm.Pool != pool || vm.Template == 1 || pve.LBOfTags(vm.Tags) != lb.ID {
			continue
		}
		if err := h.lbDestroyVM(ctx, es, proj, id); err != nil {
			return err
		}
	}
	if lb.HA {
		// The VIPs are the balancer's, not a VM's: they go with it.
		owner := LBAddressOwner(lb.ID)
		if err := h.publicIPs.ReleaseServer(ctx, es.ID, owner); err != nil {
			return err
		}
		return h.lbDropAddressRows(ctx, es, proj, owner)
	}
	// A VM someone else destroyed left its address behind, and the public address's row bills until the
	// next sample unless it goes now. A vmid another VM has since taken is that VM's.
	if vm, listed := vms[lb.ServerID]; lb.ServerID != "" && (!listed || pve.LBOfTags(vm.Tags) == lb.ID) {
		held, err := h.publicIPs.ForServer(ctx, es.ID, lb.ServerID)
		if err != nil {
			return err
		}
		for _, row := range held {
			if row.ProjectID == proj.ID {
				if err := h.publicIPs.ReleaseServer(ctx, es.ID, lb.ServerID); err != nil {
					return err
				}
				break
			}
		}
		return h.lbDropAddressRows(ctx, es, proj, lb.ServerID)
	}
	return nil
}

// lbDropAddressRows archives the FLOATING_IP rows of the addresses owner held, which would bill until the next
// sample otherwise.
func (h *Handler) lbDropAddressRows(ctx context.Context, es *externalservice.ExternalService, proj *Project, owner string) error {
	fips, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeFloatingIP)
	if err != nil {
		return err
	}
	for i := range fips {
		if fip, _ := fips[i].Data["floatingIp"].(map[string]any); strAny(fip["server_id"]) == owner {
			if err := h.cloud.DeleteAndArchive(ctx, &fips[i], time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

// lbStopWhileDisabled: a suspended tenant keeps the right to stop what it pays for (allowedWhileDisabled), the appliance
// too. Nothing starts it while the project is DISABLED, and the sync does after the resume.
func lbStopWhileDisabled(proj *Project, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if action == "INVOKEACTION" {
		action = strings.ToUpper(strAny(data["action"]))
	}
	return proj.IsDisabled() && cr.Type == cloud.TypeServer && (action == "STOP" || action == "SHUTOFF" || action == "SHUTDOWN")
}

// pveApplianceGuard refuses a tenant's direct action on what makes up a balancer: its VM, its disk, its
// public address. They show up as ordinary rows because they bill as ordinary rows.
func (h *Handler) pveApplianceGuard(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) *httpx.HTTPError {
	serverID := ""
	server := (*cloud.CloudResource)(nil)
	switch cr.Type {
	case cloud.TypeLoadBalancer:
		return lbClusterGuard(cr)
	case cloud.TypeServer:
		serverID, server = cr.ExternalID, cr
	case cloud.TypeVolume:
		vol, _ := cr.Data["volume"].(map[string]any)
		serverID = strconv.Itoa(intAny(vol["vmid"]))
	case cloud.TypeFloatingIP:
		fip, _ := cr.Data["floatingIp"].(map[string]any)
		serverID = strAny(fip["server_id"])
	}
	if serverID == "" || serverID == "0" {
		return nil
	}
	unreadable := httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "Load balancers cannot be read right now")
	if server == nil {
		var err error
		if server, err = h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, serverID); err != nil {
			return unreadable
		}
	}
	rows, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeLoadBalancer)
	if err != nil {
		return unreadable
	}
	if lb := lbOwnerOf(server, serverID, lbsOfRows(rows)); lb != nil {
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			"This belongs to load balancer "+lb.Name+". Change or delete the load balancer instead")
	}
	return nil
}

// owns: id is the balancer's VM, one of its appliances, or the owner of its VIPs.
func (lb *loadBalancer) owns(id string) bool {
	return id == lb.ServerID || (lb.HA && id == LBAddressOwner(lb.ID)) ||
		slices.ContainsFunc(lb.Appliances, func(a lbAppliance) bool { return a.ServerID == id })
}

// lbOwnerOf names the balancer a VM belongs to. The VM's row carries its tag from the first sample on, before the
// balancer's row knows its VM. A row without the tag on a vmid a balancer still names is a new server on the reused
// vmid. With no row yet, the balancer's own record is all there is.
func lbOwnerOf(server *cloud.CloudResource, serverID string, balancers []loadBalancer) *loadBalancer {
	tag := ""
	if server != nil {
		srv, _ := server.Data["server"].(map[string]any)
		tag = strAny(srv["loadBalancerId"])
	}
	for i := range balancers {
		lb := &balancers[i]
		if lb.ID == tag || (server == nil && lb.owns(serverID)) {
			return lb
		}
	}
	return nil // a tag with no balancer's row left is a VM nothing else can remove: it stays the tenant's to delete
}
