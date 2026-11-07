package pve

import (
	"context"
	"encoding/json"
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
	for name, p := range map[string]*ServerSyncProvider{
		"full":    s.ServerSyncProvider("r", "p1", nil),
		"sampled": s.SampledServerProvider("r", "p1", page, nil),
	} {
		list, err := p.List(ctx)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(list) != 1 || list[0].ExternalID != "101" {
			t.Fatalf("%s: listed %+v, want only the VM that can be read", name, list)
		}
		row := serverRow("103", "ACTIVE")
		if keep, changed := p.KeepUnread(row); !keep || !changed {
			t.Fatalf("%s: keep=%v changed=%v", name, keep, changed)
		}
		srv := row.Data["server"].(map[string]any)
		if srv["status"] != "UNKNOWN" || srv["name"] != "was-here" || srv["addresses"] == nil {
			t.Fatalf("%s: row = %v, want only the status changed", name, srv)
		}
		if keep, changed := p.KeepUnread(row); !keep || changed {
			t.Fatalf("%s: second pass keep=%v changed=%v, want no write", name, keep, changed)
		}
		if keep, _ := p.KeepUnread(serverRow("999", "ACTIVE")); keep {
			t.Fatalf("%s: a VM PVE no longer lists is gone", name)
		}
	}
}

func TestDownNodeVolumeAndImageRowsStay(t *testing.T) {
	s := nodeDownCluster(t)
	ctx := context.Background()
	vols := s.VolumeSyncProvider("r", "p1", nil)
	list, err := vols.List(ctx)
	if err != nil {
		t.Fatalf("one dead node must not stop the project's volume sync: %v", err)
	}
	if len(list) != 1 || list[0].ExternalID != "ceph-vm:vm-101-disk-0" {
		t.Fatalf("volumes = %+v", list)
	}
	row := func(vmid any) *cloud.CloudResource {
		return &cloud.CloudResource{Data: map[string]any{"volume": map[string]any{"vmid": vmid}}}
	}
	if keep, changed := vols.KeepUnread(row(json.Number("103"))); !keep || changed {
		t.Fatalf("disk of a VM on the dead node: keep=%v changed=%v", keep, changed)
	}
	if keep, _ := vols.KeepUnread(row(json.Number("999"))); keep {
		t.Fatal("a disk of a VM PVE no longer lists is gone")
	}
	if keep, _ := vols.KeepUnread(row(json.Number("105"))); keep {
		t.Fatal("another project's VM says nothing about this project's rows")
	}

	imgs := s.ImageSyncProvider("r", "p1")
	if list, err := imgs.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("images = %+v, %v", list, err)
	}
	if keep, _ := imgs.KeepUnread(&cloud.CloudResource{ExternalID: "104"}); !keep {
		t.Fatal("a template on the dead node is not gone")
	}
}

// The node died a moment ago and PVE still calls it online: a read failing must not wipe what the row knows.
func TestUnreadableVMKeepsItsRow(t *testing.T) {
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/cluster/resources": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("type") == "node" {
				fmt.Fprint(w, `{"data":[{"type":"node","node":"pve1","status":"online"}]}`)
				return
			}
			fmt.Fprint(w, `{"data":[{"type":"qemu","vmid":101,"node":"pve1","name":"a","status":"running","pool":"tp-p1"}]}`)
		},
		"GET /api2/json/nodes/pve1/qemu/101/config": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(595)
		},
	})
	s := &Service{C: c, PoolPrefix: "tp-"}
	p := s.ServerSyncProvider("r", "p1", nil)
	if list, err := p.List(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	row := serverRow("101", "ACTIVE")
	if keep, changed := p.KeepUnread(row); !keep || changed {
		t.Fatalf("keep=%v changed=%v, want the row left alone", keep, changed)
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
