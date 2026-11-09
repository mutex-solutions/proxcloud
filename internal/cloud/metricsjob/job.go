// Package metricsjob is the scheduled
// usage-ingestion driver. It walks ENABLED projects, reads each project's SERVER + PORT
// cloud resources from the cache, resolves the server's (decrypted) ExternalService, and
// per server fetches+saves the month's UsageMetrics via metrics.Service.
//
// The cache walk + per-server dispatch are testcontainer-verifiable (inject fake
// FetcherFactory / PublicNetworksFunc seams). The live defaults (a Prometheus fetcher per
// service, and the provider's config-declared public networks) are golden-verified in the
// gated live run.
package metricsjob

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// FetcherFactory builds the MeasureFetcher for a service/region (live default:
// metrics.NewPrometheus). Injected so the walk is testable without a cloud.
type FetcherFactory func(ctx context.Context, es *externalservice.ExternalService, region string) (metrics.MeasureFetcher, error)

// PublicNetworksFunc resolves the public NETWORK cloud resources for a service/region
// (their ExternalID is matched by metrics.isPublicTraffic). Live default = the provider's
// config-declared public networks.
type PublicNetworksFunc func(ctx context.Context, es *externalservice.ExternalService, region string) ([]cloud.CloudResource, error)

// Job is the metrics-ingestion driver.
type Job struct {
	projects      *project.Repo
	cloud         *cloud.Repo
	services      *externalservice.Service
	svc           *metrics.Service
	fetcherFor    FetcherFactory
	publicNetsFor PublicNetworksFunc
	db            *pgdoc.DB
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
	j.fetcherFor = j.liveFetcher
	j.publicNetsFor = livePublicNetworks
	return j
}

// WithDB wires the store behind the Proxmox fetcher's counters (nil = Proxmox traffic unavailable).
func (j *Job) WithDB(db *pgdoc.DB) *Job { j.db = db; return j }

// WithFetcherFactory / WithPublicNetworks override the live seams (tests).
func (j *Job) WithFetcherFactory(f FetcherFactory) *Job     { j.fetcherFor = f; return j }
func (j *Job) WithPublicNetworks(f PublicNetworksFunc) *Job { j.publicNetsFor = f; return j }
func (j *Job) WithNow(now func() time.Time) *Job            { j.now = now; return j }

// Run executes one ingestion pass over every provider. Per-project and per-server failures are
// logged and skipped (each is wrapped independently) so one bad server can't stall the rest.
func (j *Job) Run(ctx context.Context) error {
	return j.run(ctx, func(string) bool { return true })
}

// RunPVE is the per-minute pass over the providers using Proxmox's native counters only: those lose
// what a counter reset falls on, so they are sampled far more often than a time-series source.
func (j *Job) RunPVE(ctx context.Context) error {
	return j.run(ctx, func(source string) bool { return source == externalservice.MetricsSourcePVE })
}

// pass is one ingestion run's state: one fetcher per provider and region, reused across its servers.
type pass struct {
	start, end time.Time
	only       func(source string) bool
	services   map[string]*externalservice.ExternalService
	fetchers   map[string]metrics.MeasureFetcher
}

func (j *Job) run(ctx context.Context, only func(string) bool) error {
	projects, err := j.projects.AllEnabled(ctx)
	if err != nil {
		return err
	}
	start, end := monthBounds(j.now())
	run := &pass{
		start: start, end: end, only: only,
		services: map[string]*externalservice.ExternalService{},
		fetchers: map[string]metrics.MeasureFetcher{},
	}
	for i := range projects {
		j.processProject(ctx, &projects[i], run)
	}
	return nil
}

func (j *Job) processProject(ctx context.Context, p *project.Project, run *pass) {
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
	fips, err := j.cloud.FindByProjectAndType(ctx, p.ID, cloud.TypeFloatingIP)
	if err != nil {
		j.log.Error("metricsjob: list public IPs", "project", p.ID, "err", err)
		return
	}
	public := serversWithPublicIP(fips)
	for i := range servers {
		j.processServer(ctx, &servers[i], ports, public, run)
	}
}

// serversWithPublicIP is the set of vmids holding a public IP, the only VMs the native source has
// anything to measure on.
func serversWithPublicIP(fips []cloud.CloudResource) map[string]bool {
	out := map[string]bool{}
	for i := range fips {
		if fip, ok := fips[i].Data["floatingIp"].(map[string]any); ok {
			if id, _ := fip["server_id"].(string); id != "" {
				out[id] = true
			}
		}
	}
	return out
}

func (j *Job) processServer(ctx context.Context, server *cloud.CloudResource, ports []cloud.CloudResource, public map[string]bool, run *pass) {
	es, err := j.externalService(ctx, run.services, server.ServiceID)
	if err != nil {
		j.log.Error("metricsjob: resolve external service", "server", server.ID, "serviceId", server.ServiceID, "err", err)
		return
	}
	// Explicit opt-out (config.metrics.source == "none"): skip silently — a provider without
	// telemetry should not generate per-server error noise every hour.
	source := es.MetricsSource()
	if source == externalservice.MetricsSourceNone || !run.only(source) {
		return
	}
	if source == externalservice.MetricsSourcePVE && !public[server.ExternalID] {
		return
	}
	key := es.ID + "/" + server.Region
	fetcher := run.fetchers[key]
	if fetcher == nil {
		if fetcher, err = j.fetcherFor(ctx, es, server.Region); err != nil {
			j.log.Error("metricsjob: build fetcher", "server", server.ID, "err", err)
			return
		}
		run.fetchers[key] = fetcher
	}
	publicNets, err := j.publicNetsFor(ctx, es, server.Region)
	if err != nil {
		j.log.Error("metricsjob: public networks", "server", server.ID, "err", err)
		return
	}
	if err := j.svc.FetchAndSaveUsageMetrics(ctx, fetcher, server, ports, publicNets, run.start, run.end); err != nil {
		j.log.Error("metricsjob: fetch+save usage metrics", "server", server.ID, "err", err)
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

// liveFetcher builds the usage-source MeasureFetcher: Proxmox's native counters or Prometheus. Any
// other configured source errors and the job skips the provider.
func (j *Job) liveFetcher(_ context.Context, es *externalservice.ExternalService, _ string) (metrics.MeasureFetcher, error) {
	switch es.MetricsSource() {
	case externalservice.MetricsSourcePrometheus:
		return metrics.NewPrometheus(es.PrometheusMetricsConfig())
	case externalservice.MetricsSourcePVE:
		if j.db == nil {
			return nil, fmt.Errorf("proxmox metrics source is not wired")
		}
		ps, err := pve.NewService(es.PVEConfig(), es.ID, es.PVEPoolPrefix())
		if err != nil {
			return nil, err
		}
		return metrics.NewPVEFetcher(ps, j.cloud, j.db, j.now), nil
	}
	return nil, fmt.Errorf("metrics source %q is not available (pve, prometheus or none)", es.MetricsSource())
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
