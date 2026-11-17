package project

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

const placeGiB = int64(1) << 30

// placeSite is a site without shared storage: three nodes, each with its own pool "local-zfs".
func placeSite(memGiB ...int64) *kamajiCapacity {
	c := &kamajiCapacity{storage: map[string]pve.StorageNodeState{}, overcommit: 4, headroom: 10}
	for i, m := range memGiB {
		name := []string{"pve1", "pve2", "pve3"}[i]
		c.nodes = append(c.nodes, pve.Node{Node: name, Status: "online", MaxCPU: 16, MaxMem: m * placeGiB})
		c.storage[name] = pve.StorageNodeState{Active: true, Total: 1000 * placeGiB, Avail: 900 * placeGiB}
		// Every node holds the node image of two Kubernetes versions: 920x and 930x.
		c.vms = append(c.vms, pve.ClusterResource{Type: "qemu", Template: 1, Node: name, VMID: 9201 + i},
			pve.ClusterResource{Type: "qemu", Template: 1, Node: name, VMID: 9301 + i})
	}
	return c
}

func placeGroup(name string, count int, image string, placement map[string]int) kamaji.GroupPlacement {
	return kamaji.GroupPlacement{Name: name, Flavor: "small", Count: count, Image: image, Placement: placement, Cores: 2, MemoryMiB: 8192, DiskGiB: 40}
}

