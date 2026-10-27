//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// createdAt is immutable: stamped once on insert, never nulled or drifted
// by later re-caches (sync updates, notification ingest, action re-inserts), and healed when a
// pre-fix writer left it null.
func TestCloudCreatedAtImmutableAndHealed(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := cloud.NewRepo(db)
	base := time.Now().UTC().Truncate(time.Millisecond)

	// Insert with a creation stamp.
	orig := base.Add(-2 * time.Hour)
	first := rcRes("X", map[string]any{"name": "x1"})
	first.ServiceID = "svc1"
	first.CreatedAt, first.UpdatedAt = &orig, &orig
	if _, err := repo.Insert(ctx, &first); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Re-cache (notification-style upsert) with a LATER CreatedAt → must NOT drift.
	later := base.Add(-time.Hour)
	second := rcRes("X", map[string]any{"name": "x2"})
	second.ServiceID = "svc1"
	second.CreatedAt, second.UpdatedAt = &later, &later
	saved, err := repo.Insert(ctx, &second)
	if err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if saved.CreatedAt == nil || !saved.CreatedAt.Equal(orig) {
		t.Fatalf("createdAt drifted: want %v, got %v", orig, saved.CreatedAt)
	}

	// Re-cache with a NIL CreatedAt (the pre-fix nulling writer) → must stay orig, not null.
	third := rcRes("X", map[string]any{"name": "x3"})
	third.ServiceID = "svc1"
	third.UpdatedAt = &base
	saved, err = repo.Insert(ctx, &third)
	if err != nil {
		t.Fatalf("nil-createdAt re-insert: %v", err)
	}
	if saved.CreatedAt == nil || !saved.CreatedAt.Equal(orig) {
		t.Fatalf("createdAt nulled/changed by nil writer: got %v", saved.CreatedAt)
	}

	// Legacy null doc + a sync pass → Reconcile heals createdAt from updatedAt.
	nulledAt := base.Add(-30 * time.Minute)
	if _, err := repo.Insert(ctx, &cloud.CloudResource{
		ExternalID: "Y", ServiceID: "svc1", ProjectID: "proj1", Type: cloud.TypeServer,
		Region: "RegionOne", Data: map[string]any{"name": "y1"}, UpdatedAt: &nulledAt,
	}); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	// Force createdAt to null the way pre-fix writers persisted it.
	if _, err := db.C("cloudResource").SetFieldsOne(ctx,
		pgdoc.M{"serviceId": "svc1", "externalId": "Y"},
		pgdoc.M{"createdAt": nil}, nil); err != nil {
		t.Fatalf("force null: %v", err)
	}
	p := &reconcileProv{typ: cloud.TypeServer, list: []cloud.CloudResource{
		func() cloud.CloudResource { r := rcRes("X", map[string]any{"name": "x3"}); return r }(),
		func() cloud.CloudResource { r := rcRes("Y", map[string]any{"name": "y1"}); return r }(),
	}}
	if _, err := providers.Reconcile(ctx, p, repo, "svc1", base.Add(time.Minute)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	y, _ := repo.FindByServiceIDAndExternalID(ctx, "svc1", "Y")
	if y == nil || y.CreatedAt == nil || !y.CreatedAt.Equal(nulledAt) {
		t.Fatalf("legacy null not healed from updatedAt: %+v", y)
	}
}
