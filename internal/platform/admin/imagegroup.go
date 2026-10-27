package admin

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// imagegroup.go serves the MUTATIONS + the missing reads of the images surface
// (/api/v1/admin/images). The bare category list (GET /images/categories) is ALREADY registered
// in handler.go (listRaw "imageCategory") and is intentionally NOT re-registered here.
//
// This surface is pure datastore CRUD over two tables — NO cloud / OpenStack calls — so
// there is nothing left unwired. ImageCategory + ImageGroup are plain document domains saved/read via the
// id-aware crud.go helpers; the handler returns the RAW domain, so shapeDoc (_id→id, drop _class)
// gives the faithful JSON.
//
// Every endpoint gates on the admin:image_group:manage permission.
//
// create/update/delete on both domains should write audit events (
// IMAGE_CATEGORY / IMAGE_GROUP) — deferred this pass (// TODO(audit)); the persisted state + the
// response envelope are faithful, which is what the admin UI exercises.
//
// Faithfulness notes on the write semantics:
//   - create (category/group): store the request body with no id → the datastore assigns the id
//     (InsertDoc strips any id/_id). Returns the saved doc.
//   - update (category/group): the handler IGNORES the path {id} and just stores the request body
//     as an upsert keyed by the path id (the FE sends a matching id) — a full replace, NOT a
//     partial-field merge (an omitted body field becomes absent, dropped from the JSON). Returns
//     the saved doc.
//   - delete (category) CASCADES: load the category's groups → delete each group, then delete the
//     category by id. Returns no body → HTTP 200 with an EMPTY body.

const imageGroupPerm = "admin:image_group:manage"

const (
	imageCategoryCollection = "imageCategory"
	imageGroupCollection    = "imageGroup"
)

// routeImageGroup registers the image-group endpoints NOT already in handler.go:
// the category by-id read + groups-by-category read + category CRUD mutations, and the group
// read + CRUD mutations. The bare GET /images/categories list stays in handler.go.
//
// chi: under /images the param position after /categories/ and /groups/ uses the name {id}
// (a consistent `id` path variable); the static `groups` suffix on /categories/{id}/groups
// is a child of the {id} param (no conflict).
func (h *Handler) routeImageGroup(r chi.Router) {
	// ImageCategory
	r.Post("/images/categories", h.imageCategoryCreate)
	r.Get("/images/categories/{id}", h.imageCategoryGet)
	r.Put("/images/categories/{id}", h.imageCategoryUpdate)
	r.Delete("/images/categories/{id}", h.imageCategoryDelete)
	r.Get("/images/categories/{id}/groups", h.imageGroupsByCategory)
	// ImageGroup
	r.Post("/images/groups", h.imageGroupCreate)
	r.Get("/images/groups/{id}", h.imageGroupGet)
	r.Put("/images/groups/{id}", h.imageGroupUpdate)
	r.Delete("/images/groups/{id}", h.imageGroupDelete)
}

// imageCategoryReq is the ImageCategory domain's mutable fields (request body). The id is the
// optional body id used on write (we key the upsert by the path id on update). bareMetal
// is a primitive bool → always emitted; name/description are nullable strings → omitted
// when blank so the JSON drops them (a null field is dropped, not emitted as "").
type imageCategoryReq struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BareMetal   bool   `json:"bareMetal"`
}

// doc builds the stored JSON for an ImageCategory. bareMetal (primitive) is always set; blank
// optional strings are omitted.
func (req imageCategoryReq) doc() pgdoc.M {
	d := pgdoc.M{"bareMetal": req.BareMetal}
	if req.Name != "" {
		d["name"] = req.Name
	}
	if req.Description != "" {
		d["description"] = req.Description
	}
	return d
}

// imageGroupReq is the ImageGroup domain's mutable fields (request body). enabled/orderNumber
// are primitives → always emitted; the nullable strings + the labels/images lists are omitted when
// blank/nil. labels/images pass through as raw sub-docs ({label,description,color} /
// {name,version,orderNumber}).
type imageGroupReq struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	Enabled      bool                 `json:"enabled"`
	OrderNumber  int                  `json:"orderNumber"`
	CategoryID   string               `json:"categoryId"`
	Description  string               `json:"description"`
	GroupLogoURL string               `json:"groupLogoUrl"`
	Labels       []imageGroupLabelReq `json:"labels"`
	Images       []imageGroupImageReq `json:"images"`
}

// imageGroupLabelReq is a nested label sub-doc (no id).
type imageGroupLabelReq struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Color       string `json:"color"`
}

// imageGroupImageReq is a nested image sub-doc (no id). orderNumber is a
// primitive int → always stored.
type imageGroupImageReq struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	OrderNumber int    `json:"orderNumber"`
}

// doc builds the stored JSON for an ImageGroup. enabled/orderNumber (primitives) are always set;
// blank optional strings and nil lists are omitted. A non-nil (even empty) labels/images
// list is kept (only nulls are dropped, non-null empties are kept).
func (req imageGroupReq) doc() pgdoc.M {
	d := pgdoc.M{"enabled": req.Enabled, "orderNumber": req.OrderNumber}
	if req.Name != "" {
		d["name"] = req.Name
	}
	if req.CategoryID != "" {
		d["categoryId"] = req.CategoryID
	}
	if req.Description != "" {
		d["description"] = req.Description
	}
	if req.GroupLogoURL != "" {
		d["groupLogoUrl"] = req.GroupLogoURL
	}
	if req.Labels != nil {
		d["labels"] = req.Labels
	}
	if req.Images != nil {
		d["images"] = req.Images
	}
	return d
}

