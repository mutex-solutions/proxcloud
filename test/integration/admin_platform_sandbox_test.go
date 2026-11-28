//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// TestPlatformSandboxOffIsRefusedAtTheSave: the save of placement off on a platform whose sandboxes a run built is
// refused with 409 sandbox_provider_live while the sandbox provider the run registered is live, names the provider
// and stores nothing; with the provider disabled the same save is no longer refused for that.
func TestPlatformSandboxOffIsRefusedAtTheSave(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC", "config": pgdoc.M{
		"provider": "proxmox", "region": "lab", "apiUrl": "https://127.0.0.1:1", "tokenId": "proxcloud@pve!platform", "importStorage": "nas-templates",
		"network": pgdoc.M{"cidrPool": "10.100.0.0/16"}}, "secret": pgdoc.M{"tokenSecret": enc.Encrypt(portalToken)}})
	mustInsert(t, db, "adminPermission", pgdoc.M{"_id": "admin-sub", "role": "ADMIN"})
	store := platformlife.NewStore(db, enc)
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()),
		externalservice.NewService(externalservice.NewRepo(db), enc), "", "", "")
	h.SetPlatform(store)
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string) (int, string, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var a struct {
			Errors struct {
				Details map[string]any `json:"details"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &a)
		code, _ := a.Errors.Details["code"].(string)
		return w.Code, code, w.Body.String()
	}
	base := "/admin/platform/installations"
	status, _, raw := call(http.MethodPost, base, platformBody("10.200.200.1", 2, 0))
	var created struct {
		Data platformlife.Installation `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &created); err != nil || status != http.StatusOK || created.Data.ID == "" {
		t.Fatalf("create: %d %s", status, raw)
	}
	id := created.Data.ID
	// A run installed it, built a shared sandbox group and registered the sandbox provider, which was then set live.
	if _, err := db.C(platformlife.InstallationCollection).SetByID(ctx, id, pgdoc.M{"status": pgdoc.M{"phase": "ready",
		"bom":       pgdoc.M{"release": "0.12.0-dev", "sha256": "x"},
		"sandbox":   pgdoc.M{"placement": "shared", "nodeRange": "10.200.200.64/28", "proxyAddress": "10.200.200.210", "count": 1},
		"providers": pgdoc.M{"sandbox": "svc-sbx"}}}, nil); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-sbx", "name": "lab Sandboxes", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "sandbox", "region": "lab", platformlife.ProviderMarker: id, "sandbox": pgdoc.M{"proxmoxServiceId": "svc-pve", "mode": "shared"}}})
	off := strings.Replace(platformBody("10.200.200.1", 2, 1), `"spec":{`, `"spec":{"sandbox":{"placement":"off"},`, 1)
	if !strings.Contains(off, `"placement":"off"`) {
		t.Fatal("the body names no sandbox block")
	}
	status, code, raw := call(http.MethodPut, base+"/"+id, off)
	if status != http.StatusConflict || code != "sandbox_provider_live" || !strings.Contains(raw, "lab Sandboxes") || !strings.Contains(raw, "spec.sandbox.placement") {
		t.Fatalf("off with the provider live: %d %s %s", status, code, raw)
	}
	if got, err := store.Get(ctx, id); err != nil || got.Rev != 1 || got.Spec.Sandbox != nil {
		t.Fatalf("the refused save stored something: %+v %v", got, err)
	}
	// Disabled: the save is no longer refused for the provider (what else it answers is the settings' own check).
	if _, err := db.C("externalService").SetByID(ctx, "svc-sbx", pgdoc.M{"status": "DISABLED"}, nil); err != nil {
		t.Fatal(err)
	}
	if status, code, raw = call(http.MethodPut, base+"/"+id, off); code == "sandbox_provider_live" {
		t.Fatalf("off with the provider disabled: %d %s", status, raw)
	}
}

// TestPlatformSandboxProviderRows runs, against PostgreSQL, what a platform run writes for sandboxes: the sandbox
// settings on the Proxmox provider row (read back as the provider sync reads them, the kubeconfig sealed), their
// removal, the refusal over a block of another owner, and the sandbox provider row: created DISABLED with its
// kubeconfig sealed, adopted with the operator's templates kept, and still DISABLED after a Remove and a new install.
func TestPlatformSandboxProviderRows(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	enc := textcrypt.New("test-key")
	repo := admin.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), enc)
	h := admin.NewHandler(repo, nil, nil, nil, nil, nil, nil, nil, audit.NewService(audit.NewRepo(db), slog.Default()), esSvc, "", "", "")
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-pve", "name": "lab", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "proxmox", "region": "lab", "network": pgdoc.M{"cidrPool": "10.100.0.0/16"}}, "secret": pgdoc.M{"tokenSecret": enc.Encrypt("portal-token")}})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-installer", "name": "site", "type": "CLOUD", "status": "PUBLIC",
		"config": pgdoc.M{"provider": "proxmox", "region": "site", "sandbox": pgdoc.M{"pool": "proxcloud-sandbox"}, "network": pgdoc.M{"sandboxCidrs": []string{"10.110.0.0/24"}}}})
	render := platformlife.RenderInput{ID: "pi-1", Name: "lab", Region: "lab", RegionName: "Lab", Network: platformlife.RenderNetwork{Bridge: "vmbr0", VLAN: 140},
		Cluster: platformlife.RenderCluster{Endpoint: "192.168.140.10", NodeRange: []string{"192.168.140.20-192.168.140.39"}},
		Sandbox: &platformlife.RenderSandbox{Mode: platformlife.SandboxShared, NodeRange: "192.168.140.64/28", PlatformPrefix: "192.168.140.0/24", DNS: []string{"192.168.140.1"},
			ProxyAddress: "192.168.140.210", Domain: "sbx.lab.test"}}
	in := &platformlife.Installation{ID: "pi-1", ServiceID: "svc-pve"}
	site := func(kubeconfig string) *platformlife.SandboxSite {
		return &platformlife.SandboxSite{Range: "192.168.140.64/28", Config: platformlife.SandboxSiteConfig(render), Kubeconfig: kubeconfig}
	}

	// Stage cluster: no kubeconfig yet. Stage register: with it.
	if err := h.PlatformSandboxSite(ctx, in, site("")); err != nil {
		t.Fatal(err)
	}
	if err := h.PlatformSandboxSite(ctx, in, site("firewall-kubeconfig-0123456789")); err != nil {
		t.Fatal(err)
	}
	es, err := esSvc.Get(ctx, "svc-pve")
	if err != nil {
		t.Fatal(err)
	}
	cfg, ok := es.PVESandboxConfig()
	if !ok || cfg.Owner != "pi-1" || cfg.Mode != "shared" || cfg.Kubeconfig != "firewall-kubeconfig-0123456789" || cfg.Bridge != "vmbr0" || cfg.VLAN != 140 ||
		len(es.PVENetworkConfig().SandboxCIDRs) != 1 || es.PVENetworkConfig().CIDRPool != "10.100.0.0/16" || es.PVEConfig().Secret != "portal-token" {
		t.Fatalf("the provider as the sync reads it: %+v, network %+v", cfg, es.PVENetworkConfig())
	}
	// What the isolation rules let the range reach of its own cluster, as every reader of the provider row gets it
	// (the sync, a network create, the Check, the run itself): the API address and the node range, and nothing else.
	wantAllow := []string{"tcp 192.168.140.10 6443", "tcp 192.168.140.20/30,192.168.140.24/29,192.168.140.32/29 6443",
		"udp 192.168.140.20/30,192.168.140.24/29,192.168.140.32/29 8472", "tcp 192.168.140.20/30,192.168.140.24/29,192.168.140.32/29 4240",
		"icmp 192.168.140.20/30,192.168.140.24/29,192.168.140.32/29"}
	if got := es.PVESandboxEndpoints(); cfg.ControlPlane != "192.168.140.10:6443" || !slices.Equal(got, wantAllow) {
		t.Fatalf("the cluster flows of the stored row: controlPlane %q, allow %q", cfg.ControlPlane, got)
	}
	raw, err := repo.FindDoc(ctx, "externalService", "svc-pve")
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := raw["secret"].(map[string]any)["sandboxKubeconfig"].(string); stored == "" || strings.Contains(stored, "firewall-kubeconfig") {
		t.Fatalf("the sandbox kubeconfig is not sealed in the row: %q", stored)
	}
	if site := platformlife.SiteOf(es); site.SandboxPool != "proxcloud-sbx" || site.SandboxOwner != "pi-1" {
		t.Fatalf("the Check's site: %+v", site)
	}
	// The installer's sandbox cluster on the provider: nothing of it is overwritten or taken away.
	other := &platformlife.Installation{ID: "pi-1", ServiceID: "svc-installer"}
	if err := h.PlatformSandboxSite(ctx, other, site("")); !errors.Is(err, platformlife.ErrProviderConflict) {
		t.Fatalf("over the installer's block: %v", err)
	}
	if err := h.PlatformSandboxSite(ctx, other, &platformlife.SandboxSite{Range: "192.168.140.64/28"}); err != nil {
		t.Fatalf("a removal beside the installer's block: %v", err)
	}
	if inst, _ := esSvc.Get(ctx, "svc-installer"); inst == nil || len(inst.PVENetworkConfig().SandboxCIDRs) != 1 {
		t.Fatalf("the installer's provider row changed: %+v", inst)
	} else if c, ok := inst.PVESandboxConfig(); !ok || c.Pool != "proxcloud-sandbox" {
		t.Fatalf("the installer's block: %+v", c)
	}

	// The sandbox provider: created DISABLED, sealed; adopted with what the operator changed kept.
	body := func(kubeconfig string) map[string]any {
		b, err := platformlife.SandboxProviderBody(render, "svc-pve", kubeconfig)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	id, err := h.PlatformRegister(ctx, body("portal-kubeconfig-one-0123456789"))
	if err != nil || id == "" {
		t.Fatalf("register: %q %v", id, err)
	}
	sbx, err := esSvc.Get(ctx, id)
	if err != nil || !sbx.IsSandbox() || !sbx.IsDisabled() || sbx.SandboxProxmoxServiceID() != "svc-pve" || !sbx.SandboxConfig().Shared || sbx.SandboxConfig().Kubeconfig != "portal-kubeconfig-one-0123456789" || len(sbx.SandboxConfig().Templates) != 4 {
		t.Fatalf("the registered sandbox provider: %+v %v", sbx, err)
	}
	row, _ := repo.FindDoc(ctx, "externalService", id)
	row["name"] = "Sandboxes (renamed)"
	row["config"].(map[string]any)["sandbox"].(map[string]any)["templates"] = map[string]any{"mine": map[string]any{"image": "registry.example/mine@sha256:1", "cpu": 1, "memoryGiB": 1}}
	if err := repo.ReplaceDoc(ctx, "externalService", id, row); err != nil {
		t.Fatal(err)
	}
	again, err := h.PlatformRegister(ctx, body("portal-kubeconfig-two-0123456789"))
	sbx, _ = esSvc.Get(ctx, id)
	if err != nil || again != id || sbx.Name != "Sandboxes (renamed)" || len(sbx.SandboxConfig().Templates) != 1 || sbx.SandboxConfig().Kubeconfig != "portal-kubeconfig-two-0123456789" || !sbx.IsDisabled() {
		t.Fatalf("adopted: %q %v %+v", again, err, sbx)
	}
	// A Remove disables the installation's rows; a new install gives the others their status back, never this one.
	row, _ = repo.FindDoc(ctx, "externalService", id)
	row["status"] = "PUBLIC" // set live by hand in the store: the gates are the admin route's, not this test's
	if err := repo.ReplaceDoc(ctx, "externalService", id, row); err != nil {
		t.Fatal(err)
	}
	if err := h.PlatformDisable(ctx, "pi-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PlatformRegister(ctx, body("portal-kubeconfig-three-0123456789")); err != nil {
		t.Fatal(err)
	}
	if sbx, _ = esSvc.Get(ctx, id); !sbx.IsDisabled() {
		t.Fatalf("a run set the sandbox provider %s after a Remove", sbx.Status)
	}
	if why := platformlife.SandboxOffBlocked(sbx); why != "" {
		t.Fatalf("a disabled provider blocks off: %s", why)
	}

	// Sandboxes switched off: the settings go, the rest of the provider row stays.
	if err := h.PlatformSandboxSite(ctx, in, &platformlife.SandboxSite{Range: "192.168.140.64/28"}); err != nil {
		t.Fatal(err)
	}
	es, _ = esSvc.Get(ctx, "svc-pve")
	if _, ok := es.PVESandboxConfig(); ok || len(es.PVENetworkConfig().SandboxCIDRs) != 0 || es.PVENetworkConfig().CIDRPool != "10.100.0.0/16" || es.PVEConfig().Secret != "portal-token" {
		t.Fatalf("after the removal: %+v", es.Config)
	}
}

