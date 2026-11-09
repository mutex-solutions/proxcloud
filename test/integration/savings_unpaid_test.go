//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// TestHoldUnpaidUpfrontContracts: an upfront contract ACTIVE with nothing on record stops being ACTIVE, once,
// and the paid ones around it are left alone.
func TestHoldUnpaidUpfrontContracts(t *testing.T) {
	ctx := context.Background()
	f := newMoneyFixture(t)
	now := time.Now().UTC()
	base := func(extra pgdoc.M) pgdoc.M {
		doc := pgdoc.M{"billingProfileId": "bp-a", "status": "ACTIVE", "paidUpfront": true, "durationMonths": 12,
			"monthlyCommittedAmount": decimalOf(t, "100"), "discountRate": decimalOf(t, "20"), "endDate": now.AddDate(0, 6, 0)}
		for k, v := range extra {
			doc[k] = v
		}
		return doc
	}
	unpaid := mustInsertID(t, f.db, "savingsContract", base(nil))
	byDeposit := mustInsertID(t, f.db, "savingsContract", base(pgdoc.M{"paymentTransactionId": "txn1"}))
	byOperator := mustInsertID(t, f.db, "savingsContract", base(pgdoc.M{"paymentRecordedBy": "op1"}))
	noUpfront := mustInsertID(t, f.db, "savingsContract", base(pgdoc.M{"paidUpfront": false}))
	expired := mustInsertID(t, f.db, "savingsContract", base(pgdoc.M{"endDate": now.AddDate(0, -1, 0)}))

	held, err := f.repo.HoldUnpaidUpfrontContracts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0] != unpaid {
		t.Fatalf("held = %v, want only %s", held, unpaid)
	}
	c, err := f.repo.SavingsContractByID(ctx, unpaid)
	if err != nil || c.Status != billing.SavingsStatusPendingPayment {
		t.Fatalf("unpaid = %+v, %v", c, err)
	}
	// 100 x 12 less 20%: the amount the customer's deposit will have to match.
	if c.UpfrontAmount == nil || !c.UpfrontAmount.Equal(decimalOf(t, "960")) {
		t.Errorf("upfrontAmount = %v, want 960", c.UpfrontAmount)
	}
	for _, id := range []string{byDeposit, byOperator, noUpfront, expired} {
		if s := f.status(t, id); s != billing.SavingsStatusActive {
			t.Errorf("contract %s = %s, want ACTIVE", id, s)
		}
	}
	if again, err := f.repo.HoldUnpaidUpfrontContracts(ctx, now); err != nil || len(again) != 0 {
		t.Errorf("second run: %v, %v", again, err)
	}
}

// TestRecordUpfrontPayment drives the operator's path through the admin API.
func TestRecordUpfrontPayment(t *testing.T) {
	f := newMoneyFixture(t)
	const iss, cid = "https://kc/realms/master", "admin"
	h := newAdminRouter(f.db, iss, cid, nil)
	rc := &httpx.RequestContext{Sub: "op-1", Issuer: iss, Azp: cid}
	waiting := f.seedContract(t, "bp-a")

	if w := doAdmin(h, rc, "POST", "/admin/savings-contracts/"+waiting+"/record-payment", `{"reference":"  "}`); w.Code != 400 {
		t.Errorf("blank reference: %d", w.Code)
	}
	if w := doAdmin(h, rc, "POST", "/admin/savings-contracts/"+waiting+"/record-payment", `{"reference":"BANK-1"}`); w.Code != 200 {
		t.Fatalf("record: %d %s", w.Code, w.Body)
	}
	c, err := f.repo.SavingsContractByID(context.Background(), waiting)
	if err != nil || c.Status != billing.SavingsStatusActive || c.PaymentReference != "BANK-1" || c.PaymentRecordedBy != "op-1" || c.UpfrontUnpaid() {
		t.Fatalf("after record: %+v, %v", c, err)
	}
	if w := doAdmin(h, rc, "POST", "/admin/savings-contracts/"+waiting+"/record-payment", `{"reference":"BANK-2"}`); w.Code != 400 {
		t.Errorf("recording twice: %d", w.Code)
	}

	// An edit cannot activate what nobody paid for, and can still edit what somebody did.
	unpaid := f.seedContract(t, "bp-a")
	if w := doAdmin(h, rc, "PUT", "/admin/savings-contracts/"+unpaid, `{"status":"ACTIVE","paidUpfront":true,"billingProfileId":"bp-a"}`); w.Code != 400 {
		t.Errorf("edit to ACTIVE with no payment: %d %s", w.Code, w.Body)
	}
	if w := doAdmin(h, rc, "PUT", "/admin/savings-contracts/"+waiting, `{"status":"ACTIVE","paidUpfront":true,"billingProfileId":"bp-a"}`); w.Code != 200 {
		t.Errorf("edit of a paid contract: %d %s", w.Code, w.Body)
	}
}
