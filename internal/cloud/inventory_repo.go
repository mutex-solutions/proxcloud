package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// sameInventoryRow compares the entire persisted row read for a visibility proof,
// not just a timestamp (some writers deliberately retain an older updatedAt).
func sameInventoryRow(a, b *CloudResource) bool {
	x, err := json.Marshal(a)
	if err != nil {
		return false
	}
	y, err := json.Marshal(b)
	return err == nil && bytes.Equal(x, y)
}

// UpdateIfUnchanged writes only the exact row whose inventory was validated.
// The comparison and mutation hold the same row lock in one transaction.
func (r *Repo) UpdateIfUnchanged(ctx context.Context, expected, fresh *CloudResource) (bool, error) {
	if err := r.ensure(ctx); err != nil {
		return false, err
	}
	changed := false
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		var cur CloudResource
		found, err := r.resources.GetForUpdate(tc, expected.ID, &cur)
		if err != nil || !found {
			return err
		}
		if !sameInventoryRow(&cur, expected) {
			return nil
		}
		changed, err = r.resources.SetByID(tc, cur.ID, toDbRecord(fresh), nil)
		return err
	})
	return changed, err
}

// DeleteAndArchiveIfUnchanged retires the proved row atomically. A concurrent
// owner, CIDR, external-id or data change invalidates the proof, even if the
// writer reused updatedAt. History and deletion commit together, never separately.
func (r *Repo) DeleteAndArchiveIfUnchanged(ctx context.Context, expected *CloudResource, now time.Time) (bool, error) {
	if err := r.ensure(ctx); err != nil {
		return false, err
	}
	deleted := false
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		var cur CloudResource
		found, err := r.resources.GetForUpdate(tc, expected.ID, &cur)
		if err != nil || !found {
			return err
		}
		if !sameInventoryRow(&cur, expected) {
			return nil
		}
		if err := r.archive(tc, &cur, now, InventorySnapshotHash(&cur)); err != nil {
			return err
		}
		deleted, err = r.resources.DeleteByID(tc, cur.ID)
		return err
	})
	return deleted, err
}

// InsertIfAbsent cannot overwrite a row another writer added after validation.
// It takes the same external-id lock as Upsert, but has no update fallback.
func (r *Repo) InsertIfAbsent(ctx context.Context, res *CloudResource) (bool, error) {
	if err := r.ensure(ctx); err != nil {
		return false, err
	}
	inserted := false
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		if err := r.db.LockXact(tc, externalIDLock(res.ServiceID, res.ExternalID)); err != nil {
			return err
		}
		var cur CloudResource
		_, found, err := r.resources.FindOneForUpdate(tc, matchByServiceExternal(res), &cur)
		if err != nil || found {
			return err
		}
		doc := toDbRecord(res)
		if res.CreatedAt != nil {
			doc["createdAt"] = res.CreatedAt
		}
		_, err = r.resources.InsertOne(tc, pgdoc.M(doc))
		inserted = err == nil
		return err
	})
	return inserted, err
}

// InventorySnapshotHash binds all persisted CloudResource fields, including Info
// and updatedAt. Callers use stored snapshots, never a fresh provider projection.
func InventorySnapshotHash(cr *CloudResource) string {
	b, err := json.Marshal(cr)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
