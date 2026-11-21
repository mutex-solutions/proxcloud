package project

// cloud_lb_cluster_pve.go — the load balancer of a Kubernetes cluster's Services: always a pair (cloud_lb_pair_pve.go),
// one per cluster, built with the cluster's first Service and deleted with the cluster (Q1). Its config is not the
// tenant's: the cluster's load balancer controller PUTs the cluster's whole state to the gateway (internal/cloud/lbgw),
// which stores it and starts a converge here. The converge gives each Service a public address (a VIP of the pair,
// owner lb:<id>, a floatingIps quota each) and makes one listener per (address, port) and one pool per port, whose
// members are the cluster's workers on the node port. A worker counts only when its address is one of the cluster's
// own worker addresses (C11). A Service gone from the state loses its listeners and its address at once: the state is
// complete by construction, so there is no day of grace as in the approved design.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const (
	// lbClusterBuildRetry is how long a cluster's pair waits before an appliance build that failed is tried again.
	lbClusterBuildRetry = 5 * time.Minute
	// lbSizePlaceholder stands in for an address not yet taken when a config's size is checked: the longest one.
	lbSizePlaceholder = "255.255.255.255"
)

// lbCluster is a cluster balancer's part of the row.
type lbCluster struct {
	ID       string         `json:"id"` // the cluster
	Services []lbClusterSvc `json:"services,omitempty"`
	// Releasing are the addresses of Services gone from the state. They are held until no appliance serves them any
	// more (loadBalancer.settled), so the pool never hands out an address a keepalived may still announce.
	Releasing []string `json:"releasing,omitempty"`
	// BuildFailedAt is when an appliance build last failed, RFC 3339; the next one waits lbClusterBuildRetry.
	BuildFailedAt string `json:"buildFailedAt,omitempty"`
}

// lbClusterSvc is one Service as the balancer has it, in the order the balancer first saw them: when the config is
// full, the newest is the one refused.
type lbClusterSvc struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	VIP       string `json:"vip,omitempty"`
	State     string `json:"state"` // lbgw.StateReady, StateBuilding, StateRefused
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
}

// SetLBGateway turns on the cluster balancers: the gateway's store of credentials and states (nil = off).
func (h *Handler) SetLBGateway(s *lbgw.Store) { h.lbGW = s }

// lbClusterExternalID is the external id of the cluster's balancer row: one per cluster, the same on every replica,
// so two converges cannot make two.
func lbClusterExternalID(clusterID string) string {
	sum := sha256.Sum256([]byte(kamajiReservation(clusterID)))
	return "lb-" + hex.EncodeToString(sum[:6])
}

// lbClusterGuard refuses a tenant's change or delete of a cluster's balancer: its Services make it, the cluster's
// release deletes it, and project teardown does not pass here.
func lbClusterGuard(cr *cloud.CloudResource) *httpx.HTTPError {
	lb, err := lbOfRow(cr)
	if err != nil || lb.Cluster == nil {
		return nil
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict, "This load balancer is managed by Kubernetes cluster "+lb.Cluster.ID+
		": change the cluster's Services of type LoadBalancer instead. It is deleted with the cluster")
}

// kamajiWorkerAddresses is the worker part of a cluster's reservation, which ClusterInfra.Addresses got at create: the
// kamajiWorkerAddrs highest of the addresses reserved for k8s:<id>, as Reserve hands them out highest first. The rest
// are MetalLB's, never a member (C11).
func (h *Handler) kamajiWorkerAddresses(ctx context.Context, computeID, clusterID string) ([]string, error) {
	rows, err := h.publicIPs.ForServer(ctx, computeID, kamajiReservation(clusterID))
	if err != nil {
		return nil, err
	}
	return kamajiWorkerPart(rows), nil
}

func kamajiWorkerPart(rows []PublicIP) []string {
	var addrs []netip.Addr
	for _, r := range rows {
		if a, err := netip.ParseAddr(r.IP); err == nil && a.Is4() && r.NIC == ReservedNIC {
			addrs = append(addrs, a)
		}
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return b.Compare(a) })
	out := make([]string, 0, kamajiWorkerAddrs)
	for i := 0; i < len(addrs) && i < kamajiWorkerAddrs; i++ {
		out = append(out, addrs[i].String())
	}
	return out
}

