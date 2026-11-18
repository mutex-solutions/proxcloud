package project

// cloud_kamaji_workerfw.go — the Proxmox VM firewall of a Managed Kubernetes cluster's workers (docs/managed-k8s.md,
// "Worker firewall"). The rules and the per-VM writes are pve/workerfw.go; here is who decides: a new cluster when
// its provider has config.cluster.workerFirewall, an existing one when the operator protects it, and the provider
// sync for workers that join later.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// How long ProtectClusterWorkers watches the nodes after the firewall went on. A node's own firewall daemon reads a
// changed VM firewall within seconds (not measured on a lab), so the first look waits workerFirewallSettle. A kubelet
// renews its Lease every 10 seconds: one that has not within workerFirewallWatch no longer reaches its API server.
var (
	workerFirewallSettle = 15 * time.Second
	workerFirewallWatch  = 40 * time.Second
	workerFirewallPoll   = 5 * time.Second
)

// WorkerFirewallReport is the operator's view of one cluster's worker firewall.
type WorkerFirewallReport struct {
	ClusterID string `json:"clusterId"`
	// Protected: the cluster has its security group, so the provider sync keeps every running worker behind it.
	Protected bool   `json:"protected"`
	Group     string `json:"group,omitempty"`
	GroupName string `json:"groupName"`
	// Enforced: the datacenter firewall is on. Off, Proxmox applies no VM firewall at all.
	Enforced bool              `json:"enforced"`
	Workers  []pve.WorkerState `json:"workers"`
	// Verified: this call put the firewall on and every node that was Ready renewed its Lease afterwards.
	Verified bool `json:"verified,omitempty"`
}

// clusterAddresses are the addresses a cluster reserved on its vnet: workers first asked for, MetalLB's after.
func (h *Handler) clusterAddresses(ctx context.Context, computeID, clusterID string) ([]string, error) {
	if h.publicIPs == nil {
		return nil, nil
	}
	rows, err := h.publicIPs.ForServer(ctx, computeID, kamajiReservation(clusterID))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.IP)
	}
	slices.Sort(out)
	return out, nil
}

// workerFirewallTarget resolves a cluster of a kamaji provider to its project, its Proxmox service and what its
// workers may be reached from.
func (h *Handler) workerFirewallTarget(ctx context.Context, ks *kamaji.Service, clusterID string) (*pve.Service, pve.WorkerFirewall, []string, error) {
	var f pve.WorkerFirewall
	conflict := func(msg string, a ...any) error {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(msg, a...))
	}
	cfg := ks.Config()
	if cfg.Defaults.Shared != nil {
		return nil, f, nil, conflict("The provider runs sharedWorkers: its workers are not in a project's pool, so they get no worker firewall")
	}
	info, err := ks.ClusterNetwork(ctx, clusterID)
	if err != nil {
		return nil, f, nil, httpx.NotFound(err.Error())
	}
	proj, err := h.svc.GetProjectByID(ctx, info.ProjectID)
	if err != nil {
		return nil, f, nil, err
	}
	compute, err := h.kamajiCompute(ctx, proj, cfg.Defaults)
	if err != nil {
		return nil, f, nil, err
	}
	if info.Pool != compute.PoolFor(proj.ID) {
		return nil, f, nil, conflict("Cluster %s has its workers in %s, not the project's pool", clusterID, info.Pool)
	}
	ps, err := h.pveFor(compute)
	if err != nil {
		return nil, f, nil, err
	}
	addrs, err := h.clusterAddresses(ctx, compute.ID, clusterID)
	if err != nil {
		return nil, f, nil, err
	}
	f = pve.WorkerFirewall{ProjectID: proj.ID, ClusterID: clusterID, CIDRPool: compute.PVENetworkConfig().CIDRPool}
	for _, lb := range info.LoadBalancers {
		f.LoadBalancers = append(f.LoadBalancers, strings.TrimSuffix(lb, "/32"))
	}
	for _, a := range addrs {
		if !slices.Contains(f.LoadBalancers, a) {
			f.Workers = append(f.Workers, a)
		}
	}
	return ps, f, addrs, nil
}

// ClusterWorkerFirewall reports a cluster's worker firewall and writes nothing.
func (h *Handler) ClusterWorkerFirewall(ctx context.Context, ks *kamaji.Service, clusterID string) (*WorkerFirewallReport, error) {
	ps, f, addrs, err := h.workerFirewallTarget(ctx, ks, clusterID)
	if err != nil {
		return nil, err
	}
	return workerFirewallReport(ctx, ps, f, addrs)
}

