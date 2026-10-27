package admin

import (
	"encoding/json"
	"maps"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// custommenu.go serves the custom-menu surface (/api/v1/admin/menu) — the REFERENCE pattern for
// the admin mutation handlers: id-aware CRUD via the crud.go helpers, exact perms / error
// strings / response envelopes, `_id`→`id` shaping on the way out.
//
// All endpoints gate on ADMIN_MENU_MANAGE. create/update/delete/reorder also write audit
// events — deferred this pass (// TODO(audit)); the state + response are
// faithful, which is what the admin UI exercises.

const menuPerm = "admin:menu:manage"

const menuCollection = "customMenuItem"

// routeCustomMenu registers the custom-menu admin routes. Replaces the demo-stub `/menu`
// listRaw that previously lived in handler.go (this is the faithful, order-sorted list).
func (h *Handler) routeCustomMenu(r chi.Router) {
	r.Get("/menu", h.menuList)
	r.Post("/menu", h.menuCreate)
	r.Get("/menu/placeholders", h.menuPlaceholders)
	r.Put("/menu/reorder", h.menuReorder)
	r.Get("/menu/{id}", h.menuGet)
	r.Put("/menu/{id}", h.menuUpdate)
	r.Delete("/menu/{id}", h.menuDelete)
}

// customMenuItemReq is the CustomMenuItem domain's mutable fields (the request body).
type customMenuItemReq struct {
	DisplayName string `json:"displayName"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	RenderMode  string `json:"renderMode"`
	Order       int    `json:"order"`
}

// doc builds the stored JSON for a CustomMenuItem; optional strings are omitted when blank so the
// JSON drops them (a null field is dropped, not emitted as "").
func (req customMenuItemReq) doc() pgdoc.M {
	d := pgdoc.M{"order": req.Order}
	if req.DisplayName != "" {
		d["displayName"] = req.DisplayName
	}
	if req.URL != "" {
		d["url"] = req.URL
	}
	if req.Icon != "" {
		d["icon"] = req.Icon
	}
	if req.RenderMode != "" {
		d["renderMode"] = req.RenderMode
	}
	return d
}

// menuList lists all custom menu items sorted by `order` asc → list envelope.
func (h *Handler) menuList(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	items, err := h.repo.ListSorted(r.Context(), menuCollection, "order", 1)
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// menuCreate inserts a new custom menu item and returns it.
func (h *Handler) menuCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	var req customMenuItemReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), menuCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a menu item is created.
	httpx.OK(w, shapeDoc(saved))
}

// menuUpdate looks up the item by id (404 if absent), overwrites its 5 fields, saves, and returns it.
func (h *Handler) menuUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req customMenuItemReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	existing, err := h.repo.FindDoc(r.Context(), menuCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Custom menu item not found"))
		return
	}
	before := maps.Clone(existing)
	d := req.doc()
	for _, k := range []string{"displayName", "url", "icon", "renderMode"} {
		delete(existing, k) // overwrite — drop the old value first so an omitted field becomes null
	}
	for k, v := range d {
		existing[k] = v
	}
	if err := h.repo.ReplaceDoc(r.Context(), menuCollection, id, existing); httpx.WriteError(w, err) {
		return
	}
	// UPDATE audit: field-level diff — the middleware diffs the before/after snapshots.
	after, _ := h.repo.FindDoc(r.Context(), menuCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	httpx.OK(w, shapeDoc(existing))
}

// menuDelete looks up the item by id (404 if absent), deletes it, and returns "Successful operation".
func (h *Handler) menuDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	existing, err := h.repo.FindDoc(r.Context(), menuCollection, id)
	if httpx.WriteError(w, err) {
		return
	}
	if existing == nil {
		httpx.WriteError(w, httpx.NotFound("Custom menu item not found"))
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), menuCollection, id); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write an admin audit event when a menu item is deleted.
	httpx.OK(w, "Successful operation")
}

// menuGet looks up the item by id (404 if absent) and returns it.
func (h *Handler) menuGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), menuCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Custom menu item not found"))
		return
	}
	httpx.OK(w, shapeDoc(doc))
}

// menuReorder sets order=i for each id at index i (404 if any id is missing).
func (h *Handler) menuReorder(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	var ids []string
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	for i, id := range ids {
		existing, err := h.repo.FindDoc(r.Context(), menuCollection, id)
		if httpx.WriteError(w, err) {
			return
		}
		if existing == nil {
			httpx.WriteError(w, httpx.NotFound("Custom menu item not found"))
			return
		}
		if asInt(existing["order"]) != i {
			if _, err := h.repo.SetFields(r.Context(), menuCollection, id, pgdoc.M{"order": i}); httpx.WriteError(w, err) {
				return
			}
		}
	}
	httpx.OK(w, "Successful operation")
}

// menuPlaceholders returns the static URL placeholder map.
func (h *Handler) menuPlaceholders(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, menuPerm) {
		return
	}
	httpx.OK(w, menuURLPlaceholders())
}

// menuURLPlaceholders returns the URL placeholder map (static).
func menuURLPlaceholders() map[string][]string {
	return map[string][]string{
		"project":        {"id", "name"},
		"user":           {"id", "email", "firstName", "lastName"},
		"billingProfile": {"id", "name"},
	}
}
