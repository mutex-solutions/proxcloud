package metrics

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPrometheusLiveSmoke is a READ-ONLY live check against a real Prometheus-compatible
// endpoint — skipped unless PROXCLOUD_PROM_LIVE_URL is set. Optional env:
// PROXCLOUD_PROM_LIVE_TENANT (sent as X-Scope-OrgID), PROXCLOUD_PROM_LIVE_INSTANCE (a nova uuid
// to discover + measure). Mirrors TestGnocchiSmoke: proves endpoint/auth/schema on the real
// deployment, creates nothing.
func TestPrometheusLiveSmoke(t *testing.T) {
	url := os.Getenv("PROXCLOUD_PROM_LIVE_URL")
	if url == "" {
		t.Skip("PROXCLOUD_PROM_LIVE_URL not set — skipping read-only prometheus smoke")
	}
	cfg := PrometheusConfig{URL: url}
	if tenant := os.Getenv("PROXCLOUD_PROM_LIVE_TENANT"); tenant != "" {
		cfg.Headers = map[string]string{"X-Scope-OrgID": tenant}
	}
	p, err := NewPrometheus(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := p.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	n, err := p.CountTrafficSeries(ctx, time.Time{}, time.Hour)
	if err != nil {
		t.Fatalf("count series: %v", err)
	}
	t.Logf("live: %d incoming-traffic series in the last hour", n)

	instance := os.Getenv("PROXCLOUD_PROM_LIVE_INSTANCE")
	if instance == "" {
		return
	}
	ifaces, err := p.SearchInstanceInterfaces(ctx, instance)
	if err != nil {
		t.Fatalf("discover interfaces: %v", err)
	}
	t.Logf("live: instance %s has %d interfaces", instance, len(ifaces))
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, iface := range ifaces {
		in, err := p.MeasuresMBForCurrentMonth(ctx, iface.Metrics["network.incoming.bytes"], 0, monthStart)
		if err != nil {
			t.Fatalf("measures incoming %s: %v", iface.Name, err)
		}
		out, err := p.MeasuresMBForCurrentMonth(ctx, iface.Metrics["network.outgoing.bytes"], 0, monthStart)
		if err != nil {
			t.Fatalf("measures outgoing %s: %v", iface.Name, err)
		}
		t.Logf("live: %s month-to-date incoming=%s MB outgoing=%s MB", iface.Name, in, out)
	}
}