func workerFirewallReport(ctx context.Context, ps *pve.Service, f pve.WorkerFirewall, addrs []string) (*WorkerFirewallReport, error) {
	rep := &WorkerFirewallReport{ClusterID: f.ClusterID, GroupName: pve.WorkerGroupName(f.ClusterID)}
	var err error
	if rep.Enforced, err = ps.C.DatacenterFirewallEnabled(ctx); err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	if rep.Group, err = ps.WorkerGroup(ctx, f.ProjectID, f.ClusterID); err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	rep.Protected = rep.Group != ""
	if rep.Workers, err = ps.ClusterWorkers(ctx, f.ProjectID, f.ClusterID, rep.Group, addrs); err != nil && rep.Workers == nil {
		return nil, upstreamErr(computeBackend, err)
	}
	return rep, nil
}

// workerNodes is the part of a tenant cluster's API the protect action reads (kamajik8s.Client).
type workerNodes interface {
	ListNodes(ctx context.Context) ([]map[string]any, error)
	ListCRs(ctx context.Context, group, version, plural, ns, labelSelector string) ([]map[string]any, error)
}

// ProtectClusterWorkers puts the workers of an existing cluster behind the VM firewall: the operator's action
// "Protect this cluster's workers". It refuses before it writes when the datacenter firewall is off or a Node's
// address is not one of the cluster's reserved addresses (the IP filter would cut that node off). After the write it
// watches every node that was Ready renew its Lease; when one does not, the firewall is taken off again and the
// answer says so. Running it on a protected cluster brings stragglers in and reports.
func (h *Handler) ProtectClusterWorkers(ctx context.Context, ks *kamaji.Service, clusterID string) (*WorkerFirewallReport, error) {
	ctx = context.WithoutCancel(ctx) // a browser leaving must not stop the watch, and with it the way back
	ps, f, addrs, err := h.workerFirewallTarget(ctx, ks, clusterID)
	if err != nil {
		return nil, err
	}
	kc, err := ks.TenantKubeconfig(ctx, f.ProjectID, clusterID)
	if err != nil {
		return nil, upstreamErr("Kubernetes backend", err)
	}
	tenant, err := kamajik8s.New(string(kc))
	if err != nil {
		return nil, err
	}
	return protectClusterWorkers(ctx, ps, tenant, f, addrs)
}

func protectClusterWorkers(ctx context.Context, ps *pve.Service, tenant workerNodes, f pve.WorkerFirewall, addrs []string) (*WorkerFirewallReport, error) {
	conflict := func(msg string, a ...any) error {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(msg, a...))
	}
	on, err := ps.C.DatacenterFirewallEnabled(ctx)
	if err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	switch {
	case !on:
		return nil, conflict("The datacenter firewall of this Proxmox cluster is off, so no VM firewall is enforced. Nothing was changed")
	case len(addrs) == 0 || len(f.Workers) == 0:
		return nil, conflict("Cluster %s has no reserved addresses in the portal's store. Nothing was changed", f.ClusterID)
	}
	for _, lb := range f.LoadBalancers {
		if !slices.Contains(addrs, lb) {
			return nil, conflict("MetalLB address %s of cluster %s is not one of its reserved addresses. Nothing was changed", lb, f.ClusterID)
		}
	}
	nodes, err := tenant.ListNodes(ctx)
	if err != nil {
		return nil, upstreamErr("Kubernetes backend", err)
	}
	var ready []string
	for _, n := range nodes {
		name := nodeStr(n, "metadata", "name")
		if ip := nodeInternalIP(n); !slices.Contains(f.Workers, ip) {
			return nil, conflict("Node %s reports address %q, which is not one of the cluster's reserved worker addresses: the IP filter would cut it off. Nothing was changed", name, ip)
		}
		if nodeReady(n) {
			ready = append(ready, name)
		}
	}
	before, err := ps.WorkerGroup(ctx, f.ProjectID, f.ClusterID)
	if err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	group, err := ps.EnsureWorkerGroup(ctx, f)
	if err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	// The way back is for a cluster this call protected. One that was protected already keeps its firewall: a worker
	// that fails here is a straggler, and taking the firewall off the others would open them for it.
	back := func(why string) error {
		if before != "" {
			return conflict("%s. The cluster was protected before and keeps its firewall", why)
		}
		if uerr := ps.UnsecureClusterWorkers(ctx, f.ProjectID, f.ClusterID); uerr != nil {
			slog.Error("kamaji: worker firewall could not be taken off again", "cluster", f.ClusterID, "err", uerr)
			return conflict("%s. Taking the firewall off again failed too (%v): use Remove the worker firewall", why, uerr)
		}
		return conflict("%s. The firewall was taken off again; the cluster is as it was", why)
	}
	states, err := ps.SecureClusterWorkers(ctx, f.ProjectID, f.ClusterID, group, addrs)
	if err != nil {
		return nil, back("The firewall could not be written on every worker (" + err.Error() + ")")
	}
	changed := slices.ContainsFunc(states, func(s pve.WorkerState) bool { return s.Changed })
	if changed {
		if late := nodesNotRenewing(ctx, tenant, ready); len(late) > 0 {
			return nil, back(fmt.Sprintf("After the firewall went on, %s did not renew the node Lease within %s",
				strings.Join(late, ", "), workerFirewallSettle+workerFirewallWatch))
		}
	}
	rep, err := workerFirewallReport(ctx, ps, f, addrs)
	if err != nil {
		return nil, err
	}
	rep.Verified = changed
	return rep, nil
}

