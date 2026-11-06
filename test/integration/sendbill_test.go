//go:build integration

package integration

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

type billMailCapture struct {
	mu   sync.Mutex
	sent []struct {
		key  string
		vars map[string]any
		atts []billing.Attachment
	}
}

func (c *billMailCapture) SendTemplate(ctx context.Context, key string, to []string, vars map[string]any) error {
	return c.SendTemplateWithAttachments(ctx, key, to, vars, nil)
}

func (c *billMailCapture) SendTemplateWithAttachments(_ context.Context, key string, _ []string, vars map[string]any, atts []billing.Attachment) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, struct {
		key  string
		vars map[string]any
		atts []billing.Attachment
	}{key, vars, atts})
	return nil
}

// TestSendBillNumbersAndMails: sending a last-month OPEN bill stamps a sequential INV-<year>-<seq>
// number exactly once, sets sentAt (the issue date) and mails the period + number.
func TestSendBillNumbersAndMails(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	mustInsert(t, db, "billingConfiguration", pgdoc.M{"baseCurrency": "USD", "defaultConfiguration": true,
		"company": pgdoc.M{"businessName": "Example Cloud Pte. Ltd.", "uen": "202312345A", "gstRegistered": true, "gstRegistrationNumber": "M90312345A"},
		"address": pgdoc.M{"address": "10 Anson Road", "city": "Singapore", "zipCode": "079903", "country": "SG"}})
	repo := billing.NewRepo(db)
	svc := billing.NewBillSendService(repo, pricing.NewRepo(db))
	mails := &billMailCapture{}
	svc.SetNotifier(mails)
	// The pay-by-bank-transfer vars: the BankTransfer gateway's instructions, a 9% tax rate (the
	// mailed amountDue is the unpaid GROSS) and the injected console deep-link resolver.
	mustInsert(t, db, "thirdPartyIntegration", pgdoc.M{"name": "Wire", "thirdParty": "BankTransfer", "category": "Payment",
		"config": pgdoc.M{"bankTransferInstructions": "Bank: DBS\nIBAN: SG12 <ACME>"}})
	mustInsert(t, db, "taxRate", pgdoc.M{"name": "GST", "level": "ALL", "rateLevels": []any{pgdoc.M{"level": 1, "percentage": 9}}})
	svc.SetBillLink(func(_ context.Context, p *billing.BillingProfile, b *pricing.Bill) string {
		return "https://console.example/p/proj-" + p.ID + "/billing/history/bills/" + b.ID
	})

	now := time.Date(2026, 9, 1, 0, 0, 30, 0, time.UTC)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	openBill := func(bpID string) string {
		return mustInsertID(t, db, "bill", pgdoc.M{
			"billingProfileId": bpID, "status": "OPEN", "invoiceCurrency": "USD",
			"billingCycle": pgdoc.M{"startDate": start, "endDate": end},
			"items":        []any{pgdoc.M{"resourceType": "instance", "currency": "USD", "netAmount": decimalOf(t, "10")}},
		})
	}
	profile := &billing.BillingProfile{ID: "bp-1", Currency: "USD", Email: "ann@acme.sg", FirstName: "Ann", LastName: "Lee", Status: billing.StatusActive}
	billID := openBill(profile.ID)

	ok, err := svc.SendBillForProfile(ctx, profile, now)
	if err != nil || !ok {
		t.Fatalf("send: ok=%v err=%v", ok, err)
	}
	var b pricing.Bill
	if found, _ := db.C("bill").Get(ctx, billID, &b); !found {
		t.Fatal("bill vanished")
	}
	if b.Status != pricing.BillStatusSent || b.InvoiceNumber != "INV-2026-000001" || b.SentAt == nil || b.DueAt == nil {
		t.Fatalf("bill after send: status=%s number=%q sentAt=%v dueAt=%v", b.Status, b.InvoiceNumber, b.SentAt, b.DueAt)
	}
	if len(mails.sent) != 1 || mails.sent[0].key != "bill_is_generated" {
		t.Fatalf("mails: %+v", mails.sent)
	}
	v := mails.sent[0].vars
	if v["startDate"] != "2026-08-01" || v["endDate"] != "2026-09-01" || v["invoiceNumber"] != "INV-2026-000001" || v["fullName"] != "Ann Lee" {
		t.Fatalf("mail vars: %v", v)
	}
	if v["amountDue"] != "10.90" || v["currency"] != "USD" || v["issueDate"] != "2026-09-01" || v["dueDate"] != "2026-09-08" || v["itemsCount"] != 1 {
		t.Fatalf("mail money vars: %v", v)
	}
	if v["bankInstructions"] != "Bank: DBS\nIBAN: SG12 <ACME>" || v["bankInstructionsHtml"] != "Bank: DBS<br>IBAN: SG12 &lt;ACME&gt;" {
		t.Fatalf("bank vars: %q / %q", v["bankInstructions"], v["bankInstructionsHtml"])
	}
	if v["billUrl"] != "https://console.example/p/proj-bp-1/billing/history/bills/"+billID {
		t.Fatalf("billUrl: %v", v["billUrl"])
	}
	if s, _ := v["itemsSummaryHtml"].(string); !strings.Contains(s, ">instance<") || !strings.Contains(s, ">10.00<") {
		t.Fatalf("itemsSummaryHtml: %s", s)
	}
	// The invoice PDF rides along — the same statement the console download renders.
	atts := mails.sent[0].atts
	if len(atts) != 1 || atts[0].ContentType != "application/pdf" || !strings.HasSuffix(atts[0].Filename, ".pdf") || !strings.Contains(atts[0].Filename, "INV-2026-000001") || !strings.HasPrefix(string(atts[0].Data), "%PDF") {
		t.Fatalf("attachment: n=%d %+v", len(atts), func() any {
			if len(atts) > 0 {
				return atts[0].Filename
			}
			return nil
		}())
	}

	// Second profile → the next number; the first bill is never renumbered (it is SENT now, so a
	// re-run finds no OPEN bill and leaves it alone).
	p2 := &billing.BillingProfile{ID: "bp-2", Currency: "USD", Email: "bob@acme.sg", Status: billing.StatusActive}
	bill2 := openBill(p2.ID)
	if ok, err := svc.SendBillForProfile(ctx, p2, now); err != nil || !ok {
		t.Fatalf("send 2: ok=%v err=%v", ok, err)
	}
	var b2 pricing.Bill
	_, _ = db.C("bill").Get(ctx, bill2, &b2)
	if b2.InvoiceNumber != "INV-2026-000002" {
		t.Fatalf("second number = %q", b2.InvoiceNumber)
	}
	if ok, _ := svc.SendBillForProfile(ctx, profile, now); ok {
		t.Fatal("re-send must be a no-op for an already-sent bill")
	}
	_, _ = db.C("bill").Get(ctx, billID, &b)
	if b.InvoiceNumber != "INV-2026-000001" {
		t.Fatalf("first bill renumbered: %q", b.InvoiceNumber)
	}

	// Issuer info round-trips from the configuration doc.
	iss, err := repo.IssuerInfo(ctx)
	if err != nil || iss.BusinessName != "Example Cloud Pte. Ltd." || iss.UEN != "202312345A" || !iss.GSTRegistered || iss.ZipCode != "079903" || iss.BaseCurrency != "USD" {
		t.Fatalf("issuer: %+v err=%v", iss, err)
	}
}

