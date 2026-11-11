package pve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// nodeDownCluster is what PVE 9.2 answered on the lab with node pve2 powered off: 102 as last seen (the first minutes),
// 103 and 104 with no name, sizes or template flag (after that). 104 is a template and a VM alike on such a row.
func nodeDownCluster(t *testing.T) *Service {
	t.Helper()
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") == "node" {
				fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"},{"type":"node","node":"pve2","status":"offline"}]}`)
				return
			}
			fmt.Fprint(w, `{"data":[
				{"type":"qemu","vmid":101,"node":"pve1","name":"up","status":"running","pool":"tp-p1","tags":"proxcloud","maxcpu":2,"maxmem":2147483648},
				{"type":"qemu","vmid":102,"node":"pve2","name":"last-seen","status":"running","pool":"tp-p1","tags":"proxcloud","maxcpu":2,"maxmem":2147483648},
				{"type":"qemu","vmid":103,"node":"pve2","status":"unknown","pool":"tp-p1","tags":"proxcloud"},
				{"type":"qemu","vmid":104,"node":"pve2","status":"unknown","pool":"tp-p1"},
				{"type":"qemu","vmid":105,"node":"pve2","status":"unknown","pool":"tp-OTHER"}
			]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"scsi0":"ceph-vm:vm-101-disk-0,size=10G","ipconfig0":"ip=10.0.0.5/24","net0":"virtio=AA,bridge=v1"}}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/status/current": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"running","qmpstatus":"running"}}`)
		},
	}
	srv, c := fakePVE(t, routes)
	base := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/nodes/pve2/") {
			t.Errorf("called the dead node: %s", r.URL.Path)
		}
		base.ServeHTTP(w, r)
	})
	return &Service{C: c, ServiceID: "svc", PoolPrefix: "tp-"}
}

func serverRow(id, status string) *cloud.CloudResource {
	return &cloud.CloudResource{ExternalID: id, Data: map[string]any{"server": map[string]any{
		"name": "was-here", "status": status, "addresses": map[string]any{"private": []any{"x"}},
	}}}
}

func TestDownNodeServerRowStays(t *testing.T) {
	s := nodeDownCluster(t)
	ctx := context.Background()
	page, err := s.C.ClusterResources(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !page[1].NodeDown || page[1].Status != "unknown" || page[0].NodeDown {
		t.Fatalf("a stale running row must not survive: %+v", page[:2])
	}
	for name, p := range map[string]*ServerSyncProvider{"full": s.ServerSyncProvider("r", "p1", nil), "sampled": s.SampledServerProvider("r", "p1", page, nil)} {
		_, err := p.List(ctx)
		var unknown *cloud.InventoryUntrustedError
		if !errors.As(err, &unknown) {
			t.Fatalf("%s: want unknown scope, got %v", name, err)
		}
		row := serverRow("103", "ACTIVE")
		p.KeepUnread(row)
		if row.Data["server"].(map[string]any)["status"] != "ACTIVE" {
			t.Fatal("unknown changed cached payload")
		}
	}
}
func TestDownNodeVolumeAndImageRowsStay(t *testing.T) {
	s := nodeDownCluster(t)
	for _, p := range []interface {
		List(context.Context) ([]cloud.CloudResource, error)
	}{s.VolumeSyncProvider("r", "p1", nil), s.ImageSyncProvider("r", "p1")} {
		_, err := p.List(context.Background())
		var unknown *cloud.InventoryUntrustedError
		if !errors.As(err, &unknown) {
			t.Fatalf("want whole-scope retention, got %v", err)
		}
	}
}
func TestUnreadableVMKeepsItsRow(t *testing.T) {
	s := nodeDownCluster(t)
	_, err := s.ServerSyncProvider("r", "p1", nil).List(context.Background())
	var unknown *cloud.InventoryUntrustedError
	if !errors.As(err, &unknown) {
		t.Fatal("unreadable scope must stop before writes")
	}
}

func TestDownNodeRefusals(t *testing.T) {
	s := nodeDownCluster(t)
	ctx := context.Background()
	var ce *ConflictError
	// The switch below would answer STOP on a VM that does not read as running with a success.
	for _, verb := range []string{"STOP", "START", "REBOOT"} {
		if err := s.PowerAction(ctx, "p1", "103", verb); !errors.As(err, &ce) {
			t.Fatalf("%s: err = %v, want a refusal", verb, err)
		}
	}
	if _, err := s.VnetInUse(ctx, "p1", "v1"); !errors.As(err, &ce) {
		t.Fatalf("VnetInUse: err = %v, want a refusal: a VM that cannot be read may use it", err)
	}
	if _, err := s.SecurityGroupInUse(ctx, "p1", "g1"); !errors.As(err, &ce) {
		t.Fatalf("SecurityGroupInUse: err = %v, want a refusal", err)
	}
}

func TestDiscoverTemplatesSkipsDownNode(t *testing.T) {
	rows := []ClusterResource{{Type: "qemu", VMID: 9, Node: "pve2", Template: 1, Pool: TemplatePool, Tags: TagTemplatePub + ";" + TagTemplateImage + "debian-13", NodeDown: true}}
	if got := DiscoverTemplates(rows); len(got) != 0 {
		t.Fatalf("a clone from a dead node's template cannot start: %+v", got)
	}
}

func TestIsNodeUnreachable(t *testing.T) {
	if !IsNodeUnreachable(fmt.Errorf("x: %w", &APIError{Status: 595})) || IsNodeUnreachable(&APIError{Status: 500}) || IsNodeUnreachable(errors.New("x")) {
		t.Fatal("595 and 596 only")
	}
}

func TestResolveVMRefusesForeignServer(t *testing.T) {
	s := nodeDownCluster(t)
	for _, id := range []string{"105", "abc"} {
		if _, err := s.ResolveVM(context.Background(), "p1", id); !errors.Is(err, ErrNotInProject) {
			t.Errorf("%s: %v, want ErrNotInProject", id, err)
		}
	}
	if vm, err := s.ResolveVM(context.Background(), "p1", "101"); err != nil || vm == nil {
		t.Fatalf("own server: %v %v", vm, err)
	}
}
