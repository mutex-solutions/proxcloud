package project

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

func TestPVEPickCIDR(t *testing.T) {
	const pool = "10.100.0.0/16"
	tests := []struct {
		name       string
		requested  string
		pool       string
		used       []string
		want       string
		wantStatus int // 0 = success
	}{
		{"carve first /24", "", pool, nil, "10.100.0.0/24", 0},
		{"carve skips used", "", pool, []string{"10.100.0.0/24", "10.100.1.0/24"}, "10.100.2.0/24", 0},
		{"carve skips a wider used net", "", pool, []string{"10.100.0.0/23"}, "10.100.2.0/24", 0},
		{"carve skips a narrower used net", "", pool, []string{"10.100.0.128/25"}, "10.100.1.0/24", 0},
		{"carve exhausted", "", "10.100.0.0/24", []string{"10.100.0.0/24"}, "", http.StatusConflict},
		{"requested ok, canonicalized", "10.100.7.5/24", pool, []string{"10.100.0.0/24"}, "10.100.7.0/24", 0},
		{"requested small static subnet", "10.113.0.0/29", "10.113.0.0/16", nil, "10.113.0.0/29", 0},
		{"requested overlaps another project's subnet", "10.100.0.0/23", pool, []string{"10.100.1.0/24"}, "", http.StatusConflict},
		{"requested inside a used net", "10.100.1.64/26", pool, []string{"10.100.1.0/24"}, "", http.StatusConflict},
		{"requested out of pool", "192.168.1.0/24", pool, nil, "", http.StatusBadRequest},
		{"requested wider than pool", "10.100.0.0/15", pool, nil, "", http.StatusBadRequest},
		{"requested too small", "10.100.9.0/30", pool, nil, "", http.StatusBadRequest},
		{"requested invalid", "nope", pool, nil, "", http.StatusBadRequest},
		{"requested ipv6", "fd00::/64", pool, nil, "", http.StatusBadRequest},
		{"no pool configured", "10.100.0.0/24", "", nil, "", http.StatusServiceUnavailable},
		{"pool narrower than /24", "", "10.100.0.0/25", nil, "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, herr := pvePickCIDR(tt.requested, tt.pool, tt.used)
			if tt.wantStatus != 0 {
				if herr == nil || herr.Status != tt.wantStatus {
					t.Fatalf("got %q, %v; want status %d", got, herr, tt.wantStatus)
				}
				return
			}
			if herr != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, herr, tt.want)
			}
		})
	}
}

