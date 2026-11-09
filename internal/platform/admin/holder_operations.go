package admin

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) holderOperationService(w http.ResponseWriter, r *http.Request, permission string) *pve.Service {
	if !h.require(w, r, permission) {
		return nil
	}
	es, ok := h.loadServiceOr(w, r, chi.URLParam(r, "id"))
	if !ok {
		return nil
	}
	if !es.IsProxmox() {
		httpx.WriteError(w, httpx.BadRequest("Not a Proxmox provider"))
		return nil
	}
	if h.pveFor == nil {
		httpx.WriteError(w, httpx.NewError(503, 503, "Holder journal unavailable"))
		return nil
	}
	s, err := h.pveFor(es)
	if err != nil || s.HolderOperations == nil {
		httpx.WriteError(w, httpx.NewError(503, 503, "Holder journal unavailable"))
		return nil
	}
	return s
}

// Status reads only the journal. There is deliberately no force-clear route.
func (h *Handler) holderOperationStatus(w http.ResponseWriter, r *http.Request) {
	s := h.holderOperationService(w, r, "admin:service:read")
	if s == nil {
		return
	}
	op, err := s.HolderOperationStatus(r.Context(), chi.URLParam(r, "project"))
	if err != nil {
		httpx.WriteError(w, httpx.NewError(503, 503, "Holder status unavailable"))
		return
	}
	httpx.OK(w, map[string]any{"pending": op, "operatorHold": op != nil && op.UPID == ""})
}

// Poll requires the operation ID returned by a preceding status read. This is
// not clearance authority: only the service's stopped+identity/effect proof settles.
func (h *Handler) holderOperationPoll(w http.ResponseWriter, r *http.Request) {
	s := h.holderOperationService(w, r, "admin:service:manage")
	if s == nil {
		return
	}
	id := r.URL.Query().Get("operationId")
	if id == "" {
		httpx.WriteError(w, httpx.BadRequest("Read status first and supply operationId"))
		return
	}
	err := s.PollHolderOperation(r.Context(), chi.URLParam(r, "project"), id)
	if err != nil {
		var conflict *pve.ConflictError
		if errors.As(err, &conflict) {
			httpx.WriteError(w, httpx.NewError(409, 409, conflict.Error()))
		} else {
			httpx.WriteError(w, httpx.NewError(503, 503, "Holder observation did not settle; read status"))
		}
		return
	}
	httpx.OK(w, map[string]any{"observed": id, "dispatched": false})
}
