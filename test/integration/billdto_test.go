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

// TestBillListTypedEndToEnd verifies the bill-DTO wiring against real Postgres: a seeded bill
// (decimal-string money) decodes through billing.Repo.BillsByBillingProfile (encoding/json →
// decimal.Decimal) and maps via billing.ToBillDto to a populated BillDto with money as JSON
// numbers — and the empty case returns an empty slice ({data:[]}, never null).
func TestBillListTypedEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	cs := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// empty → empty slice (not nil)
	empty, err := repo.BillsByBillingProfile(ctx, "bp-none")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty bills: want non-nil empty slice, got %v err=%v", empty, err)
	}

	// seed a bill with one charged item (decimal money, like the live pipeline output)
	netD := decimalOf(t, "6.9454374396006267")
	mustInsert(t, db, "bill", pgdoc.M{
		"billingProfileId": "bp-1", "status": "OPEN", "invoiceCurrency": "USD",
		"billingCycle": pgdoc.M{"startDate": cs, "endDate": cs.AddDate(0, 1, 0)},
		"items": []any{pgdoc.M{
			"name": "instance_traffic", "resourceId": "instance_traffic-x", "resourceType": "instance_traffic",
			"currency": "USD", "netAmount": netD,
		}},
	})

	bills, err := repo.BillsByBillingProfile(ctx, "bp-1")
	if err != nil || len(bills) != 1 {
		t.Fatalf("typed bills: got %d err=%v", len(bills), err)
	}
	if bills[0].Items[0].NetAmount.String() != "6.9454374396006267" {
		t.Fatalf("decimal decode wrong: %s", bills[0].Items[0].NetAmount)
	}

	profile := &billing.BillingProfile{Currency: "USD"}
	dto, err := billing.ToBillDto(profile, &bills[0], nil, "USD", pricing.NewExchanger(nil), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if dto.NetAmount != "6.9454374396006267" {
		t.Fatalf("BillDto.netAmount = %s, want 6.9454374396006267", dto.NetAmount)
	}
	if dto.Status != "OPEN" || dto.InvoiceCurrency != "USD" || len(dto.Items) != 1 {
		t.Fatalf("BillDto passthrough wrong: %+v", dto)
	}
	t.Logf("bill-list typed e2e: net=%s gross=%s", dto.NetAmount, dto.GrossAmount)
}
