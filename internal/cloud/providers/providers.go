// Package providers is the CloudResource provider layer:
// each provider lists its cloud resource type and maps the cloud
// object to a CloudResource, and Sync upserts them into the cache. This read-sync
// populates the `cloudResource` collection the metrics job + rating loop read from;
// Reconcile adds the createOrUpdate gates (shouldBeDeleted / isNeededToUpdate /
// delete-missing).
package providers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// Provider is the read interface for one CloudResourceType: list the cloud objects of this type
// (already mapped to CloudResource — externalId/type/region/data set; serviceId is stamped
// by Sync).
type Provider interface {
	Type() string
	List(ctx context.Context) ([]cloud.CloudResource, error)
}

// ProjectScoped is an optional Provider capability: the ProxCloud project id this provider syncs for.
// Reconcile uses it to scope the delete-of-vanished scan to (serviceId, projectId, type)
// — so a project's sync never delete-archives ANOTHER project's cached resources
// that happen to share the serviceId. A provider that doesn't implement it keeps the old
// (serviceId, type)-only scan (leaky if serviceIds are shared across projects).
type ProjectScoped interface {
	ProjectID() string
}

// Deletable is an optional Provider capability (shouldBeDeleted,
// checked FIRST in createOrUpdate): a cached resource in a terminal/inconsistent state is
// delete-archived even though the cloud still lists it (e.g. a server in status DELETED).
type Deletable interface {
	ShouldBeDeleted(cr *cloud.CloudResource) bool
}

// Replaceable is an optional Provider capability: the live object under a cached row's externalId
// is a different object (PVE hands out the lowest free vmid again). Reconcile archives the old row
// and inserts a new one, so the new object gets its own id, createdAt and bill item.
type Replaceable interface {
	Superseded(cached, fresh *cloud.CloudResource) bool
}

// VanishGrace is an optional Provider capability for clouds whose list lags their writes (pmxcfs):
// a row younger than the grace is not archived for being absent, and a row archived less than the
// grace ago is not resurrected for being still listed.
type VanishGrace interface {
	VanishGrace() time.Duration
}

// UnreadKeeper is an optional Provider capability for a cloud that can list an object without being able to read it
// (its node is down): such a cached row is not gone, however absent from List. The provider may change the row's data
// in place; changed says to store it.
type UnreadKeeper interface {
	KeepUnread(cached *cloud.CloudResource) (keep, changed bool)
}

// CacheMerger is an optional Provider capability: before comparing, carry over what the fresh read did not
// know from the cached row (a RustFS bucket whose usage the scanner could not answer).
type CacheMerger interface {
	MergeCached(cached, fresh map[string]any)
}

// Sync lists a provider's cloud resources and upserts each into the cache (blind upsert, no
// reconciliation). Superseded by Reconcile (which adds the gates + delete-of-vanished); kept
// for callers that want a plain upsert. `now` stamps created/updatedAt.
func Sync(ctx context.Context, p Provider, repo *cloud.Repo, serviceID string, now time.Time) (int, error) {
	list, err := p.List(ctx)
	if err != nil {
		return 0, err
	}
	for i := range list {
		r := list[i]
		r.ServiceID = serviceID
		if r.Type == "" {
			r.Type = p.Type()
		}
		t := now
		r.UpdatedAt = &t
		if r.CreatedAt == nil {
			c := now
			r.CreatedAt = &c
		}
		if _, err := repo.Insert(ctx, &r); err != nil {
			return i, err
		}
	}
	return len(list), nil
}

// ReconcileStats reports what a Reconcile pass changed.
type ReconcileStats struct{ Created, Updated, Deleted int }

