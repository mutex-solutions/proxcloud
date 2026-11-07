package project

// snapshot_pve.go — the server snapshot actions (pve/snapshot.go).

import (
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// pveSnapshotAction handles LIST_SNAPSHOTS / CREATE_SNAPSHOT / ROLLBACK_SNAPSHOT / DELETE_SNAPSHOT on
// a SERVER. Returns handled.
func (h *Handler) pveSnapshotAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	if cr.Type != cloud.TypeServer {
		return false
	}
	switch action {
	case "LIST_SNAPSHOTS", "CREATE_SNAPSHOT", "ROLLBACK_SNAPSHOT", "DELETE_SNAPSHOT":
	default:
		return false
	}
	ctx := r.Context()
	ps, ok := h.pveService(w, es)
	if !ok {
		return true
	}
	if action == "LIST_SNAPSHOTS" {
		snaps, err := ps.ListServerSnapshots(ctx, proj.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, pveVolumeErr(err))
			return true
		}
		httpx.OK(w, map[string]any{"result": snaps})
		return true
	}
	name := strAny(data["name"])
	var err error
	switch action {
	case "CREATE_SNAPSHOT":
		if !pve.ValidSnapshotName(name) {
			h.fail(w, httpx.BadRequest("A snapshot name starts with a letter and uses 2 to 40 letters, digits, dashes and underscores"))
			return true
		}
		caps, cerr := ps.C.DetectStorage(ctx)
		if cerr != nil {
			h.fail(w, upstreamErr(computeBackend, cerr))
			return true
		}
		err = ps.CreateServerSnapshot(ctx, proj.ID, cr.ExternalID, name, strAny(data["description"]), func(storage string) bool {
			return caps[storage].Snapshots && es.PVEEffectiveStorageClass(storage) != pve.ClassCapacity
		})
	case "ROLLBACK_SNAPSHOT":
		caps, cerr := ps.C.DetectStorage(ctx)
		if cerr != nil {
			h.fail(w, upstreamErr(computeBackend, cerr))
			return true
		}
		err = ps.RollbackServerSnapshot(ctx, proj.ID, cr.ExternalID, name, func(storage string) bool {
			return caps[storage].Type == "zfspool" || caps[storage].Type == "zfs"
		})
	case "DELETE_SNAPSHOT":
		err = ps.DeleteServerSnapshot(ctx, proj.ID, cr.ExternalID, name)
	}
	if err != nil {
		h.fail(w, pveVolumeErr(err))
		return true
	}
	h.pveRefreshServerRow(ctx, ps, proj, cr)
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_ACTION", action, cr)
	httpx.OK(w, map[string]any{"result": map[string]any{"name": name}})
	return true
}
