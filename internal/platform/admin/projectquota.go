package admin

// projectquota.go — the per-project quota admin surface. PUT /admin/project/{id}/quota stores
// the quota config on the project doc (same stored-JSON posture as the provider default quota
// in externalservicemut.go — no provider push). Shape: {"gpu": {"<model>": n, "*": n}} where
// model names use the shared GPU alias vocabulary (see internal/cloud/gpu.go), plus the Proxmox
// resource keys (externalservice.PVEQuotaKeys: servers, vcpus, ramMb, diskGb, networks,
// floatingIps), and the sandbox keys (externalservice.SandboxQuotaKeys: sandboxes, sandboxVcpus,
// sandboxRamMb, sandboxGpus; an absent one falls back to the provider's, then the built-in default).
// Enforcement is the project cloud gate (project/gpuquota.go, project/cloud_quota_pve.go,
// project/cloud_sandbox.go). An empty body clears the quota; a Proxmox project then falls
// back to its provider's default quota.
//
// PUT /admin/project/{id}/function-quota stores the project's function quota in a document of its own,
// doc.functionQuota = {functions, vcpus, ramMb} (an absent key falls back to the provider's
// functions.projectQuota, then the built-in default; an empty body clears it). It is NOT part of doc.quota,
// and the quota route refuses its keys: any key of doc.quota other than the sandbox ones makes that document a
// Proxmox quota, in which every absent key is unlimited.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
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
	// Only the quota: a replace of the whole doc from this read would put back a status changed since.
	var err error
	if len(body) == 0 {
		_, err = h.repo.SetAndUnsetFields(r.Context(), projectCollection, id, nil, pgdoc.M{"quota": nil})
	} else {
		_, err = h.repo.SetFields(r.Context(), projectCollection, id, pgdoc.M{"quota": body})
	}
	if httpx.WriteError(w, err) {
		return
	}
	after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(after))
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
	for _, key := range functionQuotaStrayKeys {
		if _, ok := body[key]; ok {
			return nil, httpx.BadRequest(fmt.Sprintf("quota.%s is not a key of the project quota: function limits are set with PUT /project/{id}/function-quota (functions, vcpus, ramMb)", key))
		}
	}
	for _, key := range append(slices.Clone(externalservice.PVEQuotaKeys), externalservice.SandboxQuotaKeys...) {
		v, ok := body[key]
		if !ok {
			continue
		}
		if f, ok := v.(float64); !ok || f < 0 || f != float64(int64(f)) {
			return nil, httpx.BadRequest(fmt.Sprintf("quota.%s must be a non-negative integer", key))
		}
	}
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

// FunctionQuotaKeys are the keys of a project's function quota document (doc.functionQuota).
var FunctionQuotaKeys = []string{"functions", "vcpus", "ramMb"}

// functionQuotaStrayKeys are what a function limit could be mistaken for in the shared quota document. One of them
// there would limit no function and, on a project that held no Proxmox key, lift every Proxmox limit.
var functionQuotaStrayKeys = []string{"functions", "functionVcpus", "functionRamMb", "functionQuota"}

// normalizeFunctionQuota checks a function quota body: only FunctionQuotaKeys, each a non-negative integer.
func normalizeFunctionQuota(body pgdoc.M) (pgdoc.M, *httpx.HTTPError) {
	out := pgdoc.M{}
	for key, v := range body {
		if !slices.Contains(FunctionQuotaKeys, key) {
			return nil, httpx.BadRequest(fmt.Sprintf("functionQuota.%s is not a function quota key (%s)", key, strings.Join(FunctionQuotaKeys, ", ")))
		}
		if f, ok := v.(float64); !ok || f < 0 || f != float64(int64(f)) {
			return nil, httpx.BadRequest(fmt.Sprintf("functionQuota.%s must be a non-negative integer", key))
		}
		out[key] = v
	}
	return out, nil
}

// projectSetFunctionQuota handles PUT /project/{id}/function-quota: validate, store doc.functionQuota = body (an
// empty body removes it), audit the change, answer the shaped doc. It writes that one field: the project's quota
// document is not read and not written. ADMIN_PROJECT_UPDATE.
func (h *Handler) projectSetFunctionQuota(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, projectUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var body pgdoc.M
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	body, validationErr := normalizeFunctionQuota(body)
	if validationErr != nil {
		httpx.WriteError(w, validationErr)
		return
	}
	doc, ok := h.findProjectOr404(w, r, id)
	if !ok {
		return
	}
	before := maps.Clone(doc)
	var err error
	if len(body) == 0 {
		_, err = h.repo.SetAndUnsetFields(r.Context(), projectCollection, id, nil, pgdoc.M{"functionQuota": nil})
	} else {
		_, err = h.repo.SetFields(r.Context(), projectCollection, id, pgdoc.M{"functionQuota": body})
	}
	if httpx.WriteError(w, err) {
		return
	}
	after, err := h.repo.FindDoc(r.Context(), projectCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(after))
}