func TestKamajiPlaceGroups(t *testing.T) {
	const three = "pve1=9201,pve2=9202,pve3=9203"
	calls := 0
	snap := func(c *kamajiCapacity) func() (*kamajiCapacity, *httpx.HTTPError) {
		return func() (*kamajiCapacity, *httpx.HTTPError) { calls++; return c, nil }
	}
	one := func(prev, next kamaji.GroupPlacement, c *kamajiCapacity, unready map[string]int) (map[string]int, *httpx.HTTPError) {
		var p []kamaji.GroupPlacement
		if prev.Name != "" {
			p = []kamaji.GroupPlacement{prev}
		}
		out, herr := kamajiPlaceGroups(p, []kamaji.GroupPlacement{next}, snap(c), map[string]map[string]int{next.Name: unready}, nil)
		if herr != nil {
			return nil, herr
		}
		return out[0], nil
	}
	eq := func(name string, got map[string]int, want map[string]int) {
		t.Helper()
		for _, node := range []string{"pve1", "pve2", "pve3"} {
			if got[node] != want[node] {
				t.Errorf("%s: got %v, want %v", name, got, want)
				return
			}
		}
	}

	// Create: a full node gets nothing, where the even split gave it a third and its machines hung.
	site := placeSite(64, 64, 64)
	site.vms = append(site.vms, pve.ClusterResource{Type: "qemu", Node: "pve2", MaxCPU: 8, MaxMem: 60 * placeGiB})
	got, herr := one(kamaji.GroupPlacement{}, placeGroup("workers", 4, three, nil), site, nil)
	if herr != nil {
		t.Fatal(herr)
	}
	eq("create around a full node", got, map[string]int{"pve1": 2, "pve3": 2})

	// An unchanged count writes the baseline as it is and asks Proxmox nothing: an existing cluster's first edit
	// (no stored placement) gets the even split it already runs.
	calls = 0
	got, _ = one(placeGroup("workers", 5, three, nil), placeGroup("workers", 5, three, nil), nil, nil)
	eq("first edit, same count", got, map[string]int{"pve1": 2, "pve2": 2, "pve3": 1})
	got, _ = one(placeGroup("workers", 3, three, map[string]int{"pve1": 3}), placeGroup("workers", 3, three, nil), nil, nil)
	eq("stored placement kept", got, map[string]int{"pve1": 3})
	if calls != 0 {
		t.Errorf("an edit with nothing to place read the provider %d times", calls)
	}

	// Scale-up places only the new replicas: the three on pve1 stay.
	got, _ = one(placeGroup("workers", 3, three, map[string]int{"pve1": 3}), placeGroup("workers", 5, three, nil), placeSite(64, 64, 64), nil)
	eq("scale-up", got, map[string]int{"pve1": 3, "pve2": 1, "pve3": 1})

	// Scale-down needs no snapshot and takes the stuck replica first.
	calls = 0
	got, _ = one(placeGroup("workers", 3, three, map[string]int{"pve1": 2, "pve2": 1}), placeGroup("workers", 2, three, nil), nil, map[string]int{"pve2": 1})
	eq("scale-down", got, map[string]int{"pve1": 2})
	if calls != 0 {
		t.Errorf("scale-down read the provider %d times", calls)
	}

	// The image string gains a node: nothing moves, the new node starts at 0.
	got, _ = one(placeGroup("workers", 2, "pve1=9201,pve2=9202", map[string]int{"pve1": 1, "pve2": 1}), placeGroup("workers", 2, three, nil), nil, nil)
	eq("image on one more node", got, map[string]int{"pve1": 1, "pve2": 1})
	// It loses a node: that node's replicas are placed again by capacity, the others stay.
	got, _ = one(placeGroup("workers", 4, three, map[string]int{"pve1": 1, "pve2": 1, "pve3": 2}), placeGroup("workers", 4, "pve1=9301,pve2=9302", nil), placeSite(64, 64, 64), nil)
	eq("image dropped from a node", got, map[string]int{"pve1": 2, "pve2": 2})
	// With no room for them the edit is refused before anything is pruned.
	if _, herr := one(placeGroup("workers", 4, three, map[string]int{"pve1": 1, "pve2": 1, "pve3": 2}), placeGroup("workers", 4, "pve1=9301,pve2=9302", nil), placeSite(4, 4, 64), nil); herr == nil || herr.Status != http.StatusConflict {
		t.Errorf("dropped node without room elsewhere: %v", herr)
	}

	// Refusals name the group, how many fit and the size.
	_, herr = one(kamaji.GroupPlacement{}, placeGroup("batch", 5, three, nil), placeSite(8, 8, 4), nil)
	if herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, `Node group "batch": only 2 of 5 new workers fit (flavor small: 2 vCPU, 8192 MB, 40 GiB disk)`) {
		t.Errorf("no capacity: %v", herr)
	}
	// The cluster's storage is on none of the image's nodes.
	bare := placeSite(64, 64, 64)
	bare.storage = map[string]pve.StorageNodeState{}
	_, herr = one(kamaji.GroupPlacement{}, placeGroup("workers", 1, three, nil), bare, nil)
	if herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "has the cluster's storage") {
		t.Errorf("no storage: %v", herr)
	}
	// A node without that storage is skipped; the others take the group.
	partial := placeSite(64, 64, 64)
	delete(partial.storage, "pve1")
	got, _ = one(kamaji.GroupPlacement{}, placeGroup("workers", 2, three, nil), partial, nil)
	eq("node without the storage", got, map[string]int{"pve2": 1, "pve3": 1})
	// A node that holds no image never gets a replica, however free.
	got, _ = one(kamaji.GroupPlacement{}, placeGroup("workers", 2, "pve1=9201", nil), placeSite(8, 64, 64), nil)
	if got != nil {
		t.Errorf("placed %v on a node without the image; pve1 has room for none", got)
	}

	// A node whose template is gone gets no new replica, whatever the version matrix still says: it could never clone.
	gone := placeSite(64, 64, 64)
	gone.vms = slices.DeleteFunc(gone.vms, func(r pve.ClusterResource) bool { return r.VMID == 9202 })
	got, herr = one(kamaji.GroupPlacement{}, placeGroup("workers", 2, "pve1=9201,pve2=9202", nil), gone, nil)
	if herr != nil {
		t.Fatal(herr)
	}
	eq("template gone from one node", got, map[string]int{"pve1": 2})
	// The VMID must be the template of that node, not a template of the same number elsewhere or a plain VM.
	gone.vms = append(gone.vms, pve.ClusterResource{Type: "qemu", Template: 1, Node: "pve3", VMID: 9202}, pve.ClusterResource{Type: "qemu", Node: "pve2", VMID: 9202})
	got, _ = one(kamaji.GroupPlacement{}, placeGroup("workers", 2, "pve1=9201,pve2=9202", nil), gone, nil)
	eq("the number on another node", got, map[string]int{"pve1": 2})
	// On no node at all: refused, and said so.
	_, herr = one(kamaji.GroupPlacement{}, placeGroup("workers", 1, "pve1=9401,pve2=9402", nil), placeSite(64, 64, 64), nil)
	if herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, `Node group "workers": no node still holds this version's image`) {
		t.Errorf("no template left: %v", herr)
	}
	// Existing workers on a node that lost its template are left alone by an edit that places nothing.
	got, _ = one(placeGroup("workers", 2, "pve1=9201,pve2=9202", map[string]int{"pve1": 1, "pve2": 1}), placeGroup("workers", 2, "pve1=9201,pve2=9202", nil), nil, nil)
	eq("no placing, no template check", got, map[string]int{"pve1": 1, "pve2": 1})

	// A scale-down alone reads no snapshot; it asks for the nodes' free memory, once for the whole request, and takes
	// the replica off the node with the least. Without an answer that criterion is skipped: by name.
	asked := 0
	freeMem := func(m map[string]int64) func() map[string]int64 {
		return func() map[string]int64 { asked++; return m }
	}
	even := map[string]int{"pve1": 2, "pve2": 2}
	down := func(free func() map[string]int64) []map[string]int {
		calls = 0
		out, herr := kamajiPlaceGroups(
			[]kamaji.GroupPlacement{placeGroup("a", 4, "pve1=9201,pve2=9202", even), placeGroup("b", 4, "pve1=9201,pve2=9202", even)},
			[]kamaji.GroupPlacement{placeGroup("a", 3, "pve1=9201,pve2=9202", nil), placeGroup("b", 3, "pve1=9201,pve2=9202", nil)}, snap(nil), nil, free)
		if herr != nil || calls != 0 {
			t.Fatalf("scale-down: %v, %d snapshot reads", herr, calls)
		}
		return out
	}
	out := down(freeMem(map[string]int64{"pve1": 40 * placeGiB, "pve2": 4 * placeGiB}))
	eq("scale-down by free memory, group a", out[0], map[string]int{"pve1": 2, "pve2": 1})
	eq("scale-down by free memory, group b", out[1], map[string]int{"pve1": 2, "pve2": 1})
	if asked != 1 {
		t.Errorf("free memory read %d times for one request, want 1", asked)
	}
	eq("scale-down, Proxmox not read", down(freeMem(nil))[0], map[string]int{"pve1": 1, "pve2": 2})
	eq("scale-down, no reader", down(nil)[0], map[string]int{"pve1": 1, "pve2": 2})

	// Two groups of one request share the snapshot: the second sees what the first took.
	tight := placeSite(20, 20) // two 8 GiB workers per node
	out, herr = kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("first", 3, "pve1=9201,pve2=9202", nil), placeGroup("second", 2, "pve1=9201,pve2=9202", nil)}, snap(tight), nil, nil)
	if herr == nil {
		t.Errorf("five workers on room for four: %v", out)
	}
	out, herr = kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("first", 3, "pve1=9201,pve2=9202", nil), placeGroup("second", 1, "pve1=9201,pve2=9202", nil)}, snap(placeSite(20, 20)), nil, nil)
	if herr != nil || out[0]["pve1"]+out[1]["pve1"] != 2 || out[0]["pve2"]+out[1]["pve2"] != 2 {
		t.Errorf("four workers on room for four: %v %v", out, herr)
	}
	// The snapshot's failure is the request's.
	if _, herr := kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("workers", 1, three, nil)}, func() (*kamajiCapacity, *httpx.HTTPError) { return nil, errPVEUnavailable }, nil, nil); herr != errPVEUnavailable {
		t.Errorf("unavailable provider: %v", herr)
	}
}

