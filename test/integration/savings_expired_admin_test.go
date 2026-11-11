//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/mailoutbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type adminExpiredEnqueuer struct{ store *mailoutbox.Store }

func (a adminExpiredEnqueuer) EnqueueSavingsExpiredTx(ctx context.Context, in billing.SavingsExpiredRequest) (billing.ReminderQueueResult, error) {
	q, e := a.store.EnqueueSavingsExpiredTx(ctx, mailoutbox.SavingsExpired{ContractID: in.ContractID, TermGeneration: in.TermGeneration, CanonicalEndDate: in.CanonicalEndDate, Date: in.Date, ExpiresAt: in.ExpiresAt, Recipients: in.Recipients, FullName: in.FullName, SavingsPlanName: in.SavingsPlanName, Currency: in.Currency, StartDate: in.StartDate, MonthlyCommittedAmount: in.MonthlyCommittedAmount, From: "sender@example.invalid", TextBody: "fixture"})
	return billing.ReminderQueueResult{IDs: q.IDs, State: q.State}, e
}
func TestSavingsExpiredRawAdminCannotSelectHistoryAndFullReplaceDeleteRetainsTerm(t *testing.T) {
	db, store := outboxFixture(t)
	repo := billing.NewRepo(db)
	repo.SetReminderCanceler(store)
	repo.SetSavingsExpiredCanceler(store)
	repo.SetSavingsExpiredOutbox(adminExpiredEnqueuer{store: store})
	profile := mustInsertID(t, db, "billingProfile", pgdoc.M{"email": "fixture@example.invalid", "currency": "USD"})
	end := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	contract := mustInsertID(t, db, "savingsContract", pgdoc.M{"billingProfileId": profile, "status": billing.SavingsStatusActive, "endDate": end, "durationMonths": 1})
	svc := billing.NewSavingsService(repo)
	if n, e := svc.ExpireContracts(t.Context()); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	var original string
	if e := db.Pool.QueryRow(t.Context(), `SELECT (to_jsonb(t)-'cancelled_at')::text FROM savings_expired_term t`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	var body pgdoc.M
	if ok, e := db.C("savingsContract").Get(t.Context(), contract, &body); e != nil || !ok {
		t.Fatal(e)
	}
	// An untrusted raw request cannot supply a server history/generation/receipt,
	// delete the real sidecar, or invent delivery acceptance via public DTO fields.
	body["savingsExpiredTerm"] = pgdoc.M{"selection": "legacy", "outboxIds": []string{"attacker"}}
	body["termGeneration"] = "attacker"
	body["outboxIds"] = []string{"attacker"}
	body["receipt"] = pgdoc.M{"state": "accepted"}
	rc := &httpx.RequestContext{Sub: "fixture-op", Issuer: "fixture-iss", Azp: "fixture-cid"}
	raw, _ := json.Marshal(body)
	if w := doAdmin(reminderAdmin(db, repo), rc, "PUT", "/admin/savings-contracts/"+contract, string(raw)); w.Code != 200 {
		t.Fatalf("raw noop: %d %s", w.Code, w.Body)
	}
	var current string
	if e := db.Pool.QueryRow(t.Context(), `SELECT (to_jsonb(t)-'cancelled_at')::text FROM savings_expired_term t`).Scan(&current); e != nil || current != original {
		t.Fatal("raw history fields trusted", e)
	}
	c, e := repo.SavingsContractByID(t.Context(), contract)
	if e != nil {
		t.Fatal(e)
	}
	c.Status = billing.SavingsStatusActive
	if e = repo.SaveSavingsContract(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	if n, e := svc.ExpireContracts(t.Context()); e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if e = db.Pool.QueryRow(t.Context(), `SELECT (to_jsonb(t)-'cancelled_at')::text FROM savings_expired_term t`).Scan(&current); e != nil || current != original {
		t.Fatal("fullreplace reminted term", e)
	}
	if w := doAdmin(reminderAdmin(db, repo), rc, "DELETE", "/admin/savings-contracts/"+contract, ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	var terms, events int
	if e = db.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM savings_expired_term),(SELECT count(*) FROM mail_outbox_event)`).Scan(&terms, &events); e != nil || terms != 1 || events != 1 {
		t.Fatal("deleted history", e)
	}
}
