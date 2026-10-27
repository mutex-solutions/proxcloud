package admin

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// instancemetadata.go serves the MUTATIONS of the instance-metadata-options surface
// (/api/v1/admin/instance-metadata-options). The bare list read
// (GET) is ALREADY registered in handler.go (h.listRaw on collection
// "instanceMetadataOption") and is intentionally NOT re-registered here; the by-id read (GET /{id})
// is NOT yet registered, so it is added here (faithful 404 path).
//
// All endpoints gate on the admin:instance_metadata:manage permission. The writes are pure
// datastore (no identity/external side effects), so the whole surface is in-scope and handled
// faithfully via the crud.go helpers. create/update/disable/permanent-delete/reactivate emit
// audit events — deferred this pass (TODO(audit));
// the persisted state + response shape are faithful, which is what the admin UI exercises.

const instanceMetadataPerm = "admin:instance_metadata:manage"

const instanceMetadataCollection = "instanceMetadataOption"

// instanceMetadataReservedPrefixes is the reserved key prefixes.
var instanceMetadataReservedPrefixes = []string{"hw:", "os_", "proxcloud_"}

// routeInstanceMetadata registers the instance-metadata-option admin mutation routes + the by-id read.
// The bare GET list is already in handler.go. chi: the {id} param name is reused across positions.
func (h *Handler) routeInstanceMetadata(r chi.Router) {
	r.Post("/instance-metadata-options", h.instanceMetadataCreate)
	r.Get("/instance-metadata-options/{id}", h.instanceMetadataGetByID)
	r.Put("/instance-metadata-options/{id}", h.instanceMetadataUpdate)
	r.Delete("/instance-metadata-options/{id}", h.instanceMetadataDelete)
	r.Post("/instance-metadata-options/{id}/reactivate", h.instanceMetadataReactivate)
}

// metadataValueOptionReq is a MetadataValueOption (enabled defaults true, but
// the request-body value is whatever the client sends — we store it as-is). stored JSON omits blank
// optional strings so they are omitted on the way back out.
type metadataValueOptionReq struct {
	DisplayName string `json:"displayName"`
	Value       string `json:"value"`
	Enabled     bool   `json:"enabled"`
}

// numericRangeReq is a NumericRange (min/max are primitive doubles, unit optional).
type numericRangeReq struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Unit string  `json:"unit"`
}

// instanceMetadataOptionReq is the mutable fields of InstanceMetadataOption (request body). The
// pointer/slice-nil distinction matters: a nil Options/NumericRange/ServiceIds/Regions means "not
// provided" (null), which drives the validation branches and the null omission.
type instanceMetadataOptionReq struct {
	Key          string                   `json:"key"`
	DisplayName  string                   `json:"displayName"`
	Description  string                   `json:"description"`
	Type         *string                  `json:"type"`
	Options      []metadataValueOptionReq `json:"options"`
	NumericRange *numericRangeReq         `json:"numericRange"`
	ServiceIds   []string                 `json:"serviceIds"`
	Regions      []string                 `json:"regions"`
	UserEditable bool                     `json:"userEditable"`
	ShowInline   bool                     `json:"showInline"`
}

// validateTypeAndShape checks the type is present, runs validatePredefinedValues or
// validateNumericRange per type, then validateRegionsRequireServiceIds.
func (req instanceMetadataOptionReq) validateTypeAndShape() *httpx.HTTPError {
	t := req.Type
	if t == nil {
		return httpx.BadRequest("Metadata option type is required")
	}
	switch *t {
	case "PREDEFINED_VALUES":
		if err := req.validatePredefinedValues(); err != nil {
			return err
		}
	case "NUMERIC_RANGE":
		if err := req.validateNumericRange(); err != nil {
			return err
		}
	}
	return req.validateRegionsRequireServiceIds()
}

// validatePredefinedValues runs the PREDEFINED_VALUES branch.
func (req instanceMetadataOptionReq) validatePredefinedValues() *httpx.HTTPError {
	if req.NumericRange != nil {
		return httpx.BadRequest("numericRange must be null for PREDEFINED_VALUES type")
	}
	if len(req.Options) > 0 {
		for _, o := range req.Options {
			if strings.TrimSpace(o.Value) == "" {
				return httpx.BadRequest("Each value option must have a non-blank value")
			}
			if strings.TrimSpace(o.DisplayName) == "" {
				return httpx.BadRequest("Each value option must have a non-blank displayName")
			}
		}
		seen := map[string]struct{}{}
		dup := false
		for _, o := range req.Options {
			lv := strings.ToLower(o.Value)
			if _, ok := seen[lv]; ok {
				dup = true
				break
			}
			seen[lv] = struct{}{}
		}
		if dup {
			return httpx.BadRequest("Duplicate values are not allowed in options")
		}
		return nil
	}
	return httpx.BadRequest("At least one value option is required for PREDEFINED_VALUES type")
}