// lbClusterSettings is what a cluster's balancer is built from: the Kubernetes provider's config.cluster.loadBalancer,
// a public network of the compute provider the project may use, with a gateway, on a plain bridge (C10), and an
// active flavor. The message says what is missing.
func (h *Handler) lbClusterSettings(ctx context.Context, cred *lbgw.Credential, proj *Project, compute *externalservice.ExternalService) (*externalservice.PVEPublicNetwork, *pveflavor.Flavor, *httpx.HTTPError) {
	unavailable := func(msg string) *httpx.HTTPError {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, msg)
	}
	k8s, err := h.esSvc.Get(ctx, cred.KubernetesServiceID)
	if err != nil || k8s == nil {
		return nil, nil, unavailable("The cluster's Kubernetes provider cannot be read")
	}
	netID, flavorID, ok := k8s.KamajiLoadBalancer()
	if !ok {
		return nil, nil, unavailable("The Kubernetes provider offers no load balancer (config.cluster.loadBalancer): ask your operator")
	}
	pn := pveAllowedPublicNetwork(compute, proj, netID)
	if pn == nil {
		return nil, nil, unavailable("Public network " + netID + " is not available to this project: ask your operator")
	}
	if pn.Gateway == "" {
		return nil, nil, unavailable("Public network " + pn.ID + " has no gateway")
	}
	ps, err := h.pveFor(compute)
	if err != nil {
		return nil, nil, unavailable("The Proxmox provider cannot be reached")
	}
	if _, vnet, err := ps.SDNZoneOf(ctx, pn.ID); err != nil {
		return nil, nil, unavailable("The Proxmox provider cannot be reached")
	} else if vnet {
		return nil, nil, unavailable("Public network " + pn.ID + " is an SDN vnet, and two appliances cannot share an address there: ask your operator")
	}
	fl, err := h.pveFlavors.Get(ctx, flavorID)
	if err != nil || fl == nil || !fl.Active {
		return nil, nil, unavailable("The load balancer's flavor " + flavorID + " is not active: ask your operator")
	}
	return pn, fl, nil
}

// LBClusterState is the gateway's call (lbgw.Portal): services passed the gateway's own checks, nodes did not yet. It
// refuses with no change while the project is suspended or being deleted (C3), stores the state and starts a converge,
// and answers each Service as the balancer has it now. Nothing is built here.
func (h *Handler) LBClusterState(ctx context.Context, cred *lbgw.Credential, services []lbgw.Service, nodes []lbgw.Node) (*lbgw.Answer, error) {
	if h.lbGW == nil || h.sdnLock == nil || h.publicIPs == nil || h.pveFor == nil || h.pveFlavors == nil {
		return nil, &lbgw.Refusal{Status: http.StatusServiceUnavailable, Message: "Load balancers are not configured on this portal"}
	}
	proj, err := h.svc.GetProjectByID(ctx, cred.ProjectID)
	if err != nil {
		return nil, err
	}
	if proj.Deleting() || proj.Status != StatusEnabled {
		return nil, &lbgw.Refusal{Status: http.StatusConflict, Reason: lbgw.ReasonProjectNotEnabled,
			Message: "The cluster's project is suspended or being deleted: its load balancer does not change until the project is enabled"}
	}
	workers, err := h.kamajiWorkerAddresses(ctx, cred.ComputeServiceID, cred.ID)
	if err != nil {
		return nil, err
	}
	ans := &lbgw.Answer{Services: []lbgw.ServiceStatus{}}
	accepted := []string{}
	for _, n := range nodes {
		switch {
		case !slices.Contains(workers, n.Address):
			ans.Nodes = append(ans.Nodes, lbgw.NodeStatus{Name: n.Name, Address: n.Address, Reason: lbgw.ReasonNotAWorker,
				Message: "Not an address of this cluster's workers: it is not a member"})
		case !slices.Contains(accepted, n.Address):
			accepted = append(accepted, n.Address)
		}
	}
	row, err := h.cloud.FindByServiceIDAndExternalID(ctx, cred.ComputeServiceID, lbClusterExternalID(cred.ID))
	if err != nil {
		return nil, err
	}
	var lb *loadBalancer
	if row != nil {
		if lb, err = lbOfRow(row); err != nil {
			return nil, err
		}
	}
	if lb == nil && len(services) > 0 {
		// Before the pair exists, a site that cannot build it says so now; once it exists it keeps its own settings.
		compute, err := h.esSvc.Get(ctx, cred.ComputeServiceID)
		if err != nil {
			return nil, err
		}
		if _, _, herr := h.lbClusterSettings(ctx, cred, proj, compute); herr != nil {
			for _, s := range services {
				ans.Services = append(ans.Services, lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused,
					Reason: lbgw.ReasonPublicNetworkUnavailable, Message: herr.Msg})
			}
			return ans, nil
		}
	}
	if err := h.lbGW.PutState(ctx, &lbgw.State{ID: cred.ID, Services: services, Nodes: accepted}); err != nil {
		return nil, err
	}
	go h.lbClusterConverge(context.WithoutCancel(ctx), cred.ID)
	for _, s := range services {
		ans.Services = append(ans.Services, lbClusterStatus(lb, s.UID))
	}
	return ans, nil
}

