package billing

// ConstraintType enumerates the activation constraint types.
const (
	ConstraintAlternative = "ALTERNATIVE"
	ConstraintDisabled    = "DISABLED"
	ConstraintRequired    = "REQUIRED"
)

// AutoActivationFlow is the auto-activation flow config
// subdocument (billingConfiguration.autoActivationFlow). When the whole subdoc is
// absent (the default seed), isAutoActivationConfigured is false (flow == nil),
// and filling billing details alone activates a NEW profile.
type AutoActivationFlow struct {
	AutoActivationEnabled    bool   `json:"autoActivationEnabled"`
	KYC                      string `json:"kyc,omitempty"`
	PaymentMethod            string `json:"paymentMethod,omitempty"`
	PaymentMethodCard        string `json:"paymentMethodCard,omitempty"`
	PaymentMethodDeposit     string `json:"paymentMethodDeposit,omitempty"`
	BillingProfileValidation string `json:"billingProfileValidation,omitempty"`
}

func isDisabledCT(ct string) bool { return ct == ConstraintDisabled }

type constraintResult struct {
	passed bool
	ctype  string
}

// canActivate evaluates, for a
// non-admin source, every activation constraint; all REQUIRED must pass,
// and if any ALTERNATIVE exists at least one must pass. flow == nil ⇒ only the
// filling-billing-details constraint is REQUIRED (and always passes), so a NEW
// profile activates under the default seed.
func canActivate(bp *BillingProfile, source string, flow *AutoActivationFlow) bool {
	if source == SourceAdmin || source == SourceAdminAPI {
		return true
	}
	results := []constraintResult{
		fillingBillingDetailsConstraint(flow),
		billingProfileValidationConstraint(bp, flow),
		kycConstraint(bp, flow),
		paymentMethodConstraint(flow),
	}
	return canActivateFromConstraints(results)
}

// canActivateFromConstraints applies the REQUIRED/ALTERNATIVE rule to the constraint results.
func canActivateFromConstraints(results []constraintResult) bool {
	for _, r := range results {
		if r.ctype == ConstraintRequired && !r.passed {
			return false
		}
	}
	hasAlt, altPassed := false, false
	for _, r := range results {
		if r.ctype == ConstraintAlternative {
			hasAlt = true
			if r.passed {
				altPassed = true
			}
		}
	}
	if hasAlt {
		return altPassed
	}
	return true
}

// fillingBillingDetailsConstraint is the filling-billing-details constraint:
// isPassed is always true; the type is REQUIRED unless auto-activation is
// configured with KYC/payment/validation enabled. There are no KYC integrations
// in this build, so the KYC clause of areAllDisabled is always satisfied.
func fillingBillingDetailsConstraint(flow *AutoActivationFlow) constraintResult {
	ctype := ConstraintRequired
	if flow != nil {
		// areAllDisabled: (kyc disabled || no KYC integrations) && payment disabled && validation disabled.
		// No KYC integrations are wired → the kyc clause holds.
		allDisabled := isDisabledCT(flow.PaymentMethod) && isDisabledCT(flow.BillingProfileValidation)
		if !allDisabled {
			ctype = ConstraintDisabled
		}
	}
	return constraintResult{passed: true, ctype: ctype}
}

// billingProfileValidationConstraint is the billing-profile-validation constraint:
// isPassed = profile validated (no validation collection in this slice → false);
// type = DISABLED unless configured. Only consulted when configured (flow != nil).
func billingProfileValidationConstraint(_ *BillingProfile, flow *AutoActivationFlow) constraintResult {
	if flow == nil {
		return constraintResult{passed: false, ctype: ConstraintDisabled}
	}
	return constraintResult{passed: false, ctype: flow.BillingProfileValidation}
}

// kycConstraint is the know-your-customer constraint: when not configured,
// isPassed = true and type = DISABLED.
func kycConstraint(bp *BillingProfile, flow *AutoActivationFlow) constraintResult {
	if flow == nil {
		return constraintResult{passed: true, ctype: ConstraintDisabled}
	}
	passed := true
	if !isDisabledCT(flow.KYC) {
		passed = allVerificationsVerified(bp)
	}
	return constraintResult{passed: passed, ctype: flow.KYC}
}

// paymentMethodConstraint is the payment-method constraint: when not
// configured, isPassed = true and type = DISABLED. The card/deposit checks land
// with the payments slice; until then a configured payment constraint cannot
// pass (no cards/deposits modeled).
func paymentMethodConstraint(flow *AutoActivationFlow) constraintResult {
	if flow == nil {
		return constraintResult{passed: true, ctype: ConstraintDisabled}
	}
	passed := isDisabledCT(flow.PaymentMethod) // not-disabled payment needs cards/deposits → cannot pass yet
	return constraintResult{passed: passed, ctype: flow.PaymentMethod}
}

// allVerificationsVerified performs the KYC check: a nil/empty verifications list
// passes (all-match over an empty set is true). Verification objects aren't typed
// yet, so a non-empty list is treated as unverified until the KYC slice lands.
func allVerificationsVerified(bp *BillingProfile) bool {
	return len(bp.Verifications) == 0
}
