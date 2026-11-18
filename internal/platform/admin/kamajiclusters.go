package admin

// kamajiclusters.go — the admin chart-pin surface for a kamaji Managed-Kubernetes provider:
// list every proxcloud-managed cluster's pinned chart version, and re-pin one (or all) onto the
// provider's CURRENT chartVersion. Clusters keep their pin by design (a provider bump must never
// roll customer clusters implicitly) — these endpoints are the operator's explicit override.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/kamaji"
	"github.com/mutex-solutions/proxcloud/internal/cloud/kamajik8s"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
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
	if writeReportErr(w, rep, err) || writeClusterErr(w, err) {
		return
	}
	httpx.OK(w, rep)
}

// writeReportErr answers a typed error that comes with a report (a migration that stopped half way): both, at the
// error's status. That is not httpx.WriteError, so why the backend failed is logged here (httpx.LogCause): after a
// partial migration the log is the only place that says which step stopped it.
func writeReportErr(w http.ResponseWriter, rep any, err error) bool {
	var he *httpx.HTTPError
	if rep == nil || !errors.As(err, &he) {
		return false
	}
	httpx.LogCause(w, he)
	httpx.Raw(w, he.Status, map[string]any{"data": rep, "errors": httpx.APIError{Code: he.Code, Message: he.Msg}})
	return true
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

// A PVE API token as CAPMOX and the Proxmox client print it: "PVEAPIToken=user@pve!id=<secret>". RedactBuildText's
// token= rule needs a word boundary before "token", which "PVEAPIToken" does not have.
var pveTokenInText = regexp.MustCompile(`(?i)(PVEAPIToken=[^=\s]+=)\S+`)

// machineText takes the credentials out of an upstream text.
func machineText(msg string) string {
	return pveTokenInText.ReplaceAllString(sandbox.RedactBuildText(msg), "${1}[REDACTED]")
}

// machineMessage is an upstream condition message made fit for the operator's screen: credentials out, 400 runes.
func machineMessage(msg string) string {
	msg = machineText(msg)
	if r := []rune(msg); len(r) > 400 {
		msg = string(r[:400]) + "…"
	}
	return msg
}

// kamajiClusterMachines is the operator's view of what a tenant sees as a reason code: every worker Machine of ONE
// cluster with the conditions Cluster API and CAPMOX report, read live, and the node groups' MachineHealthChecks. A
// machine that did not join within its bound also carries joinError: what cloud-init in the guest says, read through
// the guest agent and masked (kamajimachines.go). ADMIN_SERVICE_READ.
func (h *Handler) kamajiClusterMachines(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:read") {
		return
	}
	_, ks, ok := h.kamajiService(w, r)
	if !ok {
		return
	}
	out, err := ks.ClusterMachines(r.Context(), chi.URLParam(r, "clusterId"), time.Now())
	var ae *kamajik8s.APIError
	switch {
	case errors.Is(err, kamaji.ErrClusterNotFound):
		err = httpx.NotFound("cluster not found")
	case errors.As(err, &ae) && ae.Status == http.StatusForbidden:
		// An install older than this endpoint: its ClusterRole has no read on Machines.
		err = httpx.NewError(http.StatusBadGateway, http.StatusBadGateway,
			"The ProxCloud service account cannot read Machines: re-apply deploy/mgmt-cluster/rbac.yaml")
	}
	if writeClusterErr(w, err) {
		return
	}
	redact := func(conds []kamaji.Condition) {
		for i := range conds {
			conds[i].Message = machineMessage(conds[i].Message)
		}
	}
	redact(out.ProxmoxCluster.Conditions)
	for i := range out.Machines {
		out.Machines[i].FailureMessage = machineMessage(out.Machines[i].FailureMessage)
		redact(out.Machines[i].Conditions)
	}
	h.addJoinErrors(r.Context(), ks, out)
	httpx.OK(w, out)
}
