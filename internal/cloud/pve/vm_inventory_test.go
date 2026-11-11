package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// Explicit successful proof fixture: legacy tests formerly provided only a VM
// page and thereby accidentally modeled a filtered/opaque inventory as a census.
func fakeVMInventoryService(t *testing.T, page []ClusterResource, configs map[int]VMConfig) (*Service, []ClusterResource) {
	t.Helper()
	members := []PoolMember{}
	for i := range page {
		r := &page[i]
		r.ID = fmt.Sprintf("%s/%d", r.Type, r.VMID)
		if r.Pool != "pc-p1" {
			continue
		}
		if r.MaxCPU == 0 {
			r.MaxCPU = 1
		}
		if r.MaxMem == 0 {
			r.MaxMem = 1 << 30
		}
		if r.Status == "" {
			r.Status = "running"
		}
		members = append(members, PoolMember{ID: r.ID, Type: r.Type, VMID: r.VMID, Node: r.Node})
		cfg := configs[r.VMID]
		if cfg == nil {
			cfg = VMConfig{}
			configs[r.VMID] = cfg
		}
		cfg["name"], cfg["tags"], cfg["template"] = r.Name, r.Tags, r.Template
		cfg["smbios1"] = fmt.Sprintf("uuid=00000000-0000-4000-8000-%012d", r.VMID)
		if cfg["scsi0"] == nil {
			cfg["scsi0"] = "z:vm-" + fmt.Sprint(r.VMID) + "-disk-0,size=8G"
		}
	}
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": page})
		},
		"GET /api2/json/nodes": say(`{"data":[{"node":"n1","status":"online"}]}`),
		"GET /api2/json/pools/pc-p1": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": PoolInfo{PoolID: "pc-p1", Members: members}})
		},
		"GET /api2/json/access/permissions": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{r.URL.Query().Get("path"): map[string]int{"VM.Audit": 1}}})
		},
	}
	routes["GET /api2/json/cluster/replication"] = say(`{"data":[]}`)
	for id, cfg := range configs {
		path := "GET /api2/json/nodes/n1/qemu/" + fmt.Sprint(id)
		routes[path+"/config"] = func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": cfg})
		}
		routes[path+"/pending"] = say(`{"data":[]}`)
		routes[path+"/snapshot"] = say(`{"data":[]}`)
		routes[path+"/status/current"] = say(`{"data":{"status":"running","qmpstatus":"running"}}`)
	}
	_, c := fakePVE(t, routes)
	return &Service{C: c, ServiceID: "svc", PoolPrefix: "pc-"}, page
}

func TestVMInventoryRejectsMalformedPermissions(t *testing.T) {
	for _, flag := range []string{"null", "{}", "\"1\""} {
		t.Run(strings.ReplaceAll(flag, "\"", "q"), func(t *testing.T) {
			_, c := fakePVE(t, fakeRoutes{"GET /api2/json/access/permissions": say(`{"data":{"/vms/1":{"VM.Audit":` + flag + `}}}`)})
			if err := c.vmInventoryPermission(context.Background(), "/vms/1", "VM.Audit", false); err == nil {
				t.Fatal("unknown permission accepted")
			}
		})
	}
}
func TestSampleCarryOnlySameProvedUUID(t *testing.T) {
	cfg := VMConfig{"smbios1": "uuid=00000000-0000-4000-8000-000000000101"}
	old := &cloud.CloudResource{Data: map[string]any{vmUUIDKey: "00000000-0000-4000-8000-000000000101", "server": map[string]any{"status": "PAUSED", "addresses": map[string]any{"private": []any{"keep"}}, "snapshots": map[string]any{"diskGb": 8}}}}
	p := &ServerSyncProvider{cached: map[string]*cloud.CloudResource{"101": old}, inventory: &vmInventory{configs: map[int]VMConfig{101: cfg}}}
	r := ClusterResource{VMID: 101, Status: "running"}
	data := p.sampledData(r)["server"].(map[string]any)
	if data["status"] != "PAUSED" || data["snapshots"] == nil {
		t.Fatal("same UUID lost enrichments")
	}
	old.Data[vmUUIDKey] = "other"
	data = p.sampledData(r)["server"].(map[string]any)
	if data["status"] != "ACTIVE" || data["snapshots"] != nil || len(data["addresses"].(map[string]any)) != 0 {
		t.Fatal("old VM state crossed identity boundary")
	}
}
