package billing

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// billpdf.go renders the bill document with go-pdf/fpdf (Helvetica core font only) as a
// Singapore-compliant invoice: an OPEN bill is a provisional STATEMENT; a SENT/PAID bill is a
// TAX INVOICE (GST-registered issuer) or INVOICE. It carries the issuer identity (business name,
// address, UEN, GST reg. no.), the invoice meta (number, issue/due date, period, currency), the
// customer block, per-item periods, adjustments, and the GST lines computed with the same pricing
// helpers settlement uses. The renderer is pure — callers fetch the issuer info + tax rates.

const pdfDateFmt = "02.01.2006"

// pdfCompress is switched off by tests so the content stream stays greppable.
var pdfCompress = true

// money formats a decimal at 2dp HALF_UP + the currency.
func money(d decimal.Decimal, currency string) string {
	return d.StringFixed(2) + " " + currency
}

// billPDFTitle returns the document title + subtitle for a bill status / issuer GST status.
func billPDFTitle(status pricing.BillStatus, gstRegistered bool) (title, subtitle string) {
	switch status {
	case pricing.BillStatusSent, pricing.BillStatusPaid:
		if gstRegistered {
			return "TAX INVOICE", ""
		}
		return "INVOICE", ""
	}
	return "STATEMENT", "Provisional — usage to date, not an invoice"
}

// countryName resolves an alpha-2 code to the catalog name (the code itself when unknown/blank).
func countryName(code string) string {
	for _, c := range Countries() {
		if strings.EqualFold(c.Alpha2, code) {
			return c.Name
		}
	}
	return code
}

// joinNonEmpty joins the non-blank parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return strings.Join(out, sep)
}

// newBillPDF creates the A4 document with the issuer footer ("<businessName> · UEN <uen>", small
// grey, page bottom) and returns it with the UTF-8→cp1252 translator for the core font.
func newBillPDF(iss IssuerInfo) (*fpdf.Fpdf, func(string) string) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetCompression(pdfCompress)
	pdf.SetMargins(15, 15, 15)
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	if iss.UEN != "" {
		footer := joinNonEmpty(" · ", iss.BusinessName, "UEN "+iss.UEN)
		pdf.SetFooterFunc(func() {
			pdf.SetY(-15)
			pdf.SetFont("Helvetica", "", 8)
			pdf.SetTextColor(128, 128, 128)
			pdf.CellFormat(0, 6, tr(footer), "", 0, "C", false, 0, "")
			pdf.SetTextColor(0, 0, 0)
		})
	}
	pdf.AddPage()
	return pdf, tr
}

