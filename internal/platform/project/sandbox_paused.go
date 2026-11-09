package project

// Native recovery addresses sandbox IDs, not ephemeral billing-row IDs. It also
// works when envd is off; the E2B API deliberately does not. Tokens and saved CRs
// (which contain environment values) are never returned by the list.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) nativePausedCaller(w http.ResponseWriter, r *http.Request) (*e2bCaller, *pausedSandbox) {
	u, ok := h.principal(w, r)
	if !ok {
		return nil, nil
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return nil, nil
	}
	p := h.loadPaused(r.Context(), proj.ID, chi.URLParam(r, "sandboxID"))
	if p == nil || !proj.HasService(p.ServiceID) {
		h.fail(w, httpx.NotFound("Paused sandbox not found"))
		return nil, nil
	}
	es, err := h.esSvc.Get(r.Context(), p.ServiceID)
	if err != nil || es == nil || !es.IsSandbox() {
		h.fail(w, httpx.NotFound("Paused sandbox not found"))
		return nil, nil
	}
	ss, ok := h.sandboxService(w, es)
	if !ok {
		return nil, nil
	}
	return &e2bCaller{user: u, proj: proj, es: es, ss: ss}, p
}

func (h *Handler) nativePausedList(w http.ResponseWriter, r *http.Request) {
	u, ok := h.principal(w, r)
	if !ok {
		return
	}
	proj, ok := h.project(w, r, u, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	out := []map[string]any{}
	for _, p := range h.projectPaused(r.Context(), proj.ID) {
		if !proj.HasService(p.ServiceID) {
			continue
		}
		state := p.State
		if state == "" {
			state = "paused"
		}
		out = append(out, map[string]any{"sandboxId": p.ID, "serviceId": p.ServiceID, "state": state, "pausedAt": p.PausedAt})
	}
	httpx.OK(w, out)
}

func (h *Handler) nativePausedResume(w http.ResponseWriter, r *http.Request) {
	c, p := h.nativePausedCaller(w, r)
	if c == nil {
		return
	}
	var b struct {
		TimeoutSeconds int `json:"timeoutSeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		h.fail(w, httpx.BadRequest("invalid request body"))
		return
	}
	if b.TimeoutSeconds == 0 {
		b.TimeoutSeconds = 300
	}
	if _, err := h.resumePaused(r.Context(), c, p, b.TimeoutSeconds); err != nil {
		var status e2bStatusErr
		if errors.As(err, &status) {
			httpx.Err(w, status.code, status.code, status.msg)
		} else {
			h.sandboxFail(w, err)
		}
		return
	}
	row := h.e2bRow(r.Context(), c, p.ID)
	if row == nil {
		h.fail(w, httpx.NotFound("Resumed sandbox not found"))
		return
	}
	h.cloudResourceAudit(c.user, c.proj, "CLOUD_RESOURCE_ACTION", "RESUME", row)
	httpx.OK(w, map[string]any{"sandboxId": p.ID, "resourceId": row.ID})
}

func (h *Handler) nativePausedDelete(w http.ResponseWriter, r *http.Request) {
	c, _ := h.nativePausedCaller(w, r)
	if c == nil {
		return
	}
	// Reuse the existing paused/pausing/resuming kill races and billing cleanup.
	h.e2bKill(w, r.WithContext(context.WithValue(r.Context(), e2bCtxKey{}, c)))
}
