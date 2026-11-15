package admin

import (
	"encoding/json"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/deploy/seed"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func testSeed(t *testing.T) *seedDefaults {
	t.Helper()
	sd, err := loadSeed()
	if err != nil {
		t.Fatal(err)
	}
	return sd
}

// The embedded files load, and every flavor and rule in them passes its admin endpoint's validation.
func TestSeedDefaultsLoad(t *testing.T) {
	sd := testSeed(t)
	if len(sd.Flavors) != 3 || sd.Plan.Name != "Public rate card" || !sd.Plan.Enabled || sd.Currency != "USD" || sd.Platform["name"] != "ProxCloud" {
		t.Fatalf("seed: %d flavors, plan %+v, currency %q, platform %v", len(sd.Flavors), sd.Plan, sd.Currency, sd.Platform)
	}
	for _, r := range sd.Rules {
		if r.PricePlanID != "" {
			t.Fatalf("rule %q keeps the file's plan id placeholder", r.Name)
		}
	}
}

func ruleTypes(rules []pricePlanRuleReq) (types []string, gpu int) {
	for _, r := range rules {
		if !slices.Contains(types, r.ResourceType) {
			types = append(types, r.ResourceType)
		}
		if isGPURule(r) {
			gpu++
		}
	}
	slices.Sort(types)
	return types, gpu
}

// The rules an install gets: the base types always, GPU rules with a GPU mapping, share and bucket rules with such a
// provider, and never the managed-service rules of the file.
func TestDefaultRules(t *testing.T) {
	base := []string{"floating_ip", "image", "instance", "instance_traffic", "server_protection", "server_snapshot", "volume"}
	tests := []struct {
		name               string
		gpu, share, bucket bool
		types              []string
		gpuRules           int
	}{
		{"proxmox only", false, false, false, base, 0},
		{"with GPUs", true, false, false, base, 7},
		{"with a share provider", false, true, false, append(slices.Clone(base), "share"), 0},
		{"with an object store", false, false, true, append(slices.Clone(base), "bucket"), 0},
		{"everything", true, true, true, append(slices.Clone(base), "bucket", "share"), 7},
	}
	sd := testSeed(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultRules(sd.Rules, tt.gpu, tt.share, tt.bucket)
			types, gpu := ruleTypes(got)
			slices.Sort(tt.types)
			if !reflect.DeepEqual(types, tt.types) || gpu != tt.gpuRules {
				t.Fatalf("types %v with %d GPU rules, want %v with %d", types, gpu, tt.types, tt.gpuRules)
			}
			// One CPU and RAM rule for instances whatever else is picked.
			if n := len(got) - gpu; n != len(tt.types) {
				t.Fatalf("%d rules besides the GPU ones for %d types", n, len(tt.types))
			}
		})
	}
}

func pveProvider(id string, cfg map[string]any) externalservice.ExternalService {
	c := map[string]any{"provider": "proxmox", "region": "lab"}
	maps.Copy(c, cfg)
	return externalservice.ExternalService{ID: id, Type: externalservice.TypeCloud, Config: c}
}

func itemByID(t *testing.T, items []defaultsItem, id string) defaultsItem {
	t.Helper()
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	t.Fatalf("no item %s in %+v", id, items)
	return defaultsItem{}
}

// applied is the state after an apply of w wrote everything.
func applied(st defaultsState, w defaultsWrite) defaultsState {
	st.Flavors = maps.Clone(st.Flavors)
	for _, f := range w.Flavors {
		st.Flavors[f.Slug] = true
	}
	if w.Plan != nil {
		st.Plans = append(slices.Clone(st.Plans), pricing.PricePlan{ID: "new", Name: str(w.Plan["name"]), Enabled: w.Plan["enabled"] == true, AccessMode: str(w.Plan["accessMode"])})
	}
	if w.Quota != nil {
		p := pveProvider(st.Provider.ID, st.Provider.Config)
		p.Config["provisioning"] = map[string]any{"quota": w.Quota}
		st.Provider = &p
	}
	if w.Region != nil {
		st.Regions = append(slices.Clone(st.Regions), *w.Region)
	}
	return st
}

