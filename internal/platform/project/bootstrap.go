package project

// bootstrap.go holds the cloud-bootstrap slice: when a project is enabled it is provisioned onto
// every non-disabled CLOUD external service — a PVE pool (proxmox), an RGW user (ceph-s3), a
// management/DB-cluster namespace (kamaji/dbaas) — and each binding is recorded as a
// ProjectExternalService on the project.

import (
	"context"
	"fmt"

	"github.com/mutex-solutions/proxcloud/internal/cloud/cephcred"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// EnableAndBootstrap exposes the bootstrap for cross-package orchestration (billing
// activation activates projects; the bootstrap is service-public).
func (h *Handler) EnableAndBootstrap(ctx context.Context, p *Project) error {
	return h.enableAndBootstrap(ctx, p)
}

// enableAndBootstrap provisions the project onto the non-disabled CLOUD services and flips it to
// ENABLED. Idempotent: a project that already has an attached service is left as-is.
func (h *Handler) enableAndBootstrap(ctx context.Context, p *Project) error {
	if h.esSvc == nil {
		return nil
	}
	if !p.IsEnabled() {
		if err := CheckTransition(ActionEnable, p.Status); err != nil {
			return err
		}
	}
	// Already provisioned (the project carries its attached services) → this is a re-entry; the
	// provider-side setup was done at first bootstrap. Skip ALL the live calls — they are idempotent
	// setup, not per-entry work, and re-running them on every project-open is both wasteful and a
	// failure surface (a provider config change must not 500 an already-working project).
	if len(p.ServiceIDs()) > 0 {
		return nil
	}
	services, err := h.esSvc.ListByType(ctx, externalservice.TypeCloud)
	if err != nil {
		return err
	}
	var cephES, kamajiES, dbaasES, pveES, truenasES, dnsES []*externalservice.ExternalService
	for i := range services {
		if services[i].IsDisabled() {
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
		}
	}
	if len(cephES) == 0 && len(kamajiES) == 0 && len(dbaasES) == 0 && len(pveES) == 0 && len(truenasES) == 0 && len(dnsES) == 0 {
		return nil // no cloud provider configured → nothing to provision
	}
	p.Status = StatusEnabled
	// Ceph-s3 object-store bootstrap — provision the project's RGW user per service.
	for _, es := range cephES {
		if err := h.BootstrapCephOnto(ctx, p, es); err != nil {
			return err
		}
	}
	// Kamaji managed-k8s bootstrap — ensure the project's management-cluster namespace.
	for _, es := range kamajiES {
		if err := h.BootstrapKamajiOnto(ctx, p, es); err != nil {
			return err
		}
	}
	// Dbaas managed-database bootstrap — ensure the project's DB-cluster namespace.
	for _, es := range dbaasES {
		if err := h.BootstrapDbaasOnto(ctx, p, es); err != nil {
			return err
		}
	}
	// Proxmox bootstrap — ensure the project's PVE pool.
	for _, es := range pveES {
		if err := h.BootstrapProxmoxOnto(ctx, p, es); err != nil {
			return err
		}
	}
	// TrueNAS: nothing to create until the first share, only the binding.
	for _, es := range truenasES {
		if err := h.bootstrapTrueNASOnto(ctx, p, es); err != nil {
			return err
		}
	}
	// PowerDNS: same, until the first zone.
	for _, es := range dnsES {
		if err := h.bootstrapBindingOnto(ctx, p, es, "powerdns"); err != nil {
			return err
		}
	}
	return nil
}

// BootstrapProxmoxOnto provisions the project onto a Proxmox VE service: ensure the project's
// pool (the tenancy boundary — every project VM lives in it, every list/mutation filters by it)
// and its default security group, seed the provider default quota, and append the binding {serviceId, provider:"proxmox", region, pool}. The pool id is persisted
// on the binding (RGWUIDFor precedent) so teardown never re-derives it.
// Idempotent: a project already attached to this service is saved as-is.
func (h *Handler) BootstrapProxmoxOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return h.svc.Save(ctx, p)
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
	// Seed the provider default quota onto a project that has none, so it is
	// visible/editable per project and survives later default changes.
	if len(p.Quota) == 0 {
		p.Quota = es.PVEDefaultQuota()
	}
	region := es.PVERegion()
	if region == "" {
		region = h.defaultRegion
	}
	p.Services = append(p.Services, map[string]any{
		"serviceId": es.ID,
		"provider":  "proxmox",
		"region":    region,
		"pool":      pool,
	})
	if err := h.svc.Save(ctx, p); err != nil {
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
// Idempotent: a project already attached to this service is saved as-is.
func (h *Handler) BootstrapDbaasOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return h.svc.Save(ctx, p)
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
	p.Services = append(p.Services, map[string]any{
		"serviceId": es.ID,
		"provider":  "dbaas",
		"region":    region,
	})
	return h.svc.Save(ctx, p)
}

// BootstrapKamajiOnto provisions the project onto a kamaji Managed-Kubernetes service: ensure the
// project's namespace on the management cluster (labelled for the sync/AppProject scoping) and
// append the binding {serviceId, provider:"kamaji", region}. No credential store — the cluster
// admin kubeconfig is fetched on demand from the Kamaji secret, never persisted (plan D5).
// Idempotent: a project already attached to this service is saved as-is.
func (h *Handler) BootstrapKamajiOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return h.svc.Save(ctx, p)
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
	p.Services = append(p.Services, map[string]any{
		"serviceId": es.ID,
		"provider":  "kamaji",
		"region":    region,
	})
	return h.svc.Save(ctx, p)
}

// BootstrapCephOnto provisions the project onto a ceph-s3 object-store service (the RGW user is
// the sole anchor). It ensures the project's RGW tenant-user + quota via Admin Ops,
// stores the generated S3 keys (encrypted) in the ceph credential store, and appends the ProjectExternal
// service binding {serviceId, provider:"ceph-s3", region, rgwTenant, rgwUid}. Idempotent: a project
// already attached to this service is saved as-is; ensureUser reuses an existing RGW user's keys.
func (h *Handler) BootstrapCephOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	if p.HasService(es.ID) {
		return h.svc.Save(ctx, p)
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
	p.Services = append(p.Services, map[string]any{
		"serviceId": es.ID,
		"provider":  es.Provider(),
		"region":    region,
		"rgwUid":    uid,
	})
	return h.svc.Save(ctx, p)
}

// bootstrapTrueNASOnto appends the binding {serviceId, provider:"truenas", region}. Idempotent.
func (h *Handler) bootstrapTrueNASOnto(ctx context.Context, p *Project, es *externalservice.ExternalService) error {
	return h.bootstrapBindingOnto(ctx, p, es, "truenas")
}

// bootstrapBindingOnto attaches a provider that needs nothing created for a project up front.
func (h *Handler) bootstrapBindingOnto(ctx context.Context, p *Project, es *externalservice.ExternalService, provider string) error {
	if !p.HasService(es.ID) {
		p.Services = append(p.Services, map[string]any{"serviceId": es.ID, "provider": provider, "region": es.CephRegion()})
	}
	return h.svc.Save(ctx, p)
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
	return fmt.Errorf("bootstrap: unsupported provider kind %q", es.Provider())
}
