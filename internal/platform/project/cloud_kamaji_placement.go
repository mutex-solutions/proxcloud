package project

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// cloud_kamaji_placement.go decides how many replicas of a node group each PVE node runs. PVE clones a node image
// to node-local storage only on the template's own node, so a group is one MachineDeployment per node that holds the
// image. The chart alone splits the count evenly, blind to what is free: on a site without shared storage a full node
// still gets its share and its machines hang. From cluster chart 0.1.13 the portal writes the split
// (machinePlacement) by free memory, CPU and storage, and keeps it stable: a re-render never moves a running worker.
//
// The decision is the pure kamajiPlaceGroups; the handler only gathers the snapshot (kamajiSnapshot) under one lock
// per compute provider, so two creates cannot both take the last free node.

const (
	kamajiPlaceLockMaxFor = 2 * time.Minute
	kamajiPlaceLockWait   = 30 * time.Second
)

var errPVEUnavailable = httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "proxmox cluster unavailable")

// kamajiCapacity is one snapshot of a compute provider for placing workers: its nodes, every VM with the workers that
// are decided and not cloned yet, and the state per node of the storage the cluster's worker disks go to.
type kamajiCapacity struct {
	nodes      []pve.Node
	vms        []pve.ClusterResource
	storage    map[string]pve.StorageNodeState // node → state of the cluster's storage there; a node without it is absent
	shared     bool
	reserve    map[string]int64
	overcommit float64
	headroom   float64
}

// freeMem is each node's memory after what its VMs are given, for the scale-down order.
func (c *kamajiCapacity) freeMem() map[string]int64 {
	out := map[string]int64{}
	for _, n := range c.nodes {
		out[n.Node] = n.MaxMem
	}
	for _, r := range c.vms {
		if r.Type == "qemu" && r.Template != 1 {
			out[r.Node] -= r.MaxMem
		}
	}
	return out
}

// kamajiPendingWorkers are the workers Cluster API has been asked for and Proxmox does not have yet, as VMs: a clone
// takes minutes, and the next create must not count that node as free. Per cluster of the region and per node: what
// its groups place there (the stored placement, else the even split) less its VMs there (tag pc-k8s-<id>, which
// kamajiHydrateGroups puts on every worker). Each missing one counts with the cluster's largest group size.
func kamajiPendingWorkers(clusters []kamaji.ClusterValues, vms []pve.ClusterResource, region string) []pve.ClusterResource {
	var out []pve.ClusterResource
	for _, c := range clusters {
		if digAnyStr(c.Values, "proxmox", "region") != region {
			continue
		}
		want := map[string]int{}
		cores, mem := 0, 0
		for _, g := range kamaji.GroupPlacements(c.Values) {
			for node, n := range g.Baseline() {
				want[node] += n
			}
			cores, mem = max(cores, g.Cores), max(mem, g.MemoryMiB)
		}
		for _, r := range vms {
			if r.Type == "qemu" && r.Template != 1 && pve.HasTag(r.Tags, "pc-k8s-"+c.ID) {
				want[r.Node]--
			}
		}
		nodes := make([]string, 0, len(want))
		for node := range want {
			nodes = append(nodes, node)
		}
		slices.Sort(nodes)
		for _, node := range nodes {
			for i := 0; i < want[node]; i++ {
				out = append(out, pve.ClusterResource{Type: "qemu", Node: node, Name: c.ID + "-pending", MaxCPU: float64(cores), MaxMem: int64(mem) << 20})
			}
		}
	}
	return out
}

// kamajiPlaceGroups decides the placement of every group of next, in order. prev is what the cluster runs (nil at
// create). snapshot is asked for only when a group has replicas to place; the groups of one call share it and what
// they take from it. unready is per group and node (nil when unknown). All or nothing: the first group that does not
// fit refuses the request.
func kamajiPlaceGroups(prev, next []kamaji.GroupPlacement, snapshot func() (*kamajiCapacity, *httpx.HTTPError), unready map[string]map[string]int) ([]map[string]int, *httpx.HTTPError) {
	var snap *kamajiCapacity
	out := make([]map[string]int, 0, len(next))
	for _, g := range next {
		nodes := g.Nodes()
		// Where its machines are now. Replicas on a node the image string no longer names are placed again as new.
		have, total := map[string]int{}, 0
		if i := slices.IndexFunc(prev, func(p kamaji.GroupPlacement) bool { return p.Name == g.Name }); i >= 0 {
			for node, n := range prev[i].Baseline() {
				if slices.Contains(nodes, node) && n > 0 {
					have[node] = n
					total += n
				}
			}
		}
		switch delta := g.Count - total; {
		case delta == 0:
		case delta < 0:
			var free map[string]int64
			if snap != nil {
				free = snap.freeMem()
			}
			have = pve.RemoveReplicas(have, -delta, unready[g.Name], free)
		default:
			if snap == nil {
				var herr *httpx.HTTPError
				if snap, herr = snapshot(); herr != nil {
					return nil, herr
				}
			}
			allowed := map[string]bool{}
			for _, node := range nodes {
				if snap.storage[node].Active {
					allowed[node] = true
				}
			}
			if len(allowed) == 0 {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict,
					fmt.Sprintf("Node group %q: no node that holds this version's image has the cluster's storage. Contact the operator.", g.Name))
			}
			fit := 0
			have, snap.vms, fit = pve.PlaceReplicas(snap.nodes, snap.vms, pve.SchedReq{
				VCPUs: g.Cores, RAMMb: int64(g.MemoryMiB), OvercommitCPU: snap.overcommit, AllowedNodes: allowed,
				Storage: snap.storage, DiskBytes: int64(g.DiskGiB) << 30, HeadroomPct: snap.headroom, ReserveMem: snap.reserve,
			}, have, delta, snap.shared)
			if fit < delta {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf(
					"Node group %q: only %d of %d new workers fit (flavor %s: %d vCPU, %d MB, %d GiB disk). Reduce the count, choose a smaller flavor, or contact the operator.",
					g.Name, fit, delta, g.Flavor, g.Cores, g.MemoryMiB, g.DiskGiB))
			}
		}
		out = append(out, have)
	}
	return out, nil
}

