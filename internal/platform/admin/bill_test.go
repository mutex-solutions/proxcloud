package admin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBillUpdateReqDecode(t *testing.T) {
	var req billUpdateReq
	body := `{"status":"SENT","invoiceCurrency":"EUR","invoiceGatewayId":"gw1","billingProfileId":"bp1","items":[{"x":1}],"createdAt":"2020-01-01T00:00:00Z"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if req.Status != "SENT" || req.InvoiceCurrency != "EUR" || req.InvoiceGatewayID != "gw1" || req.BillingProfileID != "bp1" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
}

func TestBillUpdateReqSetMapPopulated(t *testing.T) {
	req := billUpdateReq{Status: "PAID", InvoiceCurrency: "USD", InvoiceGatewayID: "gw9", BillingProfileID: "bpZ"}
	d := req.setMap()
	if d["status"] != "PAID" || d["invoiceCurrency"] != "USD" || d["invoiceGatewayId"] != "gw9" || d["billingProfileId"] != "bpZ" {
		t.Errorf("setMap mismatch: %#v", d)
	}
	if len(d) != 4 {
		t.Errorf("setMap should have 4 keys, got %d: %#v", len(d), d)
	}
}

func TestBillUpdateReqSetMapOmitsBlank(t *testing.T) {
	// Empty body → all four optional fields omitted (null/omitted → dropped on the entity).
	d := billUpdateReq{}.setMap()
	if len(d) != 0 {
		t.Errorf("empty body should produce empty setMap, got %#v", d)
	}
	// Partial body → only the provided fields appear.
	d = billUpdateReq{Status: "OPEN"}.setMap()
	if len(d) != 1 || d["status"] != "OPEN" {
		t.Errorf("partial setMap = %#v, want only status", d)
	}
}

func TestBillNotFoundInterpolatesID(t *testing.T) {
	e := billNotFound("abc123")
	if e.Status != 404 {
		t.Errorf("status = %d, want 404", e.Status)
	}
	if !strings.Contains(e.Msg, "abc123") {
		t.Errorf("message %q should interpolate the id", e.Msg)
	}
}