// lbClusterStatus is a Service as the row has it; a Service the converge has not seen yet is building.
func lbClusterStatus(lb *loadBalancer, uid string) lbgw.ServiceStatus {
	out := lbgw.ServiceStatus{UID: uid, State: lbgw.StateBuilding}
	if lb == nil || lb.Cluster == nil {
		return out
	}
	if i := slices.IndexFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.UID == uid }); i >= 0 {
		s := lb.Cluster.Services[i]
		out.Address, out.State, out.Reason, out.Message = s.VIP, s.State, s.Reason, s.Message
	}
	return out
}

// lbClusterConverge runs one converge of the cluster's balancer, unless one runs already: the controller sends its
// state every few seconds and the provider sync calls this too, so a converge that is skipped comes again.
func (h *Handler) lbClusterConverge(ctx context.Context, clusterID string) {
	release, ok, err := h.sdnLock.Wait(ctx, "lbc-"+clusterID, lbBuildTimeout, 0)
	if err != nil {
		slog.Warn("load balancer: cluster converge lock", "cluster", clusterID, "err", err)
		return
	}
	if !ok {
		return
	}
	defer release()
	if err := h.lbClusterPass(ctx, clusterID); err != nil {
		slog.Warn("load balancer: cluster converge", "cluster", clusterID, "err", err)
	}
}

// lbClusterPass: the row, then the appliances, then addresses and config. The caller holds lbc-<clusterId>.
func (h *Handler) lbClusterPass(ctx context.Context, clusterID string) error {
	cred, err := h.lbGW.Credential(ctx, clusterID)
	if err != nil || cred == nil || cred.RevokedAt != nil { // released, or being released
		return err
	}
	proj, err := h.svc.GetProjectByID(ctx, cred.ProjectID)
	if err != nil {
		return err
	}
	if proj.Deleting() || proj.Status != StatusEnabled { // the state is applied after the resume
		return nil
	}
	st, err := h.lbGW.State(ctx, clusterID)
	if err != nil {
		return err
	}
	es, err := h.esSvc.Get(ctx, cred.ComputeServiceID)
	if err != nil {
		return err
	}
	ext := lbClusterExternalID(clusterID)
	row, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, ext)
	if err != nil {
		return err
	}
	if row == nil {
		if st == nil || len(st.Services) == 0 {
			return nil // the pair comes with the first Service
		}
		pn, fl, herr := h.lbClusterSettings(ctx, cred, proj, es)
		if herr != nil {
			return herr
		}
		now := time.Now().UTC()
		row = &cloud.CloudResource{ExternalID: ext, ServiceID: es.ID, ProjectID: proj.ID, Region: h.pveRegion(proj, es),
			Type: cloud.TypeLoadBalancer, CreatedAt: &now, Info: &cloud.Info{CreatedAt: &now, UpdatedAt: &now}}
		id := ext[len("lb-"):]
		lb := &loadBalancer{ID: id, Name: "k8s-" + id, Status: lbStatusBuild, NetworkID: cred.Vnet, NetworkRef: cred.NetworkID,
			PublicNetworkID: pn.ID, HA: true, FlavorID: fl.ID, Cluster: &lbCluster{ID: clusterID}}
		if err := setLBRow(row, lb); err != nil {
			return err
		}
		if _, err := h.cloud.Insert(ctx, row); err != nil {
			return err
		}
	}
	if err := h.lbClusterBuild(ctx, es, proj, ext); err != nil {
		slog.Warn("load balancer: cluster appliance", "cluster", clusterID, "err", err)
	}
	return h.lbClusterApply(ctx, es, proj, ext, st)
}

