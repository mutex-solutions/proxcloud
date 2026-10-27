package admin

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func TestCreateAccountCreditReqDecode(t *testing.T) {
	var req createAccountCreditReq
	if err := json.Unmarshal([]byte(`{"amount":12.34}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Amount.String() != "12.34" {
		t.Errorf("amount=%q want 12.34", req.Amount.String())
	}
}

func TestUpdateAccountCreditReqDecode(t *testing.T) {
	var req updateAccountCreditReq
	body := `{"currency":"USD","amount":10,"invoiceCurrency":"EUR","initialAmount":20}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Currency != "USD" || req.InvoiceCurrency != "EUR" {
		t.Errorf("currencies mismatch: %+v", req)
	}
	if req.Amount.String() != "10" || req.InitialAmount.String() != "20" {
		t.Errorf("amounts mismatch: %+v", req)
	}
}

// TestBuildAccountCreditDocSameCurrency: base==invoice → invoiceExchangeRate ONE, amount stored as
// a decimal string, currency/invoiceCurrency set, the create/update timestamps present.
func TestBuildAccountCreditDocSameCurrency(t *testing.T) {
	r := &Repo{}
	doc, err := r.BuildAccountCreditDoc(json.Number("12.34"), "USD", "USD")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	amt, ok := doc["amount"].(decimal.Decimal)
	if !ok {
		t.Fatalf("amount is not decimal.Decimal: %T", doc["amount"])
	}
	if amt.String() != "12.34" {
		t.Errorf("amount=%s want 12.34", amt.String())
	}
	if doc["initialAmount"].(decimal.Decimal).String() != "12.34" {
		t.Errorf("initialAmount mismatch")
	}
	if doc["currency"] != "USD" || doc["invoiceCurrency"] != "USD" {
		t.Errorf("currency/invoiceCurrency mismatch: %#v", doc)
	}
	rate, ok := doc["invoiceExchangeRate"].(decimal.Decimal)
	if !ok || rate.String() != "1" {
		t.Errorf("invoiceExchangeRate=%v want 1", doc["invoiceExchangeRate"])
	}
	if _, ok := doc["createdAt"]; !ok {
		t.Errorf("createdAt missing")
	}
	if _, ok := doc["updatedAt"]; !ok {
		t.Errorf("updatedAt missing")
	}
}

// TestBuildAccountCreditDocCrossCurrency: base!=invoice → the FX not-implemented error (handler maps it to 501).
func TestBuildAccountCreditDocCrossCurrency(t *testing.T) {
	r := &Repo{}
	_, err := r.BuildAccountCreditDoc(json.Number("5"), "USD", "EUR")
	if !errors.Is(err, errAccountCreditFXSeam) {
		t.Fatalf("err=%v want errAccountCreditFXSeam", err)
	}
}

// TestAccountCreditUpdateFields: blank optional strings omitted (null fields dropped), money → decimal.Decimal.
func TestAccountCreditUpdateFields(t *testing.T) {
	r := &Repo{}

	// All present.
	set, err := r.AccountCreditUpdateFields(updateAccountCreditReq{
		Currency: "USD", Amount: "10", InvoiceCurrency: "EUR", InitialAmount: "20",
	})
	if err != nil {
		t.Fatal(err)
	}
	if set["currency"] != "USD" || set["invoiceCurrency"] != "EUR" {
		t.Errorf("currencies mismatch: %#v", set)
	}
	if set["amount"].(decimal.Decimal).String() != "10" {
		t.Errorf("amount mismatch: %#v", set["amount"])
	}
	if set["initialAmount"].(decimal.Decimal).String() != "20" {
		t.Errorf("initialAmount mismatch: %#v", set["initialAmount"])
	}

	// Blank strings + empty numbers are omitted.
	set, err = r.AccountCreditUpdateFields(updateAccountCreditReq{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"currency", "invoiceCurrency", "amount", "initialAmount"} {
		if _, ok := set[k]; ok {
			t.Errorf("blank %q must be omitted, got %#v", k, set[k])
		}
	}
}

// TestDecimalFromNumberEmpty: an empty/nil number parses to 0 (never a float panic).
func TestDecimalFromNumberEmpty(t *testing.T) {
	d, err := decimalFromNumber(json.Number(""))
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != "0" {
		t.Errorf("empty number → %s want 0", d.String())
	}
}
