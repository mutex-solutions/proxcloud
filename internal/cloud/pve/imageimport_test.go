package pve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

type importCluster struct {
	s          *Service
	uploaded   map[string]string // the upload's fields; "file" is its bytes, "filename" its name
	order      []string
	uploadLen  int64
	rootAvail  int64
	cfg        url.Values
	created    url.Values
	deletedVM  bool
	deletedVol string
	template   bool
	size       int64
	deny       string
	failTask   string
	noFile     bool
	// onUploadPoll answers a poll of the task, true when it did.
	onUploadPoll     func(http.ResponseWriter) bool
	taskDone         bool
	deletedAfterTask bool
	onCreate         func()
	onRootCheck      func()
}

func newImportCluster(t *testing.T) *importCluster {
	ok, bad := "UPID:pve1:0:0:0:x:ok:u:", "UPID:pve1:0:0:0:x:bad:u:"
	b := &importCluster{size: 2 << 30, rootAvail: 90 << 30}
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
			if b.noFile {
				http.Error(w, "volume does not exist", http.StatusInternalServerError)
				return
			}
			b.deletedVol = "pc-import-p1-abc.qcow2"
			b.deletedAfterTask = b.taskDone
			task(w, "dropfile")
		},
		"POST /api2/json/nodes/pve1/storage/imp/upload": func(w http.ResponseWriter, r *http.Request) {
			if b.deny == "upload" {
				_, _ = io.Copy(io.Discard, r.Body) // or the server resets the connection and the client may see that first
				http.Error(w, "Permission check failed", http.StatusForbidden)
				return
			}
			b.uploadLen = r.ContentLength
			mr, err := r.MultipartReader()
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			b.uploaded = map[string]string{}
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				data, _ := io.ReadAll(part)
				b.order = append(b.order, part.FormName())
				if part.FormName() == "filename" {
					b.uploaded["filename"], b.uploaded["file"] = part.FileName(), string(data)
				} else {
					b.uploaded[part.FormName()] = string(data)
				}
			}
			_, _ = io.Copy(io.Discard, r.Body) // to the stated length, as pveproxy does: a body cut short never gets an answer
			task(w, "upload")
		},
		"GET /api2/json/cluster/status": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":[{"type":"cluster","name":"lab"},{"type":"node","name":"pve3","local":0},{"type":"node","name":"pve2","local":1}]}`)
		},
		"GET /api2/json/nodes/pve2/status": func(w http.ResponseWriter, _ *http.Request) {
			if b.onRootCheck != nil {
				b.onRootCheck()
			}
			fmt.Fprintf(w, `{"data":{"rootfs":{"total":%d,"avail":%d}}}`, int64(100)<<30, b.rootAvail)
		},
		"GET /api2/json/cluster/nextid": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"data":"300"}`) },
		"POST /api2/json/nodes/pve1/qemu": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			b.created = r.PostForm
			if b.onCreate != nil {
				b.onCreate()
			}
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
			if b.onUploadPoll != nil && b.onUploadPoll(w) {
				return
			}
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"OK"}}`)
		},
		"GET /api2/json/nodes/pve1/tasks/" + bad + "/status": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, `{"data":{"status":"stopped","exitstatus":"secret internal detail"}}`)
		},
	})
	b.s = &Service{C: c, ServiceID: "svc", PoolPrefix: "st-"}
	return b
}

const imageBytes = "QFI\xfbnot really an image"

func goodImport() ImageImport {
	return ImageImport{Name: "golden", Source: strings.NewReader(imageBytes), SourceSize: int64(len(imageBytes)), Format: "qcow2", Checksum: "abc", ChecksumAlgorithm: "sha256",
		Node: "pve1", ImportStorage: "imp", DiskStorage: "local-zfs", HeadroomPct: 10}
}

