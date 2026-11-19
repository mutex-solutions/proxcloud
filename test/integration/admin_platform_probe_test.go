//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestPlatformTenantPathProbeRoutes drives the two tenant path test routes against PostgreSQL and a fake Proxmox: the
// role each needs (the start writes to Proxmox, so service:manage; the read service:read), each refusal as a 409 with
// its code and nothing written, the 202 of a start, how the probe ends, and that a probe cut off by an API restart
// reads as an error. The platform's load balancer range is on loopback, where the test can listen.
func TestPlatformTenantPathProbeRoutes(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	var writes []string
	// The template at VM id 9200 on pve1, as Template updates names and tags a published import of that digest.
	const digest = "sha256:0123456789abcdef0123456789abcdef"
	tplName, tplTags := "pc-ghcr-ubuntu-2604-os-202611161812", "pc-img-ubuntu-2604;pc-public;pc-v-202611161812;pc-tu-import;pc-src-0123456789ab;proxcloud"
	fakePVE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api2/json")
		switch {
		case r.Method != http.MethodGet:
			writes = append(writes, r.Method+" "+path)
		case path == "/cluster/sdn/zones":
			fmt.Fprint(w, `{"data":[]}`)
			return
		case path == "/cluster/resources":
			fmt.Fprintf(w, `{"data":[{"type":"qemu","node":"pve1","vmid":9200,"name":%q,"template":1,"pool":%q,"tags":%q}]}`, tplName, pve.TemplatePool, tplTags)
			return
		}
		// Everything else fails: a started probe ends as an error at its first step, having made nothing.
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":{"fake":"nothing here"}}`)
	}))
	t.Cleanup(fakePVE.Close)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform",
		"network": pgdoc.M{"cidrPool": "10.100.0.0/16", "defaultZone": "pcm0", "zones": pgdoc.M{"pcm0": pgdoc.M{"kind": "simple", "dhcp": false}}}},
		"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	jobs := durablejobs.New(db)
	if err := jobs.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	var in platformlife.Installation
	if err := json.Unmarshal([]byte(strings.ReplaceAll(platformBody("10.200.200.1", 2, 0), "10.200.200.", "127.0.0.")), &in); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &in); err != nil {
		t.Fatal(err)
	}
	// What a tenant cluster's API would be: a listener in the load balancer range. When the port is taken on every
	// address of this machine, whatever holds it answers the dial as well.
	if l, err := net.Listen("tcp", "127.0.0.200:6443"); err == nil {
		t.Cleanup(func() { l.Close() })
	}

	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	// Template updates with no import yet: the receipts say which templates are this portal's.
	receipts := templateupdates.PGReceipts{DB: db}
	if err := receipts.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	h.SetTemplateUpdates(&templateupdates.Service{Receipts: receipts, DB: db})
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)
	path := "/admin/platform/installations/" + in.ID + "/tenant-path-probe"
	call := func(sub, method, body string, want int, code string) platformlife.TenantProbe {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var a struct {
			Data   platformlife.TenantProbe `json:"data"`
			Errors struct {
				Details map[string]any `json:"details"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &a)
		if got, _ := a.Errors.Details["code"].(string); w.Code != want || got != code {
			t.Fatalf("%s %s: %d %q %s, want %d %q", sub, method, w.Code, got, w.Body.String(), want, code)
		}
		return a.Data
	}

	// The roles: SUPPORT reads and does not start; nothing was tested yet.
	call("support-sub", http.MethodPost, `{}`, http.StatusForbidden, "")
	call("support-sub", http.MethodGet, "", http.StatusNotFound, "no_probe")
	// The refusals, each before anything is written to Proxmox.
	call("admin-sub", http.MethodPost, `{}`, http.StatusConflict, "not_installed")
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, in.ID, in.Rev, func(old platformlife.Installation) (*platformlife.Installation, error) {
		old.Status.BOM = &platformlife.BOMRef{Release: cur.Release, SHA256: cur.SHA256}
		return &old, nil
	}); err != nil {
		t.Fatal(err)
	}
	call("admin-sub", http.MethodPost, `{"node":"pve9"}`, http.StatusConflict, "invalid_node")
	// The one published Ubuntu template on pve1 is in the pool, but nothing says this portal imported it: it may be
	// another party's on a shared cluster, and is not cloned. With the receipt of this provider's finished import of
	// exactly that template it is this provider's.
	call("admin-sub", http.MethodPost, `{"node":"pve1"}`, http.StatusConflict, "no_os_template")
	now := time.Now().UTC()
	receipt := func(change func(*templateupdates.Receipt)) {
		t.Helper()
		rec := &templateupdates.Receipt{ID: "import-1", Kind: "vm", VMID: 9200, CreatedAt: now, UpdatedAt: now, Step: templateupdates.StepSucceeded,
			Payload: templateupdates.VMPayload{ServiceID: in.ServiceID, Node: "pve1", Digest: digest, OS: "ubuntu-2604", Variant: "os", BuildVersion: "202611161812"}}
		if change != nil {
			change(rec)
		}
		if err := receipts.Save(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	// A receipt that names the VM id proves nothing by itself: Proxmox gives the id of a failed import, or of a
	// template deleted by hand, to the next VM, which on a shared cluster can be another portal's template.
	for name, change := range map[string]func(*templateupdates.Receipt){
		"an import that failed after it took the id":  func(r *templateupdates.Receipt) { r.Step = templateupdates.StepFailed },
		"an import that has no template yet":          func(r *templateupdates.Receipt) { r.Step = templateupdates.StepCreating },
		"another provider's import":                   func(r *templateupdates.Receipt) { r.Payload.ServiceID = "svc-other" },
		"an import whose template was deleted":        func(r *templateupdates.Receipt) { r.TemplateGone = true },
		"an import on another node":                   func(r *templateupdates.Receipt) { r.Payload.Node = "pve2" },
		"the id reused by a template of another name": func(r *templateupdates.Receipt) { r.Payload.BuildVersion = "202610010000" },
		"the id reused by another artifact":           func(r *templateupdates.Receipt) { r.Payload.Digest = "sha256:ffffffffffffffffffffffffffffffff" },
	} {
		receipt(change)
		if w := len(writes); w != 0 {
			t.Fatalf("%s: wrote to Proxmox: %v", name, writes)
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"node":"pve1"}`))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_os_template") || !strings.Contains(w.Body.String(), "ubuntu-2604 (VM 9200)") {
			t.Errorf("%s counted as this portal's template: %d %s", name, w.Code, w.Body.String())
		}
	}
	receipt(nil)
	if len(writes) != 0 {
		t.Fatalf("a refused start wrote to Proxmox: %v", writes)
	}

	// A start: 202 with the probe as running; it then ends by itself.
	if p := call("admin-sub", http.MethodPost, `{"node":"pve1"}`, http.StatusAccepted, ""); p.State != platformlife.ProbeRunning || p.Node != "pve1" || p.Image != "ubuntu-2604" || p.Template != 9200 || p.RequestedBy == "" {
		t.Fatalf("the started probe: %+v", p)
	}
	var p platformlife.TenantProbe
	for range 100 {
		if p = call("support-sub", http.MethodGet, "", http.StatusOK, ""); p.State != platformlife.ProbeRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p.State != platformlife.ProbeError || !strings.Contains(p.Message, "nothing is known about the path") {
		t.Fatalf("the probe's end: %+v", p)
	}

	// A probe cut off by an API restart is stored as running for good: a reader gets an error, and a new start is taken.
	if err := store.SaveProbe(ctx, &platformlife.TenantProbe{ID: in.ID, State: platformlife.ProbeRunning, Node: "pve1", StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if p := call("support-sub", http.MethodGet, "", http.StatusOK, ""); p.State != platformlife.ProbeError || len(p.Left) != 1 || !strings.Contains(p.Message, "restarted while it ran") {
		t.Fatalf("a cut off probe: %+v", p)
	}
	// One that still runs refuses a second start.
	if err := store.SaveProbe(ctx, &platformlife.TenantProbe{ID: in.ID, State: platformlife.ProbeRunning, Node: "pve1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	call("admin-sub", http.MethodPost, `{}`, http.StatusConflict, "probe_running")

	// During a run of the installation nothing else writes to its Proxmox.
	if _, err := store.StartRun(ctx, jobs, in.ID, platformlife.RunApply, "admin-sub", "", "", platformlife.SeedBackup{}); err != nil {
		t.Fatal(err)
	}
	call("admin-sub", http.MethodPost, `{}`, http.StatusConflict, "run_active")
}
