package admin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// dec(...) helper is shared from savingscontract_test.go in this package.

// TestMapCollectToTransaction verifies the collect overload: transactionType "collect", creditCardId
// carried through, errorMessage straight from the collect field, gatewayMessage NOT set, money copied
// as numbers.
func TestMapCollectToTransaction(t *testing.T) {
	now := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	src := &pricing.CollectTransaction{
		ID:                "ct1",
		Currency:          "USD",
		ExternalID:        "pi_123",
		BillID:            "bill1",
		Amount:            dec("12.34"),
		ErrorMessage:      "card declined",
		GrossAmount:       dec("14.68"),
		InvoiceGatewayID:  "inv-gw",
		PaymentGatewayID:  "pay-gw",
		BillingProfileID:  "bp1",
		ExternalInvoiceID: "ext-inv",
		ExchangeRate:      dec("1.5"),
		CreditCardID:      "card1",
		Status:            pricing.CollectTransactionStatus("SUCCESS"),
		Metadata:          map[string]any{"k": "v"},
		CreatedAt:         &now,
		UpdatedAt:         &now,
	}
	got := mapCollectToTransaction(src)

	if got.TransactionType != "collect" {
		t.Errorf("transactionType = %q, want collect", got.TransactionType)
	}
	if got.CreditCardID != "card1" {
		t.Errorf("creditCardId = %q, want card1", got.CreditCardID)
	}
	if got.ErrorMessage != "card declined" {
		t.Errorf("errorMessage = %q", got.ErrorMessage)
	}
	if got.GatewayMessage != "" {
		t.Errorf("gatewayMessage should be empty for collect, got %q", got.GatewayMessage)
	}
	if got.Status != "SUCCESS" {
		t.Errorf("status = %q, want SUCCESS", got.Status)
	}
	if got.Amount != json.Number("12.34") {
		t.Errorf("amount = %q, want 12.34", got.Amount)
	}
	if got.GrossAmount != json.Number("14.68") {
		t.Errorf("grossAmount = %q", got.GrossAmount)
	}
	if got.ExchangeRate != json.Number("1.5") {
		t.Errorf("exchangeRate = %q", got.ExchangeRate)
	}
}

// TestMapAccountCreditToTransaction verifies the account-credit overload: transactionType
// "account-credit", gatewayMessage used for BOTH errorMessage and gatewayMessage, creditCardId NOT
// set, pgdoc.M metadata carried through.
func TestMapAccountCreditToTransaction(t *testing.T) {
	now := time.Date(2026, 6, 27, 11, 0, 0, 0, time.UTC)
	src := &billing.AccountCreditTransaction{
		ID:               "ac1",
		Currency:         "EUR",
		ExternalID:       "pi_456",
		BillID:           "bill2",
		Amount:           dec("25"),
		GrossAmount:      dec("29.75"),
		InvoiceGatewayID: "inv-gw2",
		PaymentGatewayID: "pay-gw2",
		BillingProfileID: "bp2",
		ExchangeRate:     dec("1.0"),
		Status:           "SUCCESS",
		GatewayMessage:   "insufficient funds",
		Metadata:         pgdoc.M{"foo": "bar"},
		CreatedAt:        &now,
		UpdatedAt:        &now,
	}
	got := mapAccountCreditToTransaction(src)

	if got.TransactionType != "account-credit" {
		t.Errorf("transactionType = %q, want account-credit", got.TransactionType)
	}
	if got.CreditCardID != "" {
		t.Errorf("creditCardId should be empty for account-credit, got %q", got.CreditCardID)
	}
	if got.ErrorMessage != "insufficient funds" {
		t.Errorf("errorMessage = %q, want gatewayMessage value", got.ErrorMessage)
	}
	if got.GatewayMessage != "insufficient funds" {
		t.Errorf("gatewayMessage = %q", got.GatewayMessage)
	}
	if got.Amount != json.Number("25") {
		t.Errorf("amount = %q, want 25", got.Amount)
	}
	if got.Metadata["foo"] != "bar" {
		t.Errorf("metadata not carried: %v", got.Metadata)
	}
}

// TestTransactionDtoNonNull verifies null-omitting JSON: nil money and unset string/time fields are
// omitted; money serializes as a JSON number (not quoted); accountCredit is never emitted.
func TestTransactionDtoNonNull(t *testing.T) {
	src := &pricing.CollectTransaction{
		ID:               "ct2",
		Status:           pricing.CollectTransactionStatus("PENDING"),
		BillingProfileID: "bp9",
		// Amount/GrossAmount/ExchangeRate nil, no creditCardId, no times.
	}
	got := mapCollectToTransaction(src)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)

	for _, omitted := range []string{"amount", "grossAmount", "exchangeRate", "creditCardId", "createdAt", "updatedAt", "accountCredit", "errorMessage", "gatewayMessage"} {
		if strings.Contains(s, "\""+omitted+"\"") {
			t.Errorf("expected %q to be omitted (null-omitting), json: %s", omitted, s)
		}
	}
	if !strings.Contains(s, `"transactionType":"collect"`) {
		t.Errorf("transactionType missing: %s", s)
	}
	if !strings.Contains(s, `"status":"PENDING"`) {
		t.Errorf("status missing: %s", s)
	}
}

// TestTransactionDtoMoneyIsNumber confirms money is emitted as a bare JSON number, not a quoted
// string (shopspring decimal would otherwise quote it).
func TestTransactionDtoMoneyIsNumber(t *testing.T) {
	got := mapCollectToTransaction(&pricing.CollectTransaction{ID: "x", Amount: dec("10.5")})
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"amount":10.5`) {
		t.Errorf("amount should be a JSON number 10.5, got: %s", string(b))
	}
	if strings.Contains(string(b), `"amount":"10.5"`) {
		t.Errorf("amount must not be quoted: %s", string(b))
	}
}