// lbClusterBuild builds the appliances the pair lacks, one at a time, outside the row's lock so that the gateway's PUTs
// and the applies of the other appliance go on. A failed build waits lbClusterBuildRetry; a replace builds its own.
func (h *Handler) lbClusterBuild(ctx context.Context, es *externalservice.ExternalService, proj *Project, ext string) error {
	ps, err := h.pveFor(es)
	if err != nil {
		return err
	}
	for {
		lb, err := h.lbEdit(ctx, es, ext, nil)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		failed, _ := time.Parse(time.RFC3339, lb.Cluster.BuildFailedAt)
		if len(lb.Appliances) >= 2 || lbReplacing(lb, now) || now.Sub(failed) < lbClusterBuildRetry {
			return nil
		}
		app, berr := h.lbBuildAppliance(ctx, nil, proj, es, ps, lb)
		if berr != nil {
			_, err := h.lbEdit(ctx, es, ext, func(lb *loadBalancer) {
				lb.Cluster.BuildFailedAt = time.Now().UTC().Format(time.RFC3339)
				lb.StatusMessage = "An appliance could not be built, it is tried again in a few minutes: " + berr.Error()
				if len(lb.Appliances) == 0 {
					lb.Status = lbStatusError
				}
			})
			return errors.Join(berr, err)
		}
		if _, err := h.lbEdit(ctx, es, ext, func(lb *loadBalancer) {
			lb.Appliances = append(lb.Appliances, *app)
			lb.Cluster.BuildFailedAt, lb.StatusMessage = "", ""
			lbPairLegacyFields(lb)
		}); err != nil {
			// The row went meanwhile: the new VM is nobody's.
			if derr := h.lbDestroyVM(ctx, es, proj, app.ServerID); derr != nil {
				slog.Error("load balancer: remove an appliance built for a balancer that went", "lb", ext, "vm", app.ServerID, "err", derr)
			}
			return err
		}
	}
}

// lbClusterApply brings the row in line with the state and the appliances with the row, under the row's lock.
func (h *Handler) lbClusterApply(ctx context.Context, es *externalservice.ExternalService, proj *Project, ext string, st *lbgw.State) error {
	release, ok, err := h.sdnLock.Wait(ctx, "lb-"+ext, lbLockMaxFor, lbLockWait)
	if err != nil || !ok {
		return err // another apply is on it
	}
	defer release()
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, ext)
	if err != nil || cr == nil {
		return err
	}
	lb, err := lbOfRow(cr)
	if err != nil || lb.Cluster == nil {
		return err
	}
	if err := h.lbClusterDesired(ctx, es, proj, lb, st); err != nil {
		return err
	}
	if len(lb.Appliances) > 0 {
		err = h.lbApplyPair(ctx, es, proj, cr, lb)
	} else {
		lb.settled = true // no appliance has ever held an address
	}
	if err == nil && lb.settled && len(lb.Cluster.Releasing) > 0 {
		err = h.lbReleaseVIPs(ctx, es, proj, lb)
	}
	applied := lb.AppliedHash == lbHash(lbRender(lb)) && (lb.Status == lbStatusActive || lb.Status == lbStatusDegraded)
	for i := range lb.Cluster.Services {
		if s := &lb.Cluster.Services[i]; s.State != lbgw.StateRefused {
			s.State = lbgw.StateBuilding
			if s.VIP != "" && applied {
				s.State = lbgw.StateReady
			}
		}
	}
	if serr := h.lbSave(ctx, cr, lb); serr != nil && err == nil {
		err = serr
	}
	return err
}

