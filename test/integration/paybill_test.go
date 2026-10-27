//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// TestPayBillWithCredits exercises paying a bill from credits only (no gateway):
// promo-pay → PAID, account-credit-pay → PAID, + the 3 guards (open/paid/insufficient).
func TestPayBillWithCredits(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD"})
	pay := billing.NewPayService(billing.NewRepo(db), pricing.NewRepo(db))
	now := time.Now().UTC()

	bill := func(bpID, status, net string) string {
		return mustInsertID(t, db, "bill", pgdoc.M{
			"billingProfileId": bpID, "status": status, "invoiceCurrency": "USD",
			"items": []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, net)}},
		})
	}

	t.Run("promo pays in full → PAID", func(t *testing.T) {
		bp := "bp-promo"
		profile := &billing.BillingProfile{ID: bp, Currency: "USD"}
		// settlement candidates require expirationDate > now — a null expiry is
		// EXCLUDED (the candidate filter requires expirationDate > now); never-expiring credits carry the NO_EXPIRATION sentinel.
		noExpiry := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
		pcid := mustInsertID(t, db, "promotionalCredit", pgdoc.M{"billingProfileId": bp, "remainingAmount": decimalOf(t, "10"), "expirationDate": noExpiry})
		billID := bill(bp, "SENT", "10")

		got, err := pay.PayBillWithCredits(ctx, profile, billID, now)
		if err != nil {
			t.Fatalf("pay: %v", err)
		}
		if got.Status != pricing.BillStatusPaid {
			t.Fatalf("status = %s, want PAID", got.Status)
		}
		if len(got.AppliedPromotionalCredits) != 1 || got.AppliedPromotionalCredits[0].Amount.String() != "10" {
			t.Fatalf("applied promo wrong: %+v", got.AppliedPromotionalCredits)
		}
		// promo balance consumed → 0 (persisted)
		var pc pricing.PromotionalCredit
		_, _ = db.C("promotionalCredit").FindOne(ctx, pgdoc.M{"_id": pcid}, &pc)
		// consumed → remaining 0; note json omitempty drops a zero decimal (IsZero), so nil≡0 here.
		if pc.RemainingAmount != nil && !pc.RemainingAmount.IsZero() {
			t.Fatalf("promo remaining not consumed: %v", pc.RemainingAmount)
		}
		// bill persisted PAID
		var reread pricing.Bill
		_, _ = db.C("bill").FindOne(ctx, pgdoc.M{"billingProfileId": bp}, &reread)
		if reread.Status != pricing.BillStatusPaid {
			t.Fatalf("persisted bill not PAID: %s", reread.Status)
		}
	})

	t.Run("account credit pays in full → PAID", func(t *testing.T) {
		bp := "bp-acct"
		profile := &billing.BillingProfile{ID: bp, Currency: "USD"}
		acid := mustInsertID(t, db, "accountCredit", pgdoc.M{
			"billingProfileId": bp, "currency": "USD", "invoiceCurrency": "USD",
			"invoiceExchangeRate": decimalOf(t, "1"), "amount": decimalOf(t, "5"), "createdAt": now,
		})
		billID := bill(bp, "SENT", "5")

		got, err := pay.PayBillWithCredits(ctx, profile, billID, now)
		if err != nil {
			t.Fatalf("pay: %v", err)
		}
		if got.Status != pricing.BillStatusPaid {
			t.Fatalf("status = %s, want PAID", got.Status)
		}
		if len(got.AppliedAccountCredits) != 1 || got.AppliedAccountCredits[0].Amount.String() != "5" {
			t.Fatalf("applied account credit wrong: %+v", got.AppliedAccountCredits)
		}
		var ac pricing.AccountCredit
		_, _ = db.C("accountCredit").FindOne(ctx, pgdoc.M{"_id": acid}, &ac)
		if ac.Amount != nil && !ac.Amount.IsZero() { // consumed → 0 (omitempty may drop the zero)
			t.Fatalf("account credit not consumed: %v", ac.Amount)
		}
	})

	t.Run("OPEN bill → ErrCannotPayOpenBill", func(t *testing.T) {
		bp := "bp-open"
		billID := bill(bp, "OPEN", "5")
		_, err := pay.PayBillWithCredits(ctx, &billing.BillingProfile{ID: bp, Currency: "USD"}, billID, now)
		if err != billing.ErrCannotPayOpenBill {
			t.Fatalf("got %v, want ErrCannotPayOpenBill", err)
		}
	})

	t.Run("PAID bill → ErrBillAlreadyPaid", func(t *testing.T) {
		bp := "bp-paid"
		billID := bill(bp, "PAID", "5")
		_, err := pay.PayBillWithCredits(ctx, &billing.BillingProfile{ID: bp, Currency: "USD"}, billID, now)
		if err != billing.ErrBillAlreadyPaid {
			t.Fatalf("got %v, want ErrBillAlreadyPaid", err)
		}
	})

	t.Run("insufficient credit → ErrNotEnoughCredit", func(t *testing.T) {
		bp := "bp-poor"
		mustInsert(t, db, "promotionalCredit", pgdoc.M{"billingProfileId": bp, "remainingAmount": decimalOf(t, "3")})
		billID := bill(bp, "SENT", "10")
		_, err := pay.PayBillWithCredits(ctx, &billing.BillingProfile{ID: bp, Currency: "USD"}, billID, now)
		if err != billing.ErrNotEnoughCredit {
			t.Fatalf("got %v, want ErrNotEnoughCredit", err)
		}
	})
}
