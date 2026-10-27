package sse

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Handler serves the SSE stream endpoint. GET /api/v1/events/{projectId}
// opens a text/event-stream; events for that project (or the authed user) are pushed as they arrive,
// with a periodic heartbeat keepalive.
//
// AUTH NOTE: the intended design authenticates via a short-lived `sse` access token passed as
// ?token= (because the browser EventSource API can't set an Authorization header). That token
// subsystem is not yet built, so for now the endpoint is RS-bearer-authed like the rest of
// /api/v1 (the subscriber's userId = the token sub). Swapping to the sse-token is a later task.
type Handler struct {
	pool      *Pool
	heartbeat time.Duration
	// member reports whether userID may subscribe to projectID's stream. nil = allow (until the
	// membership checker is wired) — SET it to enforce project-scoped subscription.
	member func(userID, projectID string) bool
}

func NewHandler(pool *Pool) *Handler { return &Handler{pool: pool, heartbeat: 15 * time.Second} }

// SetMembership wires the project-membership check gating who may subscribe to a project's stream.
func (h *Handler) SetMembership(fn func(userID, projectID string) bool) { h.member = fn }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/events/{projectId}", h.stream)
}

func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	projectID := chi.URLParam(r, "projectId")
	rc := httpx.RC(r.Context())
	if rc == nil || rc.Sub == "" {
		// No authenticated principal → no stream (the path is bearer-enforced, not public).
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	userID := rc.Sub
	// Enforce project membership before subscribing: a user must not receive another project's
	// events. (nil checker = allow, until the membership checker is wired.)
	if h.member != nil && !h.member(userID, projectID) {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	sub := h.pool.add(projectID, userID)
	defer h.pool.remove(sub.id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(h.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte("event: heartbeat\ndata: {}\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case d, open := <-sub.ch:
			if !open {
				return
			}
			payload, _ := json.Marshal(d)
			if _, err := w.Write([]byte("event: " + d.Type + "\ndata: " + string(payload) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
