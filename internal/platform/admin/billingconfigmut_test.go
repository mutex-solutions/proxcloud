package admin

import (
	"encoding/json"
	"testing"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

func TestBillingConfigReqDoc_OmitsBlankOptionals(t *testing.T) {
	// Only the primitive defaultConfiguration is always present; blank/nil optionals are omitted
	// (a null field is dropped, not emitted).
	d := billingConfigReq{DefaultConfiguration: true}.doc()
	if d["defaultConfiguration"] != true {
		t.Errorf("defaultConfiguration must be set true, got %#v", d["defaultConfiguration"])
	}
	for _, k := range []string{
		"name", "address", "company", "baseCurrency", "mailGatewayId", "invoiceGatewayId",
		"settings", "promotionCodesEnabled", "provisioningSettings", "autoActivationFlow",
		"suspensionConfiguration", "savingsContractNotificationConfig",
	} {
		if _, ok := d[k]; ok {
			t.Errorf("blank/nil %q must be omitted, got %#v", k, d[k])
		}
	}
}

func TestBillingConfigReqDoc_SetsProvided(t *testing.T) {
	pce := false
	req := billingConfigReq{
		Name:                  "Default",
		BaseCurrency:          "USD",
		MailGatewayID:         "mg1",
		InvoiceGatewayID:      "ig1",
		DefaultConfiguration:  true,
		PromotionCodesEnabled: &pce,
		Address:               pgdoc.M{"country": "RO"},
		Company:               pgdoc.M{"businessName": "Acme"},
		Settings:              pgdoc.M{"x": 1},
	}
	d := req.doc()
	if d["name"] != "Default" || d["baseCurrency"] != "USD" || d["mailGatewayId"] != "mg1" || d["invoiceGatewayId"] != "ig1" {
		t.Errorf("scalar fields not stored: %#v", d)
	}
	if d["defaultConfiguration"] != true {
		t.Errorf("defaultConfiguration want true, got %#v", d["defaultConfiguration"])
	}
	// promotionCodesEnabled is a nullable Boolean — when present it must be stored as the bool value
	// (a non-null false is kept).
	if v, ok := d["promotionCodesEnabled"].(bool); !ok || v != false {
		t.Errorf("promotionCodesEnabled want stored false, got %#v", d["promotionCodesEnabled"])
	}
	if _, ok := d["address"].(pgdoc.M); !ok {
		t.Errorf("address passthrough missing: %#v", d["address"])
	}
}

func TestBillingConfigReqDoc_PromotionCodesEnabledTrue(t *testing.T) {
	pce := true
	d := billingConfigReq{PromotionCodesEnabled: &pce}.doc()
	if v, ok := d["promotionCodesEnabled"].(bool); !ok || !v {
		t.Errorf("promotionCodesEnabled want true, got %#v", d["promotionCodesEnabled"])
	}
}

func TestBillingConfigReqDecode(t *testing.T) {
	var req billingConfigReq
	body := `{"name":"Cfg","baseCurrency":"EUR","defaultConfiguration":true,"promotionCodesEnabled":false,"address":{"country":"RO","city":"Cluj"},"company":{"businessName":"X"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "Cfg" || req.BaseCurrency != "EUR" || !req.DefaultConfiguration {
		t.Errorf("decoded scalars mismatch: %+v", req)
	}
	if req.PromotionCodesEnabled == nil || *req.PromotionCodesEnabled != false {
		t.Errorf("promotionCodesEnabled want non-nil false, got %#v", req.PromotionCodesEnabled)
	}
	if addressCountry(req.Address) != "RO" {
		t.Errorf("address.country want RO, got %q", addressCountry(req.Address))
	}
}

func TestUpdateInvoiceGatewayReqDecode(t *testing.T) {
	var req updateInvoiceGatewayReq
	if err := json.Unmarshal([]byte(`{"invoiceGatewayId":"gw-9"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.InvoiceGatewayID != "gw-9" {
		t.Errorf("invoiceGatewayId want gw-9, got %q", req.InvoiceGatewayID)
	}
}

func TestValidateBillingConfig(t *testing.T) {
	h := &Handler{}
	// Missing baseCurrency → 400 "Base Currency is required ".
	if err := h.validateBillingConfig(billingConfigReq{}); err == nil {
		t.Fatal("want error for missing baseCurrency")
	} else if err.Status != 400 || err.Msg != msgBaseCurrencyRequired {
		t.Errorf("baseCurrency err = (%d,%q), want (400,%q)", err.Status, err.Msg, msgBaseCurrencyRequired)
	}
	// baseCurrency set, no address → ok.
	if err := h.validateBillingConfig(billingConfigReq{BaseCurrency: "USD"}); err != nil {
		t.Errorf("no-address want nil, got %v", err)
	}
	// baseCurrency set, blank country → ok (country check only runs when non-blank).
	if err := h.validateBillingConfig(billingConfigReq{BaseCurrency: "USD", Address: pgdoc.M{"country": ""}}); err != nil {
		t.Errorf("blank country want nil, got %v", err)
	}
	// Invalid country → 400 "Country is not valid ".
	if err := h.validateBillingConfig(billingConfigReq{BaseCurrency: "USD", Address: pgdoc.M{"country": "ZZ"}}); err == nil {
		t.Fatal("want error for invalid country")
	} else if err.Status != 400 || err.Msg != msgCountryNotValid {
		t.Errorf("country err = (%d,%q), want (400,%q)", err.Status, err.Msg, msgCountryNotValid)
	}
}

func TestCountryExists(t *testing.T) {
	// US must be a valid alpha2 in the catalog; case-insensitive.
	if !countryExists("US") {
		t.Error("US should exist")
	}
	if !countryExists("us") {
		t.Error("us (lowercase) should exist (case-insensitive)")
	}
	if countryExists("ZZ") {
		t.Error("ZZ should not exist")
	}
}

func TestAddressCountry(t *testing.T) {
	if addressCountry(nil) != "" {
		t.Error("nil address → empty")
	}
	if addressCountry(pgdoc.M{}) != "" {
		t.Error("no country key → empty")
	}
	if addressCountry(pgdoc.M{"country": "RO"}) != "RO" {
		t.Error("country=RO")
	}
}

func TestDocID(t *testing.T) {
	// generated hex id → itself.
	oid := pgdoc.NewID()
	if id, ok := docID(pgdoc.M{"_id": oid}); !ok || id != oid {
		t.Errorf("generated id = (%q,%v), want (%q,true)", id, ok, oid)
	}
	// raw string id → itself.
	if id, ok := docID(pgdoc.M{"_id": "cfg-1"}); !ok || id != "cfg-1" {
		t.Errorf("string id = (%q,%v), want (cfg-1,true)", id, ok)
	}
	// missing _id → not ok.
	if _, ok := docID(pgdoc.M{}); ok {
		t.Error("missing _id → ok must be false")
	}
}

func TestEqualFold(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"RO", "ro", true},
		{"us", "US", true},
		{"US", "USA", false},
		{"AB", "AC", false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := equalFold(c.a, c.b); got != c.want {
			t.Errorf("equalFold(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
