package admin

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
)

// The write of a run's sandbox settings on the provider document: its own block and range come and go, anything of
// another owner refuses the write and stays.
func TestApplySandboxSite(t *testing.T) {
	site := func(kubeconfig string) *platformlife.SandboxSite {
		return &platformlife.SandboxSite{Range: "192.168.140.64/28", Kubeconfig: kubeconfig,
			Config: map[string]any{platformlife.ProviderMarker: "pi-1", "pool": "proxcloud-sbx", "mode": "shared", "unsecuredMinAgeMinutes": 20, "nodes": []any{"192.168.140.64/28"}}}
	}
	seal := func(secret pgdoc.M, v string) { secret["sandboxKubeconfig"] = "sealed(" + v + ")" }
	doc := pgdoc.M{"_id": "svc-pve", "config": map[string]any{"provider": "proxmox", "network": map[string]any{"cidrPool": "10.100.0.0/16", "blockedCidrs": []any{"192.168.140.5/32"}}},
		"secret": map[string]any{"tokenSecret": "sealed-token"}}
	changed, err := applySandboxSite(doc, "pi-1", site(""), seal)
	if err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	es := &externalservice.ExternalService{Config: doc["config"].(map[string]any)}
	if cfg, ok := es.PVESandboxConfig(); !ok || cfg.Owner != "pi-1" || cfg.Pool != "proxcloud-sbx" || !reflect.DeepEqual(es.PVENetworkConfig().SandboxCIDRs, []string{"192.168.140.64/28"}) {
		t.Fatalf("the provider reads %+v, sandboxCidrs %v", cfg, es.PVENetworkConfig().SandboxCIDRs)
	}
	if _, has := doc["secret"].(map[string]any)["sandboxKubeconfig"]; has {
		t.Fatal("a kubeconfig was stored before the run had one")
	}
	// As the store hands the document back (numbers as float64): the same settings are no change.
	doc["config"].(map[string]any)["sandbox"].(map[string]any)["unsecuredMinAgeMinutes"] = float64(20)
	if changed, err := applySandboxSite(doc, "pi-1", site(""), seal); err != nil || changed {
		t.Fatalf("the same settings again: changed %v, %v", changed, err)
	}
	if changed, err := applySandboxSite(doc, "pi-1", site("KUBECONFIG"), seal); err != nil || !changed || doc["secret"].(map[string]any)["sandboxKubeconfig"] != "sealed(KUBECONFIG)" {
		t.Fatalf("with the kubeconfig: %v %v %v", changed, err, doc["secret"])
	}
	// Another installation, and the installer's block (no marker): refused, the document as it was.
	before := doc["config"].(map[string]any)["sandbox"]
	for _, other := range []string{"pi-2", ""} {
		if _, err := applySandboxSite(doc, "pi-2", &platformlife.SandboxSite{Range: "192.168.140.64/28", Config: map[string]any{platformlife.ProviderMarker: "pi-2", "pool": "proxcloud-sbx"}}, seal); !errors.Is(err, platformlife.ErrProviderConflict) {
			t.Fatalf("installation pi-2 over pi-1's block: %v", err)
		}
		inst := pgdoc.M{"config": map[string]any{"sandbox": map[string]any{"pool": "proxcloud-sandbox", platformlife.ProviderMarker: other}, "network": map[string]any{"sandboxCidrs": []any{"10.110.0.0/24"}}}}
		if _, err := applySandboxSite(inst, "pi-1", site(""), seal); !errors.Is(err, platformlife.ErrProviderConflict) || inst["config"].(map[string]any)["sandbox"].(map[string]any)["pool"] != "proxcloud-sandbox" {
			t.Fatalf("over a block of owner %q: %v %v", other, err, inst)
		}
		// A removal takes nothing of another owner's away, and does not fail for it (a Remove must still end).
		if changed, err := applySandboxSite(inst, "pi-1", &platformlife.SandboxSite{Range: "192.168.140.64/28"}, seal); err != nil || changed ||
			inst["config"].(map[string]any)["sandbox"] == nil || len(inst["config"].(map[string]any)["network"].(map[string]any)["sandboxCidrs"].([]any)) != 1 {
			t.Fatalf("removal beside a block of owner %q: changed %v, %v, %v", other, changed, err, inst)
		}
	}
	if !reflect.DeepEqual(before, doc["config"].(map[string]any)["sandbox"]) {
		t.Fatal("a refused write changed the block")
	}
	// A sandbox network of someone else's with no block: not dropped either.
	lone := pgdoc.M{"config": map[string]any{"network": map[string]any{"sandboxCidrs": []any{"10.110.0.0/24"}}}}
	if _, err := applySandboxSite(lone, "pi-1", site(""), seal); !errors.Is(err, platformlife.ErrProviderConflict) || !strings.Contains(err.Error(), "10.110.0.0/24") {
		t.Fatalf("a foreign sandbox network: %v", err)
	}
	// Sandboxes switched off: the three go, everything else of the provider stays.
	changed, err = applySandboxSite(doc, "pi-1", &platformlife.SandboxSite{Range: "192.168.140.64/28"}, seal)
	cfg := doc["config"].(map[string]any)
	if err != nil || !changed || cfg["sandbox"] != nil || cfg["network"].(map[string]any)["sandboxCidrs"] != nil || doc["secret"].(map[string]any)["sandboxKubeconfig"] != nil {
		t.Fatalf("removal: %v %v %v", changed, err, doc)
	}
	if cfg["network"].(map[string]any)["cidrPool"] != "10.100.0.0/16" || doc["secret"].(map[string]any)["tokenSecret"] != "sealed-token" || len(cfg["network"].(map[string]any)["blockedCidrs"].([]any)) != 1 {
		t.Fatalf("the removal took more than the sandbox settings: %v", doc)
	}
	if changed, err := applySandboxSite(doc, "pi-1", &platformlife.SandboxSite{Range: "192.168.140.64/28"}, seal); err != nil || changed {
		t.Fatalf("a second removal: %v %v", changed, err)
	}
}

