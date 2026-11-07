package admin

// kamajiclusters.go — the admin chart-pin surface for a kamaji Managed-Kubernetes provider:
// list every proxcloud-managed cluster's pinned chart version, and re-pin one (or all) onto the
// provider's CURRENT chartVersion. Clusters keep their pin by design (a provider bump must never
// roll customer clusters implicitly) — these endpoints are the operator's explicit override.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// writeClusterErr answers any error of a management-cluster call: a typed one as itself, anything else
// (an unreachable cluster, a bad kubeconfig) as a 502, the cluster's fault rather than the portal's.
func writeClusterErr(w http.ResponseWriter, err error) bool {
	var he *httpx.HTTPError
	if err == nil || errors.As(err, &he) {
		return httpx.WriteError(w, err)
	}
	httpx.Err(w, http.StatusBadGateway, http.StatusBadGateway, "management cluster: "+err.Error())
	return true
}

// kamajiService resolves the kamaji provider + its Service, writing the error response on
// failure. 501 when the factory is unwired (tests), 400 when the service is not kamaji.
func (h *Handler) kamajiService(w http.ResponseWriter, r *http.Request) (*externalservice.ExternalService, *kamaji.Service, bool) {
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return nil, nil, false
	}
	if !es.IsKamaji() {
		httpx.WriteError(w, httpx.BadRequest("not a Managed Kubernetes provider"))
		return nil, nil, false
	}
	if h.kamajiFor == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented,
			"managed kubernetes is not configured"))
		return nil, nil, false
	}
	ks, err := h.kamajiFor(es)
	if err != nil {
		writeClusterErr(w, err)
		return nil, nil, false
	}
	return es, ks, true
}

// kamajiClusterPins lists every managed cluster with its pinned chart version, plus the
// provider's own pin so the UI can mark who is behind. ADMIN_SERVICE_READ.
func (h *Handler) kamajiClusterPins(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	es, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	pins, err := ks.ListClusterPins(r.Context())
	if writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, map[string]any{
		"providerChartVersion": es.KamajiConfig().ChartVersion,
		"clusters":             pins,
	})
}

// kamajiClusterBump re-pins ONE cluster onto the provider's current chartVersion.
// ADMIN_SERVICE_MANAGE.
func (h *Handler) kamajiClusterBump(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	pin := es.KamajiConfig().ChartVersion
	if pin == "" {
		httpx.WriteError(w, httpx.BadRequest("the provider has no pinned chart version"))
		return
	}
	if err := ks.SetChartVersion(r.Context(), chi.URLParam(r, "clusterId"), pin); writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, map[string]any{"chartVersion": pin})
}

// CSIMigrator moves one cluster of a kamaji provider onto the CSI gateway (project.Handler.MigrateClusterCSI). The report
// comes back with an error too, when the migration stopped on it.
type CSIMigrator func(ctx context.Context, es *externalservice.ExternalService, ks *kamaji.Service, clusterID string, dryRun, force bool, adoptDetached []string) (any, error)

// SetCSIMigrator wires kamajiClusterCSIGateway (nil = 501).
func (h *Handler) SetCSIMigrator(f CSIMigrator) { h.csiMigrate = f }

// kamajiClusterCSIGateway moves ONE cluster made with the tenant-side proxmox-csi controller onto the CSI gateway:
// adopt its disks into the registry, switch the controller in one chart bump onto the provider's current chartVersion,
// delete the tenant's copy of the old config and revoke csi@pve!<id>. ?dryRun=true only reports; ?force=true goes on
// past PersistentVolumes it cannot adopt (their pods cannot attach afterwards); ?adoptDetached=pv1,pv2 names the PVs
// whose disk may be adopted although no worker of the cluster holds it, after the operator checked it is the
// cluster's. ADMIN_SERVICE_MANAGE.
func (h *Handler) kamajiClusterCSIGateway(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	if h.csiMigrate == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusNotImplemented, http.StatusNotImplemented, "the CSI gateway is not configured"))
		return
	}
	q := r.URL.Query()
	var detached []string
	for _, pv := range strings.Split(q.Get("adoptDetached"), ",") {
		if pv = strings.TrimSpace(pv); pv != "" {
			detached = append(detached, pv)
		}
	}
	rep, err := h.csiMigrate(r.Context(), es, ks, chi.URLParam(r, "clusterId"), q.Get("dryRun") == "true", q.Get("force") == "true", detached)
	var he *httpx.HTTPError
	if err != nil && rep != nil && errors.As(err, &he) {
		httpx.Raw(w, he.Status, map[string]any{"data": rep, "errors": httpx.APIError{Code: he.Code, Message: he.Msg}})
		return
	}
	if writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, rep)
}

// kamajiClusterBumpAll re-pins EVERY managed cluster onto the provider's current chartVersion.
// Best-effort per cluster; the response reports how many moved and any per-cluster errors.
// ADMIN_SERVICE_MANAGE.
func (h *Handler) kamajiClusterBumpAll(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	es, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	pin := es.KamajiConfig().ChartVersion
	if pin == "" {
		httpx.WriteError(w, httpx.BadRequest("the provider has no pinned chart version"))
		return
	}
	pins, err := ks.ListClusterPins(r.Context())
	if writeClusterErr(w, err) {
		return
	}
	bumped := 0
	errs := map[string]string{}
	for _, p := range pins {
		if p.ChartVersion == pin {
			continue
		}
		if err := ks.SetChartVersion(r.Context(), p.ID, pin); err != nil {
			errs[p.ID] = err.Error()
			continue
		}
		bumped++
	}
	httpx.OK(w, map[string]any{"chartVersion": pin, "bumped": bumped, "errors": errs})
}
