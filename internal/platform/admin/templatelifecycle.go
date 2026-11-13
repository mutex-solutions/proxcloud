package admin

// templatelifecycle.go — the lifecycle of imported templates (docs/template-updates.md, "Lifecycle"):
// state listing, smoke test, publish, withdraw. Every PVE-mutating action is gated by
// PROXCLOUD_TEMPLATE_UPDATES_VM_ENABLED (the service refuses with 409 when off) and by
// admin:service:manage; each successful action is audited by the admin audit middleware with a
// before/after snapshot (digest, VMIDs, reason, actor).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// SmokeNetworkOf reads a Proxmox provider's smoke-test network: config.templateSmoke =
// {"bridge":"vmbr0","vlan":29,"ip":"192.0.2.50/24","gateway":"192.0.2.1","nameserver":"192.0.2.53"}
// (ip empty = DHCP). Missing = smoke tests cannot run on that provider.
func SmokeNetworkOf(es *externalservice.ExternalService) templateupdates.SmokeNetwork {
	m, _ := es.Config["templateSmoke"].(map[string]any)
	str := func(k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	vlan := 0
	if f, ok := m["vlan"].(float64); ok {
		vlan = int(f)
	}
	return templateupdates.SmokeNetwork{Bridge: str("bridge"), VLAN: vlan, IP: str("ip"), Gateway: str("gateway"), Nameserver: str("nameserver")}
}

func (h *Handler) actor(r *http.Request) string {
	rc := httpx.RC(r.Context())
	if rc.Sub != "" {
		return rc.Sub
	}
	return rc.SigV4KeyID
}

// lifecyclePVE resolves the provider of an import (enabled Proxmox only).
func (h *Handler) lifecyclePVE(ctx context.Context, serviceID string) (*externalservice.ExternalService, *pve.Service, error) {
	if h.esSvc == nil || h.pveFor == nil {
		return nil, nil, &templateupdates.LifecycleError{Status: http.StatusServiceUnavailable, Code: "pve_unwired", Message: "Proxmox access is not wired"}
	}
	es, err := h.esSvc.Get(ctx, serviceID)
	if err != nil || es == nil || !es.IsProxmox() {
		return nil, nil, &templateupdates.LifecycleError{Status: http.StatusConflict, Code: "provider_missing", Message: "The import's Proxmox provider no longer exists"}
	}
	if es.IsDisabled() {
		return nil, nil, &templateupdates.LifecycleError{Status: http.StatusConflict, Code: "provider_disabled", Message: "The import's Proxmox provider is disabled"}
	}
	ps, err := h.pveFor(es)
	if err != nil {
		return nil, nil, &templateupdates.LifecycleError{Status: http.StatusConflict, Code: "provider_incomplete", Message: "The provider's configuration is incomplete"}
	}
	return es, ps, nil
}

func (h *Handler) templateUpdatesTemplates(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	rows := map[string]templateupdates.ProviderRows{}
	if h.esSvc != nil && h.pveFor != nil {
		services, _ := h.esSvc.ListByType(r.Context(), externalservice.TypeCloud)
		for i := range services {
			es := &services[i]
			if !es.IsProxmox() {
				continue
			}
			ps, err := h.pveFor(es)
			if err != nil {
				rows[es.ID] = templateupdates.ProviderRows{Error: "provider configuration is incomplete"}
				continue
			}
			got, err := ps.C.ClusterResources(r.Context(), "vm")
			if err != nil {
				rows[es.ID] = templateupdates.ProviderRows{Error: "the cluster could not be read"}
				continue
			}
			if got == nil {
				got = []pve.ClusterResource{}
			}
			rows[es.ID] = templateupdates.ProviderRows{Rows: got}
		}
	}
	items, err := h.templateUpdates.States(r.Context(), rows)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	smokes, err := h.templateUpdates.SmokeHistory(r.Context())
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	httpx.OK(w, map[string]any{"items": items, "smokes": smokes, "vmUpdatesEnabled": h.templateUpdates.VMEnabled})
}

func (h *Handler) templateUpdatesBlocklist(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesReadPerm) || !h.templateUpdatesReady(w) {
		return
	}
	items, err := h.templateUpdates.Blocklist(r.Context())
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	httpx.OK(w, map[string]any{"items": items})
}

