package project

// teardown.go implements the project-deletion cloud cascade (the admin DELETE /project/{id}/now
// leg). It is scoped STRICTLY to the project's own cached resources + its own provider-side
// anchors (PVE pool, RGW user, namespaces) — it can never touch another project.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
)

// deletionOrder ranks a cloud-resource type for teardown: LOWER = delete FIRST. Dependents (that
// hold references onto others) go before their dependencies, so a best-effort sweep in this order
// minimizes "resource still in use" failures. Unknown/leaf types sort last.
func deletionOrder(t string) int {
	switch t {
	case cloud.TypeKubernetesCluster, cloud.TypeDatabaseCluster, cloud.TypeStack, cloud.TypeLoadBalancer:
		return 0 // composites that own many children
	case cloud.TypeTrilioRestore, cloud.TypeTrilioSnapshot, cloud.TypeTrilioWorkload, cloud.TypeTrilioBackupTarget:
		return 1
	case cloud.TypeIPSecSiteConnection:
		return 2
	case cloud.TypeVPNService, cloud.TypeVPNEndpointGroup, cloud.TypeIKEPolicy, cloud.TypeIPSecPolicy:
		return 3
	case cloud.TypeServer, cloud.TypeBaremetalServer:
		return 4 // deleting an instance releases its FIP/port/volume attachments
	case cloud.TypeFloatingIP:
		return 5
	case cloud.TypePort:
		return 6
	case cloud.TypeVolumeSnapshot, cloud.TypeShareSnapshot, cloud.TypeShareSnapshotGroup:
		return 7
	case cloud.TypeVolume, cloud.TypeVolumeBackup:
		return 8
	case cloud.TypeShare, cloud.TypeShareGroup:
		return 9
	case cloud.TypeShareNetwork, cloud.TypeShareSecurityService:
		return 10
	case cloud.TypeSubnet:
		return 11
	case cloud.TypeNetwork:
		return 12
	case cloud.TypeRouter:
		return 13 // after its subnets/ports are gone
	default:
		return 100 // security-group / keypair / image / bucket / dns-zone / secret / server-group / …
	}
}

// SortCloudResourcesForDeletion orders dependents before their dependencies.
// It is exported so every project-deletion path, including the scheduled job
// wired in cmd/api, applies the same server-before-boot-volume policy.
func SortCloudResourcesForDeletion(resources []cloud.CloudResource) {
	sort.SliceStable(resources, func(i, j int) bool {
		return deletionOrder(resources[i].Type) < deletionOrder(resources[j].Type)
	})
}

// teardownSweeps is how many dependency-ordered passes the cascade makes: a resource that fails
// because a blocker still exists (e.g. a network with a lingering port) can succeed on a later pass
// once the blocker is deleted.
const teardownSweeps = 3

