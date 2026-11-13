//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// ── a fake Proxmox VE API (the /api2/json surface the lifecycle and smoke test use) ───────────

type fakeVMRow struct {
	node     string
	cfg      map[string]string
	template bool
	running  bool
	digest   int
	grownGiB int
}

type fakePVEAPI struct {
	mu        sync.Mutex
	vms       map[int]*fakeVMRow
	mutations []string // "<verb> <vmid>"
	execs     map[int]map[string]any
	nextPID   int
	// knobs
	ciStatus      string
	failTagWrites bool // a config write carrying tags answers 500 (unreachable/changed)
	down          bool // the whole API answers 503 (cluster unreachable)
}

func newFakePVEAPI() *fakePVEAPI {
	return &fakePVEAPI{vms: map[int]*fakeVMRow{}, execs: map[int]map[string]any{}, ciStatus: "done"}
}

var qemuPathRE = regexp.MustCompile(`^/nodes/([^/]+)/qemu/(\d+)(/.*)?$`)

func (f *fakePVEAPI) upid(node, kind string, vmid int) string {
	return fmt.Sprintf("UPID:%s:0000AAAA:0000BBBB:6700CCCC:%s:%d:root@pam:", node, kind, vmid)
}

func (f *fakePVEAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	if f.down {
		w.WriteHeader(503)
		return
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	path := strings.TrimPrefix(r.URL.Path, "/api2/json")
	switch {
	case path == "/cluster/resources" && r.Form.Get("type") == "node":
		write([]any{map[string]any{"type": "node", "node": "pve1", "status": "online"}})
		return
	case path == "/cluster/resources":
		var rows []any
		for id, vm := range f.vms {
			t := 0
			if vm.template {
				t = 1
			}
			rows = append(rows, map[string]any{"type": "qemu", "vmid": id, "node": vm.node, "name": vm.cfg["name"], "pool": vm.cfg["pool"],
				"template": t, "tags": vm.cfg["tags"], "maxdisk": 20 << 30, "status": map[bool]string{true: "running", false: "stopped"}[vm.running]})
		}
		write(rows)
		return
	case strings.HasPrefix(path, "/pools/"):
		pool := strings.TrimPrefix(path, "/pools/")
		var members []any
		for id, vm := range f.vms {
			if vm.cfg["pool"] == pool {
				members = append(members, map[string]any{"type": "qemu", "vmid": id, "node": vm.node, "id": fmt.Sprintf("qemu/%d", id)})
			}
		}
		write(map[string]any{"poolid": pool, "members": members})
		return
	case path == "/cluster/status":
		write([]any{map[string]any{"type": "cluster", "quorate": 1}, map[string]any{"type": "node", "name": "pve1", "ip": "192.0.2.11"},
			map[string]any{"type": "node", "name": "pve2", "ip": "192.0.2.12"}})
		return
	case strings.HasPrefix(path, "/nodes/") && strings.HasSuffix(path, "/network"):
		ip := "192.0.2.11"
		if strings.Contains(path, "/pve2/") {
			ip = "192.0.2.12"
		}
		write([]any{map[string]any{"iface": "vmbr0", "address": ip, "cidr": ip + "/24", "gateway": "192.0.2.1"},
			map[string]any{"iface": "vmbr9", "cidr": ""}})
		return
	case strings.Contains(path, "/tasks/") && strings.HasSuffix(path, "/status"):
		write(map[string]any{"status": "stopped", "exitstatus": "OK"})
		return
	}
	m := qemuPathRE.FindStringSubmatch(path)
	if m == nil {
		w.WriteHeader(404)
		return
	}
	node, rest := m[1], m[3]
	vmid, _ := strconv.Atoi(m[2])
	vm := f.vms[vmid]
	missing := func() {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"data":null,"errors":{"vmid":"Configuration file does not exist"}}`))
	}
	if vm == nil || vm.node != node {
		missing()
		return
	}
	mut := func(verb string) { f.mutations = append(f.mutations, fmt.Sprintf("%s %d", verb, vmid)) }
	switch {
	case rest == "/config" && r.Method == http.MethodGet:
		out := map[string]any{"digest": strconv.Itoa(vm.digest)}
		for k, v := range vm.cfg {
			out[k] = v
		}
		if vm.template {
			out["template"] = 1
		}
		write(out)
	case rest == "/config" && r.Method == http.MethodPost:
		if f.failTagWrites && r.PostForm.Get("tags") != "" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"data":null,"message":"got timeout"}`))
			return
		}
		if d := r.PostForm.Get("digest"); d != "" && d != strconv.Itoa(vm.digest) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"data":null,"message":"config digest mismatch"}`))
			return
		}
		mut("config")
		for k, vs := range r.PostForm {
			if k == "digest" || k == "delete" {
				continue
			}
			v := vs[0]
			if k == "sshkeys" {
				v, _ = url.QueryUnescape(v)
			}
			vm.cfg[k] = v
		}
		for _, k := range strings.Split(r.PostForm.Get("delete"), ",") {
			delete(vm.cfg, k)
		}
		vm.digest++
		write(f.upid(node, "qmconfig", vmid))
	case rest == "/clone":
		mut("clone")
		id, _ := strconv.Atoi(r.PostForm.Get("newid"))
		cfg := map[string]string{}
		for k, v := range vm.cfg {
			cfg[k] = v
		}
		cfg["name"], cfg["pool"] = r.PostForm.Get("name"), r.PostForm.Get("pool")
		f.vms[id] = &fakeVMRow{node: node, cfg: cfg}
		write(f.upid(node, "qmclone", vmid))
	case rest == "/resize":
		mut("resize")
		var n int
		fmt.Sscanf(r.PostForm.Get("size"), "+%dG", &n)
		vm.grownGiB += n
		write(f.upid(node, "resize", vmid))
	case rest == "/status/start":
		mut("start")
		vm.running = true
		write(f.upid(node, "qmstart", vmid))
	case rest == "/status/stop":
		mut("stop")
		vm.running = false
		write(f.upid(node, "qmstop", vmid))
	case rest == "/status/current":
		write(map[string]any{"status": map[bool]string{true: "running", false: "stopped"}[vm.running]})
	case rest == "" && r.Method == http.MethodDelete:
		mut("destroy")
		delete(f.vms, vmid)
		write(f.upid(node, "qmdestroy", vmid))
	case strings.HasPrefix(rest, "/agent/"):
		if !vm.running {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"data":null,"message":"QEMU guest agent is not running"}`))
			return
		}
		switch rest {
		case "/agent/ping":
			write(map[string]any{"result": map[string]any{}})
		case "/agent/get-osinfo":
			write(map[string]any{"result": map[string]any{"id": "debian", "version-id": "13", "pretty-name": "Debian GNU/Linux 13 (trixie)"}})
		case "/agent/get-fsinfo":
			write(map[string]any{"result": []any{map[string]any{"mountpoint": "/", "type": "ext4", "total-bytes": int64(20<<30) + int64(vm.grownGiB)<<30 - 512<<20}}})
		case "/agent/exec":
			mut("exec")
			f.nextPID++
			cmd := r.PostForm["command"]
			res := map[string]any{"exited": 1, "exitcode": 127}
			if len(cmd) > 0 && cmd[0] == "/usr/bin/cloud-init" {
				res = map[string]any{"exited": 1, "exitcode": 0, "out-data": fmt.Sprintf(`{"status":%q}`, f.ciStatus)}
			}
			f.execs[f.nextPID] = res
			write(map[string]any{"pid": f.nextPID})
		case "/agent/exec-status":
			pid, _ := strconv.Atoi(r.Form.Get("pid"))
			write(f.execs[pid])
		default:
			w.WriteHeader(404)
		}
	default:
		w.WriteHeader(404)
	}
}

