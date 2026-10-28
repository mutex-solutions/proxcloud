// Package job serves /api/v1/admin/job/* — the operator job triggers. The path is whitelisted
// (no bearer; an internal scheduler/sidecar drives it). Each endpoint with an in-process job
// runs it directly; the gated / per-id-rabbit / not-wired ones return 202 Accepted (they are
// fanned out to RabbitMQ consumers we don't run here).
package job

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Runners are the in-process job entry points (wired from main.go's job objects — the same ones the
// mgmt-port /debug/run-* triggers use). Any nil runner degrades its endpoint to a 202 no-op.
type Runners struct {
	Charge              func(ctx context.Context, timeUnit string) error
	Metrics             func(ctx context.Context) error
	ServicesSync        func(ctx context.Context) error
	Collect             func(ctx context.Context) error
	SavingsExpire       func(ctx context.Context) error
	ReminderSchedule    func(ctx context.Context) error // savings expiry-reminder scheduler (daily)
	ReminderDispatch    func(ctx context.Context) error // reminder-notification dispatcher (hourly)
	TransactionScan     func(ctx context.Context) error // stuck-PENDING payment-transaction reconciler
	CloudResourceExists func(ctx context.Context, serviceID, externalID string) (bool, error)
}

type Handler struct{ r Runners }

func NewHandler(r Runners) *Handler { return &Handler{r: r} }

func (h *Handler) Routes(router chi.Router) {
	// Ported, real runners.
	router.Post("/admin/job/billing/rate", h.charge)
	router.Post("/admin/job/usage/metrics", h.run(h.r.Metrics))
	router.Post("/admin/job/external-service/sync", h.run(h.r.ServicesSync))
	router.Post("/admin/job/collect", h.run(h.r.Collect))
	router.Post("/admin/job/savings-contracts/expiry-check", h.run(h.r.SavingsExpire))
	// Savings expiry reminders: reminder-check SCHEDULES,
	// notifications/reminder DISPATCHES. Nil runners degrade to 202.
	router.Post("/admin/job/savings-contracts/reminder-check", h.run(h.r.ReminderSchedule))
	router.Post("/admin/job/notifications/reminder", h.run(h.r.ReminderDispatch))
	router.Post("/admin/job/transactions/sync", h.run(h.r.TransactionScan))
	router.Get("/admin/job/cloud-resource", h.cloudResource)
	// Gated / per-id rabbit fan-out / not-wired subsystems → 202 Accepted (no-op). These are
	// published to RabbitMQ consumers (send-bill, per-id collect, contacts/segments=CRM,
	// kubernetes sync, reminders=notification, bills-recover) which the single-pod build
	// does not run; the trigger still acknowledges.
	for _, p := range []string{
		"/admin/job/billing/bills-recover",
		"/admin/job/send-bill",
		"/admin/job/send-bill/{id}",
		"/admin/job/collect/{id}",
		"/admin/job/external-service/sync/{type}",
		"/admin/job/user/sync",
		"/admin/job/user/contacts/sync",
		"/admin/job/user/segments/sync",
		"/admin/job/kubernetes/cluster/sync",
	} {
		router.Post(p, h.accepted)
	}
}

// charge serves /billing/rate?timeUnit= → the charge job for the cadence (minutely/hourly/
// monthly). Invalid timeUnit → 400.
func (h *Handler) charge(w http.ResponseWriter, r *http.Request) {
	tu := r.URL.Query().Get("timeUnit")
	switch tu {
	case "minute", "hour", "month":
	default:
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "Invalid time unit")
		return
	}
	if h.r.Charge == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := h.r.Charge(r.Context(), tu); err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// cloudResource serves GET /cloud-resource?serviceId=&externalId= → 200 if the cached resource
// exists, else 404.
func (h *Handler) cloudResource(w http.ResponseWriter, r *http.Request) {
	if h.r.CloudResourceExists == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ok, err := h.r.CloudResourceExists(r.Context(), r.URL.Query().Get("serviceId"), r.URL.Query().Get("externalId"))
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// run wraps a nil-safe job runner → 202 on success.
func (h *Handler) run(fn func(context.Context) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fn == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if err := fn(r.Context()); err != nil {
			httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *Handler) accepted(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
}
