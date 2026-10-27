package billing

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCountries verifies the embedded dataset projects to the served shape: 250
// entries, Iceland present, Kosovo (XK) numeric coerced to 0 (null in source).
func TestCountries(t *testing.T) {
	cs := Countries()
	if len(cs) != 250 {
		t.Fatalf("countries len = %d, want 250", len(cs))
	}
	byCode := map[string]Country{}
	for _, c := range cs {
		byCode[c.Alpha2] = c
	}
	is := byCode["IS"]
	if is.Name != "Iceland" || is.Alpha3 != "ISL" || is.Numeric != 352 {
		t.Errorf("IS = %+v, want {Iceland ISL 352}", is)
	}
	if xk, ok := byCode["XK"]; !ok || xk.Numeric != 0 {
		t.Errorf("XK = %+v ok=%v, want numeric 0", xk, ok)
	}
}

// TestE164MobileNumber pins the phone normalization:
// "0721234567" + region "RO" → "+40721234567".
func TestE164MobileNumber(t *testing.T) {
	got, err := e164MobileNumber("0721234567", "RO")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "+40721234567" {
		t.Errorf("e164 = %q, want %q", got, "+40721234567")
	}
	if _, err := e164MobileNumber("not-a-number", "RO"); err == nil {
		t.Error("want parse error for garbage input")
	}
	if _, err := e164MobileNumber("123", "RO"); err == nil {
		t.Error("want invalid error for impossible number")
	}
}

func TestValidateBillingDetails(t *testing.T) {
	full := &BillingProfile{Address: "a", FirstName: "f", LastName: "l", City: "c", Phone: "+40721234567", Country: "RO"}
	if !validateBillingDetails(full) {
		t.Error("complete personal profile should validate")
	}
	if validateBillingDetails(&BillingProfile{FirstName: "f", LastName: "l", City: "c", Phone: "+40721234567", Country: "RO"}) {
		t.Error("missing address must fail")
	}
	// company without companyName fails; with it, passes
	company := *full
	company.Company = true
	if validateBillingDetails(&company) {
		t.Error("company without companyName must fail")
	}
	company.CompanyName = "Acme"
	if !validateBillingDetails(&company) {
		t.Error("company with companyName should validate")
	}
	// whitespace-only counts as blank
	if validateBillingDetails(&BillingProfile{Address: "  ", FirstName: "f", LastName: "l", City: "c", Phone: "p", Country: "RO"}) {
		t.Error("whitespace address must be treated as blank")
	}
}

// TestCanActivateNoFlow: with no autoActivationFlow configured (the default
// seed), filling billing details is the only REQUIRED constraint and it passes →
// a NEW profile activates.
func TestCanActivateNoFlow(t *testing.T) {
	bp := &BillingProfile{Status: StatusNew}
	if !canActivate(bp, SourceFillingBillingDetails, nil) {
		t.Error("want canActivate=true when no auto-activation flow is configured")
	}
	if !canActivate(bp, SourceAdmin, nil) {
		t.Error("admin source always activates")
	}
}

