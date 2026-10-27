//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metricsjob"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// jobFetcher is a metrics.MeasureFetcher that returns one network interface (tap<prefix>)
// with fixed incoming/outgoing MB — no live cloud.
type jobFetcher struct {
	ifaceName  string
	incomingMB string
	outgoingMB string
}

func (f jobFetcher) SearchInstanceInterfaces(_ context.Context, _ string) ([]metrics.Resource, error) {
	return []metrics.Resource{{
		ID: "iface-1", Name: f.ifaceName,
		Metrics: map[string]string{"network.incoming.bytes": "in-m", "network.outgoing.bytes": "out-m"},
	}}, nil
}

func (f jobFetcher) MeasuresMBForCurrentMonth(_ context.Context, metricID string, _ int, _ time.Time) (decimal.Decimal, error) {
	if metricID == "in-m" {
		return decimal.RequireFromString(f.incomingMB), nil
	}
	return decimal.RequireFromString(f.outgoingMB), nil
}

// TestMetricsJobDriver exercises the Track-1.4 metrics-job walk against real Postgres with the
// live cloud seams faked: ENABLED project → its SERVER + PORT cache → resolve the server's
// ExternalService → fetch+save GnocchiMetrics. The interface's tap-prefix matches the seeded
// PORT whose networkId is in the (faked) public-network set, so the traffic lands in the
// PUBLIC buckets.
func TestMetricsJobDriver(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Now().UTC()
	cloudRepo := cloud.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))

	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "memberships": []any{}, "services": []any{},
	})
	mustInsert(t, db, "externalService", pgdoc.M{"_id": "svc-x", "type": externalservice.TypeCloud, "name": "dev", "config": pgdoc.M{}})

	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "nova-1", ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypeServer,
		Data: map[string]any{"server": map[string]any{"name": "vm"}}, CreatedAt: &now, UpdatedAt: &now,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	// A PORT on a public network; the fetcher's tap-iface prefixes this port's externalId.
	if _, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "port-abc-123", ServiceID: "svc-x", ProjectID: projectID, Type: cloud.TypePort,
		Data: map[string]any{"port": map[string]any{"networkId": "net-public"}}, CreatedAt: &now, UpdatedAt: &now,
	}); err != nil {
		t.Fatalf("seed port: %v", err)
	}

	job := metricsjob.New(project.NewRepo(db), cloudRepo, esSvc, metrics.NewService(metricsRepo), nil).
		WithNow(func() time.Time { return now }).
		WithFetcherFactory(func(context.Context, *externalservice.ExternalService, string) (metrics.MeasureFetcher, error) {
			return jobFetcher{ifaceName: "tapport-abc", incomingMB: "10", outgoingMB: "5"}, nil
		}).
		WithPublicNetworks(func(context.Context, *externalservice.ExternalService, string) ([]cloud.CloudResource, error) {
			return []cloud.CloudResource{{Type: cloud.TypeNetwork, ExternalID: "net-public"}}, nil
		})

	if err := job.Run(ctx); err != nil {
		t.Fatalf("metrics job run: %v", err)
	}

	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	saved, err := metricsRepo.FindForCurrentMonth(ctx, srv.ID, cycleStart)
	if err != nil || saved == nil {
		t.Fatalf("expected a gnocchiMetrics doc for the server: %v", err)
	}
	d := saved.Details
	if !d.IncomingPublicTrafficMb.Equal(decimal.RequireFromString("10")) ||
		!d.OutgoingPublicTrafficMb.Equal(decimal.RequireFromString("5")) ||
		!d.TotalPublicTrafficMb.Equal(decimal.RequireFromString("15")) ||
		!d.TotalTrafficMb.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("public buckets wrong: in=%s out=%s totPub=%s tot=%s",
			d.IncomingPublicTrafficMb, d.OutgoingPublicTrafficMb, d.TotalPublicTrafficMb, d.TotalTrafficMb)
	}
	if !d.TotalPrivateTrafficMb.Equal(decimal.Zero) {
		t.Fatalf("expected zero private traffic, got %s", d.TotalPrivateTrafficMb)
	}
	t.Logf("metrics job: server %s → public traffic = %s MB", srv.ID, d.TotalTrafficMb)
}
