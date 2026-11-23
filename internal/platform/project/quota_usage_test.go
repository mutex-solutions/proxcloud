package project

import (
	"reflect"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

func TestGPUQuotaLimits(t *testing.T) {
	got := gpuQuotaLimits(map[string]any{
		"gpu": map[string]any{
			"nvidia-a6000": float64(4),
			"h100":         int64(2),
			"*":            8,
			"fractional":   1.5,
			"negative":     -1,
			"invalid":      "3",
		},
	})
	want := map[string]int{"nvidia-a6000": 4, "h100": 2, "*": 8}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuQuotaLimits() = %#v, want %#v", got, want)
	}
}

func TestGPUQuotaLimitsAlwaysReturnsObject(t *testing.T) {
	got := gpuQuotaLimits(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("gpuQuotaLimits(nil) = %#v, want non-nil empty map", got)
	}
}

func TestGPUQuotaLimitsCanonicalizesLegacyAliases(t *testing.T) {
	got := gpuQuotaLimits(map[string]any{"gpu": map[string]any{
		"NVIDIA_A6000": float64(4),
		"nvidia-a6000": float64(2),
	}})
	want := map[string]int{"nvidia-a6000": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gpuQuotaLimits() = %#v, want canonical key to win: %#v", got, want)
	}
}

// Drill Q0b-2 of v0.11.1: a project at its floatingIps limit read "5 in use" and had no list of the five, four of
// them a cluster's Service addresses and one a balancer's. Every address the quota counts is named by what holds it.
func TestQuotaAddressesNameTheirHolder(t *testing.T) {
	lbRow := func(ext string, lb *loadBalancer) cloud.CloudResource {
		cr := cloud.CloudResource{ExternalID: ext, ServiceID: "pve", ProjectID: "p1", Type: cloud.TypeLoadBalancer}
		if err := setLBRow(&cr, lb); err != nil {
			t.Fatal(err)
		}
		return cr
	}
	rows := []cloud.CloudResource{
		{ExternalID: "127", ServiceID: "pve", Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{"name": "lb-one"}}},
		{ExternalID: "130", ServiceID: "pve", Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{"name": "bastion"}}},
		{ExternalID: "130", ServiceID: "other", Type: cloud.TypeServer, Data: map[string]any{"server": map[string]any{"name": "not this provider's"}}},
		{ExternalID: "pc-c-1", ServiceID: "k8s", Type: cloud.TypeKubernetesCluster, Data: map[string]any{"cluster": map[string]any{"name": "prod"}}},
		lbRow("lb-one", &loadBalancer{ID: "one", Name: "one", ServerID: "127"}),
		lbRow("lb-pair", &loadBalancer{ID: "pair", Name: "web", HA: true, Appliances: []lbAppliance{{ServerID: "124"}, {ServerID: "126"}}}),
		lbRow("lb-k8s", &loadBalancer{ID: "k8s", Name: "k8s-k8s", HA: true, Cluster: &lbCluster{ID: "pc-c-1",
			Services: []lbClusterSvc{{UID: "u1", Namespace: "default", Name: "web", VIP: "10.91.0.10"}, {UID: "u2", Namespace: "shop", Name: "api"}},
			Kept:     []string{"10.91.0.12"}, Releasing: []string{"10.91.0.13"}}}),
	}
	allocs := []PublicIP{
		{IP: "10.91.0.14", NetworkID: "vmbr1", ServerID: "127", NIC: publicNIC},
		{IP: "10.91.0.13", NetworkID: "vmbr1", ServerID: LBAddressOwner("k8s"), NIC: publicNIC},
		{IP: "10.91.0.12", NetworkID: "vmbr1", ServerID: LBAddressOwner("k8s"), NIC: publicNIC},
		{IP: "10.91.0.10", NetworkID: "vmbr1", ServerID: LBAddressOwner("k8s"), NIC: publicNIC},
		{IP: "10.91.0.11", NetworkID: "vmbr1", ServerID: LBAddressOwner("k8s"), NIC: publicNIC}, // allocated, not in the row yet
		{IP: "10.91.0.9", NetworkID: "vmbr1", ServerID: LBAddressOwner("pair"), NIC: publicNIC},
		{IP: "10.91.0.100", NetworkID: "vmbr1", ServerID: "130", NIC: publicNIC},
		{IP: "10.91.0.101", NetworkID: "vmbr1", ServerID: "999", NIC: publicNIC}, // a server whose row is not there yet
	}
	want := []quotaAddress{
		{IP: "10.91.0.9", NetworkID: "vmbr1", Kind: "loadBalancer", Name: "web", ResourceID: "lb-pair"},
		{IP: "10.91.0.10", NetworkID: "vmbr1", Kind: "service", Name: "default/web", ResourceID: "lb-k8s", Cluster: "pc-c-1", ClusterName: "prod"},
		{IP: "10.91.0.11", NetworkID: "vmbr1", Kind: "loadBalancer", Name: "k8s-k8s", ResourceID: "lb-k8s", Cluster: "pc-c-1", ClusterName: "prod"},
		{IP: "10.91.0.12", NetworkID: "vmbr1", Kind: "kept", ResourceID: "lb-k8s", Cluster: "pc-c-1", ClusterName: "prod"},
		{IP: "10.91.0.13", NetworkID: "vmbr1", Kind: "releasing", ResourceID: "lb-k8s", Cluster: "pc-c-1", ClusterName: "prod"},
		{IP: "10.91.0.14", NetworkID: "vmbr1", Kind: "loadBalancer", Name: "one", ResourceID: "lb-one"},
		{IP: "10.91.0.100", NetworkID: "vmbr1", Kind: "server", Name: "bastion", ResourceID: "130"},
		{IP: "10.91.0.101", NetworkID: "vmbr1", Kind: "server", ResourceID: "999"},
	}
	got := quotaAddresses(rows, "pve", allocs)
	if len(got) != len(allocs) {
		t.Fatalf("%d addresses named, %d counted", len(got), len(allocs))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("address %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
	if m := quotaMetricOf(map[string]any{"floatingIps": 5}, "floatingIps", 7); m != (quotaMetric{Used: 7, Limit: 5}) {
		t.Errorf("metric with a limit: %+v", m)
	}
	if m := quotaMetricOf(map[string]any{}, "floatingIps", 7); m != (quotaMetric{Used: 7, Limit: -1}) {
		t.Errorf("metric without a limit: %+v", m)
	}
}
