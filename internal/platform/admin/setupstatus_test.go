package admin

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/deploy/seed"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/platformconfig"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// setupProviderWith is a Proxmox provider held back from new projects, with cfg laid over the connection fields.
func setupProviderWith(cfg ...map[string]any) *externalservice.ExternalService {
	c := map[string]any{"provider": "proxmox", "region": "lab", "apiUrl": "https://pve.invalid:8006", "attachToNewProjects": false}
	for _, m := range cfg {
		maps.Copy(c, m)
	}
	return &externalservice.ExternalService{ID: "svc-1", Name: "lab", Type: externalservice.TypeCloud, Status: "PUBLIC", Config: c}
}

var (
	setupNetwork = map[string]any{"network": map[string]any{"defaultZone": "simple", "zones": map[string]any{"simple": map[string]any{"kind": "simple"}}, "cidrPool": "10.100.0.0/16"}}
	setupStorage = map[string]any{
		"storageProfiles":       map[string]any{"std": map[string]any{"storages": map[string]any{"*": "local-zfs"}}},
		"defaultStorageProfile": "std",
		"capabilities": map[string]any{"detectedAt": "2026-11-01T00:00:00Z", "storages": map[string]any{
			"local-zfs": map[string]any{"storage": "local-zfs", "type": "zfspool", "content": []any{"images"}},
			"local":     map[string]any{"storage": "local", "type": "dir", "content": []any{"iso", "import"}},
		}},
	}
	setupQuota = map[string]any{"provisioning": map[string]any{"quota": func() map[string]any {
		q := map[string]any{}
		for k, v := range seed.DefaultQuota {
			q[k] = float64(v)
		}
		return q
	}()}}
	setupOn = map[string]any{"attachToNewProjects": true}
	// setupPinned pins a template in the provider configuration, so the templates step is read without live.
	setupPinned = map[string]any{"templates": map[string]any{"debian-13": map[string]any{"vmid": float64(9000), "node": "pve1"}}}
)

// setupCatalogDone fills the provider-independent half of a finished catalog step.
func setupCatalogDone(s setupSnapshot) setupSnapshot {
	s.Flavors, s.PublicPlans, s.InstanceRules = 3, 1, 1
	s.Regions = []platformconfig.RegionDisplayConfig{{ServiceID: "svc-1", Region: "lab"}}
	s.Issuer = billing.IssuerInfo{BusinessName: "Example Pte Ltd", Address: "1 Street", Country: "SG", BaseCurrency: "USD"}
	return s
}

