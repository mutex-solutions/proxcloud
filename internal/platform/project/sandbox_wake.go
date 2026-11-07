package project

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// sandboxWake: POST /internal/sandboxes/{sandboxID}/wake, called by sandbox-proxy when traffic reaches a sandbox that
// has no pod (E2B autoResume). pkg/auth lets the path through without a user credential: the bearer must be the proxyWakeToken of a sandbox provider, checked before anything
// about the sandbox is looked up. The proxy reads 200 as "resumed, look again", 404 as "not resumable" and anything
// else as a refusal to pass on.
//
// Nothing is paused before pause and resume land (SB7), so every sandbox answers 404 after the token check and the
// proxy answers the client 502 "not found" itself, as it does with wake off.
func (h *Handler) sandboxWake(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" || !h.anyWakeToken(r.Context(), bearer) {
		httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "invalid wake token")
		return
	}
	if !sandbox.ValidID(chi.URLParam(r, "sandboxID")) {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "sandbox not found")
		return
	}
	// ponytail: no paused sandboxes exist yet; SB7 looks the paused one up here, checks the client's tokens against
	// it, passes the suspension gate and resumes it.
	httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "sandbox not found")
}

// anyWakeToken reports bearer as the proxyWakeToken of some sandbox provider. Every provider is compared, so the time
// taken does not say which one matched.
func (h *Handler) anyWakeToken(ctx context.Context, bearer string) bool {
	if h.esSvc == nil {
		return false
	}
	es, err := h.esSvc.List(ctx)
	if err != nil {
		return false
	}
	found := 0
	for i := range es {
		if !es[i].IsSandbox() {
			continue
		}
		want := es[i].SandboxConfig().ProxyWakeToken
		if want != "" {
			found |= subtle.ConstantTimeCompare([]byte(bearer), []byte(want))
		}
	}
	return found == 1
}
