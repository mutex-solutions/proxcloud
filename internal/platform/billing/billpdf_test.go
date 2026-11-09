package billing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// The content stream is rendered uncompressed so the literal cell strings are greppable.
func renderBill(t *testing.T, bill *pricing.Bill, iss IssuerInfo, rates []pricing.TaxRate) (string, string) {
	t.Helper()
	pdfCompress = false
	t.Cleanup(func() { pdfCompress = true })
	bp := &BillingProfile{
		Company: true, CompanyName: "Acme Pte Ltd", FirstName: "Ann", LastName: "Lee", Email: "ann@acme.sg",
		Address: "1 Raffles Place", City: "Singapore", ZipCode: "048616", Country: "SG", VatCode: "M12345678X",
	}
	data, name, err := BillStatementPDF(bill, bp, iss, rates)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte("%PDF")) {
		t.Fatalf("not a PDF: %q", data[:8])
	}
	return string(data), name
}

func sgIssuer() IssuerInfo {
	return IssuerInfo{
		BusinessName: "Example Cloud Pte. Ltd.", UEN: "202312345A", GSTRegistrationNumber: "M90312345A", GSTRegistered: true,
		Address: "10 Anson Road", City: "Singapore", ZipCode: "079903", Country: "SG", BaseCurrency: "SGD",
	}
}

func sampleBill(status pricing.BillStatus) *pricing.Bill {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 9, 1, 0, 5, 0, 0, time.UTC)
	due := sent.Add(72 * time.Hour)
	minus10 := decimal.RequireFromString("-10")
	b := &pricing.Bill{
		ID: "b1", Status: status, InvoiceCurrency: "SGD",
		BillingCycle: &pricing.BillBillingCycle{StartDate: &start, EndDate: &end},
		Items: []pricing.BillItem{
			{Name: "web-1", ResourceType: "instance", NetAmount: decimal.RequireFromString("100"), CreatedAt: &start, UpdatedAt: &end},
			{Name: "gone", ResourceType: "volume", NetAmount: decimal.Zero}, // zero rows are not printed
		},
		Adjustments: []pricing.BillAdjustment{{Amount: &minus10, Description: "Loyalty discount"}, {Amount: &minus10}},
	}
	if status != pricing.BillStatusOpen {
		b.InvoiceNumber, b.SentAt, b.DueAt = "INV-2026-000007", &sent, &due
	}
	return b
}

var gst9 = []pricing.TaxRate{{Name: "GST", Country: "SG", Level: pricing.TaxAudienceAll, RateLevels: []pricing.TaxLevel{{Level: 1, Percentage: 9}}}}

func TestBillStatementPDF_TaxInvoice(t *testing.T) {
	out, name := renderBill(t, sampleBill(pricing.BillStatusSent), sgIssuer(), gst9)
	if name != "INV-2026-000007.pdf" {
		t.Errorf("filename = %q", name)
	}
	// net = 100 - 10 - 10 = 80; GST 9% = 7.20; total 87.20 — the same helpers settlement uses.
	for _, want := range []string{
		"TAX INVOICE", "INV-2026-000007", "01.09.2026", "04.09.2026", "Example Cloud Pte. Ltd.", "UEN: 202312345A",
		"GST Reg. No.: M90312345A", "Bill to", "Acme Pte Ltd", "Tax ID: M12345678X", "Singapore 048616",
		"web-1", "Loyalty discount", "Adjustment", "-10.00 SGD", "GST 9%", "7.20 SGD", "87.20 SGD", "UEN 202312345A",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PDF missing %q", want)
		}
	}
	if strings.Contains(out, "gone") || strings.Contains(out, "STATEMENT") {
		t.Error("zero item / statement wording must not render on a tax invoice")
	}
}

func TestBillStatementPDF_NotGSTRegistered(t *testing.T) {
	iss := sgIssuer()
	iss.GSTRegistered = false
	out, _ := renderBill(t, sampleBill(pricing.BillStatusPaid), iss, gst9)
	if !strings.Contains(out, "INVOICE") || strings.Contains(out, "TAX INVOICE") {
		t.Error("want plain INVOICE title")
	}
	if strings.Contains(out, "GST") {
		t.Error("a non-registered issuer must print no GST lines or GST reg. no.")
	}
	if !strings.Contains(out, "80.00 SGD") {
		t.Error("total must be the net 80.00 SGD without tax")
	}
}

func TestBillStatementPDF_OpenIsStatement(t *testing.T) {
	out, name := renderBill(t, sampleBill(pricing.BillStatusOpen), sgIssuer(), gst9)
	if name != "Bill-01.08.2026.pdf" {
		t.Errorf("filename = %q", name)
	}
	for _, want := range []string{"STATEMENT", "Provisional", "Invoice no."} {
		if !strings.Contains(out, want) {
			t.Errorf("PDF missing %q", want)
		}
	}
	if strings.Contains(out, "INVOICE") {
		t.Error("an OPEN bill must not be titled an invoice")
	}
}

func TestBillPDFTitle(t *testing.T) {
	cases := []struct {
		status pricing.BillStatus
		gst    bool
		want   string
	}{
		{pricing.BillStatusOpen, true, "STATEMENT"},
		{pricing.BillStatusOpen, false, "STATEMENT"},
		{pricing.BillStatusSent, true, "TAX INVOICE"},
		{pricing.BillStatusPaid, true, "TAX INVOICE"},
		{pricing.BillStatusSent, false, "INVOICE"},
		{pricing.BillStatusPaid, false, "INVOICE"},
	}
	for _, c := range cases {
		if got, _ := billPDFTitle(c.status, c.gst); got != c.want {
			t.Errorf("billPDFTitle(%s,%v)=%q want %q", c.status, c.gst, got, c.want)
		}
	}
}

func TestCollectReceiptPDF(t *testing.T) {
	pdfCompress = false
	t.Cleanup(func() { pdfCompress = true })
	amt := decimal.RequireFromString("87.20")
	now := time.Now().UTC()
	txn := &pricing.CollectTransaction{ID: "txn-1", Currency: "SGD", GrossAmount: &amt, Status: pricing.CollectTransactionStatusSuccess, CreatedAt: &now}
	data, name, err := CollectReceiptPDF(txn, &BillingProfile{FirstName: "Ann", LastName: "Lee", Email: "ann@acme.sg"}, sgIssuer())
	if err != nil || name != "Receipt-txn-1.pdf" {
		t.Fatalf("err=%v name=%q", err, name)
	}
	for _, want := range []string{"RECEIPT", "Example Cloud Pte. Ltd.", "UEN: 202312345A", "txn-1", "87.20 SGD", "Ann Lee"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("receipt missing %q", want)
		}
	}
}
