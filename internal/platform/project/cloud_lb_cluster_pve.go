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
	"cmp"
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
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
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
	// lbPassEvery is how often a settled cluster balancer is looked at when its cluster's state did not change: the
	// pass that finds an appliance someone stopped, about 16 Proxmox calls. A changed state, and a balancer that is
	// not settled, converge at once.
	lbPassEvery = time.Minute
)

// lbcLease is how long the cluster's converge lock (lbc-<clusterId>) lasts without its holder: the holder extends it
// while it works (lock.ShedLock.Hold), so an API that restarts in the middle of a pass keeps the cluster's balancer
// from converging for a minute at most. It was taken for lbBuildTimeout, 20 minutes (v0.11.0-rc.1 drill, N2-3).
var lbcLease = time.Minute

// lbPass is the last pass over a cluster's balancer that ran to its end, as this process saw it.
type lbPass struct {
	digest  string // of the state it applied (lbStateDigest)
	at      time.Time
	settled bool // the row then read ACTIVE, with every Service answered and no address left to give back
}

// lbStateDigest names a cluster's state by content: the Services and nodes in the order the controller sent them,
// which is the order the config is rendered in. The time it was stored is not part of it.
func lbStateDigest(st *lbgw.State) string {
	if st == nil {
		return ""
	}
	services, nodes := st.Services, st.Nodes
	if len(services) == 0 {
		services = nil
	}
	if len(nodes) == 0 {
		nodes = nil
	}
	raw, _ := json.Marshal([]any{services, nodes})
	return lbHash(string(raw))
}

// lbPassDue says whether a controller's PUT of the state named digest starts a pass. The controller sends its state
// every ten seconds, changed or not, and each pass costs about 16 Proxmox calls with two guest commands (v0.11.0-rc.1
// drill, N2-4: 96 calls a minute per cluster). So a PUT starts one only when it has something to do: the state is
// not the one the last pass applied, that pass left the balancer unsettled (building, degraded, an address still to
// give back), or the last look at a settled balancer is lbPassEvery old.
// ponytail: kept per process, so each API replica looks once per lbPassEvery and the first PUT after a restart always
// starts a pass. Keep it on the row if replicas ever multiply the calls enough to matter.
func (h *Handler) lbPassDue(clusterID, digest string) bool {
	v, ok := h.lbPasses.Load(clusterID)
	if !ok {
		return true
	}
	last := v.(lbPass)
	return last.digest != digest || !last.settled || time.Since(last.at) >= lbPassEvery
}

