package admin

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestBillingProfileIDNotFound pins the exact 404:
// "Billing profile with id %s not found. " — interpolated id, trailing space, 404 status/code.
func TestBillingProfileIDNotFound(t *testing.T) {
	err := billingProfileIDNotFound("bp-9")
	want := "Billing profile with id bp-9 not found. "
	if err.Msg != want {
		t.Errorf("message=%q want %q", err.Msg, want)
	}
	if err.Status != http.StatusNotFound || err.Code != http.StatusNotFound {
		t.Errorf("status/code=%d/%d want 404/404", err.Status, err.Code)
	}
}

// TestBillingProfileUpdateSetMap verifies the editable-field mapping: a present field (even zero/"")
// is set; an absent field is omitted (left untouched on save).
func TestBillingProfileUpdateSetMap(t *testing.T) {
	body := `{"firstName":"Ada","company":true,"vatCode":"","email":"a@b.c","taxPayer":false}`
	var req billingProfileUpdateReq
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	m := req.setMap()
	if m["firstName"] != "Ada" {
		t.Errorf("firstName=%v want Ada", m["firstName"])
	}
	if m["company"] != true {
		t.Errorf("company=%v want true", m["company"])
	}
	// present empty string IS set (clears the field) — not omitted.
	if v, ok := m["vatCode"]; !ok || v != "" {
		t.Errorf("vatCode present-empty: got (%v,%v) want (\"\",true)", v, ok)
	}
	if m["email"] != "a@b.c" {
		t.Errorf("email=%v want a@b.c", m["email"])
	}
	// present false bool IS set.
	if v, ok := m["taxPayer"]; !ok || v != false {
		t.Errorf("taxPayer: got (%v,%v) want (false,true)", v, ok)
	}
	// absent fields are omitted.
	for _, k := range []string{"lastName", "companyName", "bank", "iban", "phone", "zipCode",
		"address", "city", "county", "country", "currency", "pricePlanConfig"} {
		if _, ok := m[k]; ok {
			t.Errorf("absent field %q should be omitted, got %v", k, m[k])
		}
	}
}

// TestTaxConfigurationDoc verifies the tax-config sub-doc: bool always present; taxRuleId omitted when
// absent, set when present.
func TestTaxConfigurationDoc(t *testing.T) {
	var withRule taxConfigurationReq
	_ = json.Unmarshal([]byte(`{"disableAutomaticTaxCalculation":true,"taxRuleId":"r1"}`), &withRule)
	d := withRule.doc()
	if d["disableAutomaticTaxCalculation"] != true {
		t.Errorf("disable=%v want true", d["disableAutomaticTaxCalculation"])
	}
	if d["taxRuleId"] != "r1" {
		t.Errorf("taxRuleId=%v want r1", d["taxRuleId"])
	}

	var noRule taxConfigurationReq
	_ = json.Unmarshal([]byte(`{"disableAutomaticTaxCalculation":false}`), &noRule)
	d2 := noRule.doc()
	if d2["disableAutomaticTaxCalculation"] != false {
		t.Errorf("disable=%v want false", d2["disableAutomaticTaxCalculation"])
	}
	if _, ok := d2["taxRuleId"]; ok {
		t.Errorf("taxRuleId should be omitted when absent, got %v", d2["taxRuleId"])
	}
}

// TestMessageTemplateConfigDoc verifies disabled is always present and messageTemplates is passed
// through (as decoded JSON) only when present.
func TestMessageTemplateConfigDoc(t *testing.T) {
	var req messageTemplateConfigReq
	_ = json.Unmarshal([]byte(`{"disabled":true,"messageTemplates":[{"key":"x"}]}`), &req)
	d := req.doc()
	if d["disabled"] != true {
		t.Errorf("disabled=%v want true", d["disabled"])
	}
	if _, ok := d["messageTemplates"]; !ok {
		t.Errorf("messageTemplates should be present")
	}

	var noTpl messageTemplateConfigReq
	_ = json.Unmarshal([]byte(`{"disabled":false}`), &noTpl)
	d2 := noTpl.doc()
	if _, ok := d2["messageTemplates"]; ok {
		t.Errorf("messageTemplates should be omitted when absent")
	}
}

// TestIsValidBillingProfileStatus pins the BillingProfile.Status enum set (NEW,ACTIVE,SUSPENDED,SKIP).
func TestIsValidBillingProfileStatus(t *testing.T) {
	for _, ok := range []string{"NEW", "ACTIVE", "SUSPENDED", "SKIP"} {
		if !isValidBillingProfileStatus(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "active", "DELETED", "PENDING"} {
		if isValidBillingProfileStatus(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

// TestIsValidValidationStatus pins the BillingProfileValidationStatus enum set.
func TestIsValidValidationStatus(t *testing.T) {
	for _, ok := range []string{"PENDING", "APPROVED", "REJECTED"} {
		if !isValidValidationStatus(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "approved", "DONE"} {
		if isValidValidationStatus(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