func TestPlanDefaults(t *testing.T) {
	sd := testSeed(t)
	gpuLimits := map[string]any{"nvidia-4090": float64(2), "*": float64(0)}
	withProvider := func(cfg map[string]any) defaultsState {
		p := pveProvider("svc-1", cfg)
		return defaultsState{Flavors: map[string]bool{}, Currency: "USD", Services: []externalservice.ExternalService{p}, Provider: &p}
	}
	tests := []struct {
		name   string
		st     defaultsState
		states map[string]string // item → state
		check  func(t *testing.T, w defaultsWrite, items []defaultsItem)
	}{
		{"fresh compose install, no provider yet", defaultsState{Flavors: map[string]bool{}, Currency: "USD", PlatformID: "seed"},
			map[string]string{defaultsFlavors: stateTodo, defaultsPricePlan: stateTodo, defaultsQuota: stateBlocked, defaultsRegion: stateBlocked},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if len(w.Flavors) != 3 || w.Plan["enabled"] != true || w.Plan["accessMode"] != "PUBLIC" || len(w.Rules) != 7 || w.Quota != nil || w.Region != nil {
					t.Fatalf("write: %d flavors, plan %v, %d rules, quota %v, region %v", len(w.Flavors), w.Plan, len(w.Rules), w.Quota, w.Region)
				}
			}},
		{"with a provider", withProvider(nil),
			map[string]string{defaultsFlavors: stateTodo, defaultsPricePlan: stateTodo, defaultsQuota: stateTodo, defaultsRegion: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				want := map[string]any{}
				for k, v := range seed.DefaultQuota {
					want[k] = v
				}
				if !reflect.DeepEqual(w.Quota, want) || *w.Region != (platformconfig.RegionDisplayConfig{ServiceID: "svc-1", Region: "lab", Order: 0}) {
					t.Fatalf("quota %v region %+v", w.Quota, w.Region)
				}
			}},
		{"one flavor exists", defaultsState{Flavors: map[string]bool{"s2-medium": true}, Currency: "USD"},
			map[string]string{defaultsFlavors: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if len(w.Flavors) != 2 || w.Flavors[0].Slug != "s1-small" || w.Flavors[1].Slug != "s4-large" {
					t.Fatalf("flavors to add: %+v", w.Flavors)
				}
			}},
		{"an enabled public plan exists", defaultsState{Flavors: map[string]bool{}, Currency: "USD", Plans: []pricing.PricePlan{{Name: "Mine", Enabled: true, AccessMode: "PUBLIC"}}},
			map[string]string{defaultsPricePlan: stateDone},
			func(t *testing.T, w defaultsWrite, items []defaultsItem) {
				if w.Plan != nil || w.Rules != nil || !strings.Contains(itemByID(t, items, defaultsPricePlan).Message, `"Mine"`) {
					t.Fatalf("plan written or not named: %v %+v", w.Plan, items)
				}
			}},
		// A second unscoped plan would bill svc-other twice, so the item asks the operator instead of adding one.
		{"every enabled public plan is scoped to another provider", func() defaultsState {
			st := withProvider(nil)
			st.Plans = []pricing.PricePlan{{Name: "Other site", Enabled: true, AccessMode: "PUBLIC", ServiceProviders: []pricing.PricePlanServiceProvider{{ServiceID: "svc-other"}}}}
			return st
		}(), map[string]string{defaultsPricePlan: stateBlocked},
			func(t *testing.T, w defaultsWrite, items []defaultsItem) {
				if m := itemByID(t, items, defaultsPricePlan).Message; w.Plan != nil || !strings.Contains(m, `"Other site"`) || !strings.Contains(m, "add this provider") {
					t.Fatalf("plan %v, message %s", w.Plan, m)
				}
			}},
		{"an enabled public plan scoped to this provider", func() defaultsState {
			st := withProvider(nil)
			st.Plans = []pricing.PricePlan{{Name: "Lab", Enabled: true, AccessMode: "PUBLIC", ServiceProviders: []pricing.PricePlanServiceProvider{{ServiceID: "svc-1"}}}}
			return st
		}(), map[string]string{defaultsPricePlan: stateDone}, nil},
		{"no provider named: a scoped public plan still counts", defaultsState{Flavors: map[string]bool{}, Currency: "USD", Plans: []pricing.PricePlan{
			{Name: "Other site", Enabled: true, AccessMode: "PUBLIC", ServiceProviders: []pricing.PricePlanServiceProvider{{ServiceID: "svc-other"}}}}},
			map[string]string{defaultsPricePlan: stateDone}, nil},
		{"only disabled and scoped plans", defaultsState{Flavors: map[string]bool{}, Currency: "USD", Plans: []pricing.PricePlan{
			{Name: "Off", Enabled: false, AccessMode: "PUBLIC"}, {Name: "Partner", Enabled: true, AccessMode: "SCOPED"}}},
			map[string]string{defaultsPricePlan: stateTodo}, nil},
		{"the default plan exists but is disabled", defaultsState{Flavors: map[string]bool{}, Currency: "USD", Plans: []pricing.PricePlan{{Name: "Public rate card", AccessMode: "PUBLIC"}}},
			map[string]string{defaultsPricePlan: stateBlocked},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if w.Plan != nil {
					t.Fatal("a second plan of the same name would be written")
				}
			}},
		{"no billing configuration", defaultsState{Flavors: map[string]bool{}}, map[string]string{defaultsPricePlan: stateBlocked}, nil},
		{"another base currency is named", defaultsState{Flavors: map[string]bool{}, Currency: "EUR"}, map[string]string{defaultsPricePlan: stateTodo},
			func(t *testing.T, _ defaultsWrite, items []defaultsItem) {
				if m := itemByID(t, items, defaultsPricePlan).Message; !strings.Contains(m, "USD amounts") || !strings.Contains(m, "EUR amounts") {
					t.Fatalf("message: %s", m)
				}
			}},
		{"GPU mappings bring the GPU rules", withProvider(map[string]any{"gpus": map[string]any{"gpu-a": map[string]any{"alias": "nvidia-4090"}}}),
			map[string]string{defaultsPricePlan: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if len(w.Rules) != 14 {
					t.Fatalf("%d rules, want the 7 base and 7 GPU ones", len(w.Rules))
				}
			}},
		{"quota merge keeps GPU limits and set values", withProvider(map[string]any{"provisioning": map[string]any{"quota": map[string]any{"gpu": gpuLimits, "servers": float64(50)}}}),
			map[string]string{defaultsQuota: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if !reflect.DeepEqual(w.Quota["gpu"], gpuLimits) || w.Quota["servers"] != float64(50) || w.Quota["vcpus"] != 80 {
					t.Fatalf("merged quota: %v", w.Quota)
				}
			}},
		{"region already listed", func() defaultsState {
			st := withProvider(nil)
			st.Regions = []platformconfig.RegionDisplayConfig{{ServiceID: "svc-1", Region: "old-name"}}
			return st
		}(), map[string]string{defaultsRegion: stateDone}, nil},
		{"second provider is appended after the first", func() defaultsState {
			st := withProvider(nil)
			st.Regions = []platformconfig.RegionDisplayConfig{{ServiceID: "other", Region: "r1"}}
			return st
		}(), map[string]string{defaultsRegion: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if w.Region.Order != 1 {
					t.Fatalf("order %d", w.Region.Order)
				}
			}},
		{"provider without a region", withProvider(map[string]any{"region": ""}), map[string]string{defaultsRegion: stateBlocked, defaultsQuota: stateTodo}, nil},
		{"not a Proxmox provider", func() defaultsState {
			p := externalservice.ExternalService{ID: "nas", Config: map[string]any{"provider": "truenas"}}
			return defaultsState{Flavors: map[string]bool{}, Currency: "USD", Services: []externalservice.ExternalService{p}, Provider: &p}
		}(), map[string]string{defaultsQuota: stateBlocked, defaultsRegion: stateBlocked, defaultsPricePlan: stateTodo},
			func(t *testing.T, w defaultsWrite, _ []defaultsItem) {
				if types, _ := ruleTypesOfDocs(w.Rules); !slices.Contains(types, "share") {
					t.Fatalf("a TrueNAS provider exists, yet no share rule: %v", types)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, items := planDefaults(sd, tt.st)
			if len(items) != 4 {
				t.Fatalf("%d items", len(items))
			}
			for id, want := range tt.states {
				it := itemByID(t, items, id)
				if it.State != want {
					t.Errorf("%s: state %s (%s), want %s", id, it.State, it.Message, want)
				}
				if (it.Preview != nil) != (it.State == stateTodo) {
					t.Errorf("%s: a preview goes with todo only (state %s, preview %v)", id, it.State, it.Preview)
				}
			}
			if tt.check != nil {
				tt.check(t, w, items)
			}
			// Create-only: once everything planned is written, the same plan writes nothing more.
			again, items2 := planDefaults(sd, applied(tt.st, w))
			if !reflect.DeepEqual(again, defaultsWrite{}) {
				t.Fatalf("a second apply would still write: %+v", again)
			}
			for _, it := range items2 {
				if it.State == stateTodo {
					t.Errorf("%s is still todo after the apply", it.ID)
				}
			}
		})
	}
}

