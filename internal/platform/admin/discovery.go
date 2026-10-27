package admin

// discovery.go — STUB. Keystone catalog discovery died with the OpenStack
// backend: a Proxmox provider's regions/services are written directly by the
// create form (no discovery round-trip, the ceph precedent). The route stays so
// the admin FE gets an honest 501 instead of a 404.

import (
	"context"
	"net/http"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func (h *Handler) externalServiceDiscover(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, "admin:service:manage") {
		return
	}
	httpx.Err(w, http.StatusNotImplemented, http.StatusNotImplemented,
		"Service discovery is not available on this fork — providers declare regions/services directly")
}

// enrichNewServiceFromCloud was the keystone autofill on create — a no-op now.
func (h *Handler) enrichNewServiceFromCloud(ctx context.Context, doc pgdoc.M, id string) bool {
	return false
}