// UnprotectClusterWorkers takes the firewall off a cluster's workers and deletes its group.
func (h *Handler) UnprotectClusterWorkers(ctx context.Context, ks *kamaji.Service, clusterID string) (*WorkerFirewallReport, error) {
	ctx = context.WithoutCancel(ctx)
	ps, f, addrs, err := h.workerFirewallTarget(ctx, ks, clusterID)
	if err != nil {
		return nil, err
	}
	if err := ps.UnsecureClusterWorkers(ctx, f.ProjectID, clusterID); err != nil {
		return nil, upstreamErr(computeBackend, err)
	}
	return workerFirewallReport(ctx, ps, f, addrs)
}

// ReconcileWorkerFirewalls is the provider sync's pass over one Proxmox provider (pve.ReconcileWorkerFirewalls).
func (h *Handler) ReconcileWorkerFirewalls(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService) error {
	return ps.ReconcileWorkerFirewalls(ctx, func(ctx context.Context, _, clusterID string) ([]string, error) {
		return h.clusterAddresses(ctx, es.ID, clusterID)
	})
}

// nodesNotRenewing names the nodes of watch whose kubelet did not renew its Lease after the firewall settled. The
// node controller reads the same Lease to call a node Ready, about a minute later. Leases that cannot be read prove
// nothing, so every node counts as late.
func nodesNotRenewing(ctx context.Context, tenant workerNodes, watch []string) []string {
	if len(watch) == 0 {
		return nil
	}
	renewals := func() map[string]string {
		leases, err := tenant.ListCRs(ctx, "coordination.k8s.io", "v1", "leases", "kube-node-lease", "")
		if err != nil {
			return nil
		}
		out := map[string]string{}
		for _, l := range leases {
			out[nodeStr(l, "metadata", "name")] = nodeStr(l, "spec", "renewTime")
		}
		return out
	}
	time.Sleep(workerFirewallSettle)
	first := renewals()
	late := slices.Clone(watch)
	for deadline := time.Now().Add(workerFirewallWatch); len(late) > 0 && first != nil; time.Sleep(workerFirewallPoll) {
		now := renewals()
		late = slices.DeleteFunc(late, func(n string) bool { return now[n] != "" && now[n] != first[n] })
		if time.Now().After(deadline) {
			break
		}
	}
	return late
}

func nodeStr(m map[string]any, keys ...string) string {
	for i, k := range keys {
		if i == len(keys)-1 {
			s, _ := m[k].(string)
			return s
		}
		m, _ = m[k].(map[string]any)
	}
	return ""
}

func nodeConditions(n map[string]any, list, typeKey, want, valueKey string) string {
	status, _ := n["status"].(map[string]any)
	items, _ := status[list].([]any)
	for _, raw := range items {
		if m, ok := raw.(map[string]any); ok && m[typeKey] == want {
			s, _ := m[valueKey].(string)
			return s
		}
	}
	return ""
}

func nodeInternalIP(n map[string]any) string {
	return nodeConditions(n, "addresses", "type", "InternalIP", "address")
}

func nodeReady(n map[string]any) bool {
	return nodeConditions(n, "conditions", "type", "Ready", "status") == "True"
}
