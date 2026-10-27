//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
)

// fakeFetcher returns canned interfaces + per-metric MB, so the aggregation is tested
// without a live cloud.
type fakeFetcher struct {
	ifaces []metrics.Resource
	mbByID map[string]string // metricId → MB decimal string
}

func (f fakeFetcher) SearchInstanceInterfaces(_ context.Context, _ string) ([]metrics.Resource, error) {
	return f.ifaces, nil
}

func (f fakeFetcher) MeasuresMBForCurrentMonth(_ context.Context, metricID string, _ int, _ time.Time) (decimal.Decimal, error) {
	if s, ok := f.mbByID[metricID]; ok {
		return decimal.RequireFromString(s), nil
	}
	return decimal.Zero, nil
}

// TestFetchAndSaveGnocchiMetrics verifies the public/private bucketing + persistence: a
// server with one public-network interface and one private-network interface, each with
// incoming+outgoing MB, lands the correct details in the gnocchiMetrics doc.
func TestFetchAndSaveGnocchiMetrics(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	svc := metrics.NewService(metrics.NewRepo(db))

	server := &cloud.CloudResource{ID: "srv-1", ExternalID: "nova-uuid", Type: cloud.TypeServer}
	ports := []cloud.CloudResource{
		{ExternalID: "pub11111-port", Data: map[string]any{"port": map[string]any{"networkId": "net-pub"}}},
		{ExternalID: "prv22222-port", Data: map[string]any{"port": map[string]any{"networkId": "net-prv"}}},
	}
	publicNets := []cloud.CloudResource{{ExternalID: "net-pub"}}

	f := fakeFetcher{
		ifaces: []metrics.Resource{
			{Name: "tappub11111", Metrics: map[string]string{"network.incoming.bytes": "in-pub", "network.outgoing.bytes": "out-pub"}},
			{Name: "tapprv22222", Metrics: map[string]string{"network.incoming.bytes": "in-prv", "network.outgoing.bytes": "out-prv"}},
		},
		mbByID: map[string]string{"in-pub": "10", "out-pub": "5", "in-prv": "3", "out-prv": "2"},
	}

	start := time.Now().UTC().Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 28)
	if err := svc.FetchAndSaveGnocchiMetrics(ctx, f, server, ports, publicNets, 0, start, end); err != nil {
		t.Fatalf("fetch+save: %v", err)
	}

	m, err := metrics.NewRepo(db).FindForCurrentMonth(ctx, "srv-1", start)
	if err != nil || m == nil {
		t.Fatalf("find: %v / %v", err, m)
	}
	d := m.Details
	eq := func(label string, got decimal.Decimal, want string) {
		if !got.Equal(decimal.RequireFromString(want)) {
			t.Errorf("%s = %s, want %s", label, got.String(), want)
		}
	}
	eq("incomingPublic", d.IncomingPublicTrafficMb, "10")
	eq("outgoingPublic", d.OutgoingPublicTrafficMb, "5")
	eq("incomingPrivate", d.IncomingPrivateTrafficMb, "3")
	eq("outgoingPrivate", d.OutgoingPrivateTrafficMb, "2")
	eq("totalPublic", d.TotalPublicTrafficMb, "15")
	eq("totalPrivate", d.TotalPrivateTrafficMb, "5")
	eq("total", d.TotalTrafficMb, "20")

	// idempotent re-run: same cycle → same doc (find-or-create returns the existing one).
	if err := svc.FetchAndSaveGnocchiMetrics(ctx, f, server, ports, publicNets, 0, start, end); err != nil {
		t.Fatalf("fetch+save (2): %v", err)
	}
	n, err := db.C("gnocchiMetrics").Count(ctx, map[string]any{"resourceId": "srv-1"})
	if err != nil || n != 1 {
		t.Fatalf("expected 1 gnocchiMetrics doc, got %d (%v)", n, err)
	}
}