// TestPlatformSandboxProviderNeedsAPriceRule: on a site set up with the Platform setup defaults, whose price plan has
// no rule for sandboxes, a platform's sandbox provider is refused live with 409 and stays DISABLED: every sandbox
// minute would be charged nothing, and nothing would say so (lab, v0.12.0-rc.2: the texts promised a placeholder rule
// that the defaults never added). A rule in a disabled plan, or in a plan that bills another provider only, does not
// count; with a sandbox rule in the plan that bills the provider (here at 0: given away knowingly) the same save goes
// through. The installer's sandbox provider is not asked.
func TestPlatformSandboxProviderNeedsAPriceRule(t *testing.T) {
	ctx := context.Background()
	db, defaults := setupDefaultsEnv(t)
	if code, items := defaults(http.MethodPost, applyAllDefaults); code != http.StatusOK || len(items) != 4 {
		t.Fatalf("the setup defaults: %d %+v", code, items)
	}
	var rules []pgdoc.M
	if err := db.C("pricePlanRule").Find(ctx, pgdoc.M{"resourceType": "sandbox"}, &rules); err != nil || len(rules) != 0 {
		t.Fatalf("the setup defaults added %d sandbox rules (%v): the texts and this test say none", len(rules), err)
	}
	var plans []pgdoc.M
	if err := db.C("pricePlan").Find(ctx, nil, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("price plans: %v %v", plans, err)
	}
	planID, _ := plans[0]["_id"].(string)

	sandboxCfg := func(marker string) pgdoc.M {
		cfg := pgdoc.M{"provider": "sandbox", "region": "lab", "sandbox": pgdoc.M{"templates": pgdoc.M{"base": pgdoc.M{"image": "x@sha256:1", "cpu": 1, "memoryGiB": 1}}}}
		if marker != "" {
			cfg[platformlife.ProviderMarker] = marker
		}
		return cfg
	}
	kubeconfig := pgdoc.M{"kubeconfig": "apiVersion: v1\nclusters: []\n"}
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-sbx", "name": "lab Sandboxes", "type": "CLOUD", "status": "DISABLED", "config": sandboxCfg("pi-1"), "secret": kubeconfig})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-own", "name": "site Sandboxes", "type": "CLOUD", "status": "DISABLED", "config": sandboxCfg(""), "secret": kubeconfig})

	const iss, cid = "test-iss", "test-cid"
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, pricing.NewRepo(db), nil, nil, nil, nil, nil, "", iss, cid)
	h.SetJobsScheduler(true)
	h.SetSandboxGates(func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
		return []sandbox.Gate{{Name: sandbox.GateFence, OK: true}}
	})
	router := chi.NewRouter()
	h.Routes(router)
	call := func(method, path, body string) (int, string) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	status := func(id string) string {
		var row pgdoc.M
		if found, err := db.C("externalService").FindOne(ctx, pgdoc.M{"_id": id}, &row); err != nil || !found {
			t.Fatalf("provider %s: %v", id, err)
		}
		s, _ := row["status"].(string)
		return s
	}
	live := func() (int, string) { return call(http.MethodPut, "/admin/service/svc-sbx", `{"status":"PUBLIC"}`) }
	refused := func(when string) {
		t.Helper()
		if code, raw := live(); code != http.StatusConflict || !strings.Contains(raw, "resource type sandbox") || !strings.Contains(raw, "charged nothing") || status("svc-sbx") != "DISABLED" {
			t.Fatalf("%s: %d %s, stored %s", when, code, raw, status("svc-sbx"))
		}
	}
	refused("live on the setup defaults' price plan")

	sandboxRule := func(plan string) string {
		return `{"name":"Sandbox minutes","timeUnit":"minute","resourceType":"sandbox","pricePlanId":"` + plan + `","applyMethod":"ADD_TO_TOTAL","prices":[{"attributeName":"vcpus","tiers":[{"from":0,"value":0}]}]}`
	}
	// A sandbox rule that bills nobody of this provider: in a disabled plan, and in a plan for another provider only.
	mustInsert(t, db, "pricePlan", pgdoc.M{"_id": "plan-off", "name": "off", "enabled": false, "accessMode": "PUBLIC"})
	mustInsert(t, db, "pricePlan", pgdoc.M{"_id": "plan-other", "name": "other", "enabled": true, "accessMode": "PUBLIC", "serviceProviders": []any{pgdoc.M{"serviceId": "svc-elsewhere"}}})
	for _, plan := range []string{"plan-off", "plan-other"} {
		if code, raw := call(http.MethodPost, "/admin/price-plan/rule", sandboxRule(plan)); code != http.StatusOK {
			t.Fatalf("rule in %s: %d %s", plan, code, raw)
		}
	}
	refused("live with sandbox rules only in a disabled plan and in another provider's plan")

	// The installer's sandbox provider goes live as before.
	if code, raw := call(http.MethodPut, "/admin/service/svc-own", `{"status":"PUBLIC"}`); code != http.StatusOK || status("svc-own") != "PUBLIC" {
		t.Fatalf("the installer's provider: %d %s", code, raw)
	}

	if code, raw := call(http.MethodPost, "/admin/price-plan/rule", sandboxRule(planID)); code != http.StatusOK {
		t.Fatalf("the sandbox rule: %d %s", code, raw)
	}
	if code, raw := live(); code != http.StatusOK || status("svc-sbx") != "PUBLIC" {
		t.Fatalf("live with a sandbox rule in the plan: %d %s, stored %s", code, raw, status("svc-sbx"))
	}
}
