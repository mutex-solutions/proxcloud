package externalservice

import "testing"

func TestKamajiConfig(t *testing.T) {
	es := &ExternalService{
		ID:   "svc-1",
		Type: TypeCloud,
		Config: map[string]any{
			"provider": "kamaji",
			"regions":  map[string]any{"az1": map[string]any{}},
			"argocd": map[string]any{
				"chartRepo":    "ghcr.io/mutex-solutions/charts",
				"chartName":    "kamaji-cluster",
				"chartVersion": "0.2.3",
				"project":      "proxcloud-k8s",
			},
			"cluster": map[string]any{
				"dataStoreName": "default",
				"dnsZone":       "k8s.example.com",
				"versions":      map[string]any{"1.35.4": "img-1"},
				"imageVariants": map[string]any{
					"nvidia": map[string]any{"1.35.4": "img-nv"},
					"empty":  map[string]any{"1.35.4": ""}, // blank ids are dropped → variant dropped
					"junk":   "not-a-map",                  // malformed entries are skipped
				},
			},
		},
		Secret: map[string]any{"kubeconfig": "KC"},
	}
	if !es.IsKamaji() {
		t.Fatal("IsKamaji = false")
	}
	cfg := es.KamajiConfig()
	if cfg.Kubeconfig != "KC" || cfg.Region != "az1" {
		t.Errorf("kubeconfig/region = %q/%q", cfg.Kubeconfig, cfg.Region)
	}
	// Defaults fill in when the doc omits them.
	if cfg.ArgoNamespace != "argocd" || cfg.ChartName != "kamaji-cluster" {
		t.Errorf("namespace default/chart = %q/%q", cfg.ArgoNamespace, cfg.ChartName)
	}
	if cfg.ArgoProject != "proxcloud-k8s" || cfg.ChartVersion != "0.2.3" {
		t.Errorf("argo = %q/%q", cfg.ArgoProject, cfg.ChartVersion)
	}
	if cfg.Defaults.Versions["1.35.4"] != "img-1" || cfg.Defaults.DNSZone != "k8s.example.com" {
		t.Errorf("cluster defaults = %+v", cfg.Defaults)
	}
	if cfg.Defaults.ImageVariants["nvidia"]["1.35.4"] != "img-nv" || len(cfg.Defaults.ImageVariants) != 1 {
		t.Errorf("imageVariants = %+v", cfg.Defaults.ImageVariants)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// A proxmox provider is not kamaji; a kamaji doc without chart pin or chart name fails
	// validation (no default chart until the Proxmox cluster chart lands).
	pve := &ExternalService{Config: map[string]any{"provider": "proxmox"}}
	if pve.IsKamaji() {
		t.Error("proxmox IsKamaji = true")
	}
	unpinned := es.KamajiConfig()
	unpinned.ChartVersion = ""
	if err := unpinned.Validate(); err == nil {
		t.Error("unpinned chart version must fail validation (plan §9: never latest)")
	}
	unnamed := es.KamajiConfig()
	unnamed.ChartName = ""
	if err := unnamed.Validate(); err == nil {
		t.Error("missing chart name must fail validation")
	}
}
