package kamaji

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// These tests pin the distinction between retaining an existing ingress-nginx installation
// and opting into Gateway Fabric. An ordinary add-on edit must never become a migration.
func TestChartHasGatewayFabric(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.1.9", false},
		{"0.0.99", false},
		{"0.1.10", true},
		{"v0.1.10", true},
		{"0.1.10-rc.1", true}, // same numeric-version policy as ChartHasGatewayCSI
		{"0.1.10+build.1", true},
		{"0.1.11", true},
		{"0.2.0", true},
		{"1.0.0", true},
		{"", false},
		{"latest", false},
		{"0.1", false},
		{"0.1.x", false},
		{"0.1.10.1", false},
		{"-1.2.3", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			if got := ChartHasGatewayFabric(tc.version); got != tc.want {
				t.Errorf("ChartHasGatewayFabric(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

func TestValidateCreateAddonsGatewayCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		picks   map[string]bool
		wantErr bool
	}{
		{"old chart defaults", "0.1.9", nil, false},
		{"new chart defaults", "0.1.10", nil, false},
		{"old client false ingress", "0.1.10", map[string]bool{"ingress": false}, false},
		{"old client false ingress on old chart", "0.1.9", map[string]bool{"ingress": false}, false},
		{"no new legacy installation", "0.1.10", map[string]bool{"ingress": true}, true},
		{"no new legacy installation on old pin", "0.1.9", map[string]bool{"ingress": true}, true},
		{"gateway opt in", "0.1.10", map[string]bool{"nginxGatewayFabric": true}, false},
		{"gateway opt in later version", "0.2.0", map[string]bool{"nginxGatewayFabric": true}, false},
		{"gateway and harmless old client key", "0.1.10", map[string]bool{"ingress": false, "nginxGatewayFabric": true}, false},
		{"old chart cannot enable gateway", "0.1.9", map[string]bool{"nginxGatewayFabric": true}, true},
		{"false gateway on old chart is harmless", "0.1.9", map[string]bool{"nginxGatewayFabric": false}, false},
		{"malformed pin cannot enable gateway", "latest", map[string]bool{"nginxGatewayFabric": true}, true},
		{"legacy is not an alias", "0.1.10", map[string]bool{"ingress": true, "nginxGatewayFabric": true}, true},
		{"unknown enabled key", "0.1.10", map[string]bool{"credentialPush": true}, true},
		{"unknown disabled key", "0.1.10", map[string]bool{"credentialPush": false}, true},
		{"ordinary picks", "0.1.10", map[string]bool{"certManager": true, "metricsServer": false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := gatewayAddonSnapshot(t, tc.picks)
			err := ValidateCreateAddons(tc.version, tc.picks)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateCreateAddons(%q, %v) = %v, wantErr %v", tc.version, tc.picks, err, tc.wantErr)
			}
			if after := gatewayAddonSnapshot(t, tc.picks); after != before {
				t.Error("validation mutated the caller's picks")
			}
		})
	}
}

func TestGatewayCreateIgnoresOldClientFalseIngressKey(t *testing.T) {
	cfg, spec := testCfg(), testSpec()
	cfg.ChartVersion = "0.1.10"
	spec.Addons = map[string]bool{"ingress": false, "metricsServer": false, "nginxGatewayFabric": true}
	if err := ValidateCreateAddons(cfg.ChartVersion, spec.Addons); err != nil {
		t.Fatal(err)
	}
	values := BuildValues(cfg, spec)
	addons, _ := values["addons"].(map[string]any)
	if _, ok := addons["ingress"]; ok {
		t.Error("accepted old-client ingress:false must not create a new legacy values block")
	}
	if dig(addons, "metricsServer", "enabled") != false || dig(addons, "nginxGatewayFabric", "enabled") != true {
		t.Errorf("ignoring the compatibility key changed modern picks: %#v", addons)
	}
}

func TestMergeAddonValuesSparsePicksAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current map[string]any
		picks   map[string]bool
		want    map[string]any
	}{
		{
			name: "defaults are a complete modern dataset",
			want: map[string]any{
				"metricsServer":      map[string]any{"enabled": true},
				"certManager":        map[string]any{"enabled": false},
				"nginxGatewayFabric": map[string]any{"enabled": false},
			},
		},
		{
			name: "old client metrics edit retains enabled gateway",
			current: map[string]any{
				"certManager":        map[string]any{"enabled": true},
				"nginxGatewayFabric": map[string]any{"enabled": true},
			},
			picks: map[string]bool{"metricsServer": false, "ingress": false},
			want: map[string]any{
				"metricsServer":      map[string]any{"enabled": false},
				"certManager":        map[string]any{"enabled": true},
				"nginxGatewayFabric": map[string]any{"enabled": true},
			},
		},
		{
			name: "enable gateway explicitly without resetting metrics",
			current: map[string]any{
				"metricsServer": map[string]any{"enabled": false},
			},
			picks: map[string]bool{"nginxGatewayFabric": true},
			want: map[string]any{
				"metricsServer":      map[string]any{"enabled": false},
				"certManager":        map[string]any{"enabled": false},
				"nginxGatewayFabric": map[string]any{"enabled": true},
			},
		},
		{
			name: "explicit false stays false",
			current: map[string]any{
				"nginxGatewayFabric": map[string]any{"enabled": true},
			},
			picks: map[string]bool{"metricsServer": false, "nginxGatewayFabric": false},
			want: map[string]any{
				"metricsServer":      map[string]any{"enabled": false},
				"certManager":        map[string]any{"enabled": false},
				"nginxGatewayFabric": map[string]any{"enabled": false},
			},
		},
		{
			name: "operator and credential-push entries do not round trip",
			current: map[string]any{
				"metricsServer":      map[string]any{"enabled": false, "operatorOnly": "discard"},
				"certManager":        map[string]any{"enabled": true, "operatorOnly": "discard"},
				"nginxGatewayFabric": map[string]any{"enabled": false, "operatorOnly": "discard"},
				"credentialPush":     map[string]any{"enabled": true, "secret": "discard"},
				"operatorOnly":       map[string]any{"enabled": true},
			},
			picks: map[string]bool{"certManager": false},
			want: map[string]any{
				"metricsServer":      map[string]any{"enabled": false},
				"certManager":        map[string]any{"enabled": false},
				"nginxGatewayFabric": map[string]any{"enabled": false},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCurrent, beforePicks := gatewayAddonSnapshot(t, tc.current), gatewayAddonSnapshot(t, tc.picks)
			got, err := MergeAddonValues("0.1.10", tc.current, tc.picks)
			if err != nil {
				t.Fatalf("MergeAddonValues: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("merged add-ons = %#v, want %#v", got, tc.want)
			}
			if gatewayAddonSnapshot(t, tc.current) != beforeCurrent || gatewayAddonSnapshot(t, tc.picks) != beforePicks {
				t.Error("merge mutated its input maps")
			}
		})
	}
}

