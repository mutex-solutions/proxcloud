package project

// cloud_zones.go — DNS zones on the powerdns provider: create and delete, record actions on the row.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/powerdns"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// zoneErr maps a refusal the user can act on to 409; anything else is the DNS server failing.
func zoneErr(err error) error {
	// CreateWithGate returns locally generated permission/proof refusals too;
	// preserve those safe HTTP errors rather than misreporting them as upstream.
	var he *httpx.HTTPError
	if errors.As(err, &he) {
		return he
	}
	if errors.Is(err, powerdns.ErrZone) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, strings.TrimPrefix(err.Error(), powerdns.ErrZone.Error()+": "))
	}
	return upstreamErr("DNS server", err)
}

// upstreamErr logs why a backend failed and tells the tenant only that it did: the raw error names the backend's internal
// address, which tenants must not learn.
func upstreamErr(what string, err error) *httpx.HTTPError {
	if errors.Is(err, pve.ErrNotInProject) {
		return httpx.NewError(http.StatusNotFound, http.StatusNotFound, "Server not found")
	}
	slog.Error("backend request failed", "backend", what, "err", err)
	return httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, "The "+what+" is unavailable. Try again later.")
}

func (h *Handler) zoneCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	if req.Type != cloud.TypeDNSZone {
		h.fail(w, httpx.BadRequest("This provider serves DNS zones only"))
		return
	}
	if h.dnsOwnership == nil {
		h.fail(w, httpx.NewError(503, 503, "DNS ownership verification is unavailable. Contact your operator."))
		return
	}
	// Native cloud and MCP both land here. No boolean, arbitrary token or
	// previously existing zone substitutes for a fresh durable scoped proof.
	name, err := powerdns.CanonicalZone(strAny(req.Data["name"]))
	if err != nil {
		h.fail(w, zoneErr(err))
		return
	}
	id := strAny(req.Data["ownershipId"])
	if id == "" {
		h.fail(w, dnsConflict("Verify domain ownership first. Start a DNS ownership challenge, publish its TXT record at your current DNS host, then verify it before creating the zone."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Reject absent/pending/foreign proofs before a tenant can occupy the global
	// create lock or cause any PowerDNS I/O. Revalidate atomically when consumed.
	preflight, err := h.dnsLoad(ctx, proj, es.ID, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	if preflight.Name != name || preflight.state(time.Now().UTC()) != "VERIFIED" {
		h.fail(w, dnsConflict("A fresh verified ownership challenge for this exact domain is required."))
		return
	}
	var saved *cloud.CloudResource
	acquired, err := h.dnsOwnership.DB().WithAdvisoryLock(ctx, dnsCreateLock, true, func() error {
		// Refresh binding/availability under the create lock. The explicit
		// provider selector must never silently fall back to another provider.
		freshProject, err := h.svc.GetProject(ctx, u.Sub, proj.ID)
		if err != nil {
			return err
		}
		if err := h.policy.RequireProjectPermission(ctx, u.Sub, freshProject, rbac.ProjectCloudResourceManage); err != nil {
			return err
		}
		if err := refuseNewUsage(freshProject); err != nil {
			return err
		}
		current, err := h.dnsProvider(ctx, freshProject, es.ID)
		if err != nil {
			return err
		}
		if selected := r.Header.Get("x-service-id"); selected != "" && selected != es.ID {
			return httpx.NotFound("DNS service not found on this project")
		}
		if selected := r.URL.Query().Get("serviceId"); selected != "" && selected != es.ID {
			return httpx.NotFound("DNS service not found on this project")
		}
		ds, _, binding, err := dnsConfig(current)
		if err != nil {
			return upstreamErr("DNS verification configuration", err)
		}
		freshProof, err := h.dnsLoad(ctx, freshProject, current.ID, id)
		if err != nil {
			return err
		}
		if freshProof.Name != name || freshProof.Binding != binding || freshProof.state(time.Now().UTC()) != "VERIFIED" {
			return dnsConflict("Ownership proof expired or DNS provider settings changed. Start a new challenge.")
		}
		// One project's valid proofs must not repeatedly monopolize the global
		// namespace lock on a slow backend. Commit cooldown before upstream I/O;
		// a busy lock or invalidated proof above never burns this attempt budget.
		if err := h.dnsCreateCooldown(ctx, proj.ID); err != nil {
			return err
		}
		z, err := ds.CreateWithGate(ctx, proj.ID, name, func() error {
			return h.consumeDNSProof(ctx, u, proj, current, name, id, binding)
		})
		if err != nil {
			return zoneErr(err)
		}
		now := time.Now().UTC()
		saved, err = h.cloud.Insert(ctx, &cloud.CloudResource{
			ServiceID: es.ID, ProjectID: proj.ID, Region: es.CephRegion(), Type: cloud.TypeDNSZone,
			ExternalID: z.Name, Data: ds.ZoneData(z), CreatedAt: &now, UpdatedAt: &now,
		})
		return err
	})
	if err == nil && !acquired {
		err = dnsConflict("Another DNS zone is being created. No proof was consumed; wait and retry.")
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	httpx.OK(w, *saved)
}

func (h *Handler) zoneDelete(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	ds, err := es.PowerDNSService()
	if err != nil {
		return err
	}
	if err := ds.Delete(ctx, proj.ID, cr.ExternalID); err != nil {
		return zoneErr(err)
	}
	return nil
}

// zoneAction: LIST_RECORDS, SET_RECORD {name, type, ttl?, records[]} replaces one name and type, DELETE_RECORD {name, type}.
func (h *Handler) zoneAction(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	ds, err := es.PowerDNSService()
	if err != nil {
		h.fail(w, err)
		return true
	}
	ctx := r.Context()
	switch action {
	case "LIST_RECORDS":
		rows, err := ds.Records(ctx, proj.ID, cr.ExternalID)
		if err != nil {
			h.fail(w, zoneErr(err))
			return true
		}
		httpx.OK(w, map[string]any{"result": rows})
	case "SET_RECORD":
		var contents []string
		if list, ok := data["records"].([]any); ok {
			for _, v := range list {
				if s, ok := v.(string); ok {
					contents = append(contents, s)
				}
			}
		}
		if err := ds.SetRecord(ctx, proj.ID, cr.ExternalID, strAny(data["name"]), strAny(data["type"]), intAny(data["ttl"]), contents); err != nil {
			h.fail(w, zoneErr(err))
			return true
		}
		h.audit.LogAsync(newZoneRecordEvent(u, proj, action, cr, data, contents))
		httpx.OK(w, map[string]any{"result": map[string]any{}})
	case "DELETE_RECORD":
		if err := ds.DeleteRecord(ctx, proj.ID, cr.ExternalID, strAny(data["name"]), strAny(data["type"])); err != nil {
			h.fail(w, zoneErr(err))
			return true
		}
		h.audit.LogAsync(newZoneRecordEvent(u, proj, action, cr, data, nil))
		httpx.OK(w, map[string]any{"result": map[string]any{}})
	default:
		return false
	}
	return true
}

// newZoneRecordEvent is the audit event of a record write. It carries the record, since "zone changed" cannot trace a
// repointed MX.
func newZoneRecordEvent(u *user.User, p *Project, action string, cr *cloud.CloudResource, data map[string]any, contents []string) audit.AuditEvent {
	ev := newCloudResourceEvent(u, p, "CLOUD_RESOURCE_ACTION", action, cr)
	record := map[string]any{"name": strAny(data["name"]), "type": strAny(data["type"])}
	if contents != nil {
		record["ttl"] = intAny(data["ttl"])
		if proofRecord(strAny(data["name"]), strAny(data["type"])) {
			record["recordsRedacted"] = true
		} else {
			record["records"] = contents
		}
	}
	ev.ResourceMetadata["record"] = record
	return ev
}

// powerDNSServices lists the project's DNS providers.
func (h *Handler) powerDNSServices(ctx context.Context, p *Project) ([]*externalservice.ExternalService, error) {
	var out []*externalservice.ExternalService
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if errors.Is(err, externalservice.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if es.IsPowerDNS() {
			out = append(out, es)
		}
	}
	return out, nil
}

// teardownZones removes the project's zones and their rows; the rest of rows stays for the caller.
func (h *Handler) teardownZones(ctx context.Context, p *Project, es *externalservice.ExternalService, rows []cloud.CloudResource) ([]cloud.CloudResource, error) {
	ds, err := es.PowerDNSService()
	if err != nil {
		return rows, err
	}
	list, err := ds.List(ctx, p.ID)
	if err != nil {
		return rows, err
	}
	for i := range list {
		if err := ds.Delete(ctx, p.ID, list[i].Name); err != nil {
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
