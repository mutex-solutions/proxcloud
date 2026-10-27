package pricing

import (
	"time"

	"github.com/shopspring/decimal"
)

// Credit + applied-credit domains. Money follows the slice convention: decimal.Decimal
// for math, *decimal.Decimal for a nullable amount. The settlement functions
// (settle.go) are pure; repos/creation/redemption are deferred.

// AccountCredit is the "accountCredit" collection. `Amount` is the live spendable balance
// in the credit's base currency (there is no separate "balance" field).
type AccountCredit struct {
	ID                  string           `json:"id,omitempty"`
	BillingProfileID    string           `json:"billingProfileId,omitempty"`
	InvoiceSeries       string           `json:"invoiceSeries,omitempty"`
	InvoiceNumber       string           `json:"invoiceNumber,omitempty"`
	InvoiceCurrency     string           `json:"invoiceCurrency,omitempty"`
	InvoiceExchangeRate *decimal.Decimal `json:"invoiceExchangeRate,omitempty"`
	InitialAmount       *decimal.Decimal `json:"initialAmount,omitempty"`
	Amount              *decimal.Decimal `json:"amount,omitempty"`
	Currency            string           `json:"currency,omitempty"`
	CreatedAt           *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt           *time.Time       `json:"updatedAt,omitempty"`
}

// PromotionalCredit is the "promotionalCredit" collection. `RemainingAmount` is the live
// balance, in base currency (no currency/exchangeRate of its own).
type PromotionalCredit struct {
	ID               string           `json:"id,omitempty"`
	CreatedAt        *time.Time       `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time       `json:"updatedAt,omitempty"`
	ExpirationDate   *time.Time       `json:"expirationDate,omitempty"`
	BillingProfileID string           `json:"billingProfileId,omitempty"`
	Code             string           `json:"code,omitempty"`
	InitialAmount    *decimal.Decimal `json:"initialAmount,omitempty"`
	RemainingAmount  *decimal.Decimal `json:"remainingAmount,omitempty"`
}

// AppliedAccountCredit / AppliedPromotionalCredit / AppliedCollectedCredit are
// embedded on a Bill. `Amount` is the product/base-currency value subtracted from
// the unpaid total; grossAmount/exchangeRate/invoiceCurrency are the FX view.
type AppliedAccountCredit struct {
	Currency        string           `json:"currency,omitempty"`
	Amount          *decimal.Decimal `json:"amount,omitempty"`
	InvoiceCurrency string           `json:"invoiceCurrency,omitempty"`
	GrossAmount     *decimal.Decimal `json:"grossAmount,omitempty"`
	ExchangeRate    *decimal.Decimal `json:"exchangeRate,omitempty"`
	AccountCreditID string           `json:"accountCreditId,omitempty"`
}

type AppliedPromotionalCredit struct {
	Currency            string           `json:"currency,omitempty"`
	Amount              *decimal.Decimal `json:"amount,omitempty"`
	InvoiceCurrency     string           `json:"invoiceCurrency,omitempty"`
	GrossAmount         *decimal.Decimal `json:"grossAmount,omitempty"`
	ExchangeRate        *decimal.Decimal `json:"exchangeRate,omitempty"`
	PromotionalCreditID string           `json:"promotionalCreditId,omitempty"`
}

type AppliedCollectedCredit struct {
	Currency             string           `json:"currency,omitempty"`
	Amount               *decimal.Decimal `json:"amount,omitempty"`
	InvoiceCurrency      string           `json:"invoiceCurrency,omitempty"`
	GrossAmount          *decimal.Decimal `json:"grossAmount,omitempty"`
	ExchangeRate         *decimal.Decimal `json:"exchangeRate,omitempty"`
	CollectTransactionID string           `json:"collectTransactionId,omitempty"`
}

// CollectTransaction is the "collectTransaction" collection — the source of an
// AppliedCollectedCredit (a completed payment). The settlement path reads
// amount/grossAmount/exchangeRate/currency.
type CollectTransaction struct {
	ID                string                   `json:"id,omitempty"`
	BillID            string                   `json:"billId,omitempty"`
	OrderID           string                   `json:"orderId,omitempty"`
	Currency          string                   `json:"currency,omitempty"`
	ExternalID        string                   `json:"externalId,omitempty"`
	Amount            *decimal.Decimal         `json:"amount,omitempty"`
	GrossAmount       *decimal.Decimal         `json:"grossAmount,omitempty"`
	ErrorMessage      string                   `json:"errorMessage,omitempty"`
	ExchangeRate      *decimal.Decimal         `json:"exchangeRate,omitempty"`
	InvoiceGatewayID  string                   `json:"invoiceGatewayId,omitempty"`
	PaymentGatewayID  string                   `json:"paymentGatewayId,omitempty"`
	BillingProfileID  string                   `json:"billingProfileId,omitempty"`
	ExternalInvoiceID string                   `json:"externalInvoiceId,omitempty"`
	CreditCardID      string                   `json:"creditCardId,omitempty"`
	Status            CollectTransactionStatus `json:"status,omitempty"`
	Metadata          map[string]any           `json:"metadata,omitempty"`
	CreatedAt         *time.Time               `json:"createdAt,omitempty"`
	UpdatedAt         *time.Time               `json:"updatedAt,omitempty"`
}

// CollectTransactionStatus (enum — note: no REFUNDED).
type CollectTransactionStatus string

const (
	CollectTransactionStatusPending   CollectTransactionStatus = "PENDING"
	CollectTransactionStatusSuccess   CollectTransactionStatus = "SUCCESS"
	CollectTransactionStatusFailed    CollectTransactionStatus = "FAILED"
	CollectTransactionStatusCancelled CollectTransactionStatus = "CANCELLED"
)

// AccountCreditSettlement is the inner result of settle(): the (mutated) credit and
// the covered amount in the credit's base currency.
type AccountCreditSettlement struct {
	AccountCredit *AccountCredit
	Settled       decimal.Decimal
}

// Adjustment types (BillAdjustment.AdjustmentType).
const (
	AdjustmentTypeSavingsContract     = "SAVINGS_CONTRACT"
	AdjustmentTypePriceAdjustmentRule = "PRICE_ADJUSTMENT_RULE"
)

// BillAdjustment is a signed amount applied to a bill by a savings
// contract (keyed by ContractID) or a price-adjustment rule (keyed by PriceAdjustmentRuleID).
// Money serializes as a JSON number (applied_json.go); the rest are omitempty.
type BillAdjustment struct {
	Amount                  *decimal.Decimal `json:"amount,omitempty"`
	Type                    string           `json:"type,omitempty"`
	Description             string           `json:"description,omitempty"`
	ContractID              string           `json:"contractId,omitempty"`
	SavingsPlanName         string           `json:"savingsPlanName,omitempty"`
	StartDateContract       *time.Time       `json:"startDateContract,omitempty"`
	EndDateContract         *time.Time       `json:"endDateContract,omitempty"`
	PriceAdjustmentRuleID   string           `json:"priceAdjustmentRuleId,omitempty"`
	PriceAdjustmentRuleName string           `json:"priceAdjustmentRuleName,omitempty"`
}
