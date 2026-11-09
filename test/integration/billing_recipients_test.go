//go:build integration

package integration

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestBillingMailsGoToAllRecipients: bill_is_generated (send) and bill_is_paid (pay-by-credits) go
// to the profile e-mail plus every billing contact, de-duplicated case-insensitively.
func TestBillingMailsGoToAllRecipients(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	repo, prepo := billing.NewRepo(db), pricing.NewRepo(db)

	profile := &billing.BillingProfile{
		ID: "bp-1", Currency: "USD", Email: "ann@acme.sg", FirstName: "Ann", LastName: "Lee", Status: billing.StatusActive,
		Contacts: []billing.Contact{{Name: "AP", Email: "ap@acme.sg"}, {Email: "Ann@Acme.sg"}, {Name: "CFO", Email: "cfo@acme.sg"}},
	}
	want := []string{"ann@acme.sg", "ap@acme.sg", "cfo@acme.sg"}
	now := time.Date(2026, 9, 1, 0, 0, 30, 0, time.UTC)
	bill := func(status string) string {
		return mustInsertID(t, db, "bill", pgdoc.M{
			"billingProfileId": profile.ID, "status": status, "invoiceCurrency": "USD",
			"billingCycle": pgdoc.M{"startDate": time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), "endDate": now.Truncate(24 * time.Hour)},
			"items":        []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}},
		})
	}

	// bill_is_generated — the monthly send of an OPEN bill with an unpaid remainder.
	send := billing.NewBillSendService(repo, prepo)
	sent := &captureNotifier{}
	send.SetNotifier(sent)
	bill("OPEN")
	if ok, err := send.SendBillForProfile(ctx, profile, now); err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	if len(sent.calls) != 1 || sent.calls[0].key != "bill_is_generated" || !slices.Equal(sent.calls[0].to, want) {
		t.Fatalf("bill_is_generated mail: %+v", sent.calls)
	}

	// bill_is_paid — the client pays a SENT bill from promotional credit.
	pay := billing.NewPayService(repo, prepo)
	paid := &captureNotifier{}
	pay.SetNotifier(paid)
	mustInsert(t, db, "promotionalCredit", pgdoc.M{"billingProfileId": profile.ID, "remainingAmount": decimalOf(t, "10"),
		"expirationDate": time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)})
	if got, err := pay.PayBillWithCredits(ctx, profile, bill("SENT"), now); err != nil || got.Status != pricing.BillStatusPaid {
		t.Fatalf("pay: status=%v err=%v", got, err)
	}
	if len(paid.calls) != 1 || paid.calls[0].key != "bill_is_paid" || !slices.Equal(paid.calls[0].to, want) {
		t.Fatalf("bill_is_paid mail: %+v", paid.calls)
	}
}
