package billing

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// Response DTOs for the billing client LIST endpoints that return raw documents
// (PromotionalCreditController → List<PromotionalCredit>, CollectTransactionController →
// List<CollectTransaction>). The domain serializes directly, so money is a
// JSON NUMBER — but shopspring decimal.Decimal marshals as a QUOTED STRING by default, so these
// mirror the domain with json.Number money (nil amount → omitted). The AccountCredit
// Transaction / SavingsContract / SavingsPlan DTOs land in the next step (need their domains modeled).

// PromotionalCreditDto is the PromotionalCredit document (returned raw).
type PromotionalCreditDto struct {
	ID               string      `json:"id,omitempty"`
	Code             string      `json:"code,omitempty"`
	BillingProfileID string      `json:"billingProfileId,omitempty"`
	InitialAmount    json.Number `json:"initialAmount,omitempty"`
	RemainingAmount  json.Number `json:"remainingAmount,omitempty"`
	ExpirationDate   *time.Time  `json:"expirationDate,omitempty"`
	CreatedAt        *time.Time  `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time  `json:"updatedAt,omitempty"`
}

// promoMinRemaining = the filter threshold: remainingAmount > 0.01 (strict).
var promoMinRemaining = decimal.RequireFromString("0.01")

// PromotionalCreditsToDtos maps + applies the controller filter (remainingAmount > 0.01).
func PromotionalCreditsToDtos(pcs []pricing.PromotionalCredit) []PromotionalCreditDto {
	out := make([]PromotionalCreditDto, 0, len(pcs))
	for i := range pcs {
		pc := &pcs[i]
		if pc.RemainingAmount == nil || pc.RemainingAmount.Cmp(promoMinRemaining) <= 0 {
			continue
		}
		out = append(out, PromotionalCreditDto{
			ID: pc.ID, Code: pc.Code, BillingProfileID: pc.BillingProfileID,
			InitialAmount: numPtr(pc.InitialAmount), RemainingAmount: numPtr(pc.RemainingAmount),
			ExpirationDate: pc.ExpirationDate, CreatedAt: pc.CreatedAt, UpdatedAt: pc.UpdatedAt,
		})
	}
	return out
}

// CollectTransactionDto is the CollectTransaction document (returned raw,
// money as JSON numbers). Used for both the list endpoint and the collect-by-card single response.
// invoiceDetails is deferred (not populated on the collect-by-card path).
type CollectTransactionDto struct {
	ID                string         `json:"id,omitempty"`
	BillID            string         `json:"billId,omitempty"`
	OrderID           string         `json:"orderId,omitempty"`
	BillingProfileID  string         `json:"billingProfileId,omitempty"`
	CreditCardID      string         `json:"creditCardId,omitempty"`
	PaymentGatewayID  string         `json:"paymentGatewayId,omitempty"`
	InvoiceGatewayID  string         `json:"invoiceGatewayId,omitempty"`
	ExternalID        string         `json:"externalId,omitempty"`
	ExternalInvoiceID string         `json:"externalInvoiceId,omitempty"`
	ErrorMessage      string         `json:"errorMessage,omitempty"`
	Currency          string         `json:"currency,omitempty"`
	Status            string         `json:"status,omitempty"`
	Amount            json.Number    `json:"amount,omitempty"`
	GrossAmount       json.Number    `json:"grossAmount,omitempty"`
	ExchangeRate      json.Number    `json:"exchangeRate,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	CreatedAt         *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time     `json:"updatedAt,omitempty"`
}

