package job

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The admin job endpoints are how an operator runs a job on demand once the /debug/* triggers are
// off, so each one must reach its runner on its own, with no management-port wiring. Mounted under
// /api/v1 as the app router does.
func TestAdminJobEndpointsRunTheirJob(t *testing.T) {
	ran := map[string]string{}
	runner := func(name string) func(context.Context) error {
		return func(context.Context) error { ran[name] = "ran"; return nil }
	}
	h := NewHandler(Runners{
		Charge:           func(_ context.Context, tu string) error { ran["charge"] = tu; return nil },
		Metrics:          runner("metrics"),
		ServicesSync:     runner("sync"),
		Collect:          runner("collect"),
		SavingsExpire:    runner("savings-expire"),
		ReminderSchedule: runner("reminder-schedule"),
		ReminderDispatch: runner("reminder-dispatch"),
		TransactionScan:  runner("txn-scan"),
	})
	r := chi.NewRouter()
	r.Route("/api/v1", h.Routes)

	for _, tc := range []struct {
		path, job, want string
		code            int
	}{
		{"/api/v1/admin/job/billing/rate?timeUnit=hour", "charge", "hour", http.StatusAccepted},
		{"/api/v1/admin/job/billing/rate", "charge", "", http.StatusBadRequest}, // timeUnit is required
		{"/api/v1/admin/job/usage/metrics", "metrics", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/external-service/sync", "sync", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/collect", "collect", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/savings-contracts/expiry-check", "savings-expire", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/savings-contracts/reminder-check", "reminder-schedule", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/notifications/reminder", "reminder-dispatch", "ran", http.StatusAccepted},
		{"/api/v1/admin/job/transactions/sync", "txn-scan", "ran", http.StatusAccepted},
	} {
		clear(ran)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if rec.Code != tc.code {
			t.Errorf("POST %s = %d, want %d", tc.path, rec.Code, tc.code)
		}
		if ran[tc.job] != tc.want {
			t.Errorf("POST %s: job %s = %q, want %q", tc.path, tc.job, ran[tc.job], tc.want)
		}
	}
}
