//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// A profile an operator activated before activation set the currency stays ACTIVE with none, and
// every bill read of it failed. Reading it fills the base currency and stores it once; a profile
// that is not ACTIVE, or has its own currency, is left alone.
func TestBillingProfileCurrencyBackfill(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp-blank", "status": "ACTIVE"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp-new", "status": "NEW"})
	mustInsert(t, db, "billingProfile", pgdoc.M{"_id": "bp-eur", "status": "ACTIVE", "currency": "EUR"})
	repo := billing.NewRepo(db)

	stored := func(id string) string {
		var doc pgdoc.M
		_, _ = db.C("billingProfile").FindOne(ctx, pgdoc.M{"_id": id}, &doc)
		s, _ := doc["currency"].(string)
		return s
	}
	bp, err := repo.FindByID(ctx, "bp-blank")
	if err != nil || bp == nil || bp.Currency != "USD" {
		t.Fatalf("FindByID = %+v, %v", bp, err)
	}
	if stored("bp-blank") != "USD" {
		t.Fatalf("currency not stored: %q", stored("bp-blank"))
	}
	all, err := repo.AllBillingProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, p := range all {
		got[p.ID] = p.Currency
	}
	if got["bp-new"] != "" || got["bp-eur"] != "EUR" || stored("bp-new") != "" || stored("bp-eur") != "EUR" {
		t.Fatalf("left-alone profiles changed: read %v, stored new=%q eur=%q", got, stored("bp-new"), stored("bp-eur"))
	}
}
