package externalservice

import (
	"strings"
	"testing"
)

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

// Stage 60 sends "{{ x | int }}", which reaches the API as a string: read as 0, a site's rwx_gib silently meant no
// StorageClass, its worker_mtu the guest default on a 1450 vnet, and vlan_min/vlan_max no tags at all.
func TestIntOfTakesTheInstallersStrings(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int
	}{{float64(100), 100}, {100, 100}, {int64(100), 100}, {"100", 100}, {" 1450 ", 1450}, {"", 0}, {"lots", 0}, {nil, 0}, {true, 0}} {
		if got := intOf(c.in); got != c.want {
			t.Errorf("intOf(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
	es := &ExternalService{Config: map[string]any{"cluster": map[string]any{"rootVolumeGiB": "30", "workerMtu": "1450", "rwxGiB": "100"}}}
	d := es.KamajiConfig().Defaults
	if d.RootVolumeGiB != 30 || d.NodeGroupMTU != 1450 || d.RWXGiB != 100 {
		t.Errorf("defaults from strings: %+v", d)
	}
}

// The tenant-side controller's keys are named whatever their value, false or empty included: a config that still
// carries one is refused rather than silently doing nothing.
func TestKamajiRemovedKeys(t *testing.T) {
	for _, c := range []struct {
		cluster map[string]any
		want    string
	}{
		{nil, ""},
		{map[string]any{"pveApiUrl": "https://192.0.2.11:8006"}, ""},
		{map[string]any{"csiSharedStores": false}, "csiSharedStores"},
		{map[string]any{"pveCsiUrl": ""}, "pveCsiUrl"},
		{map[string]any{"csiSharedStores": true, "pveCsiUrl": "https://192.0.2.60:8006"}, "csiSharedStores,pveCsiUrl"},
	} {
		cfg := map[string]any{}
		if c.cluster != nil {
			cfg["cluster"] = c.cluster
		}
		if got := strings.Join(KamajiRemovedKeys(cfg), ","); got != c.want {
			t.Errorf("%v: %q, want %q", c.cluster, got, c.want)
		}
	}
}
