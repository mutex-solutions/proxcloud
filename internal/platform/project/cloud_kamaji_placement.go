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
// disk is the bytes per node their root disks will take on storage: only clusters whose worker disks go there
// (values.proxmox.storage) count, again with the cluster's largest root disk.
func kamajiPendingWorkers(clusters []kamaji.ClusterValues, vms []pve.ClusterResource, region, storage string) (out []pve.ClusterResource, disk map[string]int64) {
	disk = map[string]int64{}
	for _, c := range clusters {
		if digAnyStr(c.Values, "proxmox", "region") != region {
			continue
		}
		want := map[string]int{}
		cores, mem, root := 0, 0, 0
		for _, g := range kamaji.GroupPlacements(c.Values) {
			for node, n := range g.Baseline() {
				want[node] += n
			}
			cores, mem, root = max(cores, g.Cores), max(mem, g.MemoryMiB), max(root, g.DiskGiB)
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
				if digAnyStr(c.Values, "proxmox", "storage") == storage {
					disk[node] += int64(root) << 30
				}
			}
		}
	}
	return out, disk
}

// kamajiDebitPendingDisk takes the root disks of pending workers off the storage state the placement reads: Proxmox
// still shows that space free. A shared storage is one pool seen from each node, so every node's state loses all of
// it.
func kamajiDebitPendingDisk(st map[string]pve.StorageNodeState, disk map[string]int64, shared bool) {
	var all int64
	for _, b := range disk {
		all += b
	}
	for node, state := range st {
		if shared {
			state.Avail -= all
		} else {
			state.Avail -= disk[node]
		}
		st[node] = state
	}
}

// holdsTemplate: does the snapshot show template vmid on node? A template that was removed, or moved, cannot be
// cloned there whatever the provider's version matrix still says.
func (c *kamajiCapacity) holdsTemplate(node string, vmid int) bool {
	return slices.ContainsFunc(c.vms, func(r pve.ClusterResource) bool {
		return r.Type == "qemu" && r.Template == 1 && r.Node == node && r.VMID == vmid
	})
}