func TestImportImage(t *testing.T) {
	b := newImportCluster(t)
	checked := int64(0)
	req := goodImport()
	req.CheckDisk = func(_ context.Context, size int64) error { checked = size; return nil }
	vmid, size, err := b.s.ImportImage(context.Background(), "p1", req)
	if err != nil || vmid != 300 || size != 2<<30 {
		t.Fatalf("%d %d %v", vmid, size, err)
	}
	// The bytes arrive here, so a node has no URL to fetch. fakePVE has no download-url route to call.
	if b.uploaded["content"] != "import" || b.uploaded["file"] != imageBytes || b.uploaded["checksum"] != "abc" || b.uploaded["checksum-algorithm"] != "sha256" {
		t.Fatalf("%v", b.uploaded)
	}
	if got := strings.Join(b.order, " "); got != "content checksum-algorithm checksum filename" {
		t.Fatalf("pveproxy's parser answers 400 to any other order: %s", got)
	}
	if b.uploadLen <= int64(len(imageBytes)) {
		t.Fatalf("PVE refuses an upload with no Content-Length: %d", b.uploadLen)
	}
	if checked != 2<<30 {
		t.Fatalf("the disk's virtual size goes to the check: %d", checked)
	}
	file := b.uploaded["filename"]
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
	b.deny = "upload"
	wantConflict(t, firstErr(b.s.ImportImage(ctx, "p1", goodImport())), "Datastore.AllocateTemplate")
	if b.created != nil || b.deletedVol != "" {
		t.Fatalf("a refused upload creates nothing: %+v", b)
	}

	// pveproxy spools to the API node's root disk first, so a big image must not fill it.
	b = newImportCluster(t)
	b.rootAvail = 11 << 30
	big := goodImport()
	big.SourceSize = 2 << 30
	wantConflict(t, firstErr(b.s.ImportImage(ctx, "p1", big)), "too large")
	if b.uploaded != nil {
		t.Fatalf("nothing may be sent: %v", b.uploaded)
	}

	// A source that ends short of its stated size must not pass for an upload.
	b = newImportCluster(t)
	short := goodImport()
	short.SourceSize += 10
	if err := firstErr(b.s.ImportImage(ctx, "p1", short)); err == nil || b.created != nil {
		t.Fatalf("%v %+v", err, b.created)
	}

	// The disk's size comes from the file: refused then, no VM, and the file goes.
	b = newImportCluster(t)
	refused := errors.New("over quota")
	overDisk := goodImport()
	overDisk.CheckDisk = func(context.Context, int64) error { return refused }
	if err := firstErr(b.s.ImportImage(ctx, "p1", overDisk)); !errors.Is(err, refused) || b.created != nil || b.deletedVol == "" {
		t.Fatalf("%v %+v", err, b)
	}

	b = newImportCluster(t)
	b.failTask = "upload"
	err := firstErr(b.s.ImportImage(ctx, "p1", goodImport()))
	wantConflict(t, err, "not a valid")
	if strings.Contains(err.Error(), "secret internal") || b.created != nil {
		t.Fatalf("the node's task log stays out of the message: %v", err)
	}

	b = newImportCluster(t)
	b.size = (MaxImportDiskGiB + 1) << 30
	wantConflict(t, firstErr(b.s.ImportImage(ctx, "p1", goodImport())), "limit")
	if b.created != nil || b.deletedVol == "" {
		t.Fatalf("too big: no VM, file gone: %+v", b)
	}

	b = newImportCluster(t)
	b.failTask = "config"
	err = firstErr(b.s.ImportImage(ctx, "p1", goodImport()))
	wantConflict(t, err, "could not be imported")
	if !b.deletedVM || b.deletedVol == "" || b.template {
		t.Fatalf("no VM and no file left: %+v", b)
	}

	b = newImportCluster(t)
	b.failTask = "template"
	if err := firstErr(b.s.ImportImage(ctx, "p1", goodImport())); err == nil || !b.deletedVM || b.deletedVol == "" {
		t.Fatalf("no VM and no file left: %v %+v", err, b)
	}
}

