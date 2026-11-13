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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/templateupdates"
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
	if h.durableStore == nil {
		if h.require(w, r, "admin:bill:manage") {
			httpx.Err(w, 503, 503, "Durable jobs are not configured")
		}
		return
	}
	// The permission follows the job's kind: a template update (Proxmox effects) needs
	// admin:service:manage, not the billing key. Only a definite "no such job" falls back to the
	// billing key; a lookup error fails closed so a billing admin can never replay an external job.
	perm := "admin:bill:manage"
	external, err := h.adminRequiresServiceManage(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if h.require(w, r, "admin:bill:manage") {
			httpx.Err(w, 503, 503, "Durable job replay failed")
		}
		return
	}
	if external {
		perm = templateUpdatesManagePerm
	}
	if !h.require(w, r, perm) {
		return
	}
	// A failed/finished template update is a no-op on replay: refuse clearly instead.
	if external && h.templateUpdates != nil {
		id := chi.URLParam(r, "id")
		var rerr error
		if j, err := h.durableStore.Get(r.Context(), id); err != nil {
			httpx.Err(w, 503, 503, "Durable job replay failed")
			return
		} else if j.Kind == durablejobs.TemplateUpdateSandbox {
			sbx, err := h.templateUpdates.Sandbox.Load(r.Context(), id)
			if err != nil {
				httpx.Err(w, 503, 503, "Durable job replay failed")
				return
			}
			rerr = templateupdates.ReplayableSandboxReceipt(sbx)
		} else {
			rec, err := h.templateUpdates.Receipts.Load(r.Context(), id)
			if err != nil {
				httpx.Err(w, 503, 503, "Durable job replay failed")
				return
			}
			rerr = templateupdates.ReplayableReceipt(rec)
		}
		if rerr != nil {
			httpx.Err(w, 409, 409, rerr.Error())
			return
		}
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
	err = h.durableStore.Replay(r.Context(), chi.URLParam(r, "id"), actor, req.Reason, h.durableValidate)
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

// adminRequiresServiceManage reports whether replaying job id needs admin:service:manage (an
// external-effect kind such as a template update) instead of the billing key. A missing job
// (or a malformed id, which Replay refuses anyway) is (false, nil); any other lookup error is
// returned so the caller fails closed.
func (h *Handler) adminRequiresServiceManage(ctx context.Context, id string) (bool, error) {
	if h.durableStore == nil {
		return false, nil
	}
	if _, err := uuid.Parse(id); err != nil {
		return false, nil
	}
	j, err := h.durableStore.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return j != nil && durablejobs.External(j.Kind), nil
}
