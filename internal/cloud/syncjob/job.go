// Package syncjob is the minimal live resource-sync driver: for each ENABLED project, for
// each CLOUD external service it is attached to, list the cloud's SERVER + PORT resources and
// upsert them into the `cloudResource` cache (stamped with the ProxCloud project id + region) so
// the metrics job + charge loop have a cache to read.
//
// This is the read-sync MVP — the full createOrUpdate dispatch (shouldBeDeleted /
// isNeededToUpdate / delete-of-vanished + the wasUserDeletedAfter guard) is a later slice.
// The CloudClient build is injectable (live default), so the walk is testable.
package syncjob

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/client"
	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/lock"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
)

// CephClientFactory builds an ADMIN-keyed ceph-s3 CloudClient scoped to a project's RGW user (uidPrefix +
// project id). Admin keys are enough for the bucket list+stats the sync/billing meter reads (no
// project S3 keys needed). Injectable so the ceph sync walk is testable against a fake.
type CephClientFactory func(ctx context.Context, es *externalservice.ExternalService, region, projectID string) (*client.Client, error)

// KamajiFactory builds the Managed-Kubernetes service for a kamaji provider (management-cluster
// client from the provider kubeconfig). Injectable so the kamaji sync walk is testable.
type KamajiFactory func(es *externalservice.ExternalService) (*kamaji.Service, error)

// DbaasFactory builds the Managed-Database service for a dbaas provider (DB-cluster client from
// the provider kubeconfig). Injectable so the dbaas sync walk is testable.
type DbaasFactory func(es *externalservice.ExternalService) (*dbaas.Service, error)

// ProxmoxFactory builds the Proxmox VE service for a proxmox provider (admin-token client).
// Injectable so the pve sync walk is testable against an httptest fake.
type ProxmoxFactory func(es *externalservice.ExternalService) (*pve.Service, error)

type Job struct {
	projects      *project.Repo
	services      *externalservice.Service
	cloud         *cloud.Repo
	cephClientFor CephClientFactory
	kamajiFor     KamajiFactory
	dbaasFor      DbaasFactory
	pveFor        ProxmoxFactory
	publicIPs     PublicIPLister
	releaseIP     PublicIPReleaser
	audit         *audit.Service
	sdnLock       *lock.ShedLock
	shareAccess   func(ctx context.Context, projectID string) error
	staleSeeds    func(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService) error
	now           func() time.Time
	log           *slog.Logger
}

func New(projects *project.Repo, services *externalservice.Service, cloudRepo *cloud.Repo, log *slog.Logger) *Job {
	if log == nil {
		log = slog.Default()
	}
	return &Job{
		projects: projects, services: services, cloud: cloudRepo,
		cephClientFor: func(ctx context.Context, es *externalservice.ExternalService, region, projectID string) (*client.Client, error) {
			return es.ObjectClient(ctx, region, "", "", es.RGWUIDFor(projectID))
		},
		kamajiFor: func(es *externalservice.ExternalService) (*kamaji.Service, error) {
			return kamaji.New(es.KamajiConfig(), es.ID)
		},
		dbaasFor: func(es *externalservice.ExternalService) (*dbaas.Service, error) {
			return dbaas.New(es.DbaasConfig(), es.ID)
		},
		pveFor: func(es *externalservice.ExternalService) (*pve.Service, error) {
			return pve.NewService(es.PVEConfig(), es.ID, es.PVEPoolPrefix())
		},
		now: func() time.Time { return time.Now().UTC() },
		log: log,
	}
}

func (j *Job) WithCephClientFactory(f CephClientFactory) *Job { j.cephClientFor = f; return j }
func (j *Job) WithKamajiFactory(f KamajiFactory) *Job         { j.kamajiFor = f; return j }
func (j *Job) WithDbaasFactory(f DbaasFactory) *Job           { j.dbaasFor = f; return j }
func (j *Job) WithProxmoxFactory(f ProxmoxFactory) *Job       { j.pveFor = f; return j }
func (j *Job) WithNow(now func() time.Time) *Job              { j.now = now; return j }

