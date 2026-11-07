package project

// cloud_zones.go — DNS zones on the powerdns provider: create and delete, record actions on the row.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/powerdns"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// zoneErr maps a refusal the user can act on to 409; anything else is the DNS server failing.
func zoneErr(err error) error {
	if errors.Is(err, powerdns.ErrZone) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, strings.TrimPrefix(err.Error(), powerdns.ErrZone.Error()+": "))
	}
	return httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error())
}

func (h *Handler) zoneCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	if req.Type != cloud.TypeDNSZone {
		h.fail(w, httpx.BadRequest("This provider serves DNS zones only"))
		return
	}
	ds, err := es.PowerDNSService()
	if err != nil {
		h.fail(w, err)
		return
	}
	ctx := r.Context()
	name, _ := req.Data["name"].(string)
	now := time.Now().UTC()
	z, err := ds.Create(ctx, proj.ID, name)
	if err != nil {
		h.fail(w, zoneErr(err))
		return
	}
	saved, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: es.ID, ProjectID: proj.ID, Region: es.CephRegion(), Type: cloud.TypeDNSZone,
		ExternalID: z.Name, Data: ds.ZoneData(z), CreatedAt: &now, UpdatedAt: &now,
	})
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
func (h *Handler) zoneAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
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
		httpx.OK(w, map[string]any{"result": map[string]any{}})
	case "DELETE_RECORD":
		if err := ds.DeleteRecord(ctx, proj.ID, cr.ExternalID, strAny(data["name"]), strAny(data["type"])); err != nil {
			h.fail(w, zoneErr(err))
			return true
		}
		httpx.OK(w, map[string]any{"result": map[string]any{}})
	default:
		return false
	}
	return true
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
