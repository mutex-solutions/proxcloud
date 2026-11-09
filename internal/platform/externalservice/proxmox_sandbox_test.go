package externalservice

import (
	"slices"
	"testing"
	"time"
)

// The isolation's sandbox endpoints are the VM firewall's: registry, build storage, portal, port 443 when unset.
func TestPVESandboxEndpoints(t *testing.T) {
	if got := (&ExternalService{Config: map[string]any{}}).PVESandboxEndpoints(); got != nil {
		t.Fatalf("no sandbox pool: %v", got)
	}
	es := &ExternalService{Config: map[string]any{"sandbox": map[string]any{"pool": "proxcloud-sandbox",
		"registry":     map[string]any{"address": "10.89.0.227"},
		"buildStorage": map[string]any{"address": "10.88.0.41", "port": float64(9000)},
		"portal":       map[string]any{"address": "10.88.0.10", "port": float64(19459)}}}}
	if got, want := es.PVESandboxEndpoints(), []string{"10.89.0.227:443", "10.88.0.41:9000", "10.88.0.10:19459"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

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
	es.Config["sandbox"].(map[string]any)["proxy"] = map[string]any{"address": "192.0.2.200",
		"entry": map[string]any{"vmid": float64(9110), "pool": "proxcloud-sandbox-entry", "address": "192.0.2.5"}}
	if p := func() PVESandboxProxy { c, _ := es.PVESandboxConfig(); return c.Proxy }(); p != (PVESandboxProxy{Address: "192.0.2.200",
		Port: 443, EntryVMID: 9110, EntryPool: "proxcloud-sandbox-entry", EntryAddress: "192.0.2.5"}) {
		t.Fatalf("proxy: %+v", p)
	}
	es.Config["sandbox"].(map[string]any)["registry"] = map[string]any{"address": "198.51.100.226", "port": float64(443)}
	if c, _ := es.PVESandboxConfig(); c.Registry != (PVESandboxRegistry{Address: "198.51.100.226", Port: 443}) {
		t.Fatalf("registry: %+v", c.Registry)
	}
}
