// Package metricsjob is the scheduled gnocchi
// usage-ingestion driver. It walks ENABLED projects, reads each project's SERVER + PORT
// cloud resources from the cache, resolves the server's (decrypted) ExternalService, and
// per server fetches+saves the month's GnocchiMetrics via metrics.Service.
//
// The cache walk + per-server dispatch are testcontainer-verifiable (inject fake
// FetcherFactory / PublicNetworksFunc seams). The live defaults (a CloudClient+gnocchi per
// service, and external-network detection) hit the shared cloud read-only and are
// golden-verified in the gated live run.
package metricsjob

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// FetcherFactory builds the gnocchi MeasureFetcher for a service/region (live default:
// CloudClient + metrics.New). Injected so the walk is testable without a cloud.
type FetcherFactory func(ctx context.Context, es *externalservice.ExternalService, region string) (metrics.MeasureFetcher, error)

// PublicNetworksFunc resolves the public NETWORK cloud resources for a service/region
// (their ExternalID is matched by metrics.isPublicTraffic). Live default = the cloud's
// router:external networks.
type PublicNetworksFunc func(ctx context.Context, es *externalservice.ExternalService, region string) ([]cloud.CloudResource, error)

// Job is the metrics-ingestion driver.
type Job struct {
	projects      *project.Repo
	cloud         *cloud.Repo
	services      *externalservice.Service
	svc           *metrics.Service
	fetcherFor    FetcherFactory
	publicNetsFor PublicNetworksFunc
	now           func() time.Time
	log           *slog.Logger
}

// New builds the job with the live defaults (CloudClient-backed fetcher + external-network
// detection). Use the With* options in tests to inject fakes.
func New(projects *project.Repo, cloudRepo *cloud.Repo, services *externalservice.Service, svc *metrics.Service, log *slog.Logger) *Job {
	if log == nil {
		log = slog.Default()
	}
	j := &Job{
		projects: projects, cloud: cloudRepo, services: services, svc: svc,
		now: func() time.Time { return time.Now().UTC() },
		log: log,
	}
	j.fetcherFor = liveFetcherFactory
	j.publicNetsFor = livePublicNetworks
	return j
}

// WithFetcherFactory / WithPublicNetworks override the live seams (tests).
func (j *Job) WithFetcherFactory(f FetcherFactory) *Job     { j.fetcherFor = f; return j }
func (j *Job) WithPublicNetworks(f PublicNetworksFunc) *Job { j.publicNetsFor = f; return j }
func (j *Job) WithNow(now func() time.Time) *Job            { j.now = now; return j }

// Run executes one ingestion pass. Per-project and per-server failures are logged and
// skipped (each is wrapped independently) so one bad server can't stall the rest.
func (j *Job) Run(ctx context.Context) error {
	projects, err := j.projects.AllEnabled(ctx)
	if err != nil {
		return err
	}
	now := j.now()
	start, end := monthBounds(now)
	esCache := map[string]*externalservice.ExternalService{}
	for i := range projects {
		j.processProject(ctx, &projects[i], esCache, start, end)
	}
	return nil
}

func (j *Job) processProject(ctx context.Context, p *project.Project, esCache map[string]*externalservice.ExternalService, start, end time.Time) {
	servers, err := j.cloud.FindByProjectAndType(ctx, p.ID, cloud.TypeServer)
	if err != nil {
		j.log.Error("metricsjob: list servers", "project", p.ID, "err", err)
		return
	}
	ports, err := j.cloud.FindByProjectAndType(ctx, p.ID, cloud.TypePort)
	if err != nil {
		j.log.Error("metricsjob: list ports", "project", p.ID, "err", err)
		return
	}
	for i := range servers {
		j.processServer(ctx, &servers[i], ports, esCache, start, end)
	}
}

func (j *Job) processServer(ctx context.Context, server *cloud.CloudResource, ports []cloud.CloudResource, esCache map[string]*externalservice.ExternalService, start, end time.Time) {
	es, err := j.externalService(ctx, esCache, server.ServiceID)
	if err != nil {
		j.log.Error("metricsjob: resolve external service", "server", server.ID, "serviceId", server.ServiceID, "err", err)
		return
	}
	// Explicit opt-out (config.metrics.source == "none"): skip silently — a provider without
	// telemetry should not generate per-server error noise every hour.
	if es.MetricsSource() == externalservice.MetricsSourceNone {
		return
	}
	fetcher, err := j.fetcherFor(ctx, es, server.Region)
	if err != nil {
		j.log.Error("metricsjob: build fetcher", "server", server.ID, "err", err)
		return
	}
	publicNets, err := j.publicNetsFor(ctx, es, server.Region)
	if err != nil {
		j.log.Error("metricsjob: public networks", "server", server.ID, "err", err)
		return
	}
	if err := j.svc.FetchAndSaveGnocchiMetrics(ctx, fetcher, server, ports, publicNets, es.GnocchiGranularity(), start, end); err != nil {
		j.log.Error("metricsjob: fetch+save gnocchi metrics", "server", server.ID, "err", err)
	}
}

func (j *Job) externalService(ctx context.Context, cache map[string]*externalservice.ExternalService, id string) (*externalservice.ExternalService, error) {
	if es, ok := cache[id]; ok {
		return es, nil
	}
	es, err := j.services.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	cache[id] = es
	return es, nil
}

// liveFetcherFactory builds the usage-source MeasureFetcher. Prometheus is the
// only live source on this fork (gnocchi died with the OpenStack backend) — any
// other configured source errors and the job skips the provider.
func liveFetcherFactory(ctx context.Context, es *externalservice.ExternalService, region string) (metrics.MeasureFetcher, error) {
	if es.MetricsSource() == externalservice.MetricsSourcePrometheus {
		return metrics.NewPrometheus(es.PrometheusMetricsConfig())
	}
	return nil, fmt.Errorf("metrics source %q is not available (prometheus or none)", es.MetricsSource())
}

// livePublicNetworks lists the provider's public networks (config-declared on a
// proxmox provider; the traffic classifier matches by ExternalID).
func livePublicNetworks(ctx context.Context, es *externalservice.ExternalService, region string) ([]cloud.CloudResource, error) {
	out := []cloud.CloudResource{}
	if es.IsProxmox() {
		for _, pn := range es.PVENetworkConfig().PublicNetworks {
			out = append(out, cloud.CloudResource{Type: cloud.TypeNetwork, ExternalID: pn.ID, Region: region})
		}
	}
	return out, nil
}

func monthBounds(now time.Time) (time.Time, time.Time) {
	y, m, _ := now.UTC().Date()
	start := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}
