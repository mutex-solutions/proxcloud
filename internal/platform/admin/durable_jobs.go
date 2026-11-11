package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) SetDurableJobs(s *durablejobs.Store, history *durablejobs.History, validate func(*durablejobs.Job) error) {
	h.durableStore = s
	h.durableHistory = history
	h.durableValidate = validate
}
func (h *Handler) durableStatus(w http.ResponseWriter, r *http.Request) {
	// Authorization always reads the existing PRIMARY admin repository, never history.
	if !h.require(w, r, "admin:bill:read") {
		return
	}
	if h.durableHistory == nil {
		httpx.Err(w, 503, 503, "Durable job history is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stats, err := h.durableHistory.Stats(ctx)
	if err != nil {
		httpx.Err(w, 503, 503, "Durable job history is unavailable (lag-tolerant read pool)")
		return
	}
	httpx.OK(w, stats)
}
func (h *Handler) durableReplay(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:bill:manage") {
		return
	}
	if h.durableStore == nil {
		httpx.Err(w, 503, 503, "Durable jobs are not configured")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil || d.Decode(new(any)) != io.EOF {
		httpx.WriteError(w, httpx.BadRequest("Expected only a reason"))
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	rc := httpx.RC(r.Context())
	actor := rc.Sub
	if actor == "" {
		actor = rc.SigV4KeyID
	}
	err := h.durableStore.Replay(r.Context(), chi.URLParam(r, "id"), actor, req.Reason, h.durableValidate)
	if errors.Is(err, durablejobs.ErrReplay) {
		httpx.Err(w, 409, 409, "Only a dead, validated known job can be replayed; provide an 8..500 character reason")
		return
	}
	if err != nil {
		httpx.Err(w, 503, 503, "Durable job replay failed")
		return
	}
	httpx.OK(w, map[string]any{"id": chi.URLParam(r, "id"), "state": "pending"})
}