// Reconcile is the full sync reconcile (createOrUpdate gates +
// delete-of-vanished), replacing Sync's blind upsert:
//   - new in cloud, not cached → Insert (skipped if the user deleted it AFTER this snapshot —
//     the wasUserDeletedAfter recreate guard);
//   - cached + cloud differs → Update via the optimistic ts-guard (a newer DB doc wins);
//     unchanged → skipped (isNeededToUpdate);
//   - cached for this (service,type) but absent from the live list → DeleteAndArchive.
//
// `now` is the sync snapshot time (stamped on writes + the delete-guard cutoff).
func Reconcile(ctx context.Context, p Provider, repo *cloud.Repo, serviceID string, now time.Time) (ReconcileStats, error) {
	var st ReconcileStats
	list, err := p.List(ctx)
	if err != nil {
		return st, err
	}
	var grace time.Duration
	if g, ok := p.(VanishGrace); ok {
		grace = g.VanishGrace()
	}
	var merge func(cached, fresh map[string]any)
	if m, ok := p.(CacheMerger); ok {
		merge = m.MergeCached
	}
	insert := func(r *cloud.CloudResource) error {
		t := now
		r.UpdatedAt = &t
		if r.CreatedAt == nil {
			c := now
			r.CreatedAt = &c
		}
		if _, err := repo.Upsert(ctx, r, merge); err != nil {
			return err
		}
		st.Created++
		return nil
	}
	fresh := make(map[string]bool, len(list))
	for i := range list {
		r := list[i]
		r.ServiceID = serviceID
		if r.Type == "" {
			r.Type = p.Type()
		}
		fresh[r.ExternalID] = true

		cached, err := repo.FindByServiceIDAndExternalID(ctx, serviceID, r.ExternalID)
		if err != nil {
			return st, err
		}
		if cached == nil {
			// Recreate guard: if the user deleted this resource after the sync snapshot, don't
			// resurrect it from a stale cloud read.
			guardAt := now.Add(-grace)
			guard, err := repo.WasUserDeletedAfter(ctx, serviceID, r.ExternalID, &guardAt)
			if err != nil {
				return st, err
			}
			if guard {
				continue
			}
			if err := insert(&r); err != nil {
				return st, err
			}
			continue
		}
		if rp, ok := p.(Replaceable); ok && rp.Superseded(cached, &r) {
			if err := repo.DeleteAndArchive(ctx, cached, now); err != nil {
				return st, err
			}
			st.Deleted++
			if err := insert(&r); err != nil {
				return st, err
			}
			continue
		}
		// shouldBeDeleted gate: a cached resource the provider
		// declares terminal/inconsistent is deleted even though the cloud listed it.
		if del, ok := p.(Deletable); ok && del.ShouldBeDeleted(cached) {
			if err := repo.DeleteAndArchive(ctx, cached, now); err != nil {
				return st, err
			}
			st.Deleted++
			continue
		}
		// ts-guard: a DB doc at/after the snapshot is newer than this read → leave it.
		if cached.UpdatedAt != nil && !cached.UpdatedAt.Before(now) {
			continue
		}
		// Heal docs whose createdAt was nulled by pre-$setOnInsert writers (best-effort — the UI
		// Created-At column + createdAt billing accrual read it). updatedAt is the closest
		// surviving evidence of age; absent that, now.
		if cached.CreatedAt == nil {
			heal := now
			if cached.UpdatedAt != nil {
				heal = *cached.UpdatedAt
			}
			_ = repo.StampCreatedAtIfNull(ctx, serviceID, r.ExternalID, heal)
		}
		// One-time heal: a doc cached before its provider supplied the real cloud created_at (Info)
		// gets it stamped so billing accrues from true age, not first-sync time. Bypass the
		// unchanged-data skip until Info.CreatedAt lands (self-limiting — once set, this is false).
		infoHeal := r.Info != nil && r.Info.CreatedAt != nil && (cached.Info == nil || cached.Info.CreatedAt == nil)
		if m, ok := p.(CacheMerger); ok {
			m.MergeCached(cached.Data, r.Data)
		}
		// isNeededToUpdate: skip the write when the cloud object is unchanged (unless healing Info). A
		// KeyedComparer provider uses the per-key compareMaps (number-width + list-order tolerant);
		// others fall back to the whole-data JSON compare.
		if !infoHeal {
			if kc, ok := p.(KeyedComparer); ok {
				if dataEqualKeyed(cached.Data, r.Data, kc.CompareKeys()) {
					continue
				}
			} else if dataEqual(cached.Data, r.Data) {
				continue
			}
		}
		t := now
		r.UpdatedAt = &t
		if r.CreatedAt == nil {
			r.CreatedAt = cached.CreatedAt
		}
		if _, err := repo.Update(ctx, &r); err != nil { // OCC ts-guard; nil result = DB newer (skipped)
			return st, err
		}
		st.Updated++
	}

	// delete-of-vanished: cached resources of this type no longer present in the cloud. Scope the
	// scan to this project when the provider exposes its projectId (the deletion scan is
	// projectId+serviceId+type-scoped) — otherwise a shared serviceId would let this project's sync
	// delete-archive another project's cached resources. Falls back to the serviceId+type scan for
	// providers that don't declare a project.
	var cachedAll []cloud.CloudResource
	if ps, ok := p.(ProjectScoped); ok && ps.ProjectID() != "" {
		cachedAll, err = repo.FindByServiceProjectAndType(ctx, serviceID, ps.ProjectID(), p.Type())
	} else {
		cachedAll, err = repo.FindByServiceAndType(ctx, serviceID, p.Type())
	}
	if err != nil {
		return st, err
	}
	for i := range cachedAll {
		cr := cachedAll[i]
		if fresh[cr.ExternalID] {
			continue
		}
		if grace > 0 && cr.CreatedAt != nil && now.Sub(*cr.CreatedAt) < grace {
			continue
		}
		// Gone under its old id because the portal is moving it: the move carries the row over to the new one.
		if cr.Moving(now) {
			continue
		}
		if k, ok := p.(UnreadKeeper); ok {
			if keep, changed := k.KeepUnread(&cr); keep {
				if changed {
					t := now
					cr.UpdatedAt = &t
					if _, err := repo.Update(ctx, &cr); err != nil {
						return st, err
					}
					st.Updated++
				}
				continue
			}
		}
		if err := repo.DeleteAndArchive(ctx, &cr, now); err != nil {
			return st, err
		}
		st.Deleted++
	}
	return st, nil
}

// dataEqual reports whether two free-form `data` sub-docs are equivalent, comparing canonical
// JSON so a datastore round-trip (pgdoc.M/pgdoc.A) matches a freshly-built map
// (isNeededToUpdate, approximated by a whole-data compare). Marshal errors → treat as differ.
func dataEqual(a, b map[string]any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
