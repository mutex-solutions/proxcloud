package project

// backup_pve.go — the server backup actions (pve/backup.go).

import (
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
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
	switch action {
	case "CREATE_BACKUP":
		caps, cerr := ps.C.DetectStorage(ctx)
		if cerr != nil {
			h.fail(w, httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, cerr.Error()))
			return true
		}
		err = ps.CreateServerBackup(ctx, proj.ID, cr.ExternalID, store, caps[store], es.PVEStorageHeadroomPct())
	case "RESTORE_BACKUP":
		err = ps.RestoreServerBackup(ctx, proj.ID, cr.ExternalID, store, id)
	case "DELETE_BACKUP":
		err = ps.DeleteServerBackup(ctx, proj.ID, cr.ExternalID, store, id)
	}
	if err != nil {
		if action == "RESTORE_BACKUP" {
			h.pveSyncVolumes(ctx, es, proj)
		}
		h.fail(w, pveVolumeErr(err))
		return true
	}
	if action == "RESTORE_BACKUP" {
		// The disks are new volumes, whatever the archive named them.
		h.pveSyncVolumes(ctx, es, proj)
	}
	h.pveRefreshServerRow(ctx, ps, proj, cr)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{"id": id}})
	return true
}
