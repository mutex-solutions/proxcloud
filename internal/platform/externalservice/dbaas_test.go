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
	if cfg.Kubeconfig != "kc-yaml" || len(cfg.StorageClasses) != 1 || cfg.StorageClasses[0] != "nvme" {
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

// TestDbaasConfigNetworkBackupScheduling pins the provider document shape for the MetalLB pool,
// the S3 backup store (credentials ride the provider SECRET), the zone spread key and the
// storage-class allowlist — which never offers the capacity tier.
func TestDbaasConfigNetworkBackupScheduling(t *testing.T) {
	es := &ExternalService{
		ID: "svc-dbaas", Type: TypeCloud,
		Config: map[string]any{
			"provider": "dbaas",
			"argocd":   map[string]any{"chartRepo": "registry.example.com/charts", "chartVersion": "0.5.0"},
			"database": map[string]any{
				"lbAddressPool":  "dbaas-services",
				"storageClasses": []any{"nvme", "capacity", "capacity-nfs", "ssd"},
				"scheduling": map[string]any{
					"zoneKey":      "kubernetes.io/hostname",
					"nodeSelector": map[string]any{"node-role": "database"},
					"tolerations":  []any{map[string]any{"key": "dedicated", "operator": "Exists"}},
				},
				"backup": map[string]any{
					"endpoint": "http://s3.example.com:9000", "bucket": "dbaas-backups", "prefix": "backups",
					"region": "us-east-1", "caCertSecret": "s3-ca",
				},
				"engines": map[string]any{"postgresql": map[string]any{"versions": []any{"17"}}},
			},
		},
		Secret: map[string]any{"kubeconfig": "kc", "backupAccessKey": "AK", "backupSecretKey": "SK"},
	}
	cfg := es.DbaasConfig()
	if cfg.LBAddressPool != "dbaas-services" {
		t.Errorf("LBAddressPool = %q", cfg.LBAddressPool)
	}
	if got := cfg.StorageClasses; len(got) != 2 || got[0] != "nvme" || got[1] != "ssd" {
		t.Errorf("StorageClasses = %v, want the capacity tier filtered out", got)
	}
	if cfg.ZoneKey != "kubernetes.io/hostname" || cfg.NodeSelector["node-role"] != "database" || len(cfg.Tolerations) != 1 {
		t.Errorf("scheduling = %q %v %v", cfg.ZoneKey, cfg.NodeSelector, cfg.Tolerations)
	}
	want := dbaas.BackupConfig{
		Endpoint: "http://s3.example.com:9000", Bucket: "dbaas-backups", Prefix: "backups", Region: "us-east-1",
		PathStyle: true, CACertSecret: "s3-ca", AccessKey: "AK", SecretKey: "SK",
	}
	if cfg.Backup != want || !cfg.Backup.Enabled() {
		t.Errorf("Backup = %+v", cfg.Backup)
	}
	// pathStyle:false is honoured; no backup block = backups off.
	es.Config["database"].(map[string]any)["backup"].(map[string]any)["pathStyle"] = false
	if es.DbaasConfig().Backup.PathStyle {
		t.Error("explicit pathStyle:false lost")
	}
	delete(es.Config["database"].(map[string]any), "backup")
	if es.DbaasConfig().Backup.Enabled() {
		t.Error("no backup block must leave backups off")
	}
}