func TestCanActivateFromConstraints(t *testing.T) {
	cases := []struct {
		name string
		in   []constraintResult
		want bool
	}{
		{"all required pass", []constraintResult{{true, ConstraintRequired}, {true, ConstraintRequired}}, true},
		{"a required fails", []constraintResult{{true, ConstraintRequired}, {false, ConstraintRequired}}, false},
		{"disabled ignored", []constraintResult{{true, ConstraintRequired}, {false, ConstraintDisabled}}, true},
		{"alternatives none pass", []constraintResult{{true, ConstraintRequired}, {false, ConstraintAlternative}, {false, ConstraintAlternative}}, false},
		{"alternatives one passes", []constraintResult{{true, ConstraintRequired}, {false, ConstraintAlternative}, {true, ConstraintAlternative}}, true},
	}
	for _, c := range cases {
		if got := canActivateFromConstraints(c.in); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// TestSummaryActivatedShape mirrors the live PUT response: status ACTIVE, billing
// fields present, activationConstraints with the FILLING_BILLING_DETAILS source,
// hasBillingDetails true, language RO (RON currency).
func TestSummaryActivatedShape(t *testing.T) {
	now := time.Now()
	bp := &BillingProfile{
		ID: "bp1", OrganizationID: "org1", Status: StatusActive,
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Currency: "RON",
		Address: "10 Analytical St", City: "Cluj-Napoca", County: "Cluj", Country: "RO",
		ZipCode: "400001", Phone: "+40721234567",
		CustomInfo: map[string]any{}, Verifications: []any{},
		PricePlanConfig:       &PricePlanConfiguration{PricePlanIDs: []string{}, IncludePublicPricePlans: true},
		Contacts:              []Contact{{Name: "Ada Lovelace", Email: "ada@example.com"}},
		ActivationConstraints: []ActivationConstraintPassed{{Source: SourceFillingBillingDetails, PassedAt: &now}},
		ActivatedAt:           &now,
	}
	b, _ := json.Marshal(ToSummary(bp))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["status"] != "ACTIVE" || m["hasBillingDetails"] != true || m["language"] != "RO" {
		t.Errorf("status/hasBillingDetails/language wrong: %v / %v / %v", m["status"], m["hasBillingDetails"], m["language"])
	}
	for k, want := range map[string]string{"address": "10 Analytical St", "city": "Cluj-Napoca", "county": "Cluj", "country": "RO", "zipCode": "400001", "phone": "+40721234567", "currency": "RON"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	acs, ok := m["activationConstraints"].([]any)
	if !ok || len(acs) != 1 {
		t.Fatalf("activationConstraints = %v", m["activationConstraints"])
	}
	if ac := acs[0].(map[string]any); ac["source"] != SourceFillingBillingDetails {
		t.Errorf("activation source = %v", ac["source"])
	}
	// optional empty fields stay omitted (company is false → companyName/vatCode absent)
	for _, k := range []string{"companyName", "vatCode"} {
		if _, present := m[k]; present {
			t.Errorf("%s should be omitted when empty", k)
		}
	}
}

func TestSummaryFreshShape(t *testing.T) {
	bp := &BillingProfile{
		ID: "bp1", OrganizationID: "org1", Status: StatusNew,
		Email: "ada@example.com", FirstName: "Ada", LastName: "Lovelace", Currency: "USD",
		CustomInfo: map[string]any{}, Verifications: []any{},
		PricePlanConfig: &PricePlanConfiguration{PricePlanIDs: []string{}, IncludePublicPricePlans: true},
		Contacts:        []Contact{{Name: "Ada Lovelace", Email: "ada@example.com"}},
	}
	b, err := json.Marshal(ToSummary(bp))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// primitives always present, false for fresh
	for _, k := range []string{"company", "taxPayer", "overwriteSuspension", "hasBillingDetails"} {
		if v, ok := m[k]; !ok || v != false {
			t.Errorf("%s: want present false, got ok=%v val=%v", k, ok, v)
		}
	}
	// financials are JSON numbers == 0 (not strings)
	for _, k := range []string{"balance", "accountCredit", "currentMonthUsage", "promotionalCredit"} {
		v, ok := m[k]
		if !ok {
			t.Errorf("%s missing", k)
			continue
		}
		if f, isNum := v.(float64); !isNum || f != 0 {
			t.Errorf("%s: want number 0, got %v (%T)", k, v, v)
		}
	}
	// validationStatus omitted (null for fresh)
	if _, ok := m["validationStatus"]; ok {
		t.Error("validationStatus should be omitted when nil")
	}
	// non-null empty collections kept
	if ci, ok := m["customInfo"].(map[string]any); !ok || len(ci) != 0 {
		t.Errorf("customInfo: want {}, got %v", m["customInfo"])
	}
	if vf, ok := m["verifications"].([]any); !ok || len(vf) != 0 {
		t.Errorf("verifications: want [], got %v", m["verifications"])
	}
	// scalar profile fields
	if m["status"] != "NEW" || m["email"] != "ada@example.com" || m["firstName"] != "Ada" || m["lastName"] != "Lovelace" {
		t.Errorf("profile fields wrong: %v", m)
	}
	// contacts + pricePlanConfig present
	contacts, _ := m["contacts"].([]any)
	if len(contacts) != 1 {
		t.Errorf("contacts = %v", m["contacts"])
	}
	ppc, _ := m["pricePlanConfig"].(map[string]any)
	if ppc == nil || ppc["includePublicPricePlans"] != true {
		t.Errorf("pricePlanConfig = %v", m["pricePlanConfig"])
	}
}