// kamajiApplyPlacement places the groups of rendered (NodeGroupValues' output, images resolved) against prev and
// writes machinePlacement into each.
func kamajiApplyPlacement(prev []kamaji.GroupPlacement, rendered []any, snapshot func() (*kamajiCapacity, *httpx.HTTPError), unready map[string]map[string]int) error {
	placed, herr := kamajiPlaceGroups(prev, kamaji.GroupPlacements(map[string]any{"nodeGroups": rendered}), snapshot, unready)
	if herr != nil {
		return herr
	}
	if len(placed) != len(rendered) {
		return fmt.Errorf("kamaji: %d node groups rendered, %d placed", len(rendered), len(placed))
	}
	for i, raw := range rendered {
		g, _ := raw.(map[string]any)
		if err := kamaji.SetPlacement(g, placed[i]); err != nil {
			return err
		}
	}
	return nil
}

// kamajiSnapshot reads the compute provider for a placement: nodes, VMs, the state of storage on each node, the
// memory kept for failover, and the workers of every cluster of ks that are decided and not built yet.
// ponytail: the pending workers of ONE Kubernetes provider. Two providers on one Proxmox cluster do not see each
// other's pending clones; list every kamaji service of the compute provider if a site runs that.
func (h *Handler) kamajiSnapshot(ctx context.Context, ks *kamaji.Service, compute *externalservice.ExternalService, storage string) (*kamajiCapacity, *httpx.HTTPError) {
	ps, err := h.pveFor(compute)
	if err != nil {
		return nil, errPVEUnavailable
	}
	nodes, nerr := ps.C.Nodes(ctx)
	vms, verr := ps.C.ClusterResources(ctx, "vm")
	caps, cerr := ps.C.DetectStorage(ctx)
	if nerr != nil || verr != nil || cerr != nil {
		return nil, errPVEUnavailable
	}
	reserve, err := ps.ReserveMem(ctx, vms)
	if err != nil {
		return nil, errPVEUnavailable
	}
	clusters, err := ks.ListClusterValues(ctx)
	if err != nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, "managed kubernetes is not available")
	}
	st := map[string]pve.StorageNodeState{}
	for node, state := range caps[storage].Nodes {
		if state.Active {
			st[node] = state
		}
	}
	return &kamajiCapacity{
		nodes: nodes, vms: append(vms, kamajiPendingWorkers(clusters, vms, compute.PVERegion())...),
		storage: st, shared: caps[storage].Shared, reserve: reserve,
		overcommit: compute.PVEOvercommitCPU(), headroom: compute.PVEStorageHeadroomPct(),
	}, nil
}

// kamajiPlaceLock serializes placement decisions on one compute provider, from the snapshot to the Application that
// records the decision: the snapshot of the next request then sees it as pending workers.
func (h *Handler) kamajiPlaceLock(ctx context.Context, computeID string) (func(), error) {
	if h.sdnLock == nil {
		return func() {}, nil
	}
	release, ok, err := h.sdnLock.Wait(ctx, "k8s-place-"+computeID, kamajiPlaceLockMaxFor, kamajiPlaceLockWait)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Another cluster change is being placed on this provider. Try again in a moment.")
	}
	return release, nil
}

// kamajiPlaceEdit runs inside a values patch of an existing cluster, after the node groups are rendered and their
// images settled: on a cluster whose own chart reads machinePlacement it places rendered against prev, the groups the
// live values held. On an older chart it writes nothing and reads nothing, exactly as before placement existed.
// computeErr is why the project's compute provider could not be resolved, which only matters past the gate.
func (h *Handler) kamajiPlaceEdit(ctx context.Context, ks *kamaji.Service, compute *externalservice.ExternalService, computeErr error, proj *Project, clusterID, chartVersion string, prev []kamaji.GroupPlacement, values map[string]any, rendered []any) error {
	if !kamaji.ChartHasPlacement(chartVersion) {
		return nil
	}
	if computeErr != nil {
		return computeErr
	}
	unready, err := ks.UnreadyReplicas(ctx, proj.ID, clusterID)
	if err != nil {
		slog.Warn("kamaji: MachineDeployments not read, scale-down goes by replica count", "cluster", clusterID, "err", err)
	}
	storage := digAnyStr(values, "proxmox", "storage")
	return kamajiApplyPlacement(prev, rendered, func() (*kamajiCapacity, *httpx.HTTPError) {
		return h.kamajiSnapshot(ctx, ks, compute, storage)
	}, unready)
}
