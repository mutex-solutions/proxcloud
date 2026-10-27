//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// captureNotifier records SendTemplate calls (stands in for mail.Service).
type captureNotifier struct {
	calls []struct {
		key  string
		to   []string
		vars map[string]any
	}
}

func (c *captureNotifier) SendTemplate(_ context.Context, key string, to []string, vars map[string]any) error {
	c.calls = append(c.calls, struct {
		key  string
		to   []string
		vars map[string]any
	}{key, to, vars})
	return nil
}

// TestSavingsExpiredNotifies verifies the mail wiring: when a savings contract expires, the
// SavingsService sends the savings_contract_expired template to the profile's email.
func TestSavingsExpiredNotifies(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)

	const bpID = "6a37e2f547f3b4378ba78549"
	if _, err := db.C("billingProfile").InsertOne(ctx, pgdoc.M{
		"_id": bpID, "email": "ada@example.com", "firstName": "Ada", "lastName": "Lovelace", "currency": "USD",
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	// an ACTIVE contract whose endDate is in the past → should expire + notify.
	if _, err := db.C("savingsContract").InsertOne(ctx, pgdoc.M{
		"billingProfileId": bpID, "status": "ACTIVE",
		"savingsPlanName": "Gold", "endDate": time.Now().UTC().AddDate(0, 0, -1),
	}); err != nil {
		t.Fatalf("seed contract: %v", err)
	}

	cap := &captureNotifier{}
	svc := billing.NewSavingsService(repo)
	svc.SetNotifier(cap)

	n, err := svc.ExpireContracts(ctx)
	if err != nil || n != 1 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(cap.calls))
	}
	c := cap.calls[0]
	if c.key != "savings_contract_expired" || len(c.to) != 1 || c.to[0] != "ada@example.com" {
		t.Fatalf("notification wrong: key=%s to=%v", c.key, c.to)
	}
	if c.vars["fullName"] != "Ada Lovelace" || c.vars["savingsPlanName"] != "Gold" {
		t.Fatalf("vars wrong: %+v", c.vars)
	}
}
