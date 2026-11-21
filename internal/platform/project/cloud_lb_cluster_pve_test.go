package project

import (
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// The workers' part of a reservation is what ClusterInfra.Addresses got at create: the first kamajiWorkerAddrs that
// Reserve handed out. The MetalLB part is never a member (C11), in whatever order the rows come back.
func TestKamajiWorkerPart(t *testing.T) {
	pn := externalservice.PVEPublicNetwork{ID: "v1", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Ranges: []string{"10.0.0.2-10.0.0.254"}}
	reserved, _, herr := freeAddresses(pn, []string{"10.0.0.254", "10.0.0.200"}, kamajiWorkerAddrs+kamajiLBAddrs)
	if herr != nil {
		t.Fatal(herr)
	}
	var rows []PublicIP
	for _, ip := range reserved {
		rows = append(rows, PublicIP{IP: ip, NIC: ReservedNIC, ServerID: "k8s:c1"})
	}
	rand.Shuffle(len(rows), func(i, j int) { rows[i], rows[j] = rows[j], rows[i] })
	got := kamajiWorkerPart(rows)
	if !slices.Equal(got, reserved[:kamajiWorkerAddrs]) {
		t.Fatalf("workers %v, want %v", got, reserved[:kamajiWorkerAddrs])
	}
	for _, metallb := range reserved[kamajiWorkerAddrs:] {
		if slices.Contains(got, metallb) {
			t.Fatalf("MetalLB address %s counted as a worker", metallb)
		}
	}
}

// A tenant's edit or delete of a cluster's balancer answers 409; its own balancers are untouched by the guard.
func TestLBClusterGuard(t *testing.T) {
	row := func(lb *loadBalancer) *cloud.CloudResource {
		cr := &cloud.CloudResource{Type: cloud.TypeLoadBalancer}
		if err := setLBRow(cr, lb); err != nil {
			t.Fatal(err)
		}
		return cr
	}
	if herr := lbClusterGuard(row(&loadBalancer{ID: "abc", HA: true, Cluster: &lbCluster{ID: "c1"}})); herr == nil || herr.Status != http.StatusConflict {
		t.Fatalf("cluster balancer: %v", herr)
	}
	if herr := lbClusterGuard(row(&loadBalancer{ID: "abc", HA: true})); herr != nil {
		t.Fatalf("tenant balancer: %v", herr)
	}
	id := lbClusterExternalID("c1")
	if id != lbClusterExternalID("c1") || id == lbClusterExternalID("c2") || len(id) != len("lb-")+12 || !pve.LBNameOK("k8s-"+id[3:]) {
		t.Fatalf("external id %q", id)
	}
	if _, err := lbOfRow(row(&loadBalancer{ID: id[3:]})); err != nil {
		t.Fatal(err)
	}
}

// One pool per Service port of the workers' node port in mode tcp, one listener on the Service's own address; policy
// Local is checked over HTTP on healthCheckNodePort.
func TestLBClusterServiceSpec(t *testing.T) {
	w := lbgw.Service{UID: "u", Ports: []lbgw.Port{{Port: 80, NodePort: 30080}, {Port: 443, NodePort: 30443}},
		ExternalTrafficPolicy: "Local", HealthCheckNodePort: 32000, ProxyProtocol: true, SourceRanges: []string{"198.51.100.0/24"}}
	spec := lbClusterServiceSpec(pve.LBSpec{Pools: []pve.LBPool{{Name: "s0-0"}}}, 1, w, "203.0.113.10", []string{"10.0.0.250", "10.0.0.249"})
	if len(spec.Pools) != 3 || len(spec.Listeners) != 2 {
		t.Fatalf("%+v", spec)
	}
	p := spec.Pools[2]
	if p.Name != "s1-1" || p.Protocol != "tcp" || !p.ProxyProtocol || p.HealthCheck == nil || p.HealthCheck.Port != 32000 ||
		len(p.Members) != 2 || !p.Members[0].Node || p.Members[0].Port != 30443 || p.Members[1].Address != "10.0.0.249" {
		t.Fatalf("pool %+v", p)
	}
	if l := spec.Listeners[1]; l.VIP != "203.0.113.10" || l.Port != 443 || l.Pool != "s1-1" || l.SourceRanges[0] != "198.51.100.0/24" {
		t.Fatalf("listener %+v", l)
	}
	w.ExternalTrafficPolicy = "Cluster"
	if spec := lbClusterServiceSpec(pve.LBSpec{}, 0, w, "203.0.113.10", nil); spec.Pools[0].HealthCheck != nil {
		t.Fatal("policy Cluster takes the TCP check on the node port")
	}
}

// A Compose site with the gateway listener (an endpoint) but no gateway token on its compute provider: a new cluster
// gets no CSI gateway, and so no StorageClass.
func TestComposeListenerWithoutTokenGetsNoStorageClass(t *testing.T) {
	h := &Handler{csiGW: &CSIGateway{Endpoint: csigw.Endpoint{URL: "https://192.0.2.7:8444", CA: "CA"}}}
	compute := &externalservice.ExternalService{Config: map[string]any{}}
	gateway, err := h.kamajiGatewayCSI(kamaji.Config{ChartVersion: "0.1.16"}, compute)
	if err != nil || gateway {
		t.Fatalf("gateway %v %v", gateway, err)
	}
	if classes := kamajiTenantClasses(gateway, kamaji.ClusterDefaults{}, []kamaji.StorageClass{{Name: "local", Storage: "local-lvm"}}); len(classes) != 0 {
		t.Fatalf("classes %v", classes)
	}
}
