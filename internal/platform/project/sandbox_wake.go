package project

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// sandboxWake: POST /internal/sandboxes/{sandboxID}/wake {port, envdAccessToken, trafficAccessToken}, called by
// sandbox-proxy when traffic reaches a sandbox that has no pod (E2B autoResume). pkg/auth lets the path through without
// a user credential: the bearer must be the proxyWakeToken of a sandbox provider, checked before anything about the
// sandbox is looked up, and then the token of the paused sandbox's own provider. The proxy reads 200 as "resumed, look
// once more", 404 as "not resumable", 403 as a refusal to pass on and anything else as "later" (503 + Retry-After).
// The resume itself is wakePaused (e2b_snapshots.go).
func (h *Handler) sandboxWake(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" || !h.anyWakeToken(r.Context(), bearer) {
		httpx.Err(w, http.StatusUnauthorized, http.StatusUnauthorized, "invalid wake token")
		return
	}
	id := chi.URLParam(r, "sandboxID")
	if !sandbox.ValidID(id) {
		httpx.Err(w, http.StatusNotFound, http.StatusNotFound, "sandbox not found")
		return
	}
	var b struct {
		Port               int    `json:"port"`
		EnvdAccessToken    string `json:"envdAccessToken"`
		TrafficAccessToken string `json:"trafficAccessToken"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&b); err != nil {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "invalid request body")
		return
	}
	h.wakePaused(w, r, bearer, id, b.Port, b.EnvdAccessToken, b.TrafficAccessToken)
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
