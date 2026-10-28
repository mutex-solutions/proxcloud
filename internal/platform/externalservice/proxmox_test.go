package externalservice

import (
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
)

func TestPVENetworkConfigIsolation(t *testing.T) {
	tests := []struct {
		name     string
		network  map[string]any
		wantMode string
		wantCtl  string
	}{
		{"absent defaults to shared", map[string]any{}, pve.IsolationShared, ""},
		{"unknown falls back to shared", map[string]any{"isolation": "none"}, pve.IsolationShared, ""},
		{"evpn per project", map[string]any{"isolation": "evpn-per-project", "evpnController": "pcx"}, pve.IsolationEVPN, "pcx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := (&ExternalService{Config: map[string]any{"network": tt.network}}).PVENetworkConfig()
			if n.Isolation != tt.wantMode || n.EVPNController != tt.wantCtl {
				t.Fatalf("isolation=%q controller=%q, want %q %q", n.Isolation, n.EVPNController, tt.wantMode, tt.wantCtl)
			}
		})
	}
}

func TestPVENetworkConfigPublicPool(t *testing.T) {
	n := (&ExternalService{Config: map[string]any{"network": map[string]any{
		"publicNetworks": []any{
			map[string]any{"id": "vmbr1", "name": "Internet", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1",
				"ranges": []any{"203.0.113.10-203.0.113.20"}, "exclude": []any{"203.0.113.15"}},
			map[string]any{"id": "vmbr2"},
		},
	}}}).PVENetworkConfig()
	if len(n.PublicNetworks) != 2 {
		t.Fatalf("got %d public networks", len(n.PublicNetworks))
	}
	pn := n.PublicNetworks[0]
	if pn.CIDR != "203.0.113.0/24" || pn.Gateway != "203.0.113.1" ||
		!reflect.DeepEqual(pn.Ranges, []string{"203.0.113.10-203.0.113.20"}) || !reflect.DeepEqual(pn.Exclude, []string{"203.0.113.15"}) {
		t.Fatalf("pool not parsed: %+v", pn)
	}
	if bare := n.PublicNetworks[1]; bare.Name != "vmbr2" || bare.CIDR != "" || len(bare.Ranges) != 0 {
		t.Fatalf("bare network: %+v", bare)
	}
}

func TestPVEDefaultQuota(t *testing.T) {
	gpu := map[string]any{"*": float64(1)}
	tests := []struct {
		name  string
		quota any
		want  map[string]any
	}{
		{"absent", nil, nil},
		{"empty", map[string]any{}, nil},
		{"proxmox keys", map[string]any{"servers": float64(5), "vcpus": float64(10), "ramMb": float64(8192),
			"diskGb": float64(100), "networks": float64(2), "floatingIps": float64(1), "gpu": gpu},
			map[string]any{"servers": 5, "vcpus": 10, "ramMb": 8192, "diskGb": 100, "networks": 2, "floatingIps": 1, "gpu": gpu}},
		{"openstack aliases", map[string]any{"instances": float64(3), "cores": float64(6), "ram": float64(4096),
			"gigabytes": float64(50), "floatingips": float64(1), "volumes": float64(9), "ports": float64(20)},
			map[string]any{"servers": 3, "vcpus": 6, "ramMb": 4096, "diskGb": 50, "floatingIps": 1}},
		{"proxmox key wins over alias", map[string]any{"cores": float64(6), "vcpus": float64(12)}, map[string]any{"vcpus": 12}},
		{"unlimited and junk dropped", map[string]any{"cores": float64(-1), "ramMb": "lots", "networks": float64(0)}, map[string]any{"networks": 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{}
			if tt.quota != nil {
				cfg["provisioning"] = map[string]any{"quota": tt.quota}
			}
			got := (&ExternalService{Config: cfg}).PVEDefaultQuota()
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}
