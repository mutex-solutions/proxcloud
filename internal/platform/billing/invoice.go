package billing

import (
	"context"
	"fmt"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// invoice.go — the issuer identity printed on bill documents (the billingConfiguration
// company/address block) + the sequential invoice numbering stamped when a bill leaves OPEN.

// IssuerInfo is the seller identity a tax invoice must carry (ACRA s144 / IRAS tax-invoice
// elements): legal name, address, UEN and — when GST-registered — the GST registration number.
// Missing configuration → empty fields (the renderer skips blank lines).
type IssuerInfo struct {
	BusinessName          string
	UEN                   string
	GSTRegistrationNumber string
	GSTRegistered         bool
	Address               string
	City                  string
	ZipCode               string
	Country               string // alpha-2
	BaseCurrency          string
}

// issuerConfig is the slice of the billingConfiguration the issuer/notification reads need.
type issuerConfig struct {
	BaseCurrency string `json:"baseCurrency"`
	Company      struct {
		BusinessName          string `json:"businessName"`
		UEN                   string `json:"uen"`
		GSTRegistrationNumber string `json:"gstRegistrationNumber"`
		GSTRegistered         bool   `json:"gstRegistered"`
		FinanceEmail          string `json:"financeEmail"`
	} `json:"company"`
	Address struct {
		Address string `json:"address"`
		City    string `json:"city"`
		ZipCode string `json:"zipCode"`
		Country string `json:"country"`
	} `json:"address"`
}

// loadIssuerConfig reads the default billingConfiguration (else the first); found=false when none.
func (r *Repo) loadIssuerConfig(ctx context.Context) (issuerConfig, bool, error) {
	var cfg issuerConfig
	found, err := r.configs.FindOne(ctx, pgdoc.M{"defaultConfiguration": true}, &cfg)
	if err == nil && !found {
		found, err = r.configs.FindOne(ctx, pgdoc.M{}, &cfg)
	}
	return cfg, found, err
}

// FinanceEmail is the operator mailbox (`company.financeEmail`) that receives customer bank-transfer
// submissions; "" when unset.
func (r *Repo) FinanceEmail(ctx context.Context) (string, error) {
	cfg, _, err := r.loadIssuerConfig(ctx)
	return cfg.Company.FinanceEmail, err
}

// IssuerInfo reads the default billingConfiguration (else the first), zero value when none.
func (r *Repo) IssuerInfo(ctx context.Context) (IssuerInfo, error) {
	cfg, found, err := r.loadIssuerConfig(ctx)
	if err != nil || !found {
		return IssuerInfo{}, err
	}
	return IssuerInfo{
		BusinessName:          cfg.Company.BusinessName,
		UEN:                   cfg.Company.UEN,
		GSTRegistrationNumber: cfg.Company.GSTRegistrationNumber,
		GSTRegistered:         cfg.Company.GSTRegistered,
		Address:               cfg.Address.Address,
		City:                  cfg.Address.City,
		ZipCode:               cfg.Address.ZipCode,
		Country:               cfg.Address.Country,
		BaseCurrency:          cfg.BaseCurrency,
	}, nil
}

// FormatInvoiceNumber renders INV-<YYYY>-<6-digit zero-padded seq>.
func FormatInvoiceNumber(year int, seq int64) string {
	return fmt.Sprintf("INV-%04d-%06d", year, seq)
}

// NextInvoiceNumber mints the next sequential invoice number for the year of `now` (UTC), from
// the per-year "counters" doc via one atomic upsert → unique even with concurrent senders.
func (r *Repo) NextInvoiceNumber(ctx context.Context, now time.Time) (string, error) {
	year := now.UTC().Year()
	seq, err := r.counters.Increment(ctx, fmt.Sprintf("invoice-%04d", year), "seq")
	if err != nil {
		return "", err
	}
	return FormatInvoiceNumber(year, seq), nil
}

// InvoiceTaxRates selects the profile's tax rates for a bill document — the rates in force at the
// bill's issue instant (sentAt, else now), the same selection settlement applied — so the printed
// GST equals what was charged.
func InvoiceTaxRates(all []pricing.TaxRate, bp *BillingProfile, bill *pricing.Bill, now time.Time) []pricing.TaxRate {
	at := now
	if bill.SentAt != nil {
		at = *bill.SentAt
	}
	return pricing.SelectTaxRates(all, bp.Country, bp.Company, at)
}
