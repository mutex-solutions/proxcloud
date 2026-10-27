package externalservice

import (
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
)

func TestDbaasConfig(t *testing.T) {
	es := &ExternalService{
		ID: "svc-dbaas", Type: TypeCloud,
		Config: map[string]any{
			"provider": "dbaas",
			"region":   "RegionOne",
			"regions":  map[string]any{"RegionOne": map[string]any{}},
			"services": map[string]any{"database": map[string]any{"RegionOne": true}},
			"argocd": map[string]any{
				"project":      "proxcloud-dbaas",
				"chartRepo":    "ghcr.io/mutex-solutions/charts",
				"chartVersion": "0.1.0",
			},
			"database": map[string]any{
				"osServiceId":    "svc-os",
				"osProjectId":    "dbaas-tenant",
				"memberSubnetId": "member-sub",
				"storageClasses": []any{"nvme"},
				"limits":         map[string]any{"maxCpu": float64(32), "maxMemoryGiB": float64(128), "maxStorageGiB": float64(2048)},
				"engines": map[string]any{
					"postgresql": map[string]any{"versions": []any{"16", "17"}, "default": "17", "replicas": []any{float64(1), float64(3)}},
					"valkey":     map[string]any{"versions": []any{"9"}, "default": "9", "beta": true},
					"broken":     map[string]any{"default": "1"}, // no versions → dropped
				},
			},
		},
		Secret: map[string]any{"kubeconfig": "kc-yaml"},
	}
	if !es.IsDbaas() {
		t.Fatal("IsDbaas")
	}
	if es.DbaasRegion() != "RegionOne" {
		t.Fatal("DbaasRegion")
	}
	cfg := es.DbaasConfig()
	if cfg.Kubeconfig != "kc-yaml" || cfg.OSServiceID != "svc-os" || cfg.OSProjectID != "dbaas-tenant" || cfg.MemberSubnetID != "member-sub" {
		t.Fatalf("cfg = %+v", cfg)
	}
	// Defaults fill in.
	if cfg.ArgoNamespace != "argocd" || cfg.ChartName != "database-cluster" {
		t.Fatalf("defaults: ns=%q chart=%q", cfg.ArgoNamespace, cfg.ChartName)
	}
	if cfg.Limits != (dbaas.Limits{MaxCPU: 32, MaxMemoryGiB: 128, MaxStorageGiB: 2048}) {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	pg, ok := cfg.Engines["postgresql"]
	if !ok || len(pg.Versions) != 2 || pg.Default != "17" || len(pg.Replicas) != 2 || pg.Beta {
		t.Fatalf("postgresql offer = %+v", pg)
	}
	if vk := cfg.Engines["valkey"]; !vk.Beta {
		t.Fatal("valkey beta flag lost")
	}
	if _, has := cfg.Engines["broken"]; has {
		t.Fatal("version-less engine must be dropped")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// A kamaji doc is not dbaas.
	other := &ExternalService{Config: map[string]any{"provider": "kamaji"}}
	if other.IsDbaas() {
		t.Fatal("kamaji doc must not be dbaas")
	}
}
