package project

import (
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
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
