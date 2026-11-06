// Package billing is the billing-profile slice. It provides the real BillingProfile
// domain + creation from an Organization + the client read
// endpoints, which return a BillingSummary (BillingProfile fields + computed
// financials, all zero/false for a fresh profile). Rating/credits/bills land in
// later slices.
package billing

import (
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// Status values for a billing profile.
const (
	StatusNew       = "NEW"
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusSkip      = "SKIP"
)

// PaymentMode values. PREPAID (the default when blank): the customer tops up and bills settle
// from the credit balance. POSTPAID (pay as you go): every issued bill is charged to the saved
// card the moment it is issued; a failed charge is e-mailed and retried by the collect cron.
const (
	PaymentModePrepaid  = "PREPAID"
	PaymentModePostpaid = "POSTPAID"
)

// ActivationSource values.
const (
	SourceDeposit               = "DEPOSIT"
	SourceCard                  = "CARD"
	SourceKYC                   = "KYC"
	SourceAdmin                 = "ADMIN"
	SourceFillingBillingDetails = "FILLING_BILLING_DETAILS"
	SourceAdminAPI              = "ADMIN_API"
	SourceValidation            = "VALIDATION"
)

// Contact is a billing-profile contact.
type Contact struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// PricePlanConfiguration is the price-plan configuration.
type PricePlanConfiguration struct {
	PricePlanIDs            []string `json:"pricePlanIds"`
	IncludePublicPricePlans bool     `json:"includePublicPricePlans"`
}

// Owner is the minimal creator info billing needs (passed in by the org service
// to avoid an org→billing→org import cycle).
type Owner struct {
	Sub       string
	Email     string
	FirstName string
	LastName  string
	FullName  string
}

// ActivationConstraintPassed is a record of
// an activation constraint that was satisfied. source is the ActivationSource enum
// name (serialized, compared); passedAt is a timestamp.
type ActivationConstraintPassed struct {
	Source   string     `json:"source"`
	PassedAt *time.Time `json:"passedAt,omitempty"`
}

// BillingProfile is the persisted document (collection "billingProfile"). The
// create path + the billing-details fields populated by the PUT update slice are
// modeled here; the remaining optional fields (tax-config/…)
// land with later slices and only matter if seed/config populates them.
// ProjectProvisioningQuota is the per-billing-profile project quota;
// the platform-level default lives on PlatformConfiguration.
type ProjectProvisioningQuota struct {
	Enabled bool `json:"enabled"`
	Limit   int  `json:"limit"`
}

// ResellerConfig is the reseller configuration (only the enabled flag matters here —
// isReseller() = reseller != null && reseller.isEnabled(); the dunning fan-out skips resellers).
type ResellerConfig struct {
	Enabled bool `json:"enabled"`
}

type BillingProfile struct {
	ID                   string `json:"id,omitempty"`
	OrganizationID       string `json:"organizationId,omitempty"`
	Sub                  string `json:"sub,omitempty"`         // the (single) member's IdP sub
	AffiliateID          string `json:"affiliateId,omitempty"` // referral source — gates the sign-up bonus
	Status               string `json:"status,omitempty"`
	Email                string `json:"email,omitempty"`
	FirstName            string `json:"firstName,omitempty"`
	LastName             string `json:"lastName,omitempty"`
	Currency             string `json:"currency,omitempty"`
	Company              bool   `json:"company"`
	CompanyName          string `json:"companyName,omitempty"`
	VatCode              string `json:"vatCode,omitempty"`
	TaxPayer             bool   `json:"taxPayer"`
	Address              string `json:"address,omitempty"`
	City                 string `json:"city,omitempty"`
	County               string `json:"county,omitempty"`
	Country              string `json:"country,omitempty"`
	ZipCode              string `json:"zipCode,omitempty"`
	Phone                string `json:"phone,omitempty"`
	IdentityValidationID string `json:"identityValidationId,omitempty"`
	DefaultCardID        string `json:"defaultCardId,omitempty"`
	// PaymentMode is PREPAID (blank = PREPAID) or POSTPAID — see the PaymentMode constants.
	PaymentMode string `json:"paymentMode,omitempty"`
	// PaymentGatewayID is the customer's preferred payment-gateway integration (a console default
	// for deposits / new cards); a charge always runs on the gateway the card was registered with.
	PaymentGatewayID         string                                    `json:"paymentGatewayId,omitempty"`
	ProjectProvisioningQuota *ProjectProvisioningQuota                 `json:"projectProvisioningQuota,omitempty"`
	OverwriteSuspension      bool                                      `json:"overwriteSuspension"`
	SuspensionConfiguration  *pricing.BillingAutomaticSuspensionConfig `json:"suspensionConfiguration,omitempty"`
	Reseller                 *ResellerConfig                           `json:"reseller,omitempty"`
	Contacts                 []Contact                                 `json:"contacts,omitempty"`
	PricePlanConfig          *PricePlanConfiguration                   `json:"pricePlanConfig,omitempty"`
	CustomInfo               map[string]any                            `json:"customInfo,omitempty"`
	Verifications            []any                                     `json:"verifications,omitempty"`
	ActivationConstraints    []ActivationConstraintPassed              `json:"activationConstraints,omitempty"`
	CreatedAt                *time.Time                                `json:"createdAt,omitempty"`
	UpdatedAt                *time.Time                                `json:"updatedAt,omitempty"`
	ActivatedAt              *time.Time                                `json:"activatedAt,omitempty"`
}

// IsPostpaid reports whether the profile pays as it goes (bills are charged to the card on issue).
func (bp *BillingProfile) IsPostpaid() bool { return bp.PaymentMode == PaymentModePostpaid }

// EffectivePaymentMode is the profile's payment mode with the PREPAID default applied.
func (bp *BillingProfile) EffectivePaymentMode() string {
	if bp.PaymentMode == "" {
		return PaymentModePrepaid
	}
	return bp.PaymentMode
}

// Restricted is the billing-profile-restricted response. reason
// is omitted when null; restricted (primitive bool) and billingProfileId always emit.
type Restricted struct {
	BillingProfileID string `json:"billingProfileId"`
	Restricted       bool   `json:"restricted"`
	Reason           string `json:"reason,omitempty"`
}

// IdentityValidation is the persisted document ("identityValidation"). The `document`
// field is intentionally not modeled: getAccountValidation always nulls it before
// returning (setDocument(null)) → omitted. An empty value marshals to
// `{}` (an empty identity validation for a profile with no
// identityValidationId). createdAt/updatedAt are timestamps.
type IdentityValidation struct {
	ID               string     `json:"id,omitempty"`
	Sub              string     `json:"sub,omitempty"`
	BillingProfileID string     `json:"billingProfileId,omitempty"`
	Country          string     `json:"country,omitempty"`
	Status           string     `json:"status,omitempty"`
	CreatedAt        *time.Time `json:"createdAt,omitempty"`
	UpdatedAt        *time.Time `json:"updatedAt,omitempty"`
}
