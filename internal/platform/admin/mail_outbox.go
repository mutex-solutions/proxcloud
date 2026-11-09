package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type mailOutboxReader interface {
	Overview(context.Context) (mailoutbox.Overview, error)
	GetStatus(context.Context, string) (*mailoutbox.Status, error)
}

// SetMailOutbox wires ONLY primary opaque metadata reads; no resend/edit/replay.
func (h *Handler) SetMailOutbox(s *mailoutbox.Store) { h.mailOutbox = s }
func (h *Handler) mailOutboxOverview(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:bill:read") {
		return
	}
	if h.mailOutbox == nil {
		httpx.Err(w, 503, 503, "Mail outbox status unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	v, err := h.mailOutbox.Overview(ctx)
	if err != nil {
		httpx.Err(w, 503, 503, "Mail outbox status unavailable")
		return
	}
	httpx.OK(w, v)
}
func (h *Handler) mailOutboxStatus(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:bill:read") {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		httpx.Err(w, 400, 400, "Invalid outbox ID")
		return
	}
	if h.mailOutbox == nil {
		httpx.Err(w, 503, 503, "Mail outbox status unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	v, err := h.mailOutbox.GetStatus(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Err(w, 404, 404, "Mail outbox record not found")
		return
	}
	if err != nil {
		httpx.Err(w, 503, 503, "Mail outbox status unavailable")
		return
	}
	httpx.OK(w, v)
}
