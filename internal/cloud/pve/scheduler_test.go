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
