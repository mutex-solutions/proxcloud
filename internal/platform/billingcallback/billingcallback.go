// Package billingcallback serves the operations the billing service needs proxcloud to perform.
//
// Most of the billing split is one-directional: proxcloud resolves cloud resources and POSTs them to
// the billing service to be rated. But three of the billing engine's side effects reach back into
// things only proxcloud owns — pausing OpenStack servers, enabling projects, sending mail — and those
// move with the engine. They become callbacks in this direction.
//
// These endpoints are DANGEROUS: suspend-clouds pauses every server belonging to a billing profile.
// Two things keep them safe:
//
//   - They are mounted on the MANAGEMENT port, not the public API port, so they are not routed from
//     outside the cluster.
//   - They require a shared bearer token, and the router refuses to mount them at all when no token
//     is configured. An unauthenticated endpoint that can pause a customer's fleet is not something
//     to leave to network policy alone.
//
// The interfaces are declared locally rather than imported from internal/platform/billing on
// purpose: that package is deleted once the engine has fully moved out, and this package has to
// outlive it.
package billingcallback

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// CloudSuspender pauses or resumes every cloud resource belonging to a billing profile.
// Best-effort by contract: per-project and per-server failures are logged and skipped, because a
// cloud error must never block the billing state machine.
type CloudSuspender interface {
	SuspendBillingProfileClouds(ctx context.Context, billingProfileID string) error
	ResumeBillingProfileClouds(ctx context.Context, billingProfileID string) error
}

// ProjectActivator enables and bootstraps every non-ENABLED project of a billing profile, applying
// the owning organisation's memberships.
type ProjectActivator func(ctx context.Context, billingProfileID string) error

// Notifier sends a templated email. Satisfied by the mail service.
type Notifier interface {
	SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error
}

// Handler serves the callbacks. Any collaborator may be nil — a nil one makes its endpoint a no-op
// rather than a panic, so a partially-configured deployment degrades instead of crashing on a
// billing state transition.
type Handler struct {
	clouds   CloudSuspender
	activate ProjectActivator
	notifier Notifier
	token    string
	log      *slog.Logger
}

// New returns a handler, or nil when no token is configured. A nil handler mounts nothing — see the
// package doc for why that is fail-closed rather than inconvenient.
func New(clouds CloudSuspender, activate ProjectActivator, notifier Notifier, token string, log *slog.Logger) *Handler {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{clouds: clouds, activate: activate, notifier: notifier, token: token, log: log}
}

// Routes registers the callbacks on a ServeMux (the management router). Nil-safe.
func (h *Handler) Routes(mux *http.ServeMux) {
	if h == nil {
		return
	}
	mux.HandleFunc("POST /internal/v1/billing-profiles/{id}/suspend-clouds", h.guard(h.suspendClouds))
	mux.HandleFunc("POST /internal/v1/billing-profiles/{id}/resume-clouds", h.guard(h.resumeClouds))
	mux.HandleFunc("POST /internal/v1/billing-profiles/{id}/activate-projects", h.guard(h.activateProjects))
	mux.HandleFunc("POST /internal/v1/notify", h.guard(h.notify))
}

// guard enforces the shared bearer token in constant time.
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
			httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (h *Handler) suspendClouds(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.clouds == nil {
		h.log.Warn("billing callback: suspend-clouds with no cloud suspender wired", "profileId", id)
		httpx.OK(w, map[string]any{"applied": false, "reason": "cloud suspender not configured"})
		return
	}
	if err := h.clouds.SuspendBillingProfileClouds(r.Context(), id); err != nil {
		h.log.Error("billing callback: suspend clouds", "profileId", id, "err", err)
		httpx.Err(w, http.StatusInternalServerError, 500, "suspend.failed")
		return
	}
	httpx.OK(w, map[string]any{"applied": true})
}

func (h *Handler) resumeClouds(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.clouds == nil {
		h.log.Warn("billing callback: resume-clouds with no cloud suspender wired", "profileId", id)
		httpx.OK(w, map[string]any{"applied": false, "reason": "cloud suspender not configured"})
		return
	}
	if err := h.clouds.ResumeBillingProfileClouds(r.Context(), id); err != nil {
		h.log.Error("billing callback: resume clouds", "profileId", id, "err", err)
		httpx.Err(w, http.StatusInternalServerError, 500, "resume.failed")
		return
	}
	httpx.OK(w, map[string]any{"applied": true})
}

func (h *Handler) activateProjects(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.activate == nil {
		h.log.Warn("billing callback: activate-projects with no activator wired", "profileId", id)
		httpx.OK(w, map[string]any{"applied": false, "reason": "project activator not configured"})
		return
	}
	if err := h.activate(r.Context(), id); err != nil {
		h.log.Error("billing callback: activate projects", "profileId", id, "err", err)
		httpx.Err(w, http.StatusInternalServerError, 500, "activate.failed")
		return
	}
	httpx.OK(w, map[string]any{"applied": true})
}

// NotifyRequest is a templated email send.
type NotifyRequest struct {
	Key  string         `json:"key"`
	To   []string       `json:"to"`
	Vars map[string]any `json:"vars,omitempty"`
}

func (h *Handler) notify(w http.ResponseWriter, r *http.Request) {
	var req NotifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "invalid.body")
		return
	}
	if req.Key == "" || len(req.To) == 0 {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "key.and.to.required")
		return
	}
	if h.notifier == nil {
		httpx.OK(w, map[string]any{"sent": false, "reason": "no mail gateway configured"})
		return
	}
	// Mail is best-effort by design: a notification must never break the billing state machine, so
	// a send failure is logged and reported, not raised. The engine treats it the same way.
	if err := h.notifier.SendTemplate(r.Context(), req.Key, req.To, req.Vars); err != nil {
		h.log.Error("billing callback: notify", "key", req.Key, "err", err)
		httpx.OK(w, map[string]any{"sent": false, "reason": err.Error()})
		return
	}
	httpx.OK(w, map[string]any{"sent": true})
}
