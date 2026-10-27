package admin

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPDFTemplateReqDoc(t *testing.T) {
	// Optional blank fields are omitted.
	d := pdfTemplateReq{}.doc()
	for _, k := range []string{"name", "description", "content", "type"} {
		if _, ok := d[k]; ok {
			t.Errorf("blank %q must be omitted", k)
		}
	}
	d = pdfTemplateReq{Name: "Inv", Description: "desc", Content: "<html/>", Type: "INVOICE"}.doc()
	for k, want := range map[string]any{"name": "Inv", "description": "desc", "content": "<html/>", "type": "INVOICE"} {
		if d[k] != want {
			t.Errorf("doc[%q]=%#v want %#v", k, d[k], want)
		}
	}
}

func TestPDFTemplateReqDecode(t *testing.T) {
	var req pdfTemplateReq
	if err := json.Unmarshal([]byte(`{"name":"N","description":"D","content":"C","type":"STATEMENT","createdAt":"2024-01-01T00:00:00Z","id":"x"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Name != "N" || req.Description != "D" || req.Content != "C" || req.Type != "STATEMENT" {
		t.Errorf("decoded req mismatch: %+v", req)
	}
}

func TestDownloadTemplateRequestDecode(t *testing.T) {
	var req downloadTemplateRequest
	if err := json.Unmarshal([]byte(`{"template":{"name":"N","content":"<p/>","type":"INVOICE"}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Template.Name != "N" || req.Template.Content != "<p/>" || req.Template.Type != "INVOICE" {
		t.Errorf("nested template mismatch: %+v", req.Template)
	}
}

func TestParsePDFTemplateType(t *testing.T) {
	for _, in := range []string{"INVOICE", "invoice", "Invoice", "STATEMENT", "statement"} {
		got, herr := parsePDFTemplateType(in)
		if herr != nil {
			t.Errorf("parsePDFTemplateType(%q) unexpected error: %v", in, herr)
			continue
		}
		want := "INVOICE"
		if got == "STATEMENT" {
			want = "STATEMENT"
		}
		if got != want {
			t.Errorf("parsePDFTemplateType(%q)=%q want %q", in, got, want)
		}
	}
	// Unknown → 500 (default-mapped).
	_, herr := parsePDFTemplateType("BOGUS")
	if herr == nil {
		t.Fatal("expected error for unknown type")
	}
	if herr.Status != http.StatusInternalServerError {
		t.Errorf("unknown type status=%d want 500", herr.Status)
	}
}

func TestPDFTemplatePlaceholdersByType(t *testing.T) {
	inv := pdfTemplatePlaceholdersByType("INVOICE")
	if len(inv["invoice"]) != 10 {
		t.Errorf("INVOICE invoice placeholders=%v want 10", inv["invoice"])
	}
	if _, ok := inv["statement"]; ok {
		t.Errorf("INVOICE map must not carry statement key")
	}
	stmt := pdfTemplatePlaceholdersByType("STATEMENT")
	if len(stmt["statement"]) != 5 || len(stmt["payments"]) != 2 {
		t.Errorf("STATEMENT placeholders unexpected: %v", stmt)
	}
	if _, ok := stmt["invoice"]; ok {
		t.Errorf("STATEMENT map must not carry invoice key")
	}
	if len(pdfTemplatePlaceholdersByType("BOGUS")) != 0 {
		t.Errorf("unknown type must map to empty")
	}
}
