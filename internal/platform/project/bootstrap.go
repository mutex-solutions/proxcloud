package project

// bootstrap.go holds the cloud-bootstrap slice: when a project is enabled it is provisioned onto
// every non-disabled CLOUD external service whose attach policy is on (config.attachToNewProjects,
// default on) — a PVE pool (proxmox), an RGW user (ceph-s3), a management/DB-cluster namespace
// (kamaji/dbaas) — and each binding is recorded as a ProjectExternalService on the project.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// EnableAndBootstrap exposes the bootstrap for cross-package orchestration (billing
// activation activates projects; the bootstrap is service-public).
func (h *Handler) EnableAndBootstrap(ctx context.Context, p *Project) error {
	return h.enableAndBootstrap(ctx, p)
}

// A provider that cannot be reached must not hold up the others, so one attempt is bounded, and a failed one is retried
// when the project is next opened but not on every open.
var (
	provisionAttemptTimeout = 45 * time.Second
	provisionRetryAfter     = 5 * time.Minute
)

// provisionRecordTimeout bounds writing down a failure. It has its own context: the attempt's may be what just ran out.
const provisionRecordTimeout = 5 * time.Second

// ProvisionByOperator is the admin API's provision: the operator's enable of what it asks to provision, so it also ends an
// operator's disable. A DISABLED project already provisioned has nothing to provision and may hold paused VMs, so it stays:
// enabling that is the operator's enable, which resumes them.
func (h *Handler) ProvisionByOperator(ctx context.Context, p *Project) error {
	if p.IsDisabled() && p.bootstrapped() {
		return nil
	}
	if p.IsDisabled() {
		if err := h.svc.EnableIfDisabled(ctx, p, true); err != nil {
			return err
		}
	}
	return h.enableAndBootstrap(ctx, p)
}

// enableAndBootstrap flips a DISABLED project to ENABLED (never an operator's disable) and provisions it onto the non-disabled
// CLOUD services whose attach policy is on. The caller knows billing allows it. The enable is its own conditional write, made before any provider call:
// the saves after those calls hold a read that may be minutes old, and a status change landing during them must stand.
// One provider failing leaves the others provisioned: the failure goes on the project, for a retry, and on the provider's row,
// for the operator. It is an error only when the project ends up on no provider at all.
// Idempotent: a project that already has an attached service is only enabled, but for what failed.
func (h *Handler) enableAndBootstrap(ctx context.Context, p *Project) error {
	if h.esSvc == nil {
		return nil
	}
	if !p.IsEnabled() {
		if err := CheckTransition(ActionEnable, p.Status); err != nil {
			return err
		}
		if err := h.svc.EnableIfDisabled(ctx, p, false); err != nil {
			return err
		}
	}
	// Already provisioned (the project carries its attached services) → this is a re-entry; the
	// provider-side setup was done at first bootstrap. Skip ALL the live calls — they are idempotent
	// setup, not per-entry work, and re-running them on every project-open is both wasteful and a
	// failure surface (a provider config change must not 500 an already-working project).
	// A binding an operator attach added before the first bootstrap does not count (Project.bootstrapped).
	// The lock is taken only when there is something to do: a first bootstrap, or a recorded failure due for its retry.
	if p.bootstrapped() && !retryDue(p) {
		return nil
	}
	// Under the project's attach lock, on the bindings and failures as stored now, so an operator attach (per project or
	// bulk) or a concurrent open landing meanwhile is neither lost nor done twice.
	return h.withAttachLock(ctx, p.ID, func(lock *attachLock) error {
		if h.hasRepo() {
			fresh, err := h.svc.repo.FindByID(ctx, p.ID)
			if err != nil {
				return err
			}
			if fresh != nil {
				// A deletion that landed meanwhile stands, and nothing new is created for a project on its way out.
				if fresh.Deleting() {
					return nil
				}
				p.Services, p.ProvisionFailures, p.Quota = fresh.Services, fresh.ProvisionFailures, fresh.Quota
			}
		}
		if p.bootstrapped() && !retryDue(p) {
			return nil
		}
		return h.provisionAll(withBindIntent(ctx, bindIntent{lock: lock}), p)
	})
}

// retryDue reports a recorded provisioning failure whose retry is due.
func retryDue(p *Project) bool {
	for _, f := range p.ProvisionFailures {
		if time.Since(f.At) >= provisionRetryAfter {
			return true
		}
	}
	return false
}

func (h *Handler) hasRepo() bool {
	return h.svc != nil && h.svc.repo != nil && h.svc.repo.col != nil && h.svc.repo.col.DB() != nil
}