// WithSDNLock enables the isolation reconcile: it edits the rules a network change also edits, so
// it takes the same lock (nil = no reconcile).
func (j *Job) WithSDNLock(l *lock.ShedLock) *Job { j.sdnLock = l; return j }

// WithShareAccess wires the reconcile of a project's file share exports to its addresses (nil = none).
func (j *Job) WithShareAccess(f func(ctx context.Context, projectID string) error) *Job {
	j.shareAccess = f
	return j
}

// SyncOne runs the sync for a single project — the admin POST /project/{id}/sync leg.
// serviceID == "" syncs every attached CLOUD service but only
// when the project is ENABLED (gated on isProjectActive); a non-blank
// serviceID syncs just that service with NO active gate (the scoped branch has none).
// ponytail: no distributed lock — the cron sync is idempotent-upsert so a concurrent run is
// harmless. Add a datastore lock (servicesSync-{p}-{es}, ~2min) if syncs ever fight.
func (j *Job) SyncOne(ctx context.Context, projectID, serviceID string) error {
	p, err := j.projects.FindByID(ctx, projectID)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %s not found", projectID)
	}
	if serviceID == "" {
		if !p.IsEnabled() {
			return nil
		}
		j.syncProject(ctx, p, map[string]*externalservice.ExternalService{})
		return nil
	}
	es, err := j.services.Get(ctx, serviceID)
	if err != nil {
		return err
	}
	if es == nil {
		return fmt.Errorf("external service %s not found", serviceID)
	}
	if es.Type != externalservice.TypeCloud {
		return nil
	}
	j.syncService(ctx, p, es)
	return nil
}

// WithStaleSeeds wires the cleanup of seeds whose VM is gone (nil = none).
func (j *Job) WithStaleSeeds(f func(ctx context.Context, ps *pve.Service, es *externalservice.ExternalService) error) *Job {
	j.staleSeeds = f
	return j
}

// Run syncs every ENABLED project's CLOUD-service resources. Returns the total SERVER+PORT
// resources synced. Per-project / per-service failures are logged and skipped.
func (j *Job) Run(ctx context.Context) (int, error) {
	projects, err := j.projects.AllEnabled(ctx)
	if err != nil {
		return 0, err
	}
	esCache := map[string]*externalservice.ExternalService{}
	total := 0
	for i := range projects {
		total += j.syncProject(ctx, &projects[i], esCache)
	}
	j.sweepKamajiOrphans(ctx)
	j.sweepDbaasOrphans(ctx)
	j.detectProxmoxCapabilities(ctx)
	return total, nil
}

// detectProxmoxCapabilities re-detects every proxmox provider's storage capabilities once per
// sync cycle (config.capabilities), so a storage added, removed or disabled — or a node joining
// or leaving — shows up without an admin action.
func (j *Job) detectProxmoxCapabilities(ctx context.Context) {
	services, err := j.services.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		j.log.Error("syncjob: list services for pve capability detection", "err", err)
		return
	}
	for i := range services {
		es := &services[i]
		if es.IsDisabled() || !es.IsProxmox() {
			continue
		}
		ps, err := j.pveFor(es)
		if err != nil {
			j.log.Error("syncjob: build pve client for capability detection", "serviceId", es.ID, "err", err)
			continue
		}
		if _, err := j.services.DetectPVECapabilities(ctx, es, ps.C); err != nil {
			j.log.Warn("syncjob: pve capability detection", "serviceId", es.ID, "err", err)
		}
		if j.staleSeeds != nil {
			if err := j.staleSeeds(ctx, ps, es); err != nil {
				j.log.Warn("syncjob: drop stale seeds", "serviceId", es.ID, "err", err)
			}
		}
	}
}