// Back-to-back creates: the first cluster's workers are decided and not cloned yet, so Proxmox shows the node free.
func TestKamajiPendingWorkers(t *testing.T) {
	disk := 40
	group := func(name string, count, cores, mem int, image string, placement map[string]any) map[string]any {
		g := map[string]any{"name": name, "machineCount": float64(count), "machineCores": float64(cores), "machineMemoryMiB": float64(mem),
			"machineImageId": image, "machineRootVolume": map[string]any{"diskSize": float64(disk)}}
		if placement != nil {
			g["machinePlacement"] = placement
		}
		return g
	}
	values := func(region string, groups ...any) map[string]any {
		return map[string]any{"proxmox": map[string]any{"region": region, "storage": "local-zfs"}, "nodeGroups": groups}
	}
	clusters := []kamaji.ClusterValues{
		// Placed 3 on pve1, 1 on pve2; one worker of pve1 exists.
		{ID: "pc-c-a", Values: values("az1", group("workers", 4, 2, 4096, "pve1=9201,pve2=9202", map[string]any{"pve1": float64(3), "pve2": float64(1)}),
			group("big", 0, 8, 32768, "pve1=9201", nil))},
		// No stored placement: the even split, 1 and 1; both exist.
		{ID: "pc-c-b", Values: values("az1", group("workers", 2, 2, 4096, "pve1=9201,pve2=9202", nil))},
		// Another region's cluster is another Proxmox.
		{ID: "pc-c-c", Values: values("az2", group("workers", 5, 2, 4096, "pve1=9201", nil))},
	}
	vms := []pve.ClusterResource{
		{Type: "qemu", Node: "pve1", Tags: "proxcloud;pc-k8s-pc-c-a"},
		{Type: "qemu", Node: "pve1", Tags: "proxcloud;pc-k8s-pc-c-b"},
		{Type: "qemu", Node: "pve2", Tags: "proxcloud;pc-k8s-pc-c-b"},
		{Type: "qemu", Node: "pve2", Tags: "proxcloud"}, // a plain server
	}
	got, gotDisk := kamajiPendingWorkers(clusters, vms, "az1", "local-zfs")
	// Their root disks, per node, with the cluster's largest: Proxmox shows that space free until the clone runs.
	if len(gotDisk) != 2 || gotDisk["pve1"] != 80*placeGiB || gotDisk["pve2"] != 40*placeGiB {
		t.Errorf("pending disk = %v, want 80 GiB on pve1 and 40 GiB on pve2", gotDisk)
	}
	if _, other := kamajiPendingWorkers(clusters, vms, "az1", "ceph"); len(other) != 0 {
		t.Errorf("pending disk on a storage no cluster uses: %v", other)
	}
	per := map[string]int{}
	for _, r := range got {
		per[r.Node]++
		// Sized as the cluster's largest group: which group a missing worker belongs to is not known from the VMs.
		if r.Type != "qemu" || r.MaxCPU != 8 || r.MaxMem != 32768<<20 {
			t.Errorf("pending worker %+v, want 8 vCPU and 32 GiB", r)
		}
	}
	if len(got) != 3 || per["pve1"] != 2 || per["pve2"] != 1 {
		t.Errorf("pending = %v, want 2 on pve1 and 1 on pve2", per)
	}
	// They count: a node Proxmox shows empty is full for the next cluster.
	site := placeSite(64, 64)
	site.vms = append(append(site.vms, vms...), got...)
	out, herr := kamajiPlaceGroups(nil, []kamaji.GroupPlacement{{Name: "workers", Flavor: "small", Count: 1, Image: "pve1=9201", Cores: 2, MemoryMiB: 8192, DiskGiB: 40}},
		func() (*kamajiCapacity, *httpx.HTTPError) { return site, nil }, nil, nil)
	if herr == nil || !strings.Contains(herr.Msg, "only 0 of 1 new workers fit") {
		t.Errorf("placed %v on a node whose memory two pending 32 GiB workers hold: %v", out, herr)
	}

	// Disk, on a site without shared storage: cluster A placed 3 workers of 300 GiB on pve1 and none is cloned. The
	// pool of pve1 is 1000 GiB with 900 free and 10 % headroom, so nothing more of that size fits there.
	disk = 300
	big := []kamaji.ClusterValues{{ID: "pc-c-a", Values: values("az1", group("workers", 3, 2, 4096, "pve1=9201", map[string]any{"pve1": float64(3)}))}}
	snapshot := func(shared bool) func() (*kamajiCapacity, *httpx.HTTPError) {
		return func() (*kamajiCapacity, *httpx.HTTPError) {
			site := placeSite(512, 512)
			pending, d := kamajiPendingWorkers(big, site.vms, "az1", "local-zfs")
			site.shared = shared
			kamajiDebitPendingDisk(site.storage, d, shared)
			site.vms = append(site.vms, pending...)
			return site, nil
		}
	}
	next := func(image string) []kamaji.GroupPlacement {
		return []kamaji.GroupPlacement{{Name: "workers", Flavor: "wide", Count: 2, Image: image, Cores: 2, MemoryMiB: 4096, DiskGiB: 300}}
	}
	if out, herr := kamajiPlaceGroups(nil, next("pve1=9201"), snapshot(false), nil, nil); herr == nil || !strings.Contains(herr.Msg, "only 0 of 2 new workers fit") {
		t.Errorf("1500 GiB decided on a 1000 GiB pool: %v %v", out, herr)
	}
	// The other node's pool is untouched.
	if out, herr := kamajiPlaceGroups(nil, next("pve1=9201,pve2=9202"), snapshot(false), nil, nil); herr != nil || out[0]["pve1"] != 0 || out[0]["pve2"] != 2 {
		t.Errorf("node-local pending disk: %v %v, want both on pve2", out, herr)
	}
	// A shared storage is one pool: the pending disks are gone from every node's view of it.
	if out, herr := kamajiPlaceGroups(nil, next("pve1=9201,pve2=9202"), snapshot(true), nil, nil); herr == nil {
		t.Errorf("shared pool with 900 GiB pending: %v", out)
	}
}

