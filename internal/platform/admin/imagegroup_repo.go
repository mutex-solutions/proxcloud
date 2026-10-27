package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// imagegroup_repo.go holds the image-group-specific repo methods. The generic crud.go
// helpers cover load-by-id / insert / delete; these add the upsert (used by the update endpoints,
// which key off the body/path id) and the list-by-category-id (the groups-by-category read + the
// category-delete cascade).

// imageUpsert replaces the doc by id for the update path, inserting if the id does not yet exist
// (upsert). The doc carries only the domain fields (no id/_id) — the key is taken from the path id.
// This is a full replace, not a partial-field merge, so an omitted body field becomes absent
// (dropped), matching a full save of the request body.
func (r *Repo) imageUpsert(ctx context.Context, collection, id string, doc pgdoc.M) error {
	return r.c(collection).Upsert(ctx, id, doc)
}

// imageGroupsByCategory returns the raw ImageGroup docs for a category id (still
// carrying `_id`; the handler shapeDoc's each). Never nil.
func (r *Repo) imageGroupsByCategory(ctx context.Context, categoryID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c(imageGroupCollection).Find(ctx, pgdoc.M{"categoryId": categoryID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
