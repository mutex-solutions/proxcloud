// Package health serves Actuator-compatible health endpoints on the
// management port so the existing chart probes pass unchanged:
//
//	liveness  GET /actuator/health
//	readiness GET /actuator/health/readiness
package health

import (
	"context"
	"encoding/json"
	"net/http"
)

// Checker reports a named dependency's health (nil error = UP).
type Checker struct {
	Name  string
	Check func(ctx context.Context) error
}

type Handler struct {
	readiness []Checker
}

func New(readiness ...Checker) *Handler {
	return &Handler{readiness: readiness}
}

// Liveness: the process is up. Emits Actuator's {"status":"UP"}.
func (h *Handler) Liveness(w http.ResponseWriter, r *http.Request) {
	writeStatus(w, http.StatusOK, "UP", nil)
}

// Readiness: UP only when every dependency check passes; else 503 DOWN with
// per-component detail (the chart enables show-components/show-details).
func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	comps := map[string]string{}
	status := "UP"
	for _, c := range h.readiness {
		if err := c.Check(r.Context()); err != nil {
			status = "DOWN"
			comps[c.Name] = "DOWN"
		} else {
			comps[c.Name] = "UP"
		}
	}
	code := http.StatusOK
	if status == "DOWN" {
		code = http.StatusServiceUnavailable
	}
	writeStatus(w, code, status, comps)
}

func writeStatus(w http.ResponseWriter, code int, status string, components map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body := map[string]any{"status": status}
	if len(components) > 0 {
		comps := map[string]any{}
		for k, v := range components {
			comps[k] = map[string]string{"status": v}
		}
		body["components"] = comps
	}
	_ = json.NewEncoder(w).Encode(body)
}
