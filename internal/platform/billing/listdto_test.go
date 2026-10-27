package billing

import (
	"encoding/json"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

func dp(s string) *decimal.Decimal { d := decimal.RequireFromString(s); return &d }

// TestPromotionalCreditsFilter: only remainingAmount > 0.01 survive (strict filter); money
// serializes as an unquoted JSON number; nil amount → omitted.
func TestPromotionalCreditsFilter(t *testing.T) {
	pcs := []pricing.PromotionalCredit{
		{ID: "keep", Code: "C1", RemainingAmount: dp("5"), InitialAmount: dp("10")},
		{ID: "drop-eq", RemainingAmount: dp("0.01")},  // == 0.01 → dropped (strict >)
		{ID: "drop-lo", RemainingAmount: dp("0.005")}, // < 0.01 → dropped
		{ID: "drop-nil"}, // nil → dropped
	}
	out := PromotionalCreditsToDtos(pcs)
	if len(out) != 1 || out[0].ID != "keep" {
		t.Fatalf("filter wrong: %+v", out)
	}
	if out[0].RemainingAmount != "5" || out[0].InitialAmount != "10" {
		t.Fatalf("money mapping wrong: %+v", out[0])
	}
	raw, _ := json.Marshal(out[0])
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if v := string(m["remainingAmount"]); v != "5" {
		t.Fatalf("remainingAmount must be unquoted number 5, got %s", v)
	}
	if _, ok := m["expirationDate"]; ok {
		t.Fatalf("nil expirationDate must be omitted")
	}
}

// TestCollectTransactionDto: money → unquoted numbers; nil decimals omitted; status string.
func TestCollectTransactionDto(t *testing.T) {
	out := CollectTransactionsToDtos([]pricing.CollectTransaction{
		{ID: "t1", BillID: "b1", Currency: "USD", Status: pricing.CollectTransactionStatusSuccess, Amount: dp("12.34"), GrossAmount: dp("14.81")},
	})
	if len(out) != 1 {
		t.Fatal("expected 1")
	}
	raw, _ := json.Marshal(out[0])
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if string(m["amount"]) != "12.34" || string(m["grossAmount"]) != "14.81" {
		t.Fatalf("money must be unquoted numbers, got amount=%s gross=%s", m["amount"], m["grossAmount"])
	}
	if _, ok := m["exchangeRate"]; ok {
		t.Fatalf("nil exchangeRate must be omitted")
	}
	if string(m["status"]) != `"SUCCESS"` {
		t.Fatalf("status = %s, want \"SUCCESS\"", m["status"])
	}
}
