package project

import (
	"net/http"
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
		out, herr := kamajiPlaceGroups(p, []kamaji.GroupPlacement{next}, snap(c), map[string]map[string]int{next.Name: unready})
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
	site.vms = []pve.ClusterResource{{Type: "qemu", Node: "pve2", MaxCPU: 8, MaxMem: 60 * placeGiB}}
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

	// Two groups of one request share the snapshot: the second sees what the first took.
	tight := placeSite(20, 20) // two 8 GiB workers per node
	out, herr := kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("first", 3, "pve1=9201,pve2=9202", nil), placeGroup("second", 2, "pve1=9201,pve2=9202", nil)}, snap(tight), nil)
	if herr == nil {
		t.Errorf("five workers on room for four: %v", out)
	}
	out, herr = kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("first", 3, "pve1=9201,pve2=9202", nil), placeGroup("second", 1, "pve1=9201,pve2=9202", nil)}, snap(placeSite(20, 20)), nil)
	if herr != nil || out[0]["pve1"]+out[1]["pve1"] != 2 || out[0]["pve2"]+out[1]["pve2"] != 2 {
		t.Errorf("four workers on room for four: %v %v", out, herr)
	}
	// The snapshot's failure is the request's.
	if _, herr := kamajiPlaceGroups(nil, []kamaji.GroupPlacement{placeGroup("workers", 1, three, nil)}, func() (*kamajiCapacity, *httpx.HTTPError) { return nil, errPVEUnavailable }, nil); herr != errPVEUnavailable {
		t.Errorf("unavailable provider: %v", herr)
	}
}

// Back-to-back creates: the first cluster's workers are decided and not cloned yet, so Proxmox shows the node free.
func TestKamajiPendingWorkers(t *testing.T) {
	group := func(name string, count, cores, mem int, image string, placement map[string]any) map[string]any {
		g := map[string]any{"name": name, "machineCount": float64(count), "machineCores": float64(cores), "machineMemoryMiB": float64(mem),
			"machineImageId": image, "machineRootVolume": map[string]any{"diskSize": float64(40)}}
		if placement != nil {
			g["machinePlacement"] = placement
		}
		return g
	}
	values := func(region string, groups ...any) map[string]any {
		return map[string]any{"proxmox": map[string]any{"region": region}, "nodeGroups": groups}
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
	got := kamajiPendingWorkers(clusters, vms, "az1")
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
	site.vms = append(vms, got...)
	out, herr := kamajiPlaceGroups(nil, []kamaji.GroupPlacement{{Name: "workers", Flavor: "small", Count: 1, Image: "pve1=9201", Cores: 2, MemoryMiB: 8192, DiskGiB: 40}},
		func() (*kamajiCapacity, *httpx.HTTPError) { return site, nil }, nil)
	if herr == nil {
		t.Errorf("placed %v on a node whose memory two pending 32 GiB workers hold", out)
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
