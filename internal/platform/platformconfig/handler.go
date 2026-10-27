package platformconfig

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Handler serves the client-facing platform-configuration endpoint.
type Handler struct {
	repo         *Repo
	authStrategy string // REMOTE_OIDC / LOCAL_IDP, derived from config
	adminURL     string // for the no-config onboarding redirect
}

func NewHandler(repo *Repo, authStrategy, adminURL string) *Handler {
	return &Handler{repo: repo, authStrategy: authStrategy, adminURL: adminURL}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/platform-configuration/default", h.getDefault)
}

// getDefault returns the default platform configuration:
// return the default config DTO, or (when none is configured) a redirect to the
// admin onboarding flow. regions are included only for authenticated callers.
func (h *Handler) getDefault(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.repo.FindDefault(r.Context())
	if err != nil {
		httpx.Err(w, http.StatusInternalServerError, 500, "internal.error")
		return
	}
	if cfg == nil {
		httpx.Redirect(w, h.adminURL+"/onboarding")
		return
	}
	// This is a public path; an authenticated caller carries a non-empty Sub.
	authenticated := httpx.RC(r.Context()).Sub != ""
	httpx.OK(w, toDto(cfg, authenticated, h.authStrategy))
}
