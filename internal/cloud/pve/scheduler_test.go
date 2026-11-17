package pve

import "testing"

func TestPickNode(t *testing.T) {
	nodes := []Node{
		{Node: "pve1", Status: "online", MaxCPU: 8, MaxMem: 32 << 30},
		{Node: "pve2", Status: "online", MaxCPU: 8, MaxMem: 64 << 30},
		{Node: "pve3", Status: "offline", MaxCPU: 64, MaxMem: 256 << 30},
	}
	vms := []ClusterResource{
		{Type: "qemu", Node: "pve1", MaxCPU: 4, MaxMem: 8 << 30},
		{Type: "qemu", Node: "pve2", MaxCPU: 30, MaxMem: 16 << 30},
		{Type: "qemu", Node: "pve2", Template: 1, MaxCPU: 2, MaxMem: 2 << 30}, // templates don't count
	}
	// pve2 has more free RAM (48G vs 24G) → wins
	got, err := PickNode(nodes, vms, SchedReq{VCPUs: 2, RAMMb: 4096, OvercommitCPU: 4})
	if err != nil || got != "pve2" {
		t.Fatalf("got %q err %v", got, err)
	}
	// CPU ceiling: pve2 already commits 30 of 8*4=32 → a 4-vCPU req only fits pve1
	got, err = PickNode(nodes, vms, SchedReq{VCPUs: 4, RAMMb: 1024, OvercommitCPU: 4})
	if err != nil || got != "pve1" {
		t.Fatalf("cpu ceiling: got %q err %v", got, err)
	}
	// RAM too big for everyone → error (offline pve3 never counts)
	if _, err = PickNode(nodes, vms, SchedReq{VCPUs: 1, RAMMb: 100 << 10, OvercommitCPU: 4}); err == nil {
		t.Fatal("want no-capacity error")
	}
	// AllowedNodes pins placement
	got, err = PickNode(nodes, vms, SchedReq{VCPUs: 1, RAMMb: 1024, OvercommitCPU: 4, AllowedNodes: map[string]bool{"pve1": true}})
	if err != nil || got != "pve1" {
		t.Fatalf("allowed: got %q err %v", got, err)
	}
	// GPU filter: only nodes with free slots
	got, err = PickNode(nodes, vms, SchedReq{VCPUs: 1, RAMMb: 1024, GPUs: 1, GPUFree: map[string]int{"pve1": 1}})
	if err != nil || got != "pve1" {
		t.Fatalf("gpu: got %q err %v", got, err)
	}
}