// --- ImageCategory handlers ---

// imageCategoryCreate stores the request body as a new category and returns the saved doc.
func (h *Handler) imageCategoryCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	var req imageCategoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), imageCategoryCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a CREATE IMAGE_CATEGORY admin audit event.
	httpx.OK(w, shapeDoc(saved))
}

// imageCategoryGet loads a category by id; 404 "Image category not found" if absent.
func (h *Handler) imageCategoryGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), imageCategoryCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Image category not found"))
		return
	}
	httpx.OK(w, shapeDoc(doc))
}

// imageCategoryUpdate IGNORES the request body's own id and upserts the category, keyed by the
// path id (the FE sends a matching id); an omitted body field becomes absent (dropped) — so this
// is a full overwrite, not a partial-field merge. Returns the saved doc.
func (h *Handler) imageCategoryUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req imageCategoryReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	before, _ := h.repo.FindDoc(r.Context(), imageCategoryCollection, id) // pre-upsert snapshot for the audit diff
	if err := h.repo.imageUpsert(r.Context(), imageCategoryCollection, id, req.doc()); httpx.WriteError(w, err) {
		return
	}
	// UPDATE IMAGE_CATEGORY: field-level diff (the middleware diffs the before/after snapshots).
	after, _ := h.repo.FindDoc(r.Context(), imageCategoryCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	out := req.doc()
	out["id"] = id
	httpx.OK(w, out)
}

// imageCategoryDelete CASCADE-deletes: load the category's groups → delete each group → delete the
// category by id. Returns no body → HTTP 200, empty body. (Does NOT 404 a missing category here —
// deleting an absent id is a no-op.)
func (h *Handler) imageCategoryDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	// Cascade-delete the category's groups first (one delete per group).
	groups, err := h.repo.imageGroupsByCategory(r.Context(), id)
	if httpx.WriteError(w, err) {
		return
	}
	for _, g := range groups {
		gid := imageDocID(g)
		if gid == "" {
			continue
		}
		if _, err := h.repo.DeleteDoc(r.Context(), imageGroupCollection, gid); httpx.WriteError(w, err) {
			return
		}
		// TODO(audit): write a DELETE IMAGE_GROUP admin audit event.
	}
	// TODO(audit): write a DELETE IMAGE_CATEGORY admin audit event.
	if _, err := h.repo.DeleteDoc(r.Context(), imageCategoryCollection, id); httpx.WriteError(w, err) {
		return
	}
	// no body → HTTP 200, empty body.
	w.WriteHeader(http.StatusOK)
}

// imageGroupsByCategory lists the groups for a category id → list envelope. NO 404 if the category
// is absent — just an empty list.
func (h *Handler) imageGroupsByCategory(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	items, err := h.repo.imageGroupsByCategory(r.Context(), chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	for i := range items {
		shapeDoc(items[i])
	}
	httpx.List(w, items)
}

// --- ImageGroup handlers ---

// imageGroupCreate stores the request body as a new group and returns the saved doc.
func (h *Handler) imageGroupCreate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	var req imageGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	saved, err := h.repo.InsertDoc(r.Context(), imageGroupCollection, req.doc())
	if httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a CREATE IMAGE_GROUP admin audit event.
	httpx.OK(w, shapeDoc(saved))
}

// imageGroupGet loads a group by id; 404 "Image group not found" if absent.
func (h *Handler) imageGroupGet(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	doc, err := h.repo.FindDoc(r.Context(), imageGroupCollection, chi.URLParam(r, "id"))
	if httpx.WriteError(w, err) {
		return
	}
	if doc == nil {
		httpx.WriteError(w, httpx.NotFound("Image group not found"))
		return
	}
	httpx.OK(w, shapeDoc(doc))
}

// imageGroupUpdate IGNORES the request body's own id and upserts the group, keyed by the path id,
// full overwrite (nulls dropped). Returns the saved doc.
func (h *Handler) imageGroupUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	id := chi.URLParam(r, "id")
	var req imageGroupReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, httpx.BadRequest("Invalid request body"))
		return
	}
	before, _ := h.repo.FindDoc(r.Context(), imageGroupCollection, id) // pre-upsert snapshot for the audit diff
	if err := h.repo.imageUpsert(r.Context(), imageGroupCollection, id, req.doc()); httpx.WriteError(w, err) {
		return
	}
	// UPDATE IMAGE_GROUP: field-level diff (the middleware diffs the before/after snapshots).
	after, _ := h.repo.FindDoc(r.Context(), imageGroupCollection, id)
	audit.RecordSnapshots(r.Context(), before, after)
	out := req.doc()
	out["id"] = id
	httpx.OK(w, out)
}

// imageGroupDelete deletes the group by id. Returns no body → HTTP 200, empty body.
func (h *Handler) imageGroupDelete(w http.ResponseWriter, r *http.Request) {
	if !h.require(w, r, imageGroupPerm) {
		return
	}
	if _, err := h.repo.DeleteDoc(r.Context(), imageGroupCollection, chi.URLParam(r, "id")); httpx.WriteError(w, err) {
		return
	}
	// TODO(audit): write a DELETE IMAGE_GROUP admin audit event.
	w.WriteHeader(http.StatusOK)
}

// imageDocID returns the stored doc's string id (`_id`, else `id`), or "".
func imageDocID(d pgdoc.M) string {
	if d == nil {
		return ""
	}
	v, ok := d["_id"]
	if !ok {
		if v, ok = d["id"]; !ok {
			return ""
		}
	}
	s, _ := v.(string)
	return s
}
