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