// provisionAll is the provisioning part of enableAndBootstrap, run under the project's attach lock.
func (h *Handler) provisionAll(ctx context.Context, p *Project) error {
	attached := p.bootstrapped()
	services, err := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return err
	}
	var cephES, kamajiES, dbaasES, pveES, truenasES, dnsES, sandboxES []*externalservice.ExternalService
	for i := range services {
		// A provider whose attach policy is off is not provisioned automatically (config.attachToNewProjects): like a
		// disabled one it is left out here, so a failure recorded while it was on is not retried either. The operator
		// attaches it per project or in bulk (internal/providerattach).
		if services[i].IsDisabled() || !services[i].AttachToNewProjects() {
			continue
		}
		switch {
		case services[i].IsObjectStore():
			cephES = append(cephES, &services[i])
		case services[i].IsKamaji():
			kamajiES = append(kamajiES, &services[i])
		case services[i].IsDbaas():
			dbaasES = append(dbaasES, &services[i])
		case services[i].IsProxmox():
			pveES = append(pveES, &services[i])
		case services[i].IsTrueNAS():
			truenasES = append(truenasES, &services[i])
		case services[i].IsPowerDNS():
			dnsES = append(dnsES, &services[i])
		case services[i].IsSandbox():
			sandboxES = append(sandboxES, &services[i])
		}
	}
	if len(cephES) == 0 && len(kamajiES) == 0 && len(dbaasES) == 0 && len(pveES) == 0 && len(truenasES) == 0 && len(dnsES) == 0 && len(sandboxES) == 0 {
		// No provider to provision onto (none configured, or none with the policy on): this still is the first
		// bootstrap, so marked operator bindings become ordinary ones.
		return h.clearMarks(context.WithoutCancel(ctx), p)
	}
	changed := false
	// A failure on a provider that has since gone or been disabled is nothing to retry.
	live := map[string]bool{}
	var lastErr error
	for _, es := range slices.Concat(cephES, kamajiES, dbaasES, pveES, truenasES, dnsES, sandboxES) {
		live[es.ID] = true
		failed, known := p.ProvisionFailures[es.ID]
		switch {
		case attached && !known: // a provider added after this project's bootstrap: the operator attaches it
			continue
		case known && p.HasService(es.ID): // attached by the operator meanwhile
			delete(p.ProvisionFailures, es.ID)
			changed = true
			continue
		case known && time.Since(failed.At) < provisionRetryAfter:
			continue
		}
		if err := h.provisionOn(ctx, p, es); err != nil {
			lastErr = err
		}
		changed = true
	}
	for id := range p.ProvisionFailures {
		if !live[id] {
			delete(p.ProvisionFailures, id)
			changed = true
		}
	}
	if changed {
		if err := h.setFailures(context.WithoutCancel(ctx), p); err != nil {
			return err
		}
	}
	// The first bootstrap ran: bindings an operator attach added before it are now ordinary ones.
	if err := h.clearMarks(context.WithoutCancel(ctx), p); err != nil {
		return err
	}
	if len(p.ServiceIDs()) > 0 {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	// On no provider, with the retry not due yet: nothing was tried, but the project still does not work.
	for _, f := range p.ProvisionFailures {
		return fmt.Errorf("project is on no provider, retrying later: %s", f.Message)
	}
	return nil
}

// setFailures writes the project's provisioning failures (alone: never the bindings).
func (h *Handler) setFailures(ctx context.Context, p *Project) error {
	if !h.hasRepo() {
		return nil
	}
	return h.svc.repo.SetProvisionFailures(ctx, p.ID, p.ProvisionFailures)
}

// provisionOn makes one bounded attempt, past the request's own life: it may have created things on the provider.
func (h *Handler) provisionOn(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	attempt, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionAttemptTimeout)
	defer cancel()
	err := h.BootstrapOnto(attempt, p, es)
	if err != nil {
		slog.Error("project provisioning failed", "project", p.ID, "service", es.ID, "provider", es.Provider(), "err", err)
		if p.ProvisionFailures == nil {
			p.ProvisionFailures = map[string]ProvisionFailure{}
		}
		p.ProvisionFailures[es.ID] = ProvisionFailure{Message: err.Error(), At: time.Now().UTC()}
	} else {
		delete(p.ProvisionFailures, es.ID)
	}
	if err != nil || es.ProvisionError != nil {
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionRecordTimeout)
		defer cancel()
		if rerr := h.esSvc.SetProvisionError(record, es.ID, p.ID, err); rerr != nil {
			slog.Error("project provisioning: record on the provider", "service", es.ID, "err", rerr)
		}
	}
	return err
}

