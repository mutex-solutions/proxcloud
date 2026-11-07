package cloud

import (
	"context"
	"sync"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/paging"
)

// Repo is the `cloudResource` persistence + sync layer (+ the `cloudResourceHistory`
// archive). The count reads back the project endpoints; the transactional locked
// upsert/OCC-update + archive + recreation guard back the sync job.
type Repo struct {
	db          *pgdoc.DB
	resources   *pgdoc.Store
	history     *pgdoc.Store
	meter       *pgdoc.Store
	tasks       *pgdoc.Store
	ensureOnce  sync.Once
	ensureError error
}

func NewRepo(db *pgdoc.DB) *Repo {
	return &Repo{
		db:        db,
		resources: db.C("cloudResource"),
		history:   db.C("cloudResourceHistory"),
		meter:     db.C("meterState"),
		tasks:     db.C("vmTaskEvent"),
	}
}

// ensure creates the backing tables on first use. Insert/Update open a
// transaction as their first statement, where the store's on-demand
// table-create can't fire (a failed statement aborts the tx), so the tables
// must exist beforehand.
func (r *Repo) ensure(ctx context.Context) error {
	r.ensureOnce.Do(func() {
		if err := r.resources.Ensure(ctx); err != nil {
			r.ensureError = err
			return
		}
		r.ensureError = r.history.Ensure(ctx)
	})
	return r.ensureError
}

// toDbRecord is the set-patch used by both Insert and Update — EXACTLY the persisted
// fields (NOT pricePlan, NOT availabilityZone, NOT _id, NOT createdAt).
//
// createdAt is IMMUTABLE: stamped only when Insert actually creates the row. Including it
// here would let any writer with a nil/stale CreatedAt null or drift the creation date on
// every re-cache (sync updates, notification ingest, action re-inserts) — which blanks the
// UI Created-At column AND breaks createdAt-based billing accrual.
func toDbRecord(r *CloudResource) pgdoc.M {
	return pgdoc.M{
		"serviceId":  r.ServiceID,
		"externalId": r.ExternalID,
		"type":       r.Type,
		"region":     r.Region,
		"projectId":  r.ProjectID,
		"userId":     r.UserID,
		"updatedAt":  r.UpdatedAt,
		"info":       r.Info,
		"data":       r.Data,
	}
}

// StampCreatedAtIfNull backfills a missing createdAt (docs nulled by pre-fix writers). No-op when
// the field is already set — creation dates stay immutable.
func (r *Repo) StampCreatedAtIfNull(ctx context.Context, serviceID, externalID string, t time.Time) error {
	_, err := r.resources.SetFieldsOne(ctx,
		pgdoc.M{"serviceId": serviceID, "externalId": externalID, "createdAt": nil},
		pgdoc.M{"createdAt": t}, nil)
	return err
}

// matchByServiceExternal is the (externalId == , serviceId ∈ [serviceId]) filter. The
// single-element $in matches serviceId against a one-element set.
func matchByServiceExternal(r *CloudResource) pgdoc.M {
	return pgdoc.M{
		"externalId": r.ExternalID,
		"serviceId":  pgdoc.M{"$in": []string{r.ServiceID}},
	}
}