// TeardownProject cascade-deletes a project's cloud resources (best-effort, dependency-ordered, with
// a few retry sweeps), tears down the project's provider-side anchors, then marks the project DELETED. It
// runs to completion regardless of individual failures; the periodic sync/reconcile is the backstop
// for anything left behind. Returns a non-nil error summarising what could not be deleted. A leg that needs a re-run keeps
// the project DELETE_IN_PROGRESS, so the deletion job retries it; otherwise the project is marked DELETED.
func (h *Handler) TeardownProject(ctx context.Context, projectID string) error {
	p, err := h.svc.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	resources, err := h.cloud.FindAllByProjectID(ctx, projectID)
	if err != nil {
		return err
	}
	SortCloudResourcesForDeletion(resources)

	// Sandboxes first: they run customer code and bill by the minute, and nothing else depends on them. Their rows end
	// at the delete, and the project's sandbox namespace goes after them.
	resources, sandboxesLeft := h.teardownSandboxes(ctx, p, resources)

	// kamaji + dbaas rows first, split out of the bucket sweep entirely: tryTenantClient below can
	// never build a client for them, and their delete is an
	// ArgoCD Application delete on the management/DB cluster whose cascade runs asynchronously.
	// Rows archive on a successful delete request; secrets / namespaces are reaped by FinalizeOrphans below once the cascade has actually finished.
	var kamajiServices, dbaasServices []string
	{
		var sweep []cloud.CloudResource
		now := time.Now().UTC()
		for i := range resources {
			res := &resources[i]
			es, gerr := h.esSvc.Get(ctx, res.ServiceID)
			if gerr != nil || es == nil || (!es.IsKamaji() && !es.IsDbaas()) {
				sweep = append(sweep, *res)
				continue
			}
			if es.IsDbaas() {
				if h.dbaasFor == nil {
					sweep = append(sweep, *res)
					continue
				}
				ds, derr := h.dbaasFor(es)
				if derr != nil {
					slog.Error("teardown: build dbaas service", "project", p.ID, "serviceId", res.ServiceID, "err", derr)
					sweep = append(sweep, *res)
					continue
				}
				if !contains(dbaasServices, res.ServiceID) {
					dbaasServices = append(dbaasServices, res.ServiceID)
				}
				if derr := ds.DeleteDatabase(ctx, p.ID, res.ExternalID); derr != nil {
					slog.Error("teardown: delete dbaas database", "project", p.ID, "database", res.ExternalID, "err", derr)
					sweep = append(sweep, *res)
					continue
				}
				if aerr := h.cloud.DeleteAndArchive(ctx, res, now); aerr != nil {
					slog.Error("teardown: archive dbaas database row", "project", p.ID, "database", res.ExternalID, "err", aerr)
					sweep = append(sweep, *res)
				}
				continue
			}
			if h.kamajiFor == nil {
				sweep = append(sweep, *res)
				continue
			}
			ks, kerr := h.kamajiFor(es)
			if kerr != nil {
				slog.Error("teardown: build kamaji service", "project", p.ID, "serviceId", res.ServiceID, "err", kerr)
				sweep = append(sweep, *res)
				continue
			}
			if !contains(kamajiServices, res.ServiceID) {
				kamajiServices = append(kamajiServices, res.ServiceID)
			}
			if derr := ks.DeleteCluster(ctx, p.ID, res.ExternalID); derr != nil {
				slog.Error("teardown: delete kamaji cluster", "project", p.ID, "cluster", res.ExternalID, "err", derr)
				sweep = append(sweep, *res)
				continue
			}
			if aerr := h.cloud.DeleteAndArchive(ctx, res, now); aerr != nil {
				slog.Error("teardown: archive kamaji cluster row", "project", p.ID, "cluster", res.ExternalID, "err", aerr)
				sweep = append(sweep, *res)
			}
		}
		resources = sweep
	}

	remaining := resources
	for sweep := 0; sweep < teardownSweeps && len(remaining) > 0; sweep++ {
		if sweep > 0 {
			// Provider deletes complete asynchronously (routinely 10-30s);
			// back-to-back sweeps would burn every retry inside that window.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Second):
			}
		}
		var stillLeft []cloud.CloudResource
		for i := range remaining {
			res := &remaining[i]
			cc, ok := h.tryTenantClient(ctx, p, res.ServiceID)
			if !ok {
				stillLeft = append(stillLeft, *res)
				continue
			}
			ws := providers.NewWriteService(cc, h.cloud)
			if err := ws.Delete(ctx, res.ServiceID, res.ExternalID); err != nil {
				stillLeft = append(stillLeft, *res)
			}
		}
		remaining = stillLeft
	}

	// Best-effort orphan finalization for kamaji services. The ArgoCD delete cascade typically
	// still runs for minutes after DeleteCluster, so this pass usually reports pending work —
	// the periodic service-level sweep (syncjob.sweepKamajiOrphans) finishes the job later.
	// What matters HERE is the pending signal: while the cascade still runs, the project's
	// provider-side teardown below must be DEFERRED, or the CAPI finalizers wedge forever.
	// Scan every ATTACHED kamaji service, not just the ones that still had cache rows — a
	// cluster deleted minutes before teardown has no row anymore, but its cascade may still be
	// running.
	for _, svcID := range p.ServiceIDs() {
		if es, err := h.esSvc.Get(ctx, svcID); err == nil && es != nil {
			if es.IsKamaji() && !contains(kamajiServices, svcID) {
				kamajiServices = append(kamajiServices, svcID)
			}
			if es.IsDbaas() && !contains(dbaasServices, svcID) {
				dbaasServices = append(dbaasServices, svcID)
			}
		}
	}
	kamajiPending := 0
	for _, svcID := range kamajiServices {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || h.kamajiFor == nil {
			continue
		}
		ks, err := h.kamajiFor(es)
		if err != nil {
			continue
		}
		pending, ferr := ks.FinalizeOrphans(ctx, p.ID)
		kamajiPending += pending
		if ferr != nil {
			slog.Warn("teardown: kamaji orphan finalize", "project", p.ID, "serviceId", svcID, "pending", pending, "err", ferr)
			kamajiPending++
		} else if pending == 0 {
			if nerr := ks.RemoveProjectNamespace(ctx, p.ID); nerr != nil {
				slog.Warn("teardown: kamaji namespace", "project", p.ID, "serviceId", svcID, "err", nerr)
				kamajiPending++
			}
		}
	}
	// Same finalize + pending signal for dbaas: while a database's delete cascade is in flight,
	// defer exactly like kamaji.
	dbaasPending := 0
	for _, svcID := range dbaasServices {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil || h.dbaasFor == nil {
			continue
		}
		ds, err := h.dbaasFor(es)
		if err != nil {
			continue
		}
		pending, ferr := ds.FinalizeOrphans(ctx, p.ID)
		dbaasPending += pending
		if ferr != nil {
			slog.Warn("teardown: dbaas orphan finalize", "project", p.ID, "serviceId", svcID, "pending", pending, "err", ferr)
			dbaasPending++
		} else if pending == 0 {
			if nerr := ds.RemoveProjectNamespace(ctx, p.ID); nerr != nil {
				slog.Warn("teardown: dbaas namespace", "project", p.ID, "serviceId", svcID, "err", nerr)
				dbaasPending++
			}
		}
	}

	// cephRevokeFailed records that a ceph-s3 credential could NOT be revoked (RGW purge failed), so its
	// local record was deliberately kept and the caller must be told deprovisioning is incomplete.
	cephRevokeFailed := false
	// pveTeardownFailed records that a Proxmox pool could not be fully torn down (VMs left) —
	// teardown reports incomplete so a re-run retries (all pve teardown legs are idempotent).
	pveTeardownFailed := false

	// File shares go first: the project's addresses return to the pools below, and an export still
	// listing one would hand this project's data to whoever gets it next. Shares left behind therefore
	// stop the Proxmox teardown, and a re-run retries.
	sharesLeft := false
	shareServices, serr := h.trueNASServices(ctx, p)
	if serr != nil {
		slog.Error("teardown: list file share providers", "project", p.ID, "err", serr)
		sharesLeft = true
	}
	for _, es := range shareServices {
		left, terr := h.teardownShares(ctx, p, es, remaining)
		remaining = left
		if terr != nil {
			slog.Error("teardown: file shares", "project", p.ID, "serviceId", es.ID, "err", terr)
			sharesLeft = true
		}
	}
	if sharesLeft {
		pveTeardownFailed = true
	}
	// DNS zones: one left behind keeps answering for the tenant's domain and holds its name, so a failure asks for a re-run.
	zonesLeft := false
	zoneServices, zerr := h.powerDNSServices(ctx, p)
	if zerr != nil {
		slog.Error("teardown: list DNS providers", "project", p.ID, "err", zerr)
		zonesLeft = true
	}
	for _, es := range zoneServices {
		left, terr := h.teardownZones(ctx, p, es, remaining)
		remaining = left
		if terr != nil {
			slog.Error("teardown: DNS zones", "project", p.ID, "serviceId", es.ID, "err", terr)
			zonesLeft = true
		}
	}

	// Delete the per-service anchor the project is bootstrapped on (admin-scoped client, not
	// project-scoped — a project user cannot delete itself). Best-effort per provider kind.
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if err != nil || es == nil {
			continue
		}
		if es.IsObjectStore() {
			// ceph-s3: purge the RGW user + ALL its data, then drop the stored credential.
			// The customer-facing DeleteBucket in the sweep above refuses a NON-EMPTY bucket (S3 semantics),
			// so those buckets are still in `remaining`. Force-delete each of THIS project's
			// ceph buckets via Admin Ops (purge-objects) so their cache rows get archived and teardown stops
			// reporting them as undeleted.
			region := es.CephRegion()
			if region == "" {
				region = h.defaultRegion
			}
			// Prefer the uid that was actually PROVISIONED (stored on the credential) over re-deriving it
			// from current service config — a uidPrefix change between provision and teardown would
			// otherwise target a user that never existed and leave the real one undeprovisioned.
			rgwUID := es.RGWUIDFor(p.ID)
			if h.cephCreds != nil {
				if cred, _ := h.cephCreds.Get(ctx, p.ID, svcID); cred != nil && cred.RGWUID != "" {
					rgwUID = cred.RGWUID
				}
			}
			adminCC, cerr := es.ObjectClient(ctx, region, "", "", rgwUID)
			if cerr != nil {
				// Could not even build the admin client → cloud credentials are NOT revoked. Keep every
				// local credential record so a re-run / operator can still revoke, and report the failure.
				slog.Error("teardown: build ceph admin client", "project", p.ID, "serviceId", svcID, "err", cerr)
				cephRevokeFailed = true
				continue
			}
			now := time.Now().UTC()
			var stillLeft []cloud.CloudResource
			for i := range remaining {
				res := &remaining[i]
				if res.ServiceID != svcID || res.Type != cloud.TypeBucket {
					stillLeft = append(stillLeft, *res)
					continue
				}
				if derr := adminCC.ForceDeleteBucket(ctx, res.ExternalID); derr != nil {
					stillLeft = append(stillLeft, *res)
					continue
				}
				// The bucket is gone cloud-side; archiving the cache row can still fail. Keep it in
				// `remaining` so teardown reports it (a re-run's ForceDeleteBucket is a no-op and retries
				// the archive) rather than silently claiming success with a stale row left behind.
				if aerr := h.cloud.DeleteAndArchive(ctx, res, now); aerr != nil {
					slog.Error("teardown: archive ceph bucket row", "project", p.ID, "externalId", res.ExternalID, "err", aerr)
					stillLeft = append(stillLeft, *res)
				}
			}
			remaining = stillLeft
			// Extra S3 keys are SEPARATE RGW users ("<projectUid>-<name>"); purging the project's own user
			// does not touch them. FAIL CLOSED (finding F1): only drop a local key record once its RGW user
			// is actually purged — otherwise a transient purge failure would leave a live S3 key behind with
			// no local inventory left to revoke it.
			if h.cephKeys != nil {
				if keys, kerr := h.cephKeys.List(ctx, p.ID, svcID); kerr == nil {
					for i := range keys {
						if derr := adminCC.DeleteChildKey(ctx, keys[i].RGWUID); derr != nil {
							slog.Error("teardown: purge ceph child key", "project", p.ID, "rgwUid", keys[i].RGWUID, "err", derr)
							cephRevokeFailed = true
							continue // keep the local record so the key can still be revoked later
						}
						// The RGW user is gone; a failed record delete leaves stale inventory, so report it
						// and keep teardown retryable (the purge re-run is a not-found no-op).
						if derr := h.cephKeys.Delete(ctx, keys[i].ID); derr != nil {
							slog.Error("teardown: delete ceph child key record", "project", p.ID, "keyId", keys[i].ID, "err", derr)
							cephRevokeFailed = true
						}
					}
				} else {
					slog.Error("teardown: list ceph child keys", "project", p.ID, "err", kerr)
					cephRevokeFailed = true
				}
			}
			if perr := adminCC.PurgeProjectUser(ctx); perr != nil {
				// The parent user (and its keys) is still alive. Keep the stored credential so it can be
				// revoked on a retry; do not blank the only record that identifies it.
				slog.Error("teardown: purge ceph user", "project", p.ID, "rgwUid", rgwUID, "err", perr)
				cephRevokeFailed = true
			} else if h.cephCreds != nil {
				if derr := h.cephCreds.Delete(ctx, p.ID, svcID); derr != nil {
					// RGW user purged but the local record remains — report it so teardown is retried
					// rather than claiming a clean deprovision with stale credentials on file.
					slog.Error("teardown: delete ceph credential record", "project", p.ID, "serviceId", svcID, "err", derr)
					cephRevokeFailed = true
				}
			}
			continue
		}
		if es.IsProxmox() {
			// proxmox: destroy every VM still in the project pool, then the pool, then the
			// project's vnets / EVPN zone / security groups. The cache rows
			// of this service are archived here (their VMs are verifiably gone); a failure keeps
			// them so a re-run retries.
			if h.pveFor == nil || sharesLeft {
				pveTeardownFailed = true
				continue
			}
			ps, perr := h.pveFor(es)
			if perr != nil {
				slog.Error("teardown: build pve client", "project", p.ID, "serviceId", svcID, "err", perr)
				pveTeardownFailed = true
				continue
			}
			pool := p.ServicePool(svcID)
			if pool == "" {
				pool = es.PoolFor(p.ID) // pre-pool-stamp bindings: re-derive
			}
			// Archives first: PVE only lets the token delete one while its VM exists.
			if store := es.PVEBackupStorage(); store != "" {
				if berr := ps.DeleteProjectBackups(ctx, p.ID, store); berr != nil {
					slog.Error("teardown: backups", "project", p.ID, "serviceId", svcID, "err", berr)
					pveTeardownFailed = true
					continue
				}
			}
			if terr := ps.TeardownProjectPool(ctx, pool); terr != nil {
				slog.Error("teardown: pve pool", "project", p.ID, "pool", pool, "err", terr)
				pveTeardownFailed = true
				continue
			}
			if nerr := h.pveTeardownNetworks(ctx, ps, p.ID); nerr != nil {
				slog.Error("teardown: pve networks", "project", p.ID, "serviceId", svcID, "err", nerr)
				pveTeardownFailed = true
				continue
			}
			if serr := h.pveReleaseProjectSeeds(ctx, ps, svcID, p.ID); serr != nil {
				slog.Error("teardown: release seeds", "project", p.ID, "serviceId", svcID, "err", serr)
				pveTeardownFailed = true
				continue
			}
			if h.publicIPs != nil {
				if rerr := h.publicIPs.ReleaseProject(ctx, svcID, p.ID); rerr != nil {
					slog.Error("teardown: release public IPs", "project", p.ID, "serviceId", svcID, "err", rerr)
					pveTeardownFailed = true
					continue
				}
			}
			now := time.Now().UTC()
			var stillLeft []cloud.CloudResource
			for i := range remaining {
				res := &remaining[i]
				if res.ServiceID != svcID {
					stillLeft = append(stillLeft, *res)
					continue
				}
				if aerr := h.cloud.DeleteAndArchive(ctx, res, now); aerr != nil {
					slog.Error("teardown: archive pve row", "project", p.ID, "externalId", res.ExternalID, "err", aerr)
					stillLeft = append(stillLeft, *res)
				}
			}
			remaining = stillLeft
			continue
		}
		// Nothing external to delete for the remaining kinds.
		continue
	}

	// ponytail: rows left with no failed leg behind them (a provider since removed) do not ask for a re-run, or a project
	// could wedge the job's sweep forever. Report them and move on.
	expect := p.Status
	teardownOutcome(p, cephRevokeFailed || pveTeardownFailed || zonesLeft || sandboxesLeft || kamajiPending > 0 || dbaasPending > 0)
	if _, err := h.svc.SaveIfStatus(ctx, p, expect); err != nil {
		return err
	}
	if p.Status == StatusDeleted && h.sbKeys != nil {
		// The project's E2B keys already answer nothing; a DELETED project keeps no rows of them either.
		if _, err := h.sbKeys.DeleteMany(ctx, pgdoc.M{"projectId": p.ID}); err != nil {
			slog.Warn("teardown: delete sandbox API keys", "project", p.ID, "err", err)
		}
	}
	if sandboxesLeft {
		return fmt.Errorf("teardown could not remove the project's sandboxes or their namespace; the deletion job retries")
	}
	if zonesLeft {
		return fmt.Errorf("teardown could not remove the project's DNS zones; the deletion job retries")
	}
	if cephRevokeFailed {
		return fmt.Errorf("teardown could not revoke some ceph-s3 credentials; their local records were kept for retry")
	}
	if pveTeardownFailed {
		return fmt.Errorf("teardown could not fully tear down the project's cloud resources; re-run teardown to retry")
	}
	if len(remaining) > 0 {
		return fmt.Errorf("teardown left %d resource(s) undeleted; re-run teardown to retry", len(remaining))
	}
	if kamajiPending > 0 {
		return fmt.Errorf("teardown: %d kamaji cluster remnant(s) still finalizing on the management cluster (project teardown deferred) — the periodic sweep GCs them; re-run teardown afterwards (docs/managed-k8s.md)", kamajiPending)
	}
	if dbaasPending > 0 {
		return fmt.Errorf("teardown: %d managed-database remnant(s) still finalizing on the DB cluster (project teardown deferred) — the periodic sweep GCs them; re-run teardown afterwards (docs/managed-dbaas.md)", dbaasPending)
	}
	return nil
}