func TestMergeAddonValuesPreservesLegacyRawBlock(t *testing.T) {
	for _, version := range []string{"0.1.9", "0.1.10"} {
		for _, explicit := range []bool{false, true} {
			t.Run(version+map[bool]string{false: "/omitted", true: "/unchanged-old-client"}[explicit], func(t *testing.T) {
				legacy := map[string]any{
					"enabled":      true,
					"controller":   map[string]any{"service": map[string]any{"loadBalancerIP": "192.0.2.10"}},
					"operatorNote": []any{"keep", "exactly"},
				}
				current := map[string]any{
					"ingress":        legacy,
					"certManager":    map[string]any{"enabled": true},
					"credentialPush": map[string]any{"enabled": true},
				}
				picks := map[string]bool{"metricsServer": false}
				if explicit {
					picks["ingress"] = true
				}
				before := gatewayAddonSnapshot(t, current)
				got, err := MergeAddonValues(version, current, picks)
				if err != nil {
					t.Fatalf("MergeAddonValues: %v", err)
				}
				if !reflect.DeepEqual(got["ingress"], legacy) {
					t.Errorf("ordinary edit changed legacy block: got %#v, want %#v", got["ingress"], legacy)
				}
				if dig(got, "metricsServer", "enabled") != false || dig(got, "certManager", "enabled") != true {
					t.Errorf("ordinary picks/defaults were lost: %#v", got)
				}
				if _, ok := got["credentialPush"]; ok {
					t.Error("preserving legacy ingress must not preserve credential-push entries")
				}
				if _, ok := got["nginxGatewayFabric"]; ok && dig(got, "nginxGatewayFabric", "enabled") != false {
					t.Error("retaining legacy ingress silently enabled Gateway Fabric")
				}
				if gatewayAddonSnapshot(t, current) != before {
					t.Error("merge mutated the existing legacy values")
				}
			})
		}
	}
}

