package admin

// metricstest.go serves POST /api/v1/admin/service/{id}/metrics-test — the live probe behind
// the admin Metrics tab's "Test connection" button. It exists so a mis-configured Prometheus
// source fails HERE, loudly, at save time — the alternative failure mode is the hourly
// metrics job logging per-server errors while traffic silently goes unbilled.
//
// The probe is read-only against the configured endpoint: (1) liveness (`vector(1)` proves
// URL + auth + tenant header + path prefix), (2) count of incoming-traffic series over the
// last hour (proves the SCHEMA actually matches what the endpoint carries), (3) the same
// count at the start of the current month (proves retention covers the billing window —
// month-to-date usage silently undercounts otherwise).

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/metrics"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// metricsTestTimeout bounds the whole 3-query probe.
const metricsTestTimeout = 20 * time.Second

func (h *Handler) externalServiceMetricsTest(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, externalServiceManagePerm) {
		return
	}
	if h.esSvc == nil {
		httpx.WriteError(w, httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError, "external-service backend not configured"))
		return
	}
	id := chi.URLParam(r, "id")
	es, err := h.esSvc.Get(r.Context(), id)
	if err != nil || es == nil {
		httpx.WriteError(w, serviceNotFoundErr(id))
		return
	}
	if es.MetricsSource() != externalservice.MetricsSourcePrometheus {
		httpx.WriteError(w, httpx.BadRequest("metrics source is not prometheus — set it via PUT /metrics-config first"))
		return
	}
	p, err := metrics.NewPrometheus(es.PrometheusMetricsConfig())
	if err != nil {
		httpx.WriteError(w, httpx.BadRequest(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), metricsTestTimeout)
	defer cancel()

	if err := p.Ping(ctx); err != nil {
		httpx.OK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	warnings := []string{}
	recent, err := p.CountTrafficSeries(ctx, time.Time{}, time.Hour)
	if err != nil {
		httpx.OK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if recent == 0 {
		warnings = append(warnings, "no traffic series found in the last hour — check the schema preset and that the exporter/publisher feeds this endpoint")
	}
	// At the very start of a month the two probes coincide; still cheap, still correct.
	atMonthStart, err := p.CountTrafficSeries(ctx, monthStart, time.Hour)
	if err != nil {
		httpx.OK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if atMonthStart == 0 && recent > 0 {
		warnings = append(warnings, "no data at the start of the current month — month-to-date usage will undercount until a full month of data exists (check retention)")
	}
	httpx.OK(w, map[string]any{
		"ok":               true,
		"trafficSeries":    recent,
		"monthStartSeries": atMonthStart,
		"warnings":         warnings,
	})
}
