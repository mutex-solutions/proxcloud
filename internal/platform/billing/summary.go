package billing

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Summary is the BillingSummary (BillingProfile + computed financials).
// The financials are JSON numbers (big-decimal → number, e.g. 0),
// so we use json.Number, not money.Money (which serializes as a string).
type Summary struct {
	bp                *BillingProfile
	balance           json.Number
	accountCredit     json.Number
	currentMonthUsage json.Number
	promotionalCredit json.Number
	hasBillingDetails bool
	validationStatus  *string
}

// ToSummary builds the summary for a profile. With no rating/credits/bills
// yet, the financials are all zero and validationStatus is nil — matching a
// freshly-created profile. Non-zero financials arrive with the rating slice.
func ToSummary(bp *BillingProfile) Summary {
	z := json.Number("0")
	return Summary{
		bp: bp, balance: z, accountCredit: z, currentMonthUsage: z, promotionalCredit: z,
		hasBillingDetails: validateBillingDetails(bp),
	}
}

// WithFinancials computes + sets the BillingSummary financials from the credit collections:
// accountCredit = available account-credit
// total, promotionalCredit = available promo total, balance = credits − unpaid bills, currentMonthUsage
// = the current-month bill's net (the current-month usage). Best-effort — a query
// error leaves that field 0. Without it the summary's financials read 0 even when the profile holds a
// balance (the add-funds bug).
func (s Summary) WithFinancials(ctx context.Context, repo *Repo, now time.Time) Summary {
	if s.bp == nil || s.bp.ID == "" {
		return s
	}
	if v, err := repo.AccountCreditTotal(ctx, s.bp.ID); err == nil {
		s.accountCredit = json.Number(v.String())
	}
	if v, err := repo.AvailablePromotionalTotal(ctx, s.bp.ID, now); err == nil {
		s.promotionalCredit = json.Number(v.String())
	}
	if v, err := NewBalanceService(repo).CurrentBalance(ctx, s.bp.ID, now); err == nil {
		s.balance = json.Number(v.String())
	}
	if bills, err := repo.BillsByBillingProfile(ctx, s.bp.ID); err == nil {
		cur, _ := MonthlyBillCosts(bills, now)
		s.currentMonthUsage = json.Number(cur.String())
	}
	return s
}

// validateBillingDetails validates the required billing-details fields:
// requires non-blank address/firstName/lastName/city/phone/country (+ companyName
// when a company). isBlank counts null/empty/whitespace-only as missing.
func validateBillingDetails(bp *BillingProfile) bool {
	blank := func(s string) bool { return strings.TrimSpace(s) == "" }
	required := blank(bp.Address) || blank(bp.FirstName) || blank(bp.LastName) ||
		blank(bp.City) || blank(bp.Phone) || blank(bp.Country)
	if bp.Company {
		required = required || blank(bp.CompanyName)
	}
	return !required
}

// billingLanguage returns the billing language: RON currency → RO, else EN.
func billingLanguage(currency string) string {
	if currency == "RON" {
		return "RO"
	}
	return "EN"
}

// MarshalJSON emits the BillingSummary shape under a null-omitting policy:
// nullable fields omitted; primitives (company/taxPayer/overwriteSuspension/
// hasBillingDetails) + the 4 financial numbers always present; customInfo/
// verifications kept as non-null empty {}/[].
func (s Summary) MarshalJSON() ([]byte, error) {
	bp := s.bp
	ci := bp.CustomInfo
	if ci == nil {
		ci = map[string]any{}
	}
	vf := bp.Verifications
	if vf == nil {
		vf = []any{}
	}
	return json.Marshal(struct {
		ID                    string                       `json:"id,omitempty"`
		OrganizationID        string                       `json:"organizationId,omitempty"`
		Status                string                       `json:"status,omitempty"`
		Email                 string                       `json:"email,omitempty"`
		FirstName             string                       `json:"firstName,omitempty"`
		LastName              string                       `json:"lastName,omitempty"`
		FullName              string                       `json:"fullName"`
		Language              string                       `json:"language"`
		Currency              string                       `json:"currency,omitempty"`
		Company               bool                         `json:"company"`
		CompanyName           string                       `json:"companyName,omitempty"`
		VatCode               string                       `json:"vatCode,omitempty"`
		TaxPayer              bool                         `json:"taxPayer"`
		Address               string                       `json:"address,omitempty"`
		City                  string                       `json:"city,omitempty"`
		County                string                       `json:"county,omitempty"`
		Country               string                       `json:"country,omitempty"`
		ZipCode               string                       `json:"zipCode,omitempty"`
		Phone                 string                       `json:"phone,omitempty"`
		DefaultCardID         string                       `json:"defaultCardId,omitempty"`
		OverwriteSuspension   bool                         `json:"overwriteSuspension"`
		Contacts              []Contact                    `json:"contacts,omitempty"`
		PricePlanConfig       *PricePlanConfiguration      `json:"pricePlanConfig,omitempty"`
		CustomInfo            map[string]any               `json:"customInfo"`
		Verifications         []any                        `json:"verifications"`
		ActivationConstraints []ActivationConstraintPassed `json:"activationConstraints,omitempty"`
		CreatedAt             *time.Time                   `json:"createdAt,omitempty"`
		UpdatedAt             *time.Time                   `json:"updatedAt,omitempty"`
		ActivatedAt           *time.Time                   `json:"activatedAt,omitempty"`
		Balance               json.Number                  `json:"balance"`
		AccountCredit         json.Number                  `json:"accountCredit"`
		CurrentMonthUsage     json.Number                  `json:"currentMonthUsage"`
		PromotionalCredit     json.Number                  `json:"promotionalCredit"`
		HasBillingDetails     bool                         `json:"hasBillingDetails"`
		ValidationStatus      *string                      `json:"validationStatus,omitempty"`
	}{
		ID: bp.ID, OrganizationID: bp.OrganizationID, Status: bp.Status, Email: bp.Email,
		FirstName: bp.FirstName, LastName: bp.LastName,
		FullName: bp.FirstName + " " + bp.LastName, Language: billingLanguage(bp.Currency), Currency: bp.Currency,
		Company: bp.Company, CompanyName: bp.CompanyName, VatCode: bp.VatCode, TaxPayer: bp.TaxPayer,
		Address: bp.Address, City: bp.City, County: bp.County, Country: bp.Country, ZipCode: bp.ZipCode, Phone: bp.Phone,
		DefaultCardID:       bp.DefaultCardID,
		OverwriteSuspension: bp.OverwriteSuspension,
		Contacts:            bp.Contacts, PricePlanConfig: bp.PricePlanConfig, CustomInfo: ci, Verifications: vf,
		ActivationConstraints: bp.ActivationConstraints,
		CreatedAt:             bp.CreatedAt, UpdatedAt: bp.UpdatedAt, ActivatedAt: bp.ActivatedAt,
		Balance: s.balance, AccountCredit: s.accountCredit, CurrentMonthUsage: s.currentMonthUsage,
		PromotionalCredit: s.promotionalCredit, HasBillingDetails: s.hasBillingDetails, ValidationStatus: s.validationStatus,
	})
}