// teardownOutcome sets the status one teardown pass leaves. DELETED is terminal: nothing looks at it again, so a pass that
// needs a re-run stays DELETE_IN_PROGRESS, keeping the past schedule the deletion job selected it by.
func teardownOutcome(p *Project, retry bool) {
	if retry {
		p.Status = StatusDeleteInProgress
		return
	}
	p.Status = StatusDeleted
}

// teardownShares deletes everything the NAS holds for the project, cached or not, and archives the provider's rows.
// It returns the rows still to delete; a failure keeps the rows so a re-run retries.
func (h *Handler) teardownShares(ctx context.Context, p *Project, es *externalservice.ExternalService, rows []cloud.CloudResource) ([]cloud.CloudResource, error) {
	ts, err := es.TrueNASService()
	if err != nil {
		return rows, err
	}
	defer ts.C.Close()
	list, err := ts.List(ctx, p.ID)
	if err != nil {
		return rows, err
	}
	for i := range list {
		if err := ts.Delete(ctx, p.ID, list[i].Dataset); err != nil {
			return rows, err
		}
	}
	now := time.Now().UTC()
	var left []cloud.CloudResource
	for i := range rows {
		if rows[i].ServiceID != es.ID || h.cloud.DeleteAndArchive(ctx, &rows[i], now) != nil {
			left = append(left, rows[i])
		}
	}
	return left, nil
}

// contains is a tiny []string membership helper (avoids importing slices for one call site).
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
