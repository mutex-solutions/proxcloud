package admin

// templatepublishall.go — "Publish on all nodes" (docs/template-updates.md):
//
//	GET  /admin/template-updates/publish-all?serviceId=&digest=   the per-node report   (admin:service:read)
//	POST /admin/template-updates/publish-all                      one advance           (admin:service:manage)
//
// The handler only reads the provider (nodes, storages, the default storage profile, the smoke
// network); templateupdates.PublishAll decides and acts.

import (
	"context"
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type publishAllRequest struct {
	ServiceID     string `json:"serviceId"`
	Package       string `json:"package"`
	Digest        string `json:"digest"`
	ConfirmDigest string `json:"confirmDigest"`
}

func (h *Handler) templateUpdatesPublishAllGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	q := r.URL.Query()
	h.publishAll(w, r, publishAllRequest{ServiceID: q.Get("serviceId"), Digest: q.Get("digest")}, false)
}

func (h *Handler) templateUpdatesPublishAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.templateUpdatesReady(w) {
		return
	}
	var req publishAllRequest
	if !decodeStrictBody(w, r, &req) {
		return
	}
	if req.Package == "" || req.Digest == "" || req.ConfirmDigest != req.Digest {
		// Refused before any provider or GitHub call.
		httpx.WriteError(w, httpx.BadRequest("Give the package and confirm the exact digest to publish"))
		return
	}
	h.publishAll(w, r, req, true)
}

func (h *Handler) publishAll(w http.ResponseWriter, r *http.Request, req publishAllRequest, act bool) {
	ctx := r.Context()
	es, ps, err := h.lifecyclePVE(ctx, req.ServiceID)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	nodes, err := ps.C.Nodes(ctx)
	if err != nil {
		templateUpdatesErr(w, &templateupdates.LifecycleError{Status: http.StatusBadGateway, Code: "pve_unreadable", Message: "The provider's nodes could not be read; nothing was changed"})
		return
	}
	opts, err := h.placementOptions(ctx, es)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	profile := es.PVEStorageProfiles()[es.PVEDefaultStorageProfile()]
	rep, err := h.templateUpdates.PublishAll(ctx, ps.C, templateupdates.PublishAllInput{
		ServiceID: es.ID, Package: req.Package, Digest: req.Digest, ConfirmDigest: req.ConfirmDigest,
		Nodes: nodes, Options: opts, ImportStorage: es.PVEImportStorage(), DiskStorage: profile.StorageFor,
		SmokeNet: func(ctx context.Context, _, node string) (templateupdates.SmokeNetwork, error) {
			net := SmokeNetworkOf(es)
			return net, templateupdates.CheckSmokeNetwork(ctx, ps.C, node, net)
		},
	}, h.actor(r), act)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	if act && len(rep.Actions) > 0 {
		audit.RecordSnapshots(ctx, nil, map[string]any{"action": "publish-all", "serviceId": es.ID, "package": req.Package, "digest": req.Digest,
			"did": rep.Actions, "versionsLine": rep.VersionsLine, "complete": rep.Complete, "nodesMissing": rep.NodesMissing})
	}
	httpx.OK(w, rep)
}
