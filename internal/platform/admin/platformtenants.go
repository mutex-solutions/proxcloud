package admin

// platformtenants.go — what makes an installed managed-services platform usable by a tenant (design decision 51,
// internal/platformlife/tenants.go):
//
//	POST /admin/platform/installations/{id}/kubernetes-version   offer the BOM's Kubernetes version on the
//	                                                             installation's Kubernetes provider, when a node image
//	                                                             of it is published on every node the platform cluster
//	                                                             uses                                (service:manage)
//
// and PlatformAttach, which a run's register stage calls to apply spec.tenants.attach with the paths an operator uses
// by hand: the provider's attach policy and a bulk attach of every project.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platformlife"
	"github.com/mutex-solutions/proxcloud/internal/providerattach"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// platformOfferVersion handles POST …/kubernetes-version. It reads the Proxmox cluster, publishes nothing and
// overwrites nothing: 409 node_image_unpublished names the nodes without a published node image, and a version the
// provider already names is answered as kept. The admin audit middleware records the call; the provider's versions
// before and after are its snapshots.
func (h *Handler) platformOfferVersion(w http.ResponseWriter, r *http.Request) {
	if !h.platformGate(w, r, externalServiceManagePerm) {
		return
	}
	ctx := r.Context()
	in, err := h.platform.Get(ctx, chi.URLParam(r, "id"))
	if platformStoreErr(w, err) {
		return
	}
	if in.Status.Providers == nil || in.Status.Providers.Kubernetes == "" || in.Status.BOM == nil {
		platformErr(w, http.StatusConflict, "no_provider", "This installation has no Kubernetes provider yet: install it first.", nil)
		return
	}
	bom, ok := platformlife.Lookup(in.Status.BOM.SHA256)
	if !ok {
		platformStoreErr(w, platformlife.ErrBOMNotHere)
		return
	}
	if h.esSvc == nil || h.pveFor == nil {
		httpx.WriteError(w, serviceNotFoundErr(in.ServiceID))
		return
	}
	es, err := h.esSvc.Get(ctx, in.ServiceID)
	if err != nil || es == nil {
		httpx.WriteError(w, serviceNotFoundErr(in.ServiceID))
		return
	}
	portal, err := h.pveFor(es)
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Proxmox connection failed: "+err.Error()))
		return
	}
	rows, err := portal.C.ClusterResources(ctx, "vm")
	if err != nil {
		platformErr(w, http.StatusBadGateway, "pve_unreadable", "Proxmox did not list its templates; nothing was changed.", nil)
		return
	}
	version := bom.Kubernetes.Version
	line, missing := platformlife.NodeImageLine(bom.BOM, in, rows)
	if line == "" {
		platformErr(w, http.StatusConflict, "node_image_unpublished", fmt.Sprintf("No node image of Kubernetes %s is published on %s. "+
			"Publish %s there (Template updates, Publish on all nodes), then offer the version again.", version, platformlife.NodesText(missing), bom.Kubernetes.NodeImage.Tag),
			map[string]any{"version": version, "missingNodes": missing})
		return
	}
	id := in.Status.Providers.Kubernetes
	var before, after map[string]any
	offered := false
	err = h.repo.db.WithTx(ctx, func(ctx context.Context) error {
		// The lock a run's register stage writes the row under.
		if err := h.repo.db.LockXact(ctx, "platform-run:"+in.ID); err != nil {
			return err
		}
		row, err := h.repo.FindDoc(ctx, externalServiceCollection, id)
		if err != nil {
			return err
		}
		if row == nil {
			return serviceNotFoundErr(id)
		}
		before = providerVersions(row)
		if offered = platformlife.OfferVersion(row, version, line); offered {
			err = h.repo.ReplaceDoc(ctx, externalServiceCollection, id, row)
		}
		after = providerVersions(row)
		if err != nil {
			return err
		}
		// A run offers a version once: with this, an apply does not add again a version removed after this call.
		return h.platform.MarkKubernetesOffered(ctx, in.ID, version)
	})
	if httpx.WriteError(w, err) {
		return
	}
	if offered {
		audit.RecordSnapshots(ctx, map[string]any{"serviceId": id, "versions": before}, map[string]any{"serviceId": id, "versions": after})
	}
	httpx.OK(w, map[string]any{"version": version, "line": line, "offered": offered, "serviceId": id, "versions": after})
}

