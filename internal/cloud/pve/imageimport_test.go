package pve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type importCluster struct {
	s          *Service
	download   url.Values
	cfg        url.Values
	created    url.Values
	deletedVM  bool
	deletedVol string
	template   bool
	size       int64
	deny       string
	failTask   string
}

func newImportCluster(t *testing.T) *importCluster {
	ok, bad := "UPID:pve1:0:0:0:x:ok:u:", "UPID:pve1:0:0:0:x:bad:u:"
	b := &importCluster{size: 2 << 30}
	task := func(w http.ResponseWriter, name string) {
		u := ok
		if b.failTask == name {
			u = bad
		}
		fmt.Fprintf(w, `{"data":%q}`, u)
	}
	importSuffix = func() string { return "abc" }
	t.Cleanup(func() { importSuffix = func() string { return randBase36(8) } })
	_, c := fakePVE(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api2/json/nodes/pve1/storage/imp/content/imp:import/pc-import-p1-abc.qcow2": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"data":{"size":%d}}`, b.size)
		},
		"DELETE /api2/json/nodes/pve1/storage/imp/content/import/pc-import-p1-abc.qcow2": func(w http.ResponseWriter, _ *http.Request) {
			b.deletedVol = "pc-import-p1-abc.qcow2"
			task(w, "dropfile")
		},
		"POST /api2/json/nodes/pve1/storage/imp/download-url": func(w http.ResponseWriter, r *http.Request) {
			if b.deny == "download" {
				http.Error(w, "Permission check failed", http.StatusForbidden)
				return
			}
			_ = r.ParseForm()
			b.download = r.PostForm
			task(w, "download")
		},
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":"300"}`) },
		"POST /api2/json/nodes/pve1/qemu": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			b.created = r.PostForm
			task(w, "create")
		},
		"POST /api2/json/nodes/pve1/qemu/300/config": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if b.cfg == nil {
				b.cfg = r.PostForm
			}
			task(w, "config")
		},
		"POST /api2/json/nodes/pve1/qemu/300/template": func(w http.ResponseWriter, _ *http.Request) {
			b.template = true
			task(w, "template")
		},
		"POST /api2/json/nodes/pve1/qemu/300/status/stop": func(w http.ResponseWriter, _ *http.Request) { task(w, "stop") },
		"DELETE /api2/json/nodes/pve1/qemu/300": func(w http.ResponseWriter, _ *http.Request) {
			b.deletedVM = true
			task(w, "delete")
		},
		"GET /api2/json/nodes/pve1/tasks/" + ok + "/status": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
		"GET /api2/json/nodes/pve1/tasks/" + bad + "/status": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"secret internal detail"}}`)
		},
	})
	b.s = &Service{C: c, ServiceID: "svc", PoolPrefix: "st-"}
	return b
}

var goodImport = ImageImport{Name: "golden", URL: "https://example.com/a.qcow2", Format: "qcow2", Checksum: "abc", ChecksumAlgorithm: "sha256", Node: "pve1", ImportStorage: "imp", DiskStorage: "local-zfs"}

func TestImportImage(t *testing.T) {
	b := newImportCluster(t)
	vmid, size, err := b.s.ImportImage(context.Background(), "p1", goodImport)
	if err != nil || vmid != 300 || size != 2<<30 {
		t.Fatalf("%d %d %v", vmid, size, err)
	}
	if b.download.Get("content") != "import" || b.download.Get("url") != goodImport.URL || b.download.Get("checksum") != "abc" || b.download.Get("checksum-algorithm") != "sha256" {
		t.Fatalf("%v", b.download)
	}
	file := b.download.Get("filename")
	if !strings.HasPrefix(file, "pc-import-p1-") || !strings.HasSuffix(file, ".qcow2") {
		t.Fatalf("the name comes from the portal, never the tenant: %q", file)
	}
	if b.created.Get("pool") != "st-p1" || b.created.Get("name") != imageBuildPrefix+"golden" {
		t.Fatalf("in the pool, and named so that no sample takes it for a server: %v", b.created)
	}
	if got := b.cfg.Get("scsi0"); got != "local-zfs:0,discard=on,import-from=imp:import/"+file {
		t.Fatalf("%q", got)
	}
	if b.cfg.Has("net0") {
		t.Fatalf("a template needs no NIC, and no SDN.Use: %v", b.cfg)
	}
	if !b.template || b.deletedVM || b.deletedVol == "" {
		t.Fatalf("converted, kept, file dropped: %+v", b)
	}
}

func TestImportImageFailures(t *testing.T) {
	ctx := context.Background()

	b := newImportCluster(t)
	b.deny = "download"
	wantConflict(t, firstErr(b.s.ImportImage(ctx, "p1", goodImport)), "Datastore.AllocateTemplate")
	if b.created != nil || b.deletedVol != "" {
		t.Fatalf("a refused download creates nothing: %+v", b)
	}

	b = newImportCluster(t)
	b.failTask = "download"
	err := firstErr(b.s.ImportImage(ctx, "p1", goodImport))
	wantConflict(t, err, "could not be downloaded")
	if strings.Contains(err.Error(), "secret internal") || b.created != nil {
		t.Fatalf("the node's task log stays out of the message: %v", err)
	}

	b = newImportCluster(t)
	b.size = (MaxImportDiskGiB + 1) << 30
	wantConflict(t, firstErr(b.s.ImportImage(ctx, "p1", goodImport)), "limit")
	if b.created != nil || b.deletedVol == "" {
		t.Fatalf("too big: no VM, file gone: %+v", b)
	}

	b = newImportCluster(t)
	b.failTask = "config"
	err = firstErr(b.s.ImportImage(ctx, "p1", goodImport))
	wantConflict(t, err, "could not be imported")
	if !b.deletedVM || b.deletedVol == "" || b.template {
		t.Fatalf("no VM and no file left: %+v", b)
	}

	b = newImportCluster(t)
	b.failTask = "template"
	if err := firstErr(b.s.ImportImage(ctx, "p1", goodImport)); err == nil || !b.deletedVM || b.deletedVol == "" {
		t.Fatalf("no VM and no file left: %v %+v", err, b)
	}
}

func firstErr(_ int, _ int64, err error) error { return err }