func TestMergeAddonValuesOldChartFalseGatewayIsHarmless(t *testing.T) {
	got, err := MergeAddonValues("0.1.9", nil, map[string]bool{"nginxGatewayFabric": false, "ingress": false})
	if err != nil {
		t.Fatalf("disabled unsupported option must not block an old client: %v", err)
	}
	if dig(got, "metricsServer", "enabled") != true || dig(got, "certManager", "enabled") != false {
		t.Errorf("default add-ons = %#v", got)
	}
	if _, ok := got["nginxGatewayFabric"]; ok && dig(got, "nginxGatewayFabric", "enabled") != false {
		t.Error("false unsupported pick must never enable a gateway")
	}
	if _, ok := got["ingress"]; ok {
		t.Error("an old client's ingress:false must not create a legacy block")
	}
}

func TestMergeAddonValuesRefusesUnsafeChangesWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		current map[string]any
		picks   map[string]bool
	}{
		{"unsupported gateway", "0.1.9", nil, map[string]bool{"nginxGatewayFabric": true}},
		{"unknown enabled pick", "0.1.10", nil, map[string]bool{"credentialPush": true}},
		{"unknown disabled pick", "0.1.10", nil, map[string]bool{"credentialPush": false}},
		{"new legacy enable", "0.1.10", nil, map[string]bool{"ingress": true}},
		{"disabled legacy cannot be reenabled", "0.1.10", map[string]any{"ingress": map[string]any{"enabled": false}}, map[string]bool{"ingress": true}},
		{"legacy disable requires migration", "0.1.10", map[string]any{"ingress": map[string]any{"enabled": true}}, map[string]bool{"ingress": false}},
		{"gateway cannot migrate legacy", "0.1.10", map[string]any{"ingress": map[string]any{"enabled": true}}, map[string]bool{"nginxGatewayFabric": true}},
		{"existing dual enable is invalid", "0.1.10", map[string]any{"ingress": map[string]any{"enabled": true}, "nginxGatewayFabric": map[string]any{"enabled": true}}, map[string]bool{"metricsServer": false}},
		{"old pin cannot silently retain an enabled gateway", "0.1.9", map[string]any{"nginxGatewayFabric": map[string]any{"enabled": true}}, map[string]bool{"certManager": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeCurrent, beforePicks := gatewayAddonSnapshot(t, tc.current), gatewayAddonSnapshot(t, tc.picks)
			if _, err := MergeAddonValues(tc.version, tc.current, tc.picks); err == nil {
				t.Error("unsafe add-on change must fail")
			}
			if gatewayAddonSnapshot(t, tc.current) != beforeCurrent || gatewayAddonSnapshot(t, tc.picks) != beforePicks {
				t.Error("rejected merge mutated input state")
			}
		})
	}
}

func TestMergeAddonValuesMalformedKnownBlocksFailClosed(t *testing.T) {
	for _, name := range []string{"metricsServer", "certManager", "nginxGatewayFabric", "ingress"} {
		for _, raw := range []any{nil, true, "enabled", map[string]any{}, map[string]any{"enabled": "true"}, map[string]any{"enabled": nil}} {
			t.Run(name+"/"+gatewayAddonSnapshot(t, raw), func(t *testing.T) {
				current := map[string]any{name: raw}
				before := gatewayAddonSnapshot(t, current)
				// Even a valid request must not guess what a malformed live enabled value meant.
				if _, err := MergeAddonValues("0.1.10", current, map[string]bool{"metricsServer": false}); err == nil {
					t.Errorf("malformed %s block %#v must fail closed", name, raw)
				}
				if gatewayAddonSnapshot(t, current) != before {
					t.Error("rejected malformed block was modified")
				}
			})
		}
	}
}

func TestCreateClusterGatewayValidationPrecedesManagementWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		picks   map[string]bool
	}{
		{"unsupported gateway", "0.1.9", map[string]bool{"nginxGatewayFabric": true}},
		{"new legacy selection", "0.1.10", map[string]bool{"ingress": true}},
		{"unknown disabled selection", "0.1.10", map[string]bool{"credentialPush": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI()
			cfg := testCfg()
			cfg.ChartVersion = tc.version
			svc := NewWithAPI(api, cfg, "svc-1")
			spec := testSpec()
			spec.Addons = tc.picks
			if _, err := svc.CreateCluster(context.Background(), spec, ClusterSecrets{Cloud: map[string]string{"token": "unused"}}); err == nil {
				t.Error("invalid add-ons must fail before provisioning")
			}
			if len(api.namespaces) != 0 || len(api.secrets) != 0 || len(api.apps) != 0 {
				t.Errorf("rejected create performed management writes: namespaces=%d secrets=%d apps=%d", len(api.namespaces), len(api.secrets), len(api.apps))
			}
		})
	}
}

