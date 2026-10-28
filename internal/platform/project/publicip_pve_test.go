package project

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestPickPublicIP(t *testing.T) {
	pool := externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "203.0.113.0/29", Gateway: "203.0.113.1"}
	withRanges := func(ranges ...string) externalservice.PVEPublicNetwork {
		pn := pool
		pn.Ranges = ranges
		return pn
	}
	tests := []struct {
		name       string
		pn         externalservice.PVEPublicNetwork
		used       []string
		want       string
		wantStatus int // 0 = success
	}{
		{"skips network and gateway", pool, nil, "203.0.113.2", 0},
		{"skips used", pool, []string{"203.0.113.2", "203.0.113.3"}, "203.0.113.4", 0},
		{"skips exclude", externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "203.0.113.0/29", Exclude: []string{"203.0.113.1"}}, nil, "203.0.113.2", 0},
		{"never the broadcast", pool, []string{"203.0.113.2", "203.0.113.3", "203.0.113.4", "203.0.113.5", "203.0.113.6"}, "", http.StatusConflict},
		{"range narrows the pool", withRanges("203.0.113.5-203.0.113.6"), nil, "203.0.113.5", 0},
		{"single-address range", withRanges("203.0.113.6"), nil, "203.0.113.6", 0},
		{"later range when the first is full", withRanges("203.0.113.2", "203.0.113.4-203.0.113.5"), []string{"203.0.113.2"}, "203.0.113.4", 0},
		{"range exhausted", withRanges("203.0.113.5-203.0.113.6"), []string{"203.0.113.5", "203.0.113.6"}, "", http.StatusConflict},
		{"range outside the cidr", withRanges("203.0.113.5-203.0.113.9"), nil, "", http.StatusServiceUnavailable},
		{"range reversed", withRanges("203.0.113.6-203.0.113.5"), nil, "", http.StatusServiceUnavailable},
		{"no pool", externalservice.PVEPublicNetwork{ID: "vmbr1"}, nil, "", http.StatusServiceUnavailable},
		{"invalid cidr", externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "nope"}, nil, "", http.StatusServiceUnavailable},
		{"ipv6 refused", externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "2001:db8::/64"}, nil, "", http.StatusServiceUnavailable},
		{"too small", externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "203.0.113.4/31"}, nil, "", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, prefix, herr := pickPublicIP(tt.pn, tt.used)
			if tt.wantStatus != 0 {
				if herr == nil || herr.Status != tt.wantStatus {
					t.Fatalf("got %q %v, want status %d", got, herr, tt.wantStatus)
				}
				return
			}
			if herr != nil || got != tt.want || prefix != 29 {
				t.Fatalf("got %q /%d %v, want %q /29", got, prefix, herr, tt.want)
			}
		})
	}
}

func TestAllocatePublicIPRetriesOnDuplicate(t *testing.T) {
	pn := externalservice.PVEPublicNetwork{ID: "vmbr1", CIDR: "203.0.113.0/29", Gateway: "203.0.113.1"}
	dup := &pgconn.PgError{Code: "23505"}

	// A concurrent attach takes .2 between our read and our insert: the insert
	// collides, the re-read sees .2 used and we land on .3.
	taken := []string{}
	var inserted []string
	a, err := allocatePublicIP(pn,
		func() ([]string, error) { return taken, nil },
		func(ip string, prefix int) (*PublicIP, error) {
			inserted = append(inserted, ip)
			if ip == "203.0.113.2" {
				taken = append(taken, ip)
				return nil, dup
			}
			return &PublicIP{IP: ip, Prefix: prefix}, nil
		})
	if err != nil || a.IP != "203.0.113.3" || len(inserted) != 2 {
		t.Fatalf("got %+v %v after inserts %v, want 203.0.113.3 on the 2nd insert", a, err, inserted)
	}

	// Endless collisions give up with a retryable 409.
	_, err = allocatePublicIP(pn,
		func() ([]string, error) { return nil, nil },
		func(string, int) (*PublicIP, error) { return nil, dup })
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		t.Fatalf("got %v, want 409", err)
	}

	// Any other insert error surfaces as-is.
	boom := errors.New("boom")
	if _, err = allocatePublicIP(pn,
		func() ([]string, error) { return nil, nil },
		func(string, int) (*PublicIP, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
}
