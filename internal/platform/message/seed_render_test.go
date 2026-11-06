package message

import (
	"strings"
	"testing"
)

// TestBillGeneratedRendersPaymentDetails: the seeded bill_is_generated body embeds the HTML vars
// unescaped (triple-stache) and escapes the plain ones (the URL in the button href).
func TestBillGeneratedRendersPaymentDetails(t *testing.T) {
	var body string
	for _, tmpl := range SystemTemplates() {
		if tmpl.Key == "bill_is_generated" {
			body = tmpl.MessageBody
		}
	}
	got := renderMustache(body, map[string]any{
		"fullName": "Ann", "invoiceNumber": "INV-2026-000001", "amountDue": "10.90", "currency": "USD",
		"dueDate": "2026-09-04", "issueDate": "2026-09-01", "billUrl": "https://c.example/p/1/billing/history/bills/b?x=1&y=2",
		"itemsSummaryHtml":     "<table><tr><td>instance</td></tr></table>",
		"bankInstructionsHtml": "Bank: DBS<br>IBAN: SG12 &lt;ACME&gt;",
	})
	for _, want := range []string{
		"<table><tr><td>instance</td></tr></table>", "Bank: DBS<br>IBAN: SG12 &lt;ACME&gt;",
		"Amount due: 10.90 USD", "due 2026-09-04",
		"href=\"https://c.example/p/1/billing/history/bills/b?x=1&amp;y=2\"",
		"quote the invoice number <strong>INV-2026-000001</strong>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered body lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "{{") {
		t.Errorf("unrendered placeholder left:\n%s", got)
	}
}