func TestGatewayPlatformBumpPreservesLegacyValuesExactly(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg := testCfg()
	cfg.ChartVersion = "0.1.10"
	spec := testSpec()
	values := BuildValues(cfg, spec)
	values["addons"] = map[string]any{
		"ingress":       map[string]any{"enabled": true, "operatorNote": "retain verbatim"},
		"metricsServer": map[string]any{"enabled": false},
	}
	// Existing legacy installations are seeded directly, not recreated through the new API.
	api.apps["argocd/"+spec.ID] = BuildApplication(cfg, spec, "svc-1", "0.1.9", values)
	before := gatewayAddonSnapshot(t, values)
	if err := NewWithAPI(api, cfg, "svc-1").SetChartVersion(context.Background(), spec.ID, "0.1.10"); err != nil {
		t.Fatalf("legacy platform bump: %v", err)
	}
	app := api.apps["argocd/"+spec.ID]
	if api.applies != 1 || dig(app, "spec", "source", "targetRevision") != "0.1.10" {
		t.Errorf("platform bump did not apply only the requested pin: applies=%d", api.applies)
	}
	if got := gatewayAddonSnapshot(t, dig(app, "spec", "source", "helm", "valuesObject")); got != before {
		t.Error("platform bump changed the legacy values or enabled a replacement gateway")
	}
}

func TestGatewayPlatformDowngradeFailsBeforeApply(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg := testCfg()
	cfg.ChartVersion = "0.1.10"
	spec := testSpec()
	spec.Addons = map[string]bool{"nginxGatewayFabric": true}
	app := BuildApplication(cfg, spec, "svc-1", "0.1.10", BuildValues(cfg, spec))
	api.apps["argocd/"+spec.ID] = app
	before := gatewayAddonSnapshot(t, app)
	if err := NewWithAPI(api, cfg, "svc-1").SetChartVersion(context.Background(), spec.ID, "0.1.9"); err == nil {
		t.Error("downgrading an enabled gateway to an unsupported chart must fail")
	}
	if api.applies != 0 || gatewayAddonSnapshot(t, app) != before {
		t.Error("rejected gateway downgrade changed the Application")
	}
}

func TestGatewaySyncKeepsLegacyAndModernStatesDistinct(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		legacy  bool
		gateway bool
	}{
		{"legacy before platform update", "0.1.9", true, false},
		{"legacy after platform update", "0.1.10", true, false},
		{"new gateway", "0.1.10", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, spec := testCfg(), testSpec()
			values := BuildValues(cfg, spec)
			values["addons"] = map[string]any{
				"ingress":            map[string]any{"enabled": tc.legacy, "operatorNote": "must not leak"},
				"nginxGatewayFabric": map[string]any{"enabled": tc.gateway},
				"metricsServer":      map[string]any{"enabled": false},
				"credentialPush":     map[string]any{"enabled": true},
			}
			app := BuildApplication(cfg, spec, "svc-1", tc.version, values)
			data := clusterData(app, nil, nil)
			got, ok := dig(data, "cluster", "addons").(map[string]any)
			if !ok || got["ingress"] != tc.legacy || got["nginxGatewayFabric"] != tc.gateway || got["metricsServer"] != false {
				t.Errorf("synced add-ons do not preserve their independent meanings: %#v", got)
			}
			for name := range got {
				if name != "ingress" && name != "nginxGatewayFabric" && name != "metricsServer" && name != "certManager" {
					t.Errorf("operator value %q leaked into the customer toggles", name)
				}
			}
		})
	}
}

func gatewayAddonSnapshot(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type gatewayAddonCountingAPI struct {
	*fakeAPI
	applies int
}

func (f *gatewayAddonCountingAPI) ApplyApplication(ctx context.Context, app map[string]any) error {
	f.applies++
	return f.fakeAPI.ApplyApplication(ctx, app)
}

// A refused legacy transition needs an actionable message, not a backend-outage error.
func TestGatewayLegacyChangeErrorNamesAddon(t *testing.T) {
	_, err := MergeAddonValues("0.1.10", map[string]any{"ingress": map[string]any{"enabled": true}}, map[string]bool{"ingress": false})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ingress") {
		t.Errorf("legacy change needs an actionable ingress error, got %v", err)
	}
}

