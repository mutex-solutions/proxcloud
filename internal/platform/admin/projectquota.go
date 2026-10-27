package admin

// projectquota.go — the per-project quota admin surface. PUT /admin/project/{id}/quota stores
// the quota config on the project doc (same stored-JSON posture as the provider default quota
// in externalservicemut.go — no OpenStack push). Shape: {"gpu": {"<model>": n, "*": n}} where
// model names use the shared GPU alias vocabulary (see internal/cloud/gpu.go). Enforcement is
// the project cloud gate (internal/platform/project/gpuquota.go), applied on server
// create/resize.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// projectSetQuota handles PUT /project/{id}/quota: validate → store doc.quota = body →
// audit UPDATE (field-level diff) → the shaped doc. ADMIN_PROJECT_UPDATE.
func (h *Handler) projectSetQuota(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var body pgdoc.M
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	normalized, validationErr := normalizeProjectQuota(body)
	if validationErr != nil {
		httpx.WriteError(w, validationErr)
		return
	}
	body = normalized
	doc, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	before := maps.Clone(doc)
	if len(body) == 0 {
		delete(doc, "quota")
	} else {
		doc["quota"] = body
	}
	if err := h.repo.ReplaceDoc(r.Context(), projectCollection, id, doc); httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, doc)
	httpx.OK(w, shapeDoc(doc))
}

type projectGPUUsageResponse struct {
	Usage          map[string]int `json:"usage"`
	UsageAvailable bool           `json:"usageAvailable"`
}

// projectGPUUsage returns the project-global GPU usage snapshot used by the
// ProxCloud quota gate. It is intentionally an admin route: an operator viewing a
// project does not need to also be one of that project's members.
func (h *Handler) projectGPUUsage(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectReadPerm) {
		return
	}
	projectID := chi.URLParam(r, "id")
	if _, ok := h.findProjectOr404(w, r, projectID); !ok {
		return
	}
	response := projectGPUUsageResponse{Usage: map[string]int{}}
	if h.cloud == nil {
		httpx.OK(w, response)
		return
	}
	usage, err := h.cloud.GPUUsageByProject(r.Context(), projectID)
	response.Usage = usage
	response.UsageAvailable = err == nil
	if err != nil {
		slog.Warn("admin project GPU usage unavailable", "project", projectID, "err", err)
	}
	httpx.OK(w, response)
}

// projectSetGPUCapacityVisible toggles whether the client dashboard shows the region's cluster
// GPU capacity for this project (PUT /project/{id}/gpu-capacity-visible: {"gpuCapacityVisible":
// bool}). Pure datastore; the client GET /project/{id}/gpu-capacity read enforces the flag.
// ADMIN_PROJECT_UPDATE.
func (h *Handler) projectSetGPUCapacityVisible(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req struct {
		GpuCapacityVisible *bool `json:"gpuCapacityVisible"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if req.GpuCapacityVisible == nil {
		httpx.WriteError(w, httpx.BadRequest("gpuCapacityVisible is required"))
		return
	}
	existing, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	before := maps.Clone(existing)
	if _, err := h.repo.SetFields(r.Context(), projectCollection, id,
		pgdoc.M{"gpuCapacityVisible": *req.GpuCapacityVisible}); httpx.WriteError(w, err) {
		return
	}
	after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(after))
}

// validateProjectQuota checks the quota shape: quota.gpu (when present) must be an object of
// non-negative integer limits keyed by GPU model alias (or "*").
func validateProjectQuota(body pgdoc.M) *httpx.HTTPError {
	_, err := normalizeProjectQuota(body)
	return err
}

// normalizeProjectQuota validates the quota document and canonicalizes model
// aliases at the write boundary so admin/API/MCP callers all match flavor aliases.
func normalizeProjectQuota(body pgdoc.M) (pgdoc.M, *httpx.HTTPError) {
	normalizedBody := maps.Clone(body)
	gpuRaw, ok := body["gpu"]
	if !ok {
		return normalizedBody, nil
	}
	gpu, ok := gpuRaw.(map[string]any)
	if !ok {
		return nil, httpx.BadRequest("quota.gpu must be an object of {model: limit}")
	}
	normalizedGPU := make(map[string]any, len(gpu))
	sources := make(map[string]string, len(gpu))
	for k, v := range gpu {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			return nil, httpx.BadRequest("quota.gpu model must not be empty")
		}
		canonical := cloud.NormalizeGPUAlias(trimmed)
		if trimmed == "*" {
			canonical = "*"
		}
		if previous, exists := sources[canonical]; exists {
			return nil, httpx.BadRequest(fmt.Sprintf(
				"quota.gpu models %q and %q resolve to the same alias %q", previous, k, canonical))
		}
		f, ok := v.(float64) // JSON numbers decode to float64
		if !ok || f < 0 || f != float64(int64(f)) {
			return nil, httpx.BadRequest(fmt.Sprintf("quota.gpu[%s] must be a non-negative integer", k))
		}
		normalizedGPU[canonical] = v
		sources[canonical] = k
	}
	normalizedBody["gpu"] = normalizedGPU
	return normalizedBody, nil
}
