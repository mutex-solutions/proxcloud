//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

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