// TestPgdocIncrementConcurrent: N concurrent increments on a fresh counter yield exactly 1..N.
func TestPgdocIncrementConcurrent(t *testing.T) {
	ctx := context.Background()
	col := freshPG(t).C("counters")
	const n = 40
	got := make(chan int64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := col.Increment(ctx, "invoice-2026", "seq")
			if err != nil {
				t.Error(err)
				return
			}
			got <- v
		}()
	}
	wg.Wait()
	close(got)
	seen := map[int64]bool{}
	for v := range got {
		if v < 1 || v > n || seen[v] {
			t.Fatalf("bad/duplicate counter value %d", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct values, want %d", len(seen), n)
	}
	if v, err := col.Increment(ctx, "invoice-2027", "seq"); err != nil || v != 1 {
		t.Fatalf("fresh counter: %d %v", v, err)
	}
}

// TestHasOverdueBillsFollowsDueDate: a SENT bill is overdue only once its dueAt (the date the
// invoice prints) has passed — not a fixed number of days after it was sent.
func TestHasOverdueBillsFollowsDueDate(t *testing.T) {
	ctx := context.Background()
	db := freshPG(t)
	repo := billing.NewRepo(db)
	now := time.Now().UTC()
	mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": "bp-1", "status": "SENT",
		"sentAt": now.AddDate(0, 0, -5), "dueAt": now.AddDate(0, 0, 2)})
	if overdue, err := repo.HasOverdueBills(ctx, "bp-1"); err != nil || overdue {
		t.Fatalf("sent 5 days ago, due in 2: overdue=%v err=%v, want false", overdue, err)
	}
	mustInsertID(t, db, "bill", pgdoc.M{"billingProfileId": "bp-1", "status": "SENT",
		"sentAt": now.AddDate(0, 0, -8), "dueAt": now.Add(-time.Hour)})
	if overdue, err := repo.HasOverdueBills(ctx, "bp-1"); err != nil || !overdue {
		t.Fatalf("due an hour ago: overdue=%v err=%v, want true", overdue, err)
	}
}