// Insert is the upsert path: a row-locked find on {externalId, serviceId∈[id]} then
// update-or-insert, in one transaction. Creates or replaces the matching doc and returns
// the post-image. createdAt is written only on the insert leg (immutable thereafter).
func (r *Repo) Insert(ctx context.Context, res *CloudResource) (*CloudResource, error) {
	if err := r.ensure(ctx); err != nil {
		return nil, err
	}
	var out CloudResource
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		var cur CloudResource
		id, found, err := r.resources.FindOneForUpdate(tc, matchByServiceExternal(res), &cur)
		if err != nil {
			return err
		}
		if found {
			if _, err := r.resources.SetByID(tc, id, toDbRecord(res), nil); err != nil {
				return err
			}
		} else {
			doc := toDbRecord(res)
			if res.CreatedAt != nil {
				doc["createdAt"] = res.CreatedAt
			}
			if id, err = r.resources.InsertOne(tc, doc); err != nil {
				return err
			}
		}
		_, err = r.resources.Get(tc, id, &out)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Update is the optimistic-concurrency path: the same filter, row-locked, PLUS the
// stored-updatedAt ≤ incoming-updatedAt guard (compared in Go), NO insert. Returns
// (nil,nil) when no doc matches or a newer doc already exists (the OCC guard rejected the
// write) so the caller can fall back to re-reading the latest.
func (r *Repo) Update(ctx context.Context, res *CloudResource) (*CloudResource, error) {
	if err := r.ensure(ctx); err != nil {
		return nil, err
	}
	var out *CloudResource
	err := r.db.WithTx(ctx, func(tc context.Context) error {
		var cur CloudResource
		id, found, err := r.resources.FindOneForUpdate(tc, matchByServiceExternal(res), &cur)
		if err != nil || !found {
			return err
		}
		// OCC guard: proceed only when stored updatedAt ≤ incoming. An incoming write without a
		// timestamp is rejected. A stored row without one is older than any read: rejecting it
		// would leave the row out of every sync, for good.
		if res.UpdatedAt == nil || (cur.UpdatedAt != nil && cur.UpdatedAt.After(*res.UpdatedAt)) {
			return nil // OCC reject (DB newer) — caller re-reads latest
		}
		if _, err := r.resources.SetByID(tc, id, toDbRecord(res), nil); err != nil {
			return err
		}
		var post CloudResource
		if _, err := r.resources.Get(tc, id, &post); err != nil {
			return err
		}
		out = &post
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ChangeExternalID follows a resource whose provider id changed, keeping the row and so its id. A row
// a sync already made for the new id goes: it is the same resource seen too early.
func (r *Repo) ChangeExternalID(ctx context.Context, res *CloudResource, externalID string) error {
	return r.db.WithTx(ctx, func(tc context.Context) error {
		if _, err := r.resources.DeleteMany(tc, pgdoc.M{"externalId": externalID, "serviceId": res.ServiceID, "_id": pgdoc.M{"$ne": res.ID}}); err != nil {
			return err
		}
		if _, err := r.resources.SetByID(tc, res.ID, pgdoc.M{"externalId": externalID}, nil); err != nil {
			return err
		}
		res.ExternalID = externalID
		return nil
	})
}

// SetDataSince records how old the resource's data is (CloudResource.DataSince).
func (r *Repo) SetDataSince(ctx context.Context, res *CloudResource, t time.Time) error {
	if _, err := r.resources.SetByID(ctx, res.ID, pgdoc.M{"dataSince": t}, nil); err != nil {
		return err
	}
	res.DataSince = &t
	return nil
}

// FindByServiceIDAndExternalID returns the cached resource, or (nil,nil).
func (r *Repo) FindByServiceIDAndExternalID(ctx context.Context, serviceID, externalID string) (*CloudResource, error) {
	var out CloudResource
	found, err := r.resources.FindOne(ctx, pgdoc.M{"serviceId": serviceID, "externalId": externalID}, &out)
	if err != nil || !found {
		return nil, err
	}
	return &out, nil
}

// FindByID looks a cloud resource up by id — returns
// (nil,nil) when absent (the admin /cloud-resource/{id} maps that to an empty {} envelope, NOT 404).
func (r *Repo) FindByID(ctx context.Context, id string) (*CloudResource, error) {
	var out CloudResource
	found, err := r.resources.Get(ctx, id, &out)
	if err != nil || !found {
		return nil, err
	}
	return &out, nil
}

// ExistsByServiceIDAndExternalID reports whether a resource with that service + external id is cached.
func (r *Repo) ExistsByServiceIDAndExternalID(ctx context.Context, serviceID, externalID string) (bool, error) {
	return r.resources.Exists(ctx, pgdoc.M{"serviceId": serviceID, "externalId": externalID})
}

// FindAllByProjectID / FindAllByUserID return every cached resource for a project or user.
func (r *Repo) FindAllByProjectID(ctx context.Context, projectID string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"projectId": projectID})
}

func (r *Repo) FindAllByUserID(ctx context.Context, userID string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"userId": userID})
}

// PageByProjectAndType keyset-pages a project's cached resources (optionally one type), cursor on
// _id / newest-first — the BE-paged cloud-resource list (SERVER/VOLUME/… at scale).
func (r *Repo) PageByProjectAndType(ctx context.Context, projectID, resourceType string, p paging.Params) ([]CloudResource, *string, *string, error) {
	filter := pgdoc.M{"projectId": projectID}
	if resourceType != "" {
		filter["type"] = resourceType
	}
	return paging.Keyset(ctx, r.resources, filter, p, func(c CloudResource) string { return c.ID })
}

// PageByUserAndType keyset-pages a user-scoped resource type (KEYPAIR carries userId, blank projectId).
func (r *Repo) PageByUserAndType(ctx context.Context, userID, resourceType string, p paging.Params) ([]CloudResource, *string, *string, error) {
	filter := pgdoc.M{"userId": userID}
	if resourceType != "" {
		filter["type"] = resourceType
	}
	return paging.Keyset(ctx, r.resources, filter, p, func(c CloudResource) string { return c.ID })
}

// FindByProjectAndService returns the
// resources a service's billing pass iterates.
func (r *Repo) FindByProjectAndService(ctx context.Context, projectID, serviceID string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"projectId": projectID, "serviceId": serviceID})
}

// FindByProjectAndType returns a project's cloud resources of one type (the metrics job
// reads SERVER + PORT per project).
func (r *Repo) FindByProjectAndType(ctx context.Context, projectID, resourceType string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"projectId": projectID, "type": resourceType})
}

// FindByServiceAndType returns the cached resources of one type for a service — the set a sync
// pass reconciles against the live cloud list (to detect resources that vanished).
func (r *Repo) FindByServiceAndType(ctx context.Context, serviceID, resourceType string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"serviceId": serviceID, "type": resourceType})
}

