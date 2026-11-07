package externalservice

import (
	"testing"
	"time"
)

func TestPVESandboxConfig(t *testing.T) {
	if _, ok := (&ExternalService{Config: map[string]any{}}).PVESandboxConfig(); ok {
		t.Fatal("no sandbox pool, no sandbox pass")
	}
	es := &ExternalService{
		Config: map[string]any{"sandbox": map[string]any{
			"pool": "proxcloud-sandbox", "nodes": []any{"192.0.2.10-192.0.2.29"}, "loadBalancers": []any{"192.0.2.200-192.0.2.209"},
			"dns": []any{"1.1.1.1"}, "kubeletSources": []any{"198.51.100.0/24"}, "snat": "203.0.113.50", "unsecuredMinAgeMinutes": float64(30),
		}},
		Secret: map[string]any{"sandboxKubeconfig": "apiVersion: v1"},
	}
	cfg, ok := es.PVESandboxConfig()
	if !ok || cfg.Pool != "proxcloud-sandbox" || len(cfg.Nodes) != 1 || len(cfg.LoadBalancers) != 1 || cfg.DNS[0] != "1.1.1.1" ||
		cfg.KubeletSources[0] != "198.51.100.0/24" || cfg.UnsecuredMinAge != 30*time.Minute || cfg.Kubeconfig != "apiVersion: v1" || cfg.SNAT != "203.0.113.50" {
		t.Fatalf("got %+v", cfg)
	}
	delete(es.Config["sandbox"].(map[string]any), "unsecuredMinAgeMinutes")
	if cfg, _ := es.PVESandboxConfig(); cfg.UnsecuredMinAge != 20*time.Minute {
		t.Fatalf("default min age: %v (CABPK's bootstrap token TTL plus a margin)", cfg.UnsecuredMinAge)
	}
}