func TestSetupStatus(t *testing.T) {
	ready := func(cfg ...map[string]any) setupSnapshot {
		s := setupCatalogDone(setupSnapshot{Provider: setupProviderWith(append([]map[string]any{setupNetwork, setupStorage, setupQuota}, cfg...)...)})
		s.Scheduler = true
		return s
	}
	liveOK := func() *setupLive {
		return &setupLive{Checks: []setupItem{{ID: "reachable", State: stateDone}, {ID: "firewall", State: stateDone}}, ImportNodes: []string{"pve1"}, Nodes: 1, Images: 3}
	}
	withLive := func(s setupSnapshot, l *setupLive) setupSnapshot { s.Live = l; return s }
	tests := []struct {
		name  string
		s     setupSnapshot
		steps map[string]string // step → state
		items map[string]string // "step/item" → state
	}{
		// What `proxcloud-up.sh` leaves: the seed's billing (USD) and platform configuration, nothing else. A count of
		// those rows would call billing and branding configured.
		{"fresh compose install", setupSnapshot{Issuer: billing.IssuerInfo{BaseCurrency: "USD"}},
			map[string]string{stepConnect: stateTodo, stepNetwork: stateBlocked, stepStorage: stateBlocked, stepTemplates: stateBlocked, stepCatalog: stateTodo, stepGoLive: stateBlocked},
			map[string]string{"catalog/flavors": stateTodo, "catalog/pricePlan": stateTodo, "catalog/quota": stateBlocked, "catalog/region": stateBlocked, "catalog/billingDetails": stateAttention}},
		{"provider saved with its connection only", setupSnapshot{Provider: setupProviderWith(), Issuer: billing.IssuerInfo{BaseCurrency: "USD"}},
			map[string]string{stepConnect: stateDone, stepNetwork: stateTodo, stepStorage: stateTodo, stepTemplates: stateUnknown, stepCatalog: stateTodo, stepGoLive: stateBlocked},
			map[string]string{"network/cidrPool": stateTodo, "network/zone": stateTodo, "storage/detected": stateTodo, "storage/profiles": stateTodo, "storage/defaultProfile": stateTodo,
				"catalog/quota": stateTodo, "catalog/region": stateTodo, "golive/attachPolicy": stateTodo}},
		// Templates are not read without live, so go live cannot say it may start.
		{"configured, database only", ready(),
			map[string]string{stepConnect: stateDone, stepNetwork: stateDone, stepStorage: stateDone, stepTemplates: stateUnknown, stepCatalog: stateDone, stepGoLive: stateUnknown},
			map[string]string{"storage/importStore": stateDone, "templates/images": stateUnknown, "golive/attachPolicy": stateTodo}},
		{"configured and live", withLive(ready(), liveOK()),
			map[string]string{stepConnect: stateDone, stepNetwork: stateDone, stepStorage: stateDone, stepTemplates: stateDone, stepCatalog: stateDone, stepGoLive: stateTodo},
			map[string]string{"connect/firewall": stateDone, "storage/importStore": stateDone, "templates/images": stateDone}},
		{"gone live", withLive(ready(setupOn), liveOK()),
			map[string]string{stepConnect: stateDone, stepNetwork: stateDone, stepStorage: stateDone, stepTemplates: stateDone, stepCatalog: stateDone, stepGoLive: stateDone},
			map[string]string{"golive/scheduler": stateDone}},
		// The owner's call: without the scheduler nothing syncs or charges on a timer, which go live says and allows.
		{"scheduler off warns and blocks nothing", func() setupSnapshot {
			s := withLive(ready(setupOn), liveOK())
			s.Scheduler = false
			return s
		}(), map[string]string{stepGoLive: stateAttention}, map[string]string{"golive/attachPolicy": stateDone, "golive/scheduler": stateAttention}},
		{"a template pinned in the configuration counts without live", ready(setupPinned),
			map[string]string{stepTemplates: stateDone, stepGoLive: stateTodo}, nil},
		{"live: a step that could not be read holds go live", withLive(ready(), &setupLive{Images: 1, ImportErr: "the cluster's storage could not be read"}),
			map[string]string{stepStorage: stateUnknown, stepGoLive: stateUnknown}, map[string]string{"golive/attachPolicy": stateTodo}},
		{"attached while a step could not be read", ready(setupOn),
			map[string]string{stepTemplates: stateUnknown, stepGoLive: stateDone}, map[string]string{"golive/attachPolicy": stateDone}},
		{"no image yet: the sub-steps show", withLive(ready(), &setupLive{ImportNodes: []string{"pve1"}, Nodes: 1, GitHubToken: true, Imports: map[string]int{"ready_to_publish": 2, "importing": 1}}),
			map[string]string{stepTemplates: stateTodo, stepGoLive: stateBlocked},
			map[string]string{"templates/images": stateTodo, "templates/flag": stateTodo, "templates/githubToken": stateDone, "templates/smokeNetwork": stateTodo, "templates/imports": stateTodo}},
		{"a failed connection check blocks go live", withLive(ready(), &setupLive{Checks: []setupItem{{ID: "reachable", State: stateDone}, {ID: "firewall", State: stateTodo, Message: "The datacenter firewall is disabled"}}, Images: 1, ImportNodes: []string{"pve1"}}),
			map[string]string{stepConnect: stateTodo, stepGoLive: stateBlocked}, nil},
		{"a missing privilege is a warning and blocks nothing", withLive(ready(), &setupLive{Checks: []setupItem{{ID: "privileges", State: stateAttention, Message: "Token lacks Mapping.Use on /mapping/pci"}}, Images: 1, ImportNodes: []string{"pve1"}}),
			map[string]string{stepConnect: stateAttention, stepGoLive: stateTodo}, nil},
		{"a disabled provider", func() setupSnapshot {
			s := ready()
			s.Provider.Status = externalservice.StatusDisabled
			return s
		}(), map[string]string{stepConnect: stateAttention}, map[string]string{"connect/status": stateAttention}},
		// A provider made before the wizard has no attach policy key, which means on.
		{"attached to new projects while steps are open", setupCatalogDone(setupSnapshot{Provider: func() *externalservice.ExternalService {
			p := setupProviderWith(setupStorage, setupQuota)
			delete(p.Config, "attachToNewProjects")
			return p
		}()}), map[string]string{stepNetwork: stateTodo, stepGoLive: stateAttention}, map[string]string{"golive/attachPolicy": stateAttention}},
		{"an open catalog does not block go live", func() setupSnapshot {
			s := withLive(ready(), liveOK())
			s.Flavors = 0
			return s
		}(), map[string]string{stepCatalog: stateTodo, stepGoLive: stateTodo}, map[string]string{"golive/catalog": stateAttention, "golive/attachPolicy": stateTodo}},
		{"no rule for servers", func() setupSnapshot {
			s := ready()
			s.InstanceRules = 0
			s.Unpriced = []UnpricedFlavor{{ID: "s1-small"}}
			return s
		}(), map[string]string{stepCatalog: stateTodo}, map[string]string{"catalog/pricePlan": stateTodo}},
		{"two enabled public plans", func() setupSnapshot {
			s := ready()
			s.PublicPlans, s.InstanceRules = 2, 2
			return s
		}(), map[string]string{stepCatalog: stateAttention}, map[string]string{"catalog/pricePlan": stateAttention}},
		{"an unpriced flavor", func() setupSnapshot {
			s := ready()
			s.Unpriced = []UnpricedFlavor{{ID: "g1-4090", Reason: "no gpu rule"}}
			return s
		}(), map[string]string{stepCatalog: stateAttention}, map[string]string{"catalog/unpricedFlavors": stateAttention, "catalog/pricePlan": stateDone}},
		{"no default quota, no region row, no business details", func() setupSnapshot {
			s := setupCatalogDone(setupSnapshot{Provider: setupProviderWith(setupNetwork, setupStorage)})
			s.Regions, s.Issuer = nil, billing.IssuerInfo{BaseCurrency: "USD"}
			return s
		}(), map[string]string{stepCatalog: stateTodo}, map[string]string{"catalog/quota": stateTodo, "catalog/region": stateTodo, "catalog/billingDetails": stateAttention}},
		// The same rule as the defaults: a quota that leaves a limit unset leaves new projects unlimited there.
		{"only per-model GPU limits are set", ready(map[string]any{"provisioning": map[string]any{"quota": map[string]any{"gpu": map[string]any{"nvidia-4090": float64(2)}}}}),
			map[string]string{stepCatalog: stateTodo}, map[string]string{"catalog/quota": stateTodo}},
		{"one limit is unset", ready(map[string]any{"provisioning": map[string]any{"quota": map[string]any{"servers": float64(20)}}}),
			map[string]string{stepCatalog: stateTodo}, map[string]string{"catalog/quota": stateTodo}},
		{"the default profile names no saved profile", ready(map[string]any{"defaultStorageProfile": "gone"}),
			map[string]string{stepStorage: stateTodo}, map[string]string{"storage/defaultProfile": stateTodo, "storage/profiles": stateDone}},
		{"no store takes import content", func() setupSnapshot {
			s := ready(setupPinned)
			s.Provider.Config["capabilities"] = map[string]any{"detectedAt": "2026-11-01T00:00:00Z", "storages": map[string]any{
				"local-zfs": map[string]any{"storage": "local-zfs", "type": "zfspool", "content": []any{"images"}}}}
			return s
		}(), map[string]string{stepStorage: stateAttention, stepGoLive: stateTodo}, map[string]string{"storage/importStore": stateAttention}},
		{"live: no node has an active import store", withLive(ready(), &setupLive{Images: 1, Nodes: 2}),
			map[string]string{stepStorage: stateAttention}, map[string]string{"storage/importStore": stateAttention}},
		{"live: the storages could not be read", withLive(ready(), &setupLive{Images: 1, ImportErr: "the cluster's storage could not be read"}),
			map[string]string{stepStorage: stateUnknown}, map[string]string{"storage/importStore": stateUnknown}},
	}
	order := []string{stepConnect, stepNetwork, stepStorage, stepTemplates, stepCatalog, stepGoLive}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps := setupStatus(tt.s)
			got, items := map[string]string{}, map[string]string{}
			for i, st := range steps {
				if i >= len(order) || st.ID != order[i] {
					t.Fatalf("step %d is %q, want the order %v", i, st.ID, order)
				}
				if st.Summary == "" || st.Items == nil {
					t.Errorf("%s: empty summary or null items (%+v)", st.ID, st)
				}
				got[st.ID] = st.State
				for _, it := range st.Items {
					if it.Message == "" && it.State != stateDone {
						t.Errorf("%s/%s: %s without a message", st.ID, it.ID, it.State)
					}
					items[st.ID+"/"+it.ID] = it.State
				}
			}
			if len(steps) != len(order) {
				t.Fatalf("%d steps", len(steps))
			}
			for id, want := range tt.steps {
				if got[id] != want {
					t.Errorf("step %s: %s, want %s (%+v)", id, got[id], want, steps)
				}
			}
			for id, want := range tt.items {
				if items[id] != want {
					t.Errorf("item %s: %q, want %s", id, items[id], want)
				}
			}
		})
	}
	// The fresh install again, as the one rule: nothing is done before the operator did something.
	for _, st := range setupStatus(setupSnapshot{Issuer: billing.IssuerInfo{BaseCurrency: "USD"}}) {
		if st.State == stateDone {
			t.Errorf("fresh install: step %s is done", st.ID)
		}
		for _, it := range st.Items {
			if it.State == stateDone {
				t.Errorf("fresh install: %s/%s is done", st.ID, it.ID)
			}
		}
	}
}