// kamajiPlaceGroups decides the placement of every group of next, in order. prev is what the cluster runs (nil at
// create). snapshot is asked for only when a group has replicas to place; the groups of one call share it and what
// they take from it. unready is per group and node (nil when unknown). freeMem is each node's free memory for the
// scale-down order, asked for at most once and only when a group shrinks and no snapshot was taken; it may be nil or
// return nil, and that criterion is then skipped. All or nothing: the first group that does not fit refuses the
// request.
func kamajiPlaceGroups(prev, next []kamaji.GroupPlacement, snapshot func() (*kamajiCapacity, *httpx.HTTPError), unready map[string]map[string]int, freeMem func() map[string]int64) ([]map[string]int, *httpx.HTTPError) {
	var snap *kamajiCapacity
	var free map[string]int64
	freeRead := false
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
			switch {
			case snap != nil:
				free = snap.freeMem()
			case !freeRead && freeMem != nil:
				free, freeRead = freeMem(), true
			}
			have = pve.RemoveReplicas(have, -delta, unready[g.Name], free)
		default:
			if snap == nil {
				var herr *httpx.HTTPError
				if snap, herr = snapshot(); herr != nil {
					return nil, herr
				}
			}
			allowed, withStorage, templates := map[string]bool{}, 0, kamaji.ImageTemplates(g.Image)
			for _, node := range nodes {
				if !snap.storage[node].Active {
					continue
				}
				withStorage++
				if snap.holdsTemplate(node, templates[node]) {
					allowed[node] = true
				}
			}
			if withStorage == 0 {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict,
					fmt.Sprintf("Node group %q: no node that holds this version's image has the cluster's storage. Contact the operator.", g.Name))
			}
			if len(allowed) == 0 {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict,
					fmt.Sprintf("Node group %q: no node still holds this version's image. Contact the operator.", g.Name))
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
func kamajiApplyPlacement(prev []kamaji.GroupPlacement, rendered []any, snapshot func() (*kamajiCapacity, *httpx.HTTPError), unready map[string]map[string]int, freeMem func() map[string]int64) error {
	placed, herr := kamajiPlaceGroups(prev, kamaji.GroupPlacements(map[string]any{"nodeGroups": rendered}), snapshot, unready, freeMem)
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

// kamajiPlaceCreate places the node groups of a cluster about to be made and writes each decision into the spec,
// from where BuildValues renders it.
func kamajiPlaceCreate(d kamaji.ClusterDefaults, spec *kamaji.ClusterSpec, snapshot func() (*kamajiCapacity, *httpx.HTTPError)) error {
	rendered := kamaji.NodeGroupValues(d, spec.Version, spec.NodeGroups)
	if err := kamajiApplyPlacement(nil, rendered, snapshot, nil, nil); err != nil {
		return err
	}
	for i, g := range kamaji.GroupPlacements(map[string]any{"nodeGroups": rendered}) {
		spec.NodeGroups[i].Placement = g.Placement
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
	pending, disk := kamajiPendingWorkers(clusters, vms, compute.PVERegion(), storage)
	kamajiDebitPendingDisk(st, disk, caps[storage].Shared)
	return &kamajiCapacity{
		nodes: nodes, vms: append(vms, pending...),
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

// kamajiPlacer is what a values patch of an existing cluster reads to place its node groups. Every read is a func, so
// a patch that places nothing reads nothing, and the tests put a site in their place.
type kamajiPlacer struct {
	// computeErr is why the project's compute provider could not be resolved, which only matters past the gate.
	computeErr error
	// unready counts, per group and node, the replicas that are asked for and not ready (nil when unknown).
	unready func() map[string]map[string]int
	// snapshot reads the compute provider for the storage the cluster's worker disks go to.
	snapshot func(storage string) (*kamajiCapacity, *httpx.HTTPError)
	// freeMem is each node's free memory, for the scale-down order; nil when it could not be read.
	freeMem func() map[string]int64
}

// edit runs inside a values patch, after the node groups are rendered and their images settled: on a cluster whose
// own chart reads machinePlacement it places rendered against prev, the groups the live values held. On an older
// chart it writes nothing and reads nothing, exactly as before placement existed.
func (p kamajiPlacer) edit(chartVersion string, prev []kamaji.GroupPlacement, values map[string]any, rendered []any) error {
	if !kamaji.ChartHasPlacement(chartVersion) {
		return nil
	}
	if p.computeErr != nil {
		return p.computeErr
	}
	storage := digAnyStr(values, "proxmox", "storage")
	return kamajiApplyPlacement(prev, rendered, func() (*kamajiCapacity, *httpx.HTTPError) { return p.snapshot(storage) }, p.unready(), p.freeMem)
}

// kamajiPlacer wires the placement reads of one cluster to the management cluster and the compute provider.
func (h *Handler) kamajiPlacer(ctx context.Context, ks *kamaji.Service, compute *externalservice.ExternalService, computeErr error, proj *Project, clusterID string) kamajiPlacer {
	return kamajiPlacer{
		computeErr: computeErr,
		unready: func() map[string]map[string]int {
			unready, err := ks.UnreadyReplicas(ctx, proj.ID, clusterID)
			if err != nil {
				slog.Warn("kamaji: MachineDeployments not read, scale-down goes by replica count", "cluster", clusterID, "err", err)
			}
			return unready
		},
		snapshot: func(storage string) (*kamajiCapacity, *httpx.HTTPError) {
			return h.kamajiSnapshot(ctx, ks, compute, storage)
		},
		// Best effort: a scale-down must work while Proxmox does not answer.
		freeMem: func() map[string]int64 {
			ps, err := h.pveFor(compute)
			if err != nil {
				return nil
			}
			nodes, nerr := ps.C.Nodes(ctx)
			vms, verr := ps.C.ClusterResources(ctx, "vm")
			if nerr != nil || verr != nil {
				slog.Warn("kamaji: Proxmox not read, scale-down goes without free memory", "cluster", clusterID)
				return nil
			}
			return (&kamajiCapacity{nodes: nodes, vms: vms}).freeMem()
		},
	}
}

// kamajiUpgradeValues is the values patch of UPGRADE: one minor at a time, every group's image rotated to the new
// version's, and the workers on a node that has no image of it placed again.
func kamajiUpgradeValues(values map[string]any, chartVersion, version string, d kamaji.ClusterDefaults, p kamajiPlacer) error {
	current, _ := values["kubernetesVersion"].(string)
	// One minor at a time, never backwards (plan §3.4) — validated against the live
	// values, not the cache, so a stale sync can't let a bad jump through.
	if err := kamaji.ValidateUpgradePath(current, version); err != nil {
		return httpx.BadRequest(err.Error())
	}
	values["kubernetesVersion"] = version
	prevPlaced := kamaji.GroupPlacements(values)
	// Node images follow the curated version→image matrix: rotate every group to the new
	// version's image (a MachineDeployment template rotation = CAPI rolling replace). A
	// group with an image VARIANT stays on that variant — resolved against the target
	// version, and the upgrade is refused when the variant has no image there, because
	// silently rolling a GPU pool onto the plain image is a broken pool with extra steps.
	if err := rotateNodeGroupImages(values, d, version); err != nil {
		return err
	}
	// The target version's image may sit on other nodes: workers on a node that has none are placed again
	// by capacity; the rest stay where they are.
	rendered, _ := values["nodeGroups"].([]any)
	return p.edit(chartVersion, prevPlaced, values, rendered)
}

// kamajiSetGroupsValues is the values patch of SET_NODE_GROUPS.
func kamajiSetGroupsValues(values map[string]any, chartVersion string, groups []kamaji.NodeGroup, d kamaji.ClusterDefaults, p kamajiPlacer) error {
	version, _ := values["kubernetesVersion"].(string)
	prevFlavor, prevImage, prevVariant := prevGroupIndex(values)
	prevPlaced := kamaji.GroupPlacements(values)
	// Flavor allowlist applies to NEW or flavor-CHANGED groups only — an admin narrowing
	// the allowlist must not brick resizes of groups that already run a since-removed
	// flavor.
	if len(d.Flavors) > 0 {
		for _, ng := range groups {
			if prevFlavor[ng.Name] == ng.FlavorID {
				continue
			}
			if !slices.Contains(d.Flavors, ng.FlavorID) {
				return httpx.BadRequest(fmt.Sprintf("node group %q: flavor %q is not offered by this provider", ng.Name, ng.FlavorID))
			}
		}
	}
	rendered := kamaji.NodeGroupValues(d, version, groups)
	if err := applyNodeGroupImages(rendered, prevImage, prevVariant, version); err != nil {
		return err
	}
	// Placement is decided here, from the live values and not the cached row: existing workers stay where
	// they are, only the difference is placed or removed.
	if err := p.edit(chartVersion, prevPlaced, values, rendered); err != nil {
		return err
	}
	values["nodeGroups"] = rendered
	return nil
}
