package pve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestSupersededByIdentity(t *testing.T) {
	row := func(typ, identity string) *cloud.CloudResource {
		data := map[string]any{}
		if identity != "" {
			data[dataIdentity] = identity
		}
		return &cloud.CloudResource{Type: typ, Data: data}
	}
	s, img := cloud.TypeServer, cloud.TypeImage
	cases := []struct {
		name     string
		old, cur *cloud.CloudResource
		want     bool
	}{
		{"same stamp", row(s, "c-1"), row(s, "c-1"), false},
		{"other stamp: the vmid was reused", row(s, "c-1"), row(s, "c-2"), true},
		{"other ctime: the vmid was reused", row(s, "m-1"), row(s, "m-2"), true},
		{"stamp gone: a new VM without one", row(s, "c-1"), row(s, "m-1"), true},
		{"stamp arrives: the portal tagging its clone", row(s, "m-1"), row(s, "c-2"), false},
		{"row from before identities", row(s, ""), row(s, "c-1"), false},
		{"unknown now (mid-clone)", row(s, "c-1"), row(s, ""), false},
		{"a template's vmid reused by a VM", row(img, ""), row(s, "c-1"), true},
	}
	for _, c := range cases {
		if got := supersededByIdentity(c.old, c.cur, serverIdentity); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTaggedIdentity(t *testing.T) {
	tags := "fl-x;" + CreatedTag(1700000000) + ";proxcloud"
	if got := taggedIdentity(tags, ""); got != "c-1700000000" {
		t.Errorf("got %q", got)
	}
	if got := taggedIdentity(tags, "clone"); got != "" {
		t.Errorf("a VM mid-clone still carries its source's tags, got %q", got)
	}
	if got := metaIdentity(VMConfig{"meta": "creation-qemu=11.1.3,ctime=1793988485"}); got != "m-1793988485" {
		t.Errorf("got %q", got)
	}
	if got := metaIdentity(VMConfig{}); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestGPUSpecsFromConfig(t *testing.T) {
	aliasOf := func(id string) string {
		return map[string]string{"gpu1": "nvidia-p40", "gpu2": "nvidia-p40", "a60": "intel-a60"}[id]
	}
	cfg := VMConfig{
		"hostpci0": "mapping=gpu1,pcie=1",
		"hostpci1": "mapping=gpu2,pcie=1",
		"hostpci2": "mapping=a60,pcie=1",
		"hostpci3": "mapping=some-nic,pcie=1", // not a GPU
		"hostpci4": "0000:01:00.0",            // raw device, no mapping
		"cores":    float64(4),
	}
	got := GPUSpecsFromConfig(cfg, aliasOf)
	if got["pci_passthrough:alias"] != "intel-a60:1,nvidia-p40:2" {
		t.Fatalf("got %v", got)
	}
	model, count := cloud.GPUFromFlavor(got)
	if model == "" || count != 3 {
		t.Fatalf("billing parse: %q x%d", model, count)
	}
	if GPUSpecsFromConfig(VMConfig{"cores": float64(1)}, aliasOf) != nil {
		t.Fatal("no hostpci means no GPU specs")
	}
	if GPUSpecsFromConfig(cfg, nil) != nil {
		t.Fatal("no alias table means no GPU specs")
	}
}

// The sampler pays for one /cluster/resources page: a stamped VM costs no call at all, and what only
// a full pass knows (agent IPs, the hostpci GPU count) survives from the cached row.
func TestSampledServerProvider(t *testing.T) {
	calls := 0
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/n1/qemu/102/config": func(w http.ResponseWriter, r *http.Request) {
			calls++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"meta": "ctime=55"}})
		},
	})
	s := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	page := []ClusterResource{
		{Type: "qemu", VMID: 101, Name: "a", Node: "n1", Status: "running", Pool: "pc-p1", MaxCPU: 2, MaxMem: 2 << 30, Tags: "fl-x;" + CreatedTag(7)},
		{Type: "qemu", VMID: 102, Name: "out-of-portal", Node: "n1", Status: "stopped", Pool: "pc-p1", MaxCPU: 1, MaxMem: 1 << 30},
		{Type: "qemu", VMID: 103, Name: "other project", Node: "n1", Status: "running", Pool: "pc-p2"},
		{Type: "qemu", VMID: 104, Name: "tpl", Node: "n1", Status: "stopped", Pool: "pc-p1", Template: 1, MaxDisk: 3 << 30},
	}
	page = append(page, ClusterResource{Type: "qemu", VMID: 105, Name: "paused", Node: "n1", Status: "running", Pool: "pc-p1", Tags: CreatedTag(8)})
	addrs := map[string]any{"private": []any{map[string]any{"addr": "10.0.0.5", "version": float64(4)}}}
	specs := map[string]any{"pci_passthrough:alias": "nvidia-p40:2"}
	cached := map[string]*cloud.CloudResource{
		"101": {Data: map[string]any{"server": map[string]any{
			"addresses": addrs, "flavor": map[string]any{"extra_specs": specs},
		}}},
		"105": {Data: map[string]any{"server": map[string]any{"status": "PAUSED"}}},
	}
	list, err := s.SampledServerProvider("r", "p1", page, cached).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("rows = %d, want the pool's 3 live VMs", len(list))
	}
	if got := list[2].Data["server"].(map[string]any)["status"]; got != "PAUSED" {
		t.Fatalf("paused state lost: %v", got)
	}
	if calls != 1 {
		t.Fatalf("per-VM calls = %d, want 1 (the VM with no stamp)", calls)
	}
	a, b := list[0], list[1]
	srv := a.Data["server"].(map[string]any)
	if a.Data[dataIdentity] != "c-7" || srv["status"] != "ACTIVE" || srv["flavor"].(map[string]any)["vcpus"] != 2 {
		t.Fatalf("stamped row: %+v", a.Data)
	}
	if got := srv["addresses"].(map[string]any); got["private"] == nil {
		t.Fatalf("agent IPs lost: %+v", srv)
	}
	if got := srv["flavor"].(map[string]any)["extra_specs"].(map[string]any); got["pci_passthrough:alias"] != "nvidia-p40:2" {
		t.Fatalf("GPU specs lost: %+v", got)
	}
	if b.Data[dataIdentity] != "m-55" {
		t.Fatalf("unstamped row identity: %+v", b.Data)
	}
	assertShapeStable(t, "sampled server", a.Data)

	images, err := s.SampledImageProvider("r", "p1", page).List(context.Background())
	if err != nil || len(images) != 1 || images[0].ExternalID != "104" {
		t.Fatalf("images: %v %+v", err, images)
	}
}