// lbClusterDesired folds the state into the row: Services gone give up their address, new ones are added in the order
// they come, each one that fits the config gets an address (a floatingIps quota each), and the spec is made from them.
// No state (the controller never sent one) changes nothing.
func (h *Handler) lbClusterDesired(ctx context.Context, es *externalservice.ExternalService, proj *Project, lb *loadBalancer, st *lbgw.State) error {
	if st == nil {
		return nil
	}
	want := map[string]lbgw.Service{}
	for _, s := range st.Services {
		want[s.UID] = s
	}
	var svcs []lbClusterSvc
	for _, s := range lb.Cluster.Services {
		if _, ok := want[s.UID]; ok {
			svcs = append(svcs, s)
		} else if s.VIP != "" {
			lb.Cluster.Releasing = append(lb.Cluster.Releasing, s.VIP)
		}
	}
	for _, s := range st.Services {
		if !slices.ContainsFunc(svcs, func(c lbClusterSvc) bool { return c.UID == s.UID }) {
			svcs = append(svcs, lbClusterSvc{UID: s.UID})
		}
	}
	pn := pveAllowedPublicNetwork(es, proj, lb.PublicNetworkID)
	var spec pve.LBSpec
	var vips []string
	for i := range svcs {
		s := &svcs[i]
		w := want[s.UID]
		s.Namespace, s.Name, s.State, s.Reason, s.Message = w.Namespace, w.Name, lbgw.StateBuilding, "", ""
		refuse := func(reason, msg string) {
			s.State, s.Reason, s.Message = lbgw.StateRefused, reason, msg
			if s.VIP != "" {
				lb.Cluster.Releasing = append(lb.Cluster.Releasing, s.VIP)
				s.VIP = ""
			}
		}
		vip := s.VIP
		if vip == "" {
			vip = lbSizePlaceholder
		}
		trial := lbClusterServiceSpec(spec, i, w, vip, st.Nodes)
		if len(pve.RenderClusterHAProxy(trial, append(slices.Clone(vips), vip))) > pve.LBMaxConfig {
			refuse(lbgw.ReasonTooLarge, "The load balancer's config would be too large with this Service: use fewer Services, ports or workers")
			continue
		}
		if s.VIP == "" {
			if pn == nil {
				refuse(lbgw.ReasonPublicNetworkUnavailable, "Public network "+lb.PublicNetworkID+" is not available to this project any more")
				continue
			}
			if herr := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{FloatingIPs: 1}); herr != nil {
				refuse(lbgw.ReasonQuotaExceeded, "A Service takes one public address, and this project cannot take another: "+herr.Msg)
				continue
			}
			ip, err := h.publicIPs.Allocate(ctx, es.ID, proj.ID, LBAddressOwner(lb.ID), publicNIC, *pn)
			if err != nil {
				var herr *httpx.HTTPError
				if !errors.As(err, &herr) {
					return err
				}
				refuse(lbgw.ReasonPublicNetworkUnavailable, herr.Msg)
				continue
			}
			s.VIP = ip.IP
			trial = lbClusterServiceSpec(spec, i, w, s.VIP, st.Nodes)
		}
		spec, vips = trial, append(vips, s.VIP)
	}
	lb.Cluster.Services, lb.LBSpec, lb.VIPs = svcs, spec, vips
	lb.PublicIP = ""
	if len(vips) > 0 {
		lb.PublicIP = vips[0]
	}
	return nil
}

// lbClusterServiceSpec adds Service w, the i-th of the row, to spec: a listener on its address per port, in mode tcp,
// to a pool of the workers' node port. Policy Local is checked over HTTP on healthCheckNodePort, which kube-proxy
// answers 200 only with a local endpoint; a check on its own port is sent without the PROXY header (C14). Names are
// generated: nothing of the Service's reaches the config.
func lbClusterServiceSpec(spec pve.LBSpec, i int, w lbgw.Service, vip string, nodes []string) pve.LBSpec {
	out := pve.LBSpec{Listeners: slices.Clone(spec.Listeners), Pools: slices.Clone(spec.Pools)}
	for j, p := range w.Ports {
		pool := pve.LBPool{Name: "s" + strconv.Itoa(i) + "-" + strconv.Itoa(j), Protocol: "tcp", Algorithm: "roundrobin", ProxyProtocol: w.ProxyProtocol}
		if w.ExternalTrafficPolicy == "Local" {
			pool.HealthCheck = &pve.LBHealthCheck{Type: "http", Port: w.HealthCheckNodePort, Path: "/"}
		}
		for _, n := range nodes {
			pool.Members = append(pool.Members, pve.LBMember{Node: true, Address: n, Port: p.NodePort, Weight: 1})
		}
		out.Pools = append(out.Pools, pool)
		out.Listeners = append(out.Listeners, pve.LBListener{Port: p.Port, Pool: pool.Name, SourceRanges: w.SourceRanges, VIP: vip})
	}
	return out
}