// A request names neither the image nor the placement of a node group.
func TestKamajiNodeGroupsDropsImageAndPlacement(t *testing.T) {
	groups, err := kamajiNodeGroups([]any{map[string]any{
		"name": "workers", "flavorId": "small", "count": float64(2), "imageVariant": "nvidia",
		"imageId": "pve9=100", "placement": map[string]any{"pve9": float64(2)}, "machinePlacement": map[string]any{"pve9": float64(2)},
	}})
	if err != nil || len(groups) != 1 {
		t.Fatalf("%v %v", groups, err)
	}
	if g := groups[0]; g.ImageID != "" || g.Placement != nil || g.ImageVariant != "nvidia" || g.Count != 2 {
		t.Errorf("group = %+v: imageId and placement must be dropped, the rest kept", g)
	}
	d := kamaji.ClusterDefaults{Versions: map[string]string{"1.35.10": "pve1=9201,pve2=9202"}, RootVolumeGiB: 30,
		ImageVariants: map[string]map[string]string{"nvidia": {"1.35.10": "pve1=9301"}}}
	rendered := kamaji.NodeGroupValues(d, "1.35.10", groups)[0].(map[string]any)
	if rendered["machineImageId"] != "pve1=9301" || rendered["machinePlacement"] != nil {
		t.Errorf("rendered %v: the image is the provider's, and no placement comes from a request", rendered)
	}
}

