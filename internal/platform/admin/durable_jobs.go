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
	kind, err := h.durableJobKind(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if h.require(w, r, "admin:bill:manage") {
			httpx.Err(w, 503, 503, "Durable job replay failed")
		}
		return
	}
	external := durablejobs.External(kind)
	if external {
		perm = templateUpdatesManagePerm
	}
	if !h.require(w, r, perm) {
		return
	}
	// A dead bulk attach is not replayed: its "Retry failed projects" starts a new run over what it did not finish, and
	// one-run-per-provider is enforced there (a replay next to a newer run would make two).
	if kind == durablejobs.ProviderAttach {
		httpx.Err(w, 409, 409, "A bulk attach is not replayed: use Retry failed projects on the provider's Projects tab")
		return
	}
	// A smoke test is never replayed: its result is per run (a new one is queued from the page).
	if kind == durablejobs.TemplateUpdateSmoke {
		httpx.Err(w, 409, 409, "A smoke test is not replayed: run a new smoke test from Template updates")
		return
	}
	// A failed/finished template update is a no-op on replay: refuse clearly instead.
	if kind == durablejobs.TemplateUpdateVM && h.templateUpdates != nil {
		rec, err := h.templateUpdates.Receipts.Load(r.Context(), chi.URLParam(r, "id"))
		if err != nil {
			httpx.Err(w, 503, 503, "Durable job replay failed")
			return
		}
		if err := templateupdates.ReplayableReceipt(rec); err != nil {
			httpx.Err(w, 409, 409, err.Error())
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
	kind, err := h.durableJobKind(ctx, id)
	return durablejobs.External(kind), err
}

// durableJobKind is the kind of job id ("" when there is no such job or the id is malformed).
func (h *Handler) durableJobKind(ctx context.Context, id string) (string, error) {
	if h.durableStore == nil {
		return "", nil
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", nil
	}
	j, err := h.durableStore.Get(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil || j == nil {
		return "", err
	}
	return j.Kind, nil
}
