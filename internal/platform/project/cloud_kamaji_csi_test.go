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
	cfg := func(version string, d kamaji.ClusterDefaults) kamaji.Config {
		return kamaji.Config{ChartVersion: version, Defaults: d}
	}
	for name, tc := range map[string]struct {
		gw      *CSIGateway
		cfg     kamaji.Config
		compute *externalservice.ExternalService
		want    bool
		refused bool
	}{
		"on":                {on, cfg("0.1.7", kamaji.ClusterDefaults{}), withToken, true, false},
		"drill tag":         {on, cfg("0.1.7-csi.2", kamaji.ClusterDefaults{}), withToken, true, false},
		"later chart":       {on, cfg("0.2.0", kamaji.ClusterDefaults{}), withToken, true, false},
		"old chart":         {on, cfg("0.1.6", kamaji.ClusterDefaults{}), withToken, false, true},
		"malformed pin":     {on, cfg("latest", kamaji.ClusterDefaults{}), withToken, false, true},
		"no gateway":        {nil, cfg("0.1.5", kamaji.ClusterDefaults{}), withToken, false, false},
		"registry only":     {&CSIGateway{}, cfg("0.1.5", kamaji.ClusterDefaults{}), withToken, false, false},
		"no token":          {on, cfg("0.1.5", kamaji.ClusterDefaults{}), noToken, false, false},
		"shared workers":    {on, cfg("0.1.5", kamaji.ClusterDefaults{Shared: &kamaji.SharedWorkers{Pool: "p"}}), withToken, false, false},
		"shared stores too": {on, cfg("0.1.7", kamaji.ClusterDefaults{CSISharedStores: true}), withToken, true, false},
	} {
		h := &Handler{csiGW: tc.gw}
		got, err := h.kamajiGatewayCSI(tc.cfg, tc.compute)
		if got != tc.want || (err != nil) != tc.refused {
			t.Errorf("%s: %v %v, want %v (refused %v)", name, got, err, tc.want, tc.refused)
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