// BootstrapProxmoxOnto provisions the project onto a Proxmox VE service: ensure the project's
// pool (the tenancy boundary — every project VM lives in it, every list/mutation filters by it)
// and its default security group, and append the binding {serviceId, provider:"proxmox", region, pool}. The pool id is persisted
// on the binding (RGWUIDFor precedent) so teardown never re-derives it. No quota is written: a project without its own
// Proxmox quota is held to the provider default as it is at each check (pveQuotaLimits), so a change of the default
// applies to it at once.
// Idempotent: a project already attached to this service is left as it is (no provider call, no write).
func (h *Handler) BootstrapProxmoxOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return nil // already bound: nothing to write
	}
	if h.pveFor == nil {
		return fmt.Errorf("bootstrap proxmox: proxmox service not configured")
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return fmt.Errorf("bootstrap proxmox: build pve client: %w", err)
	}
	pool, err := ps.EnsureProjectPool(ctx, p.ID)
	if err != nil {
		return fmt.Errorf("bootstrap proxmox: ensure pool: %w", err)
	}
	// The default security group every server gets unless the user picks others.
	if _, err := ps.EnsureDefaultSecurityGroup(ctx, p.ID, es.PVENetworkConfig().CIDRPool); err != nil {
		return fmt.Errorf("bootstrap proxmox: default security group: %w", err)
	}
	region := es.PVERegion()
	if region == "" {
		region = h.defaultRegion
	}
	if err := h.appendBinding(ctx, p, map[string]any{
		"serviceId": es.ID,
		"provider":  "proxmox",
		"region":    region,
		"pool":      pool,
	}); err != nil {
		return err
	}
	// Show the default group right away, not after the next sync cycle.
	h.pveSyncSecurityGroups(ctx, es, p)
	return nil
}

// BootstrapDbaasOnto provisions the project onto a dbaas Managed-Database service: ensure the
// project's namespace on the DB cluster (labelled for the sync/AppProject scoping) and append
// the binding {serviceId, provider:"dbaas", region}. No credential store — connection info is
// fetched on demand from operator secrets, never persisted (kamaji kubeconfig discipline).
// Idempotent: a project already attached to this service is left as it is (no provider call, no write).
func (h *Handler) BootstrapDbaasOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return nil // already bound: nothing to write
	}
	if h.dbaasFor == nil {
		return fmt.Errorf("bootstrap dbaas: managed-database service not configured")
	}
	ds, err := h.dbaasFor(es)
	if err != nil {
		return fmt.Errorf("bootstrap dbaas: build db-cluster client: %w", err)
	}
	if err := ds.EnsureProjectNamespace(ctx, p.ID); err != nil {
		return fmt.Errorf("bootstrap dbaas: ensure namespace: %w", err)
	}
	region := es.DbaasRegion()
	if region == "" {
		region = h.defaultRegion
	}
	return h.appendBinding(ctx, p, map[string]any{
		"serviceId": es.ID,
		"provider":  "dbaas",
		"region":    region,
	})
}

// BootstrapSandboxOnto attaches the project to a sandbox service: ensure sb-<projectId> on the sandbox cluster (the
// tenant label puts it under the egress guardrail; the ResourceQuota carries the project's sandbox quota) and append
// the binding {serviceId, provider:"sandbox", region}. Idempotent.
func (h *Handler) BootstrapSandboxOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return nil // already bound: nothing to write
	}
	if h.sandboxFor == nil {
		return fmt.Errorf("bootstrap sandbox: sandbox service not configured")
	}
	ss, err := h.sandboxFor(es)
	if err != nil {
		return fmt.Errorf("bootstrap sandbox: %w", err)
	}
	q := es.SandboxProjectQuota(p.Quota)
	if err := ss.EnsureProjectNamespace(ctx, p.ID, &q); err != nil {
		return fmt.Errorf("bootstrap sandbox: ensure namespace: %w", err)
	}
	region := es.SandboxRegion()
	if region == "" {
		region = h.defaultRegion
	}
	return h.appendBinding(ctx, p, map[string]any{"serviceId": es.ID, "provider": "sandbox", "region": region})
}

// BootstrapKamajiOnto provisions the project onto a kamaji Managed-Kubernetes service: ensure the
// project's namespace on the management cluster (labelled for the sync/AppProject scoping) and
// append the binding {serviceId, provider:"kamaji", region}. No credential store — the cluster
// admin kubeconfig is fetched on demand from the Kamaji secret, never persisted (plan D5).
// Idempotent: a project already attached to this service is left as it is (no provider call, no write).
func (h *Handler) BootstrapKamajiOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return nil // already bound: nothing to write
	}
	if h.kamajiFor == nil {
		return fmt.Errorf("bootstrap kamaji: managed-k8s service not configured")
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		return fmt.Errorf("bootstrap kamaji: build management client: %w", err)
	}
	if err := ks.EnsureProjectNamespace(ctx, p.ID); err != nil {
		return fmt.Errorf("bootstrap kamaji: ensure namespace: %w", err)
	}
	region := es.KamajiRegion()
	if region == "" {
		region = h.defaultRegion
	}
	return h.appendBinding(ctx, p, map[string]any{
		"serviceId": es.ID,
		"provider":  "kamaji",
		"region":    region,
	})
}