func TestStampSnapshots(t *testing.T) {
	snaps := `[{"name":"current"},{"name":"s2","snaptime":20},{"name":"__replicate_101_0__","snaptime":30},{"name":"s1","snaptime":10}]`
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/n1/qemu/101/snapshot": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":` + snaps + `}`))
		},
		"GET /api2/json/nodes/n1/qemu/101/config": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"scsi0":"z:vm-101-disk-0,size=8G","scsi1":"z:vm-101-disk-1,size=4G","ide2":"z:vm-101-cloudinit,media=cdrom"}}`))
		},
	})
	s := &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}
	vm := ClusterResource{VMID: 101, Node: "n1"}
	stamped := func() any {
		data := map[string]any{"server": map[string]any{}}
		s.StampSnapshots(context.Background(), vm, data)
		return data["server"].(map[string]any)["snapshots"]
	}
	got, _ := stamped().(map[string]any)
	items, _ := got["items"].([]any)
	if len(items) != 2 || got["diskGb"] != 12 {
		t.Fatalf("platform snapshots and the CD must not count: %+v", got)
	}
	if first := items[0].(map[string]any); first["name"] != "s1" || first["createdAt"] != "1970-01-01T00:00:10Z" {
		t.Fatalf("a stable order keeps the row from churning, and billing needs the time: %+v", items)
	}
	snaps = `[{"name":"current"},{"name":"__replicate_101_0__"}]`
	if v := stamped(); v != nil {
		t.Fatalf("no tenant snapshot, nothing to bill: %+v", v)
	}
	// The sampler keeps what the last full pass read.
	cached := map[string]*cloud.CloudResource{"101": {Data: map[string]any{"server": map[string]any{
		"snapshots": map[string]any{"items": []any{}, "diskGb": 8}, "protection": map[string]any{"state": "active"},
	}}}}
	page := []ClusterResource{{Type: "qemu", VMID: 101, Name: "a", Node: "n1", Pool: "pc-p1", Tags: CreatedTag(7)}}
	list, err := s.SampledServerProvider("r", "p1", page, cached).List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %+v", err, list)
	}
	srv := list[0].Data["server"].(map[string]any)
	if srv["snapshots"] == nil || srv["protection"] == nil {
		t.Fatalf("carried state lost: %+v", srv)
	}
}

func TestSplitVMsSkipsImageBuild(t *testing.T) {
	vms, templates := splitVMs([]ClusterResource{
		{Type: "qemu", VMID: 1, Pool: "p", Name: "web"},
		{Type: "qemu", VMID: 2, Pool: "p", Name: imageBuildPrefix + "golden"},
		{Type: "qemu", VMID: 3, Pool: "p", Name: "golden", Template: 1},
		{Type: "qemu", VMID: 4, Pool: "q", Name: "other"},
	}, "p")
	if len(vms) != 1 || vms[0].VMID != 1 || len(templates) != 1 || templates[0].VMID != 3 {
		t.Fatalf("vms=%+v templates=%+v", vms, templates)
	}
}
