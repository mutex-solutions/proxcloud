package project

import (
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/csigw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// A new cluster takes the gateway only when the portal has an endpoint, the workers are the project's, and the compute
// provider holds the gateway's PVE token. Anything else keeps the tenant-side controller and its csiSharedStores gate.
func TestKamajiGatewayCSI(t *testing.T) {
	withToken := &externalservice.ExternalService{Config: map[string]any{"csiGatewayTokenId": "csi@pve!gateway"},
		Secret: map[string]any{"csiGatewaySecret": "s"}}
	noToken := &externalservice.ExternalService{Config: map[string]any{}}
	on := &CSIGateway{Endpoint: csigw.Endpoint{URL: "https://gw:8443", CA: "CA"}}
	for name, tc := range map[string]struct {
		gw      *CSIGateway
		d       kamaji.ClusterDefaults
		compute *externalservice.ExternalService
		want    bool
	}{
		"on":                {on, kamaji.ClusterDefaults{}, withToken, true},
		"no gateway":        {nil, kamaji.ClusterDefaults{}, withToken, false},
		"registry only":     {&CSIGateway{}, kamaji.ClusterDefaults{}, withToken, false},
		"no token":          {on, kamaji.ClusterDefaults{}, noToken, false},
		"shared workers":    {on, kamaji.ClusterDefaults{Shared: &kamaji.SharedWorkers{Pool: "p"}}, withToken, false},
		"shared stores too": {on, kamaji.ClusterDefaults{CSISharedStores: true}, withToken, true},
	} {
		h := &Handler{csiGW: tc.gw}
		if got := h.kamajiGatewayCSI(tc.d, tc.compute); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// The portal never deletes, resizes or moves a tenant cluster's disk itself: the cluster's PersistentVolume owns it.
func TestCSIVolumeGuard(t *testing.T) {
	csi := &cloud.CloudResource{Type: cloud.TypeVolume, Data: map[string]any{"volume": map[string]any{"k8sCluster": "pc-c-1"}}}
	if herr := csiVolumeGuard(csi); herr == nil || herr.Status != http.StatusConflict {
		t.Fatalf("cluster disk: %v", herr)
	}
	plain := &cloud.CloudResource{Type: cloud.TypeVolume, Data: map[string]any{"volume": map[string]any{"vmid": 101}}}
	if herr := csiVolumeGuard(plain); herr != nil {
		t.Fatalf("server disk: %v", herr)
	}
}
