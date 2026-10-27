package pve

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestGPUTagRoundTrip(t *testing.T) {
	if a, n := gpuFromTags(JoinTags(TagManaged, GPUTag("nvidia-p40", 2))); a != "nvidia-p40" || n != 2 {
		t.Fatalf("got %q %d", a, n)
	}
	if a, _ := gpuFromTags("proxcloud;fl-x"); a != "" {
		t.Fatal("no gpu tag must parse empty")
	}
	specs := GPUSpecsFromTags(JoinTags(GPUTag("nvidia-p40", 3)))
	if specs["pci_passthrough:alias"] != "nvidia-p40:3" {
		t.Fatalf("extra_specs = %v", specs)
	}
}

// One mapping PER CARD (the homelab shape) sharing one alias: availability must
// count each mapping once, and a card held by ANY vm (including one created
// outside proxcloud) must not be offered.
func TestGPUInventoryPerCardMappings(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/mapping/pci": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"id":"gpu1","map":["id=10de:1b38,iommugroup=11,node=pve,path=0000:18:00.0"]},
				{"id":"gpu2","map":["id=10de:1b38,iommugroup=9,node=pve,path=0000:3b:00.0"]},
				{"id":"gpu3","map":["id=10de:1b38,iommugroup=21,node=pve,path=0000:86:00.0"]},
				{"id":"other","map":["id=8086:0001,node=pve,path=0000:01:00.0"]}
			]}`)
		},
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[
				{"id":"qemu/200","type":"qemu","vmid":200,"node":"pve","status":"running"},
				{"id":"qemu/9999","type":"qemu","vmid":9999,"node":"pve","status":"stopped","template":1}
			]}`)
		},
		// an OPS vm (no proxcloud tag) already holds gpu2
		"GET /api2/json/nodes/pve/qemu/200/config": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":{"hostpci0":"mapping=gpu2,pcie=1","cores":8}}`)
		},
	})
	s := &Service{C: c, ServiceID: "svc", PoolPrefix: "st-"}
	inv, err := s.GPUInventoryFor(context.Background(), "nvidia-p40", []string{"gpu1", "gpu2", "gpu3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.All) != 3 {
		t.Fatalf("all = %d, want 3 (one per mapping)", len(inv.All))
	}
	if len(inv.Free) != 2 {
		t.Fatalf("free = %d, want 2 (gpu2 held by an ops VM)", len(inv.Free))
	}
	if n := inv.FreeCountByNode()["pve"]; n != 2 {
		t.Fatalf("free on pve = %d", n)
	}
	// Two GPUs must come from DISTINCT mappings.
	picked, err := inv.PickGPUSlots("pve", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) != 2 || picked[0] == picked[1] {
		t.Fatalf("picked = %v (must be distinct mappings)", picked)
	}
	for _, id := range picked {
		if id == "gpu2" {
			t.Fatal("picked a mapping already in use")
		}
	}
	// Asking for more than free must fail rather than over-promise.
	if _, err := inv.PickGPUSlots("pve", 3); err == nil {
		t.Fatal("want capacity error")
	}
	// Placement on a node with no mapped device must fail too.
	if _, err := inv.PickGPUSlots("pve2", 1); err == nil {
		t.Fatal("want error for a node with no devices")
	}
}

func TestGPUCreateParams(t *testing.T) {
	p := GPUCreateParams([]string{"gpu1", "gpu3"})
	if p["machine"] != "q35" {
		t.Fatalf("machine = %v (passthrough needs q35)", p["machine"])
	}
	if p["hostpci0"] != "mapping=gpu1,pcie=1" || p["hostpci1"] != "mapping=gpu3,pcie=1" {
		t.Fatalf("params = %v", p)
	}
}