// pveLiveChecks answers every check, not only the first that fails, against the fake PVE API.
func TestPVELiveChecks(t *testing.T) {
	tests := []struct {
		name                string
		version             int    // status of GET /version
		firewall            string // datacenter firewall options body
		nodeFirewall        string // pve1's firewall options body
		privs               bool   // the token holds every platform privilege
		publicNet           string // a configured public network ("" = none)
		want                map[string]string
		wantFirewallMessage string
	}{
		{"healthy", 200, `{"data":{"enable":1}}`, `{"data":{"nftables":1}}`, true, "vmbr1",
			map[string]string{"reachable": stateDone, "firewall": stateDone, "nftables": stateDone, "publicNetworks": stateDone, "privileges": stateDone}, ""},
		{"firewall off still lists the other findings", 200, `{"data":{"enable":0}}`, `{"data":{"enable":1}}`, false, "vmbr0",
			map[string]string{"reachable": stateDone, "firewall": stateTodo, "nftables": stateTodo, "publicNetworks": stateTodo, "privileges": stateAttention}, "Datacenter > Firewall > Options"},
		{"no public network configured: no such check", 200, `{"data":{"enable":1}}`, `{"data":{"nftables":1}}`, true, "",
			map[string]string{"reachable": stateDone, "firewall": stateDone, "nftables": stateDone, "privileges": stateDone}, ""},
		{"the API refuses the token: nothing else is asked", 401, "", "", false, "vmbr1", map[string]string{"reachable": stateTodo}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api2/json/version":
					if tt.version != 200 {
						w.WriteHeader(tt.version)
						return
					}
					fmt.Fprint(w, `{"data":{"version":"9.2.1"}}`)
				case "/api2/json/cluster/firewall/options":
					fmt.Fprint(w, tt.firewall)
				case "/api2/json/nodes":
					fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online"}]}`)
				case "/api2/json/nodes/pve1/firewall/options":
					fmt.Fprint(w, tt.nodeFirewall)
				case "/api2/json/cluster/status":
					fmt.Fprint(w, `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"}]}`)
				case "/api2/json/nodes/pve1/network":
					fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1"},{"iface":"vmbr1"}]}`)
				case "/api2/json/access/permissions":
					var ps []string
					for _, chk := range pve.PlatformPrivileges {
						for _, p := range chk.Privs {
							if tt.privs || p != "Mapping.Use" {
								ps = append(ps, fmt.Sprintf("%q:1", p))
							}
						}
					}
					fmt.Fprintf(w, `{"data":{%q:{%s}}}`, r.URL.Query().Get("path"), strings.Join(ps, ","))
				default:
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!platform", Secret: "sec"})
			if err != nil {
				t.Fatal(err)
			}
			cfg := map[string]any{"tokenId": "proxcloud@pve!platform"}
			if tt.publicNet != "" {
				cfg["network"] = map[string]any{"publicNetworks": []any{map[string]any{"id": tt.publicNet}}}
			}
			items := pveLiveChecks(context.Background(), cc, setupProviderWith(cfg))
			got := map[string]string{}
			for _, it := range items {
				got[it.ID] = it.State
				if it.Message == "" || strings.Contains(it.Message, "Proxmox preflight failed") {
					t.Errorf("%s: message %q", it.ID, it.Message)
				}
				if it.ID == "firewall" && !strings.Contains(it.Message, tt.wantFirewallMessage) {
					t.Errorf("firewall message %q", it.Message)
				}
			}
			if !maps.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// The status is an operator read of the same four families as the defaults preview: a tenant, and an operator
// without one of the reads, gets 403 and nothing is read; SUPPORT and VIEWER may read it.
func TestSetupStatusRouteGate(t *testing.T) {
	h := &Handler{adminIssuer: "operator-realm"}
	r := chi.NewRouter()
	r.Route("/admin", h.routeSetup)
	for _, path := range []string{"/admin/setup/status", "/admin/setup/status?serviceId=s&live=1"} {
		if w := tuDo(r, "GET", path, "", &httpx.RequestContext{Sub: "tenant", Issuer: "tenant-realm"}); w.Code != http.StatusForbidden {
			t.Errorf("%s as tenant: %d", path, w.Code)
		}
	}
	if m := missingPerm(rolePermissions("BILLING_ADMIN"), defaultsReadPerms); m != serviceReadPerm {
		t.Errorf("BILLING_ADMIN reads the setup status without admin:service:read (missing %q)", m)
	}
	for _, role := range []string{"SUPPORT", "VIEWER", "ADMIN"} {
		if m := missingPerm(rolePermissions(role), defaultsReadPerms); m != "" {
			t.Errorf("%s cannot read the setup status: %s", role, m)
		}
	}
}