func (f *fakePVEAPI) mutationsOf(vmid int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.mutations {
		if strings.HasSuffix(m, " "+strconv.Itoa(vmid)) {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakePVEAPI) tags(vmid int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm := f.vms[vmid]; vm != nil {
		return vm.cfg["tags"]
	}
	return ""
}

func (f *fakePVEAPI) smokeVMs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for id, vm := range f.vms {
		if strings.HasPrefix(vm.cfg["name"], "pc-tu-smoke-") || strings.Contains(vm.cfg["tags"], "pc-tu-smoke") {
			out = append(out, id)
		}
	}
	return out
}

// ── fixture ───────────────────────────────────────────────────────────────────────────────────

type tlFixture struct {
	pv2     *fakePVEAPI // provider svc-pve2 (second cluster)
	db      *pgdoc.DB
	jobs    *durablejobs.Store
	svc     *templateupdates.Service
	pv      *fakePVEAPI
	client  *pve.Client
	client2 *pve.Client
	router  http.Handler
	runner  *templateupdates.Runner
	audit   *audit.Service
}

const tlIss, tlCid = "", ""

func tlPayload(os, build, digestChar string) templateupdates.VMPayload {
	return templateupdates.VMPayload{Version: 1, Package: "proxcloud-vm-" + os, Tag: os + "-os-" + build + "-aaaaaaaaaaaa",
		Digest: "sha256:" + strings.Repeat(digestChar, 64), DiskDigest: "sha256:" + strings.Repeat("e", 64), DiskBytes: 4096,
		VirtualSizeBytes: 20 << 30, OS: os, Variant: "os", BuildVersion: build, ServiceID: "svc-pve", Node: "pve1", ImportStorage: "nas-import", DiskStorage: "local-zfs"}
}

func newTLFixture(t *testing.T, vmEnabled bool) *tlFixture {
	t.Helper()
	ctx := context.Background()
	db := freshPG(t)
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	rec, smokes := templateupdates.PGReceipts{DB: db}, templateupdates.PGSmokes{DB: db}
	if err := rec.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if err := smokes.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	pv := newFakePVEAPI()
	srv := httptest.NewServer(pv)
	t.Cleanup(srv.Close)
	c, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.C("externalService").InsertOne(ctx, pgdoc.M{"_id": "svc-pve", "name": "Lab", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "proxmox", "apiUrl": srv.URL, "region": "pve", "templateSmoke": pgdoc.M{"bridge": "vmbr0", "vlan": 29, "ip": "192.0.2.50/24", "gateway": "192.0.2.1"}}}); err != nil {
		t.Fatal(err)
	}
	for _, ap := range []pgdoc.M{{"_id": "admin-sub", "role": "ADMIN"}, {"_id": "support-sub", "role": "SUPPORT"}} {
		if _, err := db.C("adminPermission").InsertOne(ctx, ap); err != nil {
			t.Fatal(err)
		}
	}
	pv2 := newFakePVEAPI()
	srv2 := httptest.NewServer(pv2)
	t.Cleanup(srv2.Close)
	c2, err := pve.New(pve.Config{APIURL: srv2.URL, TokenID: "proxcloud@pve!platform", Secret: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.C("externalService").InsertOne(ctx, pgdoc.M{"_id": "svc-pve2", "name": "Lab 2", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "proxmox", "apiUrl": srv2.URL, "region": "pve2", "templateSmoke": pgdoc.M{"bridge": "vmbr0", "vlan": 30}}}); err != nil {
		t.Fatal(err)
	}
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("fixture"))
	auditSvc := audit.NewService(audit.NewRepo(db), slog.Default())
	svc := &templateupdates.Service{Token: func(context.Context) (string, string, error) { return "", "", nil }, Jobs: jobs, DB: db,
		Receipts: rec, Smokes: smokes, VMEnabled: vmEnabled}
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, auditSvc, esSvc, "", tlIss, tlCid)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		if es.ID == "svc-pve2" {
			return &pve.Service{C: c2, ServiceID: "svc-pve2"}, nil
		}
		return &pve.Service{C: c, ServiceID: "svc-pve"}, nil
	})
	h.SetTemplateUpdates(svc)
	r := chi.NewRouter()
	h.Routes(r)
	runner := &templateupdates.Runner{Store: jobs, Receipts: rec, Smokes: smokes, Lease: 30 * time.Second,
		Build: func(context.Context, templateupdates.VMPayload) (*templateupdates.Executor, error) {
			t.Fatal("no import job in this test")
			return nil, nil
		},
		SmokeLock: func(ctx context.Context, key string) (func(), bool, error) { return db.TryAdvisoryLock(ctx, key) },
		DeadSmoke: smokes.DeadUncleaned, ClaimDead: jobs.ClaimDead,
		BuildSmoke: func(context.Context, templateupdates.SmokePayload) (*templateupdates.SmokeExecutor, error) {
			return &templateupdates.SmokeExecutor{PVE: c, Receipts: smokes, Poll: 10 * time.Millisecond, BootTimeout: 2 * time.Second, CloudInitTimeout: time.Second,
				Blocked: func(ctx context.Context, d string) (bool, error) { b, err := rec.Blocked(ctx, d); return b != nil, err }}, nil
		}}
	// The cluster: an older published pipeline template, a tenant VM, a CAPMOX-like VM in a tenant pool.
	pv.vms[9200] = &fakeVMRow{node: "pve1", template: true, cfg: map[string]string{"name": "pc-debian-13-202611010000", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000", "scsi0": "local-zfs:base-9200-disk-0,size=3G"}}
	pv.vms[101] = &fakeVMRow{node: "pve1", running: true, cfg: map[string]string{"name": "tenant-vm", "pool": "pc-tenant1", "tags": "proxcloud"}}
	pv2.vms[9200] = &fakeVMRow{node: "pve1", template: true, cfg: map[string]string{"name": "pc-debian-13-202611010000", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-public;pc-img-debian-13;pc-v-202611010000", "scsi0": "local-zfs:base-9200-disk-0,size=3G"}}
	return &tlFixture{db: db, jobs: jobs, svc: svc, pv: pv, pv2: pv2, client: c, client2: c2, router: r, runner: runner, audit: auditSvc}
}

// addImport records a finished (staged) import of p as template vmid, like the import job leaves it.
func (f *tlFixture) addImport(t *testing.T, p templateupdates.VMPayload, vmid int) string {
	t.Helper()
	return f.addImportOn(t, f.pv, p, vmid)
}

func (f *tlFixture) addImportOn(t *testing.T, pv *fakePVEAPI, p templateupdates.VMPayload, vmid int) string {
	t.Helper()
	id := uuid.NewString()
	if err := f.svc.Receipts.Save(context.Background(), &templateupdates.Receipt{ID: id, Kind: "vm", Payload: p, RequestedBy: "admin-sub",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Step: templateupdates.StepSucceeded, VMID: vmid}); err != nil {
		t.Fatal(err)
	}
	pv.mu.Lock()
	pv.vms[vmid] = &fakeVMRow{node: p.Node, template: true, cfg: map[string]string{"name": templateupdates.TemplateName(p), "pool": pve.TemplatePool,
		"tags": templateupdates.TemplateTags(p), "scsi0": fmt.Sprintf("local-zfs:base-%d-disk-0,discard=on,size=20G", vmid)}}
	pv.mu.Unlock()
	return id
}

func (f *tlFixture) do(sub, method, path string, body any) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

func errCode(w *httptest.ResponseRecorder) string {
	var env struct {
		Errors struct {
			Details map[string]any `json:"details"`
		} `json:"errors"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	s, _ := env.Errors.Details["code"].(string)
	return s
}

func (f *tlFixture) state(t *testing.T, id string) map[string]any {
	t.Helper()
	w := f.do("support-sub", "GET", "/admin/template-updates/templates", nil)
	if w.Code != 200 {
		t.Fatalf("templates: %d %s", w.Code, w.Body)
	}
	var env struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	for _, it := range env.Data.Items {
		if it["id"] == id {
			return it
		}
	}
	t.Fatalf("import %s not listed: %s", id, w.Body)
	return nil
}

// runSmoke drives the runner until no smoke job is pending.
func (f *tlFixture) runSmoke(t *testing.T) {
	t.Helper()
	for i := 0; i < 5; i++ {
		did, err := f.runner.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !did {
			return
		}
	}
}

func (f *tlFixture) catalogDefault(t *testing.T, slug string) int {
	t.Helper()
	return catalogDefaultOn(t, f.client, slug)
}

func catalogDefaultOn(t *testing.T, c *pve.Client, slug string) int {
	t.Helper()
	found, err := c.Templates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range found {
		if d.Slug == slug {
			return d.VMID
		}
	}
	return 0
}

// ── tests ─────────────────────────────────────────────────────────────────────────────────────

// The whole lifecycle through the admin API on PostgreSQL + a fake PVE: staged → smoke (PASS) →
// publish (becomes the catalog default) → idempotent → withdraw (catalog falls back, digest
// blocklisted, publish/smoke/import refused) — with permissions, audit rows and ownership checked.
func TestTemplateLifecycleEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261111000000", "9")
	id := f.addImport(t, p, 9300)
	base := "/admin/template-updates/templates/" + id

	if st := f.state(t, id); st["state"] != templateupdates.StateStaged {
		t.Fatalf("fresh import: %v", st["state"])
	}
	if got := f.catalogDefault(t, "debian-13"); got != 9200 {
		t.Fatalf("a staged import changed the catalog: %d", got)
	}
	// Import with publish=true is refused (400) before anything else happens.
	if w := f.do("admin-sub", "POST", "/admin/template-updates/vm", map[string]any{"package": p.Package, "digest": p.Digest, "confirmDigest": p.Digest,
		"serviceId": "svc-pve", "node": "pve1", "importStorage": "nas-import", "diskStorage": "local-zfs", "publish": true}); w.Code != 400 || errCode(w) != "publish_at_import_removed" {
		t.Fatalf("publish at import: %d %s", w.Code, w.Body)
	}
	// Permissions: SUPPORT reads, never acts.
	for _, a := range []string{"/smoke", "/publish", "/withdraw"} {
		if w := f.do("support-sub", "POST", base+a, map[string]any{}); w.Code != 403 {
			t.Fatalf("support %s: %d", a, w.Code)
		}
	}
	// Publish refused without a smoke PASS.
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "smoke_missing" {
		t.Fatalf("publish without smoke: %d %s", w.Code, w.Body)
	}
	// Smoke: queued (idempotent while running), then run by the runner.
	w := f.do("admin-sub", "POST", base+"/smoke", nil)
	if w.Code != 200 {
		t.Fatalf("smoke: %d %s", w.Code, w.Body)
	}
	var sm struct {
		Data templateupdates.SmokeReceipt `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sm)
	if w2 := f.do("admin-sub", "POST", base+"/smoke", nil); w2.Code != 200 || !strings.Contains(w2.Body.String(), sm.Data.ID) {
		t.Fatalf("second smoke while queued: %d %s", w2.Code, w2.Body)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StateSmokeRunning {
		t.Fatalf("queued smoke state: %v", st["state"])
	}
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "smoke_running" {
		t.Fatalf("publish during smoke: %d %s", w.Code, w.Body)
	}
	f.runSmoke(t)
	got, _ := f.svc.Smokes.LoadSmoke(ctx, sm.Data.ID)
	if got.Result != templateupdates.SmokePass || !got.Cleaned || got.FinishedAt == nil || got.StartedAt == nil {
		t.Fatalf("smoke receipt: %+v", got)
	}
	if j, _ := f.jobs.Get(ctx, sm.Data.ID); j.State != "succeeded" {
		t.Fatalf("smoke job: %+v", j)
	}
	if left := f.pv.smokeVMs(); len(left) != 0 {
		t.Fatalf("smoke VM left: %v", left)
	}
	// The smoke never changed the template (only cloned FROM it), the pipeline template or the tenant VM.
	if m := f.pv.mutationsOf(9300); len(m) != 1 || m[0] != "clone 9300" {
		t.Fatalf("template mutated by the smoke test: %v", m)
	}
	for _, vmid := range []int{9200, 101} {
		if m := f.pv.mutationsOf(vmid); len(m) != 0 {
			t.Fatalf("VM %d mutated by the smoke test: %v", vmid, m)
		}
	}
	if st := f.state(t, id); st["state"] != templateupdates.StateReady {
		t.Fatalf("after PASS: %v", st)
	}
	// Publish: wrong confirmation refused, then published; it becomes the catalog default.
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": "sha256:" + strings.Repeat("0", 64)}); w.Code != 400 {
		t.Fatalf("publish unconfirmed: %d", w.Code)
	}
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if tags := f.pv.tags(9300); !strings.HasPrefix(tags, "proxcloud;pc-public;pc-img-debian-13;pc-v-202611110000") || strings.Contains(tags, "pc-tu-staged") {
		t.Fatalf("published tags: %s", tags)
	}
	if got := f.catalogDefault(t, "debian-13"); got != 9300 {
		t.Fatalf("published import is not the default: %d", got)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StatePublishedDefault {
		t.Fatalf("after publish: %v", st["state"])
	}
	// Idempotent.
	before := len(f.pv.mutationsOf(9300))
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 || !strings.Contains(w.Body.String(), `"unchanged":true`) {
		t.Fatalf("publish again: %d %s", w.Code, w.Body)
	}
	if len(f.pv.mutationsOf(9300)) != before {
		t.Fatal("an idempotent publish wrote to PVE")
	}
	// Withdraw: reason required, digest confirmation required.
	if w := f.do("admin-sub", "POST", base+"/withdraw", map[string]any{"confirmDigest": p.Digest}); w.Code != 400 || errCode(w) != "reason_required" {
		t.Fatalf("withdraw without reason: %d %s", w.Code, w.Body)
	}
	if w := f.do("admin-sub", "POST", base+"/withdraw", map[string]any{"reason": "boots without network on the lab", "confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	if got := f.catalogDefault(t, "debian-13"); got != 9200 {
		t.Fatalf("catalog did not fall back to the previous version: %d", got)
	}
	if tags := f.pv.tags(9300); strings.Contains(tags, "pc-public") || !strings.Contains(tags, "pc-tu-withdrawn") {
		t.Fatalf("withdrawn tags: %s", tags)
	}
	if _, ok := f.pv.vms[9300]; !ok {
		t.Fatal("a withdrawn template was deleted (must be kept for forensics)")
	}
	b, _ := f.svc.Receipts.Blocked(ctx, p.Digest)
	if b == nil || b.Reason != "boots without network on the lab" || b.Actor != "admin-sub" || b.At.IsZero() {
		t.Fatalf("blocklist: %+v", b)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StateWithdrawn {
		t.Fatalf("after withdraw: %v", st["state"])
	}
	// A blocklisted digest can never be published, smoke-tested or imported again.
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "digest_withdrawn" {
		t.Fatalf("publish withdrawn: %d %s", w.Code, w.Body)
	}
	if w := f.do("admin-sub", "POST", base+"/smoke", nil); w.Code != 409 || errCode(w) != "digest_withdrawn" {
		t.Fatalf("smoke withdrawn: %d %s", w.Code, w.Body)
	}
	if _, err := f.svc.EnqueueVM(ctx, templateupdates.VMRequest{Package: p.Package, Digest: p.Digest, ConfirmDigest: p.Digest, ServiceID: "svc-pve", Node: "pve1",
		ImportStorage: "nas-import", DiskStorage: "local-zfs"}, "admin-sub"); err != templateupdates.ErrBlocklisted {
		t.Fatalf("import of a withdrawn digest: %v", err)
	}
	if w := f.do("support-sub", "GET", "/admin/template-updates/blocklist", nil); w.Code != 200 || !strings.Contains(w.Body.String(), p.Digest) {
		t.Fatalf("blocklist read: %d %s", w.Code, w.Body)
	}
	// Withdraw again: idempotent, the first reason is kept.
	if w := f.do("admin-sub", "POST", base+"/withdraw", map[string]any{"reason": "a second, different reason", "confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("withdraw again: %d %s", w.Code, w.Body)
	}
	if b, _ := f.svc.Receipts.Blocked(ctx, p.Digest); b.Reason != "boots without network on the lab" {
		t.Fatalf("blocklist overwritten: %+v", b)
	}
	// The tenant VM and the pipeline template were never touched by any of it.
	if m := f.pv.mutationsOf(101); len(m) != 0 {
		t.Fatalf("tenant VM mutated: %v", m)
	}
	if m := f.pv.mutationsOf(9200); len(m) != 0 {
		t.Fatalf("pipeline template mutated: %v", m)
	}
	// Audit rows: smoke, publish (with digest) and withdraw (with reason) as successful admin events;
	// refused requests are not audited as successes.
	want := map[string]bool{"smoke": false, "publish": false, "withdraw": false}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		evs, err := f.audit.QueryAll(ctx, audit.Filter{ResourceType: "TEMPLATE_UPDATES"}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			var action, digest, reason string
			for _, c := range ev.Changes {
				switch c.Field {
				case "action":
					action, _ = c.NewValue.(string)
				case "digest":
					digest, _ = c.NewValue.(string)
				case "reason":
					reason, _ = c.NewValue.(string)
				}
			}
			if _, ok := want[action]; ok && digest == p.Digest && ev.Outcome == audit.OutcomeSuccess && ev.Actor != nil && ev.Actor.ID == "admin-sub" &&
				(action != "withdraw" || reason != "") {
				want[action] = true
			}
		}
		if want["smoke"] && want["publish"] && want["withdraw"] {
			break
		}
	}
	for a, ok := range want {
		if !ok {
			t.Errorf("no audit event for %s", a)
		}
	}
}

// Last-version guard: withdrawing the only published version of a line needs force + the slug.
func TestTemplateWithdrawLastVersionGuard(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-12", "20261111000000", "7")
	id := f.addImport(t, p, 9400)
	now := time.Now().UTC()
	sp := templateupdates.SmokePayload{Version: 1, ImportID: id, Package: p.Package, Tag: p.Tag, Digest: p.Digest, OS: p.OS, Variant: p.Variant,
		BuildVersion: p.BuildVersion, ServiceID: p.ServiceID, Node: p.Node, TemplateVMID: 9400, DiskStorage: p.DiskStorage, Network: templateupdates.SmokeNetwork{Bridge: "vmbr0"}}
	// A PASS of ANOTHER template VMID with the same digest does not unlock this one.
	other := sp
	other.TemplateVMID, other.ImportID = 9401, ""
	if err := f.svc.Smokes.SaveSmoke(ctx, &templateupdates.SmokeReceipt{ID: uuid.NewString(), Payload: other, CreatedAt: now, Step: templateupdates.SmokeFinished,
		Result: templateupdates.SmokePass, FinishedAt: &now, Cleaned: true}); err != nil {
		t.Fatal(err)
	}
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "smoke_missing" {
		t.Fatalf("publish on another VMID's PASS: %d %s", w.Code, w.Body)
	}
	// A recorded PASS for exactly this template + digest (and its current config), then publish.
	f.recordPass(t, id, p, 9400, f.pv)
	base := "/admin/template-updates/templates/" + id
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StatePublishedDefault || st["actions"].(map[string]any)["lastPublished"] != true {
		t.Fatalf("state %v", st)
	}
	body := map[string]any{"reason": "kernel regression on our hardware", "confirmDigest": p.Digest}
	if w := f.do("admin-sub", "POST", base+"/withdraw", body); w.Code != 409 || errCode(w) != "last_published_version" {
		t.Fatalf("withdraw last: %d %s", w.Code, w.Body)
	}
	if b, _ := f.svc.Receipts.Blocked(ctx, p.Digest); b != nil {
		t.Fatal("a refused withdraw blocklisted the digest")
	}
	body["force"], body["confirmLast"] = true, "debian-13"
	if w := f.do("admin-sub", "POST", base+"/withdraw", body); w.Code != 409 {
		t.Fatalf("force with the wrong slug: %d %s", w.Code, w.Body)
	}
	body["confirmLast"] = "debian-12"
	if w := f.do("admin-sub", "POST", base+"/withdraw", body); w.Code != 200 {
		t.Fatalf("forced withdraw: %d %s", w.Code, w.Body)
	}
	if got := f.catalogDefault(t, "debian-12"); got != 0 {
		t.Fatalf("debian-12 still in the catalog: %d", got)
	}
}

// Smoke FAIL: recorded with its code, the test VM removed, publish refused; a new PASS unlocks it.
func TestTemplateSmokeFailBlocksPublish(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261112000000", "6")
	id := f.addImport(t, p, 9500)
	base := "/admin/template-updates/templates/" + id
	f.pv.ciStatus = "error"
	if w := f.do("admin-sub", "POST", base+"/smoke", nil); w.Code != 200 {
		t.Fatalf("smoke: %d %s", w.Code, w.Body)
	}
	f.runSmoke(t)
	st := f.state(t, id)
	smoke, _ := st["smoke"].(map[string]any)
	if st["state"] != templateupdates.StateSmokeFailed || smoke["code"] != templateupdates.SmokeCloudInitFailed || smoke["result"] != "FAIL" {
		t.Fatalf("state %v", st)
	}
	if left := f.pv.smokeVMs(); len(left) != 0 {
		t.Fatalf("smoke VM left after FAIL: %v", left)
	}
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "smoke_not_passed" {
		t.Fatalf("publish after FAIL: %d %s", w.Code, w.Body)
	}
	f.pv.ciStatus = "done"
	if w := f.do("admin-sub", "POST", base+"/smoke", nil); w.Code != 200 {
		t.Fatalf("second smoke: %d %s", w.Code, w.Body)
	}
	f.runSmoke(t)
	if w := f.do("admin-sub", "POST", base+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("publish after a new PASS: %d %s", w.Code, w.Body)
	}
	all, _ := f.svc.Smokes.ForTemplate(ctx, "svc-pve", "pve1", 9500, p.Digest)
	if len(all) != 2 || all[0].Result != "PASS" || all[1].Result != "FAIL" {
		t.Fatalf("smoke history %+v", all)
	}
}

// Dead-job cleanup covers smoke VMs: a smoke job that died with its test VM up is cleaned by the
// runner's dead pass (fenced ClaimDead), only its own VM goes, and the receipt says FAIL job_dead.
func TestTemplateSmokeDeadJobCleanup(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261113000000", "5")
	id := f.addImport(t, p, 9600)
	sm, err := f.svc.EnqueueSmoke(ctx, id, templateupdates.SmokeNetwork{Bridge: "vmbr0"}, "admin-sub", false)
	if err != nil {
		t.Fatal(err)
	}
	// It "ran" and died: its test VM is up; a same-named VM in a tenant pool (a copy) must survive.
	f.pv.mu.Lock()
	f.pv.vms[9601] = &fakeVMRow{node: "pve1", running: true, cfg: map[string]string{"name": templateupdates.SmokeVMName(sm.ID), "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-tu-smoke;" + templateupdates.SmokeOwnerTag(sm.ID)}}
	f.pv.vms[9602] = &fakeVMRow{node: "pve1", running: true, cfg: map[string]string{"name": templateupdates.SmokeVMName(sm.ID), "pool": "pc-tenant1",
		"tags": "proxcloud;pc-tu-smoke;" + templateupdates.SmokeOwnerTag(sm.ID)}}
	f.pv.mu.Unlock()
	sm.VMID, sm.Step = 9601, templateupdates.SmokeBooting
	if err := f.svc.Smokes.SaveSmoke(ctx, sm); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx, `UPDATE durable_job SET state='dead',attempts=max_attempts,finished_at=clock_timestamp(),last_error='lease_expired' WHERE id=$1`, sm.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := f.svc.Smokes.DeadUncleaned(ctx); len(ids) != 1 || ids[0] != sm.ID {
		t.Fatalf("dead uncleaned: %v", ids)
	}
	if did, err := f.runner.RunOnce(ctx); !did || err != nil {
		t.Fatalf("dead pass: %v %v", did, err)
	}
	if _, ok := f.pv.vms[9601]; ok {
		t.Fatal("dead smoke job's test VM survived")
	}
	if _, ok := f.pv.vms[9602]; !ok {
		t.Fatal("a VM outside pc-templates was destroyed")
	}
	got, _ := f.svc.Smokes.LoadSmoke(ctx, sm.ID)
	j, _ := f.jobs.Get(ctx, sm.ID)
	if !got.Cleaned || got.Result != templateupdates.SmokeFail || got.Code != templateupdates.SmokeJobDead || j.LastError != "cleaned_up" {
		t.Fatalf("receipt %+v job %+v", got, j)
	}
	if ids, _ := f.svc.Smokes.DeadUncleaned(ctx); len(ids) != 0 {
		t.Fatalf("still listed: %v", ids)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StateSmokeFailed {
		t.Fatalf("state %v", st["state"])
	}
}

// With PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED off, no lifecycle action reaches PVE.
func TestTemplateLifecycleFeatureFlagOff(t *testing.T) {
	f := newTLFixture(t, false)
	p := tlPayload("debian-13", "20261111000000", "4")
	id := f.addImport(t, p, 9700)
	base := "/admin/template-updates/templates/" + id
	for _, c := range []struct {
		path string
		body any
	}{{"/smoke", nil}, {"/publish", map[string]any{"confirmDigest": p.Digest}}, {"/withdraw", map[string]any{"reason": "testing the flag", "confirmDigest": p.Digest}}} {
		if w := f.do("admin-sub", "POST", base+c.path, c.body); w.Code != 409 || errCode(w) != "vm_updates_disabled" {
			t.Fatalf("%s with the flag off: %d %s", c.path, w.Code, w.Body)
		}
	}
	f.pv.mu.Lock()
	muts := append([]string(nil), f.pv.mutations...)
	f.pv.mu.Unlock()
	if len(muts) != 0 {
		t.Fatalf("PVE mutated with the flag off: %v", muts)
	}
	if acts := f.state(t, id)["actions"].(map[string]any); acts["smoke"] == true || acts["withdraw"] == true {
		t.Fatalf("actions offered with the flag off: %v", acts)
	}
}

// The automatic smoke after an import: queued on the provider's smoke network; without one the
// import shows why no smoke ran (smoke_network_unset) and nothing reaches PVE.
func TestTemplateAutoSmoke(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261114000000", "3")
	id := f.addImport(t, p, 9800)
	err := f.svc.AutoSmoke(ctx, id, func(context.Context, string, string) (templateupdates.SmokeNetwork, error) {
		return templateupdates.SmokeNetwork{}, nil
	})
	if err == nil {
		t.Fatal("auto smoke without a network succeeded")
	}
	st := f.state(t, id)
	if sm, _ := st["smoke"].(map[string]any); sm["code"] != "smoke_network_unset" || st["state"] != templateupdates.StateStaged {
		t.Fatalf("state %v", st)
	}
	if err := f.svc.AutoSmoke(ctx, id, func(context.Context, string, string) (templateupdates.SmokeNetwork, error) {
		return templateupdates.SmokeNetwork{Bridge: "vmbr0"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := f.state(t, id); st["state"] != templateupdates.StateSmokeRunning {
		t.Fatalf("after auto smoke: %v", st)
	}
	f.runSmoke(t)
	if st := f.state(t, id); st["state"] != templateupdates.StateReady {
		t.Fatalf("after the run: %v", st)
	}
	if left := f.pv.smokeVMs(); len(left) != 0 {
		t.Fatalf("smoke VM left: %v", left)
	}
}

// B1: the same digest imported on two providers. Withdraw takes every copy on BOTH out of the
// catalog; with one provider failing, the result is incomplete, that copy shows
// "withdraw_incomplete" with Withdraw still enabled, and the page action finishes it.
func TestTemplateWithdrawTwoProvidersPartialThenRetry(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p1 := tlPayload("debian-13", "20261115000000", "2")
	p2 := p1
	p2.ServiceID = "svc-pve2"
	id1 := f.addImport(t, p1, 9300)
	id2 := f.addImportOn(t, f.pv2, p2, 9310)
	// Both published (a PASS each, recorded as the smoke job would leave it).
	for _, c := range []struct {
		id   string
		p    templateupdates.VMPayload
		vmid int
		pv   *fakePVEAPI
	}{{id1, p1, 9300, f.pv}, {id2, p2, 9310, f.pv2}} {
		f.recordPass(t, c.id, c.p, c.vmid, c.pv)
		if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+c.id+"/publish", map[string]any{"confirmDigest": p1.Digest}); w.Code != 200 {
			t.Fatalf("publish %s: %d %s", c.p.ServiceID, w.Code, w.Body)
		}
	}
	if catalogDefaultOn(t, f.client, "debian-13") != 9300 || catalogDefaultOn(t, f.client2, "debian-13") != 9310 {
		t.Fatal("not published on both providers")
	}
	// Provider 2's tag writes fail: the withdraw (started from provider 1's row) blocklists the digest,
	// withdraws provider 1 and reports provider 2 incomplete.
	f.pv2.mu.Lock()
	f.pv2.failTagWrites = true
	f.pv2.mu.Unlock()
	body := map[string]any{"reason": "bad kernel on both clusters", "confirmDigest": p1.Digest}
	w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id1+"/withdraw", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatalf("partial withdraw: %d %s", w.Code, w.Body)
	}
	var res struct {
		Data templateupdates.LifecycleResult `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	statuses := map[string]string{}
	for _, po := range res.Data.Providers {
		statuses[po.ServiceID] = po.Status
	}
	if statuses["svc-pve"] != "withdrawn" || statuses["svc-pve2"] != "incomplete" {
		t.Fatalf("per-provider outcome: %+v", res.Data.Providers)
	}
	if catalogDefaultOn(t, f.client, "debian-13") != 9200 {
		t.Fatal("provider 1 still serves the withdrawn version")
	}
	if catalogDefaultOn(t, f.client2, "debian-13") != 9310 {
		t.Fatal("test precondition: provider 2's write was meant to fail")
	}
	st1, st2 := f.state(t, id1), f.state(t, id2)
	if st1["state"] != templateupdates.StateWithdrawn || st1["actions"].(map[string]any)["withdraw"] == true {
		t.Fatalf("provider 1 row: %v", st1)
	}
	if st2["state"] != templateupdates.StateWithdrawIncomplete || st2["actions"].(map[string]any)["withdraw"] != true ||
		st2["actions"].(map[string]any)["publish"] == true || st2["actions"].(map[string]any)["smoke"] == true {
		t.Fatalf("provider 2 row: %v", st2)
	}
	// A blocklisted digest is never re-published, even on the provider whose copy is still public.
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id2+"/publish", map[string]any{"confirmDigest": p1.Digest}); w.Code != 409 {
		t.Fatalf("publish of a blocklisted digest: %d", w.Code)
	}
	// Retry from the page action on the incomplete row: it finishes provider 2.
	f.pv2.mu.Lock()
	f.pv2.failTagWrites = false
	f.pv2.mu.Unlock()
	w = f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id2+"/withdraw", map[string]any{"reason": "finish the withdraw", "confirmDigest": p1.Digest})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"complete":true`) {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	if catalogDefaultOn(t, f.client2, "debian-13") != 9200 {
		t.Fatal("provider 2 still serves the withdrawn version after the retry")
	}
	if st := f.state(t, id2); st["state"] != templateupdates.StateWithdrawn || st["actions"].(map[string]any)["withdraw"] == true {
		t.Fatalf("provider 2 row after retry: %v", st)
	}
	if b, _ := f.svc.Receipts.Blocked(ctx, p1.Digest); b == nil || b.Reason != "bad kernel on both clusters" {
		t.Fatalf("blocklist entry: %+v", b)
	}
	// A provider whose cluster is unreachable: nothing changes there, the withdraw is incomplete.
	p3 := tlPayload("debian-13", "20261116000000", "1")
	p4 := p3
	p4.ServiceID = "svc-pve2"
	id3 := f.addImport(t, p3, 9320)
	f.addImportOn(t, f.pv2, p4, 9330)
	f.pv2.mu.Lock()
	f.pv2.down = true
	f.pv2.mu.Unlock()
	w = f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id3+"/withdraw", map[string]any{"reason": "unreachable provider case", "confirmDigest": p3.Digest})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unreachable"`) || !strings.Contains(w.Body.String(), `"complete":false`) {
		t.Fatalf("withdraw with an unreachable provider: %d %s", w.Code, w.Body)
	}
	// A provider removed from the portal is provider_missing, not "unreachable": nothing can ever be
	// changed there, so the withdraw completes without it (and the row's Withdraw is not left on forever).
	f.pv2.mu.Lock()
	f.pv2.down = false
	f.pv2.mu.Unlock()
	p5 := tlPayload("debian-13", "20261117000000", "1")
	p6 := p5
	p6.ServiceID = "svc-pve2"
	id5 := f.addImport(t, p5, 9340)
	f.addImportOn(t, f.pv2, p6, 9350)
	if _, err := f.db.C("externalService").DeleteOne(ctx, pgdoc.M{"_id": "svc-pve2"}); err != nil {
		t.Fatal(err)
	}
	w = f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id5+"/withdraw", map[string]any{"reason": "deleted provider case", "confirmDigest": p5.Digest})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"provider_missing"`) || strings.Contains(w.Body.String(), `"unreachable"`) ||
		!strings.Contains(w.Body.String(), `"complete":true`) {
		t.Fatalf("withdraw with a deleted provider: %d %s", w.Code, w.Body)
	}
}

// The last-version guard is evaluated on every provider: withdrawing from provider 1 is refused when
// provider 2 would lose its only published version, even though provider 1 keeps an older one.
func TestTemplateWithdrawGuardAcrossProviders(t *testing.T) {
	f := newTLFixture(t, true)
	p1 := tlPayload("debian-12", "20261115000000", "b")
	p2 := p1
	p2.ServiceID = "svc-pve2"
	id1 := f.addImport(t, p1, 9300)
	id2 := f.addImportOn(t, f.pv2, p2, 9310)
	f.pv.mu.Lock()
	f.pv.vms[9201] = &fakeVMRow{node: "pve1", template: true, cfg: map[string]string{"name": "pc-debian-12-202611010000", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-public;pc-img-debian-12;pc-v-202611010000", "scsi0": "x,size=3G"}}
	f.pv.mu.Unlock()
	f.recordPass(t, id1, p1, 9300, f.pv)
	f.recordPass(t, id2, p2, 9310, f.pv2)
	for _, id := range []string{id1, id2} {
		if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/publish", map[string]any{"confirmDigest": p1.Digest}); w.Code != 200 {
			t.Fatalf("publish: %d %s", w.Code, w.Body)
		}
	}
	if acts := f.state(t, id1)["actions"].(map[string]any); acts["lastPublished"] != true {
		t.Fatalf("row of provider 1 does not show provider 2's last-version consequence: %v", acts)
	}
	body := map[string]any{"reason": "regression found in testing", "confirmDigest": p1.Digest}
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id1+"/withdraw", body); w.Code != 409 || errCode(w) != "last_published_version" || !strings.Contains(w.Body.String(), "svc-pve2") {
		t.Fatalf("guard across providers: %d %s", w.Code, w.Body)
	}
	body["force"], body["confirmLast"] = true, "debian-12"
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id1+"/withdraw", body); w.Code != 200 {
		t.Fatalf("forced: %d %s", w.Code, w.Body)
	}
	if catalogDefaultOn(t, f.client, "debian-12") != 9201 || catalogDefaultOn(t, f.client2, "debian-12") != 0 {
		t.Fatal("catalogs after the forced withdraw")
	}
}

// A node left without a published version of the line needs confirmNodeLoss.
func TestTemplateWithdrawNodeLossConfirmation(t *testing.T) {
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261117000000", "c")
	pB := p
	pB.Node = "pve2"
	id := f.addImport(t, p, 9300)
	idB := f.addImport(t, pB, 9301) // the same artifact imported on node pve2 (no other version there)
	f.recordPass(t, id, p, 9300, f.pv)
	f.recordPass(t, idB, pB, 9301, f.pv)
	for _, i := range []string{id, idB} {
		if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+i+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
			t.Fatalf("publish: %d %s", w.Code, w.Body)
		}
	}
	if acts := f.state(t, id)["actions"].(map[string]any); fmt.Sprint(acts["nodesLosingLast"]) != "[svc-pve/pve2]" {
		t.Fatalf("actions: %v", acts)
	}
	body := map[string]any{"reason": "node loss confirmation", "confirmDigest": p.Digest}
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/withdraw", body); w.Code != 409 || errCode(w) != "nodes_losing_last_copy" {
		t.Fatalf("node loss unconfirmed: %d %s", w.Code, w.Body)
	}
	body["confirmNodeLoss"] = true
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/withdraw", body); w.Code != 200 || !strings.Contains(w.Body.String(), `"complete":true`) {
		t.Fatalf("confirmed: %d %s", w.Code, w.Body)
	}
	for _, vmid := range []int{9300, 9301} {
		if tags := f.pv.tags(vmid); !strings.Contains(tags, "pc-tu-withdrawn") || strings.Contains(tags, "pc-public") {
			t.Fatalf("VM %d: %s", vmid, tags)
		}
	}
}

// S6: any config change of the template after its smoke test invalidates the PASS.
func TestTemplatePublishRequiresUnchangedTemplate(t *testing.T) {
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261118000000", "d")
	id := f.addImport(t, p, 9300)
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/smoke", nil); w.Code != 200 {
		t.Fatalf("smoke: %d %s", w.Code, w.Body)
	}
	f.runSmoke(t)
	f.pv.mu.Lock()
	f.pv.vms[9300].cfg["memory"] = "8192" // someone edits the template after its PASS
	f.pv.vms[9300].digest++
	f.pv.mu.Unlock()
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 409 || errCode(w) != "template_changed_since_smoke" {
		t.Fatalf("publish after a change: %d %s", w.Code, w.Body)
	}
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/smoke", nil); w.Code != 200 {
		t.Fatalf("second smoke: %d %s", w.Code, w.Body)
	}
	f.runSmoke(t)
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("publish after a fresh PASS: %d %s", w.Code, w.Body)
	}
}

// Smoke on an untagged management bridge is refused before anything is queued.
func TestTemplateSmokeRefusesManagementBridge(t *testing.T) {
	ctx := context.Background()
	f := newTLFixture(t, true)
	p := tlPayload("debian-13", "20261119000000", "e")
	id := f.addImport(t, p, 9300)
	if _, err := f.db.C("externalService").SetByID(ctx, "svc-pve", pgdoc.M{"config.templateSmoke": pgdoc.M{"bridge": "vmbr0"}}, nil); err != nil {
		t.Fatal(err)
	}
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/smoke", nil); w.Code != 409 || errCode(w) != "smoke_network_management_bridge" {
		t.Fatalf("management bridge: %d %s", w.Code, w.Body)
	}
	if all, _ := f.svc.Smokes.ForTemplate(ctx, "svc-pve", "pve1", 9300, p.Digest); len(all) != 0 {
		t.Fatalf("a smoke was queued: %+v", all)
	}
}

// recordPass stores a PASS for exactly that template as the smoke job would (with its config digest).
func (f *tlFixture) recordPass(t *testing.T, importID string, p templateupdates.VMPayload, vmid int, pv *fakePVEAPI) {
	t.Helper()
	pv.mu.Lock()
	digest := strconv.Itoa(pv.vms[vmid].digest)
	pv.mu.Unlock()
	now := time.Now().UTC()
	sp := templateupdates.SmokePayload{Version: 1, ImportID: importID, Package: p.Package, Tag: p.Tag, Digest: p.Digest, OS: p.OS, Variant: p.Variant,
		BuildVersion: p.BuildVersion, ServiceID: p.ServiceID, Node: p.Node, TemplateVMID: vmid, DiskStorage: p.DiskStorage, Network: templateupdates.SmokeNetwork{Bridge: "vmbr9"}}
	if err := f.svc.Smokes.SaveSmoke(context.Background(), &templateupdates.SmokeReceipt{ID: uuid.NewString(), ImportID: importID, Payload: sp, CreatedAt: now,
		Step: templateupdates.SmokeFinished, Result: templateupdates.SmokePass, FinishedAt: &now, Cleaned: true, TemplateConfigDigest: digest}); err != nil {
		t.Fatal(err)
	}
}

// S4: withdrawing a Kubernetes node image warns that the kamaji version map names VMIDs explicitly
// and gives the replacement config.cluster.versions line (here: the pipeline template left).
func TestTemplateWithdrawK8sGivesVersionsLine(t *testing.T) {
	f := newTLFixture(t, true)
	p := templateupdates.VMPayload{Version: 1, Package: "proxcloud-vm-ubuntu-2604-k8s", Tag: "ubuntu-2604-k8s-1.35.10-20261112010203-cccccccccccc",
		Digest: "sha256:" + strings.Repeat("f", 64), DiskDigest: "sha256:" + strings.Repeat("e", 64), DiskBytes: 4096, VirtualSizeBytes: 20 << 30,
		OS: "ubuntu-2604", Variant: "kubernetes", KubernetesVersion: "1.35.10", BuildVersion: "20261112010203", ServiceID: "svc-pve", Node: "pve1",
		ImportStorage: "nas-import", DiskStorage: "local-zfs"}
	id := f.addImport(t, p, 9500)
	f.pv.mu.Lock()
	f.pv.vms[9210] = &fakeVMRow{node: "pve1", template: true, cfg: map[string]string{"name": "ubuntu-2604-kube-v1.35.10", "pool": pve.TemplatePool,
		"tags": "proxcloud;pc-k8s;pc-k8s-1-35-10;pc-k8s-os-ubuntu-2604", "scsi0": "x,size=20G"}}
	f.pv.mu.Unlock()
	f.recordPass(t, id, p, 9500, f.pv)
	if w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/publish", map[string]any{"confirmDigest": p.Digest}); w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body)
	}
	if tags := f.pv.tags(9500); strings.Contains(tags, "pc-public") || !strings.Contains(tags, "pc-tu-k8s-public") {
		t.Fatalf("k8s publish tags: %s", tags)
	}
	if catalogDefaultOn(t, f.client, "ubuntu-2604") != 0 {
		t.Fatal("a node image entered the tenant catalog")
	}
	if st := f.state(t, id); st["versionsMapLine"] != `"1.35.10": "pve1=9500"` {
		t.Fatalf("versions line while published: %v", st["versionsMapLine"])
	}
	w := f.do("admin-sub", "POST", "/admin/template-updates/templates/"+id+"/withdraw", map[string]any{"reason": "kubelet crash loop on boot", "confirmDigest": p.Digest})
	if w.Code != 200 {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body)
	}
	var res struct {
		Data templateupdates.LifecycleResult `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Data.Providers) != 1 || res.Data.Providers[0].VersionsMapLine != `"1.35.10": "pve1=9210"` ||
		!strings.Contains(strings.Join(res.Data.Warnings, " "), "names template VMIDs explicitly") {
		t.Fatalf("k8s withdraw result: %+v", res.Data)
	}
}