// validateNumericRange runs the NUMERIC_RANGE branch.
func (req instanceMetadataOptionReq) validateNumericRange() *httpx.HTTPError {
	if len(req.Options) > 0 {
		return httpx.BadRequest("options must be empty for NUMERIC_RANGE type")
	}
	if req.NumericRange == nil {
		return httpx.BadRequest("numericRange is required for NUMERIC_RANGE type")
	}
	if req.NumericRange.Min >= req.NumericRange.Max {
		return httpx.BadRequest("numericRange.min must be less than numericRange.max")
	}
	return nil
}

// validateRegionsRequireServiceIds runs the regions-without-serviceIds guard.
func (req instanceMetadataOptionReq) validateRegionsRequireServiceIds() *httpx.HTTPError {
	if len(req.Regions) > 0 && len(req.ServiceIds) == 0 {
		return httpx.BadRequest("Regions cannot be specified without service IDs")
	}
	return nil
}

// validateInstanceMetadataKey validates the key: required + not a
// reserved prefix (case-insensitive).
func validateInstanceMetadataKey(key string) *httpx.HTTPError {
	if strings.TrimSpace(key) == "" {
		return httpx.BadRequest("Metadata key is required")
	}
	lower := strings.ToLower(key)
	for _, prefix := range instanceMetadataReservedPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return httpx.BadRequest("Metadata key cannot start with reserved prefix: " + prefix)
		}
	}
	return nil
}

// validateInstanceMetadataKeyUniqueness rejects (400) an active option with the same
// key (excluding excludeId when set). The check is enabled==true and key match.
func (h *Handler) validateInstanceMetadataKeyUniqueness(r *http.Request, key, excludeID string) (*httpx.HTTPError, error) {
	exists, err := h.repo.InstanceMetadataKeyEnabledExists(r.Context(), key, excludeID)
	if err != nil {
		return nil, err
	}
	if exists {
		return httpx.BadRequest(fmt.Sprintf("An active metadata option with key '%s' already exists", key)), nil
	}
	return nil, nil
}

// valueOptionDoc builds the stored JSON for a MetadataValueOption (blank strings omitted).
func valueOptionDoc(o metadataValueOptionReq) pgdoc.M {
	d := pgdoc.M{"enabled": o.Enabled}
	if o.DisplayName != "" {
		d["displayName"] = o.DisplayName
	}
	if o.Value != "" {
		d["value"] = o.Value
	}
	return d
}

// mutableDoc builds the stored for the mutable fields shared by create/update. Optional fields are
// omitted when blank/nil so the round-tripped JSON drops them (a null is dropped).
func (req instanceMetadataOptionReq) mutableDoc() pgdoc.M {
	d := pgdoc.M{
		"key":          req.Key,
		"userEditable": req.UserEditable,
		"showInline":   req.ShowInline,
	}
	if req.DisplayName != "" {
		d["displayName"] = req.DisplayName
	}
	if req.Description != "" {
		d["description"] = req.Description
	}
	if req.Type != nil {
		d["type"] = *req.Type
	}
	if req.Options != nil {
		opts := make([]pgdoc.M, 0, len(req.Options))
		for _, o := range req.Options {
			opts = append(opts, valueOptionDoc(o))
		}
		d["options"] = opts
	}
	if req.NumericRange != nil {
		nr := pgdoc.M{"min": req.NumericRange.Min, "max": req.NumericRange.Max}
		if req.NumericRange.Unit != "" {
			nr["unit"] = req.NumericRange.Unit
		}
		d["numericRange"] = nr
	}
	if req.ServiceIds != nil {
		d["serviceIds"] = req.ServiceIds
	}
	if req.Regions != nil {
		d["regions"] = req.Regions
	}
	return d
}

// instanceMetadataCreate validates the key, type/shape and key-uniqueness, then stores a new option
// (enabled=true, createdAt=now) and returns the saved doc.
func (h *Handler) instanceMetadataCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, instanceMetadataPerm) {
		return
	}
	var req instanceMetadataOptionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	if err := validateInstanceMetadataKey(req.Key); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if err := req.validateTypeAndShape(); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if herr, err := h.validateInstanceMetadataKeyUniqueness(r, req.Key, ""); httpx.WriteError(w, err) {
		return
	} else if herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	doc := req.mutableDoc()
	doc["enabled"] = true
	doc["createdAt"] = time.Now().UTC()
	saved, err := h.repo.InsertDoc(r.Context(), instanceMetadataCollection, doc)
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a CREATE INSTANCE_METADATA_OPTION audit event.
	httpx.OK(w, shapeDoc(saved))
}