// providerVersions is a copy of a Kubernetes provider row's config.cluster.versions.
func providerVersions(row map[string]any) map[string]any {
	cfg, _ := row["config"].(map[string]any)
	cluster, _ := cfg["cluster"].(map[string]any)
	versions, _ := cluster["versions"].(map[string]any)
	out := maps.Clone(versions)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// PlatformAttach applies an installation's attach choice to one of its two providers (platformlife.Runner.Attach):
// the attach policy on for "all" and "new" and off for "none", and for "all" one bulk attach of every project, the
// run an operator starts on the provider's Projects tab. Each change is audited with actor as its author and the
// installation in its metadata. done=false with a note when the bulk attach could not start for a reason the next
// apply may find gone; the policy is written all the same.
func (h *Handler) PlatformAttach(ctx context.Context, installationID, serviceID, choice, actor string) (done bool, note string, err error) {
	if h.esSvc == nil {
		return false, "", serviceNotFoundErr(serviceID)
	}
	es, err := h.esSvc.Get(ctx, serviceID)
	if err != nil {
		return false, "", err
	}
	if es == nil {
		return false, "", serviceNotFoundErr(serviceID)
	}
	on := choice != platformlife.AttachNone
	if es.AttachToNewProjects() != on {
		if _, err := h.repo.SetAttachPolicy(ctx, serviceID, on); err != nil {
			return false, "", err
		}
		ev := platformAttachEvent(actor, es, audit.ActionUpdate, installationID, choice)
		ev.Changes = []audit.PropertyChange{{Field: "config." + externalservice.AttachPolicyKey, OldValue: fmt.Sprint(!on), NewValue: fmt.Sprint(on)}}
		h.logEvent(ev)
	}
	policy := es.Name + ": new projects get it"
	if !on {
		policy = es.Name + ": held back from new projects"
	}
	if choice != platformlife.AttachAll {
		return true, policy + ".", nil
	}
	switch {
	case es.IsDisabled():
		return false, policy + ". Existing projects were not attached: the provider is disabled. Enable it, then apply again.", nil
	case h.providerAttach == nil:
		return false, policy + ". Existing projects were not attached: bulk attach is not configured on this API.", nil
	}
	if h.canAttach != nil {
		if err := h.canAttach(es); err != nil {
			return false, policy + ". Existing projects were not attached: " + err.Error(), nil
		}
	}
	scope := providerattach.Scope{Mode: providerattach.ScopeAll}
	preview, err := h.providerAttach.Repo.Preview(ctx, scope, es.ID)
	if err != nil {
		return false, "", err
	}
	switch {
	case preview.ToAttach == 0:
		return true, policy + "; no existing project lacks it.", nil
	case preview.ToAttach > providerattach.MaxRunProjects:
		return false, fmt.Sprintf("%s. Existing projects were not attached: %d lack it and one run attaches at most %d. Attach them in parts on the provider's Projects tab.",
			policy, preview.ToAttach, providerattach.MaxRunProjects), nil
	}
	run, err := h.providerAttach.Start(ctx, providerattach.StartRequest{ServiceID: es.ID, ServiceName: es.Name, Scope: scope, Actor: actor})
	if errors.Is(err, providerattach.ErrActive) {
		return true, policy + "; a bulk attach of it is already running.", nil
	}
	if err != nil {
		return false, "", err
	}
	ev := platformAttachEvent(actor, es, ActionBulkAttach, installationID, choice)
	maps.Copy(ev.ResourceMetadata, map[string]any{"operation": "start", "runId": run.ID, "jobId": run.JobID, "scope": map[string]any{"mode": scope.Mode},
		"concurrency": run.Concurrency, "counts": map[string]any{"inScope": preview.InScope, "alreadyAttached": preview.AlreadyAttached,
			"deleting": preview.Deleting, "toAttach": preview.ToAttach}})
	h.logEvent(ev)
	return true, fmt.Sprintf("%s; a bulk attach of %d existing projects was started (its progress is on the provider's Projects tab).", policy, preview.ToAttach), nil
}

// platformAttachEvent is the audit entry of a change PlatformAttach made for a run.
func platformAttachEvent(actor string, es *externalservice.ExternalService, action, installationID, choice string) audit.AuditEvent {
	ev := audit.AdminEvent(actor, actor)
	ev.ResourceMetadata = map[string]any{"platformInstallationId": installationID, "attach": choice}
	if strings.HasPrefix(actor, templateupdates.PlatformRunActor) { // a run without a requester: the system's change, not an admin's
		ev = audit.SystemEvent()
		ev.ResourceMetadata = map[string]any{"platformInstallationId": installationID, "attach": choice, "requestedBy": actor}
	}
	ev.EventContext = audit.ContextPlatform
	ev.Action, ev.ResourceType, ev.ResourceID, ev.ResourceDisplayName = action, "EXTERNAL_SERVICE", es.ID, es.Name
	ev.Outcome = audit.OutcomeSuccess
	return ev
}

func (h *Handler) logEvent(ev audit.AuditEvent) {
	if h.audit != nil {
		h.audit.LogEvent(ev)
	}
}
