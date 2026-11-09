package billing

import (
	"context"
	"html"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// notify.go is the billing-jobs' email hook. The concrete mail.Service is
// injected via SetNotifier on the jobs; when absent (no mail gateway configured) the calls are
// no-ops, so the gated email side-effects degrade gracefully. Email failures are swallowed —
// a notification must never break the billing state machine.

// Notifier sends a templated email (mail.Service satisfies it).
//
// EXTRACTION SEAM: mail templates and the SMTP integration live in proxcloud today, so on extraction
// this becomes a billing→proxcloud callback (POST /internal/v1/notify) until the billing service owns
// its own mailer. The signature is already wire-ready: every argument is JSON-serializable.
type Notifier interface {
	SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error
}

// Attachment is a file carried by a billing mail (the invoice PDF). Mirrors mail.Attachment —
// billing must stay free of the mail package (extraction seam), so the adapter in cmd/api maps it.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Data        []byte `json:"data"` // base64 on the wire
}

// AttachmentNotifier is the optional Notifier extension that carries files. A Notifier that does
// not implement it still gets the mail, just without the attachment.
type AttachmentNotifier interface {
	SendTemplateWithAttachments(ctx context.Context, key string, to []string, vars map[string]any, atts []Attachment) error
}

// BillLinkFunc resolves the customer-console deep link of a bill ({{billUrl}} in the BILL mails).
// Wired from main (it needs the org + project repos); "" when nothing resolves.
type BillLinkFunc func(ctx context.Context, profile *BillingProfile, bill *pricing.Bill) string

// notify is the nil-safe, best-effort send helper. Every customer-facing billing mail goes to
// Recipients(bp): the profile email plus each billing contact.
func notify(ctx context.Context, n Notifier, key string, bp *BillingProfile, vars map[string]any) {
	notifyWith(ctx, n, key, bp, vars, nil)
}

// notifyWith is notify with attachments; they are dropped when the notifier cannot carry them.
func notifyWith(ctx context.Context, n Notifier, key string, bp *BillingProfile, vars map[string]any, atts []Attachment) {
	to := Recipients(bp)
	if n == nil || len(to) == 0 {
		return
	}
	if an, ok := n.(AttachmentNotifier); ok && len(atts) > 0 {
		_ = an.SendTemplateWithAttachments(ctx, key, to, vars, atts)
		return
	}
	_ = n.SendTemplate(ctx, key, to, vars)
}

// profileFullName is the profile's full name (firstName + " " + lastName).
func profileFullName(p *BillingProfile) string {
	return strings.TrimSpace(p.FirstName + " " + p.LastName)
}

// billMailVars are the BILL-category template vars (bill_is_generated / bill_is_paid) derived
// from the bill alone: fullName, the billing-cycle startDate/endDate and the invoiceNumber when
// stamped, plus currency, issueDate (sentAt), dueDate (dueAt, "" when none), itemsSummaryHtml and
// itemsCount. Dates are 2006-01-02.
func billMailVars(profile *BillingProfile, bill *pricing.Bill) map[string]any {
	ccy := bill.InvoiceCurrency
	if ccy == "" {
		ccy = profile.Currency
	}
	vars := map[string]any{
		"fullName":         profileFullName(profile),
		"currency":         ccy,
		"issueDate":        isoDate(bill.SentAt),
		"dueDate":          isoDate(bill.DueAt),
		"itemsSummaryHtml": itemsSummaryHTML(bill, ccy),
		"itemsCount":       len(bill.Items),
	}
	if bill.BillingCycle != nil {
		if bill.BillingCycle.StartDate != nil {
			vars["startDate"] = bill.BillingCycle.StartDate.Format("2006-01-02")
		}
		if bill.BillingCycle.EndDate != nil {
			vars["endDate"] = bill.BillingCycle.EndDate.Format("2006-01-02")
		}
	}
	if bill.InvoiceNumber != "" {
		vars["invoiceNumber"] = bill.InvoiceNumber
	}
	return vars
}

// BillMailVars is the exported billMailVarsFull for the payment package (the
// notify_customer_payment_failed mail on a declined charge).
func BillMailVars(ctx context.Context, repo *Repo, link BillLinkFunc, profile *BillingProfile, bill *pricing.Bill, rates []pricing.TaxRate) map[string]any {
	return billMailVarsFull(ctx, repo, link, profile, bill, rates)
}

// NotifyBillPaid mails bill_is_paid with the invoice PDF rendered now, so the attached document
// already lists the payment. Every path that settles a bill after it was issued sends it: the card
// charge (automatic, cron retry or the customer's retry), pay-from-balance and an operator-recorded
// payment. Best-effort.
func NotifyBillPaid(ctx context.Context, n Notifier, repo *Repo, pricingRepo *pricing.Repo, link BillLinkFunc, profile *BillingProfile, bill *pricing.Bill) {
	// A PAID bill has nothing due, so the tax rates are irrelevant to amountDue ("0.00").
	notifyWith(ctx, n, "bill_is_paid", profile, billMailVarsFull(ctx, repo, link, profile, bill, nil),
		invoiceAttachment(ctx, repo, pricingRepo, profile, bill, time.Now().UTC()))
}