func ruleTypesOfDocs(docs []map[string]any) ([]string, int) {
	var types []string
	for _, d := range docs {
		types = append(types, str(d["resourceType"]))
	}
	return types, len(docs)
}

// Applying an item needs the permission of the endpoint that writes the same document by hand.
func TestDefaultsPerms(t *testing.T) {
	all := []string{defaultsFlavors, defaultsPricePlan, defaultsQuota, defaultsRegion}
	if got := defaultsPerms(all); !reflect.DeepEqual(got, []string{externalServiceManagePerm, pricePlanManagePerm, billingConfigReadPerm, pcfgUpdatePerm}) {
		t.Fatalf("all four families: %v", got)
	}
	tests := []struct {
		name    string
		granted []string
		items   []string
		missing string
	}{
		{"super admin, everything", rolePermissions("SUPER_ADMIN"), all, ""},
		{"ADMIN cannot write the platform configuration", rolePermissions("ADMIN"), all, pcfgUpdatePerm},
		{"ADMIN applies the rest", rolePermissions("ADMIN"), []string{defaultsFlavors, defaultsPricePlan, defaultsQuota}, ""},
		{"BILLING_ADMIN applies the price plan", rolePermissions("BILLING_ADMIN"), []string{defaultsPricePlan}, ""},
		{"BILLING_ADMIN adds no flavors", rolePermissions("BILLING_ADMIN"), []string{defaultsFlavors}, externalServiceManagePerm},
		{"BILLING_ADMIN sets no quota", rolePermissions("BILLING_ADMIN"), []string{defaultsPricePlan, defaultsQuota}, externalServiceManagePerm},
		{"SUPPORT applies nothing", rolePermissions("SUPPORT"), []string{defaultsPricePlan}, pricePlanManagePerm},
		{"service only: no price plan", []string{"admin:service:*"}, []string{defaultsFlavors, defaultsPricePlan}, pricePlanManagePerm},
		{"price plan without the billing read", []string{"admin:price_plan:*"}, []string{defaultsPricePlan}, billingConfigReadPerm},
		{"platform config only: the region", []string{pcfgUpdatePerm}, []string{defaultsRegion}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingPerm(tt.granted, defaultsPerms(tt.items)); got != tt.missing {
				t.Fatalf("missing %q, want %q", got, tt.missing)
			}
		})
	}
	// The preview shows all four families, so it needs all four reads.
	for _, role := range []string{"SUPPORT", "VIEWER", "ADMIN"} {
		if m := missingPerm(rolePermissions(role), defaultsReadPerms); m != "" {
			t.Errorf("%s cannot preview: %s", role, m)
		}
	}
	if missingPerm(rolePermissions("BILLING_ADMIN"), defaultsReadPerms) != serviceReadPerm {
		t.Error("BILLING_ADMIN previews without admin:service:read")
	}
}

