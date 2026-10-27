package admin

import (
	"context"
	"errors"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/shopspring/decimal"
)

// accountcredit_repo.go holds the AccountCredit-specific *Repo methods the account-credit endpoints
// need that the generic crud.go helpers do not cover: the createdAt-DESC list, the billing
// profile currency + billing-config base-currency lookups for create, and the doc builders that keep
// money as decimal.Decimal (never float — the pgdoc decimal codec stores decimals as a decimal string in jsonb).

// errAccountCreditFXSeam marks the cross-currency exchange-rate lookup (the create flow
// would otherwise call the external ExchangeClient). The handler maps it to 501.
var errAccountCreditFXSeam = errors.New("account credit cross-currency exchange rate not implemented")

// AccountCreditsByBillingProfile returns all accountCredit
// docs for the profile, sorted createdAt DESC. raw document (caller shapeDoc's). Never nil.
func (r *Repo) AccountCreditsByBillingProfile(ctx context.Context, billingProfileID string) ([]pgdoc.M, error) {
	out := []pgdoc.M{}
	if err := r.c("accountCredit").Find(ctx,
		pgdoc.M{"billingProfileId": billingProfileID}, &out,
		pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		return nil, err
	}
	return out, nil
}

// BillingProfileCurrency reads billingProfile.currency for the id. found=false when no such
// profile (→ the handler's 404).
func (r *Repo) BillingProfileCurrency(ctx context.Context, id string) (currency string, found bool, err error) {
	var doc struct {
		Currency string `json:"currency"`
	}
	found, e := r.c("billingProfile").Get(ctx, id, &doc)
	if e != nil || !found {
		return "", false, e
	}
	return doc.Currency, true, nil
}

// AccountCreditBaseCurrency reads billingConfiguration.baseCurrency; "" when unconfigured.
func (r *Repo) AccountCreditBaseCurrency(ctx context.Context) (string, error) {
	var doc struct {
		BaseCurrency string `json:"baseCurrency"`
	}
	found, err := r.c("billingConfiguration").FindOne(ctx, nil, &doc)
	if err != nil || !found {
		return "", err
	}
	return doc.BaseCurrency, nil
}

// BuildAccountCreditDoc builds the stored JSON for a created credit:
//
//	amount, initialAmount  = amount
//	currency               = baseCurrency
//	invoiceCurrency        = profileCurrency
//	invoiceExchangeRate    = getExchangeRate(baseCurrency, invoiceCurrency) = ONE when equal
//	createdAt, updatedAt   = now
//
// Money is stored as a decimal string in jsonb (never float). Returns errAccountCreditFXSeam when the FX rate would
// require the external ExchangeClient (baseCurrency != invoiceCurrency).
func (r *Repo) BuildAccountCreditDoc(amount numberString, baseCurrency, profileCurrency string) (pgdoc.M, error) {
	amt, err := decimalFromNumber(amount)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	doc := pgdoc.M{
		"amount":          amt,
		"initialAmount":   amt,
		"currency":        baseCurrency,
		"invoiceCurrency": profileCurrency,
		"createdAt":       now,
		"updatedAt":       now,
	}
	if baseCurrency == profileCurrency {
		one, _ := decimal.NewFromString("1")
		doc["invoiceExchangeRate"] = one
	} else {
		return nil, errAccountCreditFXSeam
	}
	return doc, nil
}

// AccountCreditUpdateFields builds the $set map for an update: the 4
// fields (invoiceCurrency, initialAmount, amount, currency). Blank optional
// strings are omitted so a re-serialization drops them (a null field); money is
// stored as a decimal string.
func (r *Repo) AccountCreditUpdateFields(req updateAccountCreditReq) (pgdoc.M, error) {
	set := pgdoc.M{}
	if req.InvoiceCurrency != "" {
		set["invoiceCurrency"] = req.InvoiceCurrency
	}
	if req.Currency != "" {
		set["currency"] = req.Currency
	}
	if string(req.InitialAmount) != "" {
		v, err := decimalFromNumber(req.InitialAmount)
		if err != nil {
			return nil, err
		}
		set["initialAmount"] = v
	}
	if string(req.Amount) != "" {
		v, err := decimalFromNumber(req.Amount)
		if err != nil {
			return nil, err
		}
		set["amount"] = v
	}
	return set, nil
}

// numberString is satisfied by json.Number (its String() is the raw numeric text).
type numberString interface{ String() string }

// decimalFromNumber parses a numeric string ("" → 0) into a decimal.Decimal, never float.
func decimalFromNumber(n numberString) (decimal.Decimal, error) {
	s := ""
	if n != nil {
		s = n.String()
	}
	if s == "" {
		s = "0"
	}
	return decimal.NewFromString(s)
}
