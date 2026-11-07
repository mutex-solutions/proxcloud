package billingjob

import (
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
)

// A cancelled, expired or unpaid contract covering the cycle discounts nothing.
func TestOnlyActiveContractsDiscount(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	from, to := start.AddDate(0, -1, 0), end.AddDate(0, 1, 0)
	var in []billing.SavingsContract
	for _, st := range []string{billing.SavingsStatusActive, billing.SavingsStatusCancelled, billing.SavingsStatusExpired, billing.SavingsStatusPendingPayment} {
		in = append(in, billing.SavingsContract{ID: st, Status: st, StartDate: &from, EndDate: &to})
	}
	got := savingsAdjOf(in, start, end)
	if len(got) != 1 || got[0].ID != billing.SavingsStatusActive {
		t.Fatalf("discounting contracts = %+v", got)
	}
}

// An upfront contract discounts only with its payment on record, whatever its status says.
func TestUnpaidUpfrontContractDiscountsNothing(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	from, to := start.AddDate(0, -1, 0), end.AddDate(0, 1, 0)
	in := []billing.SavingsContract{
		{ID: "unpaid", PaidUpfront: true},
		{ID: "deposit", PaidUpfront: true, PaymentTransactionID: "txn"},
		{ID: "operator", PaidUpfront: true, PaymentRecordedBy: "op"},
		{ID: "no-upfront"},
	}
	for i := range in {
		in[i].Status, in[i].StartDate, in[i].EndDate = billing.SavingsStatusActive, &from, &to
	}
	got := savingsAdjOf(in, start, end)
	if len(got) != 3 {
		t.Fatalf("discounting contracts = %+v", got)
	}
	for _, c := range got {
		if c.ID == "unpaid" {
			t.Fatalf("the unpaid contract discounts: %+v", c)
		}
	}
}
