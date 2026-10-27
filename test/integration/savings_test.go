//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// TestExpireSavingsContracts exercises expiring savings contracts: only ACTIVE
// contracts whose endDate is strictly before now flip to EXPIRED; ACTIVE-future and already
// non-ACTIVE contracts are untouched.
func TestExpireSavingsContracts(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	now := time.Now().UTC()
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)

	expiredID := mustInsertID(t, db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusActive, "billingProfileId": "bp1", "endDate": past,
	})
	activeFutureID := mustInsertID(t, db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusActive, "billingProfileId": "bp1", "endDate": future,
	})
	alreadyExpiredID := mustInsertID(t, db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusExpired, "billingProfileId": "bp1", "endDate": past,
	})
	cancelledID := mustInsertID(t, db, "savingsContract", pgdoc.M{
		"status": billing.SavingsStatusCancelled, "billingProfileId": "bp1", "endDate": past,
	})

	svc := billing.NewSavingsService(billing.NewRepo(db))
	n, err := svc.ExpireContracts(ctx)
	if err != nil {
		t.Fatalf("ExpireContracts: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired count = %d, want 1", n)
	}

	wantStatus := map[string]string{
		expiredID:        billing.SavingsStatusExpired,   // ACTIVE + past → flipped
		activeFutureID:   billing.SavingsStatusActive,    // ACTIVE + future → untouched
		alreadyExpiredID: billing.SavingsStatusExpired,   // already EXPIRED → untouched
		cancelledID:      billing.SavingsStatusCancelled, // CANCELLED → untouched
	}
	for id, want := range wantStatus {
		var got struct {
			Status string `json:"status"`
		}
		found, err := db.C("savingsContract").FindOne(ctx, pgdoc.M{"_id": id}, &got)
		if err != nil {
			t.Fatalf("reread %s: %v", id, err)
		}
		if !found {
			t.Fatalf("reread %s: not found", id)
		}
		if got.Status != want {
			t.Fatalf("contract %s status = %s, want %s", id, got.Status, want)
		}
	}

	// idempotent: a second run flips nothing.
	if n2, err := svc.ExpireContracts(ctx); err != nil || n2 != 0 {
		t.Fatalf("second run = %d (err %v), want 0", n2, err)
	}
}
