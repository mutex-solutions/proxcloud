package admin

// dbaasclusters.go — the admin chart-pin surface for a dbaas Managed-Database provider: list
// every proxcloud-managed database's pinned chart version, and re-pin one (or all) onto the
// provider's CURRENT chartVersion. Databases keep their pin by design (a provider bump must
// never roll customer databases implicitly) — these endpoints are the operator's explicit
// override (kamajiclusters.go precedent).

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/dbaas"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// dbaasService resolves the dbaas provider + its Service, writing the error response on
// failure. 501 when the factory is unwired (tests), 400 when the service is not dbaas.
func (h *Handler) dbaasService(w http.ResponseWriter, r *http.Request) (*externalservice.ExternalService, *dbaas.Service, bool) {
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return nil, nil, false
	}
	if !es.IsDbaas() {
		httpx.WriteError(w, httpx.BadRequest("not a Managed Database provider"))
		return nil, nil, false
	}
	if h.dbaasFor == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"managed databases are not configured"))
		return nil, nil, false
	}
	ds, err := h.dbaasFor(es)
	if err != nil {
		writeClusterErr(w, err)
		return nil, nil, false
	}
	return es, ds, true
}

// dbaasClusterPins lists every managed database with its pinned chart version, plus the
// provider's own pin so the UI can mark who is behind. ADMIN_SERVICE_READ.
func (h *Handler) dbaasClusterPins(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ds, ok := h.dbaasService(w, r)
	if !ok {
		return
	}
	pins, err := ds.ListDatabasePins(r.Context())
	if writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, map[string]any{
		"providerChartVersion": es.DbaasConfig().ChartVersion,
		"databases":            pins,
	})
}

// dbaasClusterBump re-pins ONE database onto the provider's current chartVersion.
// ADMIN_SERVICE_MANAGE.
func (h *Handler) dbaasClusterBump(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ds, ok := h.dbaasService(w, r)
	if !ok {
		return
	}
	pin := es.DbaasConfig().ChartVersion
	if pin == "" {
		httpx.WriteError(w, httpx.BadRequest("the provider has no pinned chart version"))
		return
	}
	if err := ds.SetChartVersion(r.Context(), chi.URLParam(r, "clusterId"), pin); writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, map[string]any{"chartVersion": pin})
}

// dbaasClusterBumpAll re-pins EVERY managed database onto the provider's current chartVersion.
// Best-effort per database; the response reports how many moved and any per-database errors.
// ADMIN_SERVICE_MANAGE.
func (h *Handler) dbaasClusterBumpAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ds, ok := h.dbaasService(w, r)
	if !ok {
		return
	}
	pin := es.DbaasConfig().ChartVersion
	if pin == "" {
		httpx.WriteError(w, httpx.BadRequest("the provider has no pinned chart version"))
		return
	}
	pins, err := ds.ListDatabasePins(r.Context())
	if writeClusterErr(w, err) {
		return
	}
	bumped := 0
	errs := map[string]string{}
	for _, p := range pins {
		if p.ChartVersion == pin {
			continue
		}
		if err := ds.SetChartVersion(r.Context(), p.ID, pin); err != nil {
			errs[p.ID] = err.Error()
			continue
		}
		bumped++
	}
	httpx.OK(w, map[string]any{"chartVersion": pin, "bumped": bumped, "errors": errs})
}
