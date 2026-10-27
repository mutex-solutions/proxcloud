package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// platformconfig_mut.go serves the platform-configuration mutations (create / update /
// delete / update-regions). Pure datastore CRUD on the platformConfiguration table via the
// id-aware crud.go helpers. All gate ADMIN_PLATFORM_CONFIG_UPDATE.

const pcfgCollection = "platformConfiguration"
const pcfgUpdatePerm = "admin:platform_config:update"

// routePlatformConfigMut registers the platform-configuration mutation routes (reads stay in
// handler.go: GET list/current/login-config/{id}).
func (h *Handler) routePlatformConfigMut(r chi.Router) {
	r.Post("/platform-configuration", h.platformConfigCreate)
	r.Put("/platform-configuration/{id}", h.platformConfigUpdate)
	r.Delete("/platform-configuration/{id}", h.platformConfigDelete)
	r.Put("/platform-configuration/{id}/regions", h.platformConfigRegions)
}

// platformConfigNotFound mirrors getConfiguration's RuntimeException → HTTP 500.
func platformConfigNotFound(id string) *httpx.HTTPError {
	return httpx.NewError(http.StatusInternalServerError, http.StatusInternalServerError,
		fmt.Sprintf("No configuration found with id %s", id))
}

// platformConfigCreate handles create(body): save → single(saved).
func (h *Handler) platformConfigCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pcfgUpdatePerm) {
		return
	}
	var body pgdoc.M
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), pcfgCollection, body)
	if httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, shapeDoc(saved))
}

// platformConfigUpdate handles update(id, body): getConfiguration(id)-or-500 → save(body, id-preserved) → single.
func (h *Handler) platformConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pcfgUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var body pgdoc.M
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), pcfgCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, platformConfigNotFound(id))
		return
	}
	if err := h.repo.ReplaceDoc(r.Context(), pcfgCollection, id, body); httpx.WriteError(w, err) {
		return
	}
	doc, _ := h.repo.FindDoc(r.Context(), pcfgCollection, id)
	// UPDATE PLATFORM_CONFIGURATION: field-level diff (middleware computes diffSnapshots(before, after)).
	audit.RecordSnapshots(r.Context(), existing, doc)
	httpx.OK(w, shapeDoc(doc))
}

// platformConfigDelete handles deleteConfiguration(id) → success().
func (h *Handler) platformConfigDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pcfgUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.repo.DeleteDoc(r.Context(), pcfgCollection, id); httpx.WriteError(w, err) {
		return
	}
	httpx.OK(w, "Successful operation")
}

// platformConfigRegions handles updateRegions(id, regions): getConfiguration(id)-or-500 → set
// config.regions = body (a list of RegionDisplayConfig) → save → single.
func (h *Handler) platformConfigRegions(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, pcfgUpdatePerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var regions json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&regions); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), pcfgCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, platformConfigNotFound(id))
		return
	}
	var regionsVal any
	_ = json.Unmarshal(regions, &regionsVal)
	if _, err := h.repo.SetFields(r.Context(), pcfgCollection, id, pgdoc.M{"regions": regionsVal}); httpx.WriteError(w, err) {
		return
	}
	doc, _ := h.repo.FindDoc(r.Context(), pcfgCollection, id)
	httpx.OK(w, shapeDoc(doc))
}