// The server build turns the live VnetStaticPoolFor result into a narrowed public-IP
// pool. Exercise that boundary together: a valid /29 must never become Ranges:["-"].
func TestPVETinyStaticPoolForAllocation(t *testing.T) {
	tests := []struct {
		name, cidr, gateway, within, dhcp string
		first, last, want                 string
		prefix                            int
		wantErr                           bool
	}{
		{"accepted /29", "10.113.0.0/29", "10.113.0.1", "10.113.0.0/29", "", "10.113.0.2", "10.113.0.6", "10.113.0.2", 29, false},
		{"selected by host", "10.113.0.0/29", "10.113.0.1", "10.113.0.4", "", "10.113.0.2", "10.113.0.6", "10.113.0.2", 29, false},
		{"gateway in middle", "10.113.0.0/29", "10.113.0.4", "", "", "10.113.0.1", "10.113.0.6", "10.113.0.1", 29, false},
		{"gateway last", "10.113.0.0/29", "10.113.0.6", "", "", "10.113.0.1", "10.113.0.5", "10.113.0.1", 29, false},
		{"/30 helper", "10.113.0.0/30", "10.113.0.1", "", "", "10.113.0.2", "10.113.0.2", "10.113.0.2", 30, false},
		{"upper /25", "10.113.0.128/25", "10.113.0.129", "", "", "10.113.0.138", "10.113.0.254", "10.113.0.138", 25, false},
		{"malformed", "nope", "", "", "", "", "", "", 0, true},
		{"IPv6", "2001:db8::/64", "2001:db8::1", "", "", "", "", "", 0, true},
		{"/31", "10.113.0.0/31", "", "", "", "", "", "", 0, true},
		{"/32", "10.113.0.1/32", "", "", "", "", "", "", 0, true},
		{"requested outside subnet", "10.113.0.0/29", "10.113.0.1", "10.113.1.2", "", "", "", "", 0, true},
		{"DHCP zone", "10.113.0.0/24", "10.113.0.1", "", "dnsmasq", "", "", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api2/json/cluster/sdn/vnets", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"data":[{"vnet":"vtiny","zone":"overlay"}]}`)
			})
			mux.HandleFunc("GET /api2/json/cluster/sdn/zones", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[{"zone":"overlay","dhcp":%q}]}`, tt.dhcp)
			})
			mux.HandleFunc("GET /api2/json/cluster/sdn/vnets/vtiny/subnets", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[{"cidr":%q,"gateway":%q}]}`, tt.cidr, tt.gateway)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client, err := pve.New(pve.Config{APIURL: srv.URL, TokenID: "test@pve!test", Secret: "fake"})
			if err != nil {
				t.Fatal(err)
			}
			ps := &pve.Service{C: client}
			cidr, gw, first, last, static, err := ps.VnetStaticPoolFor(context.Background(), "vtiny", tt.within)
			if tt.wantErr {
				if err == nil || static || first != "" || last != "" {
					t.Fatalf("invalid subnet returned a pool: %q %q..%q static=%v err=%v", cidr, first, last, static, err)
				}
				return
			}
			if tt.dhcp != "" {
				if err != nil || static || first != "" || last != "" {
					t.Fatalf("DHCP zone returned static pool: %q..%q static=%v err=%v", first, last, static, err)
				}
				return
			}
			if err != nil || !static || cidr != tt.cidr || gw != tt.gateway || first != tt.first || last != tt.last {
				t.Fatalf("pool = %q gw=%q %q..%q static=%v err=%v", cidr, gw, first, last, static, err)
			}
			pool := externalservice.PVEPublicNetwork{ID: "vtiny", CIDR: cidr, Gateway: gw, Ranges: []string{first + "-" + last}}
			ip, prefix, herr := pickPublicIP(pool, nil)
			if herr != nil || ip != tt.want || prefix != tt.prefix {
				t.Fatalf("allocation = %q/%d err=%v, want %s/%d", ip, prefix, herr, tt.want, tt.prefix)
			}
			// Allocate the entire span and prove gateway/network/broadcast are never
			// handed out, including a gateway in the middle of a non-split range.
			var used []string
			for range 256 {
				ip, _, herr = pickPublicIP(pool, used)
				if herr != nil {
					if herr.Status != http.StatusConflict {
						t.Fatalf("exhausted pool must return 409, not an invalid-pool 503: %v", herr)
					}
					return
				}
				if ip == gw || ip == "10.113.0.0" || ip == "10.113.0.7" {
					t.Fatalf("allocated reserved address %s", ip)
				}
				used = append(used, ip)
			}
			t.Fatal("pool did not exhaust")
		})
	}
}

// A live cluster's addresses refuse the network delete. A deleted cluster's are purged instead, or a row no sweep will
// ever reach would keep the network forever.
func TestLiveClusterOn(t *testing.T) {
	rows := []PublicIP{
		{ServerID: "100", NIC: "net0"},
		{ServerID: "k8s:pc-c-gone", NIC: ReservedNIC},
		{ServerID: "k8s:pc-c-live", NIC: ReservedNIC},
	}
	if got := liveClusterOn(rows, []cloud.CloudResource{{ExternalID: "pc-c-live"}, {ExternalID: "pc-c-other"}}); got != "pc-c-live" {
		t.Errorf("got %q", got)
	}
	if got := liveClusterOn(rows, []cloud.CloudResource{{ExternalID: "pc-c-other"}}); got != "" {
		t.Errorf("no live cluster holds these: got %q", got)
	}
}
