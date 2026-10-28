package project

import (
	"errors"
	"net/http"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
)

func TestCheckPVEQuota(t *testing.T) {
	inUse := pveQuotaDemand{VCPUs: 6, RAMMb: 4096, Servers: 2, Networks: 1, DiskGb: 40}
	ok := func() (pveQuotaDemand, error) { return inUse, nil }
	boom := func() (pveQuotaDemand, error) { return pveQuotaDemand{}, errors.New("db down") }
	fipOK := func() (int, error) { return 1, nil }
	fipBoom := func() (int, error) { return 0, errors.New("db down") }
	tests := []struct {
		name   string
		limits map[string]any
		d      pveQuotaDemand
		usage  func() (pveQuotaDemand, error)
		fip    func() (int, error)
		want   int // 0 = allowed
	}{
		{"no quota = unlimited, usage never read", nil, pveQuotaDemand{VCPUs: 99}, boom, fipBoom, 0},
		{"within limits", map[string]any{"vcpus": float64(8), "servers": float64(3)}, pveQuotaDemand{VCPUs: 2, Servers: 1}, ok, fipOK, 0},
		{"vcpus exceeded", map[string]any{"vcpus": float64(8)}, pveQuotaDemand{VCPUs: 4}, ok, fipOK, http.StatusConflict},
		{"networks exceeded", map[string]any{"networks": 1}, pveQuotaDemand{Networks: 1}, ok, fipOK, http.StatusConflict},
		{"zero limit blocks", map[string]any{"diskGb": float64(0)}, pveQuotaDemand{DiskGb: 1}, ok, fipOK, http.StatusConflict},
		{"absent key unlimited", map[string]any{"servers": float64(10)}, pveQuotaDemand{VCPUs: 999}, boom, fipBoom, 0},
		{"unrequested limited key skips usage", map[string]any{"vcpus": float64(1)}, pveQuotaDemand{DiskGb: 5}, boom, fipBoom, 0},
		{"shrinking resize never blocks", map[string]any{"vcpus": float64(1)}, pveQuotaDemand{VCPUs: -4}, boom, fipBoom, 0},
		{"usage error fails closed", map[string]any{"ramMb": float64(1 << 20)}, pveQuotaDemand{RAMMb: 1}, boom, fipOK, http.StatusServiceUnavailable},
		{"floating ip within", map[string]any{"floatingIps": float64(2)}, pveQuotaDemand{FloatingIPs: 1}, boom, fipOK, 0},
		{"floating ip exceeded", map[string]any{"floatingIps": float64(1)}, pveQuotaDemand{FloatingIPs: 1}, boom, fipOK, http.StatusConflict},
		{"floating ip count error fails closed", map[string]any{"floatingIps": float64(5)}, pveQuotaDemand{FloatingIPs: 1}, ok, fipBoom, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			herr := checkPVEQuota(tt.limits, tt.d, tt.usage, tt.fip)
			if tt.want == 0 {
				if herr != nil {
					t.Fatalf("got %v, want allowed", herr)
				}
				return
			}
			if herr == nil || herr.Status != tt.want {
				t.Fatalf("got %v, want status %d", herr, tt.want)
			}
		})
	}
}

func TestPVEQuotaLimits(t *testing.T) {
	withDefault := &externalservice.ExternalService{Config: map[string]any{
		"provisioning": map[string]any{"quota": map[string]any{"vcpus": float64(16)}},
	}}
	noDefault := &externalservice.ExternalService{Config: map[string]any{}}
	own := map[string]any{"servers": float64(1)}

	if got := pveQuotaLimits(&Project{Quota: own}, withDefault); got["servers"] != float64(1) || got["vcpus"] != nil {
		t.Fatalf("project quota must win whole, got %v", got)
	}
	if got := pveQuotaLimits(&Project{}, withDefault); got["vcpus"] != 16 {
		t.Fatalf("empty project quota must fall back to the provider default, got %v", got)
	}
	if got := pveQuotaLimits(&Project{}, noDefault); len(got) != 0 {
		t.Fatalf("neither set = unlimited, got %v", got)
	}
}

// TestPVEResizeDemand: a resize charges the flavor delta INCLUDING the boot-disk
// growth (ResizeServer grows the disk), so a bigger-disk flavor cannot dodge diskGb.
func TestPVEResizeDemand(t *testing.T) {
	cr := &cloud.CloudResource{Data: map[string]any{"server": map[string]any{
		"flavor": map[string]any{"vcpus": 2, "ram": int64(4096), "disk": float64(20)},
	}}}
	got := pveResizeDemand(cr, &pveflavor.Flavor{VCPUs: 4, RAMMb: 8192, DiskGb: 80})
	if want := (pveQuotaDemand{VCPUs: 2, RAMMb: 4096, DiskGb: 60}); got != want {
		t.Fatalf("grow: got %+v, want %+v", got, want)
	}
	// The disk delta reaches the gate: 60 more GB over a 50 GB limit (20 in use) → 409.
	usage := func() (pveQuotaDemand, error) { return pveQuotaDemand{DiskGb: 20}, nil }
	if herr := checkPVEQuota(map[string]any{"diskGb": float64(50)}, got, usage, nil); herr == nil || herr.Status != http.StatusConflict {
		t.Fatalf("disk growth over quota: got %v, want 409", herr)
	}
	// A smaller flavor never demands anything (negative deltas are skipped).
	shrink := pveResizeDemand(cr, &pveflavor.Flavor{VCPUs: 1, RAMMb: 1024, DiskGb: 10})
	if herr := checkPVEQuota(map[string]any{"vcpus": float64(0), "ramMb": float64(0), "diskGb": float64(0)}, shrink, usage, nil); herr != nil {
		t.Fatalf("shrink: got %v, want allowed", herr)
	}
}
