package billing

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/shopspring/decimal"
)

// creditnote.go renders the two self-contained PDFs for an account-credit (deposit) transaction —
// the CREDIT NOTE issued when a deposit is refunded (txn REFUNDED) and the deposit RECEIPT (txn
// SUCCESS) — in the same fpdf/Helvetica layout family as CollectReceiptPDF (billpdf.go). Both are
// served by the client txn download route.

// issuerAddressLines renders the issuer's postal block (billpdf.go prints the same fields):
// street, "city postcode", country — blank parts skipped.
func issuerAddressLines(iss IssuerInfo) []string {
	var lines []string
	if iss.Address != "" {
		lines = append(lines, iss.Address)
	}
	if cp := strings.TrimSpace(iss.City + " " + iss.ZipCode); cp != "" {
		lines = append(lines, cp)
	}
	if iss.Country != "" {
		lines = append(lines, iss.Country)
	}
	return lines
}

// CreditNoteNumber derives the credit note number from the refunded transaction id ("CN-" + the
// id's last 8 chars, upper-cased) — deterministic, so the email, the PDF and a re-download agree.
func CreditNoteNumber(txnID string) string {
	short := txnID
	if len(short) > 8 {
		short = short[len(short)-8:]
	}
	return "CN-" + strings.ToUpper(short)
}

// CreditNotePDF renders the credit note for a REFUNDED deposit: issuer + customer blocks, the
// credit note number/date, the transaction (and invoice/bill) it refers to, the refunded amount
// (negative gross) and the reason. Returns the bytes + "CreditNote-<number>.pdf".
func CreditNotePDF(txn *AccountCreditTransaction, bp *BillingProfile, issuer IssuerInfo) ([]byte, string, error) {
	number := CreditNoteNumber(txn.ID)
	date := txn.UpdatedAt // stamped by the refund save
	if date == nil {
		date = txn.CreatedAt
	}
	reason := txn.GatewayMessage
	if reason == "" {
		reason = "Deposit refunded to the original payment method"
	}
	rows := [][2]string{
		{"Credit note number", number},
		{"Date", fmtDate(date)},
		{"Refers to", refersTo(txn)},
		{"Amount refunded", "-" + txnMoney(txn.GrossAmount, txn.Amount, txn.Currency)},
		{"Currency", txn.Currency},
		{"Reason", reason},
	}
	data, err := txnDocumentPDF("CREDIT NOTE", issuer, bp, rows,
		"This credit note reverses the payment above. The refunded amount is no longer available as account credit.")
	return data, "CreditNote-" + number + ".pdf", err
}

// DepositReceiptPDF renders the receipt for a SUCCESS deposit (the account-credit twin of
// CollectReceiptPDF). Returns the bytes + "Receipt-<txn id>.pdf".
func DepositReceiptPDF(txn *AccountCreditTransaction, bp *BillingProfile, issuer IssuerInfo) ([]byte, string, error) {
	rows := [][2]string{
		{"Transaction ID", txn.ID},
		{"Date", fmtDate(txn.CreatedAt)},
		{"Status", txn.Status},
		{"Net amount", txnMoney(txn.Amount, nil, txn.Currency)},
		{"Total paid", txnMoney(txn.GrossAmount, txn.Amount, txn.Currency)},
	}
	if txn.BillID != "" {
		rows = append(rows, [2]string{"Applied to bill", txn.BillID})
	}
	data, err := txnDocumentPDF("RECEIPT", issuer, bp, rows, "The net amount was credited to your account balance.")
	return data, "Receipt-" + txn.ID + ".pdf", err
}

// refersTo is the "Refers to" line: the transaction id, plus the external invoice number or the
// targeted bill when the deposit had one.
func refersTo(txn *AccountCreditTransaction) string {
	s := "Transaction " + txn.ID
	switch {
	case txn.ExternalInvoiceID != "":
		s += " / invoice " + txn.ExternalInvoiceID
	case txn.BillID != "":
		s += " / bill " + txn.BillID
	}
	return s
}

// txnMoney formats the first non-nil money pointer at 2dp + currency ("0.00 <cur>" when both nil).
func txnMoney(primary, fallback *decimal.Decimal, currency string) string {
	d := decimal.Zero
	if primary != nil {
		d = *primary
	} else if fallback != nil {
		d = *fallback
	}
	return money(d, currency)
}

func fmtDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("02.01.2006")
}

// txnDocumentPDF is the shared page: title (accent, right) + issuer block (left), customer block,
// a label/value table and a footnote.
func txnDocumentPDF(title string, issuer IssuerInfo, bp *BillingProfile, rows [][2]string, footnote string) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()
	// Core fonts are single-byte: route every user-supplied string through the cp1252 translator
	// (same as billpdf.go) so accented names degrade gracefully instead of printing mojibake.
	tr := pdf.UnicodeTranslatorFromDescriptor("")

	// Header — title (accent) on the right, the issuer block on the left.
	pdf.SetFont("Helvetica", "B", 26)
	pdf.SetTextColor(136, 144, 192)
	pdf.SetXY(115, 15)
	pdf.CellFormat(80, 12, title, "", 0, "R", false, 0, "")
	pdf.SetTextColor(0, 0, 0)
	name := issuer.BusinessName
	if name == "" {
		name = "ProxCloud"
	}
	pdf.SetFont("Helvetica", "", 14)
	pdf.SetXY(15, 16)
	pdf.CellFormat(100, 8, tr(name), "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 9)
	lines := issuerAddressLines(issuer)
	if issuer.UEN != "" {
		lines = append(lines, "UEN: "+issuer.UEN)
	}
	if issuer.GSTRegistered && issuer.GSTRegistrationNumber != "" {
		lines = append(lines, "GST Reg. No: "+issuer.GSTRegistrationNumber)
	}
	for _, l := range lines {
		pdf.SetX(15)
		pdf.CellFormat(100, 4.5, tr(l), "", 2, "L", false, 0, "")
	}

	// Customer block (the billing profile).
	y := pdf.GetY() + 6
	if y < 40 {
		y = 40
	}
	pdf.SetFont("Helvetica", "B", 10)
	pdf.SetXY(15, y)
	pdf.CellFormat(120, 5, "Customer", "", 2, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	customer := []string{strings.TrimSpace(bp.FirstName + " " + bp.LastName)}
	if bp.Company && bp.CompanyName != "" {
		customer = append(customer, bp.CompanyName)
	}
	customer = append(customer, bp.Email, fmt.Sprintf("Address: %s, %s, %s", bp.Country, bp.County, bp.Address))
	if bp.Company && bp.VatCode != "" {
		customer = append(customer, "VAT Code: "+bp.VatCode)
	}
	for _, l := range customer {
		pdf.SetX(15)
		pdf.CellFormat(180, 5, tr(l), "", 2, "L", false, 0, "")
	}

	// Details table.
	pdf.Ln(6)
	for _, r := range rows {
		pdf.SetFont("Helvetica", "B", 10)
		pdf.SetX(15)
		pdf.CellFormat(50, 7, tr(r[0]), "B", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		pdf.CellFormat(130, 7, tr(r[1]), "B", 1, "L", false, 0, "")
	}
	if footnote != "" {
		pdf.Ln(6)
		pdf.SetFont("Helvetica", "I", 9)
		pdf.SetX(15)
		pdf.MultiCell(180, 5, tr(footnote), "", "L", false)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