// instanceMetadataGetByID loads an option by id; 404 if absent. (The GET /{id} read — not
// previously registered.)
func (h *Handler) instanceMetadataGetByID(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, instanceMetadataPerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), instanceMetadataCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Instance metadata option not found"))
		return
	}
	httpx.OK(w, shapeDoc(doc))
}

// instanceMetadataUpdate loads the option (404 if absent); if the key changed, re-validates the key
// and its uniqueness; validates type/shape; overwrites the 10 mutable fields; sets updatedAt=now;
// saves and returns it.
func (h *Handler) instanceMetadataUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, instanceMetadataPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req instanceMetadataOptionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), instanceMetadataCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Instance metadata option not found"))
		return
	}
	before := maps.Clone(existing)
	// Only when the key changed: re-validate the key and its uniqueness.
	existingKey, _ := existing["key"].(string)
	if existingKey != req.Key {
		if herr := validateInstanceMetadataKey(req.Key); herr != nil {
			httpx.WriteError(w, herr)
			return
		}
		if herr, err := h.validateInstanceMetadataKeyUniqueness(r, req.Key, id); httpx.WriteError(w, err) {
			return
		} else if herr != nil {
			httpx.WriteError(w, herr)
			return
		}
	}
	if herr := req.validateTypeAndShape(); herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	// Overwrite the 10 mutable fields (key, displayName, description, type, options, numericRange,
	// serviceIds, regions, userEditable, showInline) + updatedAt. Drop the old values first so an
	// omitted/null field becomes absent (dropped), following the menuUpdate reference pattern.
	for _, k := range []string{"key", "displayName", "description", "type", "options", "numericRange", "serviceIds", "regions", "userEditable", "showInline"} {
		delete(existing, k)
	}
	for k, v := range req.mutableDoc() {
		existing[k] = v
	}
	existing["updatedAt"] = time.Now().UTC()
	if err := h.repo.ReplaceDoc(r.Context(), instanceMetadataCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff (the middleware diffs the before/after snapshots).
	after, _ := h.repo.FindDoc(r.Context(), instanceMetadataCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// instanceMetadataDelete handles delete with ?permanent (default false). permanent=true →
// hard delete (load-or-404 → if enabled 400 "Cannot permanently delete an active metadata
// option. Disable it first." else delete by id); else soft-disable (load-or-404 → enabled=false,
// disabledAt=now, disabledBy=admin email → save). Both return 204 No Content.
func (h *Handler) instanceMetadataDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, instanceMetadataPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	permanent := r.URL.Query().Get("permanent") == "true"

	existing, err := h.repo.FindDoc(r.Context(), instanceMetadataCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Instance metadata option not found"))
		return
	}

	if permanent {
		// hard delete: enabled → 400; else delete by id.
		enabled, _ := existing["enabled"].(bool)
		if enabled {
			httpx.WriteError(w, httpx.BadRequest("Cannot permanently delete an active metadata option. Disable it first."))
			return
		}
		if _, err := h.repo.DeleteDoc(r.Context(), instanceMetadataCollection, id); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event when an option is permanently deleted.
	} else {
		// disable: enabled=false, disabledAt=now, disabledBy=admin email.
		email := httpx.RC(r.Context()).Email
		if _, err := h.repo.SetFields(r.Context(), instanceMetadataCollection, id, pgdoc.M{
			"enabled":    false,
			"disabledAt": time.Now().UTC(),
			"disabledBy": email,
		}); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write an admin audit event when an option is disabled.
	}
	// delete returns 204, no body.
	w.WriteHeader(http.StatusNoContent)
}

// instanceMetadataReactivate loads the option (404 if absent), re-checks key uniqueness, then sets
// enabled=true and clears disabledAt/disabledBy → save → returns it.
func (h *Handler) instanceMetadataReactivate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, instanceMetadataPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), instanceMetadataCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Instance metadata option not found"))
		return
	}
	key, _ := existing["key"].(string)
	if herr, err := h.validateInstanceMetadataKeyUniqueness(r, key, id); httpx.WriteError(w, err) {
		return
	} else if herr != nil {
		httpx.WriteError(w, herr)
		return
	}
	// enabled=true; disabledAt/disabledBy cleared. Set enabled and remove the disabled-* fields so
	// they are absent (nulls are dropped anyway).
	existing["enabled"] = true
	delete(existing, "disabledAt")
	delete(existing, "disabledBy")
	if err := h.repo.ReplaceDoc(r.Context(), instanceMetadataCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an UPDATE INSTANCE_METADATA_OPTION audit event.
	httpx.OK(w, shapeDoc(existing))
}