// issuerBlock prints the issuer identity top-left (name, address lines, UEN, GST reg. no.) and
// returns the y just below it.
func issuerBlock(pdf *fpdf.Fpdf, tr func(string) string, iss IssuerInfo) float64 {
	name := iss.BusinessName
	if name == "" {
		name = "ProxCloud"
	}
	pdf.SetTextColor(0, 0, 0)
	pdf.SetFont("Helvetica", "B", 14)
	pdf.SetXY(15, 15)
	pdf.CellFormat(105, 8, tr(name), "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	lines := []string{iss.Address, joinNonEmpty(" ", iss.City, iss.ZipCode), countryName(iss.Country)}
	if iss.UEN != "" {
		lines = append(lines, "UEN: "+iss.UEN)
	}
	if iss.GSTRegistered && iss.GSTRegistrationNumber != "" {
		lines = append(lines, "GST Reg. No.: "+iss.GSTRegistrationNumber)
	}
	for _, l := range lines {
		if l == "" {
			continue
		}
		pdf.SetX(15)
		pdf.CellFormat(105, 4.5, tr(l), "", 2, "L", false, 0, "")
	}
	return pdf.GetY()
}

// BillStatementPDF renders the bill document for a billing profile, returning the bytes + the
// download filename ("<invoiceNumber>.pdf" once numbered, else "Bill-<startDate dd.MM.yyyy>.pdf").
// `rates` are the profile's tax rates (see InvoiceTaxRates); they are only printed when the issuer
// is GST-registered.
func BillStatementPDF(bill *pricing.Bill, bp *BillingProfile, iss IssuerInfo, rates []pricing.TaxRate) ([]byte, string, error) {
	const dash = "—"
	cur := bill.InvoiceCurrency
	if cur == "" {
		cur = iss.BaseCurrency
	}

	pdf, tr := newBillPDF(iss)

	// Title (accent, right) + optional subtitle.
	title, subtitle := billPDFTitle(bill.Status, iss.GSTRegistered)
	pdf.SetFont("Helvetica", "B", 22)
	pdf.SetTextColor(136, 144, 192)
	pdf.SetXY(120, 15)
	pdf.CellFormat(75, 10, tr(title), "", 2, "R", false, 0, "")
	if subtitle != "" {
		pdf.SetFont("Helvetica", "I", 8)
		pdf.SetTextColor(120, 120, 120)
		pdf.SetX(120)
		pdf.CellFormat(75, 4, tr(subtitle), "", 2, "R", false, 0, "")
	}
	pdf.SetTextColor(0, 0, 0)

	// Issuer (left).
	leftY := issuerBlock(pdf, tr, iss)

	// Meta block (right): invoice no., issue/due date, billing period, currency.
	start, end := "", ""
	if bill.BillingCycle != nil {
		if bill.BillingCycle.StartDate != nil {
			start = bill.BillingCycle.StartDate.UTC().Format(pdfDateFmt)
		}
		if bill.BillingCycle.EndDate != nil {
			end = bill.BillingCycle.EndDate.UTC().Format(pdfDateFmt)
		}
	}
	invoiceNo, issued, due := dash, dash, dash
	if bill.InvoiceNumber != "" {
		invoiceNo = bill.InvoiceNumber
	}
	if bill.SentAt != nil {
		issued = bill.SentAt.UTC().Format(pdfDateFmt)
	}
	if bill.DueAt != nil {
		due = bill.DueAt.UTC().Format(pdfDateFmt)
	}
	period := joinNonEmpty(" – ", start, end)
	if period == "" {
		period = dash
	}
	pdf.SetXY(120, 32)
	for _, kv := range [][2]string{
		{"Invoice no.", invoiceNo}, {"Issue date", issued}, {"Due date", due},
		{"Billing period", period}, {"Currency", cur},
	} {
		pdf.SetX(120)
		pdf.SetFont("Helvetica", "B", 9)
		pdf.CellFormat(30, 5, tr(kv[0]), "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 9)
		pdf.CellFormat(45, 5, tr(kv[1]), "", 1, "L", false, 0, "")
	}
	y := max(leftY, pdf.GetY()) + 6

	// Bill to (the billing profile).
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetXY(15, y)
	pdf.CellFormat(105, 5, "Bill to", "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	customer := profileFullName(bp)
	if bp.Company && bp.CompanyName != "" {
		customer = bp.CompanyName
	}
	custLines := []string{customer, bp.Email, bp.Address, joinNonEmpty(" ", bp.City, bp.ZipCode), countryName(bp.Country)}
	if bp.VatCode != "" {
		custLines = append(custLines, "Tax ID: "+bp.VatCode)
	}
	for _, l := range custLines {
		if l == "" {
			continue
		}
		pdf.SetX(15)
		pdf.CellFormat(105, 4.5, tr(l), "", 2, "L", false, 0, "")
	}

	// Items table.
	pdf.SetXY(15, pdf.GetY()+6)
	pdf.SetFont("Helvetica", "B", 9)
	pdf.SetFillColor(240, 240, 245)
	w := []float64{10, 75, 30, 37, 28} // Nr / Name / Type / Period / Net (sum 180)
	pdf.CellFormat(w[0], 7, "Nr.", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[1], 7, "Name of products or services", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[2], 7, "Resource Type", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[3], 7, "Period", "B", 0, "L", true, 0, "")
	pdf.CellFormat(w[4], 7, "Net Amount", "B", 1, "R", true, 0, "")

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
		// Item period: its own lifetime within the bill (createdAt – updatedAt), else the cycle.
		from, to := start, end
		if it.CreatedAt != nil {
			from = it.CreatedAt.UTC().Format(pdfDateFmt)
		}
		if it.UpdatedAt != nil {
			to = it.UpdatedAt.UTC().Format(pdfDateFmt)
		}
		pdf.CellFormat(w[0], 6, fmt.Sprintf("%d", nr), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[1], 6, tr(truncate(it.Name, 42)), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[2], 6, tr(it.ResourceType), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[3], 6, tr(joinNonEmpty(" – ", from, to)), "B", 0, "L", false, 0, "")
		pdf.CellFormat(w[4], 6, money(it.NetAmount, itemCur), "B", 1, "R", false, 0, "")
	}
	totalRow := func(label, val string, bold bool, size, h float64) {
		style := ""
		if bold {
			style = "B"
		}
		pdf.SetFont("Helvetica", style, size)
		pdf.CellFormat(w[0], h, "", "", 0, "L", false, 0, "")
		pdf.CellFormat(w[1]+w[2]+w[3], h, tr(label), "", 0, "L", false, 0, "")
		pdf.CellFormat(w[4], h, val, "", 1, "R", false, 0, "")
	}
	// Subtotal → adjustments → net (the tax base) → GST lines → total.
	subtotalLabel, totalLabel := "Subtotal", "Total"
	if iss.GSTRegistered {
		subtotalLabel, totalLabel = "Subtotal (excl. GST)", "Total (incl. GST)"
	}
	totalRow(subtotalLabel, money(subtotal, cur), true, 9, 6)
	net := subtotal
	for i := range bill.Adjustments {
		adj := &bill.Adjustments[i]
		amt := decimal.Zero
		if adj.Amount != nil {
			amt = *adj.Amount
		}
		net = net.Add(amt)
		label := adj.Description
		if label == "" {
			label = "Adjustment"
		}
		totalRow(label, money(amt, cur), false, 9, 6)
	}
	total := net
	if iss.GSTRegistered {
		for _, r := range rates {
			one := []pricing.TaxRate{r}
			totalRow(fmt.Sprintf("GST %d%%", pricing.CalculateTotalPercentage(one)), money(pricing.CalculateTaxAmount(net, one), cur), false, 9, 6)
		}
		total = pricing.CalculateGrossAmount(net, rates) // the exact amount settlement charged
	}
	totalRow(totalLabel, money(total, cur), true, 11, 7)

	// Payments (applied credits) table — only when present.
	pay := func(label string, items func() (decimal.Decimal, bool)) {
		amt, ok := items()
		if !ok {
			return
		}
		pdf.SetFont("Helvetica", "", 9)
		pdf.CellFormat(150, 6, label, "B", 0, "L", false, 0, "")
		pdf.CellFormat(30, 6, money(amt, cur), "B", 1, "R", false, 0, "")
	}
	// Payments are tracked net; print them in the same terms as the total line above so a paid
	// tax invoice reconciles (net → gross with the invoice's rates when GST-registered).
	grossOf := func(n decimal.Decimal) decimal.Decimal {
		if iss.GSTRegistered {
			return pricing.CalculateGrossAmount(n, rates)
		}
		return n
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
			return grossOf(s), true
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
			return grossOf(s), true
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
			return grossOf(s), true
		})
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, "", err
	}
	name := "Bill.pdf"
	switch {
	case bill.InvoiceNumber != "":
		name = bill.InvoiceNumber + ".pdf"
	case start != "":
		name = "Bill-" + start + ".pdf"
	}
	return buf.Bytes(), name, nil
}