// decodeLifecycleBody decodes an optional strict JSON body (empty = zero value).
func decodeLifecycleBody(w http.ResponseWriter, r *http.Request, out any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return true
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil || d.Decode(new(any)) != io.EOF {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return false
	}
	return true
}

// importService loads the import receipt's provider id (404 when the import is unknown).
func (h *Handler) importService(ctx context.Context, id string) (string, error) {
	rec, err := h.templateUpdates.Receipts.Load(ctx, id)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", &templateupdates.LifecycleError{Status: http.StatusNotFound, Code: "not_found", Message: "No template import with that id"}
	}
	return rec.Payload.ServiceID, nil
}

func (h *Handler) templateUpdatesSmoke(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.templateUpdatesReady(w) {
		return
	}
	var req struct{}
	if !decodeLifecycleBody(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	svc, err := h.importService(r.Context(), id)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	es, ps, err := h.lifecyclePVE(r.Context(), svc)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	rec, err := h.templateUpdates.Receipts.Load(r.Context(), id)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	net := SmokeNetworkOf(es)
	if err := templateupdates.CheckSmokeNetwork(r.Context(), ps.C, rec.Payload.Node, net); err != nil {
		templateUpdatesErr(w, err)
		return
	}
	sm, err := h.templateUpdates.EnqueueSmoke(r.Context(), id, net, h.actor(r), false)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	audit.RecordSnapshots(r.Context(), nil, map[string]any{"action": "smoke", "importId": id, "smokeJob": sm.ID, "digest": sm.Payload.Digest,
		"serviceId": sm.Payload.ServiceID, "node": sm.Payload.Node, "templateVmid": sm.Payload.TemplateVMID})
	httpx.OK(w, sm)
}

func (h *Handler) templateUpdatesPublish(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.templateUpdatesReady(w) {
		return
	}
	var req struct {
		ConfirmDigest string `json:"confirmDigest"`
	}
	if !decodeLifecycleBody(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	svc, err := h.importService(r.Context(), id)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	_, ps, err := h.lifecyclePVE(r.Context(), svc)
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	res, err := h.templateUpdates.Publish(r.Context(), ps.C, id, req.ConfirmDigest, h.actor(r))
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	audit.RecordSnapshots(r.Context(), map[string]any{"published": false}, map[string]any{"published": true, "action": "publish", "importId": id,
		"digest": res.Digest, "slug": res.Slug, "serviceId": res.ServiceID, "changedVmids": res.Changed, "smokeJob": res.Smoke, "unchanged": res.Unchanged})
	httpx.OK(w, res)
}

func (h *Handler) templateUpdatesWithdraw(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, templateUpdatesManagePerm) || !h.templateUpdatesReady(w) {
		return
	}
	var req templateupdates.WithdrawRequest
	if !decodeLifecycleBody(w, r, &req) {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.importService(r.Context(), id); err != nil {
		templateUpdatesErr(w, err)
		return
	}
	// Every provider holding a copy of the artifact is resolved (a disabled provider still is: a
	// withdrawn digest must not stay published anywhere).
	resolve := func(ctx context.Context, serviceID string) (templateupdates.LifecyclePVE, error) {
		if h.esSvc == nil || h.pveFor == nil {
			return nil, errors.New("Proxmox access is not wired")
		}
		es, err := h.esSvc.Get(ctx, serviceID)
		if err != nil {
			return nil, err
		}
		if es == nil || !es.IsProxmox() {
			return nil, &templateupdates.LifecycleError{Status: http.StatusConflict, Code: "provider_missing", Message: "The provider no longer exists"}
		}
		ps, err := h.pveFor(es)
		if err != nil {
			return nil, err
		}
		return ps.C, nil
	}
	res, err := h.templateUpdates.Withdraw(r.Context(), resolve, id, req, h.actor(r))
	if err != nil {
		templateUpdatesErr(w, err)
		return
	}
	audit.RecordSnapshots(r.Context(), map[string]any{"withdrawn": false}, map[string]any{"withdrawn": true, "action": "withdraw", "importId": id,
		"digest": res.Digest, "slug": res.Slug, "serviceId": res.ServiceID, "reason": strings.TrimSpace(req.Reason), "force": req.Force,
		"confirmNodeLoss": req.ConfirmNodeLoss, "changedVmids": res.Changed, "remainingPublished": res.Remaining, "complete": res.Complete,
		"providers": res.Providers})
	httpx.OK(w, res)
}
