//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// promJSON renders one instant-vector sample with the given labels + value.
func promJSON(labels map[string]string, value string) map[string]any {
	return map[string]any{"metric": labels, "value": []any{1780000000.0, value}}
}

// TestMetricsJobPrometheusSource is the source=prometheus END-TO-END: the job's LIVE fetcher
// factory (not a test seam) builds the Prometheus fetcher from the seeded externalService
// config — auth header included — against a faked libvirt-exporter endpoint, and the month
// doc lands with the public/private bucket semantics.
func TestMetricsJobPrometheusSource(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Now().UTC()
	cloudRepo := cloud.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))

	var sawTenantHeader atomic.Bool // written from the httptest handler goroutine
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("X-Scope-OrgID") == "tenant-a" {
			sawTenantHeader.Store(true)
		}
		q := r.PostFormValue("query")
		var result []map[string]any
		switch {
		case strings.Contains(q, "libvirt_domain_openstack_info"):
			result = []map[string]any{promJSON(map[string]string{"domain": "instance-0a1b", "instance": "cmp1:9177"}, "1")}
		case strings.Contains(q, "count by (target_device)"):
			result = []map[string]any{promJSON(map[string]string{"target_device": "tapport-abc"}, "1")}
		case strings.Contains(q, "receive_bytes_total") && strings.Contains(q, "max_over_time"):
			result = []map[string]any{promJSON(map[string]string{}, "10485760")} // 10 MiB in
		case strings.Contains(q, "transmit_bytes_total") && strings.Contains(q, "max_over_time"):
			result = []map[string]any{promJSON(map[string]string{}, "5242880")} // 5 MiB out
		default:
			t.Errorf("unexpected prometheus query: %s", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": result},
		})
	}))
	defer fake.Close()

	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "memberships": []any{}, "services": []any{},
	})
	mustInsert(t, db, "externalService", pgdoc.M{
		"_id": "svc-prom", "type": externalservice.TypeCloud, "name": "dev",
		"config": pgdoc.M{"metrics": pgdoc.M{
			"source": "prometheus",
			"prometheus": pgdoc.M{
				"url": fake.URL, "schema": "libvirt-exporter",
				"headers": pgdoc.M{"X-Scope-OrgID": "tenant-a"},
			},
		}},
	})

	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "vm-1", ServiceID: "svc-prom", ProjectID: projectID, Type: cloud.TypeServer,
		Data: map[string]any{"server": map[string]any{"name": "vm"}}, CreatedAt: &now, UpdatedAt: &now,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}
	if _, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "port-abc-123", ServiceID: "svc-prom", ProjectID: projectID, Type: cloud.TypePort,
		Data: map[string]any{"port": map[string]any{"networkId": "net-public"}}, CreatedAt: &now, UpdatedAt: &now,
	}); err != nil {
		t.Fatalf("seed port: %v", err)
	}

	// NO WithFetcherFactory: the live factory must pick prometheus from the config.
	job := metricsjob.New(project.NewRepo(db), cloudRepo, esSvc, metrics.NewService(metricsRepo), nil).
		WithNow(func() time.Time { return now }).
		WithPublicNetworks(func(context.Context, *externalservice.ExternalService, string) ([]cloud.CloudResource, error) {
			return []cloud.CloudResource{{Type: cloud.TypeNetwork, ExternalID: "net-public"}}, nil
		})
	if err := job.Run(ctx); err != nil {
		t.Fatalf("metrics job run: %v", err)
	}

	cycleStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	saved, err := metricsRepo.FindForCurrentMonth(ctx, srv.ID, cycleStart)
	if err != nil || saved == nil {
		t.Fatalf("expected a usageMetrics doc for the server: %v", err)
	}
	d := saved.Details
	if !d.IncomingPublicTrafficMb.Equal(decimal.RequireFromString("10")) ||
		!d.OutgoingPublicTrafficMb.Equal(decimal.RequireFromString("5")) ||
		!d.TotalTrafficMb.Equal(decimal.RequireFromString("15")) {
		t.Fatalf("buckets wrong: in=%s out=%s tot=%s",
			d.IncomingPublicTrafficMb, d.OutgoingPublicTrafficMb, d.TotalTrafficMb)
	}
	if !sawTenantHeader.Load() {
		t.Fatal("X-Scope-OrgID header never reached the prometheus endpoint")
	}
	t.Logf("prometheus source: server %s → %s MB via %s", srv.ID, d.TotalTrafficMb, fake.URL)
}

// TestMetricsJobSourceNone: config.metrics.source=none skips the provider's servers silently
// — no fetcher build, no doc, no error (the fetcher factory would fail loudly if invoked).
func TestMetricsJobSourceNone(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Now().UTC()
	cloudRepo := cloud.NewRepo(db)
	metricsRepo := metrics.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("k"))

	projectID := mustInsertID(t, db, "project", pgdoc.M{
		"name": "p1", "status": project.StatusEnabled, "memberships": []any{}, "services": []any{},
	})
	mustInsert(t, db, "externalService", pgdoc.M{
		"_id": "svc-none", "type": externalservice.TypeCloud, "name": "dev",
		"config": pgdoc.M{"metrics": pgdoc.M{"source": "none"}},
	})
	srv, err := cloudRepo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "vm-2", ServiceID: "svc-none", ProjectID: projectID, Type: cloud.TypeServer,
		Data: map[string]any{"server": map[string]any{"name": "vm"}}, CreatedAt: &now, UpdatedAt: &now,
	})
	if err != nil {
		t.Fatalf("seed server: %v", err)
	}

	job := metricsjob.New(project.NewRepo(db), cloudRepo, esSvc, metrics.NewService(metricsRepo), nil).
		WithNow(func() time.Time { return now }).
		WithFetcherFactory(func(context.Context, *externalservice.ExternalService, string) (metrics.MeasureFetcher, error) {
			return nil, fmt.Errorf("fetcher factory must not be called for source=none")
		})
	if err := job.Run(ctx); err != nil {
		t.Fatalf("metrics job run: %v", err)
	}
	n, err := db.C("usageMetrics").Count(ctx, map[string]any{"resourceId": srv.ID})
	if err != nil || n != 0 {
		t.Fatalf("expected no metrics doc for source=none, got %d (%v)", n, err)
	}
}