func (j *Job) syncProject(ctx context.Context, p *project.Project, esCache map[string]*externalservice.ExternalService) int {
	count := 0
	for _, serviceID := range p.ServiceIDs() {
		es := esCache[serviceID]
		if es == nil {
			got, err := j.services.Get(ctx, serviceID)
			if err != nil {
				j.log.Error("syncjob: resolve external service", "project", p.ID, "serviceId", serviceID, "err", err)
				continue
			}
			esCache[serviceID] = got
			es = got
		}
		if es.Type != externalservice.TypeCloud {
			continue
		}
		count += j.syncService(ctx, p, es)
	}
	return count
}

func (j *Job) syncService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	// ceph-s3: sync only the object-store (buckets) via an admin-keyed client.
	if es.IsObjectStore() {
		return j.syncCephService(ctx, p, es)
	}
	// kamaji: sync only the managed k8s clusters off the management cluster.
	if es.IsKamaji() {
		return j.syncKamajiService(ctx, p, es)
	}
	// dbaas: sync only the managed databases off the DB cluster.
	if es.IsDbaas() {
		return j.syncDbaasService(ctx, p, es)
	}
	// proxmox: sync the project pool's VMs, disks, templates, networks and security groups.
	if es.IsProxmox() {
		return j.syncProxmoxService(ctx, p, es)
	}
	if es.IsTrueNAS() {
		return j.syncTrueNASService(ctx, p, es)
	}
	if es.IsPowerDNS() {
		return j.syncPowerDNSService(ctx, p, es)
	}
	// No other provider kinds exist.
	return 0
}