// lbReleaseVIPs gives back the addresses of Services that are gone, and drops their FLOATING_IP rows, which would
// bill until the next sample otherwise. One that cannot go stays listed for the next apply.
func (h *Handler) lbReleaseVIPs(ctx context.Context, es *externalservice.ExternalService, proj *Project, lb *loadBalancer) error {
	owner := LBAddressOwner(lb.ID)
	var left []string
	var errs []error
	for _, ip := range lb.Cluster.Releasing {
		if slices.Contains(lb.VIPs, ip) { // taken again meanwhile: not ours to give back
			continue
		}
		err := h.publicIPs.release(ctx, pgdoc.M{"serviceId": es.ID, "serverId": owner, "ip": ip})
		if err == nil {
			err = h.lbDropAddressRow(ctx, es, proj, owner, ip)
		}
		if err != nil {
			left, errs = append(left, ip), append(errs, err)
		}
	}
	lb.Cluster.Releasing = left
	return errors.Join(errs...)
}

// lbDropAddressRow archives the FLOATING_IP row of one address of owner.
func (h *Handler) lbDropAddressRow(ctx context.Context, es *externalservice.ExternalService, proj *Project, owner, ip string) error {
	fips, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeFloatingIP)
	if err != nil {
		return err
	}
	for i := range fips {
		fip, _ := fips[i].Data["floatingIp"].(map[string]any)
		if strAny(fip["server_id"]) == owner && strAny(fip["floating_ip_address"]) == ip {
			if err := h.cloud.DeleteAndArchive(ctx, &fips[i], time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

// LBReleaseCluster is the balancer's part of a cluster's release (kamaji.OnClusterGone), with or without a controller:
// the credential is revoked first, so no PUT can build the pair again, then the appliances and the addresses go, then
// the row and the state (C3). An error keeps the cluster's marker, and the sweep runs it again.
func (h *Handler) LBReleaseCluster(ctx context.Context, computeID, projectID, clusterID string) error {
	if h.lbGW != nil {
		if err := h.lbGW.RevokeCredential(ctx, clusterID); err != nil {
			return err
		}
	}
	if h.sdnLock == nil || h.pveFor == nil {
		return nil // no balancer can have been built
	}
	release, ok, err := h.sdnLock.Wait(ctx, "lbc-"+clusterID, lbBuildTimeout, lbLockWait)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("load balancer of cluster %s: a converge is running, the release is tried again", clusterID)
	}
	defer release()
	es, err := h.esSvc.Get(ctx, computeID)
	if errors.Is(err, externalservice.ErrNotFound) {
		return h.lbDropState(ctx, clusterID) // a provider that is gone holds nothing to delete
	}
	if err != nil {
		return err
	}
	ext := lbClusterExternalID(clusterID)
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, ext)
	if err != nil {
		return err
	}
	if cr != nil {
		proj, perr := h.svc.GetProjectByID(ctx, projectID)
		if perr != nil { // a project already torn down: its pool and addresses are gone, the row is all that is left
			proj = &Project{ID: projectID}
		}
		unlock, ok, err := h.sdnLock.Wait(ctx, "lb-"+ext, lbLockMaxFor, lbLockWait)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("load balancer of cluster %s is being changed, the release is tried again", clusterID)
		}
		defer unlock()
		lb, err := lbOfRow(cr)
		if err != nil {
			return err
		}
		ps, err := h.pveFor(es)
		if err != nil {
			return err
		}
		if err := h.lbDestroy(ctx, ps, es, proj, lb); err != nil {
			return err
		}
		if err := h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC()); err != nil {
			return err
		}
	}
	return h.lbDropState(ctx, clusterID)
}

func (h *Handler) lbDropState(ctx context.Context, clusterID string) error {
	if h.lbGW == nil {
		return nil
	}
	return h.lbGW.DeleteState(ctx, clusterID)
}
