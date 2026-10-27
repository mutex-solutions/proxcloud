// Package catalog serves the cloud-catalog config reads — flavor categories and image
// groups. These are admin-configured document collections (flavorCategory / imageGroup /
// imageCategory), NOT live cloud. Under the greenfield seed they are empty.
//
// The image-group projection: groups + their image descriptions sort by orderNumber, and
// categories are filtered to those an ENABLED group points at (plus the bareMetal category
// filter). flavor-categories stays a raw pass-through list.
package catalog

import (
	"context"
	"net/http"
	"reflect"
	"sort"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type Repo struct {
	flavorCategories *pgdoc.Store
	imageGroups      *pgdoc.Store
	imageCategories  *pgdoc.Store
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{
		flavorCategories: db.C("flavorCategory"),
		imageGroups:      db.C("imageGroup"),
		imageCategories:  db.C("imageCategory"),
	}
}

func findAll(ctx context.Context, col *pgdoc.Store, filter pgdoc.M) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := col.Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	for _, d := range out {
		shapeCatalogDoc(d)
	}
	return out, nil
}

// shapeCatalogDoc maps a raw catalog document to the client shape: rename `_id`→`id`
// (the FE binds `id` + matches imageGroup.categoryId to imageCategory.id) and drop the
// `_class` discriminator. Internal test markers are removed too.
func shapeCatalogDoc(d pgdoc.M) {
	if v, ok := d["_id"]; ok {
		d["id"] = v
		delete(d, "_id")
	}
	delete(d, "_class")
	delete(d, "seedTag")
}

// AllFlavorCategories returns every flavor category.
func (r *Repo) AllFlavorCategories(ctx context.Context) ([]pgdoc.M, error) {
	return findAll(ctx, r.flavorCategories, pgdoc.M{})
}

// AllImageGroups returns every image group.
func (r *Repo) AllImageGroups(ctx context.Context) ([]pgdoc.M, error) {
	return findAll(ctx, r.imageGroups, pgdoc.M{})
}

// AllImageCategories returns image categories:
// bareMetal=true → only bareMetal categories; else → bareMetal false OR absent.
func (r *Repo) AllImageCategories(ctx context.Context, bareMetal bool) ([]pgdoc.M, error) {
	filter := pgdoc.M{"$or": []pgdoc.M{{"bareMetal": false}, {"bareMetal": pgdoc.M{"$exists": false}}, {"bareMetal": nil}}}
	if bareMetal {
		filter = pgdoc.M{"bareMetal": true}
	}
	return findAll(ctx, r.imageCategories, filter)
}

// orderNum reads an orderNumber that may round-trip from the codec as int32/int64/float64.
func orderNum(d pgdoc.M) int {
	switch v := d["orderNumber"].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

var tList = reflect.TypeOf([]any(nil))

// asList tolerates the named slice types the codec hands back for nested arrays
// (e.g. pgdoc.A). The conversion shares the backing array, so in-place sorts
// stick to the original document.
func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	rv := reflect.ValueOf(v)
	if rv.IsValid() && rv.Kind() == reflect.Slice && rv.Type().ConvertibleTo(tList) {
		return rv.Convert(tList).Interface().([]any)
	}
	return nil
}

// SortImageGroups applies the image-group projection: each group's `images` sorted by
// orderNumber asc, then the groups themselves sorted by orderNumber asc (stable sort).
func SortImageGroups(groups []pgdoc.M) []pgdoc.M {
	for _, g := range groups {
		imgs := asList(g["images"])
		if imgs == nil {
			continue
		}
		sort.SliceStable(imgs, func(i, j int) bool {
			di, _ := imgs[i].(pgdoc.M)
			dj, _ := imgs[j].(pgdoc.M)
			return orderNum(di) < orderNum(dj)
		})
	}
	sort.SliceStable(groups, func(i, j int) bool { return orderNum(groups[i]) < orderNum(groups[j]) })
	return groups
}

// FilterCategoriesWithEnabledGroup applies the category filter: keep only categories
// that SOME enabled group points at (group.enabled && group.categoryId == category.id).
// The docs are already shaped (`_id`→`id` via shapeCatalogDoc); ids are plain strings.
func FilterCategoriesWithEnabledGroup(cats, groups []pgdoc.M) []pgdoc.M {
	enabledCatIDs := map[string]bool{}
	for _, g := range groups {
		enabled, _ := g["enabled"].(bool)
		if !enabled {
			continue
		}
		if cid, _ := g["categoryId"].(string); cid != "" {
			enabledCatIDs[cid] = true
		}
	}
	out := make([]pgdoc.M, 0, len(cats))
	for _, c := range cats {
		if id, _ := c["id"].(string); id != "" && enabledCatIDs[id] {
			out = append(out, c)
		}
	}
	return out
}

// ImageGrouping is the image-grouping response. Both lists always serialize (non-null empties)
// → {imageGroups:[], imageCategories:[]} for a fresh deployment.
type ImageGrouping struct {
	ImageGroups     []pgdoc.M `json:"imageGroups"`
	ImageCategories []pgdoc.M `json:"imageCategories"`
}

type Handler struct{ repo *Repo }

func NewHandler(repo *Repo) *Handler { return &Handler{repo: repo} }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/flavor-categories", h.flavorCategories)
	r.Get("/groups/images", h.imageGroups)
}

// flavorCategories serves GET /api/v1/flavor-categories → list envelope.
func (h *Handler) flavorCategories(w http.ResponseWriter, r *http.Request) {
	items, err := h.repo.AllFlavorCategories(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	httpx.List(w, items)
}

// imageGroups serves GET /api/v1/groups/images?bareMetal= → single ImageGrouping envelope.
// A missing/invalid bareMetal value defaults to false, which is identical under the empty seed.
func (h *Handler) imageGroups(w http.ResponseWriter, r *http.Request) {
	bareMetal := r.URL.Query().Get("bareMetal") == "true"
	groups, err := h.repo.AllImageGroups(r.Context())
	if httpx.WriteError(w, err) {
		return
	}
	cats, err := h.repo.AllImageCategories(r.Context(), bareMetal)
	if httpx.WriteError(w, err) {
		return
	}
	// Sort images+groups by orderNumber, keep only categories that an enabled group points at
	// (the wizard's category tabs).
	groups = SortImageGroups(groups)
	cats = FilterCategoriesWithEnabledGroup(cats, groups)
	httpx.OK(w, ImageGrouping{ImageGroups: groups, ImageCategories: cats})
}