// lbCluster is a cluster balancer's part of the row.
type lbCluster struct {
	ID       string         `json:"id"` // the cluster
	Services []lbClusterSvc `json:"services,omitempty"`
	// Releasing are the addresses of Services gone from the state. They are held until no appliance serves them any
	// more (loadBalancer.settled), so the pool never hands out an address a keepalived may still announce.
	Releasing []string `json:"releasing,omitempty"`
	// Kept are the addresses of Services that went while they asked to keep theirs (proxcloud.io/lb-keep-address), at
	// most lbgw.MaxKeptAddresses. No appliance serves one, and it stays the balancer's (LBAddressOwner): allocated,
	// so in the project's floatingIps quota and billed as any public address, and no other cluster or project can
	// be given it. It leaves the list for a Service of this cluster that names it in spec.loadBalancerIP
	// (lbClusterDesired), by the tenant's release (lbClusterReleaseKept), or with the cluster (lbDestroy).
	Kept []string `json:"kept,omitempty"`
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
// refuses with no change while the project is suspended or being deleted (C3), takes the certificates it asked for
// (lbClusterTLS), stores the state, starts a converge when there is something to converge (lbPassDue), and answers
// each Service as the balancer has it now. gone are the Services the controller names as being deleted, with their
// last word (lbClusterKeepGone). Nothing is built here.
func (h *Handler) LBClusterState(ctx context.Context, cred *lbgw.Credential, services []lbgw.Service, nodes []lbgw.Node, certs []lbgw.CertificateData, gone []lbgw.Gone) (*lbgw.Answer, error) {
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
	// A Service whose TLS is not as it asks stays as the last state had it, or out of the state (lbClusterTLS).
	stored, keep, held, need, err := h.lbClusterTLS(ctx, cred, services, certs)
	if err != nil {
		return nil, err
	}
	st := &lbgw.State{ID: cred.ID, Services: stored, Nodes: accepted, CertHashes: keep}
	if st.KeepGone, err = h.lbClusterKeepGone(ctx, cred.ID, lb, stored, gone); err != nil {
		return nil, err
	}
	if err := h.lbGW.PutState(ctx, st); err != nil {
		return nil, err
	}
	if h.lbPassDue(cred.ID, lbStateDigest(st)) {
		go h.lbClusterConverge(context.WithoutCancel(ctx), cred.ID, false)
	}
	ans.NeedCertificates = need
	var kept []string // the addresses the cluster keeps, and the ones the next pass keeps for it
	if lb != nil && lb.Cluster != nil {
		kept = slices.Clone(lb.Cluster.Kept)
		for _, c := range lb.Cluster.Services {
			if c.VIP != "" && slices.Contains(st.KeepGone, c.UID) {
				kept = append(kept, c.VIP)
			}
		}
	}
	for i := range services {
		s := &services[i]
		status := lbClusterStatus(lb, s.UID)
		if said, ok := held[s.UID]; ok && said.State != "" {
			status = said // not in the state: refused, or waiting for its certificate
		} else if ok {
			status.Events = said.Events
		}
		status.Events = append(status.Events, lbClusterAddressEvents(kept, s, status)...)
		ans.Services = append(ans.Services, status)
	}
	return ans, nil
}

// lbClusterKeepGone is the new state's KeepGone: the Services that are not in stored, that asked to keep their address
// and whose address the balancer lb still holds for them. A Service the converge has let go of (its address is kept
// or given back, and the row lists it no more) leaves the list with the next PUT, and one that went before the
// balancer had an address for it keeps none.
//
// What a Service asked is, in this order: what the controller says of it in gone, which is the object as it reads at
// its deletion; else what the last state had of it, in its annotations or carried in its KeepGone. The last state
// alone is not enough: the controller takes its turn a second after a change, so a Service annotated and deleted
// within that second was never sent with the annotation and its address was given back (drill R2a-2 of v0.11.1-rc.2,
// twice, 0.31 and 0.36 s between kubectl annotate and kubectl delete), and one whose annotation was removed and that
// was then deleted kept an address nobody asked to keep. A controller of before v0.11.1-rc.3 sends no gone, and its
// Services go by the last state as they did.
func (h *Handler) lbClusterKeepGone(ctx context.Context, clusterID string, lb *loadBalancer, stored []lbgw.Service, gone []lbgw.Gone) ([]string, error) {
	if lb == nil || lb.Cluster == nil {
		return nil, nil // no balancer, no address
	}
	last, err := h.lbGW.State(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	var asked []string
	if last != nil {
		asked = slices.Clone(last.KeepGone)
		for i := range last.Services {
			if lbgw.KeepsAddress(&last.Services[i]) {
				asked = append(asked, last.Services[i].UID)
			}
		}
	}
	for _, g := range gone {
		asked = slices.DeleteFunc(asked, func(uid string) bool { return uid == g.UID })
		if lbgw.KeepsAddress(&lbgw.Service{Annotations: g.Annotations}) {
			asked = append(asked, g.UID)
		}
	}
	var out []string
	for _, uid := range asked {
		holds := slices.ContainsFunc(lb.Cluster.Services, func(c lbClusterSvc) bool { return c.UID == uid && c.VIP != "" })
		if holds && !slices.Contains(out, uid) && !slices.ContainsFunc(stored, func(s lbgw.Service) bool { return s.UID == uid }) {
			out = append(out, uid)
		}
	}
	return out, nil
}

// lbClusterAddressEvents is what a Service that is not refused is told about the address it asks for
// (spec.loadBalancerIP) and the one it asks to keep. kept are the addresses its cluster keeps.
//
// The address a Service names is its own when the cluster keeps it and the Service has none yet: OCCM reads
// loadBalancerIP only for a balancer without a floating address, and takes one that exists and that nothing uses
// (loadbalancer.go:732-755). It also creates the address when none exists, and fails the Service when another port
// has it; here every other value is one answer, whatever the address is to anyone else: the Service is told so and
// served on an address of the pool. So a Service never learns from it whether an address it names is another
// cluster's, another project's or free, and never gets one of those.
func lbClusterAddressEvents(kept []string, s *lbgw.Service, status lbgw.ServiceStatus) []lbgw.Event {
	if status.State == lbgw.StateRefused {
		return nil
	}
	const how = `Only an address this cluster keeps can be named: one a Service left behind with ` + lbgw.OwnPrefix + lbgw.KnobKeepAddress + `: "true"`
	var out []lbgw.Event
	// "Not kept" is asked first, and its text names no address the Service has: a Service that names such an address
	// hears the same words before it has its pool address and after, which is one Warning on the Service. Asked the
	// other way round it heard a second one once it had its address, "an address is taken when the Service gets its
	// address, not after", as if it had named the address too late (drill Q1-3 of v0.11.1). That text is for the
	// one case it is true of: an address the cluster keeps, named by a Service that already has another.
	switch want := s.LoadBalancerIP; {
	case want == "" || want == status.Address:
	case !slices.Contains(kept, want):
		out = append(out, lbgw.Event{Type: "Warning", Reason: lbgw.ReasonLoadBalancerIPIgnored, Message: "spec.loadBalancerIP " + want +
			" is not an address this cluster keeps: the Service is served on an address of the pool. " + how})
	case status.Address != "":
		out = append(out, lbgw.Event{Type: "Warning", Reason: lbgw.ReasonLoadBalancerIPIgnored, Message: "spec.loadBalancerIP " + want +
			" is not obeyed: the Service has " + status.Address + ". An address is taken when the Service gets its address, not after. " + how})
	}
	if lbgw.KeepsAddress(s) && len(kept) >= lbgw.MaxKeptAddresses {
		out = append(out, lbgw.Event{Type: "Warning", Reason: lbgw.ReasonKeepAddressLimit, Message: fmt.Sprintf(
			"This cluster keeps %d addresses already, the most it can: this Service's address is given back when the Service goes. Release a kept address on the cluster's page first", lbgw.MaxKeptAddresses)})
	}
	return out
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
// state every few seconds and the provider sync calls this too, so a converge that is skipped comes again. force is
// the sync's: its pass runs whatever the last one found. A PUT's pass asks lbPassDue again once it holds the lock,
// since the pass it waited behind may have applied its state.
//
// A pass applies the state it read at its start. One the controller stores while the pass is at work finds the lock
// held and starts no pass of its own, so it waited for the controller's next send of the same state (about 11 s for
// the second of two changes 2 to 3 s apart). The converge therefore looks at the stored state once more before it
// lets go of the lock, and passes over it again when it is not the one it applied. Once, not until the state stands
// still: a Service that keeps changing cannot keep a converge and its lock, and what changes during the second pass
// is the next PUT's.
// ponytail: a state stored between that last look and the release still waits for the controller's next send; a
// "changed" mark the PUT leaves for the lock's holder would close it, if a drill ever shows the gap.
func (h *Handler) lbClusterConverge(ctx context.Context, clusterID string, force bool) {
	release, ok, err := h.sdnLock.Hold(ctx, "lbc-"+clusterID, lbcLease, 0)
	if err != nil {
		slog.Warn("load balancer: cluster converge lock", "cluster", clusterID, "err", err)
		return
	}
	if !ok {
		return
	}
	defer release()
	for range 2 {
		if err := h.lbClusterPass(ctx, clusterID, force); err != nil {
			slog.Warn("load balancer: cluster converge", "cluster", clusterID, "err", err)
		}
		applied, ok := h.lbPasses.Load(clusterID)
		st, err := h.lbGW.State(ctx, clusterID)
		if !ok || err != nil || applied.(lbPass).digest == lbStateDigest(st) {
			return
		}
	}
}

// lbClusterPass: the row, then the appliances, then addresses and config. The caller holds lbc-<clusterId>.
func (h *Handler) lbClusterPass(ctx context.Context, clusterID string, force bool) error {
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
	digest := lbStateDigest(st)
	if !force && !h.lbPassDue(clusterID, digest) {
		return nil
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
			h.lbPasses.Store(clusterID, lbPass{digest: digest, at: time.Now(), settled: true})
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
	} else if lb, err := lbOfRow(row); err != nil || lb.Cluster == nil || lb.Cluster.ID != clusterID {
		return fmt.Errorf("load balancer row %s is not cluster %s's", ext, clusterID)
	}
	if err := h.lbClusterBuild(ctx, es, proj, ext); err != nil {
		slog.Warn("load balancer: cluster appliance", "cluster", clusterID, "err", err)
	}
	settled, err := h.lbClusterApply(ctx, es, proj, ext, st)
	h.lbPasses.Store(clusterID, lbPass{digest: digest, at: time.Now(), settled: settled})
	if settled { // the appliances serve the state: the certificates nothing names any more are not needed again
		if perr := h.lbClusterPruneCerts(ctx, clusterID); perr != nil {
			slog.Warn("load balancer: cluster certificates", "cluster", clusterID, "err", perr)
		}
	}
	return err
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
		prefix := lb.PrivatePrefix // the appliance's network, read during its build
		full := false
		if _, err := h.lbEdit(ctx, es, ext, func(lb *loadBalancer) {
			// The build ran outside the row's lock, and the converge lock is a lease: a holder cut off from the
			// database for a whole lease works on while the next one builds too. The row takes two appliances.
			if full = len(lb.Appliances) >= 2; full {
				return
			}
			lb.Appliances = append(lb.Appliances, *app)
			lb.Cluster.BuildFailedAt, lb.StatusMessage = "", ""
			lbPairLegacyFields(lb)
			if lb.PrivatePrefix == 0 {
				lb.PrivatePrefix = prefix
			}
		}); err != nil || full {
			// The row went meanwhile, or has its pair already: the new VM is nobody's.
			if derr := h.lbDestroyVM(ctx, es, proj, app.ServerID); derr != nil {
				slog.Error("load balancer: remove an appliance built for a balancer that went or is complete", "lb", ext, "vm", app.ServerID, "err", derr)
			}
			return err
		}
	}
}

// lbClusterApply brings the row in line with the state and the appliances with the row, under the row's lock. settled
// says that it left nothing for a next pass: the balancer is ACTIVE, every Service has its answer (ready or refused),
// and no address waits to be given back.
func (h *Handler) lbClusterApply(ctx context.Context, es *externalservice.ExternalService, proj *Project, ext string, st *lbgw.State) (settled bool, err error) {
	release, ok, err := h.lbLock(ctx, ext, lbLockWait)
	if err != nil || !ok {
		return false, err // another apply is on it
	}
	defer release()
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, es.ID, ext)
	if err != nil || cr == nil {
		return false, err
	}
	lb, err := lbOfRow(cr)
	if err != nil || lb.Cluster == nil {
		return false, err
	}
	if err := h.lbClusterDesired(ctx, es, proj, lb, st); err != nil {
		return false, err
	}
	if len(lb.Appliances) > 0 {
		err = h.lbApplyPair(ctx, es, proj, cr, lb)
	} else {
		lb.settled = true // no appliance has ever held an address
	}
	// The first appliance of a cluster's pair serves alone until the second is built, and that one the platform
	// builds itself (lbClusterBuild): the row says so, where a tenant's own pair is told to replace its appliances.
	if err == nil && len(lb.Appliances) == 1 && lb.Status == lbStatusDegraded && !lbReplacing(lb, time.Now().UTC()) {
		lb.StatusMessage = "One appliance carries the traffic, with no standby yet. The platform builds the second one itself; after a build that failed it tries again in a few minutes."
	}
	if err == nil && lb.settled && len(lb.Cluster.Releasing) > 0 {
		err = h.lbReleaseVIPs(ctx, es, proj, lb)
	}
	// A Service is ready when the appliance that holds the addresses serves this config: the controller writes the
	// Service's address into its status on that word (drill N3-7), and the standby gets every config first.
	applied := lb.AppliedHash == lbAppliedHash(lb) && (lb.Status == lbStatusActive || lb.Status == lbStatusDegraded) &&
		slices.ContainsFunc(lb.Appliances, func(a lbAppliance) bool { return a.State == lbAppUp && a.HoldsVIP })
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
	settled = err == nil && lb.Status == lbStatusActive && len(lb.Cluster.Releasing) == 0 &&
		!slices.ContainsFunc(lb.Cluster.Services, func(s lbClusterSvc) bool { return s.State == lbgw.StateBuilding })
	return settled, err
}

// lbClusterDesired folds the state into the row: Services gone give up their address (kept for the cluster when they
// asked so, State.KeepGone), new ones are added in the order they come, each one that fits the config gets an address
// (the kept one it names, or one of the pool for a floatingIps quota), and the spec is made from them.
// No state (the controller never sent one) changes nothing.
func (h *Handler) lbClusterDesired(ctx context.Context, es *externalservice.ExternalService, proj *Project, lb *loadBalancer, st *lbgw.State) error {
	if st == nil {
		return nil
	}
	want := map[string]lbgw.Service{}
	for _, s := range st.Services {
		want[s.UID] = s
	}
	// An address no Service has any more is kept for the cluster when its Service asked so and the cluster has room
	// for one more, and given back otherwise.
	giveUp := func(ip string, keep bool) {
		if keep && len(lb.Cluster.Kept) < lbgw.MaxKeptAddresses {
			lb.Cluster.Kept = append(lb.Cluster.Kept, ip)
		} else {
			lb.Cluster.Releasing = append(lb.Cluster.Releasing, ip)
		}
	}
	var svcs []lbClusterSvc
	for _, s := range lb.Cluster.Services {
		if _, ok := want[s.UID]; ok {
			svcs = append(svcs, s)
		} else if s.VIP != "" {
			giveUp(s.VIP, slices.Contains(st.KeepGone, s.UID))
		}
	}
	for _, s := range st.Services {
		if !slices.ContainsFunc(svcs, func(c lbClusterSvc) bool { return c.UID == s.UID }) {
			svcs = append(svcs, lbClusterSvc{UID: s.UID})
		}
	}
	pn := pveAllowedPublicNetwork(es, proj, lb.PublicNetworkID)
	var certs map[string]*lbCertificate // the cluster's certificates, read when a Service ends TLS
	if slices.ContainsFunc(st.Services, func(s lbgw.Service) bool { return len(s.Certificates) > 0 }) {
		var err error
		if certs, err = h.lbClusterCertRowsOf(ctx, lb.Cluster.ID); err != nil {
			return err
		}
	}
	var spec pve.LBSpec
	var vips []string
	for i := range svcs {
		s := &svcs[i]
		w := want[s.UID]
		s.Namespace, s.Name, s.State, s.Reason, s.Message = w.Namespace, w.Name, lbgw.StateBuilding, "", ""
		refuse := func(reason, msg string) {
			s.State, s.Reason, s.Message = lbgw.StateRefused, reason, msg
			if s.VIP != "" {
				giveUp(s.VIP, lbgw.KeepsAddress(&w))
				s.VIP = ""
			}
		}
		vip := s.VIP
		if vip == "" {
			vip = lbSizePlaceholder
		}
		trial := lbClusterServiceSpecTLS(spec, w, vip, st.Nodes, lb.Cluster.ID, certs)
		with := append(slices.Clone(vips), vip)
		if len(pve.RenderClusterHAProxy(trial, with)) > pve.LBMaxConfig || len(pve.RenderLVS(trial, with)) > pve.LBMaxConfig {
			refuse(lbgw.ReasonTooLarge, "The load balancer's config would be too large with this Service: use fewer Services, ports or workers")
			continue
		}
		if i := slices.Index(lb.Cluster.Kept, w.LoadBalancerIP); s.VIP == "" && w.LoadBalancerIP != "" && i >= 0 {
			// The Service names an address its cluster keeps: it gets that one. The address is the balancer's
			// already, counted in the quota and billed, so neither the quota nor the pool is asked. Kept is this
			// cluster's row alone: an address of another cluster or project, or a free one, is never found here.
			s.VIP, lb.Cluster.Kept = w.LoadBalancerIP, slices.Delete(lb.Cluster.Kept, i, i+1)
			trial = lbClusterServiceSpecTLS(spec, w, s.VIP, st.Nodes, lb.Cluster.ID, certs)
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
			trial = lbClusterServiceSpecTLS(spec, w, s.VIP, st.Nodes, lb.Cluster.ID, certs)
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

// lbClusterServiceSpec adds Service w to spec: a listener on its address per port to a pool of the workers' node
// port, TCP ports in HAProxy and UDP ports in LVS (pve.RenderLVS). Policy Local is checked over HTTP on
// healthCheckNodePort, which kube-proxy answers 200 only with a local endpoint, for TCP and UDP ports alike; a check
// on its own port is sent without the PROXY header (C14). What the Service's annotations and affinity ask
// (lbgw.ServiceOptions) goes on every port it applies to: a UDP port takes no PROXY header, no timeouts and no
// forwarded-for header. Names are generated: nothing of the Service's reaches the config. A pool is named by its
// Service (a digest of the UID) and its node port, not by their places in the state (it was s<i>-<j>): HAProxy keeps
// a member's state over a reload by the pool's name, and a port or a Service that comes or goes must not rename the
// pools after it (drill N3-8). A UDP pool's name ends in -udp: Kubernetes gives a TCP and a UDP port of one number
// the same node port.
//
// A Service without annotations or affinity, which is every Service of a protocol 1 controller, makes the spec it
// made before protocol 2, field for field: its listener names no protocol (it reads its pool's) and its pool has no
// option set.
func lbClusterServiceSpec(spec pve.LBSpec, w lbgw.Service, vip string, nodes []string) pve.LBSpec {
	return lbClusterServiceSpecTLS(spec, w, vip, nodes, "", nil)
}

// lbClusterServiceSpecTLS is lbClusterServiceSpec with the certificates of cluster clusterID (by the hash of their
// Secret), for the TCP ports of w that end TLS (lbgw.Options.EndsTLS): such a port is an https listener to an HTTP
// pool, which adds the two forwarded headers, or with proxcloud.io/lb-backend-protocol: tcp a tls listener to a TCP
// pool. It presents the certificates of the Service's Secrets in their order, the first one to a client that asks
// for no name. Its members are spoken to without TLS either way, on the node port. The forwarded-for annotation
// says nothing on such a port: the backend protocol decides.
func lbClusterServiceSpecTLS(spec pve.LBSpec, w lbgw.Service, vip string, nodes []string, clusterID string, certs map[string]*lbCertificate) pve.LBSpec {
	out := pve.LBSpec{Listeners: slices.Clone(spec.Listeners), Pools: slices.Clone(spec.Pools)}
	o, _, _ := lbgw.ServiceOptions(&w) // the gateway refused what this refuses before the state was stored
	if w.Nodes != nil {                // the Service's node selector; only the cluster's own workers count (C11)
		nodes = slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return !slices.Contains(*w.Nodes, n) })
	}
	uid := sha256.Sum256([]byte(w.UID))
	for _, p := range w.Ports {
		udp := p.Protocol == "UDP"
		pool := pve.LBPool{Name: "s" + hex.EncodeToString(uid[:4]) + "-" + strconv.Itoa(p.NodePort), Protocol: "tcp", Algorithm: cmp.Or(o.Method, "roundrobin")}
		l := pve.LBListener{Port: p.Port, SourceRanges: w.SourceRanges, VIP: vip, ConnectionLimit: o.ConnectionLimit}
		switch {
		case udp:
			pool.Name += "-udp"
			pool.Protocol, l.Protocol = "udp", "udp"
		default:
			switch {
			case o.EndsTLS(&w, p):
				pool.Protocol, l.Protocol = "http", "https"
				if o.TLSBackendTCP {
					pool.Protocol, l.Protocol = "tcp", "tls"
				}
				l.Certificates, l.CertFiles = lbClusterCertNames(clusterID, w, certs)
			case o.XForwardedFor:
				pool.Protocol, l.Protocol = "http", "http"
			}
			pool.ProxyProtocol = o.ProxyVersion != 0
			if o.ProxyVersion == 1 { // 2 is the renderer's own, and what a row from before the annotation holds
				pool.ProxyVersion = 1
			}
			pool.TimeoutMemberConnect, pool.TimeoutMemberData, l.TimeoutClientData = o.TimeoutMemberConnect, o.TimeoutMemberData, o.TimeoutClientData
		}
		if o.AffinityTimeout > 0 {
			pool.SessionPersistence = &pve.LBPersistence{Type: "source_ip", Timeout: o.AffinityTimeout}
		}
		hc := pve.LBHealthCheck{Interval: o.HealthDelay, Timeout: o.HealthTimeout, Rise: o.HealthRise, Fall: o.HealthFall}
		if udp {
			// keepalived's check: a member is up again at its first good check and down after two failed ones at
			// least (pve.LBSpec.Validate), so "max-retries" has nothing to set and "max-retries-down: 1" is 2.
			hc.Rise = 0
			if hc.Fall == 1 {
				hc.Fall = 2
			}
		}
		switch {
		case w.ExternalTrafficPolicy == "Local":
			hc.Type, hc.Port, hc.Path = "http", w.HealthCheckNodePort, "/"
		case o.NoMonitor:
			hc = pve.LBHealthCheck{Type: "none"}
		case udp:
			hc.Type = "udp-connect"
		default:
			hc.Type = "tcp"
		}
		// No figure and no type of its own: the pool's check is the balancer's default, as before the annotations.
		if hc != (pve.LBHealthCheck{Type: "tcp"}) && hc != (pve.LBHealthCheck{Type: "udp-connect"}) {
			pool.HealthCheck = &hc
		}
		for _, n := range nodes {
			pool.Members = append(pool.Members, pve.LBMember{Node: true, Address: n, Port: p.NodePort, Weight: 1})
		}
		l.Pool = pool.Name
		out.Pools = append(out.Pools, pool)
		out.Listeners = append(out.Listeners, l)
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

// lbClusterReleaseKept gives back one address cluster clusterID of proj keeps: the tenant's
// RELEASE_LOAD_BALANCER_ADDRESS. The address goes the way a Service's does, through Releasing, so it returns to the
// pool, and stops being billed, once no appliance holds it (the pass that kept it may not have reached both yet).
// Only an address in the cluster's own Kept is released: not one a Service is on, and not another cluster's.
func (h *Handler) lbClusterReleaseKept(ctx context.Context, proj *Project, clusterID, ip string) error {
	notKept := httpx.NewError(http.StatusNotFound, http.StatusNotFound, "Not an address this cluster keeps")
	if h.lbGW == nil || h.sdnLock == nil || h.pveFor == nil {
		return notKept // no balancer can have been built
	}
	cred, err := h.lbGW.Credential(ctx, clusterID)
	if err != nil {
		return err
	}
	if cred == nil || cred.RevokedAt != nil || cred.ProjectID != proj.ID {
		return notKept
	}
	ext := lbClusterExternalID(clusterID)
	release, ok, err := h.lbLock(ctx, ext, lbLockWait)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, "The cluster's load balancer is being changed. Try again in a moment")
	}
	defer release()
	cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, cred.ComputeServiceID, ext)
	if err != nil {
		return err
	}
	if cr == nil {
		return notKept
	}
	lb, err := lbOfRow(cr)
	if err != nil {
		return err
	}
	i := -1
	if lb.Cluster != nil && lb.Cluster.ID == clusterID {
		i = slices.Index(lb.Cluster.Kept, ip)
	}
	if i < 0 {
		return notKept
	}
	lb.Cluster.Kept = slices.Delete(lb.Cluster.Kept, i, i+1)
	lb.Cluster.Releasing = append(lb.Cluster.Releasing, ip)
	if err := h.lbSave(ctx, cr, lb); err != nil {
		return err
	}
	h.lbPasses.Delete(clusterID) // the next pass has an address to give back, whatever the last one found
	go h.lbClusterConverge(context.WithoutCancel(ctx), clusterID, true)
	return nil
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
	release, ok, err := h.sdnLock.Hold(ctx, "lbc-"+clusterID, lbcLease, lbLockWait)
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
		unlock, ok, err := h.lbLock(ctx, ext, lbLockWait)
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
	h.lbPasses.Delete(clusterID)
	if h.lbCerts != nil { // the certificates of the cluster's Services, and their sealed keys, go with it
		if _, err := h.lbCerts.DeleteMany(ctx, pgdoc.M{"projectId": lbClusterCertOwner(clusterID)}); err != nil {
			return err
		}
	}
	if h.lbGW == nil {
		return nil
	}
	return h.lbGW.DeleteState(ctx, clusterID)
}

// The load balancer controller of a cluster (cmd/lb-controller, chart proxmox-kamaji-cluster from 0.1.17): what a cluster
// needs for one, the credential it presents, and the operator's "Enable load balancer controller" for an existing cluster.

// LBClusterView is a cluster's load balancer as the consoles show it next to the cluster: when its controller's
// credential was minted and last used, and the cluster's balancer row (none before the first Service). No token.
type LBClusterView struct {
	ControllerSince    *time.Time `json:"controllerSince,omitempty"`
	ControllerLastSeen *time.Time `json:"controllerLastSeen,omitempty"`
	// ControllerProtocol is the protocol of /lb/v1 the controller named at its last call (0 before the first one),
	// Protocol the portal's own. A controller below the portal's is the one of an earlier release: below
	// lbgw.ProtocolUDP its UDP ports are refused and none of its annotations is read, so no TLS from a Secret either.
	// A chart bump sets the controller's image to the portal's (kamaji.Service.SetChartVersion).
	ControllerProtocol int    `json:"controllerProtocol,omitempty"`
	Protocol           int    `json:"protocol"`
	BalancerID         string `json:"balancerId,omitempty"` // the LOAD_BALANCER row
	Balancer           any    `json:"balancer,omitempty"`   // its data.loadbalancer, as the tenant's list has it
	// Certificates are the certificates the cluster's Services end TLS with, by the id a listener of the balancer
	// names: the Secret each came from and what it says in public. Nothing of a key, and not the Secret's hash.
	Certificates []LBClusterCertView `json:"certificates,omitempty"`
	projectID    string
	kubernetesID string
}

// LBClusterCertView is one certificate of a cluster's Services as the consoles show it.
type LBClusterCertView struct {
	ID          string    `json:"id"`
	Secret      string    `json:"secret"` // the Secret it came from, as namespace/name
	Subject     string    `json:"subject"`
	DNSNames    []string  `json:"dnsNames"`
	NotBefore   time.Time `json:"notBefore"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

// lbClusterView reads cluster clusterID's view; nil when it has no controller (never minted, or released).
func (h *Handler) lbClusterView(ctx context.Context, clusterID string) (*LBClusterView, error) {
	if h.lbGW == nil {
		return nil, nil
	}
	cred, err := h.lbGW.Credential(ctx, clusterID)
	if err != nil || cred == nil || cred.RevokedAt != nil {
		return nil, err
	}
	v := &LBClusterView{ControllerSince: cred.CreatedAt, ControllerLastSeen: cred.LastSeenAt, ControllerProtocol: cred.Protocol,
		Protocol: lbgw.Protocol, projectID: cred.ProjectID, kubernetesID: cred.KubernetesServiceID}
	row, err := h.cloud.FindByServiceIDAndExternalID(ctx, cred.ComputeServiceID, lbClusterExternalID(clusterID))
	if err != nil || row == nil {
		return v, err
	}
	v.BalancerID, v.Balancer = row.ID, row.Data[lbDataKey]
	certs, err := h.lbClusterCertRowsOf(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	for _, c := range certs {
		if c.Refused == "" && c.File != "" { // a refused Secret holds no certificate: its Service says why
			v.Certificates = append(v.Certificates, LBClusterCertView{ID: c.ID, Secret: c.Name, Subject: c.Subject, DNSNames: c.DNSNames,
				NotBefore: c.NotBefore, NotAfter: c.NotAfter, Fingerprint: c.Fingerprint})
		}
	}
	slices.SortFunc(v.Certificates, func(a, b LBClusterCertView) int {
		return cmp.Or(cmp.Compare(a.Secret, b.Secret), cmp.Compare(a.ID, b.ID))
	})
	return v, nil
}

// ClusterLBView is the operator's read of one cluster of the kamaji provider es (admin.LBViewer); nil when the cluster
// has no controller.
func (h *Handler) ClusterLBView(ctx context.Context, es *externalservice.ExternalService, clusterID string) (any, error) {
	v, err := h.lbClusterView(ctx, clusterID)
	if err != nil || v == nil || v.kubernetesID != es.ID {
		return nil, err
	}
	return v, nil
}

// kamajiLBGate names the first thing that keeps a cluster of the Kubernetes provider k8s, with its workers on compute in
// proj, from a load balancer controller; "" when every gate holds: this portal holds the gateway's store, a gateway URL
// and the controller image, the provider pins a chart that renders the controller, the workers are the project's own,
// and the provider names a load balancer whose public network the project may use.
func (h *Handler) kamajiLBGate(cfg kamaji.Config, k8s, compute *externalservice.ExternalService, proj *Project) string {
	switch {
	case h.lbGW == nil:
		return "the portal keeps no load balancer gateway store"
	case h.csiGW == nil || h.csiGW.Endpoint.URL == "":
		return "the portal has no gateway URL (PROXCLOUD_CSI_GATEWAY_URL)"
	case cfg.LBControllerImage == "":
		return "the portal has no controller image (PROXCLOUD_LB_CONTROLLER_IMAGE)"
	case cfg.Defaults.Shared != nil:
		return "the provider runs sharedWorkers: the controller needs the project's own workers"
	case !kamaji.ChartHasLBController(cfg.ChartVersion):
		return fmt.Sprintf("the provider pins cluster chart %s; the controller needs %s or later", cfg.ChartVersion, kamaji.LBControllerChartVersion)
	}
	netID, _, ok := k8s.KamajiLoadBalancer()
	if !ok {
		return "the Kubernetes provider offers no load balancer (config.cluster.loadBalancer)"
	}
	if pveAllowedPublicNetwork(compute, proj, netID) == nil {
		return "public network " + netID + " is not available to the project"
	}
	return ""
}

// lbControllerSecret mints the cluster's credential (minting again replaces it: a rotation) and returns the data of the
// controller's Secret: the token it presents and the gateway's CA. The secret is in nothing else.
func (h *Handler) lbControllerSecret(ctx context.Context, cred lbgw.Credential) (map[string]string, error) {
	tokenID, secret, err := h.lbGW.MintCredential(ctx, cred)
	if err != nil {
		return nil, err
	}
	return map[string]string{"token": tokenID + "=" + secret, "ca.crt": h.csiGW.Endpoint.CA}, nil
}

// EnableClusterLB gives one existing cluster of the kamaji provider es its load balancer controller in class-only mode:
// Services of class proxcloud.io/lb get the product balancer, MetalLB keeps every Service without a class and its
// address. The chart is pinned to the provider's version (as SwitchCSIGateway, with the same transition check). Again
// on a cluster that has one: a new credential (the old one stops at once) and the portal's image, the mode unchanged.
// Refused, naming the gate, for sharedWorkers or a site that cannot serve.
func (h *Handler) EnableClusterLB(ctx context.Context, es *externalservice.ExternalService, ks *kamaji.Service, clusterID string) (any, error) {
	ctx = context.WithoutCancel(ctx)
	cfg := ks.Config()
	info, err := ks.ClusterNetwork(ctx, clusterID)
	if err != nil {
		return nil, httpx.NotFound(err.Error())
	}
	conflict := func(msg string) error { return httpx.NewError(http.StatusConflict, http.StatusConflict, msg) }
	proj, err := h.svc.GetProjectByID(ctx, info.ProjectID)
	if err != nil {
		return nil, err
	}
	if proj.Deleting() {
		return nil, conflict("The cluster's project is being deleted")
	}
	compute, err := h.kamajiCompute(ctx, proj, cfg.Defaults)
	if err != nil {
		return nil, err
	}
	if gate := h.kamajiLBGate(cfg, es, compute, proj); gate != "" {
		return nil, conflict("No load balancer controller: " + gate)
	}
	if info.Pool != compute.PoolFor(proj.ID) {
		return nil, conflict("No load balancer controller: cluster " + clusterID + " has its workers in " + info.Pool + ", not the project's pool")
	}
	nets, err := h.cloud.FindByProjectAndType(ctx, proj.ID, cloud.TypeNetwork)
	if err != nil {
		return nil, err
	}
	netRef := ""
	for i := range nets {
		if n, _ := nets[i].Data["network"].(map[string]any); nets[i].ServiceID == compute.ID && info.Bridge != "" && strAny(n["vnet"]) == info.Bridge {
			netRef = nets[i].ExternalID
			break
		}
	}
	if netRef == "" {
		return nil, conflict("No load balancer controller: the cluster's network " + info.Bridge + " is not a network of the project")
	}
	secret, err := h.lbControllerSecret(ctx, lbgw.Credential{ID: clusterID, ProjectID: proj.ID, ComputeServiceID: compute.ID,
		KubernetesServiceID: es.ID, NetworkID: netRef, Vnet: info.Bridge})
	if err != nil {
		return nil, err
	}
	lb := kamaji.LBController{Image: cfg.LBControllerImage, GatewayURL: h.csiGW.Endpoint.URL}
	if err := ks.EnableLBController(ctx, proj.ID, clusterID, cfg.ChartVersion, lb, secret); err != nil {
		return nil, upstreamErr("Kubernetes backend", err)
	}
	return map[string]any{"clusterId": clusterID, "chartVersion": cfg.ChartVersion, "image": cfg.LBControllerImage}, nil
}
