package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// flexInt tolerates the recovered admin FE sending a numeric field as "" (empty string), a JSON
// number, or a numeric string — all of these coerce to an int, but Go's encoding/json
// rejects "" for an int. The orderNumber field arrives as "" when the admin leaves it blank.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		*f = flexInt(n)
		return nil
	}
	fl, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", s)
	}
	*f = flexInt(int(fl))
	return nil
}

// flavorcategory.go serves the MUTATIONS of the flavor-categories surface (/api/v1/admin/
// flavor-categories). The list + by-id reads are ALREADY in handler.go
// (h.flavorCategories + rawByID, gated ADMIN_FLAVOR_CATEGORY_MANAGE) and are NOT re-registered.
// create/update/delete are pure datastore on the flavorCategory table (no external side effects).
// The wizard Hardware list consumes flavor categories, so this is client-relevant. No money fields.

const flavorCategoryPerm = "admin:flavor_category:manage"

const flavorCategoryCollection = "flavorCategory"

// routeFlavorCategory registers the flavor-category admin mutation routes. The list + by-id reads
// stay in handler.go.
func (h *Handler) routeFlavorCategory(r chi.Router) {
	r.Post("/flavor-categories", h.flavorCategoryCreate)
	r.Put("/flavor-categories/{id}", h.flavorCategoryUpdate)
	r.Delete("/flavor-categories/{id}", h.flavorCategoryDelete)
}

// flavorCategoryReq is the mutable fields of FlavorCategory (the request body). flavors +
// flavorAttributes are arbitrary nested arrays (Flavor / FlavorAttributes) stored as-is.
type flavorCategoryReq struct {
	Name                     string  `json:"name"`
	Description              string  `json:"description"`
	OrderNumber              flexInt `json:"orderNumber"`
	BareMetal                bool    `json:"bareMetal"`
	KubernetesFlavorCategory bool    `json:"kubernetesFlavorCategory"`
	Flavors                  []any   `json:"flavors"`
	FlavorAttributes         []any   `json:"flavorAttributes"`
}

// doc builds the stored JSON for the FlavorCategory fields (nil flavors/flavorAttributes
// omitted; an empty list round-trips as []).
func (req flavorCategoryReq) doc() pgdoc.M {
	d := pgdoc.M{
		"name":                     req.Name,
		"description":              req.Description,
		"orderNumber":              int(req.OrderNumber),
		"bareMetal":                req.BareMetal,
		"kubernetesFlavorCategory": req.KubernetesFlavorCategory,
	}
	if req.Flavors != nil {
		d["flavors"] = req.Flavors
	}
	if req.FlavorAttributes != nil {
		d["flavorAttributes"] = req.FlavorAttributes
	}
	return d
}

// flavorCategoryCreate creates a flavor category: persist it and return the saved doc. No validation.
func (h *Handler) flavorCategoryCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, flavorCategoryPerm) {
		return
	}
	var req flavorCategoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), flavorCategoryCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): CREATE FLAVOR_CATEGORY audit event
	httpx.OK(w, shapeDoc(saved))
}

// flavorCategoryUpdate updates a flavor category: get-or-404 → overwrite the 7 mutable fields →
// bare-metal collision guard (a flavor name must not live in both a bare-metal and a non-bare-metal
// category) → save → return the saved doc.
func (h *Handler) flavorCategoryUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, flavorCategoryPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req flavorCategoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), flavorCategoryCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Flavor category not found"))
		return
	}
	before := maps.Clone(existing)
	// Bare-metal collision guard: for each flavor in the
	// new set, the opposite-bareMetal flag must NOT already contain it in another category.
	for _, f := range req.Flavors {
		fm, _ := f.(map[string]any)
		fn, _ := fm["flavorName"].(string)
		if fn == "" {
			continue
		}
		collide, err := h.flavorNameInCategory(r.Context(), !req.BareMetal, fn, id)
		if httpx.WriteError(w, err) {
			return
		}
		if collide {
			kind := "bare metal"
			if req.BareMetal {
				kind = "non bare metal"
			}
			httpx.WriteError(w, httpx.BadRequest(fmt.Sprintf("Flavor %s is already assigned to a %s category", fn, kind)))
			return
		}
	}
	for _, k := range []string{"name", "description", "orderNumber", "bareMetal", "kubernetesFlavorCategory", "flavors", "flavorAttributes"} {
		delete(existing, k)
	}
	for k, v := range req.doc() {
		existing[k] = v
	}
	if err := h.repo.ReplaceDoc(r.Context(), flavorCategoryCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: record the before/after snapshots; the middleware emits the field-level diff.
	after, _ := h.repo.FindDoc(r.Context(), flavorCategoryCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// flavorNameInCategory reports whether any other
// category with the given bareMetal flag lists this flavor name.
func (h *Handler) flavorNameInCategory(ctx context.Context, bareMetal bool, flavorName, excludeID string) (bool, error) {
	filter := pgdoc.M{
		"bareMetal": bareMetal,
		"flavors":   pgdoc.M{"$contains": pgdoc.M{"flavorName": flavorName}},
		"_id":       pgdoc.M{"$ne": excludeID},
	}
	n, err := h.repo.CountBy(ctx, flavorCategoryCollection, filter)
	return n > 0, err
}

// flavorCategoryDelete deletes a flavor category: get-or-404 → delete by id + audit → 204 No Content.
func (h *Handler) flavorCategoryDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, flavorCategoryPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), flavorCategoryCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Flavor category not found"))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), flavorCategoryCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin DELETE audit event for the flavor category.
	w.WriteHeader(http.StatusNoContent)
}