// A platform's sandbox provider goes live only with the scheduler on; the installer's is not asked.
func TestPlatformSandboxProviderNeedsTheScheduler(t *testing.T) {
	ctx := context.Background()
	gates := func(context.Context, *externalservice.ExternalService) []sandbox.Gate {
		return []sandbox.Gate{{Name: sandbox.GateFence, OK: true}}
	}
	doc := func(status, marker string) pgdoc.M {
		cfg := map[string]any{"provider": "sandbox", "region": "az1", "sandbox": map[string]any{"templates": map[string]any{"base": map[string]any{"image": "x@sha256:1", "cpu": 1, "memoryGiB": 1}}}}
		if marker != "" {
			cfg[platformlife.ProviderMarker] = marker
		}
		return pgdoc.M{"_id": "sbx", "status": status, "config": cfg, "secret": map[string]any{"kubeconfig": "apiVersion: v1\nclusters: []\n"}}
	}
	h := &Handler{}
	h.SetSandboxGates(gates)
	if herr := h.checkSandboxGoLive(ctx, doc("PUBLIC", "pi-1")); herr == nil || herr.Status != http.StatusConflict || !strings.Contains(herr.Msg, "job scheduler is off") || !strings.Contains(herr.Msg, "placeholder rate") {
		t.Fatalf("live with the scheduler off: %+v", herr)
	}
	if herr := h.checkSandboxGoLive(ctx, doc("DISABLED", "pi-1")); herr != nil {
		t.Fatalf("stored disabled: %+v", herr)
	}
	if herr := h.checkSandboxGoLive(ctx, doc("PUBLIC", "")); herr != nil && strings.Contains(herr.Msg, "scheduler") {
		t.Fatalf("the installer's provider was asked for the scheduler: %+v", herr)
	}
	h.SetJobsScheduler(true)
	if herr := h.checkSandboxGoLive(ctx, doc("PUBLIC", "pi-1")); herr != nil && strings.Contains(herr.Msg, "scheduler") {
		t.Fatalf("live with the scheduler on: %+v", herr)
	}
}