// PatchAddons, not just its pure merge helper, must use the live Application pin.
// The provider may already advertise the new chart while this cluster retains 0.1.9.
func TestPatchAddonsKeepsActualPinAndLegacyValues(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg, spec := testCfg(), testSpec()
	cfg.ChartVersion = "0.1.10"
	values := BuildValues(cfg, spec)
	legacy := map[string]any{
		"enabled":      true,
		"controller":   map[string]any{"service": map[string]any{"loadBalancerIP": "192.0.2.10"}},
		"operatorNote": []any{"retain", "verbatim"},
	}
	values["addons"] = map[string]any{
		"ingress":       legacy,
		"metricsServer": map[string]any{"enabled": true},
		"certManager":   map[string]any{"enabled": false},
	}
	app := BuildApplication(cfg, spec, "svc-1", "0.1.9", values)
	api.apps["argocd/"+spec.ID] = app

	// The entire expected Application is compared, including all SSA-owned metadata
	// and spec fields; only its customer add-on block may differ.
	var want map[string]any
	if err := json.Unmarshal([]byte(gatewayAddonSnapshot(t, app)), &want); err != nil {
		t.Fatal(err)
	}
	wantValues := dig(want, "spec", "source", "helm", "valuesObject").(map[string]any)
	wantValues["addons"] = map[string]any{
		"ingress":       legacy,
		"metricsServer": map[string]any{"enabled": false},
		"certManager":   map[string]any{"enabled": false},
	}
	svc := NewWithAPI(api, cfg, "svc-1")
	if err := svc.PatchAddons(context.Background(), spec.ID, map[string]bool{"metricsServer": false}); err != nil {
		t.Fatalf("ordinary old-pin add-on edit: %v", err)
	}
	got := api.apps["argocd/"+spec.ID]
	if api.applies != 1 {
		t.Errorf("ordinary add-on edit applies = %d, want 1", api.applies)
	}
	if gatewayAddonSnapshot(t, got) != gatewayAddonSnapshot(t, want) {
		t.Error("ordinary add-on edit changed the live pin, legacy ingress, or non-addon Application fields")
	}
	if !reflect.DeepEqual(dig(got, "spec", "source", "helm", "valuesObject", "addons", "ingress"), legacy) {
		t.Error("PatchAddons did not preserve the raw legacy block exactly")
	}

	before := gatewayAddonSnapshot(t, got)
	if err := svc.PatchAddons(context.Background(), spec.ID, map[string]bool{"nginxGatewayFabric": true}); err == nil {
		t.Error("new provider pin must not implicitly migrate a legacy cluster")
	}
	if api.applies != 1 || gatewayAddonSnapshot(t, got) != before {
		t.Error("rejected legacy replacement applied or mutated the Application")
	}
}

func TestPatchAddonsRejectsGatewayUsingLivePinNotProviderPin(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg, spec := testCfg(), testSpec()
	cfg.ChartVersion = "0.1.10"
	values := BuildValues(cfg, spec)
	// No legacy conflict: rejection must come from the actual old chart capability.
	values["addons"] = map[string]any{"metricsServer": map[string]any{"enabled": true}}
	app := BuildApplication(cfg, spec, "svc-1", "0.1.9", values)
	api.apps["argocd/"+spec.ID] = app
	before := gatewayAddonSnapshot(t, app)
	err := NewWithAPI(api, cfg, "svc-1").PatchAddons(context.Background(), spec.ID, map[string]bool{"nginxGatewayFabric": true})
	if err == nil || !strings.Contains(err.Error(), "0.1.9") {
		t.Errorf("gateway enable must explain actual unsupported pin 0.1.9, got %v", err)
	}
	if api.applies != 0 || gatewayAddonSnapshot(t, app) != before {
		t.Error("unsupported actual pin was mutated or applied")
	}
}

func TestPatchAddonsOldClientEditRetainsEnabledGateway(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg, spec := testCfg(), testSpec()
	cfg.ChartVersion = "0.1.10"
	spec.Addons = map[string]bool{"nginxGatewayFabric": true, "certManager": true, "metricsServer": true}
	app := BuildApplication(cfg, spec, "svc-1", "0.1.10", BuildValues(cfg, spec))
	api.apps["argocd/"+spec.ID] = app
	var want map[string]any
	if err := json.Unmarshal([]byte(gatewayAddonSnapshot(t, app)), &want); err != nil {
		t.Fatal(err)
	}
	dig(want, "spec", "source", "helm", "valuesObject", "addons", "metricsServer").(map[string]any)["enabled"] = false
	// The old client has never heard of nginxGatewayFabric; its disabled legacy
	// compatibility key must not reset the enabled replacement controller.
	err := NewWithAPI(api, cfg, "svc-1").PatchAddons(context.Background(), spec.ID, map[string]bool{"metricsServer": false, "ingress": false})
	if err != nil {
		t.Fatalf("old-client sparse edit: %v", err)
	}
	if api.applies != 1 || gatewayAddonSnapshot(t, api.apps["argocd/"+spec.ID]) != gatewayAddonSnapshot(t, want) {
		t.Error("old-client edit reset Gateway Fabric or changed fields beyond the requested metrics toggle")
	}
}

