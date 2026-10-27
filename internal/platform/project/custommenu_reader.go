package project

// CustomMenuReader feeds the admin-configured customMenuItem docs into the client
// /init menu (one of the base menu-item providers, merged before the OpenStack
// service items).
// Each doc renders as a "More"-section item on the client:
// { displayName, url, icon, enabled:true, externalRoute:true, newMenuItem:true,
//   renderMode, order } keyed by the display-name slug.

import (
	"context"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

type CustomMenuReader struct {
	col *pgdoc.Store
}

// NewCustomMenuReader binds the reader to the customMenuItem collection.
func NewCustomMenuReader(db *pgdoc.DB) *CustomMenuReader {
	return &CustomMenuReader{col: db.C("customMenuItem")}
}

// Items returns the menu-item map entries, keyed by slug (displayName spaces→dashes,
// lowercased), sorted by order. Errors collapse to empty
// (the menu is best-effort, matching the OpenStack half).
func (r *CustomMenuReader) Items(ctx context.Context) map[string]any {
	out := map[string]any{}
	var docs []pgdoc.M
	if err := r.col.Find(ctx, pgdoc.M{}, &docs,
		pgdoc.Sort(pgdoc.AscK("order", pgdoc.KNum))); err != nil {
		return out
	}
	for _, d := range docs {
		name, _ := d["displayName"].(string)
		if name == "" {
			continue
		}
		slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
		item := map[string]any{
			"displayName":   name,
			"enabled":       true,
			"externalRoute": true,
			"newMenuItem":   true,
		}
		if v, ok := d["url"]; ok {
			item["url"] = v
		}
		if v, ok := d["icon"]; ok {
			item["icon"] = v
		}
		if v, ok := d["renderMode"]; ok {
			item["renderMode"] = v
		}
		if v, ok := d["order"]; ok {
			item["order"] = v
		}
		out[slug] = item
	}
	return out
}
