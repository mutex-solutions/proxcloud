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
	// A block of someone else's that names no pool (what is left of an installer's sandbox cluster: its registry): the
	// sync fences nothing for it, and it is not overwritten either.
	poolless := pgdoc.M{"config": map[string]any{"sandbox": map[string]any{"registry": map[string]any{"address": "10.110.0.9", "port": 443}}}}
	if _, err := applySandboxSite(poolless, "pi-1", site(""), seal); !errors.Is(err, platformlife.ErrProviderConflict) || poolless["config"].(map[string]any)["sandbox"].(map[string]any)["registry"] == nil ||
		poolless["config"].(map[string]any)["sandbox"].(map[string]any)["pool"] != nil {
		t.Fatalf("over a block without a pool: %v %v", err, poolless)
	}
	// An empty block is nobody's.
	if changed, err := applySandboxSite(pgdoc.M{"config": map[string]any{"sandbox": map[string]any{}}}, "pi-1", site(""), seal); err != nil || !changed {
		t.Fatalf("over an empty block: %v %v", changed, err)
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

// The wake token of a platform's functions on its sandbox provider: stored under its own key, taken away by "", and
// never on a row that is not that installation's sandbox provider. The sandbox wake token is not touched: the provider
// read by the sandbox service still has none, which keeps autoResume refused.
func TestSetFunctionWakeToken(t *testing.T) {
	seal := func(secret pgdoc.M, v string) { secret["functionWakeToken"] = "sealed(" + v + ")" }
	doc := pgdoc.M{"_id": "svc-sandbox", "type": "CLOUD", "config": map[string]any{"provider": "sandbox", platformlife.ProviderMarker: "pi-1", "sandbox": map[string]any{"mode": "shared"}},
		"secret": map[string]any{"kubeconfig": "sealed-kubeconfig"}}
	if !setFunctionWakeToken(doc, "pi-1", "TOKEN", seal) {
		t.Fatal("the installation's own sandbox provider was refused")
	}
	secret := doc["secret"].(map[string]any)
	if len(secret) != 2 || secret["functionWakeToken"] != "sealed(TOKEN)" || secret["kubeconfig"] != "sealed-kubeconfig" {
		t.Fatalf("secret keys %v", secret)
	}
	es := &externalservice.ExternalService{Type: externalservice.TypeCloud, Config: doc["config"].(map[string]any), Secret: map[string]any{"kubeconfig": "KUBECONFIG", "functionWakeToken": "TOKEN"}}
	if cfg := es.SandboxConfig(); cfg.ProxyWakeToken != "" {
		t.Fatal("the functions' token became the sandbox wake token: autoResume would no longer be refused")
	}
	if !setFunctionWakeToken(doc, "pi-1", "", seal) || len(secret) != 1 || secret["kubeconfig"] != "sealed-kubeconfig" {
		t.Fatalf("taking it away: %v", secret)
	}
	for name, other := range map[string]pgdoc.M{
		"another installation's": {"config": map[string]any{"provider": "sandbox", platformlife.ProviderMarker: "pi-2"}},
		"the installer's":        {"config": map[string]any{"provider": "sandbox"}},
		"a Kubernetes provider":  {"config": map[string]any{"provider": "kamaji", platformlife.ProviderMarker: "pi-1"}},
	} {
		if setFunctionWakeToken(other, "pi-1", "TOKEN", seal) || other["secret"] != nil {
			t.Errorf("%s provider took the token: %v", name, other)
		}
	}
}

// Functions a run built are not switched off at the save while their provider holds a function, nor by a build that
// cannot read the records. Sandboxes switched off as a whole, and functions never built, are not this refusal's.
func TestFunctionsOffRefused(t *testing.T) {
	old := &platformlife.Installation{}
	old.Spec.Sandbox = &platformlife.SandboxSpec{Placement: platformlife.SandboxShared, Functions: &platformlife.SandboxFunctions{Enabled: true, PortalURL: "https://portal.example.org"}}
	old.Status.Sandbox = &platformlife.SandboxStatus{Placement: platformlife.SandboxShared, Functions: &platformlife.FunctionsStatus{PortalURL: "https://portal.example.org", TokenVersion: "pr-1"}}
	old.Status.Providers = &platformlife.ProviderIDs{Sandbox: "svc-sandbox"}
	off := &platformlife.Installation{}
	off.Spec.Sandbox = &platformlife.SandboxSpec{Placement: platformlife.SandboxShared, Functions: &platformlife.SandboxFunctions{}}
	h := &Handler{}
	if why := h.functionsOffRefused(context.Background(), old, off); !strings.Contains(why, "cannot read the function records") {
		t.Fatalf("without a read of the records: %q", why)
	}
	exist, asked := true, ""
	h.SetPlatformFunctions(func(_ context.Context, id string) (bool, error) { asked = id; return exist, nil })
	if why := h.functionsOffRefused(context.Background(), old, off); why != platformlife.FunctionsOffBlocked || asked != "svc-sandbox" {
		t.Fatalf("with a function: %q (asked %q)", why, asked)
	}
	exist = false
	if why := h.functionsOffRefused(context.Background(), old, off); why != "" {
		t.Fatalf("no function left: %q", why)
	}
	h.SetPlatformFunctions(func(context.Context, string) (bool, error) { return false, errors.New("no answer") })
	if why := h.functionsOffRefused(context.Background(), old, off); !strings.Contains(why, "could not be read") {
		t.Fatalf("a failed read: %q", why)
	}
	h.SetPlatformFunctions(func(context.Context, string) (bool, error) { t.Fatal("asked"); return true, nil })
	gone := &platformlife.Installation{}
	gone.Spec.Sandbox = &platformlife.SandboxSpec{Placement: platformlife.SandboxOff}
	never := &platformlife.Installation{Spec: old.Spec}
	never.Status.Sandbox = &platformlife.SandboxStatus{Placement: platformlife.SandboxShared}
	for name, pair := range map[string][2]*platformlife.Installation{"functions stay": {old, old}, "sandboxes off as a whole": {old, gone}, "never built": {never, off}} {
		if why := h.functionsOffRefused(context.Background(), pair[0], pair[1]); why != "" {
			t.Errorf("%s: %q", name, why)
		}
	}
}

// Functions that name no portal URL get the API's own at the save, and only they do.
func TestDefaultPortalURL(t *testing.T) {
	spec := func(fn *platformlife.SandboxFunctions) *platformlife.Spec {
		return &platformlife.Spec{Sandbox: &platformlife.SandboxSpec{Placement: platformlife.SandboxShared, Functions: fn}}
	}
	for name, tc := range map[string]struct {
		fn   *platformlife.SandboxFunctions
		want string
	}{
		"enabled, none named": {&platformlife.SandboxFunctions{Enabled: true}, "https://api.example.org"},
		"enabled, one named":  {&platformlife.SandboxFunctions{Enabled: true, PortalURL: "https://10.0.0.5"}, "https://10.0.0.5"},
		"not enabled":         {&platformlife.SandboxFunctions{}, ""},
	} {
		s := spec(tc.fn)
		if defaultPortalURL(s, "https://api.example.org"); s.Sandbox.Functions.PortalURL != tc.want {
			t.Errorf("%s: %q", name, s.Sandbox.Functions.PortalURL)
		}
	}
	defaultPortalURL(spec(nil), "https://api.example.org")
	defaultPortalURL(&platformlife.Spec{}, "https://api.example.org")
}