// BootstrapCephOnto provisions the project onto a ceph-s3 object-store service (the RGW user is
// the sole anchor). It ensures the project's RGW tenant-user + quota via Admin Ops,
// stores the generated S3 keys (encrypted) in the ceph credential store, and appends the ProjectExternal
// service binding {serviceId, provider:"ceph-s3", region, rgwTenant, rgwUid}. Idempotent: a project
// already attached to this service is saved as-is; ensureUser reuses an existing RGW user's keys.
func (h *Handler) BootstrapCephOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return nil // already bound: nothing to write
	}
	if h.cephCreds == nil {
		return fmt.Errorf("bootstrap ceph: credential store not configured")
	}
	region := es.CephRegion()
	if region == "" {
		region = h.defaultRegion
	}
	uid := es.RGWUIDFor(p.ID) // uidPrefix + projectId — the one derivation
	// Admin-keyed client (no project keys yet) drives the Admin Ops user create + quota.
	adminCC, err := es.ObjectClient(ctx, region, "", "", uid)
	if err != nil {
		return fmt.Errorf("bootstrap ceph: build admin client: %w", err)
	}
	access, secret, err := adminCC.EnsureProjectUser(ctx, "proxcloud-"+p.ID)
	if err != nil {
		return fmt.Errorf("bootstrap ceph: ensure rgw user: %w", err)
	}
	if err := h.cephCreds.Save(ctx, &cephcred.Credential{
		ProjectID: p.ID, ServiceID: es.ID, RGWUID: uid,
		AccessKey: access, SecretKey: secret,
	}); err != nil {
		return fmt.Errorf("bootstrap ceph: store credential: %w", err)
	}
	return h.appendBinding(ctx, p, map[string]any{
		"serviceId": es.ID,
		"provider":  es.Provider(),
		"region":    region,
		"rgwUid":    uid,
	})
}

// bootstrapTrueNASOnto appends the binding {serviceId, provider:"truenas", region}. Idempotent.
func (h *Handler) bootstrapTrueNASOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	return h.bootstrapBindingOnto(ctx, p, es, "truenas")
}

// bootstrapBindingOnto attaches a provider that needs nothing created for a project up front.
func (h *Handler) bootstrapBindingOnto(ctx context.Context, p *Project, es *externalservice.ExternalService, provider string) error {
	if p.HasService(es.ID) {
		return nil
	}
	return h.appendBinding(ctx, p, map[string]any{"serviceId": es.ID, "provider": provider, "region": es.CephRegion()})
}

// BootstrapOnto provisions the project onto the GIVEN cloud service — the explicit-service leg of
// the project bootstrap, dispatched per provider kind. Does NOT flip status (the bootstrap never does; enableAndBootstrap sets ENABLED itself
// before calling). Idempotent: a project already attached to this service is saved as-is.
func (h *Handler) BootstrapOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	// A ceph-s3 service routes to the RGW-user bootstrap. This is what lets the
	// admin "attach external service" leg onboard EXISTING projects onto a newly added ceph provider
	// (enableAndBootstrap only provisions projects that have no service attached yet).
	if es.IsObjectStore() {
		return h.BootstrapCephOnto(ctx, p, es)
	}
	// kamaji: the management-cluster namespace leg.
	if es.IsKamaji() {
		return h.BootstrapKamajiOnto(ctx, p, es)
	}
	// dbaas: the DB-cluster namespace leg.
	if es.IsDbaas() {
		return h.BootstrapDbaasOnto(ctx, p, es)
	}
	// proxmox: the PVE pool leg.
	if es.IsProxmox() {
		return h.BootstrapProxmoxOnto(ctx, p, es)
	}
	if es.IsTrueNAS() {
		return h.bootstrapTrueNASOnto(ctx, p, es)
	}
	if es.IsPowerDNS() {
		return h.bootstrapBindingOnto(ctx, p, es, "powerdns")
	}
	if es.IsSandbox() {
		return h.BootstrapSandboxOnto(ctx, p, es)
	}
	return fmt.Errorf("bootstrap: unsupported provider kind %q", es.Provider())
}