// CollectReceiptPDF renders a payment receipt for a collect transaction (the bill-history
// transaction "Download" button). When no external invoice gateway is configured,
// ProxCloud generates a self-contained receipt instead (functional, not the external invoice). Returns
// the bytes + filename.
func CollectReceiptPDF(txn *pricing.CollectTransaction, bp *BillingProfile, iss IssuerInfo) ([]byte, string, error) {
	const dateFmt = "02.01.2006 15:04"
	cur := txn.Currency
	amt := decimal.Zero
	if txn.GrossAmount != nil {
		amt = *txn.GrossAmount
	} else if txn.Amount != nil {
		amt = *txn.Amount
	}

	pdf, tr := newBillPDF(iss)

	// Header — "RECEIPT" (accent) on the right + the issuer block on the left.
	pdf.SetFont("Helvetica", "B", 22)
	pdf.SetTextColor(136, 144, 192)
	pdf.SetXY(120, 15)
	pdf.CellFormat(75, 10, "RECEIPT", "", 0, "R", false, 0, "")
	y := issuerBlock(pdf, tr, iss) + 6

	// Billed-to (the billing profile).
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetXY(15, y)
	pdf.CellFormat(75, 5, "Billed to", "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	customer := profileFullName(bp)
	if bp.Company && bp.CompanyName != "" {
		customer = bp.CompanyName
	}
	for _, line := range []string{customer, bp.Email} {
		if line == "" {
			continue
		}
		pdf.SetX(15)
		pdf.CellFormat(120, 5, tr(line), "", 2, "L", false, 0, "")
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
		pdf.CellFormat(130, 7, tr(val), "B", 1, "L", false, 0, "")
	}
	pdf.SetXY(15, pdf.GetY()+6)
	row("Receipt / Transaction no.", txn.ID)
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