// The values patches of SET_NODE_GROUPS and UPGRADE, on a stubbed site. The gate is the chart the cluster itself is
// pinned to: on 0.1.12 nothing is written and nothing is read; from 0.1.13 the stored placement is carried and only
// the difference is placed.
func TestKamajiValuePatches(t *testing.T) {
	const v135, v136 = "1.35.10", "1.36.1"
	d := kamaji.ClusterDefaults{RootVolumeGiB: 40, Versions: map[string]string{
		v135: "pve1=9201,pve2=9202,pve3=9203", v136: "pve1=9301,pve2=9302",
	}}
	live := func(placement map[string]any) map[string]any {
		g := map[string]any{"name": "workers", "machineFlavor": "small", "machineCount": float64(3), "machineCores": float64(2),
			"machineMemoryMiB": float64(8192), "machineImageId": d.Versions[v135], "machineRootVolume": map[string]any{"diskSize": float64(40)}}
		if placement != nil {
			g["machinePlacement"] = placement
		}
		return map[string]any{"kubernetesVersion": v135, "proxmox": map[string]any{"region": "az1", "storage": "local-zfs"}, "nodeGroups": []any{g}}
	}
	want := func(count int) []kamaji.NodeGroup {
		return []kamaji.NodeGroup{{Name: "workers", FlavorID: "small", Count: count, Cores: 2, MemoryMiB: 8192}}
	}
	var reads []string
	placer := func() kamajiPlacer {
		reads = nil
		return kamajiPlacer{
			unready: func() map[string]map[string]int { reads = append(reads, "unready"); return nil },
			snapshot: func(storage string) (*kamajiCapacity, *httpx.HTTPError) {
				reads = append(reads, "snapshot "+storage)
				return placeSite(64, 64, 64), nil
			},
			freeMem: func() map[string]int64 { reads = append(reads, "freeMem"); return nil },
		}
	}
	group := func(values map[string]any) kamaji.GroupPlacement { return kamaji.GroupPlacements(values)[0] }
	stored := map[string]any{"pve1": float64(3)}

	// A cluster on the older chart: a scale-up writes the count and no placement, and asks nobody.
	values := live(nil)
	if err := kamajiSetGroupsValues(values, "0.1.12", want(5), d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); g.Count != 5 || g.Placement != nil || len(reads) != 0 {
		t.Errorf("0.1.12 scale-up: %+v, reads %v; want no machinePlacement and no read", g, reads)
	}
	values = live(nil)
	if err := kamajiUpgradeValues(values, "0.1.12", v136, d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); values["kubernetesVersion"] != v136 || g.Image != d.Versions[v136] || g.Placement != nil || len(reads) != 0 {
		t.Errorf("0.1.12 upgrade: %+v, reads %v", g, reads)
	}
	// Not even an unresolved compute provider matters before the gate; past it, it refuses.
	blind := kamajiPlacer{computeErr: errPVEUnavailable}
	if err := kamajiUpgradeValues(live(nil), "0.1.12", v136, d, blind); err != nil {
		t.Errorf("0.1.12 upgrade without a compute provider: %v", err)
	}
	if err := kamajiUpgradeValues(live(stored), "0.1.13", v136, d, blind); err != errPVEUnavailable {
		t.Errorf("0.1.13 upgrade without a compute provider: %v", err)
	}

	// From 0.1.13: the three workers on pve1 stay, the two new ones are placed on the storage the cluster uses.
	values = live(stored)
	if err := kamajiSetGroupsValues(values, "0.1.13", want(5), d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); g.Count != 5 || g.Placement["pve1"] != 3 || g.Placement["pve2"] != 1 || g.Placement["pve3"] != 1 {
		t.Errorf("0.1.13 scale-up: %+v, want pve1 3, pve2 1, pve3 1", g)
	}
	if !slices.Contains(reads, "snapshot local-zfs") {
		t.Errorf("0.1.13 scale-up reads %v, want a snapshot of local-zfs", reads)
	}
	// An edit that changes no count keeps the stored map and reads no capacity.
	values = live(stored)
	if err := kamajiSetGroupsValues(values, "0.1.13", want(3), d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); len(g.Placement) != 1 || g.Placement["pve1"] != 3 || slices.Contains(reads, "snapshot local-zfs") {
		t.Errorf("0.1.13 same count: %+v, reads %v", g, reads)
	}
	// The first edit of a cluster that has no stored placement writes the even split it runs, less the one removed.
	values = live(nil)
	if err := kamajiSetGroupsValues(values, "0.1.13", want(2), d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); g.Placement["pve1"]+g.Placement["pve2"]+g.Placement["pve3"] != 2 || !slices.Contains(reads, "freeMem") || slices.Contains(reads, "snapshot local-zfs") {
		t.Errorf("0.1.13 scale-down: %+v, reads %v", g, reads)
	}

	// UPGRADE carries the placement onto the new image: 1.36 has no image on pve3, so only its worker is placed again.
	values = live(map[string]any{"pve1": float64(1), "pve2": float64(1), "pve3": float64(1)})
	if err := kamajiUpgradeValues(values, "0.1.13", v136, d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); g.Image != d.Versions[v136] || g.Count != 3 || g.Placement["pve3"] != 0 || g.Placement["pve1"]+g.Placement["pve2"] != 3 || g.Placement["pve1"] < 1 || g.Placement["pve2"] < 1 {
		t.Errorf("0.1.13 upgrade: %+v, want pve3's worker moved to pve1 or pve2 and the others kept", g)
	}
	// With every image node kept, an upgrade places nothing and reads no capacity.
	d.Versions[v136] = "pve1=9301,pve2=9302,pve3=9303"
	values = live(stored)
	if err := kamajiUpgradeValues(values, "0.1.13", v136, d, placer()); err != nil {
		t.Fatal(err)
	}
	if g := group(values); len(g.Placement) != 1 || g.Placement["pve1"] != 3 || slices.Contains(reads, "snapshot local-zfs") {
		t.Errorf("0.1.13 upgrade, same nodes: %+v, reads %v", g, reads)
	}
	// A refused request leaves the live values as they were.
	values = live(stored)
	tight := placer()
	tight.snapshot = func(string) (*kamajiCapacity, *httpx.HTTPError) { return placeSite(4, 4, 4), nil }
	if err := kamajiSetGroupsValues(values, "0.1.13", want(5), d, tight); err == nil || group(values).Count != 3 {
		t.Errorf("no room: err %v, values %+v", err, group(values))
	}
}

