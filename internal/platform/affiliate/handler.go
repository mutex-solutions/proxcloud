package affiliate

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// affiliateAmount is the fixed credit amount reported for a known cfy.
const affiliateAmount = 10

type Handler struct {
	repo     *Repo
	projects *project.Service
	orgs     *org.Service
	billing  *billing.Repo
	users    *user.Repo
}

func NewHandler(repo *Repo, projects *project.Service, orgs *org.Service, billingRepo *billing.Repo, users *user.Repo) *Handler {
	return &Handler{repo: repo, projects: projects, orgs: orgs, billing: billingRepo, users: users}
}

func (h *Handler) Routes(r chi.Router) {
	r.Get("/affiliate/check", h.check)
	r.Get("/affiliate/project/{id}/config", h.config)
	r.Get("/affiliate/project/{id}/log", h.log)
}

func fail(w http.ResponseWriter, err error) {
	if !httpx.WriteError(w, err) {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, "internal.error")
	}
}

// check reports a fixed affiliate amount when cfy matches a user id OR a billing-profile
// id: 200 {"amount":10} or a bare 404 (empty body). No User — authenticated only.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	cfy := r.URL.Query().Get("cfy")
	ok, err := h.exists(r.Context(), cfy)
	if err != nil {
		fail(w, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	httpx.Raw(w, http.StatusOK, map[string]any{"amount": affiliateAmount})
}

func (h *Handler) exists(ctx context.Context, cfy string) (bool, error) {
	if cfy == "" {
		return false, nil
	}
	if u, err := h.users.ExistsByID(ctx, cfy); err != nil || u {
		return u, err
	}
	return h.billing.Exists(ctx, cfy)
}

// config returns the affiliate config for a project: {cfyAff, percent}.
// cfyAff = the project's resolved billing-profile id; percent is the fixed "20".
func (h *Handler) config(w http.ResponseWriter, r *http.Request) {
	bp, ok := h.resolveProjectBillingProfile(w, r)
	if !ok {
		return
	}
	httpx.Raw(w, http.StatusOK, map[string]any{"cfyAff": bp, "percent": "20"})
}

// log returns a project's affiliate entries, newest first — a bare list, empty under the
// current seed.
func (h *Handler) log(w http.ResponseWriter, r *http.Request) {
	bp, ok := h.resolveProjectBillingProfile(w, r)
	if !ok {
		return
	}
	entries, err := h.repo.EntriesByBillingProfile(r.Context(), bp)
	if err != nil {
		fail(w, err)
		return
	}
	httpx.Raw(w, http.StatusOK, entries)
}

// resolveProjectBillingProfile is the shared head of config/log: resolve the
// caller (400 if uninitialized), load the member-scoped project (404 otherwise), then its
// billing-profile id (the project's own, else the org's).
func (h *Handler) resolveProjectBillingProfile(w http.ResponseWriter, r *http.Request) (string, bool) {
	u, err := h.users.Require(r.Context(), httpx.RC(r.Context()).Sub)
	if err != nil {
		fail(w, err)
		return "", false
	}
	p, err := h.projects.GetProject(r.Context(), u.Sub, chi.URLParam(r, "id"))
	if err != nil {
		fail(w, err)
		return "", false
	}
	bp := p.BillingProfileID
	if bp == "" {
		o, err := h.orgs.GetOrganization(r.Context(), p.OrganizationID)
		if err != nil {
			fail(w, err)
			return "", false
		}
		bp = o.BillingProfileID
	}
	return bp, true
}
