// Package feature serves the client feature-flag endpoints:
// GET /api/v1/features (the available feature set) and GET /api/v1/features/{feature}
// (whether one feature is enabled). Both are authenticated (under /api/v1/**) but take
// no User — there is no User-init requirement.
//
// Open-source build: there is no license. All features are available; the set below is
// the canonical list the UI/clients query.
package feature

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Features is the available feature set (no license gating in the open-source build).
var Features = []string{"billing", "search", "mailchimp"}

// IsEnabled reports whether a feature is in the available set.
func IsEnabled(f string) bool {
	for _, x := range Features {
		if x == f {
			return true
		}
	}
	return false
}

type Handler struct{}

func NewHandler() *Handler { return &Handler{} }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/features", h.list)
	r.Get("/features/{feature}", h.check)
}

// list returns the available feature set as {data:[...]}.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	out := make([]string, len(Features))
	copy(out, Features)
	httpx.OK(w, out)
}

// check returns whether one feature is enabled as {data:bool}.
func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, IsEnabled(chi.URLParam(r, "feature")))
}