// invoiceAttachment renders the bill's statement PDF for the BILL mails — the same document as the
// console download; nil when it cannot be rendered (logged), so the mail still goes out.
func invoiceAttachment(ctx context.Context, repo *Repo, pricingRepo *pricing.Repo, profile *BillingProfile, bill *pricing.Bill, now time.Time) []Attachment {
	var allRates []pricing.TaxRate
	if pricingRepo != nil {
		allRates, _ = pricingRepo.AllTaxRates(ctx)
	}
	issuer, _ := repo.IssuerInfo(ctx)
	data, filename, err := BillStatementPDF(bill, profile, issuer, InvoiceTaxRates(allRates, profile, bill, now))
	if err != nil {
		slog.Warn("bill mail: invoice pdf not attached", "bill", bill.ID, "err", err)
		return nil
	}
	return []Attachment{{Filename: filename, ContentType: "application/pdf", Data: data}}
}

// billMailVarsFull is billMailVars plus the vars that need a lookup, so the customer can pay
// by bank transfer straight from the mail: amountDue (the unpaid GROSS in the invoice currency),
// the BankTransfer gateway's instructions (bankInstructions raw, bankInstructionsHtml escaped with
// <br> line breaks for {{{…}}}) and billUrl (the console deep link, via link).
func billMailVarsFull(ctx context.Context, repo *Repo, link BillLinkFunc, profile *BillingProfile, bill *pricing.Bill, rates []pricing.TaxRate) map[string]any {
	vars := billMailVars(profile, bill)
	vars["amountDue"] = amountDue(bill, rates)
	instr := ""
	if repo != nil {
		instr, _ = repo.BankTransferInstructions(ctx)
	}
	vars["bankInstructions"] = instr
	vars["bankInstructionsHtml"] = strings.ReplaceAll(html.EscapeString(strings.ReplaceAll(instr, "\r\n", "\n")), "\n", "<br>")
	url := ""
	if link != nil {
		url = link(ctx, profile, bill)
	}
	vars["billUrl"] = url
	return vars
}

// amountDue is the bill's unpaid GROSS in the invoice currency (2dp) — the same math the
// settlement and the bill DTO use (unpaid net → CalculateGrossAmount); "0.00" once covered.
func amountDue(bill *pricing.Bill, rates []pricing.TaxRate) string {
	unpaid := pricing.GetUnpaidAmountBillProductCurrency(bill)
	if !unpaid.IsPositive() {
		return "0.00"
	}
	return pricing.CalculateGrossAmount(unpaid, rates).StringFixed(2)
}

func isoDate(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// itemsSummaryMaxRows caps the per-service rows of itemsSummaryHTML; the rest fold into "Other".
const itemsSummaryMaxRows = 12

// itemsSummaryHTML renders the bill's charges grouped by resourceType as a two-column table
// (Service | Net amount), largest first, at most itemsSummaryMaxRows rows + an "Other" row (the
// overflow and any item without a resourceType), then a net total row. Values are HTML-escaped
// and every style is inline so Outlook/Gmail render it; templates embed it as {{{itemsSummaryHtml}}}.
func itemsSummaryHTML(bill *pricing.Bill, currency string) string {
	type row struct {
		label  string
		amount decimal.Decimal
	}
	sums := map[string]decimal.Decimal{}
	other, hasOther := decimal.Zero, false
	for i := range bill.Items {
		it := &bill.Items[i]
		if it.ResourceType == "" {
			other, hasOther = other.Add(it.NetAmount), true
			continue
		}
		sums[it.ResourceType] = sums[it.ResourceType].Add(it.NetAmount)
	}
	rows := make([]row, 0, len(sums)+1)
	for k, v := range sums {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if c := rows[i].amount.Cmp(rows[j].amount); c != 0 {
			return c > 0
		}
		return rows[i].label < rows[j].label
	})
	if len(rows) > itemsSummaryMaxRows {
		for _, r := range rows[itemsSummaryMaxRows:] {
			other = other.Add(r.amount)
		}
		rows, hasOther = rows[:itemsSummaryMaxRows], true
	}
	if hasOther {
		rows = append(rows, row{"Other", other})
	}

	const cell = `padding:8px 10px;border-bottom:1px solid #e5e7eb;`
	var b strings.Builder
	b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="width:100%;border-collapse:collapse;font-family:Arial,Helvetica,sans-serif;font-size:14px;color:#111827;">`)
	b.WriteString(`<tr><th align="left" style="` + cell + `border-bottom:2px solid #d1d5db;">Service</th>`)
	b.WriteString(`<th align="right" style="` + cell + `border-bottom:2px solid #d1d5db;">Net amount (` + html.EscapeString(currency) + `)</th></tr>`)
	for _, r := range rows {
		b.WriteString(`<tr><td style="` + cell + `">` + html.EscapeString(r.label) + `</td>`)
		b.WriteString(`<td align="right" style="` + cell + `">` + r.amount.StringFixed(2) + `</td></tr>`)
	}
	b.WriteString(`<tr><td style="padding:8px 10px;font-weight:bold;">Total (net)</td>`)
	b.WriteString(`<td align="right" style="padding:8px 10px;font-weight:bold;">` + bill.CalculateTotalAmount().StringFixed(2) + `</td></tr>`)
	b.WriteString(`</table>`)
	return b.String()
}
