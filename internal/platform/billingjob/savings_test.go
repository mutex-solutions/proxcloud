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