// CollectTransactionToDto maps one transaction (the collect-by-card single response).
func CollectTransactionToDto(t *pricing.CollectTransaction) CollectTransactionDto {
	// The FE builds the download filename as `${externalInvoiceId}.pdf` (and forces it via the <a>
	// download attr, ignoring our Content-Disposition). Our txns have no external invoice doc, so fall
	// back to the external payment id (real txns always carry the pi_) → a meaningful filename instead
	// of "undefined.pdf". NO further fallback to t.ID — a txn with NEITHER field must omit the key
	// (an earlier bug had the t.ID fallback leak into the serialized doc).
	externalInvoiceID := firstNonEmpty(t.ExternalInvoiceID, t.ExternalID)
	return CollectTransactionDto{
		ID: t.ID, BillID: t.BillID, OrderID: t.OrderID, BillingProfileID: t.BillingProfileID,
		CreditCardID: t.CreditCardID, PaymentGatewayID: t.PaymentGatewayID, InvoiceGatewayID: t.InvoiceGatewayID,
		ExternalID: t.ExternalID, ExternalInvoiceID: externalInvoiceID, ErrorMessage: t.ErrorMessage,
		Currency: t.Currency, Status: string(t.Status),
		Amount: numPtr(t.Amount), GrossAmount: numPtr(t.GrossAmount), ExchangeRate: numPtr(t.ExchangeRate),
		Metadata: t.Metadata, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// firstNonEmpty returns the first non-blank string (or "").
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// CreditCardTransactionDto is the CreditCardTransaction document (admin returns it raw,
// money as JSON numbers).
type CreditCardTransactionDto struct {
	ID               string      `json:"id,omitempty"`
	BillingProfileID string      `json:"billingProfileId,omitempty"`
	Status           string      `json:"status,omitempty"`
	ExternalID       string      `json:"externalId,omitempty"`
	PaymentGatewayID string      `json:"paymentGatewayId,omitempty"`
	InvoiceGatewayID string      `json:"invoiceGatewayId,omitempty"`
	Currency         string      `json:"currency,omitempty"`
	Amount           json.Number `json:"amount,omitempty"`
	ExchangeRate     json.Number `json:"exchangeRate,omitempty"`
	ErrorMessage     string      `json:"errorMessage,omitempty"`
	CreatedAt        *time.Time  `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time  `json:"updatedAt,omitempty"`
}

// CreditCardTransactionToDto maps one card transaction (the admin by-id read).
func CreditCardTransactionToDto(t *CreditCardTransaction) CreditCardTransactionDto {
	return CreditCardTransactionDto{
		ID: t.ID, BillingProfileID: t.BillingProfileID, Status: t.Status, ExternalID: t.ExternalID,
		PaymentGatewayID: t.PaymentGatewayID, InvoiceGatewayID: t.InvoiceGatewayID, Currency: t.Currency,
		Amount: numPtr(t.Amount), ExchangeRate: numPtr(t.ExchangeRate), ErrorMessage: t.ErrorMessage,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func CreditCardTransactionsToDtos(txs []CreditCardTransaction) []CreditCardTransactionDto {
	out := make([]CreditCardTransactionDto, 0, len(txs))
	for i := range txs {
		out = append(out, CreditCardTransactionToDto(&txs[i]))
	}
	return out
}

func CollectTransactionsToDtos(txs []pricing.CollectTransaction) []CollectTransactionDto {
	out := make([]CollectTransactionDto, 0, len(txs))
	for i := range txs {
		out = append(out, CollectTransactionToDto(&txs[i]))
	}
	return out
}

// AccountCreditTransaction is the "accountCreditTransaction" document (json decode).
// The domain is returned raw, so the DTO is the same field set. The
// nested invoiceDetails / accountCredit / metadata are kept as raw sub-docs (deferred typed
// mapping — for a rich txn carrying these, the raw passthrough would not match the canonical
// serialization; the scalar + money fields ARE faithful now).
type AccountCreditTransaction struct {
	ID                string           `json:"id,omitempty"`
	Currency          string           `json:"currency,omitempty"`
	OrderID           string           `json:"orderId,omitempty"`
	ExternalID        string           `json:"externalId,omitempty"`
	BillID            string           `json:"billId,omitempty"`
	Amount            *decimal.Decimal `json:"amount,omitempty"`
	GrossAmount       *decimal.Decimal `json:"grossAmount,omitempty"`
	InvoiceGatewayID  string           `json:"invoiceGatewayId,omitempty"`
	PaymentGatewayID  string           `json:"paymentGatewayId,omitempty"`
	BillingProfileID  string           `json:"billingProfileId,omitempty"`
	ExternalInvoiceID string           `json:"externalInvoiceId,omitempty"`
	ExchangeRate      *decimal.Decimal `json:"exchangeRate,omitempty"`
	Status            string           `json:"status,omitempty"`
	GatewayMessage    string           `json:"gatewayMessage,omitempty"`
	InvoiceDetails    pgdoc.M          `json:"invoiceDetails,omitempty"`
	Metadata          pgdoc.M          `json:"metadata,omitempty"`
	AccountCredit     pgdoc.M          `json:"accountCredit,omitempty"`
	CreatedAt         *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time       `json:"updatedAt,omitempty"`
}

// AccountCreditTransactionDto is the serialized shape (money as JSON number, _id → id,
// nulls omitted). Matches the raw AccountCreditTransaction serialization.
type AccountCreditTransactionDto struct {
	ID                string      `json:"id,omitempty"`
	Currency          string      `json:"currency,omitempty"`
	OrderID           string      `json:"orderId,omitempty"`
	ExternalID        string      `json:"externalId,omitempty"`
	BillID            string      `json:"billId,omitempty"`
	Amount            json.Number `json:"amount,omitempty"`
	GrossAmount       json.Number `json:"grossAmount,omitempty"`
	InvoiceGatewayID  string      `json:"invoiceGatewayId,omitempty"`
	PaymentGatewayID  string      `json:"paymentGatewayId,omitempty"`
	BillingProfileID  string      `json:"billingProfileId,omitempty"`
	ExternalInvoiceID string      `json:"externalInvoiceId,omitempty"`
	ExchangeRate      json.Number `json:"exchangeRate,omitempty"`
	Status            string      `json:"status,omitempty"`
	GatewayMessage    string      `json:"gatewayMessage,omitempty"`
	InvoiceDetails    pgdoc.M     `json:"invoiceDetails,omitempty"`
	Metadata          pgdoc.M     `json:"metadata,omitempty"`
	AccountCredit     pgdoc.M     `json:"accountCredit,omitempty"`
	CreatedAt         *time.Time  `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time  `json:"updatedAt,omitempty"`
}

// AccountCreditTransactionToDto maps one transaction (money → JSON number).
func AccountCreditTransactionToDto(t *AccountCreditTransaction) AccountCreditTransactionDto {
	return AccountCreditTransactionDto{
		ID: t.ID, Currency: t.Currency, OrderID: t.OrderID, ExternalID: t.ExternalID, BillID: t.BillID,
		Amount: numPtr(t.Amount), GrossAmount: numPtr(t.GrossAmount),
		InvoiceGatewayID: t.InvoiceGatewayID, PaymentGatewayID: t.PaymentGatewayID,
		BillingProfileID: t.BillingProfileID, ExternalInvoiceID: t.ExternalInvoiceID,
		ExchangeRate: numPtr(t.ExchangeRate), Status: t.Status, GatewayMessage: t.GatewayMessage,
		InvoiceDetails: t.InvoiceDetails, Metadata: t.Metadata, AccountCredit: t.AccountCredit,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// AccountCreditTransactionsToDtos maps a list.
func AccountCreditTransactionsToDtos(txs []AccountCreditTransaction) []AccountCreditTransactionDto {
	out := make([]AccountCreditTransactionDto, 0, len(txs))
	for i := range txs {
		out = append(out, AccountCreditTransactionToDto(&txs[i]))
	}
	return out
}

// numPtr renders a nullable Decimal as a JSON number, or "" (omitted via omitempty) when nil.
func numPtr(d *decimal.Decimal) json.Number {
	if d == nil {
		return ""
	}
	return json.Number(d.String())
}
