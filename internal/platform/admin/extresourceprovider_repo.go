package admin

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// extresourceprovider_repo.go: the hmac_keys store ops the ExternalResourceProvider admin mutations
// need. HmacKey lives in the hmac_keys collection with a String `_id` (the generated "pk…" id).

// InsertHmacKey persists a generated HMAC key doc from the generate flow. The doc
// already carries its string `_id`.
func (r *Repo) InsertHmacKey(ctx context.Context, doc pgdoc.M) (string, error) {
	return r.c(erpHmacCollection).InsertOne(ctx, doc)
}

// DeleteHmacKey deletes an HMAC key by its string id (a silent no-op when
// the key is absent). Returns the deleted count.
func (r *Repo) DeleteHmacKey(ctx context.Context, id string) (int64, error) {
	ok, err := r.c(erpHmacCollection).DeleteByID(ctx, id)
	if err != nil {
		return 0, err
	}
	if ok {
		return 1, nil
	}
	return 0, nil
}