// The routes refuse before anything is read: a tenant, a body the apply cannot act on, rates not confirmed.
func TestSetupDefaultsRouteGates(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	r.Route("/admin", h.routeSetup)
	tenant := &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}
	op := &httpx.RequestContext{SigV4KeyID: "pk"}
	tests := []struct {
		name, method, body string
		rc                 *httpx.RequestContext
		code               int
		message            string
	}{
		{"tenant preview", "GET", "", tenant, http.StatusForbidden, ""},
		{"tenant apply", "POST", `{"items":["flavors"]}`, tenant, http.StatusForbidden, ""},
		{"tenant apply, bad body", "POST", `{`, tenant, http.StatusForbidden, ""},
		{"no items", "POST", `{}`, op, http.StatusBadRequest, "items is required"},
		{"unknown item", "POST", `{"items":["flavors","billing"]}`, op, http.StatusBadRequest, `Unknown item "billing"`},
		{"unknown field", "POST", `{"items":["flavors"],"force":true}`, op, http.StatusBadRequest, ""},
		{"price plan without the confirmation", "POST", `{"items":["flavors","pricePlan"]}`, op, http.StatusBadRequest, "confirmRates must be true"},
		{"price plan with confirmRates false", "POST", `{"items":["pricePlan"],"confirmRates":false}`, op, http.StatusBadRequest, "confirmRates must be true"},
		{"confirmRates is a boolean", "POST", `{"items":["pricePlan"],"confirmRates":"yes"}`, op, http.StatusBadRequest, ""},
		{"confirmed, catalog not wired", "POST", `{"items":["pricePlan"],"confirmRates":true}`, op, http.StatusServiceUnavailable, ""},
		{"flavors need no confirmation", "POST", `{"items":["flavors"]}`, op, http.StatusServiceUnavailable, ""},
		{"preview, catalog not wired", "GET", "", op, http.StatusServiceUnavailable, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := tuDo(r, tt.method, "/admin/setup/defaults", tt.body, tt.rc)
			if w.Code != tt.code {
				t.Fatalf("%d %s, want %d", w.Code, w.Body, tt.code)
			}
			var env struct {
				Errors struct{ Message string } `json:"errors"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if !strings.Contains(env.Errors.Message, tt.message) {
				t.Fatalf("message %q, want it to contain %q", env.Errors.Message, tt.message)
			}
		})
	}
	// An apply is audited like every other admin mutation.
	if auditSkip("/api/v1/admin/setup/defaults") {
		t.Fatal("the apply is not audited")
	}
}
