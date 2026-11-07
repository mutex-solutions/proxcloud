package project

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
)

// Addresses come from the top of the range, the taken ones skipped, and a range too small is refused.
func TestFreeAddresses(t *testing.T) {
	pn := externalservice.PVEPublicNetwork{ID: "v1", CIDR: "192.0.2.0/24", Gateway: "192.0.2.1", Ranges: []string{"192.0.2.2-192.0.2.6"}}
	got, prefix, herr := freeAddresses(pn, []string{"192.0.2.6"}, 2)
	if herr != nil || prefix != 24 {
		t.Fatalf("freeAddresses: %v prefix %d", herr, prefix)
	}
	if len(got) != 2 || got[0] != "192.0.2.5" || got[1] != "192.0.2.4" {
		t.Errorf("got %v, want the two highest free", got)
	}
	if _, _, herr := freeAddresses(pn, nil, 6); herr == nil {
		t.Error("6 addresses out of 5: want a refusal")
	}
}

// The CSI config is a manifest inside a Secret inside a ClusterResourceSet: it must survive as YAML
// and carry the provider mode that resolves a CAPMOX machine's provider id.
func TestKamajiCSIManifest(t *testing.T) {
	m := kamajiCSIManifest("https://192.0.2.60:8006/", "csi@pve!stc-1", `se"cret`, "r1")
	for _, want := range []string{"kind: Namespace", "name: proxmox-csi-plugin", "namespace: csi-proxmox", "config.yaml: "} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest lacks %q:\n%s", want, m)
		}
	}
	line := m[strings.Index(m, "config.yaml: ")+len("config.yaml: "):]
	var raw string
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &raw); err != nil {
		t.Fatalf("config.yaml is not one quoted scalar: %v", err)
	}
	var cfg struct {
		Features struct {
			Provider string `json:"provider"`
		} `json:"features"`
		Clusters []struct {
			URL         string `json:"url"`
			TokenID     string `json:"token_id"`
			TokenSecret string `json:"token_secret"`
			Region      string `json:"region"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Features.Provider != "capmox" || len(cfg.Clusters) != 1 {
		t.Fatalf("cfg = %+v", cfg)
	}
	c := cfg.Clusters[0]
	if c.URL != "https://192.0.2.60:8006/api2/json" || c.TokenID != "csi@pve!stc-1" || c.TokenSecret != `se"cret` || c.Region != "r1" {
		t.Errorf("cluster = %+v", c)
	}
}

// Workers count against the quota like servers, and an edit asks only for its difference.
func TestKamajiWorkerDemand(t *testing.T) {
	flavor := func(id string) (*pveflavor.Flavor, error) {
		if id == "gone" {
			return nil, nil
		}
		return &pveflavor.Flavor{VCPUs: 2, RAMMb: 2048, DiskGb: 20}, nil
	}
	got, err := kamajiWorkerDemand([]kamaji.NodeGroup{{FlavorID: "a", Count: 3}, {FlavorID: "a", Count: 1, RootVolumeGiB: 50}}, nil, 30, flavor)
	if err != nil {
		t.Fatal(err)
	}
	if want := (pveQuotaDemand{Servers: 4, VCPUs: 8, RAMMb: 8192, DiskGb: 140}); got != want {
		t.Errorf("create: %+v, want %+v", got, want)
	}
	got, _ = kamajiWorkerDemand([]kamaji.NodeGroup{{FlavorID: "a", Count: 3}}, []kamaji.NodeGroup{{FlavorID: "a", Count: 2}, {FlavorID: "gone", Count: 9}}, 30, flavor)
	if want := (pveQuotaDemand{Servers: 1, VCPUs: 2, RAMMb: 2048, DiskGb: 30}); got != want {
		t.Errorf("scale up: %+v, want %+v", got, want)
	}
	got, _ = kamajiWorkerDemand([]kamaji.NodeGroup{{FlavorID: "a", Count: 1}}, []kamaji.NodeGroup{{FlavorID: "a", Count: 3}}, 30, flavor)
	if got.Servers >= 0 || got.VCPUs >= 0 {
		t.Errorf("scale down must ask for nothing: %+v", got)
	}
}