// Create: the decision lands in the spec, per group, and a cluster that does not fit leaves the spec as it was.
func TestKamajiPlaceCreate(t *testing.T) {
	d := kamaji.ClusterDefaults{RootVolumeGiB: 40, Versions: map[string]string{"1.35.10": "pve1=9201,pve2=9202"}}
	spec := func(counts ...int) *kamaji.ClusterSpec {
		s := &kamaji.ClusterSpec{Version: "1.35.10"}
		for i, n := range counts {
			s.NodeGroups = append(s.NodeGroups, kamaji.NodeGroup{Name: []string{"workers", "batch"}[i], FlavorID: "small", Count: n, Cores: 2, MemoryMiB: 8192})
		}
		return s
	}
	site := func(mem ...int64) func() (*kamajiCapacity, *httpx.HTTPError) {
		return func() (*kamajiCapacity, *httpx.HTTPError) { return placeSite(mem...), nil }
	}
	s := spec(3, 1)
	if err := kamajiPlaceCreate(d, s, site(64, 20)); err != nil {
		t.Fatal(err)
	}
	// pve2 has room for two 8 GiB workers: one of each group; the rest of "workers" goes to pve1.
	if w, b := s.NodeGroups[0].Placement, s.NodeGroups[1].Placement; w["pve1"] != 2 || w["pve2"] != 1 || b["pve1"]+b["pve2"] != 1 {
		t.Errorf("placement in the spec: workers %v, batch %v", w, b)
	}
	// What BuildValues renders from the spec is what was decided.
	rendered := kamaji.NodeGroupValues(d, s.Version, s.NodeGroups)
	if g := kamaji.GroupPlacements(map[string]any{"nodeGroups": rendered})[0]; g.Placement["pve1"] != 2 || g.Placement["pve2"] != 1 {
		t.Errorf("rendered placement %v", g.Placement)
	}
	s = spec(9)
	if err := kamajiPlaceCreate(d, s, site(20, 20)); err == nil || s.NodeGroups[0].Placement != nil {
		t.Errorf("nine workers on room for four: err %v, placement %v", err, s.NodeGroups[0].Placement)
	}
}
