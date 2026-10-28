package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestCheckPublicNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api2/json/cluster/status":
			fmt.Fprint(w, `{"data":[{"type":"node","name":"pve1","ip":"10.0.0.11"}]}`)
		case "/api2/json/nodes/pve1/network":
			fmt.Fprint(w, `{"data":[{"iface":"vmbr0","address":"10.0.0.11","gateway":"10.0.0.1"},{"iface":"vmbr1"}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pns     []externalservice.PVEPublicNetwork
		wantErr string
	}{
		{"none configured", nil, ""},
		{"dedicated bridge", []externalservice.PVEPublicNetwork{{ID: "vmbr1"}}, ""},
		{"management bridge", []externalservice.PVEPublicNetwork{{ID: "vmbr1"}, {ID: "vmbr0"}}, `public network "vmbr0" is the management bridge on node pve1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			herr := checkPublicNetworks(context.Background(), cc, tt.pns)
			switch {
			case tt.wantErr == "" && herr != nil:
				t.Fatalf("unexpected error %v", herr)
			case tt.wantErr != "" && (herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr)):
				t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
			}
		})
	}
}

func TestTokenWarnings(t *testing.T) {
	full := map[string]bool{}
	for _, chk := range pve.PlatformPrivileges {
		for _, p := range chk.Privs {
			full[p] = true
		}
	}
	tests := []struct {
		name    string
		tokenID string
		status  int             // non-200 = /access/permissions fails
		privs   map[string]bool // granted on every path
		want    []string        // substrings, in order
	}{
		{"complete platform token", "proxcloud@pve!platform", 200, full, nil},
		{"root token", "root@pam!x", 200, full, []string{"belongs to root@pam"}},
		{"missing privilege", "proxcloud@pve!platform", 200, without(full, "SDN.Allocate"), []string{"Token lacks SDN.Allocate on /sdn"}},
		{"permissions unreadable", "proxcloud@pve!platform", 500, nil, []string{"Could not read the token's permissions"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api2/json/access/permissions" || tt.status != 200 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var ps []string
				for p := range tt.privs {
					ps = append(ps, fmt.Sprintf("%q:1", p))
				}
				fmt.Fprintf(w, `{"data":{%q:{%s}}}`, r.URL.Query().Get("path"), strings.Join(ps, ","))
			}))
			defer srv.Close()
			cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: tt.tokenID, Secret: "sec"})
			if err != nil {
				t.Fatal(err)
			}
			got := tokenWarnings(context.Background(), cc, tt.tokenID)
			if len(got) != len(tt.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func without(m map[string]bool, key string) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// TestCheckNodeFirewalls: an online node on the iptables firewall (nftables 0 or
// absent) fails the preflight — the vnet isolation rules would be ignored there.
func TestCheckNodeFirewalls(t *testing.T) {
	tests := []struct {
		name    string
		pve2    string // pve2's firewall options body; "" = request fails
		wantErr string
	}{
		{"all nftables", `{"data":{"nftables":1}}`, ""},
		{"iptables node", `{"data":{"enable":1}}`, "node pve2 runs the legacy iptables firewall"},
		{"options unreadable", "", "cannot read the node firewall options"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api2/json/nodes":
					fmt.Fprint(w, `{"data":[{"node":"pve1","status":"online"},{"node":"pve2","status":"online"},{"node":"pve3","status":"offline"}]}`)
				case "/api2/json/nodes/pve1/firewall/options":
					fmt.Fprint(w, `{"data":{"nftables":1}}`)
				case "/api2/json/nodes/pve2/firewall/options":
					if tt.pve2 == "" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					fmt.Fprint(w, tt.pve2)
				default: // the offline pve3 must never be asked
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			cc, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "proxcloud@pve!t1", Secret: "sec"})
			if err != nil {
				t.Fatal(err)
			}
			herr := checkNodeFirewalls(context.Background(), cc)
			switch {
			case tt.wantErr == "" && herr != nil:
				t.Fatalf("unexpected error %v", herr)
			case tt.wantErr != "" && (herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr)):
				t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
			}
		})
	}
}

func TestApplyStorageClasses(t *testing.T) {
	stored := func() pgdoc.M {
		return pgdoc.M{"config": map[string]any{"storageClasses": map[string]any{
			"nas": map[string]any{"class": "capacity", "suggestedClass": "standard"},
		}}}
	}
	tests := []struct {
		name    string
		body    string
		wantErr string
		want    map[string]any // storageClasses after apply
	}{
		{"set override keeps suggestion", `{"nas":"performance"}`, "",
			map[string]any{"nas": pgdoc.M{"class": "performance", "suggestedClass": "standard"}}},
		{"clear override keeps suggestion", `{"nas":""}`, "",
			map[string]any{"nas": pgdoc.M{"suggestedClass": "standard"}}},
		{"new storage", `{"local-zfs":"performance"}`, "",
			map[string]any{"nas": pgdoc.M{"class": "capacity", "suggestedClass": "standard"}, "local-zfs": pgdoc.M{"class": "performance"}}},
		{"invalid class", `{"nas":"fast"}`, "must be one of", nil},
		{"bad body", `[1]`, "Invalid request body", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := stored()
			req := httptest.NewRequest(http.MethodPut, "/service/s1/storage-classes", strings.NewReader(tt.body))
			herr := applyStorageClasses(req, doc)
			if tt.wantErr != "" {
				if herr == nil || herr.Status != http.StatusBadRequest || !strings.Contains(herr.Msg, tt.wantErr) {
					t.Fatalf("got %+v, want 400 containing %q", herr, tt.wantErr)
				}
				return
			}
			if herr != nil {
				t.Fatalf("unexpected error %+v", herr)
			}
			got := map[string]any(doc["config"].(pgdoc.M)["storageClasses"].(pgdoc.M))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", got, tt.want)
			}
		})
	}
}
