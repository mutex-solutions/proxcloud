package pricing

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

// The applied-credit + adjustment sub-documents are serialized on the BillDto response.
// shopspring decimal.Decimal marshals to a QUOTED string by default, but these
// money fields must serialize as JSON NUMBERS. These MarshalJSON methods emit money as
// json.Number (nil → omitted). storage is unaffected (the codec
// stores money as a decimal string in jsonb via the struct tags, not MarshalJSON).

func numJSON(d *decimal.Decimal) json.Number {
	if d == nil {
		return ""
	}
	return json.Number(d.String())
}

func (a AppliedAccountCredit) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Currency        string      `json:"currency,omitempty"`
		Amount          json.Number `json:"amount,omitempty"`
		InvoiceCurrency string      `json:"invoiceCurrency,omitempty"`
		GrossAmount     json.Number `json:"grossAmount,omitempty"`
		ExchangeRate    json.Number `json:"exchangeRate,omitempty"`
		AccountCreditID string      `json:"accountCreditId,omitempty"`
	}{a.Currency, numJSON(a.Amount), a.InvoiceCurrency, numJSON(a.GrossAmount), numJSON(a.ExchangeRate), a.AccountCreditID})
}

func (a AppliedPromotionalCredit) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Currency            string      `json:"currency,omitempty"`
		Amount              json.Number `json:"amount,omitempty"`
		InvoiceCurrency     string      `json:"invoiceCurrency,omitempty"`
		GrossAmount         json.Number `json:"grossAmount,omitempty"`
		ExchangeRate        json.Number `json:"exchangeRate,omitempty"`
		PromotionalCreditID string      `json:"promotionalCreditId,omitempty"`
	}{a.Currency, numJSON(a.Amount), a.InvoiceCurrency, numJSON(a.GrossAmount), numJSON(a.ExchangeRate), a.PromotionalCreditID})
}

func (a AppliedCollectedCredit) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Currency             string      `json:"currency,omitempty"`
		Amount               json.Number `json:"amount,omitempty"`
		InvoiceCurrency      string      `json:"invoiceCurrency,omitempty"`
		GrossAmount          json.Number `json:"grossAmount,omitempty"`
		ExchangeRate         json.Number `json:"exchangeRate,omitempty"`
		CollectTransactionID string      `json:"collectTransactionId,omitempty"`
	}{a.Currency, numJSON(a.Amount), a.InvoiceCurrency, numJSON(a.GrossAmount), numJSON(a.ExchangeRate), a.CollectTransactionID})
}

func (a BillAdjustment) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount                  json.Number `json:"amount,omitempty"`
		Type                    string      `json:"type,omitempty"`
		Description             string      `json:"description,omitempty"`
		ContractID              string      `json:"contractId,omitempty"`
		SavingsPlanName         string      `json:"savingsPlanName,omitempty"`
		StartDateContract       *time.Time  `json:"startDateContract,omitempty"`
		EndDateContract         *time.Time  `json:"endDateContract,omitempty"`
		PriceAdjustmentRuleID   string      `json:"priceAdjustmentRuleId,omitempty"`
		PriceAdjustmentRuleName string      `json:"priceAdjustmentRuleName,omitempty"`
	}{numJSON(a.Amount), a.Type, a.Description, a.ContractID, a.SavingsPlanName, a.StartDateContract, a.EndDateContract, a.PriceAdjustmentRuleID, a.PriceAdjustmentRuleName})
}