// FindByServiceProjectAndType is the PROJECT-scoped vanished-scan (filter projectId +
// serviceId + type). Reconcile uses this instead of FindByServiceAndType so a project's sync only
// ever delete-archives ITS OWN cached resources — two projects sharing a serviceId no longer clobber
// each other (the cross-project delete leak in the audit §5).
func (r *Repo) FindByServiceProjectAndType(ctx context.Context, serviceID, projectID, resourceType string) ([]CloudResource, error) {
	return r.findAll(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID, "type": resourceType})
}

func (r *Repo) findAll(ctx context.Context, filter pgdoc.M) ([]CloudResource, error) {
	out := []CloudResource{}
	if err := r.resources.Find(ctx, filter, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAndArchive hard-deletes the resource from `cloudResource` then archives it into
// `cloudResourceHistory`. The archive is idempotent per
// cloudResourceId. `now` is the archive (deletedAt) timestamp — pass time.Now().UTC().
func (r *Repo) DeleteAndArchive(ctx context.Context, res *CloudResource, now time.Time) error {
	if res.ID != "" {
		if _, err := r.resources.DeleteByID(ctx, res.ID); err != nil {
			return err
		}
	}
	return r.archive(ctx, res, now)
}

// archive writes a one-time history copy:
// no-op if a record already exists for this cloudResourceId; copies only
// cloudResourceId/region/serviceId/type/data/createdAt/externalId/projectId + deletedAt.
func (r *Repo) archive(ctx context.Context, res *CloudResource, now time.Time) error {
	exists, err := r.history.Exists(ctx, pgdoc.M{"cloudResourceId": res.ID})
	if err != nil {
		return err
	}
	if exists {
		return nil // idempotent per cloudResourceId
	}
	_, err = r.history.InsertOne(ctx, History{
		CloudResourceID: res.ID,
		Region:          res.Region,
		ServiceID:       res.ServiceID,
		Type:            res.Type,
		Data:            res.Data,
		CreatedAt:       res.CreatedAt,
		ExternalID:      res.ExternalID,
		ProjectID:       res.ProjectID,
		DeletedAt:       &now,
	})
	return err
}

// WasUserDeletedAfter is the recreation guard: true iff the
// most-recent history record for (serviceId, externalId) was deleted strictly AFTER the
// sync snapshot time — i.e. the user deleted it after the cloud snapshot, so a stale sync
// must not recreate it. nil snapshot → false.
func (r *Repo) WasUserDeletedAfter(ctx context.Context, serviceID, externalID string, snapshotAt *time.Time) (bool, error) {
	if snapshotAt == nil {
		return false, nil
	}
	var h History
	found, err := r.history.FindOne(ctx, pgdoc.M{"serviceId": serviceID, "externalId": externalID}, &h,
		pgdoc.Sort(pgdoc.DescK("deletedAt", pgdoc.KTime)))
	if err != nil || !found {
		return false, err
	}
	return h.DeletedAt != nil && h.DeletedAt.After(*snapshotAt), nil
}

// typeCounts aggregates count-by-type for one scoping field (projectId/userId — internal
// callers only, never user input). Never nil.
func (r *Repo) typeCounts(ctx context.Context, field, value string) (map[string]int64, error) {
	// The table appears on first write; make counts on a fresh DB read as empty, not error.
	if err := r.resources.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := r.db.Pool.Query(ctx,
		`SELECT doc->>'type', count(*) FROM "cloudResource" WHERE doc->>'`+field+`' = $1 GROUP BY 1`, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var typ *string
		var n int64
		if err := rows.Scan(&typ, &n); err != nil {
			return nil, err
		}
		t := ""
		if typ != nil {
			t = *typ
		}
		out[t] = n
	}
	return out, rows.Err()
}

// CountsForProject returns per-type counts for a project (raw, no adjustments) — backs
// ProjectStats.
func (r *Repo) CountsForProject(ctx context.Context, projectID string) (map[string]int64, error) {
	return r.typeCounts(ctx, "projectId", projectID)
}

// CountsForUser returns per-type counts for a user's identity resources (keypairs,
// app-credentials, users, credentials).
func (r *Repo) CountsForUser(ctx context.Context, userID string) (map[string]int64, error) {
	return r.typeCounts(ctx, "userId", userID)
}

// CountByType groups by
// (type, serviceId), subtracts 1 from each SECURITY_GROUP group (the default sg), sums by
// type, then add "TOTAL" = sum of all. Empty project → {"TOTAL":0}.
func (r *Repo) CountByType(ctx context.Context, projectID string) (map[string]int64, error) {
	if err := r.resources.Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := r.db.Pool.Query(ctx,
		`SELECT doc->>'type', count(*) FROM "cloudResource" WHERE doc->>'projectId' = $1`+
			` GROUP BY doc->>'type', doc->>'serviceId'`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counter := map[string]int64{}
	var total int64
	for rows.Next() {
		var typ *string
		var c int64
		if err := rows.Scan(&typ, &c); err != nil {
			return nil, err
		}
		t := ""
		if typ != nil {
			t = *typ
		}
		if t == TypeSecurityGroup {
			c-- // the always-present default security group is not counted
		}
		counter[t] += c
		total += c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counter["TOTAL"] = total
	return counter, nil
}