// syncCephService reconciles the project's ceph-s3 buckets (the only synced type for a ceph provider): an
// admin-keyed client lists the buckets OWNED BY this project's RGW user (admin-ops `uid` filter) WITH
// stats, so the BUCKET cache carries the {objectCount, sizeInBytes, sizeInGb} the existing "bucket"
// pricing rules rate. That uid filter IS the leak-guard: it returns only this project's buckets, even
// though the RGW bucket namespace is global.
func (j *Job) syncCephService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.CephRegion()
	// Honour the provider's Services-tab toggle.
	if !es.ServiceEnabledInRegion("object-store", region) {
		return 0
	}
	cc, err := j.cephClientFor(ctx, es, region, p.ID)
	if err != nil {
		j.log.Error("syncjob: build ceph client", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
		return 0
	}
	st, err := providers.Reconcile(ctx, providers.NewBucketProvider(cc, region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile ceph buckets", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
	}
	return st.Created + st.Updated
}

// syncTrueNASService reconciles the project's file shares and their usage off the NAS.
func (j *Job) syncTrueNASService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.CephRegion()
	if !es.ServiceEnabledInRegion("shares", region) {
		return 0
	}
	ts, err := es.TrueNASService()
	if err != nil {
		j.log.Error("syncjob: build truenas client", "project", p.ID, "serviceId", es.ID, "err", err)
		return 0
	}
	defer ts.C.Close()
	st, err := providers.Reconcile(ctx, ts.ShareSyncProvider(region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile shares", "project", p.ID, "serviceId", es.ID, "err", err)
	}
	if j.shareAccess != nil {
		if err := j.shareAccess(ctx, p.ID); err != nil {
			j.log.Error("syncjob: reconcile share access", "project", p.ID, "serviceId", es.ID, "err", err)
		}
	}
	return st.Created + st.Updated
}

// syncPowerDNSService reconciles the project's DNS zones, by the account field on the server.
func (j *Job) syncPowerDNSService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.CephRegion()
	if !es.ServiceEnabledInRegion("dns", region) {
		return 0
	}
	ds, err := es.PowerDNSService()
	if err != nil {
		j.log.Error("syncjob: build powerdns client", "project", p.ID, "serviceId", es.ID, "err", err)
		return 0
	}
	st, err := providers.Reconcile(ctx, ds.ZoneSyncProvider(region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile zones", "project", p.ID, "serviceId", es.ID, "err", err)
	}
	return st.Created + st.Updated
}

// syncProxmoxService reconciles the project's SERVER, VOLUME, IMAGE, NETWORK, SECURITY_GROUP and
// FLOATING_IP resources off the PVE cluster and the public-IP allocations. The project-pool filter is the leak-guard: only this project's pool members are
// listed, and Reconcile's delete-of-vanished scan is ProjectScoped.
func (j *Job) syncProxmoxService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.PVERegion()
	if !es.ServiceEnabledInRegion("compute", region) {
		return 0
	}
	ps, err := j.pveFor(es)
	if err != nil {
		j.log.Error("syncjob: build pve client", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
		return 0
	}
	count := 0
	now := j.now()
	provs := []providers.Provider{
		ps.ServerSyncProvider(region, p.ID, es.PVEGPUAliasOf),
		ps.VolumeSyncProvider(region, p.ID, es.PVEVolumeProfileFunc()),
		ps.ImageSyncProvider(region, p.ID),
		ps.NetworkSyncProvider(region, p.ID),
		ps.SecurityGroupSyncProvider(region, p.ID),
		j.floatingIPProvider(region, p.ID, es.ID),
	}
	for _, prov := range provs {
		st, err := providers.Reconcile(ctx, prov, j.cloud, es.ID, now)
		if err != nil {
			j.log.Error("syncjob: reconcile pve", "project", p.ID, "serviceId", es.ID, "region", region, "type", prov.Type(), "err", err)
		}
		count += st.Created + st.Updated
	}
	j.reconcileIsolation(ctx, ps, p, es)
	return count
}

// reconcileIsolation brings the project's firewall rules to the current rule set: a project made
// before a rule change gets it here instead of at its next network change, and a rule deleted by
// hand comes back. A busy lock skips this project until the next cycle.
// ponytail: one short lock per project per cycle, so a network create can hit the usual "retry in a
// moment" 409 during a sync. Take it only on detected drift if that ever shows up on a big cluster.
func (j *Job) reconcileIsolation(ctx context.Context, ps *pve.Service, p *project.Project, es *externalservice.ExternalService) {
	netCfg := es.PVENetworkConfig()
	if j.sdnLock == nil || netCfg.CIDRPool == "" {
		return
	}
	start := j.now()
	ok, err := j.sdnLock.Lock(ctx, project.SDNApplyLockName, project.SDNApplyMaxFor, start)
	if err != nil || !ok {
		return
	}
	defer func() { _ = j.sdnLock.Unlock(ctx, project.SDNApplyLockName, 0, start, j.now()) }()
	if err := ps.ReconcileIsolation(ctx, p.ID, netCfg.CIDRPool, netCfg.BlockedCIDRs); err != nil {
		j.log.Error("syncjob: reconcile isolation", "project", p.ID, "serviceId", es.ID, "err", err)
	}
}

// syncKamajiService reconciles the project's KUBERNETES_CLUSTER resources off the kamaji
// management cluster (Applications labelled with the project id, enriched from the TCP +
// MachineDeployments). The project-label filter is the leak-guard: only this project's
// clusters are listed, and Reconcile's delete-of-vanished scan is ProjectScoped.
func (j *Job) syncKamajiService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.KamajiRegion()
	if !es.ServiceEnabledInRegion("kubernetes", region) {
		return 0
	}
	ks, err := j.kamajiFor(es)
	if err != nil {
		j.log.Error("syncjob: build kamaji client", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
		return 0
	}
	st, err := providers.Reconcile(ctx, ks.SyncProvider(region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile kamaji clusters", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
	}
	return st.Created + st.Updated
}

// sweepKamajiOrphans runs each kamaji provider's service-level orphan sweep once per sync cycle:
// deleted clusters whose ArgoCD cascade has finished get their cloud credentials secret reaped
// and (when the project namespace empties out) the namespace GC'd. Service-level
// on purpose — it must also reap leftovers of projects whose proxcloud doc is already gone
// (scheduled deletion, teardown), which no per-project walk can visit.
func (j *Job) sweepKamajiOrphans(ctx context.Context) {
	services, err := j.services.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		j.log.Error("syncjob: list services for kamaji sweep", "err", err)
		return
	}
	for i := range services {
		es := &services[i]
		if es.IsDisabled() || !es.IsKamaji() {
			continue
		}
		ks, err := j.kamajiFor(es)
		if err != nil {
			j.log.Error("syncjob: build kamaji service for sweep", "serviceId", es.ID, "err", err)
			continue
		}
		pending, err := ks.FinalizeAllOrphans(ctx)
		if err != nil {
			j.log.Warn("syncjob: kamaji orphan sweep", "serviceId", es.ID, "pending", pending, "err", err)
		}
	}
}

// syncDbaasService reconciles the project's DATABASE_CLUSTER resources off the DB cluster
// (Applications labelled with the project id, enriched with the LB Service's ingress IP). The
// project-label filter is the leak-guard, same as kamaji.
func (j *Job) syncDbaasService(ctx context.Context, p *project.Project, es *externalservice.ExternalService) int {
	region := es.DbaasRegion()
	if !es.ServiceEnabledInRegion("database", region) {
		return 0
	}
	ds, err := j.dbaasFor(es)
	if err != nil {
		j.log.Error("syncjob: build dbaas client", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
		return 0
	}
	st, err := providers.Reconcile(ctx, ds.SyncProvider(region, p.ID), j.cloud, es.ID, j.now())
	if err != nil {
		j.log.Error("syncjob: reconcile dbaas databases", "project", p.ID, "serviceId", es.ID, "region", region, "err", err)
	}
	// Autoscale tick rides the same cadence: VPA recommendation (brain) → bounded values patch
	// (hand) — declared size moves, so billing follows automatically. Disk leg only when the
	// provider carries a Prometheus metrics config (kubelet volume-stats fill ratios).
	var diskUsage dbaas.DiskUsageFunc
	if es.MetricsSource() == externalservice.MetricsSourcePrometheus {
		if prom, perr := metrics.NewPrometheus(es.PrometheusMetricsConfig()); perr == nil {
			diskUsage = func(ctx context.Context, ns string) (map[string]float64, error) {
				q := fmt.Sprintf(`max by (persistentvolumeclaim) (kubelet_volume_stats_used_bytes{namespace=%q} / kubelet_volume_stats_capacity_bytes{namespace=%q})`, ns, ns)
				return prom.InstantValues(ctx, q, "persistentvolumeclaim")
			}
		} else {
			j.log.Warn("syncjob: dbaas prometheus client", "serviceId", es.ID, "err", perr)
		}
	}
	if bumped, aerr := ds.AutoscaleTick(ctx, p.ID, diskUsage); aerr != nil {
		j.log.Warn("syncjob: dbaas autoscale tick", "project", p.ID, "serviceId", es.ID, "bumped", bumped, "err", aerr)
	}
	return st.Created + st.Updated
}

// sweepDbaasOrphans runs each dbaas provider's service-level orphan sweep once per sync cycle:
// deleted databases whose ArgoCD cascade has finished (LB Service gone) get their marker + auth
// secrets reaped and (when the project namespace empties out) the namespace GC'd.
func (j *Job) sweepDbaasOrphans(ctx context.Context) {
	services, err := j.services.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		j.log.Error("syncjob: list services for dbaas sweep", "err", err)
		return
	}
	for i := range services {
		es := &services[i]
		if es.IsDisabled() || !es.IsDbaas() {
			continue
		}
		ds, err := j.dbaasFor(es)
		if err != nil {
			j.log.Error("syncjob: build dbaas service for sweep", "serviceId", es.ID, "err", err)
			continue
		}
		pending, err := ds.FinalizeAllOrphans(ctx)
		if err != nil {
			j.log.Warn("syncjob: dbaas orphan sweep", "serviceId", es.ID, "pending", pending, "err", err)
		}
	}
}
