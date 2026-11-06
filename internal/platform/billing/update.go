package billing

import (
	"context"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// Literal billing-details error messages (the
// default-locale values — trailing space preserved as in the resource bundle).
const (
	MsgFillToFieldBillingProfile = "You need to fill in all the fields for your billing profile. "
	MsgImpossibleChange          = "The change is not possible because there are unpaid invoices or credits in your account. "
)

// PopulateBillingData populates + validates the billing data:
// merge the request `input` onto the persisted `profile`, validating + normalizing
// (phone → E.164, currency resolved), then — on first fill of a NEW profile — run
// the auto-activation flow (NEW→ACTIVE + append activationConstraints + activatedAt)
// and persist. Returns the saved profile, or an *httpx.HTTPError (400) on invalid
// input / unparseable phone.
//
// Deferred (no effect on this response under the default seed):
//   - processConstraints bill/credit gates — no bills/credit exist until the rating
//     slice, so existsBill/existsAccountCredit are false (the impossibleChange guard
//     is present but unreachable for now).
//   - createValidationIfNeeded — only acts when billingConfiguration requires
//     validation, which the seed does not (→ validationStatus stays omitted).
//   - activation side-effects on OTHER aggregates (promo credit, project enable,
//     affiliate bonus, project bootstrap) — none mutate this profile's response.
func (r *Repo) PopulateBillingData(ctx context.Context, profile, input *BillingProfile) (*BillingProfile, error) {
	isFirstBillingFilling := strings.TrimSpace(profile.Address) == ""
	validInput := validateBillingDetails(input)

	if err := r.processConstraints(ctx, profile, input); err != nil {
		return nil, err
	}
	processBusinessBilling(profile, input)

	// contacts present in the body (even []) replace the profile's billing contacts; absent → kept.
	if input.Contacts != nil {
		contacts, err := NormalizeContacts(input.Contacts)
		if err != nil {
			return nil, err
		}
		profile.Contacts = contacts
	}

	if validInput {
		currency, _ := r.getCurrency(ctx, profile, input)
		profile.Currency = currency
		profile.Address = input.Address
		profile.City = input.City
		profile.County = input.County
		profile.Country = input.Country
		profile.FirstName = input.FirstName
		profile.LastName = input.LastName
		profile.Company = input.Company
		profile.CompanyName = input.CompanyName
		profile.VatCode = input.VatCode
		phone, err := e164MobileNumber(input.Phone, input.Country)
		if err != nil {
			return nil, err
		}
		profile.Phone = phone
		profile.ZipCode = input.ZipCode
	}

	if validInput && validateBillingDetails(profile) {
		// createValidationIfNeeded is a no-op under the seed (validation not required).
		if profile.Status == StatusNew && isFirstBillingFilling {
			if err := r.activateIfPossible(ctx, profile, SourceFillingBillingDetails); err != nil {
				return nil, err
			}
		}
		return r.Update(ctx, profile)
	}
	return nil, httpx.BadRequest(MsgFillToFieldBillingProfile)
}

// processBusinessBilling: for a company, copy the
// VAT code + company name. Redundant with the main copy block when the input is
// valid; kept for fidelity.
func processBusinessBilling(profile, input *BillingProfile) {
	if input.Company {
		profile.VatCode = input.VatCode
		profile.CompanyName = input.CompanyName
	}
}

// processConstraints blocks identity changes
// (country/company-flag/company-name/name) once sent bills or account credit exist.
// existsBill/existsAccountCredit are false until the rating slice → unreachable now.
func (r *Repo) processConstraints(_ context.Context, profile, input *BillingProfile) error {
	const existsBill, existsAccountCredit = false, false
	notBlank := func(s string) bool { return strings.TrimSpace(s) != "" }
	newCountry := notBlank(profile.Country) && profile.Country != input.Country
	newCompanyFlag := profile.Company != input.Company
	newCompanyName := notBlank(profile.CompanyName) && profile.CompanyName != input.CompanyName
	newName := !input.Company && notBlank(profile.LastName) && notBlank(profile.FirstName) &&
		(profile.LastName != input.LastName || profile.FirstName != input.FirstName)
	if (newCountry || newCompanyFlag || newCompanyName || newName) && (existsBill || existsAccountCredit) {
		return httpx.BadRequest(MsgImpossibleChange)
	}
	return nil
}

// getCurrency resolves the currency: the input currency if non-blank, else the profile's
// current currency (an admin-set non-base currency must survive a details save), else the
// configured base currency ("" when unconfigured).
func (r *Repo) getCurrency(ctx context.Context, profile, input *BillingProfile) (string, error) {
	if strings.TrimSpace(input.Currency) != "" {
		return input.Currency, nil
	}
	if profile != nil && strings.TrimSpace(profile.Currency) != "" {
		return profile.Currency, nil
	}
	return r.BaseCurrency(ctx)
}

// UpdateContacts replaces ONLY the profile's billing contacts (validated, de-duplicated) with a
// partial write — the tenant "invoice recipients" editor must not re-run the full billing-details
// validation nor rewrite the rest of the document.
func (r *Repo) UpdateContacts(ctx context.Context, profile *BillingProfile, contacts []Contact) (*BillingProfile, error) {
	normalized, err := NormalizeContacts(contacts)
	if err != nil {
		return nil, err
	}
	if normalized == nil {
		normalized = []Contact{}
	}
	now := time.Now().UTC()
	if _, err := r.profiles.SetByID(ctx, profile.ID, pgdoc.M{"contacts": normalized, "updatedAt": now}, nil); err != nil {
		return nil, err
	}
	profile.Contacts = normalized
	profile.UpdatedAt = &now
	return profile, nil
}

// activateIfPossible runs the activation flow for the
// profile-level mutations: when the auto-activation flow permits, append the passed
// constraint, flip NEW→ACTIVE, and stamp activatedAt. (Cross-aggregate side-effects
// are deferred — see PopulateBillingData.)
func (r *Repo) activateIfPossible(ctx context.Context, profile *BillingProfile, source string) error {
	flow, err := r.autoActivationFlow(ctx)
	if err != nil {
		return err
	}
	if !canActivate(profile, source, flow) {
		return nil
	}
	now := time.Now().UTC()
	if profile.ActivationConstraints == nil {
		profile.ActivationConstraints = []ActivationConstraintPassed{}
	}
	profile.ActivationConstraints = append(profile.ActivationConstraints, ActivationConstraintPassed{Source: source, PassedAt: &now})
	profile.Status = StatusActive
	profile.ActivatedAt = &now
	return nil
}
