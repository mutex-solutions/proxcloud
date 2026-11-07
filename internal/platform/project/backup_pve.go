package project

// backup_pve.go — the server backup actions (pve/backup.go).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveBackupAction handles LIST_BACKUPS / CREATE_BACKUP / RESTORE_BACKUP / DELETE_BACKUP on a SERVER. Returns handled.
func (h *Handler) pveBackupAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if cr.Type != cloud.TypeServer {
		return false
	}
	switch action {
	case "LIST_BACKUPS", "CREATE_BACKUP", "RESTORE_BACKUP", "DELETE_BACKUP":
	default:
		return false
	}
	store := es.PVEBackupStorage()
	if store == "" {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "This provider has no backup storage configured"))
		return true
	}
	ctx := r.Context()
	ps, ok := h.pveService(w, es)
	if !ok {
		return true
	}
	if action == "LIST_BACKUPS" {
		backups, err := ps.ListServerBackups(ctx, proj.ID, cr.ExternalID, store)
		if err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		httpx.OK(w, map[string]any{"result": backups})
		return true
	}
	id := strAny(data["id"])
	var err error
	var archivedAt time.Time
	switch action {
	case "CREATE_BACKUP":
		caps, cerr := ps.C.DetectStorage(ctx)
		if cerr != nil {
			h.fail(w, upstreamErr(computeBackend, cerr))
			return true
		}
		err = ps.CreateServerBackup(ctx, proj.ID, cr.ExternalID, store, caps[store], es.PVEStorageHeadroomPct())
	case "RESTORE_BACKUP":
		archivedAt, err = ps.RestoreServerBackup(ctx, proj.ID, cr.ExternalID, store, id, h.pveRestoreGuard(proj, es, cr))
	case "DELETE_BACKUP":
		err = ps.DeleteServerBackup(ctx, proj.ID, cr.ExternalID, store, id)
	}
	if err != nil {
		if action == "RESTORE_BACKUP" {
			// A reconcile, not only a refresh: a failed restore destroyed the disks, and their rows must stop billing.
			prov := ps.VolumeSyncProvider(h.pveRegion(proj, es), proj.ID, es.PVEVolumeProfileFunc())
			if _, rerr := providers.Reconcile(ctx, prov, h.cloud, es.ID, time.Now().UTC()); rerr != nil {
				slog.Warn("restore: refresh rows", "project", proj.ID, "err", rerr)
			}
			h.pveRefreshServerRow(ctx, ps, proj, cr)
		}
		var quota *httpx.HTTPError
		if errors.As(err, &quota) {
			h.fail(w, quota)
			return true
		}
		h.fail(w, pveVolumeErr(err))
		return true
	}
	if action == "RESTORE_BACKUP" {
		// The disks are new volumes, whatever the archive named them.
		h.pveSyncVolumes(ctx, es, proj)
		h.pveDateRestoredVolumes(ctx, es, proj, cr.ExternalID, archivedAt)
	}
	h.pveRefreshServerRow(ctx, ps, proj, cr)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{"id": id}})
	return true
}

// pveRestoreGuard refuses a restore that would take the project over its quota, as a resize would, and hands the
// provider the server's VOLUME rows. It runs on what the restore would produce, before anything is destroyed: the archive
// can hold more vCPUs and RAM than a server resized down since, a GPU it has left, and a disk now on another server.
func (h *Handler) pveRestoreGuard(proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource) pve.RestoreGuard {
	return func(ctx context.Context, cur, archive pve.VMConfig) (map[string]pve.RestoreVolume, error) {
		if err := h.enforcePVEGPURestore(ctx, proj, es, cur, archive); err != nil {
			return nil, err
		}
		rows, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeVolume)
		if err != nil {
			return nil, err
		}
		vcpus, ramMb, diskGb := pve.RestoreDemand(cur, archive)
		if pve.RestoreCutDown(cur) {
			// A failed restore left a config that says nothing. Usage reads the rows, so that is what this server counts as.
			srv, err := h.cloud.FindByID(ctx, cr.ID)
			if err != nil || srv == nil {
				return nil, errQuotaUsageUnavailable()
			}
			vcpus, ramMb, diskGb = pveRowsDemand(srv, rows, archive)
		}
		if err := h.enforcePVEQuota(ctx, proj, es, pveQuotaDemand{VCPUs: vcpus, RAMMb: ramMb, DiskGb: diskGb}); err != nil {
			return nil, err
		}
		return pveRestoreVolumes(rows, cr.ExternalID), nil
	}
}

