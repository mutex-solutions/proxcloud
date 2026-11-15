package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/config"
	"github.com/mutex-solutions/proxcloud/internal/health"
	"github.com/mutex-solutions/proxcloud/internal/server"
)

// The /debug/* job triggers have no authentication, so the management router mounts them only with
// debug triggers on. The scheduler, which every production install runs, must not mount them; health
// and readiness stay either way.
func TestDebugTriggersMountOnlyWithDebugFlag(t *testing.T) {
	names := []string{"run-sync", "run-charge", "run-project-deletion", "send-test-mail", "jobs-status", "gen-hmac-key"}
	for _, tc := range []struct {
		name             string
		scheduler, debug bool
		want             int
	}{
		{"scheduler on, triggers off", true, false, http.StatusNotFound},
		{"both off", false, false, http.StatusNotFound},
		{"triggers on", false, true, http.StatusNoContent},
		{"both on", true, true, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg config.Config
			cfg.Jobs.SchedulerEnabled, cfg.Jobs.DebugTriggers = tc.scheduler, tc.debug
			built := false
			triggers := debugTriggers(&cfg, slog.New(slog.DiscardHandler), func() map[string]http.HandlerFunc {
				built = true
				m := map[string]http.HandlerFunc{}
				for _, n := range names {
					m[n] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
				}
				return m
			})
			if built != tc.debug {
				t.Errorf("triggers built = %v, want %v", built, tc.debug)
			}
			mux := server.MgmtRouter(health.New(), triggers, nil)
			for _, n := range names {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/"+n, nil))
				if rec.Code != tc.want {
					t.Errorf("POST /debug/%s = %d, want %d", n, rec.Code, tc.want)
				}
			}
			for _, p := range []string{"/actuator/health", "/actuator/health/readiness"} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("GET %s = %d, want 200", p, rec.Code)
				}
			}
		})
	}
}