func TestPatchAddonsMalformedStateAndKubernetesRejectBeforeApply(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		addons  any
		picks   map[string]bool
	}{
		{"null root", "1.35.4", nil, map[string]bool{"metricsServer": false}},
		{"scalar root", "1.35.4", "bad stored state", map[string]bool{"metricsServer": false}},
		{"list root", "1.35.4", []any{}, map[string]bool{"metricsServer": false}},
		{"malformed modern block", "1.35.4", map[string]any{"metricsServer": map[string]any{"enabled": "true"}}, map[string]bool{"certManager": true}},
		{"malformed legacy block", "1.35.4", map[string]any{"ingress": true}, map[string]bool{"metricsServer": false}},
		{"unsupported Kubernetes enable", "1.31.9", map[string]any{"metricsServer": map[string]any{"enabled": true}}, map[string]bool{"nginxGatewayFabric": true}},
		{"unsupported Kubernetes retained gateway", "1.31.9", map[string]any{"nginxGatewayFabric": map[string]any{"enabled": true}}, map[string]bool{"metricsServer": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
			cfg, spec := testCfg(), testSpec()
			cfg.ChartVersion = "0.1.10"
			values := BuildValues(cfg, spec)
			values["kubernetesVersion"] = tc.version
			values["addons"] = tc.addons
			app := BuildApplication(cfg, spec, "svc-1", "0.1.10", values)
			api.apps["argocd/"+spec.ID] = app
			before := gatewayAddonSnapshot(t, app)
			if err := NewWithAPI(api, cfg, "svc-1").PatchAddons(context.Background(), spec.ID, tc.picks); err == nil {
				t.Error("malformed state/unsupported Kubernetes must fail closed")
			}
			if api.applies != 0 || gatewayAddonSnapshot(t, app) != before {
				t.Error("rejected add-on edit mutated state or called ApplyApplication")
			}
		})
	}
}

func TestGatewayCSISwitchDowngradeRejectsBeforeSecrets(t *testing.T) {
	api := &gatewayAddonCountingAPI{fakeAPI: newFakeAPI()}
	cfg, spec := testCfg(), testSpec()
	cfg.ChartVersion = "0.1.10"
	spec.Addons = map[string]bool{"nginxGatewayFabric": true}
	app := BuildApplication(cfg, spec, "svc-1", "0.1.10", BuildValues(cfg, spec))
	api.apps["argocd/"+spec.ID] = app
	// Existing secrets must remain untouched too, not merely remain present.
	api.secrets["pc-p1/"+CSIGatewaySecretName(spec.ID)] = map[string]string{"config.yaml": "original gateway credential"}
	api.secrets["pc-p1/"+CSISecretName(spec.ID)] = map[string]string{"csi.yaml": "original tenant manifest"}
	beforeApp := gatewayAddonSnapshot(t, app)
	beforeSecrets := gatewayAddonSnapshot(t, api.secrets)
	beforeSecretObjects := gatewayAddonSnapshot(t, api.secretObjs)
	if !ChartHasGatewayCSI("0.1.9") {
		t.Fatal("fixture must support CSI so only the Gateway Fabric downgrade guard refuses it")
	}
	err := NewWithAPI(api, cfg, "svc-1").SwitchCSIGateway(context.Background(), spec.ProjectID, spec.ID, "0.1.9",
		map[string]string{"config.yaml": "must not be written"}, "must not replace tenant manifest")
	if err == nil {
		t.Error("CSI migration must not downgrade an enabled Gateway Fabric cluster")
	}
	if api.applies != 0 || gatewayAddonSnapshot(t, app) != beforeApp ||
		gatewayAddonSnapshot(t, api.secrets) != beforeSecrets || gatewayAddonSnapshot(t, api.secretObjs) != beforeSecretObjects {
		t.Error("rejected CSI downgrade wrote secrets or changed the Application")
	}
}
