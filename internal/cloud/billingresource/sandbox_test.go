package billingresource

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/pkg/billingapi"
)

func TestSandboxBilling(t *testing.T) {
	p := NewSandboxProvider()
	row := func(status string) *cloud.CloudResource {
		return &cloud.CloudResource{ID: "r1", ProjectID: "p1", Type: cloud.TypeSandbox, ExternalID: "sbx-1", Data: map[string]any{
			"name": "agent", "template": "pytorch", "status": status, "runningSince": "2026-11-05T10:00:00Z",
			"cpu": float64(4), "memoryGiB": float64(16), "gpuCount": float64(1), "gpuModel": "nvidia-3090",
		}}
	}
	out, err := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, row("RUNNING"))
	if err != nil || len(out) != 1 {
		t.Fatalf("out=%v err=%v", out, err)
	}
	r := out[0]
	if r.ResourceType != "sandbox" || r.NotEligibleForBilling || r.Values["vcpus"] != float64(4) || r.Values["ram_gb"] != float64(16) ||
		r.Values["gpu_count"] != float64(1) || r.Values["gpu_model"] != "nvidia-3090" || r.Values["display_name"] != "agent" {
		t.Errorf("running: %+v", r)
	}
	// PENDING (image pull / waiting for a GPU) yields no billing resource at all, so the engine
	// never creates an item whose watermark would back-bill the wait.
	if out, _ := p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, row("PENDING")); len(out) != 0 {
		t.Errorf("PENDING sandbox must not be billed: %+v", out)
	}
	cr := row("RUNNING")
	_, _ = p.GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if cr.Info == nil || cr.Info.CreatedAt == nil || cr.Info.CreatedAt.Format("15:04:05") != "10:00:00" {
		t.Errorf("billing start must be runningSince: %+v", cr.Info)
	}
	found := false
	for _, typ := range Catalog() {
		found = found || typ.ResourceType == "sandbox"
	}
	if !found {
		t.Error("sandbox missing from Catalog()")
	}
}

// An ended sandbox is rated up to its runtime rounded up to whole minutes, at least one.
func TestSandboxBillingEnd(t *testing.T) {
	rs := time.Date(2026, 11, 6, 1, 0, 30, 0, time.UTC)
	for ended, want := range map[string]time.Duration{
		"2026-11-06T01:00:40Z": time.Minute,     // 10 s → 1 min
		"2026-11-06T01:01:30Z": time.Minute,     // exactly 1 min
		"2026-11-06T01:01:31Z": 2 * time.Minute, // 61 s → 2 min
		"garbage":              time.Minute,
	} {
		if got := billedEnd(rs, ended).Sub(rs); got != want {
			t.Errorf("ended %s: billed %v, want %v", ended, got, want)
		}
	}
	cr := &cloud.CloudResource{ID: "r1", ProjectID: "p1", ExternalID: "sbx-1", Data: map[string]any{
		"status": "DELETED", "runningSince": "2026-11-06T01:00:30Z", "endedAt": "2026-11-06T01:00:40Z", "cpu": 1, "memoryGiB": 2,
	}}
	res, err := NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	if err != nil || len(res) != 1 || res[0].DeletedAt == nil || !res[0].DeletedAt.Equal(rs.Add(time.Minute)) {
		t.Fatalf("deleted sandbox billing = %+v, %v", res, err)
	}
}

// A RUNNING row whose shutdownTime has passed is charged only up to it.
func TestSandboxBillingStopsAtShutdown(t *testing.T) {
	cr := &cloud.CloudResource{ID: "r1", Data: map[string]any{
		"status": "RUNNING", "runningSince": "2026-11-06T01:00:00Z", "shutdownTime": "2026-11-06T01:10:00Z",
	}}
	res, _ := NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr)
	want := time.Date(2026, 11, 6, 1, 10, 0, 0, time.UTC)
	if len(res) != 1 || res[0].DeletedAt == nil || !res[0].DeletedAt.Equal(want) {
		t.Fatalf("billing past shutdownTime: %+v", res)
	}
	cr.Data["shutdownTime"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if res, _ := NewSandboxProvider().GetBillingInformation(context.Background(), billingapi.BillingContext{}, cr); res[0].DeletedAt != nil {
		t.Fatal("a live sandbox has no end")
	}
}

// An archived sandbox stops at its own end, not at the archive time minutes later.
func TestArchivedEndKeepsTheEarlierEnd(t *testing.T) {
	ended := time.Date(2026, 11, 6, 1, 1, 0, 0, time.UTC)
	archived := ended.Add(3 * time.Minute)
	if got := archivedEnd(&ended, &archived); !got.Equal(ended) {
		t.Errorf("provider end %v, archived %v: got %v", ended, archived, got)
	}
	if got := archivedEnd(nil, &archived); !got.Equal(archived) {
		t.Errorf("no provider end: got %v", got)
	}
	late := archived.Add(time.Hour)
	if got := archivedEnd(&late, &archived); !got.Equal(archived) {
		t.Errorf("a provider end after the archive: got %v", got)
	}
}