func TestPickNodeStorage(t *testing.T) {
	const g = int64(1) << 30
	nodes := []Node{
		{Node: "pve1", Status: "online", MaxCPU: 16, MaxMem: 64 << 30}, // most free RAM
		{Node: "pve2", Status: "online", MaxCPU: 16, MaxMem: 32 << 30},
		{Node: "pve3", Status: "online", MaxCPU: 16, MaxMem: 16 << 30},
	}
	roomy := StorageNodeState{Active: true, Total: 1000 * g, Avail: 800 * g}
	full := StorageNodeState{Active: true, Total: 1000 * g, Avail: 105 * g} // 50G leaves < 10% free
	tests := []struct {
		name     string
		storage  map[string]StorageNodeState
		allowed  map[string]bool
		headroom float64
		want     string // "" = no capacity
	}{
		{"no profile: no storage filter", nil, nil, 10, "pve1"},
		{"full pool on the RAM-best node is skipped", map[string]StorageNodeState{"pve1": full, "pve2": roomy, "pve3": roomy}, nil, 10, "pve2"},
		{"full pool fits without headroom", map[string]StorageNodeState{"pve1": full, "pve2": roomy}, nil, 0, "pve1"},
		{"node without the profile storage is skipped", map[string]StorageNodeState{"pve3": roomy}, nil, 10, "pve3"},
		{"inactive storage is skipped", map[string]StorageNodeState{"pve1": {Total: 1000 * g, Avail: 1000 * g}, "pve2": roomy}, nil, 10, "pve2"},
		{"local template pins to its node: shared pool roomy everywhere", map[string]StorageNodeState{"pve1": roomy, "pve2": roomy, "pve3": roomy}, map[string]bool{"pve3": true}, 10, "pve3"},
		{"local template node full: no capacity", map[string]StorageNodeState{"pve1": roomy, "pve3": full}, map[string]bool{"pve3": true}, 10, ""},
		{"every pool full", map[string]StorageNodeState{"pve1": full, "pve2": full, "pve3": full}, nil, 10, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PickNode(nodes, nil, SchedReq{
				VCPUs: 2, RAMMb: 2048, AllowedNodes: tt.allowed,
				Storage: tt.storage, DiskBytes: 50 * g, HeadroomPct: tt.headroom,
			})
			if tt.want == "" {
				if err == nil {
					t.Fatalf("got %q, want no-capacity error", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q err %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestPickNodeSpreadGroup(t *testing.T) {
	const g = int64(1) << 30
	nodes := []Node{
		{Node: "a", Status: "online", MaxCPU: 8, MaxMem: 64 * g},
		{Node: "b", Status: "online", MaxCPU: 8, MaxMem: 32 * g},
	}
	member := func(node, pool, tags string) ClusterResource {
		return ClusterResource{Type: "qemu", Node: node, Pool: pool, Tags: tags}
	}
	req := SchedReq{VCPUs: 1, RAMMb: 1024, SpreadPool: "p", SpreadGroup: "web"}

	got, err := PickNode(nodes, []ClusterResource{member("a", "p", "grp-web")}, req)
	if err != nil || got != "b" {
		t.Fatalf("a member on a must send the next to b although a has more RAM: %q %v", got, err)
	}
	got, err = PickNode(nodes, []ClusterResource{member("a", "other", "grp-web"), member("a", "p", "grp-db")}, req)
	if err != nil || got != "a" {
		t.Fatalf("another project's or another group's VMs do not count: %q %v", got, err)
	}
	both := []ClusterResource{member("a", "p", "grp-web"), member("b", "p", "grp-web")}
	if _, err = PickNode(nodes, both, req); err != nil {
		t.Fatalf("soft spread still places: %v", err)
	}
	req.SpreadStrict = true
	if _, err = PickNode(nodes, both, req); err == nil {
		t.Fatal("strict spread must refuse when every node has a member")
	}
}

func TestPickNodeIsStable(t *testing.T) {
	nodes := []Node{{Node: "pve2", Status: "online", MaxCPU: 8, MaxMem: 32 << 30}, {Node: "pve1", Status: "online", MaxCPU: 8, MaxMem: 32 << 30}}
	for i := 0; i < 20; i++ {
		if got, _ := PickNode(nodes, nil, SchedReq{VCPUs: 1, RAMMb: 1024}); got != "pve1" {
			t.Fatalf("equal nodes: got %q, want pve1 every time", got)
		}
	}
	// Members counts like tagged spread members.
	if got, _ := PickNode(nodes, nil, SchedReq{VCPUs: 1, RAMMb: 1024, Members: map[string]int{"pve1": 1}}); got != "pve2" {
		t.Errorf("members: got %q, want the node with fewer", got)
	}
}

func TestPlaceReplicas(t *testing.T) {
	const gib = int64(1) << 30
	node := func(name string, memGiB int64) Node {
		return Node{Node: name, Status: "online", MaxCPU: 16, MaxMem: memGiB * gib}
	}
	store := func(avail ...int64) map[string]StorageNodeState {
		out := map[string]StorageNodeState{}
		for i, a := range avail {
			out[[]string{"pve1", "pve2", "pve3"}[i]] = StorageNodeState{Active: true, Total: 1000 * gib, Avail: a * gib}
		}
		return out
	}
	three := []Node{node("pve1", 64), node("pve2", 64), node("pve3", 64)}
	req := func(st map[string]StorageNodeState) SchedReq {
		return SchedReq{VCPUs: 4, RAMMb: 8192, OvercommitCPU: 4, Storage: st, DiskBytes: 50 * gib, HeadroomPct: 10,
			AllowedNodes: map[string]bool{"pve1": true, "pve2": true, "pve3": true}}
	}
	sum := func(m map[string]int) (n int) {
		for _, v := range m {
			n += v
		}
		return n
	}

	// Several nodes fit: the group is spread, not stacked on the emptiest.
	got, vms, fit := PlaceReplicas(three, nil, req(store(900, 900, 900)), nil, 3, false)
	if fit != 3 || got["pve1"] != 1 || got["pve2"] != 1 || got["pve3"] != 1 || len(vms) != 3 {
		t.Errorf("spread: %v fit %d", got, fit)
	}
	// A node full of other VMs gets none: the even split would have given it a third.
	full := []ClusterResource{{Type: "qemu", Node: "pve2", MaxCPU: 4, MaxMem: 60 * gib}}
	got, _, fit = PlaceReplicas(three, full, req(store(900, 900, 900)), nil, 4, false)
	if fit != 4 || got["pve2"] != 0 || got["pve1"] != 2 || got["pve3"] != 2 {
		t.Errorf("full node: %v fit %d", got, fit)
	}
	// Memory fits everywhere, the node-local store of pve1 is full (inside its headroom).
	got, _, fit = PlaceReplicas(three, nil, req(store(120, 900, 900)), nil, 2, false)
	if fit != 2 || got["pve1"] != 0 {
		t.Errorf("full store: %v fit %d", got, fit)
	}
	// Local stores: each pick takes its disk from its own node only. 200 GiB free, 100 kept as headroom: two disks each.
	st := store(200, 200, 200)
	got, _, fit = PlaceReplicas(three, nil, req(st), nil, 9, false)
	if fit != 6 || got["pve1"] != 2 || got["pve2"] != 2 || got["pve3"] != 2 || st["pve1"].Avail != 100*gib {
		t.Errorf("local stores: %v fit %d, pve1 has %d GiB left", got, fit, st["pve1"].Avail/gib)
	}
	// A shared store is one pool: the same 200 GiB hold two disks in all, not two per node.
	got, _, fit = PlaceReplicas(three, nil, req(store(200, 200, 200)), nil, 9, true)
	if fit != 2 || sum(got) != 2 {
		t.Errorf("shared store: %v fit %d, want 2 in all", got, fit)
	}
	// Scale-up keeps what runs and places only the new ones, on the nodes with fewer.
	got, _, fit = PlaceReplicas(three, nil, req(store(900, 900, 900)), map[string]int{"pve1": 3}, 2, false)
	if fit != 2 || got["pve1"] != 3 || got["pve2"] != 1 || got["pve3"] != 1 {
		t.Errorf("scale-up: %v fit %d", got, fit)
	}
	// Nothing fits: a refusal, and the placement is what it was.
	got, _, fit = PlaceReplicas([]Node{node("pve1", 4)}, nil, req(store(900)), map[string]int{"pve1": 1}, 2, false)
	if fit != 0 || got["pve1"] != 1 {
		t.Errorf("no room: %v fit %d", got, fit)
	}
	// A node without the storage (no entry) is never picked.
	got, _, fit = PlaceReplicas(three, nil, req(map[string]StorageNodeState{"pve3": {Active: true, Total: 1000 * gib, Avail: 900 * gib}}), nil, 2, false)
	if fit != 2 || got["pve3"] != 2 {
		t.Errorf("storage on one node: %v fit %d", got, fit)
	}
}

func TestRemoveReplicas(t *testing.T) {
	const gib = int64(1) << 30
	for _, tc := range []struct {
		name    string
		have    map[string]int
		n       int
		unready map[string]int
		freeMem map[string]int64
		want    map[string]int
	}{
		{"the stuck machine goes first, not a healthy one", map[string]int{"pve1": 2, "pve2": 1}, 1, map[string]int{"pve2": 1}, nil, map[string]int{"pve1": 2, "pve2": 0}},
		{"one stuck machine is one removal", map[string]int{"pve1": 2, "pve2": 2}, 2, map[string]int{"pve2": 1}, nil, map[string]int{"pve1": 1, "pve2": 1}},
		{"the node with the most replicas", map[string]int{"pve1": 1, "pve2": 3}, 1, nil, nil, map[string]int{"pve1": 1, "pve2": 2}},
		{"a node is emptied last", map[string]int{"pve1": 1, "pve2": 2, "pve3": 1}, 1, nil, map[string]int64{"pve1": 1 * gib}, map[string]int{"pve1": 1, "pve2": 1, "pve3": 1}},
		{"equal counts: the node with the least free memory", map[string]int{"pve1": 2, "pve2": 2}, 1, nil, map[string]int64{"pve1": 40 * gib, "pve2": 4 * gib}, map[string]int{"pve1": 2, "pve2": 1}},
		{"equal all round: by name", map[string]int{"pve2": 1, "pve1": 1}, 1, nil, nil, map[string]int{"pve1": 0, "pve2": 1}},
		{"more than there is", map[string]int{"pve1": 1}, 3, nil, nil, map[string]int{"pve1": 0}},
	} {
		got := RemoveReplicas(tc.have, tc.n, tc.unready, tc.freeMem)
		for node, want := range tc.want {
			if got[node] != want {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
	have := map[string]int{"pve1": 2}
	if RemoveReplicas(have, 1, nil, nil); have["pve1"] != 2 {
		t.Error("RemoveReplicas changed its input")
	}
}