// The upload's task runs on the node whatever becomes of the call that started it. A cancelled call or a failed poll must
// not return before the task is over, or the file lands with the project's lock released and nothing to remove it.
func TestImportImageWaitsOutTheUploadTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := newImportCluster(t)
	polls := 0
	b.onUploadPoll = func(w http.ResponseWriter) bool {
		polls++
		switch polls {
		case 1:
			cancel() // the caller is gone
			http.Error(w, "proxy error", http.StatusBadGateway)
		case 2:
			http.Error(w, "proxy error", http.StatusBadGateway)
		case 3:
			fmt.Fprint(w, `{"data":{"status":"running"}}`)
		default:
			b.taskDone = true
			return false
		}
		return true
	}
	if err := firstErr(b.s.ImportImage(ctx, "p1", goodImport())); err == nil {
		t.Fatal("a cancelled import goes no further than the upload")
	}
	if !b.taskDone || b.deletedVol == "" || !b.deletedAfterTask {
		t.Fatalf("the file goes, and only after its task: done=%v deleted=%q after=%v", b.taskDone, b.deletedVol, b.deletedAfterTask)
	}
	if b.created != nil {
		t.Fatalf("no VM from a cancelled import: %v", b.created)
	}
}

// A refused upload keeps no file, so there is nothing to drop and no cause to report a file as left behind.
func TestRefusedUploadDropsNoFile(t *testing.T) {
	b := newImportCluster(t)
	b.failTask = "upload"
	b.noFile = true
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(old)
	wantConflict(t, firstErr(b.s.ImportImage(context.Background(), "p1", goodImport())), "not a valid")
	if strings.Contains(logged.String(), "stays on the store") {
		t.Fatalf("%s", logged.String())
	}
}

// One upload at a time per provider, for as long as the body is on the API node's disk: not through the rest of the import.
func TestImportUploadLock(t *testing.T) {
	b := newImportCluster(t)
	held := map[string]bool{}
	var heldAtCreate bool
	b.s.ServiceID = "svc"
	b.s.HolderLock = func(_ context.Context, name string, _, wait time.Duration) (func(), bool, error) {
		if held[name] {
			return nil, false, nil
		}
		held[name] = true
		return func() { held[name] = false }, true, nil
	}
	const lock = "pve-image-upload-svc"
	b.onCreate = func() { heldAtCreate = held[lock] }
	// Taken before the room check, or two could pass it.
	b.onRootCheck = func() {
		if !held[lock] {
			t.Error("the room check ran outside the lock")
		}
	}
	if err := firstErr(b.s.ImportImage(context.Background(), "p1", goodImport())); err != nil {
		t.Fatal(err)
	}
	if heldAtCreate || held[lock] {
		t.Fatalf("released once the body is off the node: at create %v, after %v", heldAtCreate, held[lock])
	}
	// A source that stops mid-body lets go of the lock at once, with no task to wait for and nothing to remove.
	b.deletedVol = ""
	failing := goodImport()
	failing.Source = io.MultiReader(strings.NewReader("QFI"), iotest.ErrReader(errors.New("source too slow")))
	if err := firstErr(b.s.ImportImage(context.Background(), "p1", failing)); err == nil || !strings.Contains(err.Error(), "source too slow") {
		t.Fatalf("the source's error comes back: %v", err)
	}
	if held[lock] || b.deletedVol != "" {
		t.Fatalf("lock held %v, dropped %q", held[lock], b.deletedVol)
	}
	// Another project's import while one uploads: refused with nothing sent.
	b = newImportCluster(t)
	b.s.ServiceID = "svc"
	b.s.HolderLock = func(context.Context, string, time.Duration, time.Duration) (func(), bool, error) {
		return nil, false, nil
	}
	wantConflict(t, firstErr(b.s.ImportImage(context.Background(), "p2", goodImport())), "being uploaded on this provider")
	if b.uploaded != nil {
		t.Fatalf("%v", b.uploaded)
	}
}

func firstErr(_ int, _ int64, err error) error { return err }

func TestLockImageImport(t *testing.T) {
	s := &Service{}
	release, err := s.LockImageImport(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	release()
	s.HolderLock = func(_ context.Context, name string, _, wait time.Duration) (func(), bool, error) {
		if name != "pve-image-import-p1" || wait != 0 {
			t.Fatalf("%s waits %v", name, wait)
		}
		return nil, false, nil
	}
	_, err = s.LockImageImport(context.Background(), "p1")
	wantConflict(t, err, "Another image import")
}
