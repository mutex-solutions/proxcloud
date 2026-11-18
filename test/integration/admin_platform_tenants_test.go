//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestPlatformOfferKubernetesVersion drives POST …/kubernetes-version against PostgreSQL and a fake Proxmox: SUPPORT
// is refused; an installation without a provider answers 409 no_provider; while the node image is only staged the
// answer is 409 node_image_unpublished with the node and the provider row is untouched; once it is published the
// BOM's version is written with its line and audited; a second call and a version the operator set change nothing.
func TestPlatformOfferKubernetesVersion(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	cur, err := platformlife.Current()
	if err != nil {
		t.Fatal(err)
	}
	version := cur.Kubernetes.Version
	tag, _ := templateupdates.ParseTag(cur.Kubernetes.NodeImage.Tag)
	tags := templateupdates.TagImport + ";" + templateupdates.TagStaged
	fakePVE := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || strings.TrimPrefix(r.URL.Path, "/api2/json") != "/cluster/resources" {
			t.Errorf("unexpected Proxmox call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"data":[{"type":"qemu","node":"pve1","vmid":9212,"name":"pc-ghcr-%s-k8s","template":1,"pool":%q,"tags":%q}]}`, tag.OS, pve.TemplatePool, tags)
	}))
	t.Cleanup(fakePVE.Close)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": fakePVE.URL, "tokenId": "proxcloud@pve!platform"},
		"secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-k8s", "name": "lab Kubernetes", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "kamaji", "region": "lab", "cluster": pgdoc.M{"versions": pgdoc.M{"1.34.2": "pve1=9300"}, "rootVolumeGiB": 30}}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "support-sub", "role": "SUPPORT"})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	var in platformlife.Installation
	if err := json.Unmarshal([]byte(platformBody("10.200.200.1", 2, 0)), &in); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &in); err != nil {
		t.Fatal(err)
	}
	auditRepo := audit.NewRepo(db)
	repo := admin.NewRepo(db)
	h := admin.NewHandler(repo, nil, nil, nil, nil, nil, nil, nil, audit.NewService(auditRepo, slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	h.SetProxmox(func(es *externalservice.ExternalService) (*pve.Service, error) {
		c, err := pve.New(es.PVEConfig())
		return &pve.Service{C: c, ServiceID: es.ID}, err
	})
	router := chi.NewRouter()
	h.Routes(router)
	call := func(sub string, want int, code string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/platform/installations/"+in.ID+"/kubernetes-version", strings.NewReader(`{}`))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: sub}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var a struct {
			Data   map[string]any `json:"data"`
			Errors struct {
				Details map[string]any `json:"details"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &a)
		if got, _ := a.Errors.Details["code"].(string); w.Code != want || got != code {
			t.Fatalf("%s: %d %q %s, want %d %q", sub, w.Code, got, w.Body.String(), want, code)
		}
		if a.Data == nil {
			return a.Errors.Details
		}
		return a.Data
	}
	versions := func() map[string]any {
		t.Helper()
		row, err := repo.FindDoc(ctx, "externalService", "svc-k8s")
		if err != nil {
			t.Fatal(err)
		}
		return row["config"].(map[string]any)["cluster"].(map[string]any)["versions"].(map[string]any)
	}

	call("support-sub", http.StatusForbidden, "")
	call("admin-sub", http.StatusConflict, "no_provider")
	if _, _, err := store.Update(ctx, in.ID, in.Rev, func(old platformlife.Installation) (*platformlife.Installation, error) {
		old.Status.BOM = &platformlife.BOMRef{Release: cur.Release, SHA256: cur.SHA256}
		old.Status.Providers = &platformlife.ProviderIDs{Kubernetes: "svc-k8s"}
		return &old, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Staged: nothing is offered, and the answer names the node.
	if d := call("admin-sub", http.StatusConflict, "node_image_unpublished"); fmt.Sprint(d["missingNodes"]) != "[pve1]" || d["version"] != version {
		t.Fatalf("staged: %v", d)
	}
	if v := versions(); len(v) != 1 {
		t.Fatalf("a refused offer changed the provider: %v", v)
	}

	// Published: the version is added beside the operator's own.
	tags = strings.Join([]string{pve.TagManaged, templateupdates.TagK8sPublished, templateupdates.TagK8sImport,
		templateupdates.TagK8sVersion + strings.ReplaceAll(version, ".", "-"), pve.TagTemplateVersion + tag.BuildVersion[:12]}, ";")
	if d := call("admin-sub", http.StatusOK, ""); d["offered"] != true || d["line"] != "pve1=9212" || d["version"] != version {
		t.Fatalf("published: %v", d)
	}
	if v := versions(); v[version] != "pve1=9212" || v["1.34.2"] != "pve1=9300" {
		t.Fatalf("versions after the offer: %v", v)
	}
	// Again, and against a line the operator wrote for that version: nothing changes.
	row, _ := repo.FindDoc(ctx, "externalService", "svc-k8s")
	row["config"].(map[string]any)["cluster"].(map[string]any)["versions"].(map[string]any)[version] = "pve1=9999"
	if err := repo.ReplaceDoc(ctx, "externalService", "svc-k8s", row); err != nil {
		t.Fatal(err)
	}
	if d := call("admin-sub", http.StatusOK, ""); d["offered"] != false {
		t.Fatalf("a second offer: %v", d)
	}
	if v := versions(); v[version] != "pve1=9999" {
		t.Fatalf("the operator's line was overwritten: %v", v)
	}
}

// TestPlatformAttachChoice runs what a run's register stage calls for spec.tenants.attach against PostgreSQL: "new"
// turns the attach policy on and attaches nobody; "all" also starts one bulk attach of every project, which attaches
// the ones that lack the provider; a second "all" finds none left and starts no run; "none" turns the policy off and
// detaches nobody; a disabled provider takes the policy and reports that the projects wait. Each change is audited
// with the run's requester and the installation.
func TestPlatformAttachChoice(t *testing.T) {
	ctx := context.Background()
	rig := newAttachRig(t)
	mustInsert(t, rig.db, "externalService", rig.s3Row("svc-s3", pgdoc.M{externalservice.AttachPolicyKey: false, platformlife.ProviderMarker: "pi-1"}))
	rig.project(t, "p1", "org-1", "ACTIVE")
	rig.project(t, "p2", "org-1", "ACTIVE")
	policy := func() bool {
		t.Helper()
		es, err := rig.esSvc.Get(ctx, "svc-s3")
		if err != nil {
			t.Fatal(err)
		}
		return es.AttachToNewProjects()
	}
	runs := func() int {
		t.Helper()
		all, _, err := rig.svc.List(ctx, "svc-s3", 20)
		if err != nil {
			t.Fatal(err)
		}
		return len(all)
	}
	attach := func(choice string, wantDone bool) string {
		t.Helper()
		done, note, err := rig.adminH.PlatformAttach(ctx, "pi-1", "svc-s3", choice, "admin-1")
		if err != nil || done != wantDone {
			t.Fatalf("%s: done %v, %q, %v", choice, done, note, err)
		}
		return note
	}

	attach(platformlife.AttachNew, true)
	if !policy() || runs() != 0 || bindings(rig.load(t, "p1"), "svc-s3") != 0 {
		t.Fatalf("new: policy %v, %d runs", policy(), runs())
	}
	if note := attach(platformlife.AttachAll, true); !strings.Contains(note, "2 existing projects") {
		t.Fatalf("all: %q", note)
	}
	rig.drain(t)
	if runs() != 1 || bindings(rig.load(t, "p1"), "svc-s3") != 1 || bindings(rig.load(t, "p2"), "svc-s3") != 1 {
		t.Fatalf("all: %d runs, p1 %d, p2 %d", runs(), bindings(rig.load(t, "p1"), "svc-s3"), bindings(rig.load(t, "p2"), "svc-s3"))
	}
	attach(platformlife.AttachAll, true)
	if runs() != 1 {
		t.Fatalf("a second all started a run: %d", runs())
	}
	attach(platformlife.AttachNone, true)
	if policy() || bindings(rig.load(t, "p1"), "svc-s3") != 1 {
		t.Fatalf("none: policy %v, p1 %d", policy(), bindings(rig.load(t, "p1"), "svc-s3"))
	}

	// A disabled provider: the policy is written, the projects wait for the next apply.
	rig.project(t, "p3", "org-1", "ACTIVE")
	if _, err := rig.db.Pool.Exec(ctx, `UPDATE "externalService" SET doc=jsonb_set(doc,'{status}','"DISABLED"') WHERE id='svc-s3'`); err != nil {
		t.Fatal(err)
	}
	if note := attach(platformlife.AttachAll, false); !strings.Contains(note, "disabled") || !policy() || runs() != 1 {
		t.Fatalf("disabled: %q, policy %v, %d runs", note, policy(), runs())
	}

	// The audit log: three policy changes and one bulk attach, each by the requester and naming the installation.
	var events []audit.AuditEvent
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) { // LogEvent writes in the background
		var err error
		if events, err = rig.auditRepo.QueryAll(ctx, audit.Filter{ResourceID: "svc-s3"}, 20); err != nil {
			t.Fatal(err)
		}
		if len(events) >= 4 || time.Now().After(deadline) {
			break
		}
	}
	counts := map[string]int{}
	for _, ev := range events {
		if ev.Actor == nil || ev.Actor.ID != "admin-1" || ev.ResourceMetadata["platformInstallationId"] != "pi-1" {
			t.Fatalf("event %+v", ev)
		}
		counts[ev.Action]++
	}
	if counts[audit.ActionUpdate] != 3 || counts[admin.ActionBulkAttach] != 1 {
		t.Fatalf("audit events: %v", counts)
	}
}
