package billing

import (
	"bytes"
	"fmt"

	"github.com/go-pdf/fpdf"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// billpdf.go generates a bill consumption-summary
// statement PDF (header "BILL", statement dates, statement-for, an items table with subtotal +
// adjustments + total, and a payments table) using go-pdf/fpdf. The HTML-template
// variant (PDFTemplateType.STATEMENT) is not modeled; this is the default path.

// money formats a decimal at 2dp HALF_UP + the currency.
func money(d decimal.Decimal, currency string) string {
	return d.StringFixed(2) + " " + currency
}

// BillStatementPDF renders the bill statement PDF for a billing profile, returning the bytes + the
// download filename ("Bill-<startDate dd.MM.yyyy>.pdf").
func BillStatementPDF(bill *pricing.Bill, bp *BillingProfile) ([]byte, string, error) {
	const dateFmt = "02.01.2006"
	cur := bill.InvoiceCurrency

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()

	// Header — "BILL" (accent) on the right + the seller name on the left.
	pdf.SetFont("Helvetica", "B", 26)
	pdf.SetTextColor(136, 144, 192)
	pdf.SetXY(150, 15)
	pdf.CellFormat(45, 12, "BILL", "", 0, "R", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 14)
	pdf.SetXY(15, 16)
	pdf.CellFormat(120, 8, "ProxCloud", "", 0, "L", false, 0, "")

	// Statement dates (the bill's billing cycle).
	start, end := "", ""
	if bill.BillingCycle != nil {
		if bill.BillingCycle.StartDate != nil {
			start = bill.BillingCycle.StartDate.UTC().Format(dateFmt)
		}
		if bill.BillingCycle.EndDate != nil {
			end = bill.BillingCycle.EndDate.UTC().Format(dateFmt)
		}
	}
	pdf.SetFont("Helvetica", "", 10)
	labelVal := func(x, y float64, label, val string) {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.SetXY(x, y)
		pdf.CellFormat(28, 5, label, "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.SetXY(x+28, y)
		pdf.CellFormat(60, 5, val, "", 0, "L", false, 0, "")
	}
	labelVal(15, 30, "Start date", start)
	labelVal(15, 36, "End date", end)

	// Statement for (the billing profile).
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetXY(120, 30)
	pdf.CellFormat(75, 5, "Statement for", "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	fullName := bp.FirstName + " " + bp.LastName
	for _, line := range []string{
		fullName, bp.Email,
		fmt.Sprintf("Address: %s, %s, %s", bp.Country, bp.County, bp.Address),
	} {
		pdf.SetX(120)
		pdf.CellFormat(75, 5, line, "", 2, "L", false, 0, "")
	}
	if bp.Company && bp.VatCode != "" {
		pdf.SetX(120)
		pdf.CellFormat(75, 5, "VAT Code: "+bp.VatCode, "", 2, "L", false, 0, "")
	}

	// Items table.
	pdf.SetXY(15, 60)
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(0, 7, "Total generated costs", "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetFillColor(240, 240, 245)
	w := []float64{14, 96, 35, 35} // Nr / Name / Type / Net (sum 180)
	pdf.CellFormat(w[0], 7, "Nr. crt", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[1], 7, "Name of products or services", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[2], 7, "Resource Type", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[3], 7, "Net Amount", "B", 1, "L", true, 0, "")

	pdf.SetFont("Helvetica", "", 9)
	subtotal := decimal.Zero
	nr := 0
	for i := range bill.Items {
		it := &bill.Items[i]
		subtotal = subtotal.Add(it.NetAmount)
		if it.NetAmount.Cmp(decimal.Zero) <= 0 {
			continue
		}
		nr++
		itemCur := it.Currency
		if itemCur == "" {
			itemCur = cur
		}
		pdf.CellFormat(w[0], 6, fmt.Sprintf("%d", nr), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[1], 6, truncate(it.Name, 50), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[2], 6, it.ResourceType, "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[3], 6, money(it.NetAmount, itemCur), "B", 1, "L", false, 0, "")
	}
	// Subtotal.
	pdf.SetFont("Helvetica", "B", 9)
	pdf.CellFormat(w[0], 6, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[1], 6, "Subtotal", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[2], 6, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[3], 6, money(subtotal, cur), "", 1, "L", false, 0, "")
	// Adjustments.
	total := subtotal
	pdf.SetFont("Helvetica", "", 9)
	for i := range bill.Adjustments {
		adj := &bill.Adjustments[i]
		amt := decimal.Zero
		if adj.Amount != nil {
			amt = *adj.Amount
		}
		total = total.Add(amt)
		pdf.CellFormat(w[0], 6, "", "", 0, "L", false, 0, "")
		pdf.CellFormat(w[1], 6, "Adjustment", "", 0, "L", false, 0, "")
		pdf.CellFormat(w[2], 6, "", "", 0, "L", false, 0, "")
		pdf.CellFormat(w[3], 6, money(amt, cur), "", 1, "L", false, 0, "")
	}
	// Total.
	pdf.SetFont("Helvetica", "B", 11)
	pdf.CellFormat(w[0], 7, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[1], 7, "Total", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[2], 7, "", "", 0, "L", false, 0, "")
	pdf.CellFormat(w[3], 7, money(total, cur), "", 1, "L", false, 0, "")

	// Payments (applied credits) table — only when present.
	pay := func(label string, items func() (decimal.Decimal, bool)) {
		amt, ok := items()
		if !ok {
			return
		}
		pdf.SetFont("Helvetica", "", 9)
		pdf.CellFormat(150, 6, label, "B", 0, "L", false, 0, "")
		pdf.CellFormat(30, 6, money(amt, cur), "B", 1, "L", false, 0, "")
	}
	hasPayments := len(bill.AppliedAccountCredits) > 0 || len(bill.AppliedPromotionalCredits) > 0 || len(bill.CollectedAmounts) > 0
	if hasPayments {
		pdf.Ln(6)
		pdf.SetFont("Helvetica", "B", 11)
		pdf.CellFormat(0, 7, "Payments", "", 1, "L", false, 0, "")
		pay("From Account credits", func() (decimal.Decimal, bool) {
			if len(bill.AppliedAccountCredits) == 0 {
				return decimal.Zero, false
			}
			s := decimal.Zero
			for i := range bill.AppliedAccountCredits {
				if bill.AppliedAccountCredits[i].Amount != nil {
					s = s.Add(*bill.AppliedAccountCredits[i].Amount)
				}
			}
			return s, true
		})
		pay("From Promotional credits", func() (decimal.Decimal, bool) {
			if len(bill.AppliedPromotionalCredits) == 0 {
				return decimal.Zero, false
			}
			s := decimal.Zero
			for i := range bill.AppliedPromotionalCredits {
				if bill.AppliedPromotionalCredits[i].Amount != nil {
					s = s.Add(*bill.AppliedPromotionalCredits[i].Amount)
				}
			}
			return s, true
		})
		pay("Collected Amounts", func() (decimal.Decimal, bool) {
			if len(bill.CollectedAmounts) == 0 {
				return decimal.Zero, false
			}
			s := decimal.Zero
			for i := range bill.CollectedAmounts {
				if bill.CollectedAmounts[i].Amount != nil {
					s = s.Add(*bill.CollectedAmounts[i].Amount)
				}
			}
			return s, true
		})
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, "", err
	}
	name := "Bill.pdf"
	if bill.BillingCycle != nil && bill.BillingCycle.StartDate != nil {
		name = "Bill-" + bill.BillingCycle.StartDate.UTC().Format(dateFmt) + ".pdf"
	}
	return buf.Bytes(), name, nil
}

// CollectReceiptPDF renders a payment receipt for a collect transaction (the bill-history
// transaction "Download" button). When no external invoice gateway is configured,
// ProxCloud generates a self-contained receipt instead (functional, not the external invoice). Returns
// the bytes + filename.
func CollectReceiptPDF(txn *pricing.CollectTransaction, bp *BillingProfile) ([]byte, string, error) {
	const dateFmt = "02.01.2006 15:04"
	cur := txn.Currency
	amt := decimal.Zero
	if txn.GrossAmount != nil {
		amt = *txn.GrossAmount
	} else if txn.Amount != nil {
		amt = *txn.Amount
	}

	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()

	// Header — "RECEIPT" (accent) on the right + the seller name on the left.
	pdf.SetFont("Helvetica", "B", 26)
	pdf.SetTextColor(136, 144, 192)
	pdf.SetXY(135, 15)
	pdf.CellFormat(60, 12, "RECEIPT", "", 0, "R", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "", 14)
	pdf.SetXY(15, 16)
	pdf.CellFormat(120, 8, "ProxCloud", "", 0, "L", false, 0, "")

	// Billed-to (the billing profile).
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetXY(15, 32)
	pdf.CellFormat(75, 5, "Billed to", "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	for _, line := range []string{bp.FirstName + " " + bp.LastName, bp.Email} {
		pdf.SetX(15)
		pdf.CellFormat(120, 5, line, "", 2, "L", false, 0, "")
	}

	// Details table.
	created := ""
	if txn.CreatedAt != nil {
		created = txn.CreatedAt.UTC().Format(dateFmt)
	}
	row := func(label, val string) {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.SetX(15)
		pdf.CellFormat(50, 7, label, "B", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.CellFormat(130, 7, val, "B", 1, "L", false, 0, "")
	}
	pdf.SetXY(15, 56)
	row("Transaction ID", txn.ID)
	row("Created At", created)
	row("Status", string(txn.Status))
	row("Invoice Amount", money(amt, cur))
	if txn.PaymentGatewayID != "" {
		row("Payment Gateway", txn.PaymentGatewayID)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, "", err
	}
	name := "Receipt.pdf"
	if txn.ID != "" {
		name = "Receipt-" + txn.ID + ".pdf"
	}
	return buf.Bytes(), name, nil
}

// truncate caps a string to n runes.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
