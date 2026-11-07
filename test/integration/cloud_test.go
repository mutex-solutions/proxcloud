//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// TestCloudResourcePersistence exercises the sync layer against real Postgres: the
// upsert insert, the optimistic-concurrency update (fresh applied / stale rejected), the
// delete→archive (idempotent), the wasUserDeletedAfter recreation guard, and the free-form
// `data` round-trip.
func TestCloudResourcePersistence(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))

	base := time.Now().UTC().Truncate(time.Millisecond) // store keeps ms; avoid ns mismatch
	t1, t2, t0 := base, base.Add(time.Second), base.Add(-time.Second)

	res := &cloud.CloudResource{
		ProjectID: "proj1", UserID: "user1", ServiceID: "svc1", ExternalID: "ext1",
		Type: cloud.TypeServer, Region: "RegionOne",
		Data:      map[string]any{"name": "vm-a", "vcpus": int32(2)},
		CreatedAt: &t1, UpdatedAt: &t1,
	}

	// 1. Insert (upsert) → post-image with generated _id + data round-trip.
	got, err := repo.Insert(ctx, res)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got.ID == "" {
		t.Fatal("insert: expected generated _id")
	}
	if got.Data["name"] != "vm-a" {
		t.Fatalf("insert: data round-trip, got %v", got.Data)
	}
	id := got.ID

	// 2. Find + Exists.
	found, err := repo.FindByServiceIDAndExternalID(ctx, "svc1", "ext1")
	if err != nil || found == nil {
		t.Fatalf("find: %v / %v", err, found)
	}
	if ok, _ := repo.ExistsByServiceIDAndExternalID(ctx, "svc1", "ext1"); !ok {
		t.Fatal("exists: want true")
	}

	// 3. OCC update with a FRESH (later) timestamp → applies.
	fresh := &cloud.CloudResource{
		ServiceID: "svc1", ExternalID: "ext1", Type: cloud.TypeServer, Region: "RegionOne",
		ProjectID: "proj1", UserID: "user1", CreatedAt: &t1, UpdatedAt: &t2,
		Data: map[string]any{"name": "vm-a2"},
	}
	out, err := repo.Update(ctx, fresh)
	if err != nil {
		t.Fatalf("update fresh: %v", err)
	}
	if out == nil || out.Data["name"] != "vm-a2" {
		t.Fatalf("update fresh: expected applied post-image, got %v", out)
	}

	// 4. OCC update with a STALE (earlier) timestamp → rejected (DB now at t2 > t0).
	stale := &cloud.CloudResource{
		ServiceID: "svc1", ExternalID: "ext1", Type: cloud.TypeServer, UpdatedAt: &t0,
		Data: map[string]any{"name": "should-not-win"},
	}
	rej, err := repo.Update(ctx, stale)
	if err != nil {
		t.Fatalf("update stale: %v", err)
	}
	if rej != nil {
		t.Fatalf("update stale: expected OCC reject (nil), got %v", rej)
	}

	// 5. Delete + archive (idempotent).
	delAt := base.Add(2 * time.Second)
	if err := repo.DeleteAndArchive(ctx, &cloud.CloudResource{ID: id, ServiceID: "svc1", ExternalID: "ext1", Region: "RegionOne", Type: cloud.TypeServer, ProjectID: "proj1", CreatedAt: &t1}, delAt); err != nil {
		t.Fatalf("delete+archive: %v", err)
	}
	if gone, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "ext1"); gone != nil {
		t.Fatal("delete: resource should be gone from cloudResource")
	}
	// archive again → no-op (idempotent per cloudResourceId)
	if err := repo.DeleteAndArchive(ctx, &cloud.CloudResource{ID: id, ServiceID: "svc1", ExternalID: "ext1", CreatedAt: &t1}, delAt.Add(time.Second)); err != nil {
		t.Fatalf("re-archive: %v", err)
	}

	// 6. Recreation guard: snapshot BEFORE the delete → user deleted after snapshot → true.
	before := base.Add(time.Second)
	if w, err := repo.WasUserDeletedAfter(ctx, "svc1", "ext1", &before); err != nil || !w {
		t.Fatalf("wasUserDeletedAfter(before): want true, got %v (%v)", w, err)
	}
	// snapshot AFTER the delete → false (not deleted after the snapshot).
	after := base.Add(10 * time.Second)
	if w, _ := repo.WasUserDeletedAfter(ctx, "svc1", "ext1", &after); w {
		t.Fatal("wasUserDeletedAfter(after): want false")
	}
	// nil snapshot → false.
	if w, _ := repo.WasUserDeletedAfter(ctx, "svc1", "ext1", nil); w {
		t.Fatal("wasUserDeletedAfter(nil): want false")
	}
}

// A row written without updatedAt (share rows were) must still take sync updates, or its usage and addresses never move.
func TestCloudUpdateHealsRowWithoutTimestamp(t *testing.T) {
	ctx := context.Background()
	repo := cloud.NewRepo(freshPG(t))
	row := &cloud.CloudResource{ServiceID: "svc", ExternalID: "ext", Type: cloud.TypeShare, Data: map[string]any{"used": 0.0}}
	if _, err := repo.Insert(ctx, row); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	got, err := repo.Update(ctx, &cloud.CloudResource{ServiceID: "svc", ExternalID: "ext", Type: cloud.TypeShare, Data: map[string]any{"used": 5.0}, UpdatedAt: &now})
	if err != nil || got == nil || got.Data["used"] != 5.0 {
		t.Fatalf("got %+v, err %v", got, err)
	}
	if rej, err := repo.Update(ctx, &cloud.CloudResource{ServiceID: "svc", ExternalID: "ext", Type: cloud.TypeShare, Data: map[string]any{"used": 9.0}}); err != nil || rej != nil {
		t.Fatalf("a write without a timestamp was applied: %+v, err %v", rej, err)
	}
}