// pveRowsDemand is what a restore adds to a server as its rows count it: usage + demand must come to the others + the archive.
func pveRowsDemand(srv *cloud.CloudResource, volumes []cloud.CloudResource, archive pve.VMConfig) (vcpus, ramMb, diskGb int) {
	disk := 0
	for i := range volumes {
		vol, _ := volumes[i].Data["volume"].(map[string]any)
		if fmt.Sprint(intAny(vol["vmid"])) == srv.ExternalID {
			disk += intAny(vol["size"])
		}
	}
	s, _ := srv.Data["server"].(map[string]any)
	fl, _ := s["flavor"].(map[string]any)
	return pve.RestoreDemandOver(intAny(fl["vcpus"]), intAny(fl["ram"]), disk, archive)
}

// pveRestoreVolumes maps one server's VOLUME rows by volid.
func pveRestoreVolumes(rows []cloud.CloudResource, serverExtID string) map[string]pve.RestoreVolume {
	out := map[string]pve.RestoreVolume{}
	for i := range rows {
		vol, _ := rows[i].Data["volume"].(map[string]any)
		if fmt.Sprint(intAny(vol["vmid"])) != serverExtID || rows[i].CreatedAt == nil {
			continue
		}
		since := *rows[i].CreatedAt
		if d := rows[i].DataSince; d != nil && d.Before(since) {
			since = *d
		}
		// A retype keeps the row and its age, but the name is new: an archive older than that means another disk by it.
		if e := rows[i].ExternalIDSince; e != nil && e.After(since) {
			since = *e
		}
		out[rows[i].ExternalID] = pve.RestoreVolume{Name: strAny(vol["name"]), Since: since}
	}
	return out
}

// pveDateRestoredVolumes dates a disk the restore just brought back, one with no row before it, from the backup: the
// same archive can then restore it again. A row that was there stays as it is: an older backup may not hold that disk.
// A failure costs only that: the next restore refuses the disk by name.
func (h *Handler) pveDateRestoredVolumes(ctx context.Context, es *externalservice.ExternalService, proj *Project, serverExtID string, archivedAt time.Time) {
	rows, err := h.cloud.FindByServiceProjectAndType(ctx, es.ID, proj.ID, cloud.TypeVolume)
	if err != nil {
		slog.Error("restore: date the volumes", "project", proj.ID, "server", serverExtID, "err", err)
		return
	}
	for i := range rows {
		vol, _ := rows[i].Data["volume"].(map[string]any)
		if fmt.Sprint(intAny(vol["vmid"])) == serverExtID && rows[i].CreatedAt != nil && rows[i].CreatedAt.After(archivedAt) && rows[i].DataSince == nil {
			if err := h.cloud.SetDataSince(ctx, &rows[i], archivedAt); err != nil {
				slog.Error("restore: date the volume", "project", proj.ID, "volume", rows[i].ExternalID, "err", err)
			}
		}
	}
}

// enforcePVEGPURestore gates the GPUs a restore would bring back. It reads the hostpci entries, like the sync, since
// that is what the VM would hold; devices the server has now are its own and count once.
func (h *Handler) enforcePVEGPURestore(ctx context.Context, proj *Project, es *externalservice.ExternalService, cur, archive pve.VMConfig) error {
	have := pve.GPUCounts(cur, es.PVEGPUAliasOf)
	var usage map[string]int
	for alias, n := range pve.GPUCounts(archive, es.PVEGPUAliasOf) {
		more := n - have[alias]
		model := cloud.NormalizeGPUAlias(alias)
		limit, limited := gpuLimitFor(pveQuotaLimits(proj, es), model)
		if more <= 0 || !limited {
			continue
		}
		if usage == nil {
			var err error
			if usage, err = h.cloud.GPUUsageByProject(ctx, proj.ID); err != nil {
				return errQuotaUsageUnavailable()
			}
		}
		if usage[model]+more > limit {
			return httpx.NewError(http.StatusConflict, http.StatusConflict,
				fmt.Sprintf("GPU quota exceeded for %s: %d in use, %d requested, limit %d", model, usage[model], more, limit))
		}
	}
	return nil
}
