//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// ActivationService.Activate (ADMIN source) flips a NEW profile to ACTIVE, stamps the passed
// constraint, and mints the configured provisioning promotional credits. A non-NEW profile is
// a no-op ("no effect for other statuses").
func TestActivationService_Activate(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)

	// billingConfiguration with a provisioning promotional (10 USD / 30 days).
	if _, err := db.C("billingConfiguration").InsertOne(ctx, pgdoc.M{
		"baseCurrency": "USD",
		"provisioningSettings": pgdoc.M{
			"promotionals": []any{pgdoc.M{"amount": 10, "daysValidity": 30}},
		},
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	bpID := pgdoc.NewID()
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "status": "NEW", "email": "new@demo", "currency": "USD",
	}); err != nil {
		t.Fatalf("seed bp: %v", err)
	}

	svc := billing.NewActivationService(repo, nil, nil)
	bp, err := repo.FindByID(ctx, bpID)
	if err != nil || bp == nil {
		t.Fatalf("load bp: %v", err)
	}
	if err := svc.Activate(ctx, bp, billing.SourceAdmin); err != nil {
		t.Fatalf("activate: %v", err)
	}
	got, _ := repo.FindByID(ctx, bpID)
	if got.Status != billing.StatusActive {
		t.Fatalf("status: want ACTIVE, got %s", got.Status)
	}
	if got.ActivatedAt == nil || len(got.ActivationConstraints) != 1 || got.ActivationConstraints[0].Source != billing.SourceAdmin {
		t.Fatalf("activation constraint not stamped: %+v", got.ActivationConstraints)
	}
	n, err := db.C("promotionalCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID})
	if err != nil || n != 1 {
		t.Fatalf("provisioning promo credit: want 1, got %d (%v)", n, err)
	}

	// Re-activate → no-op (already ACTIVE): no second promo credit.
	if err := svc.Activate(ctx, got, billing.SourceAdmin); err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if n, _ := db.C("promotionalCredit").Count(ctx, pgdoc.M{"billingProfileId": bpID}); n != 1 {
		t.Fatalf("re-activate minted a duplicate credit: %d", n)
	}
}

// Suspend/Unsuspend flip the profile status and are best-effort on the cloud leg (nil clouds).
func TestActivationService_SuspendResume(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	bpID := pgdoc.NewID()
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "status": "ACTIVE", "email": "a@demo", "currency": "USD",
	}); err != nil {
		t.Fatalf("seed bp: %v", err)
	}
	svc := billing.NewActivationService(repo, nil, nil)
	bp, _ := repo.FindByID(ctx, bpID)
	if err := svc.Suspend(ctx, bp, billing.SourceAdmin); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if got, _ := repo.FindByID(ctx, bpID); got.Status != billing.StatusSuspended {
		t.Fatalf("want SUSPENDED, got %s", got.Status)
	}
	if err := svc.Unsuspend(ctx, bp, billing.SourceAdmin); err != nil {
		t.Fatalf("unsuspend: %v", err)
	}
	if got, _ := repo.FindByID(ctx, bpID); got.Status != billing.StatusActive {
		t.Fatalf("want ACTIVE, got %s", got.Status)
	}
}
