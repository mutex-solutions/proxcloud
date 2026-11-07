package project

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/pveflavor"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func TestRestoreVolumesOfOneServer(t *testing.T) {
	at, older, later := time.Unix(100, 0), time.Unix(50, 0), time.Unix(150, 0)
	rows := []cloud.CloudResource{
		{ExternalID: "z:a", CreatedAt: &at, Data: map[string]any{"volume": map[string]any{"vmid": 101, "name": "web-data"}}},
		{ExternalID: "z:b", CreatedAt: &at, Data: map[string]any{"volume": map[string]any{"vmid": float64(202), "name": "other"}}},
		{ExternalID: "z:c", Data: map[string]any{"volume": map[string]any{"vmid": 101}}},
		{ExternalID: "z:d", CreatedAt: &at, DataSince: &older, Data: map[string]any{"volume": map[string]any{"vmid": 101, "name": "restored"}}},
		// An old row that took this name only later: a retype, which can land on the name of a disk that left.
		{ExternalID: "z:e", CreatedAt: &older, ExternalIDSince: &later, Data: map[string]any{"volume": map[string]any{"vmid": 101, "name": "retyped"}}},
		{ExternalID: "z:f", CreatedAt: &at, DataSince: &older, ExternalIDSince: &later, Data: map[string]any{"volume": map[string]any{"vmid": 101, "name": "both"}}},
	}
	got := pveRestoreVolumes(rows, "101")
	if len(got) != 4 || got["z:a"].Name != "web-data" || !got["z:a"].Since.Equal(at) || !got["z:d"].Since.Equal(older) ||
		!got["z:e"].Since.Equal(later) || !got["z:f"].Since.Equal(later) {
		t.Fatalf("%+v", got)
	}
}

// A retry after a failed restore: rows that still count the server, or that a sync already removed, add up the same.
func TestRowsDemandAfterAFailedRestore(t *testing.T) {
	archive := pve.VMConfig{"cores": "2", "memory": "2048", "scsi0": "z:a,size=8G", "scsi1": "z:b,size=4G"}
	srv := func(cpu, ram int) *cloud.CloudResource {
		return &cloud.CloudResource{ExternalID: "101", Data: map[string]any{"server": map[string]any{"flavor": map[string]any{"vcpus": cpu, "ram": ram}}}}
	}
	vols := []cloud.CloudResource{
		{Data: map[string]any{"volume": map[string]any{"vmid": 101, "size": 8}}},
		{Data: map[string]any{"volume": map[string]any{"vmid": 101, "size": 4}}},
		{Data: map[string]any{"volume": map[string]any{"vmid": 202, "size": 100}}},
	}
	if v, r, d := pveRowsDemand(srv(2, 2048), vols, archive); v != 0 || r != 0 || d != 0 {
		t.Fatalf("rows still there: %d %d %d", v, r, d)
	}
	if v, r, d := pveRowsDemand(srv(1, 128), vols[2:], archive); v != 1 || r != 1920 || d != 12 {
		t.Fatalf("rows gone: %d %d %d", v, r, d)
	}
}

// A restore that brings a card back is a GPU demand; one the server already holds is not. The repo is nil, so reaching
// the usage read answers 503: that is how the test tells a demand from none.
func TestRestoreGPUDemand(t *testing.T) {
	es := &externalservice.ExternalService{Config: map[string]any{"gpus": map[string]any{"map1": map[string]any{"alias": "nvidia-p40"}}}}
	proj := &Project{Quota: map[string]any{"gpu": map[string]any{"nvidia-p40": float64(1)}}}
	h := &Handler{}
	noGPU := pve.VMConfig{"cores": "2"}
	oneGPU := pve.VMConfig{"hostpci0": "mapping=map1,pcie=1"}
	if err := h.enforcePVEGPURestore(context.Background(), proj, es, oneGPU, oneGPU); err != nil {
		t.Fatalf("the server's own card: %v", err)
	}
	if err := h.enforcePVEGPURestore(context.Background(), proj, es, oneGPU, noGPU); err != nil {
		t.Fatalf("dropping a card: %v", err)
	}
	err := h.enforcePVEGPURestore(context.Background(), proj, es, noGPU, oneGPU)
	var he *httpx.HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusServiceUnavailable {
		t.Fatalf("a card coming back must be checked: %v", err)
	}
}

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
