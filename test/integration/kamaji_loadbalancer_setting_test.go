//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/admin"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// config.cluster.loadBalancer on a Kubernetes provider is checked when a save sets or changes it: an active flavor, a
// public network the workers' Proxmox provider has, with a gateway, on a plain bridge. A save that leaves the block as
// stored is not refused for it.
func TestKamajiLoadBalancerSettingCheckedOnSave(t *testing.T) {
	pveAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/sdn/vnets":
			_, _ = w.Write([]byte(`{"data":[{"vnet":"vnetpub","zone":"z1"}]}`))
		case "/api2/json/cluster/sdn/zones":
			_, _ = w.Write([]byte(`{"data":[{"zone":"z1","type":"evpn"}]}`))
		default:
			t.Errorf("reached PVE: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	}))
	defer pveAPI.Close()

	db := freshPG(t)
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "pve1", "name": "pve1", "type": "CLOUD", "config": pgdoc.M{"provider": "proxmox",
		"network": pgdoc.M{"publicNetworks": []any{
			pgdoc.M{"id": "vmbr1", "cidr": "203.0.113.0/24", "gateway": "203.0.113.1", "ranges": []any{"203.0.113.10-203.0.113.20"}},
			pgdoc.M{"id": "vmbr2", "cidr": "198.51.100.0/24", "ranges": []any{"198.51.100.10-198.51.100.20"}},
			pgdoc.M{"id": "vnetpub", "cidr": "192.0.2.0/24", "gateway": "192.0.2.1", "ranges": []any{"192.0.2.10-192.0.2.20"}},
		}}}})
	flavors := pveflavor.New(db)
	for _, f := range []pveflavor.Flavor{{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10, Active: true},
		{Slug: "old", Name: "old", VCPUs: 1, RAMMb: 1024, DiskGb: 10}} {
		if err := flavors.Save(t.Context(), &f); err != nil {
			t.Fatal(err)
		}
	}
	const iss, cid = "test-iss", "test-cid"
	es := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := admin.NewHandler(admin.NewRepo(db), nil, nil, nil, nil, nil, nil, nil, nil, es, "", iss, cid)
	h.SetPVEFlavors(flavors)
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) {
		return pve.NewService(pve.Config{APIURL: pveAPI.URL, TokenID: "t", Secret: "s"}, "pve1", "pc-")
	})
	r := chi.NewRouter()
	h.Routes(r)
	do := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "admin-sub", Issuer: iss, Azp: cid}))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	provider := func(lb any) map[string]any {
		return map[string]any{"name": "k8s", "type": "CLOUD", "config": map[string]any{"provider": "kamaji",
			"cluster": map[string]any{"proxmoxServiceId": "pve1", "loadBalancer": lb}}}
	}
	lb := func(network, flavor string) map[string]any { return map[string]any{"publicNetworkId": network, "flavorId": flavor} }
	for name, tc := range map[string]struct {
		lb   any
		want string
	}{
		"inactive flavor": {lb("vmbr1", "old"), "flavor old is not an active flavor"},
		"unknown flavor":  {lb("vmbr1", "nope"), "flavor nope is not an active flavor"},
		"unknown network": {lb("vmbr9", "lb-small"), "no Proxmox provider of the workers has public network vmbr9"},
		"no gateway":      {lb("vmbr2", "lb-small"), "has no gateway"},
		"sdn vnet":        {lb("vnetpub", "lb-small"), "is an SDN vnet"},
		"shape":           {map[string]any{"publicNetworkId": "vmbr1"}, "config.cluster.loadBalancer must be"},
	} {
		if w := do(http.MethodPost, "/admin/service", provider(tc.lb)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	w := do(http.MethodPost, "/admin/service", provider(lb("vmbr1", "lb-small")))
	if w.Code != http.StatusOK {
		t.Fatalf("valid: %d %s", w.Code, w.Body)
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.Data.ID == "" {
		t.Fatalf("create answer: %v %s", err, w.Body)
	}
	// The flavor is retired afterwards: a save that keeps the block passes, one that changes it is checked again.
	if err := flavors.Save(t.Context(), &pveflavor.Flavor{Slug: "lb-small", Name: "lb-small", VCPUs: 1, RAMMb: 1024, DiskGb: 10}); err != nil {
		t.Fatal(err)
	}
	keep := provider(lb("vmbr1", "lb-small"))
	keep["config"].(map[string]any)["cluster"].(map[string]any)["dnsZone"] = "k8s.example.com"
	if w := do(http.MethodPut, "/admin/service/"+created.Data.ID, keep); w.Code != http.StatusOK {
		t.Fatalf("save keeping the block: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPut, "/admin/service/"+created.Data.ID, provider(lb("vnetpub", "lb-small"))); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "flavor lb-small") {
		t.Fatalf("save changing the block: %d %s", w.Code, w.Body)
	}
}
