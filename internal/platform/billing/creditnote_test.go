package billing

import (
	"bytes"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

// A refunded credit is voided in place: zero balance + refundedAt, initialAmount kept — and every
// balance reader excludes it exactly as a deletion did.
func TestMarkAccountCreditRefundedExcludesFromBalances(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	refunded := &pricing.AccountCredit{ID: "ac-refunded", InitialAmount: dec("100"), Amount: dec("70"), Currency: "USD", InvoiceCurrency: "USD"}
	live := &pricing.AccountCredit{ID: "ac-live", InitialAmount: dec("30"), Amount: dec("30"), Currency: "USD", InvoiceCurrency: "USD"}

	MarkAccountCreditRefunded(refunded, now)

	if !refunded.Amount.IsZero() || refunded.RefundedAt == nil || !refunded.RefundedAt.Equal(now) {
		t.Fatalf("void: amount=%v refundedAt=%v", refunded.Amount, refunded.RefundedAt)
	}
	if !refunded.InitialAmount.Equal(decimal.RequireFromString("100")) {
		t.Fatalf("initialAmount must survive the void: %v", refunded.InitialAmount)
	}
	// pay-a-bill balance check sums Amount → only the live credit counts.
	if got := creditBalance(nil, []*pricing.AccountCredit{refunded, live}); !got.Equal(decimal.RequireFromString("30")) {
		t.Fatalf("creditBalance = %v, want 30", got)
	}
	// settle path skips the zero credit — nothing is taken from it.
	settlements, err := pricing.SettleAccountCredits([]*pricing.AccountCredit{refunded, live}, decimal.RequireFromString("20"), "USD", pricing.NewExchanger(nil), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(settlements) != 1 || settlements[0].AccountCredit.ID != "ac-live" || !settlements[0].Settled.Equal(decimal.RequireFromString("20")) {
		t.Fatalf("settlements = %+v", settlements)
	}
	if !refunded.Amount.IsZero() {
		t.Fatalf("refunded credit was consumed: %v", refunded.Amount)
	}
}

func TestCreditNoteNumber(t *testing.T) {
	if got := CreditNoteNumber("6a37e2f547f3b4378ba78549"); got != "CN-8BA78549" {
		t.Fatalf("got %s", got)
	}
	if got := CreditNoteNumber("abc"); got != "CN-ABC" {
		t.Fatalf("short id: got %s", got)
	}
}

func TestCreditNoteAndReceiptPDFs(t *testing.T) {
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	bp := &BillingProfile{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com", Country: "SG", Address: "1 Marina Blvd"}
	txn := &AccountCreditTransaction{
		ID: "6a37e2f547f3b4378ba78549", Status: "REFUNDED", Currency: "SGD",
		Amount: dec("100"), GrossAmount: dec("109"), BillID: "bill-1", CreatedAt: &now, UpdatedAt: &now,
	}
	issuer := IssuerInfo{BusinessName: "Example Cloud Pte. Ltd.", Address: "1 Raffles Place", City: "Singapore", ZipCode: "048616", Country: "SG", UEN: "201234567A", GSTRegistered: true, GSTRegistrationNumber: "M9-1234567-8"}

	data, name, err := CreditNotePDF(txn, bp, issuer)
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF")) || name != "CreditNote-CN-8BA78549.pdf" {
		t.Fatalf("credit note: err=%v name=%s len=%d", err, name, len(data))
	}
	txn.Status = "SUCCESS"
	data, name, err = DepositReceiptPDF(txn, bp, IssuerInfo{})
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF")) || name != "Receipt-6a37e2f547f3b4378ba78549.pdf" {
		t.Fatalf("receipt: err=%v name=%s len=%d", err, name, len(data))
	}
	if got := refersTo(txn); got != "Transaction 6a37e2f547f3b4378ba78549 / bill bill-1" {
		t.Fatalf("refersTo = %q", got)
	}
}
